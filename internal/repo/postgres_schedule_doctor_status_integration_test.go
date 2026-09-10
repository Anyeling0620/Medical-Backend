package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"

	"Medical-Web-Backend/internal/config"
	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/domain/schedule"
)

// 本文件覆盖“离职或退休医生不能新增出诊时段”缺陷的仓储层真实 PostgreSQL 集成测试：
// CreateSlot 必须在事务内校验计划所属医生仍为 ACTIVE（doctor.status=1），
// 非在诊状态（含医生行缺失）返回 schedule.ErrDoctorInactive 且不落库任何时段行；
// 同时校验医生目录按 status 过滤（列表能展示不同状态医生）。
// 未设置 PGSQL_INTEGRATION_TEST=1 或连不上数据库时一律 t.Skip，不计为失败；
// 所有写入数据都按主键精确清理，夹具数据统一使用 itest-fixture 名称，便于兜底巡检识别。
// 兜底巡检默认关闭（多工作树/多进程并行跑本包时按 name 全局删除会误删其它进程在途的夹具，
// 含 status=1 的 ACTIVE 夹具）；若上次运行被强杀留下残留，设置 SCHEDULE_IT_SWEEP_FIXTURES=1
// 重跑一次即可清理干净，例如：
//   $env:PGSQL_INTEGRATION_TEST='1'; $env:SCHEDULE_IT_SWEEP_FIXTURES='1'; go test ./internal/repo/ -run TestScheduleRepository -v -count=1

// scheduleDoctorStatusSkipMsg 是未开启集成测试开关时的跳过说明。
const scheduleDoctorStatusSkipMsg = "设置 PGSQL_INTEGRATION_TEST=1 后才会执行 PostgreSQL 集成测试"

// 临时夹具医生的名称（doctor.name 为 varchar(20)，长度必须受限），便于人工识别与兜底清理。
const scheduleDoctorStatusFixtureName = "itest-fixture"

// scheduleDoctorStatusSweepEnv 是兜底巡检的显式开关：仅取值为 "1" 时才执行巡检。
// 巡检按 name 全局删除，默认关闭以避免误删并行运行进程中在途的夹具数据。
const scheduleDoctorStatusSweepEnv = "SCHEDULE_IT_SWEEP_FIXTURES"

// scheduleDoctorStatusIT 承载一次集成测试所需的连接、仓库、业务时钟与待清理主键。
type scheduleDoctorStatusIT struct {
	db        *sql.DB
	slots     *PostgresScheduleRepository
	doctors   *PostgresDoctorRepository
	now       time.Time
	planIDs   []int64 // 本用例创建的 doctor_work_plan 主键
	linkIDs   []int64 // 本用例创建的 medical_dept_sub_and_doctor 夹具主键
	doctorIDs []int64 // 本用例创建的临时 doctor 夹具主键
}

// newScheduleDoctorStatusIT 组装真实 PostgreSQL 连接；未开启开关或连不上时跳过而不是失败。
// 兜底巡检仅在 SCHEDULE_IT_SWEEP_FIXTURES=1 时执行（默认关闭），随后注册清理逻辑。
func newScheduleDoctorStatusIT(t *testing.T) (*scheduleDoctorStatusIT, context.Context) {
	t.Helper()
	if os.Getenv("PGSQL_INTEGRATION_TEST") != "1" {
		t.Skip(scheduleDoctorStatusSkipMsg)
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

	it := &scheduleDoctorStatusIT{
		db:      db,
		slots:   NewPostgresScheduleRepository(db),
		doctors: NewPostgresDoctorRepository(db),
		now:     time.Now(),
	}
	// 兜底巡检默认关闭：只有显式设置开关（上次运行被强杀需要清理残留）时才执行。
	if os.Getenv(scheduleDoctorStatusSweepEnv) == "1" {
		sweepCtx, sweepCancel := context.WithTimeout(context.Background(), 20*time.Second)
		it.sweepLeftoverFixtures(t, sweepCtx)
		sweepCancel()
	}
	t.Cleanup(func() { it.cleanup(t) })
	return it, context.Background()
}

// cleanup 按外键依赖顺序精确删除本用例写入的数据：时段行 -> 计划行 -> 关联行 -> 夹具医生行，
// 最后关闭连接。本用例自己主键的删除失败一律 t.Errorf 暴露为测试失败（避免脏数据被静默吞掉），
// 仅关闭连接失败用 t.Logf 记录。
func (it *scheduleDoctorStatusIT) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remove := func(query string, args ...any) {
		t.Helper()
		if _, err := it.db.ExecContext(ctx, query, args...); err != nil {
			t.Errorf("清理失败（sql=%s）：%v", query, err)
		}
	}
	for _, planID := range it.planIDs {
		remove(`DELETE FROM hospital.doctor_work_plan_schedule WHERE work_plan_id = $1`, planID)
		remove(`DELETE FROM hospital.doctor_work_plan WHERE id = $1`, planID)
	}
	for _, linkID := range it.linkIDs {
		remove(`DELETE FROM hospital.medical_dept_sub_and_doctor WHERE id = $1`, linkID)
	}
	for _, doctorID := range it.doctorIDs {
		remove(`DELETE FROM hospital.doctor WHERE id = $1`, doctorID)
	}
	if err := it.db.Close(); err != nil {
		t.Logf("关闭集成测试数据库连接失败：%v", err)
	}
}

// sweepLeftoverFixtures 兜底巡检：清理历史崩溃可能残留的夹具医生（name='itest-fixture'）
// 及其时段行、计划行、关联行，并打印各步清理数量。只匹配夹具专用名称，不触碰真实业务数据。
// 仅在 SCHEDULE_IT_SWEEP_FIXTURES=1 时由 newScheduleDoctorStatusIT 调用。
func (it *scheduleDoctorStatusIT) sweepLeftoverFixtures(t *testing.T, ctx context.Context) {
	t.Helper()
	steps := []struct {
		label string
		query string
	}{
		{"时段行", `DELETE FROM hospital.doctor_work_plan_schedule WHERE work_plan_id IN (
SELECT id FROM hospital.doctor_work_plan WHERE doctor_id IN (SELECT id FROM hospital.doctor WHERE name = $1))`},
		{"计划行", `DELETE FROM hospital.doctor_work_plan WHERE doctor_id IN (SELECT id FROM hospital.doctor WHERE name = $1)`},
		{"关联行", `DELETE FROM hospital.medical_dept_sub_and_doctor WHERE doctor_id IN (SELECT id FROM hospital.doctor WHERE name = $1)`},
		{"夹具医生行", `DELETE FROM hospital.doctor WHERE name = $1`},
	}
	counts := make([]string, 0, len(steps))
	for _, step := range steps {
		result, err := it.db.ExecContext(ctx, step.query, scheduleDoctorStatusFixtureName)
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
	t.Logf("启动兜底巡检（name=%s）：%s", scheduleDoctorStatusFixtureName, strings.Join(counts, "、"))
}

// scheduleDoctorStatusDate 返回业务时区（Asia/Shanghai）当天偏移 offset 天后的 YYYY-MM-DD 日期。
// 排班日期必须晚于业务当日，否则 CreateSlot 会先命中 ErrSlotLocked，掩盖医生资格判定分支。
func scheduleDoctorStatusDate(offset int) string {
	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	return time.Now().In(shanghai).AddDate(0, 0, offset).Format("2006-01-02")
}

// scheduleDoctorStatusHighID 生成一个避开现有数据范围的高位随机主键，供自洽夹具行使用。
func scheduleDoctorStatusHighID() int64 {
	return int64(2000000000) + int64(uuid.New().ID()%100000000)
}

// insertPlan 直接以 SQL 插入一条排班计划（id 取序列 nextval、num=0）并登记待清理主键。
func (it *scheduleDoctorStatusIT) insertPlan(t *testing.T, ctx context.Context, doctorID, subdepartmentID int64, date string, maximum int16) int64 {
	t.Helper()
	var planID int64
	err := it.db.QueryRowContext(ctx, `INSERT INTO hospital.doctor_work_plan(id,doctor_id,dept_sub_id,date,maximum,num)
VALUES (nextval('hospital.doctor_work_plan_sequence'),$1,$2,$3::date,$4,0) RETURNING id`,
		doctorID, subdepartmentID, date, maximum).Scan(&planID)
	if err != nil {
		t.Fatalf("插入测试排班计划失败：%v", err)
	}
	it.planIDs = append(it.planIDs, planID)
	return planID
}

// insertSlot 直接以 SQL 插入一条时段行（不走 CreateSlot），用于构造“时段已存在”的夹具场景。
func (it *scheduleDoctorStatusIT) insertSlot(t *testing.T, ctx context.Context, planID int64, slot, maximum int16) int64 {
	t.Helper()
	var slotID int64
	err := it.db.QueryRowContext(ctx, `INSERT INTO hospital.doctor_work_plan_schedule(id, work_plan_id, slot, maximum, num)
VALUES (nextval('hospital.doctor_work_plan_schedule_sequence'),$1,$2,$3,0) RETURNING id`,
		planID, slot, maximum).Scan(&slotID)
	if err != nil {
		t.Fatalf("插入测试时段失败：%v", err)
	}
	return slotID
}

// findDoctorIDByStatus 取一名指定 doctor.status 的医生主键；查不到时返回 false。
func (it *scheduleDoctorStatusIT) findDoctorIDByStatus(t *testing.T, ctx context.Context, status int16) (int64, bool) {
	t.Helper()
	var doctorID int64
	err := it.db.QueryRowContext(ctx, `SELECT id FROM hospital.doctor WHERE status = $1 ORDER BY id LIMIT 1`, status).Scan(&doctorID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false
		}
		t.Fatalf("查询医生失败：%v", err)
	}
	return doctorID, true
}

// insertDoctorWithStatus 插入一条指定 status 的临时医生作为夹具，并登记待清理主键。
// 开发库当前医生全部为 ACTIVE：负例若依赖真实数据无法覆盖缺陷，正例复用真实医生又会在
// 共享库上与真实排班撞键（doctor_work_plan 无唯一约束），因此统一使用自洽夹具：
// 主键取避开现有数据的高位随机值，用例结束按主键删除。
func (it *scheduleDoctorStatusIT) insertDoctorWithStatus(t *testing.T, ctx context.Context, status int16) int64 {
	t.Helper()
	for attempt := 0; attempt < 5; attempt++ {
		doctorID := scheduleDoctorStatusHighID()
		var exists bool
		if err := it.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hospital.doctor WHERE id = $1)`, doctorID).Scan(&exists); err != nil {
			t.Fatalf("检查临时医生主键是否冲突失败：%v", err)
		}
		if exists {
			continue
		}
		if _, err := it.db.ExecContext(ctx, `INSERT INTO hospital.doctor(id,name,status) VALUES ($1,$2,$3)`,
			doctorID, scheduleDoctorStatusFixtureName, status); err != nil {
			t.Fatalf("插入临时夹具医生失败：%v", err)
		}
		it.doctorIDs = append(it.doctorIDs, doctorID)
		return doctorID
	}
	t.Fatal("连续多次生成的临时医生主键均与现有数据冲突")
	return 0
}

// insertDoctorSubdepartmentLink 为夹具医生插入一条 medical_dept_sub_and_doctor 关联行
// （ListDoctors 的关联守卫 EXISTS 依赖该表），并登记待清理主键。
func (it *scheduleDoctorStatusIT) insertDoctorSubdepartmentLink(t *testing.T, ctx context.Context, doctorID, subdepartmentID int64) int64 {
	t.Helper()
	for attempt := 0; attempt < 5; attempt++ {
		linkID := scheduleDoctorStatusHighID()
		var exists bool
		if err := it.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hospital.medical_dept_sub_and_doctor WHERE id = $1)`, linkID).Scan(&exists); err != nil {
			t.Fatalf("检查夹具关联主键是否冲突失败：%v", err)
		}
		if exists {
			continue
		}
		if _, err := it.db.ExecContext(ctx, `INSERT INTO hospital.medical_dept_sub_and_doctor(id,dept_sub_id,doctor_id) VALUES ($1,$2,$3)`,
			linkID, subdepartmentID, doctorID); err != nil {
			t.Fatalf("插入夹具医生关联失败：%v", err)
		}
		it.linkIDs = append(it.linkIDs, linkID)
		return linkID
	}
	t.Fatal("连续多次生成的夹具关联主键均与现有数据冲突")
	return 0
}

// missingDoctorID 返回一个已确认不存在于 hospital.doctor 的高位随机主键，用于“医生行缺失”边界场景。
func (it *scheduleDoctorStatusIT) missingDoctorID(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	for attempt := 0; attempt < 5; attempt++ {
		doctorID := scheduleDoctorStatusHighID()
		var exists bool
		if err := it.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hospital.doctor WHERE id = $1)`, doctorID).Scan(&exists); err != nil {
			t.Fatalf("检查医生主键是否存在失败：%v", err)
		}
		if !exists {
			return doctorID
		}
	}
	t.Fatal("连续多次生成的医生主键均与现有数据冲突")
	return 0
}

// inactiveDoctorID 返回一名非在诊（status<>1）医生主键：优先复用库中真实存在的同状态医生，
// 库中没有时插入临时夹具医生（用例结束删除），保证负例始终被真实执行。
func (it *scheduleDoctorStatusIT) inactiveDoctorID(t *testing.T, ctx context.Context, status int16) int64 {
	t.Helper()
	if doctorID, ok := it.findDoctorIDByStatus(t, ctx, status); ok {
		return doctorID
	}
	doctorID := it.insertDoctorWithStatus(t, ctx, status)
	t.Logf("库中无 status=%d 医生，改用临时夹具医生 id=%d（用例结束会删除）", status, doctorID)
	return doctorID
}

// anySubdepartmentID 取任一存在的子科室主键，保证计划的 dept_sub_id 是真实值。
func (it *scheduleDoctorStatusIT) anySubdepartmentID(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var subdepartmentID int64
	if err := it.db.QueryRowContext(ctx, `SELECT id FROM hospital.medical_dept_sub ORDER BY id LIMIT 1`).Scan(&subdepartmentID); err != nil {
		t.Skipf("库中无可用子科室，跳过：%v", err)
	}
	return subdepartmentID
}

// slotUsedFromDB 直接查询时段行的 num 字段（即 used），用于核对数据库事实而非仅依赖返回值。
func (it *scheduleDoctorStatusIT) slotUsedFromDB(t *testing.T, ctx context.Context, slotID int64) int16 {
	t.Helper()
	var used int16
	if err := it.db.QueryRowContext(ctx, "SELECT num FROM hospital.doctor_work_plan_schedule WHERE id = $1", slotID).Scan(&used); err != nil {
		t.Fatalf("查询时段 num 失败：%v", err)
	}
	return used
}

// countPlanSlots 统计某计划下的时段行数，用于复核失败路径没有落库。
func (it *scheduleDoctorStatusIT) countPlanSlots(t *testing.T, ctx context.Context, planID int64) int {
	t.Helper()
	var count int
	if err := it.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hospital.doctor_work_plan_schedule WHERE work_plan_id = $1`, planID).Scan(&count); err != nil {
		t.Fatalf("统计时段行数失败：%v", err)
	}
	return count
}

// TestScheduleRepositoryCreateSlotRejectsInactiveDoctor 负例：计划所属医生为离职/退休/隐藏
// （status<>1）时，CreateSlot 必须返回 schedule.ErrDoctorInactive、不返回时段对象，
// 且数据库中不得新增任何 doctor_work_plan_schedule 行（失败路径整体回滚）。
func TestScheduleRepositoryCreateSlotRejectsInactiveDoctor(t *testing.T) {
	it, ctx := newScheduleDoctorStatusIT(t)
	subdepartmentID := it.anySubdepartmentID(t, ctx)
	cases := []struct {
		name   string
		status int16
	}{
		{"离职 RESIGNED(2)", 2},
		{"退休 RETIRED(3)", 3},
		{"隐藏 HIDDEN(4)", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doctorID := it.inactiveDoctorID(t, ctx, tc.status)
			planID := it.insertPlan(t, ctx, doctorID, subdepartmentID, scheduleDoctorStatusDate(7), 5)

			result, err := it.slots.CreateSlot(ctx, schedule.ScheduleSlot{WorkPlanID: planID, Slot: 1, Maximum: 3}, it.now)
			if !errors.Is(err, schedule.ErrDoctorInactive) {
				t.Fatalf("CreateSlot 错误 = %v，期望 %v", err, schedule.ErrDoctorInactive)
			}
			if result != nil {
				t.Fatalf("医生非在诊时不得返回时段对象：%+v", result)
			}
			if count := it.countPlanSlots(t, ctx, planID); count != 0 {
				t.Fatalf("医生非在诊时计划 %d 下时段行数 = %d，期望 0", planID, count)
			}
		})
	}
}

// TestScheduleRepositoryCreateSlotRejectsMissingDoctor 边界：计划的 doctor_id 指向不存在的医生
// （先确认该主键在 hospital.doctor 中不存在）时，资格判定必须仍然失败并返回
// schedule.ErrDoctorInactive，而不是因医生行缺失误判为“无异常”放行落库。
func TestScheduleRepositoryCreateSlotRejectsMissingDoctor(t *testing.T) {
	it, ctx := newScheduleDoctorStatusIT(t)
	doctorID := it.missingDoctorID(t, ctx)
	planID := it.insertPlan(t, ctx, doctorID, it.anySubdepartmentID(t, ctx), scheduleDoctorStatusDate(7), 5)

	result, err := it.slots.CreateSlot(ctx, schedule.ScheduleSlot{WorkPlanID: planID, Slot: 1, Maximum: 3}, it.now)
	if !errors.Is(err, schedule.ErrDoctorInactive) {
		t.Fatalf("医生行缺失时 CreateSlot 错误 = %v，期望 %v", err, schedule.ErrDoctorInactive)
	}
	if result != nil {
		t.Fatalf("医生行缺失时不得返回时段对象：%+v", result)
	}
	if count := it.countPlanSlots(t, ctx, planID); count != 0 {
		t.Fatalf("医生行缺失时计划 %d 下时段行数 = %d，期望 0", planID, count)
	}
}

// TestScheduleRepositoryCreateSlotValidationOrder 校验 CreateSlot 的校验顺序与实现说明一致：
// 1) 已开始计划的日期锁优先于医生资格（返回 ErrSlotLocked 而非 ErrDoctorInactive）；
// 2) 医生资格优先于时段重复（同为未来计划且该 slot 已存在时返回 ErrDoctorInactive 而非 ErrSlotExists）。
func TestScheduleRepositoryCreateSlotValidationOrder(t *testing.T) {
	it, ctx := newScheduleDoctorStatusIT(t)
	subdepartmentID := it.anySubdepartmentID(t, ctx)
	doctorID := it.inactiveDoctorID(t, ctx, 2)

	t.Run("日期锁优先于医生资格", func(t *testing.T) {
		// 计划日期取业务当日：CanModify 为 false，应先返回 ErrSlotLocked。
		planID := it.insertPlan(t, ctx, doctorID, subdepartmentID, scheduleDoctorStatusDate(0), 5)
		_, err := it.slots.CreateSlot(ctx, schedule.ScheduleSlot{WorkPlanID: planID, Slot: 1, Maximum: 3}, it.now)
		if !errors.Is(err, schedule.ErrSlotLocked) {
			t.Fatalf("CreateSlot 错误 = %v，期望 %v", err, schedule.ErrSlotLocked)
		}
	})

	t.Run("医生资格优先于时段重复", func(t *testing.T) {
		planID := it.insertPlan(t, ctx, doctorID, subdepartmentID, scheduleDoctorStatusDate(7), 5)
		it.insertSlot(t, ctx, planID, 1, 3)
		_, err := it.slots.CreateSlot(ctx, schedule.ScheduleSlot{WorkPlanID: planID, Slot: 1, Maximum: 3}, it.now)
		if !errors.Is(err, schedule.ErrDoctorInactive) {
			t.Fatalf("CreateSlot 错误 = %v，期望 %v", err, schedule.ErrDoctorInactive)
		}
	})
}

// TestScheduleRepositoryCreateSlotAllowsActiveDoctor 正例：医生 status=1 时 CreateSlot 成功，
// 返回的 remaining 等于 maximum-used（新建时段 used=0），并确实落库一行时段。
// 医生统一使用临时夹具（status=1），避免复用真实 ACTIVE 医生时与真实排班撞键
// （doctor_work_plan 无唯一约束，会在共享库上临时产生重复计划行）。
func TestScheduleRepositoryCreateSlotAllowsActiveDoctor(t *testing.T) {
	it, ctx := newScheduleDoctorStatusIT(t)
	doctorID := it.insertDoctorWithStatus(t, ctx, 1)
	planID := it.insertPlan(t, ctx, doctorID, it.anySubdepartmentID(t, ctx), scheduleDoctorStatusDate(7), 5)

	created, err := it.slots.CreateSlot(ctx, schedule.ScheduleSlot{WorkPlanID: planID, Slot: 1, Maximum: 3}, it.now)
	if err != nil {
		t.Fatalf("在诊医生创建时段意外失败：%v", err)
	}
	if created == nil {
		t.Fatal("创建成功时应返回时段对象")
	}
	if created.WorkPlanID != planID || created.Slot != 1 || created.Maximum != 3 {
		t.Fatalf("返回时段字段异常：%+v", created)
	}
	if created.Remaining != 3 {
		t.Fatalf("新建时段 remaining = %d，期望 3（maximum=3、used=0）", created.Remaining)
	}
	// 核对 SQL 事实：落库行的 num（used）必须为 0，而不是仅依赖返回值自洽。
	if used := it.slotUsedFromDB(t, ctx, created.ID); used != 0 {
		t.Fatalf("时段 id=%d 落库 num = %d，期望 0", created.ID, used)
	}
	if count := it.countPlanSlots(t, ctx, planID); count != 1 {
		t.Fatalf("计划 %d 下时段行数 = %d，期望 1", planID, count)
	}
}

// TestScheduleRepositoryListDoctorsByStatusFilter 校验“查询医生列表能展示不同状态医生”：
// ListDoctors 按 status 过滤时只返回该状态医生；库中已有样本时直接使用真实数据；
// 库中确实没有该状态样本时用例自建夹具（临时医生 + medical_dept_sub_and_doctor 关联，
// 使列表的关联守卫 EXISTS 能命中），断言结果包含该夹具医生，用例结束精确清理。
func TestScheduleRepositoryListDoctorsByStatusFilter(t *testing.T) {
	it, ctx := newScheduleDoctorStatusIT(t)
	subdepartmentID := it.anySubdepartmentID(t, ctx)
	cases := []struct {
		name       string
		status     string
		statusCode int16
	}{
		{"在诊 ACTIVE", catalog.DoctorStatusActive, 1},
		{"离职 RESIGNED", catalog.DoctorStatusResigned, 2},
		{"退休 RETIRED", catalog.DoctorStatusRetired, 3},
		{"隐藏 HIDDEN", catalog.DoctorStatusHidden, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, total, err := it.doctors.ListDoctors(ctx, catalog.DoctorFilter{Status: tc.status}, 0, 20)
			if err != nil {
				t.Fatalf("ListDoctors(status=%s) 失败：%v", tc.status, err)
			}
			// 无论样本来自真实数据还是夹具，返回条目状态都必须等于过滤值。
			assertListDoctorsStatus(t, items, tc.status)
			if total > 0 && len(items) > 0 {
				t.Logf("库中已有 status=%s 样本 %d 条，直接使用真实数据", tc.status, total)
				return
			}

			// 库中无该状态样本：自建夹具医生与关联行，验证过滤与关联守卫都能命中。
			doctorID := it.insertDoctorWithStatus(t, ctx, tc.statusCode)
			it.insertDoctorSubdepartmentLink(t, ctx, doctorID, subdepartmentID)

			items, total, err = it.doctors.ListDoctors(ctx, catalog.DoctorFilter{Status: tc.status}, 0, 20)
			if err != nil {
				t.Fatalf("ListDoctors(status=%s) 夹具查询失败：%v", tc.status, err)
			}
			if total == 0 || len(items) == 0 {
				t.Fatalf("夹具医生(id=%d,status=%d)应能被 status=%s 列表查到，实际 total=%d len=%d",
					doctorID, tc.statusCode, tc.status, total, len(items))
			}
			found := false
			for _, item := range items {
				if item.ID == doctorID {
					found = true
				}
			}
			if !found {
				t.Fatalf("ListDoctors(status=%s) 结果未包含夹具医生 id=%d：%+v", tc.status, doctorID, items)
			}
			assertListDoctorsStatus(t, items, tc.status)
			t.Logf("库中无 status=%s 样本，已用夹具医生 id=%d 验证（用例结束删除）", tc.status, doctorID)
		})
	}
}

// assertListDoctorsStatus 断言列表条目状态全部等于过滤值，防止过滤器被忽略而返回全部医生。
func assertListDoctorsStatus(t *testing.T, items []catalog.DoctorCatalog, status string) {
	t.Helper()
	for _, item := range items {
		if item.Status != status {
			t.Fatalf("status=%s 过滤返回了 %q 状态医生：%+v", status, item.Status, item)
		}
	}
}
