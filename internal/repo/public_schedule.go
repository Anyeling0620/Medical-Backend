package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"Medical-Web-Backend/internal/domain/schedule"
)

// 本文件实现匿名公开域的可挂号时段查询（契约 §8.1）。
// 该查询只读：不占用号源、不写幂等记录；返回的字段是公开字段集合，
// 不包含 planId、workPlanId 等排班内部管理字段。

// publicScheduleFrom 是公开时段查询的固定关联：时段 -> 计划 -> 医生 / 子科室。
// 内连接保证脏数据（计划或医生缺失的时段）不会以空医生对象的形式返回给患者端。
const publicScheduleFrom = ` FROM hospital.doctor_work_plan_schedule s
JOIN hospital.doctor_work_plan p ON p.id = s.work_plan_id
JOIN hospital.doctor d ON d.id = p.doctor_id
JOIN hospital.medical_dept_sub ds ON ds.id = p.dept_sub_id`

// publicDoctorPriceSubquery 取该医生的挂号金额。
// amount 的业务口径来自 doctor_price（契约 §8.1）：price_1/price_2 的语义在规格中仍是开放项，
// 本实现按「price1 = 挂号费」落地，且用标量子查询取首行，避免一个医生多条价目把时段行放大。
const publicDoctorPriceSubquery = `(SELECT dp.price_1 FROM hospital.doctor_price dp WHERE dp.doctor_id = d.id ORDER BY dp.id LIMIT 1)`

// ListPublicSchedules 分页返回可挂号时段（含 remaining=0 的满号时段，由前端置灰）。
// 固定按日期、时段、时段编号升序，保证同一查询的分页结果稳定。
func (r *PostgresScheduleRepository) ListPublicSchedules(ctx context.Context, f schedule.PublicScheduleFilter, offset, limit int) ([]schedule.PublicSchedule, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, sql.ErrConnDone
	}
	where, args := publicScheduleWhere(f)
	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*)"+publicScheduleFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `SELECT s.id,p.date::text,s.slot,s.maximum,s.num,
d.id,d.name,d.job,d.degree,d.photo,
ds.id,ds.name,` + publicDoctorPriceSubquery + publicScheduleFrom + where +
		fmt.Sprintf(" ORDER BY p.date ASC, s.slot ASC, s.id ASC LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	args = append(args, limit, offset)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]schedule.PublicSchedule, 0)
	for rows.Next() {
		item, used, err := scanPublicSchedule(rows)
		if err != nil {
			return nil, 0, err
		}
		item.Recalculate(used)
		items = append(items, item)
	}
	return items, total, rows.Err()
}

// publicScheduleWhere 构造公开时段查询的 WHERE 片段。
// 固定条件为「医生在岗（status=1）」：非在岗医生的号源无法完成挂号，不应出现在可挂号列表中。
// subdepartmentId 与 doctorId 同时给出时取交集（契约 §8.1）。
func publicScheduleWhere(f schedule.PublicScheduleFilter) (string, []any) {
	conditions := []string{"d.status = $1"}
	args := []any{publicDoctorActiveStatusCode}
	if f.SubdepartmentID != nil {
		args = append(args, *f.SubdepartmentID)
		conditions = append(conditions, fmt.Sprintf("p.dept_sub_id = $%d", len(args)))
	}
	if f.DoctorID != nil {
		args = append(args, *f.DoctorID)
		conditions = append(conditions, fmt.Sprintf("p.doctor_id = $%d", len(args)))
	}
	if f.FromDate != "" {
		args = append(args, f.FromDate)
		conditions = append(conditions, fmt.Sprintf("p.date >= $%d::date", len(args)))
	}
	if f.ToDate != "" {
		args = append(args, f.ToDate)
		conditions = append(conditions, fmt.Sprintf("p.date <= $%d::date", len(args)))
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// scanPublicSchedule 读取一行公开时段；第二个返回值是该时段已用号源，
// 仅用于计算 remaining，不进入响应体（公开字段集合不含 used）。
func scanPublicSchedule(scanner interface{ Scan(dest ...any) error }) (schedule.PublicSchedule, int16, error) {
	var item schedule.PublicSchedule
	var date string
	var slot, maximum, used sql.NullInt16
	var doctorName, doctorJob, doctorDegree, doctorPhoto sql.NullString
	var subdepartmentName sql.NullString
	var amount sql.NullString
	err := scanner.Scan(
		&item.ScheduleID, &date, &slot, &maximum, &used,
		&item.Doctor.ID, &doctorName, &doctorJob, &doctorDegree, &doctorPhoto,
		&item.Subdepartment.ID, &subdepartmentName, &amount,
	)
	if err != nil {
		return schedule.PublicSchedule{}, 0, err
	}
	item.Date = date
	item.Slot, item.Maximum = nullInt16(slot), nullInt16(maximum)
	item.Doctor.Name, item.Doctor.Job = doctorName.String, doctorJob.String
	item.Doctor.Degree, item.Doctor.PhotoURL = doctorDegree.String, doctorPhoto.String
	item.Subdepartment.Name = subdepartmentName.String
	// 无价目记录时按 0.00 返回：公开接口的 amount 是字符串金额，
	// 返回空串会让前端金额展示与支付流程解析失败，这里给出确定值并在测试中固化。
	if amount.Valid && strings.TrimSpace(amount.String) != "" {
		item.Amount = formatDoctorPrice(amount.String)
	} else {
		item.Amount = "0.00"
	}
	return item, nullInt16(used), nil
}
