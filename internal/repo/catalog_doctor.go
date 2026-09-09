package repo

import (
	"context"
	"database/sql"

	"Medical-Web-Backend/internal/domain/catalog"
)

func (r *PostgresDoctorRepository) FindDoctor(ctx context.Context, id int64) (*catalog.DoctorCatalogDetail, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	const query = `SELECT id,name,sex,photo,birthday,school,degree,job,remark,description,hiredate,tag,recommended,status,create_time FROM hospital.doctor WHERE id=$1`
	var d catalog.DoctorCatalog
	var photo, birthday, school, degree, job, remark, description, hiredate, tag sql.NullString
	var recommended sql.NullBool
	var status sql.NullInt16
	var createTime sql.NullTime
	if err := r.db.QueryRowContext(ctx, query, id).Scan(&d.ID, &d.Name, &d.Sex, &photo, &birthday, &school, &degree, &job, &remark, &description, &hiredate, &tag, &recommended, &status, &createTime); err != nil {
		return nil, err
	}
	d.PhotoURL = photo.String
	d.Birthday, d.School, d.Degree, d.Job = birthday.String, school.String, degree.String, job.String
	d.Remark, d.Description, d.HireDate = remark.String, description.String, hiredate.String
	d.Tags = catalog.ParseTags(tag.String)
	d.Recommended = recommended.Valid && recommended.Bool
	d.Status = doctorStatus(status)
	d.CreateDate = catalogDate(createTime)
	detail := &catalog.DoctorCatalogDetail{DoctorCatalog: d, Subdepartments: make([]catalog.SubdepartmentOption, 0), Prices: make([]catalog.DoctorPrice, 0)}
	rows, err := r.db.QueryContext(ctx, `SELECT ds.id,ds.name,ds.dept_id FROM hospital.medical_dept_sub_and_doctor sd JOIN hospital.medical_dept_sub ds ON ds.id=sd.dept_sub_id WHERE sd.doctor_id=$1 ORDER BY ds.name,ds.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item catalog.SubdepartmentOption
		if err := rows.Scan(&item.ID, &item.Name, &item.DepartmentID); err != nil {
			return nil, err
		}
		detail.Subdepartments = append(detail.Subdepartments, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	prices, _, err := r.ListDoctorPrices(ctx, id, 0, 100)
	if err != nil {
		return nil, err
	}
	detail.Prices = prices
	return detail, nil
}

func (r *PostgresDoctorRepository) ListDoctorOptions(ctx context.Context) (catalog.DoctorOptions, error) {
	if r == nil || r.db == nil {
		return catalog.DoctorOptions{}, sql.ErrConnDone
	}
	result := catalog.DoctorOptions{Departments: make([]catalog.DepartmentOption, 0), Subdepartments: make([]catalog.SubdepartmentOption, 0), Jobs: make([]string, 0), Degrees: make([]string, 0)}
	rows, err := r.db.QueryContext(ctx, `SELECT id,name FROM hospital.medical_dept WHERE name IS NOT NULL AND name<>'' ORDER BY name,id`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var x catalog.DepartmentOption
		if err := rows.Scan(&x.ID, &x.Name); err != nil {
			rows.Close()
			return result, err
		}
		result.Departments = append(result.Departments, x)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	rows, err = r.db.QueryContext(ctx, `SELECT id,name,dept_id FROM hospital.medical_dept_sub WHERE name IS NOT NULL AND name<>'' ORDER BY name,id`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var x catalog.SubdepartmentOption
		if err := rows.Scan(&x.ID, &x.Name, &x.DepartmentID); err != nil {
			rows.Close()
			return result, err
		}
		result.Subdepartments = append(result.Subdepartments, x)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	result.Jobs, err = r.listStrings(ctx, `SELECT DISTINCT job FROM hospital.doctor WHERE job IS NOT NULL AND job<>'' ORDER BY job`)
	if err != nil {
		return result, err
	}
	result.Degrees, err = r.listStrings(ctx, `SELECT DISTINCT degree FROM hospital.doctor WHERE degree IS NOT NULL AND degree<>'' ORDER BY degree`)
	return result, err
}

func (r *PostgresDoctorRepository) ListDoctorPrices(ctx context.Context, doctorID int64, offset, limit int) ([]catalog.DoctorPrice, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, sql.ErrConnDone
	}
	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hospital.doctor_price WHERE doctor_id=$1`, doctorID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id,doctor_id,level,price_1,price_2 FROM hospital.doctor_price WHERE doctor_id=$1 ORDER BY id LIMIT $2 OFFSET $3`, doctorID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]catalog.DoctorPrice, 0)
	for rows.Next() {
		var x catalog.DoctorPrice
		var level, p1, p2 sql.NullString
		if err := rows.Scan(&x.ID, &x.DoctorID, &level, &p1, &p2); err != nil {
			return nil, 0, err
		}
		x.Level, x.Price1, x.Price2 = level.String, p1.String, p2.String
		items = append(items, x)
	}
	return items, total, rows.Err()
}

func doctorStatus(status sql.NullInt16) string {
	if !status.Valid {
		return ""
	}
	switch status.Int16 {
	case 1:
		return "ACTIVE"
	case 2:
		return "RESIGNED"
	case 3:
		return "RETIRED"
	case 4:
		return "HIDDEN"
	default:
		return ""
	}
}
func catalogDate(value sql.NullTime) string {
	if !value.Valid {
		return ""
	}
	return value.Time.Format("2006-01-02")
}
