package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"

	"Medical-Web-Backend/internal/config"
	"Medical-Web-Backend/internal/domain/registration"
)

// 本文件是挂号建单事务的仓储层真实 PostgreSQL 集成测试（契约 §6.2、§11）：
// 覆盖「并发竞争最后一个号源最多成功一个」「同一 pid 跨就诊卡重复挂号」「计划级上限
// 触发整单回滚」「两次计数器递增之后再失败仍然整单回滚」「金额从 doctor_price 读取」
// 「占用判重：EXPIRED 释放、PAID 占用」，并且每次都读库核对 doctor_work_plan.num 与
// doctor_work_plan_schedule.num 两个计数器，证明失败事务不会留下部分更新
// （不会只扣其中一个计数器或留下半条挂号）。
//
// 运行方式（工作树根目录，cmd，注意 set 的引号避免尾随空格）：
//   set "PGSQL_INTEGRATION_TEST=1" && go test ./internal/repo/ -run TestPostgresRegistration -count=1 -v
// 未设置开关或连不上数据库时一律 t.Skip，不计为失败。
//
// 夹具安全：所有写入都带明显夹具标记（doctor.name 与 doctor_price.level 为 itest-reg、
// pid 前缀 ITREG、out_trade_no 前缀 ITREG、patient_user.open_id 前缀 itest-reg），
// 主键取避开现网数据范围的高位随机值或序列值，t.Cleanup 按主键逐条删除并复核残留为 0；
// 不做任何缺少主键限定条件的 UPDATE/DELETE，也不触碰非本用例创建的行。
// 残留复核会按夹具标记做全库 COUNT，因此不要在其它进程同时运行本文件时依赖该断言
// （与既有 SCHEDULE_IT_SWEEP_FIXTURES 同一取舍）；若上次运行被强杀留下残留，
// 设置 REGISTRATION_IT_SWEEP_FIXTURES=1 重跑一次即可完成兜底清理。

// registrationITSkipMsg 是未开启集成测试开关时的跳过说明。
const registrationITSkipMsg = "设置 PGSQL_INTEGRATION_TEST=1 后才会执行 PostgreSQL 集成测试"

// 夹具名称标记：doctor.name 与 doctor_price.level 都是 varchar(20)，'itest-reg' 便于人工识别。
const (
	registrationITDoctorName = "itest-reg"
	registrationITPriceLevel = "itest-reg"
	registrationITOpenIDMark = "itest-reg"
)

// registrationITPIDPrefix 是夹具身份证号前缀：pid 是 CHAR(18)，统一为 ITREG + 13 位十六进制。
const registrationITPIDPrefix = "ITREG"

// registrationITOutTradeNoPrefix 是夹具交易号前缀：out_trade_no 是 CHAR(32)，
// 统一为 ITREG + 27 位十六进制，同时作为残留复核的标记。
const registrationITOutTradeNoPrefix = "ITREG"

// registrationITSweepEnv 是兜底巡检的显式开关：仅取值为 "1" 时才按夹具标记全局清理。
const registrationITSweepEnv = "REGISTRATION_IT_SWEEP_FIXTURES"

// registrationIT 承载一次集成测试所需的连接、仓库、业务时钟与待清理主键。
type registrationIT struct {
	db   *sql.DB
	repo *PostgresRegistrationRepository
	now  time.Time

	// mu 保护下列登记切片：并发用例会有多个 goroutine 同时登记成功落库的主键。
	mu              sync.Mutex
	doctorIDs       []int64  // 本用例创建的 doctor 夹具主键
	priceIDs        []int64  // 本用例创建的 doctor_price 夹具主键
	planIDs         []int64  // 本用例创建的 doctor_work_plan 夹具主键
	slotIDs         []int64  // 本用例创建的 doctor_work_plan_schedule 夹具主键
	patientIDs      []int64  // 本用例创建的 patient_user 夹具主键
	cardIDs         []int64  // 本用例创建的 patient_user_info_card 夹具主键
	registrationIDs []int64  // 本用例创建的 medical_registration 主键
	outTradeNos     []string // 本用例创建的交易号（便于人工排查）
}

// newRegistrationIT 组装真实 PostgreSQL 连接；未开启开关或连不上库时跳过而不是失败。
// 兜底巡检仅在 REGISTRATION_IT_SWEEP_FIXTURES=1 时执行（默认关闭），随后注册清理逻辑。
func newRegistrationIT(t *testing.T) (*registrationIT, context.Context) {
	t.Helper()
	if os.Getenv("PGSQL_INTEGRATION_TEST") != "1" {
		t.Skip(registrationITSkipMsg)
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
	// 并发用例会同时占用多条连接，显式放开连接上限，避免连接池排队掩盖并发竞争。
	db.SetMaxOpenConns(16)

	it := &registrationIT{
		db:   db,
		repo: NewPostgresRegistrationRepository(db),
		now:  time.Now(),
	}
	// 兜底巡检默认关闭：只有显式设置开关（上次运行被强杀需要清理残留）时才执行。
	if os.Getenv(registrationITSweepEnv) == "1" {
		sweepCtx, sweepCancel := context.WithTimeout(context.Background(), 20*time.Second)
		it.sweepLeftoverFixtures(t, sweepCtx)
		sweepCancel()
	}
	t.Cleanup(func() { it.cleanup(t) })
	return it, context.Background()
}

// cleanup 按外键依赖顺序精确删除本用例写入的数据：挂号记录 -> 时段 -> 计划 -> 价目 ->
// 就诊卡 -> 患者账号 -> 医生，随后复核夹具残留为 0，最后关闭连接。
// 本用例自己主键的删除失败一律 t.Errorf 暴露为测试失败（避免脏数据被静默吞掉），
// 仅关闭连接失败用 t.Logf 记录。
func (it *registrationIT) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remove := func(query string, args ...any) {
		t.Helper()
		if _, err := it.db.ExecContext(ctx, query, args...); err != nil {
			t.Errorf("清理夹具失败（sql=%s）：%v", query, err)
		}
	}
	// 先加锁复制登记表，避免与仍在收尾的并发 goroutine 争用同一批切片。
	it.mu.Lock()
	registrationIDs := append([]int64(nil), it.registrationIDs...)
	slotIDs := append([]int64(nil), it.slotIDs...)
	planIDs := append([]int64(nil), it.planIDs...)
	priceIDs := append([]int64(nil), it.priceIDs...)
	cardIDs := append([]int64(nil), it.cardIDs...)
	patientIDs := append([]int64(nil), it.patientIDs...)
	doctorIDs := append([]int64(nil), it.doctorIDs...)
	it.mu.Unlock()

	for _, id := range registrationIDs {
		remove(`DELETE FROM hospital.medical_registration WHERE id = $1`, id)
	}
	for _, id := range slotIDs {
		remove(`DELETE FROM hospital.doctor_work_plan_schedule WHERE id = $1`, id)
	}
	for _, id := range planIDs {
		remove(`DELETE FROM hospital.doctor_work_plan WHERE id = $1`, id)
	}
	for _, id := range priceIDs {
		remove(`DELETE FROM hospital.doctor_price WHERE id = $1`, id)
	}
	for _, id := range cardIDs {
		remove(`DELETE FROM hospital.patient_user_info_card WHERE id = $1`, id)
	}
	for _, id := range patientIDs {
		remove(`DELETE FROM hospital.patient_user WHERE id = $1`, id)
	}
	for _, id := range doctorIDs {
		remove(`DELETE FROM hospital.doctor WHERE id = $1`, id)
	}
	it.assertNoResidue(t, ctx, registrationIDs, slotIDs, planIDs, priceIDs, cardIDs, patientIDs, doctorIDs)
	if err := it.db.Close(); err != nil {
		t.Logf("关闭集成测试数据库连接失败：%v", err)
	}
}

// assertNoResidue 复核夹具已全部删除：先按登记主键精确 COUNT，
// 再按夹具标记（医生名、价目等级、pid 前缀、交易号前缀、open_id 前缀）做一次全库 COUNT，
// 任何非 0 结果都用 t.Errorf 暴露，避免脏数据被静默留在共享库上。
func (it *registrationIT) assertNoResidue(
	t *testing.T,
	ctx context.Context,
	registrationIDs, slotIDs, planIDs, priceIDs, cardIDs, patientIDs, doctorIDs []int64,
) {
	t.Helper()
	// 主键精确复核：表名与列名都是本文件内的常量字面量，不拼接任何外部输入。
	byID := []struct {
		table  string
		column string
		ids    []int64
	}{
		{"medical_registration", "id", registrationIDs},
		{"doctor_work_plan_schedule", "id", slotIDs},
		{"doctor_work_plan", "id", planIDs},
		{"doctor_price", "id", priceIDs},
		{"patient_user_info_card", "id", cardIDs},
		{"patient_user", "id", patientIDs},
		{"doctor", "id", doctorIDs},
	}
	for _, check := range byID {
		if count := it.countIn(t, ctx, check.table, check.column, check.ids); count != 0 {
			t.Errorf("夹具残留：hospital.%s 中 %s IN %v 仍有 %d 行", check.table, check.column, check.ids, count)
		}
	}

	// 标记复核：覆盖「进程被杀、主键尚未登记到内存」的漏网行。
	markers := []struct {
		label string
		query string
	}{
		{"医生", `SELECT COUNT(*) FROM hospital.doctor WHERE name = 'itest-reg'`},
		{"价目", `SELECT COUNT(*) FROM hospital.doctor_price WHERE level = 'itest-reg'`},
		{"就诊卡", `SELECT COUNT(*) FROM hospital.patient_user_info_card WHERE pid LIKE 'ITREG%'`},
		{"挂号记录", `SELECT COUNT(*) FROM hospital.medical_registration WHERE out_trade_no LIKE 'ITREG%'`},
		{"患者账号", `SELECT COUNT(*) FROM hospital.patient_user WHERE open_id LIKE 'itest-reg%'`},
	}
	total := 0
	for _, marker := range markers {
		var count int64
		if err := it.db.QueryRowContext(ctx, marker.query).Scan(&count); err != nil {
			t.Errorf("复核夹具残留失败（%s）：%v", marker.label, err)
			continue
		}
		total += int(count)
		t.Logf("夹具残留复核：%s = %d", marker.label, count)
	}
	if total != 0 {
		t.Errorf("夹具标记复核存在残留：合计 %d 行（doctor.name/doctor_price.level/pid/out_trade_no/open_id 标记）", total)
	}
}

// countIn 用主键列表精确统计行数；table 与 column 只允许传本文件内的常量字面量。
func (it *registrationIT) countIn(t *testing.T, ctx context.Context, table, column string, ids []int64) int64 {
	t.Helper()
	if len(ids) == 0 {
		return 0
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		args = append(args, id)
	}
	query := fmt.Sprintf("SELECT COUNT(*) FROM hospital.%s WHERE %s IN (%s)",
		table, column, strings.Join(placeholders, ","))
	var count int64
	if err := it.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		t.Errorf("按主键复核 hospital.%s 残留失败：%v", table, err)
		return -1
	}
	return count
}

// sweepLeftoverFixtures 兜底巡检：按夹具标记清理历史崩溃可能残留的数据，并打印各步影响行数。
// 只匹配本文件专用的夹具标记（itest-reg / ITREG），不触碰真实业务数据；
// 仅在 REGISTRATION_IT_SWEEP_FIXTURES=1 时由 newRegistrationIT 调用。
func (it *registrationIT) sweepLeftoverFixtures(t *testing.T, ctx context.Context) {
	t.Helper()
	steps := []struct {
		label string
		query string
	}{
		{"挂号记录", `DELETE FROM hospital.medical_registration
WHERE out_trade_no LIKE 'ITREG%'
   OR patient_card_id IN (SELECT id FROM hospital.patient_user_info_card WHERE pid LIKE 'ITREG%')`},
		{"时段行", `DELETE FROM hospital.doctor_work_plan_schedule WHERE work_plan_id IN (
SELECT id FROM hospital.doctor_work_plan WHERE doctor_id IN (SELECT id FROM hospital.doctor WHERE name = 'itest-reg'))`},
		{"计划行", `DELETE FROM hospital.doctor_work_plan WHERE doctor_id IN (SELECT id FROM hospital.doctor WHERE name = 'itest-reg')`},
		{"价目行", `DELETE FROM hospital.doctor_price WHERE level = 'itest-reg'
   OR doctor_id IN (SELECT id FROM hospital.doctor WHERE name = 'itest-reg')`},
		{"就诊卡行", `DELETE FROM hospital.patient_user_info_card WHERE pid LIKE 'ITREG%'`},
		{"患者账号行", `DELETE FROM hospital.patient_user WHERE open_id LIKE 'itest-reg%'`},
		{"夹具医生行", `DELETE FROM hospital.doctor WHERE name = 'itest-reg'`},
	}
	counts := make([]string, 0, len(steps))
	for _, step := range steps {
		result, err := it.db.ExecContext(ctx, step.query)
		if err != nil {
			t.Logf("兜底巡检清理%s失败：%v", step.label, err)
			continue
		}
		affected, err := result.RowsAffected()
		if err != nil {
			t.Logf("兜底巡检读取%s影响行数失败：%v", step.label, err)
			continue
		}
		counts = append(counts, fmt.Sprintf("%s=%d", step.label, affected))
	}
	t.Logf("启动兜底巡检（夹具标记 itest-reg / ITREG）：%s", strings.Join(counts, "、"))
}

// registrationITFutureDate 返回业务时区（Asia/Shanghai）当天偏移 offset 天后的 YYYY-MM-DD。
// 排班日期必须晚于业务当日，否则建单会先命中 ErrScheduleStarted，掩盖号源竞争分支。
func registrationITFutureDate(offset int) string {
	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	return time.Now().In(shanghai).AddDate(0, 0, offset).Format("2006-01-02")
}

// registrationITHighID 生成避开现网数据范围的高位随机主键（20 亿段），供夹具行使用。
func registrationITHighID() int64 {
	return int64(2000000000) + int64(uuid.New().ID()%100000000)
}

// registrationITNewPID 生成夹具身份证号：ITREG + 13 位十六进制，恰好 18 位（pid 为 CHAR(18)）。
func registrationITNewPID() string {
	hex := strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))
	return registrationITPIDPrefix + hex[:13]
}

// registrationITNewOutTradeNo 生成夹具交易号：ITREG + 27 位十六进制，恰好 32 位
// （out_trade_no 为 CHAR(32)），同时作为残留复核标记。
func registrationITNewOutTradeNo() string {
	hex := strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))
	return registrationITOutTradeNoPrefix + hex[:27]
}

// freeID 生成一个在指定表中不存在的高位随机主键；table 只允许传本文件内的常量字面量。
func (it *registrationIT) freeID(t *testing.T, ctx context.Context, table string) int64 {
	t.Helper()
	for attempt := 0; attempt < 5; attempt++ {
		id := registrationITHighID()
		var exists bool
		query := fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM hospital.%s WHERE id = $1)", table)
		if err := it.db.QueryRowContext(ctx, query, id).Scan(&exists); err != nil {
			t.Fatalf("检查 hospital.%s 主键是否冲突失败：%v", table, err)
		}
		if !exists {
			return id
		}
	}
	t.Fatalf("连续多次生成的 hospital.%s 主键均与现有数据冲突", table)
	return 0
}

// insertDoctor 插入一名夹具医生（status=1 在诊），并登记待清理主键。
func (it *registrationIT) insertDoctor(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	doctorID := it.freeID(t, ctx, "doctor")
	if _, err := it.db.ExecContext(ctx,
		`INSERT INTO hospital.doctor(id, name, status) VALUES ($1, $2, 1)`,
		doctorID, registrationITDoctorName); err != nil {
		t.Fatalf("插入夹具医生失败：%v", err)
	}
	it.mu.Lock()
	it.doctorIDs = append(it.doctorIDs, doctorID)
	it.mu.Unlock()
	return doctorID
}

// insertDoctorPrice 为夹具医生插入一条价目（level 兼作夹具标记），并登记待清理主键。
func (it *registrationIT) insertDoctorPrice(t *testing.T, ctx context.Context, doctorID int64, price1, price2 string) int64 {
	t.Helper()
	priceID := it.freeID(t, ctx, "doctor_price")
	if _, err := it.db.ExecContext(ctx,
		`INSERT INTO hospital.doctor_price(id, doctor_id, level, price_1, price_2)
VALUES ($1, $2, $3, $4::numeric, $5::numeric)`,
		priceID, doctorID, registrationITPriceLevel, price1, price2); err != nil {
		t.Fatalf("插入夹具价目失败：%v", err)
	}
	it.mu.Lock()
	it.priceIDs = append(it.priceIDs, priceID)
	it.mu.Unlock()
	return priceID
}

// insertPatientUser 插入一个夹具患者账号（open_id 前缀 itest-reg 作为标记），并登记待清理主键。
func (it *registrationIT) insertPatientUser(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	patientID := it.freeID(t, ctx, "patient_user")
	if _, err := it.db.ExecContext(ctx,
		`INSERT INTO hospital.patient_user(id, open_id, nickname, status, create_time)
VALUES ($1, $2, 'itest-reg', 1, $3::date)`,
		patientID, registrationITOpenIDMark+"-"+uuid.NewString(), registration.BusinessDate(it.now)); err != nil {
		t.Fatalf("插入夹具患者账号失败：%v", err)
	}
	it.mu.Lock()
	it.patientIDs = append(it.patientIDs, patientID)
	it.mu.Unlock()
	return patientID
}

// insertCard 插入一张夹具就诊卡；pid 由调用方给出，同一 pid 可插入多张卡以覆盖跨卡重复挂号。
func (it *registrationIT) insertCard(t *testing.T, ctx context.Context, userID int64, pid string) int64 {
	t.Helper()
	cardID := it.freeID(t, ctx, "patient_user_info_card")
	if _, err := it.db.ExecContext(ctx,
		`INSERT INTO hospital.patient_user_info_card
(id, user_id, uuid, name, sex, pid, tel, exist_face_model)
VALUES ($1, $2, $3, 'itest-reg', '1', $4, '13800138000', false)`,
		cardID, userID, strings.ReplaceAll(uuid.NewString(), "-", ""), pid); err != nil {
		t.Fatalf("插入夹具就诊卡失败（pid=%s）：%v", pid, err)
	}
	it.mu.Lock()
	it.cardIDs = append(it.cardIDs, cardID)
	it.mu.Unlock()
	return cardID
}

// deptSubID 取一个存在的子科室主键；库中为空时回退到高位随机值。
// 之所以能安全回退随机值：init.sql 里 medical_registration 等表都没有 FOREIGN KEY，
// 建单事务也不会回查 medical_dept_sub；将来若补上外键约束，这里必须改为先插入夹具子科室。
func (it *registrationIT) deptSubID(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var subdepartmentID int64
	err := it.db.QueryRowContext(ctx, `SELECT id FROM hospital.medical_dept_sub ORDER BY id LIMIT 1`).Scan(&subdepartmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return registrationITHighID()
	}
	if err != nil {
		t.Fatalf("查询子科室失败：%v", err)
	}
	return subdepartmentID
}

// insertPlan 直接以 SQL 插入一条排班计划（id 取序列 nextval、num=0）并登记待清理主键。
func (it *registrationIT) insertPlan(t *testing.T, ctx context.Context, doctorID, deptSubID int64, date string, maximum int16) int64 {
	t.Helper()
	var planID int64
	err := it.db.QueryRowContext(ctx, `INSERT INTO hospital.doctor_work_plan(id, doctor_id, dept_sub_id, date, maximum, num)
VALUES (nextval('hospital.doctor_work_plan_sequence'), $1, $2, $3::date, $4, 0) RETURNING id`,
		doctorID, deptSubID, date, maximum).Scan(&planID)
	if err != nil {
		t.Fatalf("插入夹具排班计划失败：%v", err)
	}
	it.mu.Lock()
	it.planIDs = append(it.planIDs, planID)
	it.mu.Unlock()
	return planID
}

// insertSlot 直接以 SQL 插入一条时段行（num=0）并登记待清理主键。
func (it *registrationIT) insertSlot(t *testing.T, ctx context.Context, planID int64, slot, maximum int16) int64 {
	t.Helper()
	var slotID int64
	err := it.db.QueryRowContext(ctx, `INSERT INTO hospital.doctor_work_plan_schedule(id, work_plan_id, slot, maximum, num)
VALUES (nextval('hospital.doctor_work_plan_schedule_sequence'), $1, $2, $3, 0) RETURNING id`,
		planID, slot, maximum).Scan(&slotID)
	if err != nil {
		t.Fatalf("插入夹具时段失败：%v", err)
	}
	it.mu.Lock()
	it.slotIDs = append(it.slotIDs, slotID)
	it.mu.Unlock()
	return slotID
}

// createRegistration 调用被测仓储建单，并在成功后登记主键与交易号（并发安全，供多 goroutine 调用）。
// 这里刻意不接收 *testing.T：goroutine 中不得调用 t.Fatalf，错误一律交给调用方断言。
func (it *registrationIT) createRegistration(
	ctx context.Context,
	cardID int64,
	pid string,
	scheduleID int64,
) (*registration.Registration, error) {
	created, err := it.repo.CreateRegistration(ctx, registration.CreateInput{
		PatientCardID: cardID,
		PID:           pid,
		ScheduleID:    scheduleID,
		OutTradeNo:    registrationITNewOutTradeNo(),
		Now:           it.now,
	})
	if err != nil {
		return nil, err
	}
	it.mu.Lock()
	it.registrationIDs = append(it.registrationIDs, created.ID)
	it.outTradeNos = append(it.outTradeNos, created.OutTradeNo)
	it.mu.Unlock()
	return created, nil
}

// planUsed 读取计划级计数器 doctor_work_plan.num。
func (it *registrationIT) planUsed(t *testing.T, ctx context.Context, planID int64) int16 {
	t.Helper()
	var used int16
	if err := it.db.QueryRowContext(ctx, `SELECT num FROM hospital.doctor_work_plan WHERE id = $1`, planID).Scan(&used); err != nil {
		t.Fatalf("查询计划级 num 失败：%v", err)
	}
	return used
}

// slotUsed 读取时段级计数器 doctor_work_plan_schedule.num。
func (it *registrationIT) slotUsed(t *testing.T, ctx context.Context, slotID int64) int16 {
	t.Helper()
	var used int16
	if err := it.db.QueryRowContext(ctx, `SELECT num FROM hospital.doctor_work_plan_schedule WHERE id = $1`, slotID).Scan(&used); err != nil {
		t.Fatalf("查询时段级 num 失败：%v", err)
	}
	return used
}

// countScheduleRegistrations 统计某时段下的挂号行数，证明失败事务没有落库半条记录。
func (it *registrationIT) countScheduleRegistrations(t *testing.T, ctx context.Context, slotID int64) int {
	t.Helper()
	var count int
	if err := it.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM hospital.medical_registration WHERE doctor_schedule_id = $1`, slotID).Scan(&count); err != nil {
		t.Fatalf("统计时段挂号行数失败：%v", err)
	}
	return count
}

// registrationAmountFromDB 读取落库金额的文本形式（numeric -> text），核对不是仅靠返回值自洽。
func (it *registrationIT) registrationAmountFromDB(t *testing.T, ctx context.Context, registrationID int64) string {
	t.Helper()
	var amount string
	if err := it.db.QueryRowContext(ctx,
		`SELECT amount::text FROM hospital.medical_registration WHERE id = $1`, registrationID).Scan(&amount); err != nil {
		t.Fatalf("查询落库金额失败：%v", err)
	}
	return amount
}

// setPaymentStatus 只按主键更新本用例创建的挂号行状态与创建日期，供占用判重语义用例使用。
// 严禁在此处去掉 id 条件：这是共享库上的唯一安全写法。
func (it *registrationIT) setPaymentStatus(t *testing.T, ctx context.Context, registrationID int64, status int16, createDate string) {
	t.Helper()
	if _, err := it.db.ExecContext(ctx,
		`UPDATE hospital.medical_registration SET payment_status = $2, create_time = $3::date WHERE id = $1`,
		registrationID, status, createDate); err != nil {
		t.Fatalf("更新夹具挂号状态失败（id=%d）：%v", registrationID, err)
	}
}

// TestPostgresRegistrationRepositoryConcurrentLastSlot 覆盖契约 §11「最后一个号源并发竞争
// 最多成功一个」：时段 maximum=1，8 个 goroutine 用不同 pid 同时建单同一时段，
// 恰好 1 个成功、其余全部命中 *registration.SlotSoldOutError。
//
// 口径说明（不要据此断言推导「回滚」）：本用例证明的是「行锁 + 前置余量校验 + 条件更新
// num < maximum 足以保证计数器不被重复扣减，8 个并发请求里只有一个进入递增路径」。
// 失败方是在 createRegistrationTx 的前置余量校验（slotMaximum - slotUsed <= 0）处提前返回的，
// 根本没有执行计划级 UPDATE，所以「两个计数器仍为 1」在这里不构成回滚证据。
// 「两次递增之后再失败仍然整单回滚」的证据见
// TestPostgresRegistrationRepositoryRollbackAfterCounterIncrement。
func TestPostgresRegistrationRepositoryConcurrentLastSlot(t *testing.T) {
	it, ctx := newRegistrationIT(t)
	doctorID := it.insertDoctor(t, ctx)
	deptSubID := it.deptSubID(t, ctx)
	userID := it.insertPatientUser(t, ctx)

	const workers = 8
	// 计划级上限给足，让「时段级 maximum=1」成为唯一瓶颈，避免计划级先于时段级失败。
	planID := it.insertPlan(t, ctx, doctorID, deptSubID, registrationITFutureDate(7), workers)
	slotID := it.insertSlot(t, ctx, planID, 1, 1)

	type attempt struct {
		cardID int64
		pid    string
	}
	attempts := make([]attempt, 0, workers)
	for i := 0; i < workers; i++ {
		pid := registrationITNewPID()
		attempts = append(attempts, attempt{cardID: it.insertCard(t, ctx, userID, pid), pid: pid})
	}

	start := make(chan struct{})
	var (
		wg        sync.WaitGroup
		resultMu  sync.Mutex
		succeeded []*registration.Registration
		soldOut   int
		others    []error
	)
	for _, a := range attempts {
		wg.Add(1)
		go func(a attempt) {
			defer wg.Done()
			<-start // 同时起跑，最大化竞争窗口
			created, err := it.createRegistration(ctx, a.cardID, a.pid, slotID)
			resultMu.Lock()
			defer resultMu.Unlock()
			switch {
			case err == nil:
				succeeded = append(succeeded, created)
			case errors.As(err, new(*registration.SlotSoldOutError)):
				soldOut++
			default:
				others = append(others, err)
			}
		}(a)
	}
	close(start)
	wg.Wait()

	if len(others) != 0 {
		t.Fatalf("并发建单出现非号源售罄错误 %d 个：%v", len(others), others)
	}
	if len(succeeded) != 1 {
		t.Fatalf("并发竞争最后一个号源成功数 = %d，期望恰好 1", len(succeeded))
	}
	if soldOut != workers-1 {
		t.Fatalf("命中号源售罄的失败数 = %d，期望 %d", soldOut, workers-1)
	}
	if got := succeeded[0]; got.ScheduleID != slotID || got.PaymentStatus != registration.PaymentStatusUnpaid {
		t.Fatalf("成功挂号字段异常：%+v", got)
	}
	// 读库校验：两个计数器都必须恰好为 1，且只落库 1 条挂号记录。
	planNum, slotNum := it.planUsed(t, ctx, planID), it.slotUsed(t, ctx, slotID)
	if planNum != 1 || slotNum != 1 {
		t.Fatalf("并发结束后计数器漂移：doctor_work_plan.num=%d、doctor_work_plan_schedule.num=%d，期望均为 1", planNum, slotNum)
	}
	if count := it.countScheduleRegistrations(t, ctx, slotID); count != 1 {
		t.Fatalf("时段 %d 落库挂号行数 = %d，期望 1", slotID, count)
	}
	t.Logf("并发竞争最后一个号源实测：成功=%d、号源售罄=%d、其他错误=%d；plan.num=%d、slot.num=%d、挂号行数=%d",
		len(succeeded), soldOut, len(others), planNum, slotNum, 1)
}

// TestPostgresRegistrationRepositoryDuplicatePIDAcrossCards 覆盖「同一身份证号跨不同就诊卡
// 重复挂号」：同一 pid 两张卡先后对同一时段建单，第二张必须返回 registration.ErrDuplicate，
// 并且两个计数器仍为 1（失败请求没有重复扣号）。
func TestPostgresRegistrationRepositoryDuplicatePIDAcrossCards(t *testing.T) {
	it, ctx := newRegistrationIT(t)
	doctorID := it.insertDoctor(t, ctx)
	deptSubID := it.deptSubID(t, ctx)
	userID := it.insertPatientUser(t, ctx)
	planID := it.insertPlan(t, ctx, doctorID, deptSubID, registrationITFutureDate(7), 5)
	slotID := it.insertSlot(t, ctx, planID, 1, 5)

	pid := registrationITNewPID()
	firstCard := it.insertCard(t, ctx, userID, pid)
	secondCard := it.insertCard(t, ctx, userID, pid) // 同一 pid 的第二张就诊卡

	if _, err := it.createRegistration(ctx, firstCard, pid, slotID); err != nil {
		t.Fatalf("第一张卡建单意外失败：%v", err)
	}
	created, err := it.createRegistration(ctx, secondCard, pid, slotID)
	if err == nil {
		t.Fatalf("同一 pid 的第二张卡重复挂号意外成功：%+v", created)
	}
	if !errors.Is(err, registration.ErrDuplicate) {
		t.Fatalf("重复挂号错误 = %v，期望 %v", err, registration.ErrDuplicate)
	}
	planNum, slotNum := it.planUsed(t, ctx, planID), it.slotUsed(t, ctx, slotID)
	if planNum != 1 || slotNum != 1 {
		t.Fatalf("判重失败后计数器漂移：plan.num=%d、slot.num=%d，期望均为 1", planNum, slotNum)
	}
	if count := it.countScheduleRegistrations(t, ctx, slotID); count != 1 {
		t.Fatalf("时段 %d 落库挂号行数 = %d，期望 1", slotID, count)
	}
	t.Logf("同一 pid 跨两张卡判重实测：错误=%v；plan.num=%d、slot.num=%d、挂号行数=%d", err, planNum, slotNum, 1)
}

// TestPostgresRegistrationRepositoryPlanCapacityRollback 覆盖「计划级上限成为瓶颈时建单被拒」：
// plan.maximum=1、slot.maximum=5，第一个（pid A）建单成功；第二个（pid B，不同 pid）必须
// 命中 *registration.SlotSoldOutError（计划级条件更新 num<maximum 未生效），
// 且时段级与计划级计数器都停在 1，证明被拒请求没有留下任何部分更新（两个计数器不漂移）。
//
// 口径说明：被测实现的事务顺序是「先加计划级 num、再时段级 num」，因此计划级条件更新失败时
// 时段级 UPDATE 根本不会执行——本用例证明的是「计划级失败后时段级保持原值」。
// 「两次递增之后再失败仍然整单回滚」的证据见
// TestPostgresRegistrationRepositoryRollbackAfterCounterIncrement（注入 INSERT 绑定失败）。
func TestPostgresRegistrationRepositoryPlanCapacityRollback(t *testing.T) {
	it, ctx := newRegistrationIT(t)
	doctorID := it.insertDoctor(t, ctx)
	deptSubID := it.deptSubID(t, ctx)
	userID := it.insertPatientUser(t, ctx)
	planID := it.insertPlan(t, ctx, doctorID, deptSubID, registrationITFutureDate(7), 1)
	slotID := it.insertSlot(t, ctx, planID, 1, 5)

	firstPID := registrationITNewPID()
	if _, err := it.createRegistration(ctx, it.insertCard(t, ctx, userID, firstPID), firstPID, slotID); err != nil {
		t.Fatalf("计划仍余 1 个号源时建单意外失败：%v", err)
	}

	secondPID := registrationITNewPID()
	created, err := it.createRegistration(ctx, it.insertCard(t, ctx, userID, secondPID), secondPID, slotID)
	if err == nil {
		t.Fatalf("计划级号源已满时建单意外成功：%+v", created)
	}
	var soldOutErr *registration.SlotSoldOutError
	if !errors.As(err, &soldOutErr) {
		t.Fatalf("计划级售罄错误 = %v，期望 *registration.SlotSoldOutError", err)
	}
	if soldOutErr.ScheduleID != slotID {
		t.Fatalf("SlotSoldOutError.ScheduleID = %d，期望 %d", soldOutErr.ScheduleID, slotID)
	}
	planNum, slotNum := it.planUsed(t, ctx, planID), it.slotUsed(t, ctx, slotID)
	if planNum != 1 || slotNum != 1 {
		t.Fatalf("计划级失败后计数器漂移：plan.num=%d、slot.num=%d，期望均为 1", planNum, slotNum)
	}
	if count := it.countScheduleRegistrations(t, ctx, slotID); count != 1 {
		t.Fatalf("时段 %d 落库挂号行数 = %d，期望 1", slotID, count)
	}
	t.Logf("计划级瓶颈实测：错误=%v（remaining=%d）；plan.num=%d、slot.num=%d、挂号行数=%d",
		err, soldOutErr.Remaining, planNum, slotNum, 1)
}

// registrationITOverflowCardID 是故意超出 PostgreSQL INTEGER(int4) 上限的就诊卡主键，
// 用来把建单失败点固定在事务最后一步的 INSERT 上。
const registrationITOverflowCardID int64 = 4000000000

// TestPostgresRegistrationRepositoryRollbackAfterCounterIncrement 覆盖契约 §11
// 「失败整单回滚、不留下部分更新」，并且把失败点注入到两次计数器递增之后：
// 先正控（合法就诊卡建单成功，证明两个计数器确实会 +1），再用全新 pid + 超范围就诊卡
// 让 INSERT 失败，最后核对计数器停在正控后的值、且失败那一单没有落库。
//
// 注入点为什么成立（已实测，勿凭推测改动）：
//   - medical_registration.patient_card_id 是 INTEGER(int4)，传入 4000000000 时 INSERT 在绑定
//     参数阶段就会失败，实测错误为 failed to encode args[0]: unable to encode 4000000000
//     into binary format for int4 (OID 23)；args[0] 正是该 INSERT 的 patient_card_id，
//     因此可以确认失败点就在 INSERT，而不是更早的业务校验。
//   - 通过前置校验后，createRegistrationTx 是直线执行：计划级 UPDATE -> 时段级 UPDATE ->
//     读价目 -> INSERT。两个计数器 UPDATE 只要 RowsAffected==0 就会提前返回
//     *registration.SlotSoldOutError，所以「拿到 INSERT 的绑定错误」本身就说明两次递增已执行过。
//   - 就诊卡主键在仓储层只作为 INSERT 参数使用，事务开始前只校验 OutTradeNo（见
//     internal/repo/postgres_registration.go 的 CreateRegistration），不存在提前校验就诊卡
//     把失败点前移的路径，所以本用例不需要（也无法）插入该超范围就诊卡行。
func TestPostgresRegistrationRepositoryRollbackAfterCounterIncrement(t *testing.T) {
	it, ctx := newRegistrationIT(t)
	doctorID := it.insertDoctor(t, ctx)
	it.insertDoctorPrice(t, ctx, doctorID, "80.00", "200.00")
	deptSubID := it.deptSubID(t, ctx)
	userID := it.insertPatientUser(t, ctx)
	planID := it.insertPlan(t, ctx, doctorID, deptSubID, registrationITFutureDate(7), 5)
	slotID := it.insertSlot(t, ctx, planID, 1, 5)

	// 步骤 1：初值必须为 0，否则「+1 后回滚」无从判断。
	if planNum, slotNum := it.planUsed(t, ctx, planID), it.slotUsed(t, ctx, slotID); planNum != 0 || slotNum != 0 {
		t.Fatalf("夹具初值异常：plan.num=%d、slot.num=%d，期望均为 0", planNum, slotNum)
	}

	// 步骤 2：正控。合法就诊卡建单成功，证明两个计数器在成功路径上确实会 +1。
	controlPID := registrationITNewPID()
	if _, err := it.createRegistration(ctx, it.insertCard(t, ctx, userID, controlPID), controlPID, slotID); err != nil {
		t.Fatalf("正控建单意外失败：%v", err)
	}
	planNumAfterControl, slotNumAfterControl := it.planUsed(t, ctx, planID), it.slotUsed(t, ctx, slotID)
	if planNumAfterControl != 1 || slotNumAfterControl != 1 {
		t.Fatalf("正控后计数器异常：plan.num=%d、slot.num=%d，期望均为 1", planNumAfterControl, slotNumAfterControl)
	}
	if count := it.countScheduleRegistrations(t, ctx, slotID); count != 1 {
		t.Fatalf("正控后时段 %d 落库挂号行数 = %d，期望 1", slotID, count)
	}

	// 步骤 3：回滚。全新 pid（保证不会被占用判重提前拦截）+ 超范围就诊卡，让失败点落在 INSERT。
	failingPID := registrationITNewPID()
	created, err := it.repo.CreateRegistration(ctx, registration.CreateInput{
		PatientCardID: registrationITOverflowCardID,
		PID:           failingPID,
		ScheduleID:    slotID,
		OutTradeNo:    registrationITNewOutTradeNo(),
		Now:           it.now,
	})
	if err == nil {
		t.Fatalf("超范围就诊卡建单意外成功：%+v", created)
	}
	// 必须先确认这不是「递增之前」的业务错误，否则本用例证明不了任何回滚。
	var soldOutErr *registration.SlotSoldOutError
	if errors.As(err, &soldOutErr) {
		t.Fatalf("失败点被前置余量校验拦截（%v），没有走到计数器递增：%v", soldOutErr, err)
	}
	if errors.Is(err, registration.ErrDuplicate) {
		t.Fatalf("失败点被占用判重拦截，没有走到计数器递增：%v", err)
	}
	if marker := registrationITBindingFailureMarker(err); marker == "" {
		t.Fatalf("注入的错误不是预期的参数绑定失败，无法确认失败点位于 INSERT：%v", err)
	}

	// 步骤 4：核心断言。计数器停在正控后的值（递增已随事务回滚），且失败那一单没有落库。
	planNum, slotNum := it.planUsed(t, ctx, planID), it.slotUsed(t, ctx, slotID)
	if planNum != planNumAfterControl || slotNum != slotNumAfterControl {
		t.Fatalf("回滚后计数器漂移：plan.num=%d、slot.num=%d，期望仍是正控后的 %d/%d",
			planNum, slotNum, planNumAfterControl, slotNumAfterControl)
	}
	if count := it.countScheduleRegistrations(t, ctx, slotID); count != 1 {
		t.Fatalf("回滚后时段 %d 落库挂号行数 = %d，期望 1", slotID, count)
	}
	// 挂号行只按 patient_card_id 关联就诊卡（表里不存 pid），超范围主键与真实数据不会碰撞，
	// 因此直接按它计数即可证明「半条挂号」没有落库；注意 $1 必须显式转 bigint，
	// 否则 pgx 会按列类型 int4 绑定，同样报参数超出 int4 范围。
	var leaked int
	if err := it.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM hospital.medical_registration WHERE patient_card_id = $1::bigint`,
		registrationITOverflowCardID).Scan(&leaked); err != nil {
		t.Fatalf("按超范围就诊卡统计挂号行失败：%v", err)
	}
	if leaked != 0 {
		t.Fatalf("超范围就诊卡 %d 名下已落库挂号行数 = %d，期望 0", registrationITOverflowCardID, leaked)
	}
	t.Logf("递增后失败整单回滚实测：注入错误=%v（判定标记=%s）；失败前 plan.num=%d、slot.num=%d；"+
		"失败后 plan.num=%d、slot.num=%d、挂号行数=%d、超范围卡名下挂号行数=%d",
		err, registrationITBindingFailureMarker(err), planNumAfterControl, slotNumAfterControl,
		planNum, slotNum, 1, leaked)
}

// registrationITBindingFailureMarker 判断 err 是否为「超范围就诊卡主键在绑定 int4 参数时被拒绝」，
// 命中时返回命中的标记串（便于日志），未命中返回空串。
// 判据只看「驱动或服务端在绑定阶段拒绝 int4 超范围取值」这一事实，不绑定完整错误文案，
// 避免驱动升级改文案就让用例失败。
func registrationITBindingFailureMarker(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, marker := range []string{"int4", "integer", "out of range", "encode"} {
		if strings.Contains(message, marker) {
			return marker
		}
	}
	return ""
}

// TestPostgresRegistrationRepositoryAmountFromDoctorPrice 覆盖契约 §6.2「金额从 doctor_price
// 重新读取」：夹具价目 price_1=80.00 时，返回实体的 Amount 与落库 amount 都必须是 "80.00"。
func TestPostgresRegistrationRepositoryAmountFromDoctorPrice(t *testing.T) {
	it, ctx := newRegistrationIT(t)
	doctorID := it.insertDoctor(t, ctx)
	it.insertDoctorPrice(t, ctx, doctorID, "80.00", "200.00")
	deptSubID := it.deptSubID(t, ctx)
	userID := it.insertPatientUser(t, ctx)
	planID := it.insertPlan(t, ctx, doctorID, deptSubID, registrationITFutureDate(7), 5)
	slotID := it.insertSlot(t, ctx, planID, 1, 5)

	pid := registrationITNewPID()
	created, err := it.createRegistration(ctx, it.insertCard(t, ctx, userID, pid), pid, slotID)
	if err != nil {
		t.Fatalf("建单意外失败：%v", err)
	}
	if created.Amount != "80.00" {
		t.Fatalf("返回实体 Amount = %q，期望 \"80.00\"", created.Amount)
	}
	amount := it.registrationAmountFromDB(t, ctx, created.ID)
	if amount != "80.00" {
		t.Fatalf("落库 amount::text = %q，期望 \"80.00\"", amount)
	}
	t.Logf("金额口径实测：doctor_price.price_1=80.00 -> 实体 Amount=%q、落库 amount=%q", created.Amount, amount)
}

// TestPostgresRegistrationRepositoryOccupyingSemantics 覆盖契约 §6.1 的占用判重语义：
// 同一 pid 同一时段，把已有记录改为 EXPIRED(4) 后可以再次挂号成功；改为 PAID(2) 后，
// 即使创建日期被改到很早（2020-01-01）也仍然占用，重复挂号返回 registration.ErrDuplicate。
//
// 已知偏差（本用例不断言该路径，仅在此记录）：medical_registration.create_time 是 DATE 列，
// §6.4 的 35 分钟支付有效期只能按「创建日期 >= 当前业务日」近似，跨业务日（昨天 23:50 创建、
// 仍在 35 分钟内）的 UNPAID 单会被判为不占用，存在漏判重复挂号的风险。
func TestPostgresRegistrationRepositoryOccupyingSemantics(t *testing.T) {
	it, ctx := newRegistrationIT(t)
	doctorID := it.insertDoctor(t, ctx)
	deptSubID := it.deptSubID(t, ctx)
	userID := it.insertPatientUser(t, ctx)
	planID := it.insertPlan(t, ctx, doctorID, deptSubID, registrationITFutureDate(7), 5)
	slotID := it.insertSlot(t, ctx, planID, 1, 5)
	pid := registrationITNewPID()
	cardID := it.insertCard(t, ctx, userID, pid)

	first, err := it.createRegistration(ctx, cardID, pid, slotID)
	if err != nil {
		t.Fatalf("首次建单意外失败：%v", err)
	}
	// 历史单过期（EXPIRED）后不再占用号源，允许重新挂号。
	it.setPaymentStatus(t, ctx, first.ID, registration.PaymentCodeExpired, registration.BusinessDate(it.now))
	second, err := it.createRegistration(ctx, cardID, pid, slotID)
	if err != nil {
		t.Fatalf("已有记录置为 EXPIRED 后重新挂号意外失败：%v", err)
	}
	// 已付款（PAID）一定占用：把创建日期改到很早也仍然拦住重复挂号。
	it.setPaymentStatus(t, ctx, second.ID, registration.PaymentCodePaid, "2020-01-01")
	if created, err := it.createRegistration(ctx, cardID, pid, slotID); !errors.Is(err, registration.ErrDuplicate) {
		t.Fatalf("存在 PAID 记录时重复挂号错误 = %v（created=%+v），期望 %v", err, created, registration.ErrDuplicate)
	}
	planNum, slotNum := it.planUsed(t, ctx, planID), it.slotUsed(t, ctx, slotID)
	if planNum != 2 || slotNum != 2 {
		t.Fatalf("占用语义用例计数器异常：plan.num=%d、slot.num=%d，期望均为 2", planNum, slotNum)
	}
	t.Logf("占用语义实测：EXPIRED 后可再建单（第二次 id=%d），PAID 后判重生效；plan.num=%d、slot.num=%d", second.ID, planNum, slotNum)
}
