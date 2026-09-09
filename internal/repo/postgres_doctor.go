package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/domain/doctor"
)

// PostgresDoctorRepository implements doctor search persistence in PostgreSQL.
type PostgresDoctorRepository struct {
	db *sql.DB
}

func (r *PostgresDoctorRepository) ListDepartments(ctx context.Context, f catalog.DepartmentFilter, offset, limit int) ([]catalog.Department, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, sql.ErrConnDone
	}
	where, args := []string{"1=1"}, []any{}
	if f.Outpatient != nil {
		args = append(args, *f.Outpatient)
		where = append(where, fmt.Sprintf("outpatient=$%d", len(args)))
	}
	if f.Recommended != nil {
		args = append(args, *f.Recommended)
		where = append(where, fmt.Sprintf("recommended=$%d", len(args)))
	}
	base := " FROM hospital.medical_dept WHERE " + strings.Join(where, " AND ")
	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*)"+base, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	column := "name"
	if f.Sort == "id" {
		column = "id"
	}
	direction := "ASC"
	if strings.EqualFold(f.Order, "desc") {
		direction = "DESC"
	}
	args = append(args, limit, offset)
	q := fmt.Sprintf("SELECT id,name,outpatient,description,recommended%s ORDER BY %s %s LIMIT $%d OFFSET $%d", base, column, direction, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]catalog.Department, 0)
	for rows.Next() {
		var d catalog.Department
		var name, desc sql.NullString
		var out, rec sql.NullBool
		if err := rows.Scan(&d.ID, &name, &out, &desc, &rec); err != nil {
			return nil, 0, err
		}
		d.Name = name.String
		d.Description = desc.String
		d.Outpatient = out.Valid && out.Bool
		d.Recommended = rec.Valid && rec.Bool
		items = append(items, d)
	}
	return items, total, rows.Err()
}

func (r *PostgresDoctorRepository) FindDepartment(ctx context.Context, id int64) (*catalog.Department, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	var d catalog.Department
	var name, desc sql.NullString
	var out, rec sql.NullBool
	err := r.db.QueryRowContext(ctx, "SELECT id,name,outpatient,description,recommended FROM hospital.medical_dept WHERE id=$1", id).Scan(&d.ID, &name, &out, &desc, &rec)
	if err != nil {
		return nil, err
	}
	d.Name = name.String
	d.Description = desc.String
	d.Outpatient = out.Valid && out.Bool
	d.Recommended = rec.Valid && rec.Bool
	return &d, nil
}

func (r *PostgresDoctorRepository) ListSubdepartments(ctx context.Context, departmentID int64, offset, limit int) ([]catalog.Subdepartment, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, sql.ErrConnDone
	}
	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM hospital.medical_dept_sub WHERE dept_id=$1", departmentID).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		var exists bool
		if err := r.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM hospital.medical_dept WHERE id=$1)", departmentID).Scan(&exists); err != nil {
			return nil, 0, err
		}
		if !exists {
			return nil, 0, sql.ErrNoRows
		}
	}
	rows, err := r.db.QueryContext(ctx, "SELECT id,name,dept_id,location FROM hospital.medical_dept_sub WHERE dept_id=$1 ORDER BY name ASC,id ASC LIMIT $2 OFFSET $3", departmentID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]catalog.Subdepartment, 0)
	for rows.Next() {
		var s catalog.Subdepartment
		var name, loc sql.NullString
		if err := rows.Scan(&s.ID, &name, &s.DepartmentID, &loc); err != nil {
			return nil, 0, err
		}
		s.Name = name.String
		s.Location = loc.String
		items = append(items, s)
	}
	return items, total, rows.Err()
}

func NewPostgresDoctorRepository(db *sql.DB) *PostgresDoctorRepository {
	return &PostgresDoctorRepository{db: db}
}

func (r *PostgresDoctorRepository) Search(
	ctx context.Context,
	filters doctor.SearchFilters,
	offset int,
	limit int,
) ([]doctor.Doctor, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}

	query, args := buildDoctorQuery(filters, true)
	args = append(args, limit, offset)
	query += fmt.Sprintf("\nLIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]doctor.Doctor, 0)
	for rows.Next() {
		item, err := scanDoctor(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *PostgresDoctorRepository) Count(
	ctx context.Context,
	filters doctor.SearchFilters,
) (int64, error) {
	if r == nil || r.db == nil {
		return 0, sql.ErrConnDone
	}

	query, args := buildDoctorQuery(filters, false)
	var count int64
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func buildDoctorQuery(filters doctor.SearchFilters, list bool) (string, []any) {
	args := make([]any, 0, 7)
	where := make([]string, 0, 6)

	if filters.Name != nil {
		where = append(where, fmt.Sprintf("d.name LIKE $%d", len(args)+1))
		args = append(args, "%"+*filters.Name+"%")
	}
	if filters.DeptID != nil {
		where = append(where, fmt.Sprintf("md.id = $%d", len(args)+1))
		args = append(args, *filters.DeptID)
	}
	if filters.Degree != nil {
		where = append(where, fmt.Sprintf("d.degree = $%d", len(args)+1))
		args = append(args, *filters.Degree)
	}
	if filters.Job != nil {
		where = append(where, fmt.Sprintf("d.job = $%d", len(args)+1))
		args = append(args, *filters.Job)
	}
	if filters.Recommended != nil {
		where = append(where, fmt.Sprintf("d.recommended = $%d", len(args)+1))
		args = append(args, *filters.Recommended)
	}
	if filters.Status != nil {
		where = append(where, fmt.Sprintf("d.status = $%d", len(args)+1))
		args = append(args, *filters.Status)
	}

	base := `
FROM hospital.doctor d
JOIN hospital.medical_dept_sub_and_doctor sd ON sd.doctor_id = d.id
JOIN hospital.medical_dept_sub ds ON sd.dept_sub_id = ds.id
JOIN hospital.medical_dept md ON ds.dept_id = md.id
WHERE 1 = 1`
	if len(where) > 0 {
		base += "\nAND " + strings.Join(where, "\nAND ")
	}

	if !list {
		return "SELECT COUNT(*)" + base, args
	}

	query := `SELECT d.id, d.name, d.sex, d.tel, d.school, d.degree, d.job,
       md.name AS dept_name, ds.name AS sub_name, d.recommended, d.status` + base
	if filters.Order == nil {
		query += "\nORDER BY d.name ASC"
	} else if *filters.Order == "ASC" {
		query += "\nORDER BY md.id ASC"
	} else {
		query += "\nORDER BY md.id DESC"
	}
	// The association ID makes pages deterministic even for doctors in multiple clinics.
	query += ", d.id ASC, sd.id ASC"
	return query, args
}

type doctorScanner interface {
	Scan(dest ...any) error
}

func scanDoctor(scanner doctorScanner) (doctor.Doctor, error) {
	var result doctor.Doctor
	var name, sex, tel, school, degree, job, deptName, subName sql.NullString
	var recommended sql.NullBool
	var status sql.NullInt16

	err := scanner.Scan(
		&result.ID,
		&name,
		&sex,
		&tel,
		&school,
		&degree,
		&job,
		&deptName,
		&subName,
		&recommended,
		&status,
	)
	if err != nil {
		return doctor.Doctor{}, err
	}

	result.Name = name.String
	result.Sex = sex.String
	result.Tel = strings.TrimRight(tel.String, " ")
	result.School = school.String
	result.Degree = degree.String
	result.Job = job.String
	result.DeptName = deptName.String
	result.SubName = subName.String
	result.Recommended = recommended.Valid && recommended.Bool
	if status.Valid {
		result.Status = status.Int16
	}
	return result, nil
}

func (r *PostgresDoctorRepository) FindByID(
	ctx context.Context,
	id int64,
) (*doctor.DoctorDetail, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}

	// 实际数据库字段没有使用双引号，使用参数化查询避免 SQL 注入。
	const query = `
SELECT photo,
       pid,
       birthday,
       uuid,
       hiredate,
       email,
       remark,
       tag,
       address,
       description
FROM hospital.doctor
WHERE id = $1`

	var result doctor.DoctorDetail

	var photo sql.NullString
	var pid sql.NullString
	var birthday sql.NullTime
	var uuid sql.NullString
	var hiredate sql.NullTime
	var email sql.NullString
	var remark sql.NullString
	var tag sql.NullString
	var address sql.NullString
	var description sql.NullString

	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&photo,
		&pid,
		&birthday,
		&uuid,
		&hiredate,
		&email,
		&remark,
		&tag,
		&address,
		&description,
	)
	if err != nil {
		return nil, err
	}

	result.Photo = photo.String
	result.PID = pid.String
	result.Birthday = formatDoctorDate(birthday)
	result.UUID = uuid.String
	result.Hiredate = formatDoctorDate(hiredate)
	result.Email = email.String
	result.Remark = remark.String
	result.Tag = tag.String
	result.Address = address.String
	result.Description = description.String

	return &result, nil
}

func formatDoctorDate(value sql.NullTime) string {
	if !value.Valid {
		return ""
	}

	return value.Time.Format("2006-01-02")
}
func (r *PostgresDoctorRepository) ListDepts(ctx context.Context) ([]string, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}

	const query = `
SELECT DISTINCT name
FROM hospital.medical_dept
WHERE name IS NOT NULL AND name <> ''
ORDER BY name`
	return r.listStrings(ctx, query)
}

func (r *PostgresDoctorRepository) ListDegrees(ctx context.Context) ([]string, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}

	const query = `
SELECT DISTINCT degree
FROM hospital.doctor
WHERE degree IS NOT NULL AND degree <> ''
ORDER BY degree`
	return r.listStrings(ctx, query)
}

func (r *PostgresDoctorRepository) ListJobs(ctx context.Context) ([]string, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}

	const query = `
SELECT DISTINCT job
FROM hospital.doctor
WHERE job IS NOT NULL AND job <> ''
ORDER BY job`
	return r.listStrings(ctx, query)
}

func (r *PostgresDoctorRepository) listStrings(ctx context.Context, query string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]string, 0)
	for rows.Next() {
		var value sql.NullString
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		if value.Valid {
			result = append(result, strings.TrimSpace(value.String))
		}
	}
	return result, rows.Err()
}
