package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
)

// PostgresScheduleRepository 同时实现排班计划（plans）与排班时段（slots）的 PostgreSQL 持久化。
// 计划列表/详情直接读表；计划写操作经 ExecTx 在事务内完成；时段创建、更新与删除均在事务内
// 用 SELECT ... FOR UPDATE 串行化，保证同一计划下时段编号不重复、未来排班校验与容量约束原子性。
type PostgresScheduleRepository struct {
	db *sql.DB
}

// NewPostgresScheduleRepository 构造排班仓库（同时支持计划与时段）。
func NewPostgresScheduleRepository(db *sql.DB) *PostgresScheduleRepository {
	return &PostgresScheduleRepository{db: db}
}

// ListPlans 分页查询计划。departmentId 通过 medical_dept_sub_and_doctor 关联过滤；
// includeSlots=true 时按 slot 升序装载各计划 slots。
func (r *PostgresScheduleRepository) ListPlans(ctx context.Context, f schedule.PlanFilter, offset, limit int) ([]schedule.WorkPlan, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, sql.ErrConnDone
	}
	where, args := planWhere(f)
	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM hospital.doctor_work_plan p"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	column, direction := planOrderBy(f)
	args = append(args, limit, offset)
	query := fmt.Sprintf(`SELECT p.id,p.doctor_id,p.dept_sub_id,p.date::text,p.maximum,p.num
FROM hospital.doctor_work_plan p%s ORDER BY %s %s, p.id ASC LIMIT $%d OFFSET $%d`, where, column, direction, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]schedule.WorkPlan, 0)
	for rows.Next() {
		p, err := scanWorkPlan(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if f.IncludeSlots {
		if err := r.attachSlots(ctx, items); err != nil {
			return nil, 0, err
		}
	}
	return items, total, nil
}

// FindPlan 返回单个计划及其 slots。
func (r *PostgresScheduleRepository) FindPlan(ctx context.Context, planID int64) (*schedule.WorkPlan, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	p, err := queryWorkPlan(ctx, r.db, "SELECT p.id,p.doctor_id,p.dept_sub_id,p.date::text,p.maximum,p.num FROM hospital.doctor_work_plan p WHERE p.id=$1", planID)
	if err != nil {
		return nil, err
	}
	if err := r.attachSlots(ctx, []schedule.WorkPlan{*p}); err != nil {
		return nil, err
	}
	return p, nil
}

// ExecTx 在单事务内执行 fn，成功提交、失败回滚。
func (r *PostgresScheduleRepository) ExecTx(ctx context.Context, fn func(tx port.ScheduleTx) error) error {
	if r == nil || r.db == nil {
		return sql.ErrConnDone
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	wrap := &postgresScheduleTx{tx: tx}
	if err := fn(wrap); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// postgresScheduleTx 是事务作用域内的计划写操作集合。
type postgresScheduleTx struct {
	tx *sql.Tx
}

// TxInsertPlan 插入计划：id 取序列 nextval、num 固定 0，RETURNING 返回自增 id。
func (t *postgresScheduleTx) TxInsertPlan(ctx context.Context, p schedule.WorkPlan) (int64, error) {
	var id int64
	err := t.tx.QueryRowContext(ctx, `INSERT INTO hospital.doctor_work_plan(id,doctor_id,dept_sub_id,date,maximum,num)
VALUES (nextval('hospital.doctor_work_plan_sequence'),$1,$2,$3::date,$4,0) RETURNING id`,
		p.DoctorID, p.SubdepartmentID, p.Date, p.Maximum).Scan(&id)
	if err != nil {
		return 0, mapInsertPlanError(err, p)
	}
	return id, nil
}

// TxHasPlan 判断同一医生/子科室/日期是否已有计划。
func (t *postgresScheduleTx) TxHasPlan(ctx context.Context, doctorID, subdepartmentID int64, date string) (bool, error) {
	var exists bool
	err := t.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hospital.doctor_work_plan
WHERE doctor_id=$1 AND dept_sub_id=$2 AND date=$3::date)`, doctorID, subdepartmentID, date).Scan(&exists)
	return exists, err
}

// TxLockPlanCreate 按“医生/子科室/日期”创建键加事务级咨询锁，串行化同一创建键的并发创建。
// doctor_work_plan 没有 (doctor_id,dept_sub_id,date) 唯一约束且库结构不允许改动，READ COMMITTED
// 下两个并发事务可能同时通过“先 TxHasPlan 再 TxInsertPlan”的检查造成重复行；先在此处对同创建
// 键排队后，后到事务要等先行事务提交后才能继续查重/插入，从而稳定观察到已插入行并返回 409。
func (t *postgresScheduleTx) TxLockPlanCreate(ctx context.Context, doctorID, subdepartmentID int64, date string) error {
	_, err := t.tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", planCreateLockKey(doctorID, subdepartmentID, date))
	return err
}

// planCreateLockKey 把创建键编码为确定性 64 位 FNV-1a 哈希后转 int64，作为 pg_advisory_xact_lock
// 的 bigint 锁键。FNV-1a 对相同输入产生相同哈希，锁键不依赖执行环境；转 int64 后即使为负仍作为
// bigint 咨询锁键使用，“同输入同锁”的串行化语义保持不变。哈希仅用于分组排队，不要求无冲突。
func planCreateLockKey(doctorID, subdepartmentID int64, date string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fmt.Sprintf("%d|%d|%s", doctorID, subdepartmentID, date)))
	return int64(h.Sum64())
}

// TxFindPlanLocked 事务内 FOR UPDATE 重读计划，防止并发修改被静默覆盖。
func (t *postgresScheduleTx) TxFindPlanLocked(ctx context.Context, planID int64) (*schedule.WorkPlan, error) {
	p, err := queryWorkPlan(ctx, t.tx, `SELECT p.id,p.doctor_id,p.dept_sub_id,p.date::text,p.maximum,p.num
FROM hospital.doctor_work_plan p WHERE p.id=$1 FOR UPDATE`, planID)
	if err != nil {
		return nil, err
	}
	slots, err := querySlots(ctx, t.tx, p.ID)
	if err != nil {
		return nil, err
	}
	p.Slots = slots
	return p, nil
}

// TxUpdatePlanMaximum 条件更新 maximum：重读 num 校验新值不小于已用量，再执行 UPDATE。
func (t *postgresScheduleTx) TxUpdatePlanMaximum(ctx context.Context, planID int64, maximum int16) error {
	var used int16
	if err := t.tx.QueryRowContext(ctx, "SELECT COALESCE(num,0) FROM hospital.doctor_work_plan WHERE id=$1 FOR UPDATE", planID).Scan(&used); err != nil {
		return err
	}
	if maximum < used {
		return schedule.ErrMaximumBelowUsed
	}
	_, err := t.tx.ExecContext(ctx, "UPDATE hospital.doctor_work_plan SET maximum=$1 WHERE id=$2", maximum, planID)
	return err
}

// TxHasRegistrations 检查计划或任一 slots 是否已产生挂号记录，避免破坏历史关联。
func (t *postgresScheduleTx) TxHasRegistrations(ctx context.Context, planID int64) (bool, error) {
	var exists bool
	err := t.tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM hospital.medical_registration mr
WHERE mr.work_plan_id=$1
   OR mr.doctor_schedule_id IN (SELECT s.id FROM hospital.doctor_work_plan_schedule s WHERE s.work_plan_id=$1))`, planID).Scan(&exists)
	return exists, err
}

// TxDoctorAssociation 事务内分别校验医生状态、子科室存在性与关联关系：
// 三态独立返回，use case 据此区分“医生不存在或未出诊”“子科室不存在”“医生未关联该子科室”，
// 避免把后两种情况误报为医生状态错误。
func (t *postgresScheduleTx) TxDoctorAssociation(ctx context.Context, doctorID, subdepartmentID int64) (doctorActive bool, subdepartmentExists bool, associated bool, err error) {
	if err = t.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM hospital.doctor WHERE id=$1 AND status=1)", doctorID).Scan(&doctorActive); err != nil {
		return false, false, false, err
	}
	if err = t.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM hospital.medical_dept_sub WHERE id=$1)", subdepartmentID).Scan(&subdepartmentExists); err != nil {
		return false, false, false, err
	}
	// 医生与子科室都有效时才需要检查关联表。
	if doctorActive && subdepartmentExists {
		if err = t.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hospital.medical_dept_sub_and_doctor
WHERE doctor_id=$1 AND dept_sub_id=$2)`, doctorID, subdepartmentID).Scan(&associated); err != nil {
			return false, false, false, err
		}
	}
	return doctorActive, subdepartmentExists, associated, nil
}

// TxDeletePlan 物理删除计划并级联删除其 slots（表结构无级联外键时显式先删 slots）。
// 删除子时段前先对子时段逐行 SELECT ... FOR UPDATE，串行化并发的挂号写入：挂号事务若对
// 时段行加锁会在本事务持有行锁期间等待，从而消除 READ COMMITTED 下 TxHasRegistrations 的
// EXISTS 错过未提交并发挂号事务的理论窗口。
func (t *postgresScheduleTx) TxDeletePlan(ctx context.Context, planID int64) error {
	rows, err := t.tx.QueryContext(ctx, "SELECT id FROM hospital.doctor_work_plan_schedule WHERE work_plan_id=$1 FOR UPDATE", planID)
	if err != nil {
		return err
	}
	for rows.Next() {
		// 仅消费行以取得对子时段行的排他锁，行数据本删除不再需要。
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM hospital.doctor_work_plan_schedule WHERE work_plan_id=$1", planID); err != nil {
		return err
	}
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM hospital.doctor_work_plan WHERE id=$1", planID); err != nil {
		return err
	}
	return nil
}

// planWhere 构造列表 WHERE 片段：departmentId 经子科室关联过滤。
func planWhere(f schedule.PlanFilter) (string, []any) {
	conditions := make([]string, 0, 6)
	args := make([]any, 0, 6)
	if f.DoctorID != nil {
		args = append(args, *f.DoctorID)
		conditions = append(conditions, fmt.Sprintf("p.doctor_id=$%d", len(args)))
	}
	if f.DepartmentID != nil {
		args = append(args, *f.DepartmentID)
		conditions = append(conditions, fmt.Sprintf(`EXISTS (SELECT 1 FROM hospital.medical_dept_sub_and_doctor sd
JOIN hospital.medical_dept_sub ds ON ds.id=sd.dept_sub_id
WHERE sd.doctor_id=p.doctor_id AND sd.dept_sub_id=p.dept_sub_id AND ds.dept_id=$%d)`, len(args)))
	}
	if f.SubdepartmentID != nil {
		args = append(args, *f.SubdepartmentID)
		conditions = append(conditions, fmt.Sprintf("p.dept_sub_id=$%d", len(args)))
	}
	if f.FromDate != "" {
		args = append(args, f.FromDate)
		conditions = append(conditions, fmt.Sprintf("p.date>=$%d::date", len(args)))
	}
	if f.ToDate != "" {
		args = append(args, f.ToDate)
		conditions = append(conditions, fmt.Sprintf("p.date<=$%d::date", len(args)))
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// planOrderBy 把白名单 sort 映射为 SQL 排序列，禁止拼接用户输入。
func planOrderBy(f schedule.PlanFilter) (string, string) {
	column := "p.id"
	switch f.Sort {
	case "date":
		column = "p.date"
	case "doctorId":
		column = "p.doctor_id"
	}
	direction := "ASC"
	if f.Order == "desc" {
		direction = "DESC"
	}
	return column, direction
}

// attachSlots 批量装载多个计划的 slots（按 slot 升序）并计算 remaining。
func (r *PostgresScheduleRepository) attachSlots(ctx context.Context, plans []schedule.WorkPlan) error {
	if len(plans) == 0 {
		return nil
	}
	byID := make(map[int64]*schedule.WorkPlan, len(plans))
	ids := make([]any, 0, len(plans))
	placeholders := make([]string, 0, len(plans))
	for i := range plans {
		byID[plans[i].ID] = &plans[i]
		ids = append(ids, plans[i].ID)
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
	}
	query := fmt.Sprintf(`SELECT id,work_plan_id,slot,maximum,num FROM hospital.doctor_work_plan_schedule
WHERE work_plan_id IN (%s) ORDER BY work_plan_id ASC,slot ASC`, strings.Join(placeholders, ","))
	rows, err := r.db.QueryContext(ctx, query, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s schedule.ScheduleSlot
		var maxV, num sql.NullInt16
		if err := rows.Scan(&s.ID, &s.WorkPlanID, &s.Slot, &maxV, &num); err != nil {
			return err
		}
		s.Maximum = int16Val(maxV)
		s.Used = int16Val(num)
		s.Recalculate()
		if p, ok := byID[s.WorkPlanID]; ok {
			p.Slots = append(p.Slots, s)
		}
	}
	return rows.Err()
}

// scanWorkPlan 读取一行计划并把 num 映射为 used、计算 remaining。
func scanWorkPlan(scanner interface{ Scan(dest ...any) error }) (schedule.WorkPlan, error) {
	var p schedule.WorkPlan
	var maxV, num sql.NullInt16
	if err := scanner.Scan(&p.ID, &p.DoctorID, &p.SubdepartmentID, &p.Date, &maxV, &num); err != nil {
		return p, err
	}
	p.Maximum = int16Val(maxV)
	p.Used = int16Val(num)
	p.Recalculate()
	return p, nil
}

func queryWorkPlan(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, query string, id int64) (*schedule.WorkPlan, error) {
	p, err := scanWorkPlan(q.QueryRowContext(ctx, query, id))
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func querySlots(ctx context.Context, tx *sql.Tx, planID int64) ([]schedule.ScheduleSlot, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,work_plan_id,slot,maximum,num FROM hospital.doctor_work_plan_schedule WHERE work_plan_id=$1 ORDER BY slot ASC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	slots := make([]schedule.ScheduleSlot, 0)
	for rows.Next() {
		var s schedule.ScheduleSlot
		var maxV, num sql.NullInt16
		if err := rows.Scan(&s.ID, &s.WorkPlanID, &s.Slot, &maxV, &num); err != nil {
			return nil, err
		}
		s.Maximum = int16Val(maxV)
		s.Used = int16Val(num)
		s.Recalculate()
		slots = append(slots, s)
	}
	return slots, rows.Err()
}

// mapInsertPlanError 把重复键冲突映射为领域错误 ErrPlanExists（先显式 TxHasPlan 检查，
// 这里兜底并发插入的唯一冲突）。
func mapInsertPlanError(err error, p schedule.WorkPlan) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: doctor=%d dept_sub=%d date=%s", schedule.ErrPlanExists, p.DoctorID, p.SubdepartmentID, p.Date)
	}
	return err
}

// int16Val 把可空的 SMALLINT 值转为 int16（NULL 视为 0，num/maximum 在业务上必有值）。
func int16Val(v sql.NullInt16) int16 {
	if v.Valid {
		return v.Int16
	}
	return 0
}

// executeTx 在单个事务中执行 fn；fn 返回错误时统一回滚，避免留下半截写入。
func (r *PostgresScheduleRepository) executeTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if r == nil || r.db == nil {
		return sql.ErrConnDone
	}
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

// ListSlotsByPlan 返回某计划全部时段，按 slot 升序，并计算 remaining。
// 计划存在性与时段读取在同一条 SQL 中完成：以排班计划表为主表 LEFT JOIN 时段表，
// 计划不存在时无任何行（返回 ErrPlanNotFound）；计划存在但无时段时只有一行
// 全 NULL 时段（返回空列表），不存在“先查存在再读列表”的并发删除窗口。
func (r *PostgresScheduleRepository) ListSlotsByPlan(ctx context.Context, planID int64) ([]schedule.ScheduleSlot, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	const query = `SELECT s.id, s.work_plan_id, s.slot, s.maximum, s.num
FROM hospital.doctor_work_plan p
LEFT JOIN hospital.doctor_work_plan_schedule s ON s.work_plan_id = p.id
WHERE p.id = $1
ORDER BY s.slot ASC, s.id ASC`
	rows, err := r.db.QueryContext(ctx, query, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]schedule.ScheduleSlot, 0)
	planRowSeen := false
	for rows.Next() {
		planRowSeen = true
		var slot schedule.ScheduleSlot
		var slotID, workPlanID sql.NullInt64
		var slotNo, maximum, used sql.NullInt16
		if err := rows.Scan(&slotID, &workPlanID, &slotNo, &maximum, &used); err != nil {
			return nil, err
		}
		// 计划存在但没有任何时段：LEFT JOIN 会产生一行时段字段全为 NULL 的记录。
		if !slotID.Valid {
			continue
		}
		slot.ID, slot.WorkPlanID = slotID.Int64, workPlanID.Int64
		slot.Slot, slot.Maximum, slot.Used = slotNo.Int16, maximum.Int16, used.Int16
		slot.Recalculate()
		result = append(result, slot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !planRowSeen {
		return nil, schedule.ErrPlanNotFound
	}
	return result, nil
}

// scheduleSlotScanner 兼容 *sql.Row 与 *sql.Rows，用于统一解析一条时段记录。
type scheduleSlotScanner interface {
	Scan(dest ...any) error
}

// scanScheduleSlot 解析 doctor_work_plan_schedule 一行并计算 remaining=maximum-num。
// slot/maximum/num 在表结构中可为 NULL：对历史脏数据按 0 折算（GET 列表同规则），
// 避免 PATCH/DELETE 锁行读取时 Scan 报错；新建/更新 RETURNING 的行恒有值，不受影响。
func scanScheduleSlot(scanner scheduleSlotScanner) (schedule.ScheduleSlot, error) {
	var item schedule.ScheduleSlot
	var slotNo, maximum, used sql.NullInt16
	if err := scanner.Scan(&item.ID, &item.WorkPlanID, &slotNo, &maximum, &used); err != nil {
		return schedule.ScheduleSlot{}, err
	}
	item.Slot = nullInt16(slotNo)
	item.Maximum = nullInt16(maximum)
	item.Used = nullInt16(used)
	item.Recalculate()
	return item, nil
}

// nullInt16 把可空 SMALLINT 值转为 int16（NULL 视为 0）。
func nullInt16(value sql.NullInt16) int16 {
	if value.Valid {
		return value.Int16
	}
	return 0
}

// planDateBySlot 事务内锁定排班计划行并返回日期字符串（YYYY-MM-DD）。
// 加锁顺序统一为“先父计划、后子时段”，与删除计划（锁计划后级联删除时段）一致，
// 避免反向加锁引发死锁。计划不存在返回 ErrPlanNotFound。
func planDateBySlot(ctx context.Context, tx *sql.Tx, planID int64) (string, error) {
	var date string
	err := tx.QueryRowContext(ctx, "SELECT date::text FROM hospital.doctor_work_plan WHERE id=$1 FOR UPDATE", planID).Scan(&date)
	if errors.Is(err, sql.ErrNoRows) {
		return "", schedule.ErrPlanNotFound
	}
	if err != nil {
		return "", err
	}
	return date, nil
}

// slotPlanID 读取时段所属计划编号（不取行锁）。
// 更新/删除时段前先用它确定父计划，再按“父计划 -> 时段”顺序加锁，
// 与 planDateBySlot 的注释约定保持一致，避免锁序反转造成死锁。
func slotPlanID(ctx context.Context, tx *sql.Tx, slotID int64) (int64, error) {
	var planID int64
	err := tx.QueryRowContext(ctx, "SELECT work_plan_id FROM hospital.doctor_work_plan_schedule WHERE id=$1", slotID).Scan(&planID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, schedule.ErrSlotNotFound
	}
	if err != nil {
		return 0, err
	}
	return planID, nil
}

// lockSlot 事务内锁定时段并返回完整记录；不存在返回 ErrSlotNotFound。
func lockSlot(ctx context.Context, tx *sql.Tx, slotID int64) (schedule.ScheduleSlot, error) {
	row := tx.QueryRowContext(ctx, "SELECT id, work_plan_id, slot, maximum, num FROM hospital.doctor_work_plan_schedule WHERE id=$1 FOR UPDATE", slotID)
	item, err := scanScheduleSlot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return schedule.ScheduleSlot{}, schedule.ErrSlotNotFound
	}
	if err != nil {
		return schedule.ScheduleSlot{}, err
	}
	return item, nil
}

// hasRegistrations 报告某时段是否已有挂号记录（medical_registration）。
func (r *PostgresScheduleRepository) hasRegistrations(ctx context.Context, tx *sql.Tx, slotID int64) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM hospital.medical_registration WHERE doctor_schedule_id=$1)", slotID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// CreateSlot 事务内创建时段：计划不存在返回 ErrPlanNotFound；
// 计划已开始/已结束返回 ErrSlotLocked；同计划时段重复返回 ErrSlotExists；
// 参数不合法返回对应校验错误；成功后返回含 remaining 的完整资源。
func (r *PostgresScheduleRepository) CreateSlot(ctx context.Context, value schedule.ScheduleSlot, now time.Time) (*schedule.ScheduleSlot, error) {
	if err := value.ValidateNew(); err != nil {
		return nil, err
	}
	var result *schedule.ScheduleSlot
	err := r.executeTx(ctx, func(tx *sql.Tx) error {
		date, err := planDateBySlot(ctx, tx, value.WorkPlanID)
		if err != nil {
			return err
		}
		plan := schedule.WorkPlan{Date: date}
		if !plan.CanModify(now) {
			return schedule.ErrSlotLocked
		}
		var exists bool
		err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM hospital.doctor_work_plan_schedule WHERE work_plan_id=$1 AND slot=$2)", value.WorkPlanID, value.Slot).Scan(&exists)
		if err != nil {
			return err
		}
		if exists {
			return schedule.ErrSlotExists
		}
		row := tx.QueryRowContext(ctx, "INSERT INTO hospital.doctor_work_plan_schedule(id, work_plan_id, slot, maximum, num) VALUES (nextval('hospital.doctor_work_plan_schedule_sequence'), $1, $2, $3, 0) RETURNING id, work_plan_id, slot, maximum, num", value.WorkPlanID, value.Slot, value.Maximum)
		item, err := scanScheduleSlot(row)
		if err != nil {
			return err
		}
		result = &item
		return nil
	})
	return result, err
}

// UpdateSlotMaximum 事务内更新 maximum。
// 校验顺序：时段存在（ErrSlotNotFound/404）-> 计划未开始（ErrSlotLocked/409 SCHEDULE_SLOT_LOCKED）
// -> 无挂号（ErrSlotLocked）-> 新容量不小于已用号源
// （MaximumBelowUsedError/409 SCHEDULE_CONFLICT，携带 used）。
// 加锁顺序固定为“父计划 -> 时段”，避免与删除计划（级联删除时段）锁序反转导致死锁。
func (r *PostgresScheduleRepository) UpdateSlotMaximum(ctx context.Context, slotID int64, maximum int16, now time.Time) (*schedule.ScheduleSlot, error) {
	var result *schedule.ScheduleSlot
	err := r.executeTx(ctx, func(tx *sql.Tx) error {
		planID, err := slotPlanID(ctx, tx, slotID)
		if err != nil {
			return err
		}
		date, err := planDateBySlot(ctx, tx, planID)
		if err != nil {
			return err
		}
		if !(schedule.WorkPlan{Date: date}).CanModify(now) {
			return schedule.ErrSlotLocked
		}
		// 对时段行加锁以串行化并发挂号（num 变化），并取回当前记录用于容量校验。
		slot, err := lockSlot(ctx, tx, slotID)
		if err != nil {
			return err
		}
		registered, err := r.hasRegistrations(ctx, tx, slotID)
		if err != nil {
			return err
		}
		if registered {
			return schedule.ErrSlotLocked
		}
		if err := schedule.ValidateMaximum(maximum, slot.Used); err != nil {
			if errors.Is(err, schedule.ErrMaximumBelowUsed) {
				return &schedule.MaximumBelowUsedError{Used: slot.Used}
			}
			return err
		}
		row := tx.QueryRowContext(ctx, "UPDATE hospital.doctor_work_plan_schedule SET maximum=$1 WHERE id=$2 RETURNING id, work_plan_id, slot, maximum, num", maximum, slotID)
		item, err := scanScheduleSlot(row)
		if err != nil {
			return err
		}
		result = &item
		return nil
	})
	return result, err
}

// DeleteSlot 物理删除时段：仅允许无挂号且所属计划尚未开始的时段。
// 已有挂号返回 ErrHasRegistrations，计划已开始返回 ErrSlotLocked，时段不存在返回 ErrSlotNotFound。
// 加锁顺序固定为“父计划 -> 时段”（同 UpdateSlotMaximum），防止与计划删除级联死锁。
func (r *PostgresScheduleRepository) DeleteSlot(ctx context.Context, slotID int64, now time.Time) error {
	return r.executeTx(ctx, func(tx *sql.Tx) error {
		planID, err := slotPlanID(ctx, tx, slotID)
		if err != nil {
			return err
		}
		date, err := planDateBySlot(ctx, tx, planID)
		if err != nil {
			return err
		}
		if !(schedule.WorkPlan{Date: date}).CanModify(now) {
			return schedule.ErrSlotLocked
		}
		// 对时段行加锁以串行化并发挂号（num 变化），行锁返回的完整记录此处用不到。
		if _, err := lockSlot(ctx, tx, slotID); err != nil {
			return err
		}
		registered, err := r.hasRegistrations(ctx, tx, slotID)
		if err != nil {
			return err
		}
		if registered {
			return schedule.ErrHasRegistrations
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM hospital.doctor_work_plan_schedule WHERE id=$1", slotID); err != nil {
			return err
		}
		return nil
	})
}
