package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"Medical-Web-Backend/internal/domain/registration"
)

// PostgresRegistrationRepository 实现挂号（medical_registration）的 PostgreSQL 持久化。
//
// 号源扣减必须在事务内完成：先按「父计划 -> 时段」顺序加行锁（与排班域
// UpdateSlotMaximum/DeleteSlot 的加锁顺序一致，避免死锁），再用条件更新
// （num < maximum）原子占号，最后插入挂号记录。任一环节失败整单回滚，
// 不会留下半条记录或永久吞号（契约 §6.2）。
type PostgresRegistrationRepository struct {
	db *sql.DB
}

// NewPostgresRegistrationRepository 构造挂号仓库。
func NewPostgresRegistrationRepository(db *sql.DB) *PostgresRegistrationRepository {
	return &PostgresRegistrationRepository{db: db}
}

// registrationColumns 是列表/详情共用的挂号字段投影；out_trade_no 是 CHAR(32)，
// 读取时 btrim 去掉尾部补位空格。金额取 numeric 的文本形式（如 80.00），
// 由 formatDoctorPrice 统一为两位小数，避免浮点误差。
const registrationColumns = `r.id, r.patient_card_id, r.work_plan_id, r.doctor_schedule_id, r.doctor_id, r.dept_sub_id,
r.date::text, r.slot, r.amount::text, btrim(r.out_trade_no) AS out_trade_no,
r.payment_status, r.create_time::text`

// registrationListFrom 是列表与计数共用的固定来源（别名为 r，供 registrationWhere 使用）。
const registrationListFrom = ` FROM hospital.medical_registration r`

// scheduleSnapshotQuery 一次读取时段、计划、医生与价目快照：
// 时段 -> 计划 -> 医生/子科室的内连接保证脏数据（关联缺失的时段）按「时段不存在」收敛，
// 不会以空医生对象的形式进入资格校验。金额取 doctor_price 的首行 price_1，
// 与公开排班接口同一口径（契约 §8.1、§6.2：price1/price2 的业务口径仍是规格开放项）。
const scheduleSnapshotQuery = `SELECT s.id, s.work_plan_id, s.slot, s.maximum, s.num,
p.doctor_id, p.dept_sub_id, p.date::text, p.maximum, p.num,
d.name, d.job, (d.status IS NOT NULL AND d.status = 1) AS doctor_active,
ds.name,
(SELECT dp.price_1 FROM hospital.doctor_price dp WHERE dp.doctor_id = p.doctor_id ORDER BY dp.id LIMIT 1)
FROM hospital.doctor_work_plan_schedule s
JOIN hospital.doctor_work_plan p ON p.id = s.work_plan_id
JOIN hospital.doctor d ON d.id = p.doctor_id
JOIN hospital.medical_dept_sub ds ON ds.id = p.dept_sub_id
WHERE s.id = $1`

// occupyingRegistrationExistsQuery 判断「同一身份证号在同一时段是否已有占用中的挂号」。
//
// 按 pid 判重必须跨账号生效，因此通过 medical_registration 关联
// patient_user_info_card 后按 pid 过滤，并使用 EXISTS 语义——同一 pid 可能对应多张
// 就诊卡，JOIN 结果的行数不代表挂号数（契约 §6.1）。
//
// 占用状态：PAID（payment_status=2）一定占用；UNPAID（=1）在「未超过支付有效期」时占用。
// 支付有效期为创建后 35 分钟（契约 §6.4、§6.8），但 medical_registration.create_time 是
// PostgreSQL DATE 列，库里没有时刻精度，因此按「创建日期不早于当前业务日」判定未超期：
// 该近似只会把当天创建的 UNPAID 记录继续视为占用（保守方向，宁可不放行也不重复占号），
// 超期记录由过期任务置为 EXPIRED 后自动释放。EXPIRED(4)/REFUNDED(3) 不占用，可重新挂号。
const occupyingRegistrationExistsQuery = `SELECT EXISTS(
SELECT 1 FROM hospital.medical_registration r
JOIN hospital.patient_user_info_card c ON c.id = r.patient_card_id
WHERE btrim(c.pid) = $1 AND r.doctor_schedule_id = $2
  AND (r.payment_status = $3 OR (r.payment_status = $4 AND r.create_time >= $5::date)))`

// registrationScanner 抽象 *sql.Row 与 *sql.Rows 共有的单行扫描能力。
type registrationScanner interface {
	Scan(dest ...any) error
}

// FindScheduleSnapshot 读取时段及其计划、医生与价目快照；时段或关联不存在返回
// registration.ErrScheduleNotFound。
func (r *PostgresRegistrationRepository) FindScheduleSnapshot(
	ctx context.Context,
	scheduleID int64,
) (*registration.ScheduleSnapshot, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	var (
		item                        registration.ScheduleSnapshot
		slot, slotMaximum, slotUsed sql.NullInt16
		planMaximum, planUsed       sql.NullInt16
		doctorName, doctorJob       sql.NullString
		subdepartmentName           sql.NullString
		amount                      sql.NullString
	)
	err := r.db.QueryRowContext(ctx, scheduleSnapshotQuery, scheduleID).Scan(
		&item.ScheduleID, &item.WorkPlanID, &slot, &slotMaximum, &slotUsed,
		&item.DoctorID, &item.SubdepartmentID, &item.Date, &planMaximum, &planUsed,
		&doctorName, &doctorJob, &item.DoctorActive,
		&subdepartmentName, &amount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, registration.ErrScheduleNotFound
	}
	if err != nil {
		return nil, err
	}
	item.Slot = nullInt16(slot)
	item.SlotMaximum, item.SlotUsed = nullInt16(slotMaximum), nullInt16(slotUsed)
	item.PlanMaximum, item.PlanUsed = nullInt16(planMaximum), nullInt16(planUsed)
	item.DoctorName, item.DoctorJob = doctorName.String, doctorJob.String
	item.SubdepartmentName = subdepartmentName.String
	item.Amount = registrationAmount(amount)
	return &item, nil
}

// HasOccupyingRegistration 以 EXISTS 语义判断同一身份证号在同一时段是否已有占用中的挂号。
func (r *PostgresRegistrationRepository) HasOccupyingRegistration(
	ctx context.Context,
	pid string,
	scheduleID int64,
	now time.Time,
) (bool, error) {
	if r == nil || r.db == nil {
		return false, sql.ErrConnDone
	}
	return hasOccupyingRegistration(ctx, r.db, pid, scheduleID, now)
}

// registrationQuerier 抽象 *sql.DB 与 *sql.Tx 共有的单行查询能力，
// 让占用判定既能直接在连接池上执行（资格校验），也能在事务内复用（建单前重查）。
type registrationQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// hasOccupyingRegistration 执行占用判定；参数顺序与查询占位符一致。
func hasOccupyingRegistration(
	ctx context.Context,
	querier registrationQuerier,
	pid string,
	scheduleID int64,
	now time.Time,
) (bool, error) {
	var exists bool
	err := querier.QueryRowContext(
		ctx,
		occupyingRegistrationExistsQuery,
		strings.TrimSpace(pid),
		scheduleID,
		registration.PaymentCodePaid,
		registration.PaymentCodeUnpaid,
		registration.BusinessDate(now),
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// CreateRegistration 在单事务内建单：重新读取关联与金额、校验时段与号源、
// 同步递增时段级与计划级 num、插入挂号记录。
func (r *PostgresRegistrationRepository) CreateRegistration(
	ctx context.Context,
	input registration.CreateInput,
) (*registration.Registration, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	if err := registration.ValidateOutTradeNo(input.OutTradeNo); err != nil {
		return nil, err
	}
	var created *registration.Registration
	err := r.withRegistrationTx(ctx, func(tx *sql.Tx) error {
		result, err := createRegistrationTx(ctx, tx, input)
		if err != nil {
			return err
		}
		created = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// createRegistrationTx 是建单事务的完整步骤，失败时由调用方回滚整单。
func createRegistrationTx(
	ctx context.Context,
	tx *sql.Tx,
	input registration.CreateInput,
) (*registration.Registration, error) {
	// 1. 先不加锁读出时段所属计划：后续必须按「父计划 -> 时段」顺序加锁。
	var workPlanID int64
	err := tx.QueryRowContext(ctx,
		"SELECT work_plan_id FROM hospital.doctor_work_plan_schedule WHERE id = $1",
		input.ScheduleID).Scan(&workPlanID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, registration.ErrScheduleNotFound
	}
	if err != nil {
		return nil, err
	}

	// 2. 锁父计划并读取日期与计划级计数。
	var (
		date            string
		doctorID        int64
		subdepartmentID int64
		planMaximum     sql.NullInt16
		planUsed        sql.NullInt16
	)
	err = tx.QueryRowContext(ctx,
		`SELECT doctor_id, dept_sub_id, date::text, maximum, num
FROM hospital.doctor_work_plan WHERE id = $1 FOR UPDATE`,
		workPlanID).Scan(&doctorID, &subdepartmentID, &date, &planMaximum, &planUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, registration.ErrScheduleNotFound
	}
	if err != nil {
		return nil, err
	}

	// 3. 锁时段行并读取时段级计数（与排班域修改容量、挂号占用共享同一把行锁）。
	var (
		slot        sql.NullInt16
		slotMaximum sql.NullInt16
		slotUsed    sql.NullInt16
	)
	err = tx.QueryRowContext(ctx,
		"SELECT slot, maximum, num FROM hospital.doctor_work_plan_schedule WHERE id = $1 FOR UPDATE",
		input.ScheduleID).Scan(&slot, &slotMaximum, &slotUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, registration.ErrScheduleNotFound
	}
	if err != nil {
		return nil, err
	}

	// 4. 业务规则校验：日期未到（当天即视为已开始）、医生在诊、号源未售罄、同一身份证号未重复占用。
	if registration.Started(date, input.Now) {
		return nil, registration.ErrScheduleStarted
	}
	doctorActive, err := planDoctorActive(ctx, tx, workPlanID)
	if err != nil {
		return nil, err
	}
	if !doctorActive {
		return nil, registration.ErrDoctorInactive
	}
	if nullInt16(slotMaximum)-nullInt16(slotUsed) <= 0 {
		return nil, &registration.SlotSoldOutError{ScheduleID: input.ScheduleID, Remaining: 0}
	}
	duplicate, err := hasOccupyingRegistration(ctx, tx, input.PID, input.ScheduleID, input.Now)
	if err != nil {
		return nil, err
	}
	if duplicate {
		return nil, registration.ErrDuplicate
	}

	// 5. 条件更新占号：两个计数器必须同时递增（计划级 num 是所属时段 num 的汇总），
	//    任一计数器达到上限都回滚整单，不会只更新其中一个。
	planUpdated, err := tx.ExecContext(ctx,
		"UPDATE hospital.doctor_work_plan SET num = num + 1 WHERE id = $1 AND num < maximum",
		workPlanID)
	if err != nil {
		return nil, err
	}
	if affected, err := planUpdated.RowsAffected(); err != nil {
		return nil, err
	} else if affected == 0 {
		// 计划级上限已满：计划级 num 是时段 num 的汇总，此时该计划整体无号可挂，
		// 余量按 0 返回（契约 §12.4 的 details.remaining）。
		return nil, &registration.SlotSoldOutError{ScheduleID: input.ScheduleID, Remaining: 0}
	}
	slotUpdated, err := tx.ExecContext(ctx,
		"UPDATE hospital.doctor_work_plan_schedule SET num = num + 1 WHERE id = $1 AND num < maximum",
		input.ScheduleID)
	if err != nil {
		return nil, err
	}
	if affected, err := slotUpdated.RowsAffected(); err != nil {
		return nil, err
	} else if affected == 0 {
		// 防御性兜底：本事务已持有该时段行的 FOR UPDATE 行锁，且第 4 步已校验过余量，
		// 正常路径下不会命中（并发争抢在加锁阶段即被串行化）。一旦命中说明时段级计数
		// 与 maximum 已不一致，这里回滚整单、连计划级已 +1 的计数一并恢复。
		return nil, &registration.SlotSoldOutError{ScheduleID: input.ScheduleID, Remaining: 0}
	}

	// 6. 金额从医生价目重新读取，客户端提交的金额不可信（与公开排班同一口径）。
	amount, err := doctorRegistrationAmount(ctx, tx, doctorID)
	if err != nil {
		return nil, err
	}

	// 7. 插入挂号记录：payment_status=1（UNPAID），create_time 写业务日历日。
	createDate := registration.BusinessDate(input.Now)
	var registrationID int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO hospital.medical_registration
(id, patient_card_id, work_plan_id, doctor_schedule_id, doctor_id, dept_sub_id, date, slot, amount, out_trade_no, prepay_id, transaction_id, payment_status, create_time)
VALUES (nextval('hospital.medical_registration_sequence'), $1, $2, $3, $4, $5, $6::date, $7, $8::numeric, $9, NULL, NULL, $10, $11::date)
RETURNING id`,
		input.PatientCardID, workPlanID, input.ScheduleID, doctorID, subdepartmentID,
		date, nullInt16(slot), amount, strings.TrimSpace(input.OutTradeNo),
		registration.PaymentCodeUnpaid, createDate).Scan(&registrationID)
	if err != nil {
		return nil, err
	}

	return &registration.Registration{
		ID:              registrationID,
		PatientCardID:   input.PatientCardID,
		WorkPlanID:      workPlanID,
		ScheduleID:      input.ScheduleID,
		DoctorID:        doctorID,
		SubdepartmentID: subdepartmentID,
		Date:            date,
		Slot:            nullInt16(slot),
		Amount:          amount,
		OutTradeNo:      strings.TrimSpace(input.OutTradeNo),
		PaymentStatus:   registration.PaymentStatusUnpaid,
		CreateDate:      createDate,
	}, nil
}

// doctorRegistrationAmount 读取该医生的挂号金额（doctor_price.price_1 首行）。
//
// 金额在事务内从价目表重新读取，客户端提交的金额不可信（契约 §6.2）。
// 无价目记录（sql.ErrNoRows）是业务事实，与公开排班口径一致，按 "0.00" 记录；
// 其它错误（连接中断、权限错误等）必须向上返回，由调用方回滚整单——
// 不能把可重试的依赖故障落库成 0.00 的错误金额，否则下游支付按金额校验必然失败。
func doctorRegistrationAmount(ctx context.Context, tx *sql.Tx, doctorID int64) (string, error) {
	var amount sql.NullString
	err := tx.QueryRowContext(ctx,
		"SELECT price_1 FROM hospital.doctor_price WHERE doctor_id = $1 ORDER BY id LIMIT 1",
		doctorID).Scan(&amount)
	if errors.Is(err, sql.ErrNoRows) {
		return "0.00", nil
	}
	if err != nil {
		return "", err
	}
	return registrationAmount(amount), nil
}

// ListRegistrations 按过滤条件分页返回挂号记录并统计总数。
func (r *PostgresRegistrationRepository) ListRegistrations(
	ctx context.Context,
	f registration.Filter,
	offset, limit int,
) ([]registration.Registration, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, sql.ErrConnDone
	}
	where, args := registrationWhere(f)
	var total int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*)"+registrationListFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	column, direction := registrationOrderBy(f)
	query := "SELECT " + registrationColumns + registrationListFrom + where +
		fmt.Sprintf(" ORDER BY %s %s, r.id DESC LIMIT $%d OFFSET $%d",
			column, direction, len(args)+1, len(args)+2)
	rows, err := r.db.QueryContext(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]registration.Registration, 0)
	for rows.Next() {
		item, err := scanRegistration(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// registrationWhere 构造列表/计数共用的 WHERE 片段与参数。
// 所有条件都是可选的；ownerPatientID 是患者端的强制归属条件（越权记录不可见）。
func registrationWhere(f registration.Filter) (string, []any) {
	conditions := make([]string, 0, 7)
	args := make([]any, 0, 7)
	if f.OwnerPatientID != nil {
		args = append(args, *f.OwnerPatientID)
		conditions = append(conditions, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM hospital.patient_user_info_card c WHERE c.id = r.patient_card_id AND c.user_id = $%d)",
			len(args)))
	}
	if f.PatientCardID != nil {
		args = append(args, *f.PatientCardID)
		conditions = append(conditions, fmt.Sprintf("r.patient_card_id = $%d", len(args)))
	}
	if f.DoctorID != nil {
		args = append(args, *f.DoctorID)
		conditions = append(conditions, fmt.Sprintf("r.doctor_id = $%d", len(args)))
	}
	if f.SubdepartmentID != nil {
		args = append(args, *f.SubdepartmentID)
		conditions = append(conditions, fmt.Sprintf("r.dept_sub_id = $%d", len(args)))
	}
	if f.FromDate != "" {
		args = append(args, f.FromDate)
		conditions = append(conditions, fmt.Sprintf("r.date >= $%d::date", len(args)))
	}
	if f.ToDate != "" {
		args = append(args, f.ToDate)
		conditions = append(conditions, fmt.Sprintf("r.date <= $%d::date", len(args)))
	}
	if f.PaymentStatus != nil {
		args = append(args, *f.PaymentStatus)
		conditions = append(conditions, fmt.Sprintf("r.payment_status = $%d", len(args)))
	}
	if len(conditions) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// registrationOrderBy 把白名单排序参数映射为列名与方向。
// 默认按创建日期倒序（最近挂号在前），并统一以 id 倒序为次级键，保证分页结果稳定。
func registrationOrderBy(f registration.Filter) (string, string) {
	column := "r.create_time"
	switch f.Sort {
	case registration.SortDate:
		column = "r.date"
	case registration.SortID:
		column = "r.id"
	}
	direction := "DESC"
	if strings.EqualFold(f.Order, "asc") {
		direction = "ASC"
	}
	return column, direction
}

// FindRegistrationDetail 读取挂号详情；ownerPatientID > 0 时同时限定归属，
// 他人记录与不存在的记录统一返回 registration.ErrRegistrationNotFound。
func (r *PostgresRegistrationRepository) FindRegistrationDetail(
	ctx context.Context,
	registrationID int64,
	ownerPatientID int64,
) (*registration.Detail, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	// 容量取自挂号关联的时段行：时段是号源的唯一来源，LEFT JOIN 容忍脏数据
	// （时段被删）导致的两列空值，容量按 0 返回而不是让详情整体 404。
	query := `SELECT r.id, r.patient_card_id, r.doctor_id, r.dept_sub_id, r.date::text, r.slot,
r.amount::text, btrim(r.out_trade_no), r.payment_status,
d.name, d.job, ds.name, s.maximum, s.num
FROM hospital.medical_registration r
LEFT JOIN hospital.doctor d ON d.id = r.doctor_id
LEFT JOIN hospital.medical_dept_sub ds ON ds.id = r.dept_sub_id
LEFT JOIN hospital.doctor_work_plan_schedule s ON s.id = r.doctor_schedule_id
WHERE r.id = $1`
	args := []any{registrationID}
	if ownerPatientID > 0 {
		args = append(args, ownerPatientID)
		query += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM hospital.patient_user_info_card c
WHERE c.id = r.patient_card_id AND c.user_id = $%d)`, len(args))
	}

	var (
		item                  registration.Detail
		doctorName, doctorJob sql.NullString
		subdepartmentName     sql.NullString
		slot                  sql.NullInt16
		maximum, used         sql.NullInt16
		amount, outTradeNo    sql.NullString
		paymentStatus         sql.NullInt16
	)
	err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&item.ID, &item.PatientCardID, &item.Doctor.ID, &item.Subdepartment.ID,
		&item.Date, &slot, &amount, &outTradeNo, &paymentStatus,
		&doctorName, &doctorJob, &subdepartmentName, &maximum, &used,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, registration.ErrRegistrationNotFound
	}
	if err != nil {
		return nil, err
	}
	item.Doctor.Name, item.Doctor.Job = doctorName.String, doctorJob.String
	item.Subdepartment.Name = subdepartmentName.String
	item.Slot = nullInt16(slot)
	item.Amount = registrationAmount(amount)
	item.OutTradeNo = outTradeNo.String
	item.PaymentStatus = registration.PaymentStatusLabel(nullInt16(paymentStatus))
	item.Capacity = registration.Capacity{
		Maximum:   nullInt16(maximum),
		Used:      nullInt16(used),
		Remaining: remainingCapacity(nullInt16(maximum), nullInt16(used)),
	}
	return &item, nil
}

// scanRegistration 把一行挂号记录转换为实体。
func scanRegistration(scanner registrationScanner) (registration.Registration, error) {
	var (
		item          registration.Registration
		slot          sql.NullInt16
		paymentStatus sql.NullInt16
		amount        sql.NullString
		outTradeNo    sql.NullString
		createDate    sql.NullString
	)
	if err := scanner.Scan(
		&item.ID, &item.PatientCardID, &item.WorkPlanID, &item.ScheduleID,
		&item.DoctorID, &item.SubdepartmentID, &item.Date, &slot,
		&amount, &outTradeNo, &paymentStatus, &createDate,
	); err != nil {
		return registration.Registration{}, err
	}
	item.Slot = nullInt16(slot)
	item.Amount = registrationAmount(amount)
	item.OutTradeNo = outTradeNo.String
	item.PaymentStatus = registration.PaymentStatusLabel(nullInt16(paymentStatus))
	item.CreateDate = createDate.String
	return item, nil
}

// registrationAmount 把 numeric 的文本形式统一为两位小数的金额字符串；
// 无价目记录（NULL 或空串）时按 "0.00" 返回，保证响应字段始终可解析。
func registrationAmount(raw sql.NullString) string {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return "0.00"
	}
	return formatDoctorPrice(raw.String)
}

// remainingCapacity 按时段容量计算剩余号源，保证不出现负数。
func remainingCapacity(maximum, used int16) int16 {
	if remaining := maximum - used; remaining > 0 {
		return remaining
	}
	return 0
}

// withRegistrationTx 在单个事务内执行 fn，成功提交、失败回滚。
func (r *PostgresRegistrationRepository) withRegistrationTx(
	ctx context.Context,
	fn func(tx *sql.Tx) error,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
