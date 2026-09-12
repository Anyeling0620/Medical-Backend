// 病历仓储（hospital.doctor_prescription）的 PostgreSQL 集成测试（契约 §11：仓储 contract 测试）。
//
// 覆盖：序列主键 nextval('hospital.doctor_prescription_sequence')、同一挂号唯一、
// 「挂号归属 = 当前医生」的 fail-closed 语义、PATCH 只改提交字段、删除后再删除返回不存在、
// 列表归属过滤与 total 一致。
//
//	set "PGSQL_INTEGRATION_TEST=1" && go test ./internal/repo/ -run TestPostgresMedicalRecord -count=1 -v
//
// 夹具安全：只插入本用例自建的 medical_registration 行（out_trade_no 前缀 ITMEDREC）
// 与经由仓储写入的 doctor_prescription 行（uuid 前缀 RXIT），
// 并在 t.Cleanup 中按主键逐条删除后复核残留为 0；绝不 TRUNCATE 或全表 DELETE。
// 归属医生使用真实存在的医生编号（1 与 2），避免写入悬空外键。
package repo

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"

	"Medical-Web-Backend/internal/config"
	"Medical-Web-Backend/internal/domain/medical_record"
)

// medicalRecordITSkipMsg 是未开启集成测试开关时的跳过说明。
const medicalRecordITSkipMsg = "设置 PGSQL_INTEGRATION_TEST=1 后才会执行 PostgreSQL 集成测试"

const (
	// medicalRecordITUUIDPrefix 是病历业务标识的夹具标记（doctor_prescription.uuid）。
	medicalRecordITUUIDPrefix = "RXIT"
	// medicalRecordITOutTradeNoPrefix 是挂号夹具标记（out_trade_no 是 CHAR(32)，前缀 8 位）。
	medicalRecordITOutTradeNoPrefix = "ITMEDREC"
	// medicalRecordITOwnerDoctorID 是病历归属医生（真实存在的医生编号）。
	medicalRecordITOwnerDoctorID int64 = 1
	// medicalRecordITOtherDoctorID 是「他人医生」，用于验证越权 fail-closed。
	medicalRecordITOtherDoctorID int64 = 2
)

// medicalRecordIT 汇总集成测试连接、仓储与待清理主键。
type medicalRecordIT struct {
	db   *sql.DB
	repo *PostgresMedicalRecordRepository

	registrationIDs []int64
	recordIDs       []int64
}

// newMedicalRecordIT 组装真实 PostgreSQL 连接；未开启开关或连不上库时跳过而不是失败。
func newMedicalRecordIT(t *testing.T) (*medicalRecordIT, context.Context) {
	t.Helper()
	if os.Getenv("PGSQL_INTEGRATION_TEST") != "1" {
		t.Skip(medicalRecordITSkipMsg)
	}
	if err := godotenv.Load("../../.env"); err != nil {
		t.Skipf("加载 .env 失败，跳过集成测试：%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("加载配置失败，跳过集成测试：%v", err)
	}
	dsn := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.Postgres.Username, cfg.Postgres.Password),
		Host:   cfg.Postgres.Addr,
		Path:   cfg.Postgres.Database,
	}
	query := dsn.Query()
	query.Set("sslmode", cfg.Postgres.SSLMode)
	dsn.RawQuery = query.Encode()

	db, err := sql.Open("pgx", dsn.String())
	if err != nil {
		t.Skipf("打开 PostgreSQL 连接失败，跳过集成测试：%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("连接 PostgreSQL 失败，跳过集成测试：%v", err)
	}
	db.SetMaxOpenConns(8)

	it := &medicalRecordIT{db: db, repo: NewPostgresMedicalRecordRepository(db)}
	t.Cleanup(func() { it.cleanup(t) })
	return it, context.Background()
}

// cleanup 按主键逐条删除夹具，并按 uuid / out_trade_no 前缀兜底复核残留为 0。
func (it *medicalRecordIT) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, id := range it.recordIDs {
		if _, err := it.db.ExecContext(ctx,
			`DELETE FROM hospital.doctor_prescription WHERE id = $1`, id); err != nil {
			t.Errorf("清理病历夹具失败（id=%d）：%v", id, err)
		}
	}
	// 兜底：若用例中途失败导致主键未登记，按业务标识前缀再清一次。
	if _, err := it.db.ExecContext(ctx,
		`DELETE FROM hospital.doctor_prescription WHERE uuid LIKE $1`,
		medicalRecordITUUIDPrefix+"%"); err != nil {
		t.Errorf("按 uuid 前缀清理病历夹具失败：%v", err)
	}
	for _, id := range it.registrationIDs {
		if _, err := it.db.ExecContext(ctx,
			`DELETE FROM hospital.medical_registration WHERE id = $1`, id); err != nil {
			t.Errorf("清理挂号夹具失败（id=%d）：%v", id, err)
		}
	}
	if _, err := it.db.ExecContext(ctx,
		`DELETE FROM hospital.medical_registration WHERE out_trade_no LIKE $1`,
		medicalRecordITOutTradeNoPrefix+"%"); err != nil {
		t.Errorf("按 out_trade_no 前缀清理挂号夹具失败：%v", err)
	}

	var records, registrations int64
	if err := it.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM hospital.doctor_prescription WHERE uuid LIKE $1`,
		medicalRecordITUUIDPrefix+"%").Scan(&records); err != nil {
		t.Errorf("复核病历夹具残留失败：%v", err)
	}
	if err := it.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM hospital.medical_registration WHERE out_trade_no LIKE $1`,
		medicalRecordITOutTradeNoPrefix+"%").Scan(&registrations); err != nil {
		t.Errorf("复核挂号夹具残留失败：%v", err)
	}
	if records != 0 || registrations != 0 {
		t.Errorf("夹具残留：doctor_prescription=%d、medical_registration=%d，期望均为 0", records, registrations)
	}
	if err := it.db.Close(); err != nil {
		t.Logf("关闭集成测试数据库连接失败：%v", err)
	}
}

// medicalRecordITNewOutTradeNo 生成带夹具标记的唯一商户订单号（前缀 8 位 + 24 位十六进制 = 32）。
func medicalRecordITNewOutTradeNo() string {
	raw := strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))
	return medicalRecordITOutTradeNoPrefix + raw[:24]
}

// medicalRecordITNewUUID 生成带夹具标记的病历业务标识（RX 前缀 + 30 位大写十六进制）。
func medicalRecordITNewUUID() string {
	return medicalRecordITUUIDPrefix + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:28]
}

// insertRegistration 插入一条归属指定医生的挂号夹具行，并登记主键供清理。
// patient_card_id / dept_sub_id 留空，避免依赖就诊卡与子科室的既有数据。
func (it *medicalRecordIT) insertRegistration(
	t *testing.T,
	ctx context.Context,
	doctorID int64,
) int64 {
	t.Helper()
	var id int64
	err := it.db.QueryRowContext(ctx,
		`INSERT INTO hospital.medical_registration
(id, doctor_id, date, slot, out_trade_no, payment_status, create_time)
VALUES (nextval('hospital.medical_registration_sequence'), $1, $2::date, 1, $3, 1, $2::date)
RETURNING id`,
		doctorID, time.Now().Format("2006-01-02"), medicalRecordITNewOutTradeNo()).Scan(&id)
	if err != nil {
		t.Fatalf("插入挂号夹具失败：%v", err)
	}
	it.registrationIDs = append(it.registrationIDs, id)
	return id
}

// createRecord 经仓储写入病历夹具并登记主键供清理。
func (it *medicalRecordIT) createRecord(
	t *testing.T,
	ctx context.Context,
	registrationID int64,
	doctorID int64,
	diagnosis, content string,
) *medical_record.MedicalRecord {
	t.Helper()
	owner, err := it.repo.FindRegistrationOwner(ctx, registrationID, doctorID)
	if err != nil {
		t.Fatalf("读取挂号归属失败：%v", err)
	}
	created, err := it.repo.CreateMedicalRecord(
		ctx, *owner, medicalRecordITNewUUID(), diagnosis, content)
	if err != nil {
		t.Fatalf("写入病历夹具失败：%v", err)
	}
	it.recordIDs = append(it.recordIDs, created.ID)
	return created
}

// recordRowCount 统计某挂号下的病历行数（证明唯一性与删除是否真正落库）。
func (it *medicalRecordIT) recordRowCount(t *testing.T, ctx context.Context, registrationID int64) int {
	t.Helper()
	var count int
	if err := it.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM hospital.doctor_prescription WHERE registration_id = $1`,
		registrationID).Scan(&count); err != nil {
		t.Fatalf("统计挂号 %d 下的病历行数失败：%v", registrationID, err)
	}
	return count
}

// TestPostgresMedicalRecordRepositoryCreatePersistsOwnership 覆盖写入路径：
// nextval('hospital.doctor_prescription_sequence') 能拿到主键，
// 归属字段全部取自挂号行并原样落库（含 NULL 就诊卡/子科室读回 0 的兜底口径）。
func TestPostgresMedicalRecordRepositoryCreatePersistsOwnership(t *testing.T) {
	it, ctx := newMedicalRecordIT(t)
	registrationID := it.insertRegistration(t, ctx, medicalRecordITOwnerDoctorID)

	owner, err := it.repo.FindRegistrationOwner(ctx, registrationID, medicalRecordITOwnerDoctorID)
	if err != nil {
		t.Fatalf("FindRegistrationOwner 意外错误：%v", err)
	}
	if owner.RegistrationID != registrationID || owner.DoctorID != medicalRecordITOwnerDoctorID {
		t.Fatalf("挂号归属 = %+v，期望 registrationID=%d doctorID=%d",
			owner, registrationID, medicalRecordITOwnerDoctorID)
	}

	recordUUID := medicalRecordITNewUUID()
	created, err := it.repo.CreateMedicalRecord(ctx, *owner, recordUUID, "牙髓炎", "主诉：牙痛\n处理：根管治疗")
	if err != nil {
		t.Fatalf("CreateMedicalRecord 意外错误：%v", err)
	}
	it.recordIDs = append(it.recordIDs, created.ID)

	if created.ID <= 0 {
		t.Fatalf("序列主键异常：id = %d，期望正整数（nextval 未生效？）", created.ID)
	}
	if created.UUID != recordUUID {
		t.Errorf("uuid = %q，期望 %q", created.UUID, recordUUID)
	}
	if created.RegistrationID != registrationID || created.DoctorID != medicalRecordITOwnerDoctorID {
		t.Errorf("归属字段 = %+v，期望挂号 %d / 医生 %d", created, registrationID, medicalRecordITOwnerDoctorID)
	}
	if created.Diagnosis != "牙髓炎" || created.Content != "主诉：牙痛\n处理：根管治疗" {
		t.Errorf("病历内容 = (%q, %q)，与写入不一致", created.Diagnosis, created.Content)
	}

	stored, err := it.repo.FindMedicalRecord(ctx, created.ID, medicalRecordITOwnerDoctorID)
	if err != nil {
		t.Fatalf("FindMedicalRecord 意外错误：%v", err)
	}
	if stored.UUID != recordUUID || stored.Diagnosis != "牙髓炎" ||
		stored.Content != "主诉：牙痛\n处理：根管治疗" ||
		stored.RegistrationID != registrationID || stored.DoctorID != medicalRecordITOwnerDoctorID {
		t.Fatalf("读回的病历与写入不一致：%+v", stored)
	}
	if stored.PatientCardID != 0 || stored.SubdepartmentID != 0 {
		t.Errorf("NULL 归属列读回应为 0（coalesce 兜底），实际 patientCardID=%d subDeptID=%d",
			stored.PatientCardID, stored.SubdepartmentID)
	}
	if count := it.recordRowCount(t, ctx, registrationID); count != 1 {
		t.Errorf("挂号 %d 下的病历行数 = %d，期望 1", registrationID, count)
	}
	t.Logf("写入实测：挂号=%d id=%d uuid=%s doctorId=%d", registrationID, created.ID, created.UUID, created.DoctorID)
}

// TestPostgresMedicalRecordRepositoryRejectsDuplicatePerRegistration 同一挂号只允许一份病历：
// 第二次写入返回 medical_record.ErrDuplicate，且库里仍只有一行。
func TestPostgresMedicalRecordRepositoryRejectsDuplicatePerRegistration(t *testing.T) {
	it, ctx := newMedicalRecordIT(t)
	registrationID := it.insertRegistration(t, ctx, medicalRecordITOwnerDoctorID)
	it.createRecord(t, ctx, registrationID, medicalRecordITOwnerDoctorID, "牙髓炎", "第一次正文")

	owner, err := it.repo.FindRegistrationOwner(ctx, registrationID, medicalRecordITOwnerDoctorID)
	if err != nil {
		t.Fatalf("读取挂号归属失败：%v", err)
	}
	created, err := it.repo.CreateMedicalRecord(
		ctx, *owner, medicalRecordITNewUUID(), "复诊诊断", "第二次正文")
	if !errors.Is(err, medical_record.ErrDuplicate) {
		t.Fatalf("同一挂号重复写入错误 = %v（created=%+v），期望 %v", err, created, medical_record.ErrDuplicate)
	}
	if count := it.recordRowCount(t, ctx, registrationID); count != 1 {
		t.Errorf("重复写入后挂号 %d 下的病历行数 = %d，期望 1（不得写入第二行）", registrationID, count)
	}
}

// TestPostgresMedicalRecordRepositoryOwnershipFailClosed 归属过滤为强制条件：
// 非本人负责的挂号与病历一律按「不存在」处理（不区分越权与不存在），且不产生任何写入。
func TestPostgresMedicalRecordRepositoryOwnershipFailClosed(t *testing.T) {
	it, ctx := newMedicalRecordIT(t)
	registrationID := it.insertRegistration(t, ctx, medicalRecordITOwnerDoctorID)
	created := it.createRecord(t, ctx, registrationID, medicalRecordITOwnerDoctorID, "牙髓炎", "原正文")

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "FindRegistrationOwner",
			call: func() error {
				_, err := it.repo.FindRegistrationOwner(ctx, registrationID, medicalRecordITOtherDoctorID)
				return err
			},
		},
		{
			name: "FindMedicalRecord",
			call: func() error {
				_, err := it.repo.FindMedicalRecord(ctx, created.ID, medicalRecordITOtherDoctorID)
				return err
			},
		},
		{
			name: "UpdateMedicalRecord",
			call: func() error {
				diagnosis := "越权诊断"
				_, err := it.repo.UpdateMedicalRecord(ctx, created.ID, medicalRecordITOtherDoctorID,
					medical_record.UpdateInput{Diagnosis: &diagnosis})
				return err
			},
		},
		{
			name: "DeleteMedicalRecord",
			call: func() error {
				return it.repo.DeleteMedicalRecord(ctx, created.ID, medicalRecordITOtherDoctorID)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if tc.name == "FindRegistrationOwner" {
				if !errors.Is(err, medical_record.ErrRegistrationNotFound) {
					t.Fatalf("错误 = %v，期望 %v", err, medical_record.ErrRegistrationNotFound)
				}
				return
			}
			if !errors.Is(err, medical_record.ErrNotFound) {
				t.Fatalf("错误 = %v，期望 %v", err, medical_record.ErrNotFound)
			}
		})
	}

	// 越权失败后原病历必须原样存在且内容未被改动。
	stored, err := it.repo.FindMedicalRecord(ctx, created.ID, medicalRecordITOwnerDoctorID)
	if err != nil {
		t.Fatalf("本人读取病历失败：%v", err)
	}
	if stored.Diagnosis != "牙髓炎" || stored.Content != "原正文" {
		t.Errorf("越权操作改动了病历：%+v", stored)
	}
	if count := it.recordRowCount(t, ctx, registrationID); count != 1 {
		t.Errorf("挂号 %d 下的病历行数 = %d，期望 1", registrationID, count)
	}
}

// TestPostgresMedicalRecordRepositoryUpdateOnlySubmittedFields 覆盖 PATCH 语义：
// 只提交 diagnosis 时 rp 保持原值，只提交 rp 时 diagnosis 保持原值。
func TestPostgresMedicalRecordRepositoryUpdateOnlySubmittedFields(t *testing.T) {
	it, ctx := newMedicalRecordIT(t)
	registrationID := it.insertRegistration(t, ctx, medicalRecordITOwnerDoctorID)
	created := it.createRecord(t, ctx, registrationID, medicalRecordITOwnerDoctorID, "原诊断", "原正文")

	// 只提交 diagnosis：正文必须保持原值。
	diagnosis := "新诊断"
	updated, err := it.repo.UpdateMedicalRecord(ctx, created.ID, medicalRecordITOwnerDoctorID,
		medical_record.UpdateInput{Diagnosis: &diagnosis})
	if err != nil {
		t.Fatalf("只改诊断意外错误：%v", err)
	}
	if updated.Diagnosis != "新诊断" || updated.Content != "原正文" {
		t.Fatalf("只改诊断后 = (%q, %q)，期望 (新诊断, 原正文)", updated.Diagnosis, updated.Content)
	}
	if stored, err := it.repo.FindMedicalRecord(ctx, created.ID, medicalRecordITOwnerDoctorID); err != nil {
		t.Fatalf("重新读取失败：%v", err)
	} else if stored.Content != "原正文" {
		t.Fatalf("未提交的正文被改动：%q", stored.Content)
	}

	// 只提交 rp：诊断必须保持上一步的值。
	content := "新正文"
	updated, err = it.repo.UpdateMedicalRecord(ctx, created.ID, medicalRecordITOwnerDoctorID,
		medical_record.UpdateInput{Content: &content})
	if err != nil {
		t.Fatalf("只改正文意外错误：%v", err)
	}
	if updated.Diagnosis != "新诊断" || updated.Content != "新正文" {
		t.Fatalf("只改正文后 = (%q, %q)，期望 (新诊断, 新正文)", updated.Diagnosis, updated.Content)
	}

	// 两个字段都提交。
	nextDiagnosis, nextContent := "复诊诊断", "复诊正文"
	if _, err := it.repo.UpdateMedicalRecord(ctx, created.ID, medicalRecordITOwnerDoctorID,
		medical_record.UpdateInput{Diagnosis: &nextDiagnosis, Content: &nextContent}); err != nil {
		t.Fatalf("同时改两字段意外错误：%v", err)
	}
	stored, err := it.repo.FindMedicalRecord(ctx, created.ID, medicalRecordITOwnerDoctorID)
	if err != nil {
		t.Fatalf("重新读取失败：%v", err)
	}
	if stored.Diagnosis != nextDiagnosis || stored.Content != nextContent {
		t.Fatalf("落库结果 = (%q, %q)，期望 (%q, %q)",
			stored.Diagnosis, stored.Content, nextDiagnosis, nextContent)
	}
}

// TestPostgresMedicalRecordRepositoryDeleteThenNotFound 覆盖删除：
// 首次删除成功并真正落库，重复删除与删除后读取都返回 ErrNotFound。
func TestPostgresMedicalRecordRepositoryDeleteThenNotFound(t *testing.T) {
	it, ctx := newMedicalRecordIT(t)
	registrationID := it.insertRegistration(t, ctx, medicalRecordITOwnerDoctorID)
	created := it.createRecord(t, ctx, registrationID, medicalRecordITOwnerDoctorID, "牙髓炎", "正文")

	if err := it.repo.DeleteMedicalRecord(ctx, created.ID, medicalRecordITOwnerDoctorID); err != nil {
		t.Fatalf("首次删除意外错误：%v", err)
	}
	if count := it.recordRowCount(t, ctx, registrationID); count != 0 {
		t.Fatalf("删除后挂号 %d 下的病历行数 = %d，期望 0", registrationID, count)
	}
	if err := it.repo.DeleteMedicalRecord(ctx, created.ID, medicalRecordITOwnerDoctorID); !errors.Is(err, medical_record.ErrNotFound) {
		t.Fatalf("重复删除错误 = %v，期望 %v", err, medical_record.ErrNotFound)
	}
	if _, err := it.repo.FindMedicalRecord(ctx, created.ID, medicalRecordITOwnerDoctorID); !errors.Is(err, medical_record.ErrNotFound) {
		t.Fatalf("删除后读取错误 = %v，期望 %v", err, medical_record.ErrNotFound)
	}
}

// TestPostgresMedicalRecordRepositoryListOwnerFilterAndTotal 覆盖列表归属过滤与分页：
// 归属条件强制生效（fail closed），total 与命中行数一致，doctorId 过滤与归属取同一列。
func TestPostgresMedicalRecordRepositoryListOwnerFilterAndTotal(t *testing.T) {
	it, ctx := newMedicalRecordIT(t)
	ownerRegistration := it.insertRegistration(t, ctx, medicalRecordITOwnerDoctorID)
	otherRegistration := it.insertRegistration(t, ctx, medicalRecordITOtherDoctorID)
	owned := it.createRecord(t, ctx, ownerRegistration, medicalRecordITOwnerDoctorID, "本人诊断", "本人正文")
	it.createRecord(t, ctx, otherRegistration, medicalRecordITOtherDoctorID, "他人诊断", "他人正文")

	ownerID := medicalRecordITOwnerDoctorID
	otherID := medicalRecordITOtherDoctorID

	cases := []struct {
		name      string
		filter    medical_record.Filter
		offset    int
		limit     int
		wantTotal int64
		wantItems int
	}{
		{
			name:      "按本人挂号过滤",
			filter:    medical_record.Filter{OwnerDoctorID: ownerID, RegistrationID: &ownerRegistration},
			limit:     20,
			wantTotal: 1,
			wantItems: 1,
		},
		{
			name:      "归属医生不匹配时看不到他人挂号下的病历",
			filter:    medical_record.Filter{OwnerDoctorID: otherID, RegistrationID: &ownerRegistration},
			limit:     20,
			wantTotal: 0,
			wantItems: 0,
		},
		{
			name:      "本人不能看到挂号归属他人的病历",
			filter:    medical_record.Filter{OwnerDoctorID: ownerID, RegistrationID: &otherRegistration},
			limit:     20,
			wantTotal: 0,
			wantItems: 0,
		},
		{
			name: "doctorId 过滤与归属取同一列（命中）",
			filter: medical_record.Filter{
				OwnerDoctorID: ownerID, RegistrationID: &ownerRegistration, DoctorID: &ownerID,
			},
			limit:     20,
			wantTotal: 1,
			wantItems: 1,
		},
		{
			name: "doctorId 过滤与归属取同一列（不命中）",
			filter: medical_record.Filter{
				OwnerDoctorID: ownerID, RegistrationID: &ownerRegistration, DoctorID: &otherID,
			},
			limit:     20,
			wantTotal: 0,
			wantItems: 0,
		},
		{
			name:      "offset 越过命中行时 items 为空但 total 仍为命中数",
			filter:    medical_record.Filter{OwnerDoctorID: ownerID, RegistrationID: &ownerRegistration},
			offset:    1,
			limit:     20,
			wantTotal: 1,
			wantItems: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, total, err := it.repo.ListMedicalRecords(ctx, tc.filter, tc.offset, tc.limit)
			if err != nil {
				t.Fatalf("ListMedicalRecords 意外错误：%v", err)
			}
			if total != tc.wantTotal {
				t.Errorf("total = %d，期望 %d", total, tc.wantTotal)
			}
			if len(items) != tc.wantItems {
				t.Errorf("items 长度 = %d，期望 %d", len(items), tc.wantItems)
			}
			// total 与返回列表口径必须自洽：只有分页窗口装得下时才要求相等。
			if tc.offset == 0 && total != int64(len(items)) {
				t.Errorf("total(%d) 与 items 长度(%d) 不一致（同一分页窗口）", total, len(items))
			}
			for _, item := range items {
				if item.RegistrationID != ownerRegistration && item.RegistrationID != otherRegistration {
					t.Errorf("列表返回了非本用例夹具行：%+v", item)
				}
			}
			if tc.wantItems == 1 && items[0].ID != owned.ID {
				t.Errorf("返回病历 id = %d，期望 %d", items[0].ID, owned.ID)
			}
		})
	}
}
