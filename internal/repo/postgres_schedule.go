package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"Medical-Web-Backend/internal/domain/schedule"
)

// PostgresScheduleRepository 实现排班时段（doctor_work_plan_schedule）的 PostgreSQL 持久化。
// 时段创建、更新与删除都在事务内用 SELECT ... FOR UPDATE 串行化，
// 保证同一计划下时段编号不重复、未来排班校验与容量约束的原子性。
type PostgresScheduleRepository struct {
	db *sql.DB
}

func NewPostgresScheduleRepository(db *sql.DB) *PostgresScheduleRepository {
	return &PostgresScheduleRepository{db: db}
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
