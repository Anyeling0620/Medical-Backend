package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"

	"Medical-Web-Backend/internal/config"
	"Medical-Web-Backend/internal/domain/patient"
)

// patientIntegrationSkipMsg 是未开启集成测试开关时的跳过说明。
const patientIntegrationSkipMsg = "设置 PGSQL_INTEGRATION_TEST=1 后才会执行 PostgreSQL 集成测试"

// 集成测试使用的真实身份证号（校验位均正确），避免用随机串绕过领域规则。
const (
	itestPIDMale   = "110101199001011237" // 1990-01-01，第 17 位奇数 → 男
	itestPIDFemale = "110101199001011202" // 1990-01-01，第 17 位偶数 → 女
)

// itestCardInputMale / itestCardInputFemale 返回独立的建卡入参，
// 避免并发用例共享底层切片。
func itestCardInputMale() patient.CardInput {
	return patient.CardInput{
		Name:           "集成测试甲",
		Sex:            patient.SexMale,
		PID:            itestPIDMale,
		Tel:            "13800138000",
		MedicalHistory: []string{"高血压", "糖尿病"},
		InsuranceType:  "社会基本医疗保险",
	}
}

func itestCardInputFemale() patient.CardInput {
	return patient.CardInput{
		Name:           "集成测试乙",
		Sex:            patient.SexFemale,
		PID:            itestPIDFemale,
		Tel:            "13900139000",
		MedicalHistory: []string{"无"},
		InsuranceType:  "其他",
	}
}

// itestMissingID 返回一个远超现有数据范围的 int32 主键，用于验证“不存在”分支。
func itestMissingID() int64 {
	return int64(2000000000) + int64(uuid.New().ID()%100000000)
}

// patientIT 承载一次集成测试所需的连接、仓库与被创建的测试数据主键。
// 所有写入的数据都会在 t.Cleanup 中按主键精确删除，绝不使用 TRUNCATE 或全表 DELETE。
type patientIT struct {
	t           *testing.T
	db          *sql.DB
	repo        *PostgresPatientRepository
	patientIDs  []int64
	cardIDs     []int64
	faceAuthIDs []int64
}

// newPatientIT 组装真实 PostgreSQL 连接；未开启开关或连不上时跳过而不是失败。
func newPatientIT(t *testing.T) (*patientIT, context.Context) {
	t.Helper()
	if os.Getenv("PGSQL_INTEGRATION_TEST") != "1" {
		t.Skip(patientIntegrationSkipMsg)
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

	it := &patientIT{t: t, db: db, repo: NewPostgresPatientRepository(db)}
	t.Cleanup(it.cleanup)
	return it, context.Background()
}

// cleanup 按主键精确删除本用例创建的数据，顺序为“人脸记录 → 就诊卡 → 患者账号”。
func (it *patientIT) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, id := range it.faceAuthIDs {
		_, _ = it.db.ExecContext(ctx, `DELETE FROM hospital.patient_face_auth WHERE id = $1`, id)
	}
	for _, id := range it.cardIDs {
		_, _ = it.db.ExecContext(ctx, `DELETE FROM hospital.patient_face_auth WHERE patient_card_id = $1`, id)
		_, _ = it.db.ExecContext(ctx, `DELETE FROM hospital.patient_user_info_card WHERE id = $1`, id)
	}
	for _, id := range it.patientIDs {
		_, _ = it.db.ExecContext(ctx, `DELETE FROM hospital.patient_user_info_card WHERE user_id = $1`, id)
		_, _ = it.db.ExecContext(ctx, `DELETE FROM hospital.patient_user WHERE id = $1`, id)
	}
	_ = it.db.Close()
}

// trackPatient / trackCard / trackFaceAuth 登记待清理的主键。
func (it *patientIT) trackPatient(id int64) { it.patientIDs = append(it.patientIDs, id) }
func (it *patientIT) trackCard(id int64)    { it.cardIDs = append(it.cardIDs, id) }
func (it *patientIT) trackFaceAuth(id int64) {
	it.faceAuthIDs = append(it.faceAuthIDs, id)
}

// uniqueOpenID 生成带随机后缀的 openId，避免与真实数据或其他用例冲突。
func (it *patientIT) uniqueOpenID() string {
	return fmt.Sprintf("itest-%s", uuid.NewString())
}

// newPatient 注册一个测试患者账号并登记清理。
func (it *patientIT) newPatient(ctx context.Context) *patient.Patient {
	it.t.Helper()
	created, isNew, err := it.repo.FindOrCreatePatientByOpenID(ctx, it.uniqueOpenID(), time.Now())
	if err != nil {
		it.t.Fatalf("注册测试患者失败：%v", err)
	}
	if !isNew {
		it.t.Fatalf("首次注册应返回 created=true，实际 false（id=%d）", created.ID)
	}
	it.trackPatient(created.ID)
	return created
}

// newCard 为指定患者建立就诊卡并登记清理。
func (it *patientIT) newCard(ctx context.Context, userID int64, input patient.CardInput) *patient.Card {
	it.t.Helper()
	draft, err := patient.NewCard(userID, "", input, time.Now())
	if err != nil {
		it.t.Fatalf("构造就诊卡实体失败：%v", err)
	}
	created, err := it.repo.CreateCard(ctx, draft)
	if err != nil {
		it.t.Fatalf("写入就诊卡失败：%v", err)
	}
	it.trackCard(created.ID)
	return created
}

// insertFaceAuth 直接写入一条人脸认证日期记录，返回主键并登记清理。
func (it *patientIT) insertFaceAuth(ctx context.Context, cardID int64, date string) int64 {
	it.t.Helper()
	var id int64
	err := it.db.QueryRowContext(ctx, `INSERT INTO hospital.patient_face_auth (id, patient_card_id, date)
VALUES (nextval('hospital.patient_face_auth_sequence'), $1, $2::date)
RETURNING id`, cardID, date).Scan(&id)
	if err != nil {
		it.t.Fatalf("插入人脸认证记录失败：%v", err)
	}
	it.trackFaceAuth(id)
	return id
}

// itestCount 执行 COUNT 查询，供并发用例断言数据库中的真实行数。
func (it *patientIT) itestCount(ctx context.Context, query string, args ...any) int {
	it.t.Helper()
	var count int
	if err := it.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		it.t.Fatalf("统计行数失败：%v", err)
	}
	return count
}

// itestAssertCardEqual 比较两张就诊卡的字段是否一致。
func itestAssertCardEqual(t *testing.T, got, want patient.Card) {
	t.Helper()
	if got.ID != want.ID || got.UserID != want.UserID || got.UUID != want.UUID ||
		got.Name != want.Name || got.Sex != want.Sex || got.PID != want.PID ||
		got.Tel != want.Tel || got.Birthday != want.Birthday ||
		got.InsuranceType != want.InsuranceType || got.ExistFaceModel != want.ExistFaceModel {
		t.Fatalf("就诊卡不一致：%+v，期望 %+v", got, want)
	}
	if !reflect.DeepEqual(got.MedicalHistory, want.MedicalHistory) {
		t.Fatalf("疾病史不一致：%v，期望 %v", got.MedicalHistory, want.MedicalHistory)
	}
}

// TestPatientRepositoryFindOrCreateByOpenID 覆盖“登录与注册合一”的首次创建、
// 重复登录复用账号，以及 openId 为空的必填校验。
func TestPatientRepositoryFindOrCreateByOpenID(t *testing.T) {
	it, ctx := newPatientIT(t)

	openID := it.uniqueOpenID()
	now := time.Now()
	wantDate := patient.BusinessDate(now)

	first, created, err := it.repo.FindOrCreatePatientByOpenID(ctx, openID, now)
	if err != nil {
		t.Fatalf("首次调用意外错误：%v", err)
	}
	if !created {
		t.Fatal("首次调用应返回 created=true")
	}
	it.trackPatient(first.ID)

	if first.OpenID != openID {
		t.Errorf("OpenID = %q，期望 %q", first.OpenID, openID)
	}
	if first.Status != patient.StatusActive {
		t.Errorf("Status = %q，期望 %q", first.Status, patient.StatusActive)
	}
	if !first.IsActive() {
		t.Error("新注册账号应为 ACTIVE")
	}
	if first.CreateDate != wantDate {
		t.Errorf("CreateDate = %q，期望业务日期 %q", first.CreateDate, wantDate)
	}
	if first.Nickname != nil || first.Photo != nil || first.Sex != nil {
		t.Errorf("本切片不写昵称/头像/性别，期望 NULL：%+v", first)
	}

	second, createdAgain, err := it.repo.FindOrCreatePatientByOpenID(ctx, openID, now)
	if err != nil {
		t.Fatalf("重复调用意外错误：%v", err)
	}
	if createdAgain {
		t.Error("重复登录不应再创建账号")
	}
	if second.ID != first.ID {
		t.Errorf("重复登录返回了不同账号：%d vs %d", second.ID, first.ID)
	}

	if _, _, err := it.repo.FindOrCreatePatientByOpenID(ctx, "", now); !errors.Is(err, patient.ErrOpenIDRequired) {
		t.Fatalf("openId 为空时错误 = %v，期望 %v", err, patient.ErrOpenIDRequired)
	}
}

// TestPatientRepositoryConcurrentRegisterSameOpenID 用同一时刻发起的 8 个并发请求
// 暴露“先查后插”竞态：同一 openId 只能创建一次，且数据库只留一行。
func TestPatientRepositoryConcurrentRegisterSameOpenID(t *testing.T) {
	it, ctx := newPatientIT(t)
	openID := it.uniqueOpenID()
	const workers = 8

	type outcome struct {
		id      int64
		created bool
		err     error
	}
	results := make([]outcome, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			profile, created, err := it.repo.FindOrCreatePatientByOpenID(ctx, openID, time.Now())
			result := outcome{created: created, err: err}
			if profile != nil {
				result.id = profile.ID
			}
			results[idx] = result
		}(i)
	}
	close(start)
	wg.Wait()

	createdCount := 0
	var firstID int64
	for i, result := range results {
		if result.err != nil {
			t.Fatalf("第 %d 个并发调用出错：%v", i, result.err)
		}
		if result.id == 0 {
			t.Fatalf("第 %d 个并发调用未返回账号主键", i)
		}
		if result.created {
			createdCount++
		}
		if firstID == 0 {
			firstID = result.id
		} else if result.id != firstID {
			t.Fatalf("并发注册返回了不同账号：%d vs %d", result.id, firstID)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created=true 出现 %d 次，期望恰好 1 次", createdCount)
	}
	it.trackPatient(firstID)

	rows := it.itestCount(ctx, `SELECT COUNT(*) FROM hospital.patient_user WHERE open_id = $1`, openID)
	if rows != 1 {
		t.Fatalf("数据库中同 openId 账号行数 = %d，期望 1", rows)
	}
}

// TestPatientRepositoryFindPatientByIDNotFound 验证不存在的主键映射为领域错误。
func TestPatientRepositoryFindPatientByIDNotFound(t *testing.T) {
	it, ctx := newPatientIT(t)
	if _, err := it.repo.FindPatientByID(ctx, itestMissingID()); !errors.Is(err, patient.ErrPatientNotFound) {
		t.Fatalf("错误 = %v，期望 %v", err, patient.ErrPatientNotFound)
	}
}

// TestPatientRepositoryCreateCardRoundTrip 验证建卡字段完整落库、读回一致，
// 以及同一账号重复建卡返回 ErrCardExists。
func TestPatientRepositoryCreateCardRoundTrip(t *testing.T) {
	it, ctx := newPatientIT(t)
	owner := it.newPatient(ctx)

	draft, err := patient.NewCard(owner.ID, "", itestCardInputMale(), time.Now())
	if err != nil {
		t.Fatalf("构造就诊卡失败：%v", err)
	}
	created, err := it.repo.CreateCard(ctx, draft)
	if err != nil {
		t.Fatalf("建卡失败：%v", err)
	}
	it.trackCard(created.ID)

	if created.ID == 0 {
		t.Fatal("建卡后主键不应为 0")
	}
	if created.UserID != owner.ID {
		t.Errorf("UserID = %d，期望 %d", created.UserID, owner.ID)
	}
	if created.Name != "集成测试甲" || created.Sex != patient.SexMale {
		t.Errorf("姓名/性别 = %q/%q", created.Name, created.Sex)
	}
	if created.PID != itestPIDMale {
		t.Errorf("PID = %q，期望 %q（读回应去掉 CHAR(18) 的尾部空格）", created.PID, itestPIDMale)
	}
	if created.Tel != "13800138000" {
		t.Errorf("Tel = %q", created.Tel)
	}
	if created.Birthday != "1990-01-01" {
		t.Errorf("Birthday = %q，期望 1990-01-01", created.Birthday)
	}
	if !reflect.DeepEqual(created.MedicalHistory, []string{"高血压", "糖尿病"}) {
		t.Errorf("MedicalHistory = %v", created.MedicalHistory)
	}
	if created.InsuranceType != "社会基本医疗保险" {
		t.Errorf("InsuranceType = %q", created.InsuranceType)
	}
	if created.ExistFaceModel {
		t.Error("新建卡 ExistFaceModel 应为 false")
	}
	if len(created.UUID) != 32 || strings.Contains(created.UUID, "-") {
		t.Errorf("UUID = %q，期望 32 位无横线", created.UUID)
	}

	stored, err := it.repo.FindCardByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("按主键读取就诊卡失败：%v", err)
	}
	itestAssertCardEqual(t, *stored, *created)

	duplicate, err := patient.NewCard(owner.ID, "", itestCardInputFemale(), time.Now())
	if err != nil {
		t.Fatalf("构造重复就诊卡失败：%v", err)
	}
	if _, err := it.repo.CreateCard(ctx, duplicate); !errors.Is(err, patient.ErrCardExists) {
		t.Fatalf("重复建卡错误 = %v，期望 %v", err, patient.ErrCardExists)
	}
}

// TestPatientRepositoryConcurrentCreateCardSameUser 并发建卡必须被串行化：
// 恰好 1 次成功，其余返回 ErrCardExists，数据库只留 1 行。
func TestPatientRepositoryConcurrentCreateCardSameUser(t *testing.T) {
	it, ctx := newPatientIT(t)
	owner := it.newPatient(ctx)
	const workers = 8

	errs := make([]error, workers)
	ids := make([]int64, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			draft, err := patient.NewCard(owner.ID, "", itestCardInputMale(), time.Now())
			if err != nil {
				errs[idx] = err
				<-start
				return
			}
			<-start
			created, err := it.repo.CreateCard(ctx, draft)
			errs[idx] = err
			if created != nil {
				ids[idx] = created.ID
			}
		}(i)
	}
	close(start)
	wg.Wait()

	successCount := 0
	var cardID int64
	for i, err := range errs {
		switch {
		case err == nil:
			successCount++
			cardID = ids[i]
		case errors.Is(err, patient.ErrCardExists):
		default:
			t.Fatalf("第 %d 个并发建卡返回意外错误：%v", i, err)
		}
	}
	if successCount != 1 {
		t.Fatalf("并发建卡成功 %d 次，期望恰好 1 次", successCount)
	}
	it.trackCard(cardID)

	rows := it.itestCount(ctx,
		`SELECT COUNT(*) FROM hospital.patient_user_info_card WHERE user_id = $1`, owner.ID)
	if rows != 1 {
		t.Fatalf("该账号就诊卡行数 = %d，期望 1", rows)
	}
}

// TestPatientRepositoryFindCardNotFound 验证不存在的主键映射为 ErrCardNotFound。
func TestPatientRepositoryFindCardNotFound(t *testing.T) {
	it, ctx := newPatientIT(t)
	missing := itestMissingID()
	if _, err := it.repo.FindCardByID(ctx, missing); !errors.Is(err, patient.ErrCardNotFound) {
		t.Fatalf("FindCardByID 错误 = %v，期望 %v", err, patient.ErrCardNotFound)
	}
	if _, err := it.repo.FindCardByPatientID(ctx, missing); !errors.Is(err, patient.ErrCardNotFound) {
		t.Fatalf("FindCardByPatientID 错误 = %v，期望 %v", err, patient.ErrCardNotFound)
	}
}

// TestPatientRepositoryListCardsByPatient 验证列表按 user_id 过滤、total 正确、
// 分页越界返回空列表（非 nil），无卡账号返回 total=0。
func TestPatientRepositoryListCardsByPatient(t *testing.T) {
	it, ctx := newPatientIT(t)
	firstOwner := it.newPatient(ctx)
	firstCard := it.newCard(ctx, firstOwner.ID, itestCardInputMale())
	secondOwner := it.newPatient(ctx)
	secondCard := it.newCard(ctx, secondOwner.ID, itestCardInputFemale())

	items, total, err := it.repo.ListCardsByPatient(ctx, firstOwner.ID, 0, 10)
	if err != nil {
		t.Fatalf("ListCardsByPatient 意外错误：%v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("首位患者 total=%d, len=%d，期望 1/1", total, len(items))
	}
	if items[0].ID != firstCard.ID || items[0].UserID != firstOwner.ID {
		t.Fatalf("列表返回了他人就诊卡：%+v", items[0])
	}

	secondItems, secondTotal, err := it.repo.ListCardsByPatient(ctx, secondOwner.ID, 0, 10)
	if err != nil {
		t.Fatalf("ListCardsByPatient 意外错误：%v", err)
	}
	if secondTotal != 1 || len(secondItems) != 1 {
		t.Fatalf("第二位患者 total=%d, len=%d，期望 1/1", secondTotal, len(secondItems))
	}
	if secondItems[0].ID != secondCard.ID || secondItems[0].UserID != secondOwner.ID {
		t.Fatalf("user_id 过滤失效：%+v", secondItems[0])
	}

	emptyPage, pageTotal, err := it.repo.ListCardsByPatient(ctx, firstOwner.ID, 1, 10)
	if err != nil {
		t.Fatalf("分页越界查询意外错误：%v", err)
	}
	if pageTotal != 1 {
		t.Fatalf("分页越界 total = %d，期望 1", pageTotal)
	}
	if emptyPage == nil {
		t.Fatal("分页越界应返回非 nil 空切片")
	}
	if len(emptyPage) != 0 {
		t.Fatalf("分页越界 len = %d，期望 0", len(emptyPage))
	}

	noCardOwner := it.newPatient(ctx)
	none, noneTotal, err := it.repo.ListCardsByPatient(ctx, noCardOwner.ID, 0, 10)
	if err != nil {
		t.Fatalf("无卡账号查询意外错误：%v", err)
	}
	if noneTotal != 0 || len(none) != 0 {
		t.Fatalf("无卡账号 total=%d, len=%d，期望 0/0", noneTotal, len(none))
	}
	if none == nil {
		t.Fatal("无卡账号应返回非 nil 空切片")
	}
}

// TestPatientRepositoryUpdateCard 验证允许字段落库、不可修改字段被拒绝且数据库不变，
// 以及不存在的卡返回 ErrCardNotFound。
func TestPatientRepositoryUpdateCard(t *testing.T) {
	it, ctx := newPatientIT(t)
	owner := it.newPatient(ctx)
	card := it.newCard(ctx, owner.ID, itestCardInputMale())

	newName := "更名后"
	newTel := "13700137000"
	newHistory := []string{"癫痫", "肾病"}
	updated, err := it.repo.UpdateCard(ctx, card.ID,
		patient.CardUpdate{Name: &newName, Tel: &newTel, MedicalHistory: &newHistory})
	if err != nil {
		t.Fatalf("UpdateCard 意外错误：%v", err)
	}
	if updated.Name != newName || updated.Tel != newTel {
		t.Fatalf("返回值未反映变更：%+v", updated)
	}
	if !reflect.DeepEqual(updated.MedicalHistory, newHistory) {
		t.Fatalf("返回值疾病史 = %v，期望 %v", updated.MedicalHistory, newHistory)
	}

	stored, err := it.repo.FindCardByID(ctx, card.ID)
	if err != nil {
		t.Fatalf("重新读取就诊卡失败：%v", err)
	}
	if stored.Name != newName || stored.Tel != newTel {
		t.Fatalf("变更未落库：%+v", stored)
	}
	if !reflect.DeepEqual(stored.MedicalHistory, newHistory) {
		t.Fatalf("疾病史未落库：%v", stored.MedicalHistory)
	}
	if stored.PID != card.PID || stored.Birthday != card.Birthday ||
		stored.UUID != card.UUID || stored.UserID != owner.ID {
		t.Fatalf("身份字段被意外修改：%+v vs %+v", stored, card)
	}

	immutablePID := itestPIDFemale
	if _, err := it.repo.UpdateCard(ctx, card.ID, patient.CardUpdate{PID: &immutablePID}); !errors.Is(err, patient.ErrPIDImmutable) {
		t.Fatalf("提交 pid 错误 = %v，期望 %v", err, patient.ErrPIDImmutable)
	}
	afterPID, err := it.repo.FindCardByID(ctx, card.ID)
	if err != nil {
		t.Fatalf("重新读取就诊卡失败：%v", err)
	}
	if afterPID.PID != card.PID {
		t.Fatalf("pid 被修改：%q，期望 %q", afterPID.PID, card.PID)
	}

	immutableUserID := int64(999999)
	if _, err := it.repo.UpdateCard(ctx, card.ID, patient.CardUpdate{UserID: &immutableUserID}); !errors.Is(err, patient.ErrUserIDImmutable) {
		t.Fatalf("提交 userId 错误 = %v，期望 %v", err, patient.ErrUserIDImmutable)
	}

	immutableBirthday := "2000-01-01"
	if _, err := it.repo.UpdateCard(ctx, card.ID, patient.CardUpdate{Birthday: &immutableBirthday}); !errors.Is(err, patient.ErrBirthdayImmutable) {
		t.Fatalf("提交 birthday 错误 = %v，期望 %v", err, patient.ErrBirthdayImmutable)
	}

	afterAll, err := it.repo.FindCardByID(ctx, card.ID)
	if err != nil {
		t.Fatalf("重新读取就诊卡失败：%v", err)
	}
	if afterAll.PID != card.PID || afterAll.Birthday != card.Birthday || afterAll.UserID != owner.ID {
		t.Fatalf("不可修改字段被改动：%+v", afterAll)
	}

	if _, err := it.repo.UpdateCard(ctx, itestMissingID(), patient.CardUpdate{Name: &newName}); !errors.Is(err, patient.ErrCardNotFound) {
		t.Fatalf("不存在的卡错误 = %v，期望 %v", err, patient.ErrCardNotFound)
	}
}

// TestPatientRepositoryListFaceAuthByCard 验证按日期倒序返回，
// 空记录返回非 nil 空切片，卡不存在返回 ErrCardNotFound。
func TestPatientRepositoryListFaceAuthByCard(t *testing.T) {
	it, ctx := newPatientIT(t)
	owner := it.newPatient(ctx)
	card := it.newCard(ctx, owner.ID, itestCardInputMale())

	empty, err := it.repo.ListFaceAuthByCard(ctx, card.ID)
	if err != nil {
		t.Fatalf("ListFaceAuthByCard 意外错误：%v", err)
	}
	if empty == nil {
		t.Fatal("无记录时应返回非 nil 空切片")
	}
	if len(empty) != 0 {
		t.Fatalf("无记录时 len = %d，期望 0", len(empty))
	}

	older := it.insertFaceAuth(ctx, card.ID, "2026-01-05")
	newer := it.insertFaceAuth(ctx, card.ID, "2026-06-20")

	records, err := it.repo.ListFaceAuthByCard(ctx, card.ID)
	if err != nil {
		t.Fatalf("ListFaceAuthByCard 意外错误：%v", err)
	}
	if len(records) != 2 {
		t.Fatalf("记录数 = %d，期望 2", len(records))
	}
	if records[0].ID != newer || records[1].ID != older {
		t.Fatalf("未按日期倒序：%+v", records)
	}
	if records[0].Date != "2026-06-20" || records[1].Date != "2026-01-05" {
		t.Fatalf("日期解析异常：%+v", records)
	}

	if _, err := it.repo.ListFaceAuthByCard(ctx, itestMissingID()); !errors.Is(err, patient.ErrCardNotFound) {
		t.Fatalf("不存在的卡错误 = %v，期望 %v", err, patient.ErrCardNotFound)
	}
}
