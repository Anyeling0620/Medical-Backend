package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"Medical-Web-Backend/internal/domain/doctorpatient"
	domainpatient "Medical-Web-Backend/internal/domain/patient"
	domainregistration "Medical-Web-Backend/internal/domain/registration"
)

// PostgresDoctorPatientRepository 实现「医生本人患者」查询（契约 §6.10）。
//
// 数据来源：patient_user_info_card 作为患者主体，medical_registration.doctor_id
// 限定「本医生接诊过」的范围。日期列沿用挂号仓储的口径统一 ::text 取回，
// 避免驱动把 date 解析成带时区的时刻后在跨日边界上偏移。
type PostgresDoctorPatientRepository struct {
	db *sql.DB
}

func NewPostgresDoctorPatientRepository(db *sql.DB) *PostgresDoctorPatientRepository {
	return &PostgresDoctorPatientRepository{db: db}
}

// doctorPatientFrom 是列表查询的固定来源（别名 c/agg/latest 供其它片段使用）。
//
// agg 先按 doctor_id 收敛再聚合，避免把其它医生的挂号计入患者的就诊次数；
// latest 用 LATERAL 取「本医生处最近一次挂号」的支付状态，
// 按就诊日期倒序、id 倒序取值，保证同一日期多次挂号时结果稳定。
const doctorPatientFrom = `
FROM hospital.patient_user_info_card c
JOIN (
    SELECT r.patient_card_id,
           COUNT(*) AS registration_count,
           MAX(r.date) AS last_visit_date
    FROM hospital.medical_registration r
    WHERE r.doctor_id = $1
    GROUP BY r.patient_card_id
) agg ON agg.patient_card_id = c.id
LEFT JOIN LATERAL (
    SELECT r.payment_status
    FROM hospital.medical_registration r
    WHERE r.patient_card_id = c.id AND r.doctor_id = $1
    ORDER BY r.date DESC NULLS LAST, r.id DESC
    LIMIT 1
) latest ON TRUE`

// doctorPatientCountFrom 是计数查询的固定来源：只需判断「该患者在本医生处是否
// 有挂号」，不需要聚合列与最近一次状态，去掉 LATERAL 可显著减少扫描量。
const doctorPatientCountFrom = `
FROM hospital.patient_user_info_card c
JOIN (
    SELECT r.patient_card_id
    FROM hospital.medical_registration r
    WHERE r.doctor_id = $1
    GROUP BY r.patient_card_id
) agg ON agg.patient_card_id = c.id`

// doctorPatientColumns 是列表输出列，字段顺序与 scanDoctorPatient 一一对应。
const doctorPatientColumns = `
c.id,
COALESCE(c.name, '') AS name,
COALESCE(c.sex, '') AS sex,
COALESCE(c.tel, '') AS tel,
COALESCE(c.birthday::text, '') AS birthday,
COALESCE(c.medical_history, '') AS medical_history,
COALESCE(c.insurance_type, '') AS insurance_type,
agg.registration_count,
COALESCE(agg.last_visit_date::text, '') AS last_visit_date,
latest.payment_status`

// ListDoctorPatients 按过滤条件分页返回该医生接诊过的患者并统计总数。
//
// 总数与列表分两次查询、不在同一事务内：并发写入时 total 与 items 可能略有出入，
// 这是列表接口常见的取舍（避免为大结果集持有长事务）；如需强一致可改用 COUNT(*) OVER ()。
func (r *PostgresDoctorPatientRepository) ListDoctorPatients(
	ctx context.Context,
	f doctorpatient.Filter,
	offset, limit int,
) ([]doctorpatient.Patient, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, sql.ErrConnDone
	}

	where, args := doctorPatientWhere(f)
	var total int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*)"+doctorPatientCountFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := "SELECT " + doctorPatientColumns + doctorPatientFrom + where +
		fmt.Sprintf(" ORDER BY %s LIMIT $%d OFFSET $%d",
			doctorPatientOrderBy(f), len(args)+1, len(args)+2)
	rows, err := r.db.QueryContext(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]doctorpatient.Patient, 0)
	for rows.Next() {
		item, err := scanDoctorPatient(rows)
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

// doctorPatientWhere 构造列表与计数共用的 WHERE 片段与参数。
//
// $1 固定为医生编号（两个 FROM 片段都以它作为第一个参数），关键词条件从 $2 起，
// 保证列表与计数查询的参数顺序一致。
func doctorPatientWhere(f doctorpatient.Filter) (string, []any) {
	args := []any{f.DoctorID}

	pattern := doctorpatient.KeywordPattern(f.Keyword)
	if pattern == "" {
		return "", args
	}
	args = append(args, pattern)
	return fmt.Sprintf(
		` WHERE (c.name ILIKE $%d ESCAPE '\' OR c.tel ILIKE $%d ESCAPE '\')`,
		len(args), len(args)), args
}

// doctorPatientOrderBy 把白名单排序参数映射为 SQL 排序表达式。
//
// 统一追加 NULLS LAST：本视图只有最近就诊日期可能为空（挂号记录缺少日期），
// 把空值排在末尾不会让脏数据占据首页。排序表达式后由调用方补 c.id 作为次级键，
// 保证同序值之间的分页结果稳定。
func doctorPatientOrderBy(f doctorpatient.Filter) string {
	expression := "agg.last_visit_date"
	switch f.Sort {
	case doctorpatient.SortName:
		expression = "COALESCE(c.name, '')"
	case doctorpatient.SortRegistrationCount:
		expression = "agg.registration_count"
	}

	direction := "DESC"
	if strings.EqualFold(f.Order, "asc") {
		direction = "ASC"
	}
	return fmt.Sprintf("%s %s NULLS LAST, c.id ASC", expression, direction)
}

// scanDoctorPatient 把一行聚合结果转换为领域实体。
// 支付状态为空（脏数据缺少 payment_status）时按未付款收敛，与挂号域的映射口径一致。
func scanDoctorPatient(scanner registrationScanner) (doctorpatient.Patient, error) {
	var (
		item              doctorpatient.Patient
		lastVisitDate     string
		rawMedicalHistory string
		paymentStatus     sql.NullInt16
	)
	if err := scanner.Scan(
		&item.PatientCardID,
		&item.Name,
		&item.Sex,
		&item.Tel,
		&item.Birthday,
		&rawMedicalHistory,
		&item.InsuranceType,
		&item.RegistrationCount,
		&lastVisitDate,
		&paymentStatus,
	); err != nil {
		return doctorpatient.Patient{}, err
	}
	item.LastVisitDate = lastVisitDate
	// 库中疾病史是 JSON 文本（如 ["高血压"]），按契约 §2 解析为字符串数组再对外暴露。
	item.MedicalHistory = domainpatient.ParseMedicalHistory(rawMedicalHistory)
	item.LastPaymentStatus = domainregistration.PaymentStatusLabel(nullInt16(paymentStatus))
	return item, nil
}
