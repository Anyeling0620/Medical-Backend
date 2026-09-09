package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
)

// PostgresScheduleRepository 实现排班计划的 PostgreSQL 持久化。
// 列表/详情直接读表；写操作经 ExecTx 在事务内完成，状态规则由 use case 判定后调用。
type PostgresScheduleRepository struct {
	db *sql.DB
}

// NewPostgresScheduleRepository 构造排班计划仓库。
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
func (t *postgresScheduleTx) TxDeletePlan(ctx context.Context, planID int64) error {
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
