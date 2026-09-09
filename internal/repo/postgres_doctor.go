package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"Medical-Web-Backend/internal/domain/catalog"
)

// PostgresDoctorRepository implements doctor catalog persistence in PostgreSQL.
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

func (r *PostgresDoctorRepository) FindSubdepartment(ctx context.Context, id int64) (*catalog.SubdepartmentDetail, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	var item catalog.SubdepartmentDetail
	var name, location, deptName sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT s.id,s.name,s.dept_id,s.location,p.id,p.name FROM hospital.medical_dept_sub s JOIN hospital.medical_dept p ON p.id=s.dept_id WHERE s.id=$1`, id).Scan(&item.ID, &name, &item.DepartmentID, &location, &item.Department.ID, &deptName)
	if err != nil {
		return nil, err
	}
	item.Name = name.String
	item.Location = location.String
	item.Department.Name = deptName.String
	return &item, nil
}

func NewPostgresDoctorRepository(db *sql.DB) *PostgresDoctorRepository {
	return &PostgresDoctorRepository{db: db}
}

// listStrings 供目录 options 接口复用，查询单列字符串列表（职称、学历等）。
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
