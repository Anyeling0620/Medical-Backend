package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"Medical-Web-Backend/internal/domain/catalog"
)

// 本文件实现匿名公开域（/api/v1/public/*）的目录查询。
// 公开域的 SQL 只 SELECT 契约 §2.3 允许的列，敏感列（pid、tel、address、email、uuid 等）
// 不进入查询语句，从数据访问层保证字段裁剪。

// publicDoctorColumns 是公开医生列表与详情共用的列清单。
const publicDoctorColumns = "d.id,d.name,d.sex,d.photo,d.degree,d.job,d.description,d.recommended"

// publicDoctorActiveStatusCode 是 doctor.status=1（ACTIVE，在岗）。
// 公开域只展示在岗医生：隐藏（4）、离职（2）、退休（3）都不出现在匿名接口。
const publicDoctorActiveStatusCode = int16(1)

// ListPublicDepartments 公开科室列表：字段集合与管理端目录完全一致，
// 因此直接复用 ListDepartments，保证两条路径的过滤、排序与分页语义不会漂移。
func (r *PostgresDoctorRepository) ListPublicDepartments(ctx context.Context, f catalog.DepartmentFilter, offset, limit int) ([]catalog.Department, int64, error) {
	return r.ListDepartments(ctx, f, offset, limit)
}

// FindPublicDepartment 公开科室详情（不存在返回 sql.ErrNoRows）。
func (r *PostgresDoctorRepository) FindPublicDepartment(ctx context.Context, id int64) (*catalog.Department, error) {
	return r.FindDepartment(ctx, id)
}

// ListPublicSubdepartments 公开子科室列表；科室不存在时返回 sql.ErrNoRows（由 use case 转 404）。
func (r *PostgresDoctorRepository) ListPublicSubdepartments(ctx context.Context, departmentID int64, offset, limit int) ([]catalog.Subdepartment, int64, error) {
	return r.ListSubdepartments(ctx, departmentID, offset, limit)
}

// ListPublicDoctors 分页查询公开医生列表。
func (r *PostgresDoctorRepository) ListPublicDoctors(ctx context.Context, f catalog.PublicDoctorFilter, offset, limit int) ([]catalog.PublicDoctor, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, sql.ErrConnDone
	}
	where, args := publicDoctorWhere(f)
	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM hospital.doctor d"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	column, direction := publicDoctorOrderBy(f)
	args = append(args, limit, offset)
	query := fmt.Sprintf("SELECT "+publicDoctorColumns+
		" FROM hospital.doctor d%s ORDER BY %s %s, d.id ASC LIMIT $%d OFFSET $%d",
		where, column, direction, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]catalog.PublicDoctor, 0)
	for rows.Next() {
		item, err := scanPublicDoctor(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

// FindPublicDoctor 返回公开医生详情（含子科室与价目）。
// 非在岗医生与不存在的医生返回同一结果 sql.ErrNoRows，避免匿名调用方探测管理端状态。
func (r *PostgresDoctorRepository) FindPublicDoctor(ctx context.Context, id int64) (*catalog.PublicDoctorDetail, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	query := "SELECT " + publicDoctorColumns +
		` FROM hospital.doctor d WHERE d.id=$1 AND d.status=$2
AND EXISTS (SELECT 1 FROM hospital.medical_dept_sub_and_doctor sd WHERE sd.doctor_id=d.id)`
	var item catalog.PublicDoctor
	var name, sex, photo, degree, job, description sql.NullString
	var recommended sql.NullBool
	if err := r.db.QueryRowContext(ctx, query, id, publicDoctorActiveStatusCode).
		Scan(&item.ID, &name, &sex, &photo, &degree, &job, &description, &recommended); err != nil {
		return nil, err
	}
	item.Name, item.Sex, item.PhotoURL = name.String, sex.String, photo.String
	item.Degree, item.Job, item.Description = degree.String, job.String, description.String
	item.Recommended = recommended.Valid && recommended.Bool

	detail := &catalog.PublicDoctorDetail{
		PublicDoctor:   item,
		Subdepartments: make([]catalog.PublicSubdepartmentRef, 0),
		Prices:         make([]catalog.PublicDoctorPrice, 0),
	}
	// 子科室：与医生列表的关联条件保持一致，只返回 id 与 name。
	rows, err := r.db.QueryContext(ctx, `SELECT ds.id,ds.name
FROM hospital.medical_dept_sub_and_doctor sd
JOIN hospital.medical_dept_sub ds ON ds.id=sd.dept_sub_id
WHERE sd.doctor_id=$1 ORDER BY ds.name,ds.id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref catalog.PublicSubdepartmentRef
		var refName sql.NullString
		if err := rows.Scan(&ref.ID, &refName); err != nil {
			rows.Close()
			return nil, err
		}
		ref.Name = refName.String
		detail.Subdepartments = append(detail.Subdepartments, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// 价目：公开域不返回 doctor_id，金额统一格式化为两位小数字符串。
	priceRows, err := r.db.QueryContext(ctx,
		`SELECT id,level,price_1,price_2 FROM hospital.doctor_price WHERE doctor_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer priceRows.Close()
	for priceRows.Next() {
		var price catalog.PublicDoctorPrice
		var level, price1, price2 sql.NullString
		if err := priceRows.Scan(&price.ID, &level, &price1, &price2); err != nil {
			return nil, err
		}
		price.Level = level.String
		price.Price1 = formatDoctorPrice(price1.String)
		price.Price2 = formatDoctorPrice(price2.String)
		detail.Prices = append(detail.Prices, price)
	}
	return detail, priceRows.Err()
}

// publicDoctorWhere 构造公开医生列表的 WHERE 片段。
// 固定条件：在岗（status=1）且至少关联一个子科室——未关联子科室的医生不在任何科室下展示。
func publicDoctorWhere(f catalog.PublicDoctorFilter) (string, []any) {
	conditions := []string{
		"d.status = $1",
		"EXISTS (SELECT 1 FROM hospital.medical_dept_sub_and_doctor sd WHERE sd.doctor_id = d.id)",
	}
	args := []any{publicDoctorActiveStatusCode}
	if f.DepartmentID != nil {
		args = append(args, *f.DepartmentID)
		conditions = append(conditions, fmt.Sprintf(`EXISTS (SELECT 1 FROM hospital.medical_dept_sub_and_doctor sd
JOIN hospital.medical_dept_sub ds ON ds.id = sd.dept_sub_id
WHERE sd.doctor_id = d.id AND ds.dept_id = $%d)`, len(args)))
	}
	if f.SubdepartmentID != nil {
		args = append(args, *f.SubdepartmentID)
		conditions = append(conditions, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM hospital.medical_dept_sub_and_doctor sd WHERE sd.doctor_id = d.id AND sd.dept_sub_id = $%d)",
			len(args)))
	}
	if f.Name != nil {
		args = append(args, "%"+*f.Name+"%")
		conditions = append(conditions, fmt.Sprintf("d.name ILIKE $%d", len(args)))
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// publicDoctorOrderBy 把白名单 sort 映射为 SQL 排序列，禁止拼接用户输入。
func publicDoctorOrderBy(f catalog.PublicDoctorFilter) (string, string) {
	column := "d.id"
	switch f.Sort {
	case "name":
		column = "d.name"
	case "hireDate":
		column = "d.hiredate"
	case "recommended":
		column = "d.recommended"
	}
	direction := "ASC"
	if f.Order == "desc" {
		direction = "DESC"
	}
	return column, direction
}

// scanPublicDoctor 读取公开医生列表的一行。
func scanPublicDoctor(scanner interface{ Scan(dest ...any) error }) (catalog.PublicDoctor, error) {
	var item catalog.PublicDoctor
	var name, sex, photo, degree, job, description sql.NullString
	var recommended sql.NullBool
	if err := scanner.Scan(&item.ID, &name, &sex, &photo, &degree, &job, &description, &recommended); err != nil {
		return catalog.PublicDoctor{}, err
	}
	item.Name, item.Sex, item.PhotoURL = name.String, sex.String, photo.String
	item.Degree, item.Job, item.Description = degree.String, job.String, description.String
	item.Recommended = recommended.Valid && recommended.Bool
	return item, nil
}
