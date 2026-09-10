package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"Medical-Web-Backend/internal/domain/patient"
)

// PostgresPatientRepository 实现患者账号（patient_user）、就诊卡
// （patient_user_info_card）与人脸认证记录（patient_face_auth）的持久化。
//
// patient_user.open_id 与 patient_user_info_card.user_id 在库中只有普通索引、
// 没有唯一约束（库结构不可改），因此“按 openId 注册去重”和“每账号最多一张就诊卡”
// 这两条业务唯一性不能交给数据库兜底：两条写入路径都在事务内先取对应键的
// pg_advisory_xact_lock 再重读判断，把并发请求串行化，后到事务必须等先行事务
// 提交后才能继续，从而稳定观察到已写入的行，而不是插入重复数据
// （spec/03-domain-and-state.md §患者与会话域）。
type PostgresPatientRepository struct {
	db *sql.DB
}

// NewPostgresPatientRepository 构造患者域仓库。
func NewPostgresPatientRepository(db *sql.DB) *PostgresPatientRepository {
	return &PostgresPatientRepository{db: db}
}

const (
	// patientUserColumns / patientCardColumns 是实体字段到列的固定映射，
	// 不用 SELECT *，避免列顺序变化时静默串位。
	// pid/uuid/tel 是 CHAR(n) 列，读取时统一 btrim 去掉可能的尾部空格补位。
	patientUserColumns = `id, open_id, nickname, photo, sex, status, create_time::text`
	patientCardColumns = `id, user_id, btrim(uuid) AS uuid, name, sex, btrim(pid) AS pid, btrim(tel) AS tel,
       birthday::text, medical_history, insurance_type, exist_face_model`
)

// patientScanner 抽象 *sql.Row 与 *sql.Rows 共有的单行扫描能力。
type patientScanner interface {
	Scan(dest ...any) error
}

// patientQuerier 抽象 *sql.DB 与 *sql.Tx 共有的单行查询能力，
// 让同一段读取逻辑既能直接在连接池上执行，也能在事务内复用（建卡前的重读）。
type patientQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// FindPatientByID 按主键读取患者账号；不存在返回 patient.ErrPatientNotFound。
func (r *PostgresPatientRepository) FindPatientByID(ctx context.Context, patientID int64) (*patient.Patient, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	return queryPatient(ctx, r.db,
		`SELECT `+patientUserColumns+` FROM hospital.patient_user WHERE id = $1 LIMIT 1`,
		patientID)
}

// FindOrCreatePatientByOpenID 按 openId 查找或注册患者账号。
//
// 实现在单事务内先取 openId 维度的咨询锁再查重：同一 openId 的并发调用会被
// 串行化，先到者插入、后到者等锁释放后读到同一行，返回 created=false，
// 所以“登录与注册合一”不会重复建号，isNewUser 也只会对其中一个调用为 true。
func (r *PostgresPatientRepository) FindOrCreatePatientByOpenID(
	ctx context.Context,
	openID string,
	now time.Time,
) (*patient.Patient, bool, error) {
	if r == nil || r.db == nil {
		return nil, false, sql.ErrConnDone
	}
	if openID == "" {
		return nil, false, patient.ErrOpenIDRequired
	}

	var (
		result  *patient.Patient
		created bool
	)
	err := r.withPatientTx(ctx, func(tx *sql.Tx) error {
		if err := lockPatientKey(ctx, tx, patientRegisterLockKey(openID)); err != nil {
			return err
		}
		existing, err := queryPatientByOpenID(ctx, tx, openID)
		switch {
		case err == nil:
			result, created = existing, false
			return nil
		case !errors.Is(err, patient.ErrPatientNotFound):
			return err
		}
		inserted, err := insertPatient(ctx, tx, openID, now)
		if err != nil {
			return err
		}
		result, created = inserted, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, created, nil
}

// ListCardsByPatient 按 user_id 分页返回就诊卡，按 id 升序并返回总数。
func (r *PostgresPatientRepository) ListCardsByPatient(
	ctx context.Context,
	patientID int64,
	offset, limit int,
) ([]patient.Card, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, sql.ErrConnDone
	}
	// 分页窗口由 repository 主动校验：PostgreSQL 对负 offset/limit 会抛
	// 2201X/2201W，直接冒泡会让入参问题变成 500，与契约 §1.4 的 422 语义不符。
	if offset < 0 || limit < 1 {
		return nil, 0, patient.ErrInvalidPagination
	}
	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM hospital.patient_user_info_card WHERE user_id = $1`,
		patientID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+patientCardColumns+` FROM hospital.patient_user_info_card
WHERE user_id = $1 ORDER BY id ASC LIMIT $2 OFFSET $3`,
		patientID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]patient.Card, 0)
	for rows.Next() {
		item, err := scanCard(rows)
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

// FindCardByID 按主键读取就诊卡；不存在返回 patient.ErrCardNotFound。
func (r *PostgresPatientRepository) FindCardByID(ctx context.Context, cardID int64) (*patient.Card, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	return queryCard(ctx, r.db,
		`SELECT `+patientCardColumns+` FROM hospital.patient_user_info_card WHERE id = $1 LIMIT 1`,
		cardID)
}

// FindCardByPatientID 读取账号名下唯一的就诊卡；没有卡返回 patient.ErrCardNotFound。
func (r *PostgresPatientRepository) FindCardByPatientID(ctx context.Context, patientID int64) (*patient.Card, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	return queryCardByPatientID(ctx, r.db, patientID)
}

// CreateCard 在事务内创建就诊卡，保证“每账号最多一张”。
//
// 取 user_id 维度的咨询锁后再重读：并发的两次建卡只有先到者能插入，
// 后到者观察到已存在的卡并返回 patient.ErrCardExists（409 PATIENT_CARD_EXISTS）。
func (r *PostgresPatientRepository) CreateCard(ctx context.Context, card patient.Card) (*patient.Card, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	history, err := patient.EncodeMedicalHistory(card.MedicalHistory)
	if err != nil {
		return nil, err
	}

	var created *patient.Card
	err = r.withPatientTx(ctx, func(tx *sql.Tx) error {
		if err := lockPatientKey(ctx, tx, patientCardLockKey(card.UserID)); err != nil {
			return err
		}
		if _, err := queryCardByPatientID(ctx, tx, card.UserID); err == nil {
			return patient.ErrCardExists
		} else if !errors.Is(err, patient.ErrCardNotFound) {
			return err
		}
		row := tx.QueryRowContext(ctx, `INSERT INTO hospital.patient_user_info_card
    (id, user_id, uuid, name, sex, pid, tel, birthday, medical_history, insurance_type, exist_face_model)
VALUES (nextval('hospital.patient_user_info_card_sequence'), $1, $2, $3, $4, $5, $6, $7::date, $8, $9, false)
RETURNING `+patientCardColumns,
			card.UserID, card.UUID, card.Name, card.Sex, card.PID, card.Tel,
			card.Birthday, history, card.InsuranceType)
		inserted, err := scanCard(row)
		if err != nil {
			return err
		}
		created = &inserted
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateCard 在事务内按主键 FOR UPDATE 重读就诊卡后应用变更。
//
// UPDATE 只覆盖契约允许修改的列：身份证号、出生日期、user_id、uuid 与
// exist_face_model 在 SQL 层面不参与更新，即使领域校验被绕过也无法改到这些字段。
func (r *PostgresPatientRepository) UpdateCard(
	ctx context.Context,
	cardID int64,
	update patient.CardUpdate,
) (*patient.Card, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}

	var updated *patient.Card
	err := r.withPatientTx(ctx, func(tx *sql.Tx) error {
		current, err := queryCardForUpdate(ctx, tx, cardID)
		if err != nil {
			return err
		}
		next, err := current.ApplyUpdate(update)
		if err != nil {
			return err
		}
		history, err := patient.EncodeMedicalHistory(next.MedicalHistory)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE hospital.patient_user_info_card
SET name = $2, sex = $3, tel = $4, medical_history = $5, insurance_type = $6
WHERE id = $1`,
			cardID, next.Name, next.Sex, next.Tel, history, next.InsuranceType); err != nil {
			return err
		}
		updated = &next
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// ListFaceAuthByCard 返回就诊卡的人脸认证日期记录，最近的在最前。
//
// 以就诊卡为主表 LEFT JOIN 认证记录，用一条 SQL 同时完成“卡是否存在”和“取记录”：
// 卡不存在时没有任何行（返回 patient.ErrCardNotFound）；卡存在但没有记录时只有一行
// 全 NULL 的占位行（返回空列表）。这样既不存在“先查卡再查记录”之间卡被删除的窗口，
// 也少一次往返（与排班域 ListSlotsByPlan 同一做法）。
func (r *PostgresPatientRepository) ListFaceAuthByCard(
	ctx context.Context,
	cardID int64,
) ([]patient.FaceAuthRecord, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT f.id, f.date::text
FROM hospital.patient_user_info_card c
LEFT JOIN hospital.patient_face_auth f ON f.patient_card_id = c.id
WHERE c.id = $1
ORDER BY f.date DESC NULLS LAST, f.id DESC`, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := make([]patient.FaceAuthRecord, 0)
	cardSeen := false
	for rows.Next() {
		cardSeen = true
		var (
			recordID sql.NullInt64
			dateText sql.NullString
		)
		if err := rows.Scan(&recordID, &dateText); err != nil {
			return nil, err
		}
		if !recordID.Valid {
			// LEFT JOIN 的占位行：卡存在，但没有任何人脸认证记录。
			continue
		}
		records = append(records, patient.FaceAuthRecord{
			ID:   recordID.Int64,
			Date: dateText.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !cardSeen {
		return nil, patient.ErrCardNotFound
	}
	return records, nil
}

// queryPatient 执行单行查询并把 sql.ErrNoRows 映射为领域错误 ErrPatientNotFound，
// 避免上层依赖 database/sql 的具体错误。
func queryPatient(ctx context.Context, q patientQuerier, query string, args ...any) (*patient.Patient, error) {
	result, err := scanPatient(q.QueryRowContext(ctx, query, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, patient.ErrPatientNotFound
		}
		return nil, err
	}
	return &result, nil
}

// queryPatientByOpenID 按 openId 读取患者账号；不存在返回 patient.ErrPatientNotFound。
func queryPatientByOpenID(ctx context.Context, q patientQuerier, openID string) (*patient.Patient, error) {
	return queryPatient(ctx, q,
		`SELECT `+patientUserColumns+` FROM hospital.patient_user WHERE open_id = $1 ORDER BY id ASC LIMIT 1`,
		openID)
}

// queryCard 执行单行查询并把 sql.ErrNoRows 映射为领域错误 ErrCardNotFound。
func queryCard(ctx context.Context, q patientQuerier, query string, args ...any) (*patient.Card, error) {
	result, err := scanCard(q.QueryRowContext(ctx, query, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, patient.ErrCardNotFound
		}
		return nil, err
	}
	return &result, nil
}

// queryCardByPatientID 读取账号名下的就诊卡；没有卡返回 patient.ErrCardNotFound。
// 正常情况下每个账号至多一张卡，这里仍按 id 升序取第一张，保证脏数据下结果稳定。
func queryCardByPatientID(ctx context.Context, q patientQuerier, patientID int64) (*patient.Card, error) {
	return queryCard(ctx, q,
		`SELECT `+patientCardColumns+` FROM hospital.patient_user_info_card
WHERE user_id = $1 ORDER BY id ASC LIMIT 1`,
		patientID)
}

// queryCardForUpdate 在事务内按主键加行锁重读就诊卡，防止并发 PATCH 互相覆盖。
func queryCardForUpdate(ctx context.Context, tx *sql.Tx, cardID int64) (*patient.Card, error) {
	return queryCard(ctx, tx,
		`SELECT `+patientCardColumns+` FROM hospital.patient_user_info_card WHERE id = $1 FOR UPDATE`,
		cardID)
}

// insertPatient 以序列取主键插入患者账号，status 固定为“正常”，
// create_time 写入业务日期（DATE 列）。
//
// 昵称、头像与性别在本切片不落库：微信侧这些资料需要前端解密 encryptedData 后
// 才能可信获取，而 Slice 4 定义的登录请求只有 code 字段，因此新账号相应列保持
// NULL，响应按契约返回 null，后续切片拿到可信资料后再补写。
func insertPatient(ctx context.Context, tx *sql.Tx, openID string, now time.Time) (*patient.Patient, error) {
	row := tx.QueryRowContext(ctx, `INSERT INTO hospital.patient_user
    (id, open_id, nickname, photo, sex, status, create_time)
VALUES (nextval('hospital.patient_user_sequence'), $1, NULL, NULL, NULL, $2, $3::date)
RETURNING `+patientUserColumns,
		openID, patient.StatusCodeActive, patient.BusinessDate(now))
	result, err := scanPatient(row)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// scanPatient 把一行 patient_user 转换为实体。
func scanPatient(scanner patientScanner) (patient.Patient, error) {
	var (
		result     patient.Patient
		openID     sql.NullString
		nickname   sql.NullString
		photo      sql.NullString
		sex        sql.NullString
		status     sql.NullInt16
		createDate sql.NullString
	)
	if err := scanner.Scan(
		&result.ID, &openID, &nickname, &photo, &sex, &status, &createDate,
	); err != nil {
		return patient.Patient{}, err
	}
	result.OpenID = openID.String
	// 可空字段只在有值时设置指针，保证响应输出 null 而不是空字符串。
	result.Nickname = nullableString(nickname)
	result.Photo = nullableString(photo)
	result.Sex = nullableString(sex)
	result.Status = patientStatusLabel(status)
	result.CreateDate = createDate.String
	return result, nil
}

// scanCard 把一行 patient_user_info_card 转换为实体。
func scanCard(scanner patientScanner) (patient.Card, error) {
	var (
		result    patient.Card
		userID    sql.NullInt64
		cardUUID  sql.NullString
		name      sql.NullString
		sex       sql.NullString
		pid       sql.NullString
		tel       sql.NullString
		birthday  sql.NullString
		history   sql.NullString
		insurance sql.NullString
		faceModel sql.NullBool
	)
	if err := scanner.Scan(
		&result.ID, &userID, &cardUUID, &name, &sex, &pid, &tel,
		&birthday, &history, &insurance, &faceModel,
	); err != nil {
		return patient.Card{}, err
	}
	result.UserID = userID.Int64
	result.UUID = cardUUID.String
	result.Name = name.String
	result.Sex = sex.String
	result.PID = pid.String
	result.Tel = tel.String
	result.Birthday = birthday.String
	result.MedicalHistory = patient.ParseMedicalHistory(history.String)
	result.InsuranceType = insurance.String
	result.ExistFaceModel = faceModel.Valid && faceModel.Bool
	return result, nil
}

// patientStatusLabel 把 SMALLINT 存储的患者状态映射为对外字符串。
//
// 只有 1（正常）判定为 ACTIVE，其余值（2 或脏数据）一律按 DISABLED 处理：
// 该判定直接决定是否签发令牌，未知取值必须向“拒绝”一侧收敛，不能放行。
func patientStatusLabel(status sql.NullInt16) string {
	if status.Valid && status.Int16 == patient.StatusCodeActive {
		return patient.StatusActive
	}
	return patient.StatusDisabled
}

// nullableString 把可空文本列转换为指针：NULL 保持 nil，以便响应输出 null。
func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

// withPatientTx 在单个事务内执行 fn，成功提交、失败回滚。
func (r *PostgresPatientRepository) withPatientTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
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

// patientAdvisoryClass 是患者域在 PostgreSQL 咨询锁“双键空间”里的固定 classid。
//
// PostgreSQL 的单 bigint 键空间与 (classid, objid) 双 int32 键空间互不重叠，
// 患者域改用双键形式后，与排班域 planCreateLockKey 使用的单键空间在数据库层面
// 就彻底隔离，不存在跨域撞键导致无关业务互相排队（甚至拖慢）的可能。
const patientAdvisoryClass int32 = 0x50415431 // "PAT1"

// lockPatientKey 取事务级咨询锁；锁随事务提交或回滚自动释放，无需显式解锁。
func lockPatientKey(ctx context.Context, tx *sql.Tx, objID int32) error {
	_, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock($1, $2)",
		patientAdvisoryClass, objID)
	return err
}

// patientRegisterLockKey 把 openId 映射为确定性的 32 位咨询锁对象键，
// 用于串行化同一 openId 的并发注册。
func patientRegisterLockKey(openID string) int32 {
	return hashPatientLockKey("patient_user:open_id:" + openID)
}

// patientCardLockKey 以 user_id 为互斥对象串行化同一账号的并发建卡，
// 保证“每账号最多一张就诊卡”在没有唯一约束的表上同样成立。
func patientCardLockKey(userID int64) int32 {
	return hashPatientLockKey(fmt.Sprintf("patient_card:user_id:%d", userID))
}

// hashPatientLockKey 用 FNV-1a 把任意键编码为确定性的 32 位锁对象键：
// 同输入必得同键，使并发请求落在同一把咨询锁上（与排班域 planCreateLockKey 同一做法）。
// 哈希只用于分组排队，不要求无冲突；转 int32 后即使为负也仍是合法的 objid。
func hashPatientLockKey(value string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(value))
	return int32(h.Sum32())
}
