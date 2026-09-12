package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"Medical-Web-Backend/internal/domain/medical_record"
)

// 本文件是病历域（hospital.doctor_prescription）的 PostgreSQL 实现。
//
// 病历用既有的 doctor_prescription 表存储：diagnosis 存诊断，rp 存医生自行整理的正文，
// registration_id 把病历绑定到一次挂号。该表没有 create_time 列，因此列表排序固定为
// id 倒序（最近书写的病历在前），不做时间排序（表结构变更不由代码执行，见 spec/README.md）。

// PostgresMedicalRecordRepository 是病历域的 PostgreSQL 仓储。
type PostgresMedicalRecordRepository struct {
	db *sql.DB
}

// NewPostgresMedicalRecordRepository 构造病历仓储。
func NewPostgresMedicalRecordRepository(db *sql.DB) *PostgresMedicalRecordRepository {
	return &PostgresMedicalRecordRepository{db: db}
}

// medicalRecordColumns 是病历的对外读取列（与 response DTO 一一对应）。
// uuid/diagnosis/rp 是可空列，coalesce 保证扫描到 NULL 时得到空串而不是报错；
// 关联列同样 coalesce 为 0，避免历史脏数据把 NULL 当成有效编号传播出去。
const medicalRecordColumns = `p.id, coalesce(p.uuid, '') AS uuid, coalesce(p.registration_id, 0) AS registration_id,
coalesce(p.patient_card_id, 0) AS patient_card_id, coalesce(p.doctor_id, 0) AS doctor_id,
coalesce(p.sub_dept_id, 0) AS sub_dept_id, coalesce(p.diagnosis, '') AS diagnosis, coalesce(p.rp, '') AS rp`

// medicalRecordListFrom 是列表与计数共用的来源。
//
// 内连接 medical_registration：没有挂号归属的历史病历（registration_id 为空或挂号已删除）
// 不参与本接口，因为它们无法通过「自己负责的挂号记录」这一授权边界判定。
const medicalRecordListFrom = ` FROM hospital.doctor_prescription p
JOIN hospital.medical_registration r ON r.id = p.registration_id`

// FindDoctorIDByUserID 读取 mis_user.ref_id，即管理端账号绑定的医生编号。
func (r *PostgresMedicalRecordRepository) FindDoctorIDByUserID(
	ctx context.Context,
	userID int64,
) (int64, error) {
	if r == nil || r.db == nil {
		return 0, sql.ErrConnDone
	}
	var refID sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		"SELECT ref_id FROM hospital.mis_user WHERE id = $1", userID).Scan(&refID)
	if errors.Is(err, sql.ErrNoRows) {
		// 账号不存在（令牌有效但用户已被删除）按「未绑定医生身份」处理，由 use case 拒绝。
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !refID.Valid || refID.Int64 <= 0 {
		return 0, nil
	}
	return refID.Int64, nil
}

// FindRegistrationOwner 读取挂号归属；挂号不存在或不由 ownerDoctorID 负责时按不存在处理。
func (r *PostgresMedicalRecordRepository) FindRegistrationOwner(
	ctx context.Context,
	registrationID, ownerDoctorID int64,
) (*medical_record.RegistrationOwner, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	var owner medical_record.RegistrationOwner
	err := r.db.QueryRowContext(ctx,
		`SELECT r.id, coalesce(r.patient_card_id, 0), coalesce(r.doctor_id, 0), coalesce(r.dept_sub_id, 0)
		   FROM hospital.medical_registration r
		  WHERE r.id = $1 AND r.doctor_id = $2`,
		registrationID, ownerDoctorID).Scan(
		&owner.RegistrationID, &owner.PatientCardID, &owner.DoctorID, &owner.SubdepartmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, medical_record.ErrRegistrationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &owner, nil
}

// CreateMedicalRecord 在单事务内完成「锁定挂号行 -> 校验同一挂号尚无病历 -> 插入病历」。
//
// patient_card_id、doctor_id、sub_dept_id 取自挂号行（owner 由 use case 在同一事务外读取后传入，
// 这里再次 FOR UPDATE 锁定挂号行做并发串行化），客户端无法把病历写到他人名下。
func (r *PostgresMedicalRecordRepository) CreateMedicalRecord(
	ctx context.Context,
	owner medical_record.RegistrationOwner,
	recordUUID, diagnosis, content string,
) (*medical_record.MedicalRecord, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 锁定挂号行：同一挂号的并发书写在这里串行化，后到者会在下面的存在性检查上得到 409 而非重复写入。
	// 锁语句同时复核归属：即使 use case 读归属后挂号被改派，也不会把病历写到新医生的名下。
	var registrationID int64
	err = tx.QueryRowContext(ctx,
		"SELECT id FROM hospital.medical_registration WHERE id = $1 AND doctor_id = $2 FOR UPDATE",
		owner.RegistrationID, owner.DoctorID).Scan(&registrationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, medical_record.ErrRegistrationNotFound
	}
	if err != nil {
		return nil, err
	}

	// 同一挂号只允许一份病历：该表没有唯一约束，因此在持锁事务内做存在性检查。
	var exists int
	err = tx.QueryRowContext(ctx,
		"SELECT 1 FROM hospital.doctor_prescription WHERE registration_id = $1 LIMIT 1",
		owner.RegistrationID).Scan(&exists)
	if err == nil {
		return nil, medical_record.ErrDuplicate
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	record := &medical_record.MedicalRecord{
		UUID:            recordUUID,
		RegistrationID:  owner.RegistrationID,
		PatientCardID:   owner.PatientCardID,
		DoctorID:        owner.DoctorID,
		SubdepartmentID: owner.SubdepartmentID,
		Diagnosis:       diagnosis,
		Content:         content,
	}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO hospital.doctor_prescription
(id, uuid, patient_card_id, diagnosis, sub_dept_id, doctor_id, registration_id, rp)
VALUES (nextval('hospital.doctor_prescription_sequence'), $1, $2, $3, $4, $5, $6, $7)
RETURNING id`,
		recordUUID, nullInt64(owner.PatientCardID), diagnosis,
		nullInt64(owner.SubdepartmentID), nullInt64(owner.DoctorID),
		owner.RegistrationID, content).Scan(&record.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return record, nil
}

// ListMedicalRecords 按过滤条件分页返回病历并统计总数（固定按 id 倒序）。
func (r *PostgresMedicalRecordRepository) ListMedicalRecords(
	ctx context.Context,
	f medical_record.Filter,
	offset, limit int,
) ([]medical_record.MedicalRecord, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, sql.ErrConnDone
	}
	where, args := medicalRecordWhere(f)

	var total int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*)"+medicalRecordListFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := "SELECT " + medicalRecordColumns + medicalRecordListFrom + where +
		fmt.Sprintf(" ORDER BY p.id DESC LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	rows, err := r.db.QueryContext(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]medical_record.MedicalRecord, 0)
	for rows.Next() {
		item, scanErr := scanMedicalRecord(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// FindMedicalRecord 读取病历详情；不存在或不属于 ownerDoctorID 负责的挂号时返回 ErrNotFound。
func (r *PostgresMedicalRecordRepository) FindMedicalRecord(
	ctx context.Context,
	id, ownerDoctorID int64,
) (*medical_record.MedicalRecord, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	item, err := scanMedicalRecord(r.db.QueryRowContext(ctx,
		"SELECT "+medicalRecordColumns+medicalRecordListFrom+
			" WHERE p.id = $1 AND r.doctor_id = $2", id, ownerDoctorID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, medical_record.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return item, nil
}

// UpdateMedicalRecord 只更新提交的字段（PATCH 语义），返回更新后的病历。
//
// 归属校验与更新在同一条语句内完成：EXISTS 子句保证只有「自己负责的挂号」下的病历会被更新，
// 越权与不存在都命中 0 行并统一按 ErrNotFound 返回。
func (r *PostgresMedicalRecordRepository) UpdateMedicalRecord(
	ctx context.Context,
	id, ownerDoctorID int64,
	input medical_record.UpdateInput,
) (*medical_record.MedicalRecord, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	var diagnosisArg, contentArg any
	if input.Diagnosis != nil {
		diagnosisArg = *input.Diagnosis
	}
	if input.Content != nil {
		contentArg = *input.Content
	}
	item, err := scanMedicalRecord(r.db.QueryRowContext(ctx,
		`UPDATE hospital.doctor_prescription p
		    SET diagnosis = COALESCE($2::varchar, p.diagnosis),
		        rp = COALESCE($3::varchar, p.rp)
		  WHERE p.id = $1
		    AND EXISTS (SELECT 1 FROM hospital.medical_registration r
		                 WHERE r.id = p.registration_id AND r.doctor_id = $4)
		RETURNING p.id, coalesce(p.uuid, ''), coalesce(p.registration_id, 0), coalesce(p.patient_card_id, 0),
		          coalesce(p.doctor_id, 0), coalesce(p.sub_dept_id, 0), coalesce(p.diagnosis, ''), coalesce(p.rp, '')`,
		id, diagnosisArg, contentArg, ownerDoctorID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, medical_record.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return item, nil
}

// DeleteMedicalRecord 删除病历；不存在或不属于 ownerDoctorID 负责的挂号时返回 ErrNotFound。
func (r *PostgresMedicalRecordRepository) DeleteMedicalRecord(
	ctx context.Context,
	id, ownerDoctorID int64,
) error {
	if r == nil || r.db == nil {
		return sql.ErrConnDone
	}
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM hospital.doctor_prescription p
		  WHERE p.id = $1
		    AND EXISTS (SELECT 1 FROM hospital.medical_registration r
		                 WHERE r.id = p.registration_id AND r.doctor_id = $2)`,
		id, ownerDoctorID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return medical_record.ErrNotFound
	}
	return nil
}

// medicalRecordWhere 构造列表与计数共用的 WHERE 片段与参数。
// 所有条件都是可选的；OwnerDoctorID 是调用者身份的强制归属条件（他人病历不可见）。
func medicalRecordWhere(f medical_record.Filter) (string, []any) {
	conditions := make([]string, 0, 4)
	args := make([]any, 0, 4)
	// 归属条件是强制条件（fail closed）：误传 0 只会匹配不到记录，
	// 不会退化成「不限定归属」的全量查询。
	args = append(args, f.OwnerDoctorID)
	conditions = append(conditions, fmt.Sprintf("r.doctor_id = $%d", len(args)))
	if f.RegistrationID != nil {
		args = append(args, *f.RegistrationID)
		conditions = append(conditions, fmt.Sprintf("p.registration_id = $%d", len(args)))
	}
	if f.PatientCardID != nil {
		args = append(args, *f.PatientCardID)
		conditions = append(conditions, fmt.Sprintf("p.patient_card_id = $%d", len(args)))
	}
	if f.DoctorID != nil {
		// 与归属条件取同一列（挂号的 doctor_id），避免历史脏数据下「列表可见但按 doctorId 过滤不可见」。
		args = append(args, *f.DoctorID)
		conditions = append(conditions, fmt.Sprintf("r.doctor_id = $%d", len(args)))
	}
	// 防御性兜底：归属条件是强制条件，正常调用下 conditions 永不为空；
	// 保留该分支是为了让空条件不会拼出非法的 " WHERE " 语句。
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// scanMedicalRecord 把一行查询结果投影为病历实体。
func scanMedicalRecord(row interface{ Scan(dest ...any) error }) (*medical_record.MedicalRecord, error) {
	item := &medical_record.MedicalRecord{}
	if err := row.Scan(
		&item.ID, &item.UUID, &item.RegistrationID, &item.PatientCardID,
		&item.DoctorID, &item.SubdepartmentID, &item.Diagnosis, &item.Content,
	); err != nil {
		return nil, err
	}
	return item, nil
}

// nullInt64 把 int64 转为可空 bigint 参数：0 视为 NULL，避免把「没有归属」写成 0 号外键。
func nullInt64(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}
