// 本文件是订单过期收口（契约 §6.8、创建订单与支付业务说明.md 第 4、7 节）相关 SQL 片段的
// 文本语义测试（不连数据库）：扫描语句、收口状态迁移语句与关单前置查询的固定口径必须显式
// 落在 SQL 文本里，避免「实现改了、契约要素悄悄丢失」。断言方式与 postgres_registration_test.go
// 对 release*Query 的做法一致：对包级常量与函数体源码做片段包含判断，不引入 sqlmock。
package repo

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestExpiredUnpaidQueryShape 覆盖扫描语句的固定口径：只取「未付款 + 已过 expire_at（数据库时钟）+
// out_trade_no 非空」的订单，并按 expire_at 升序取批量上限。
// 缺 payment_status 会把已支付订单扫进收口；缺 now() 或改用应用时钟会误收未过期订单；
// 缺「out_trade_no 非空」过滤会让空交易号的历史脏数据进入按交易号定位的释放路径。
func TestExpiredUnpaidQueryShape(t *testing.T) {
	for _, fragment := range []string{
		"payment_status = $1",
		"expire_at IS NOT NULL",
		"expire_at <= now()",
		"btrim(r.out_trade_no) <> ''",
		"ORDER BY r.expire_at, r.id",
		"LIMIT $2",
	} {
		if !strings.Contains(expiredUnpaidQuery, fragment) {
			t.Errorf("expiredUnpaidQuery 缺少片段 %q：\n%s", fragment, expiredUnpaidQuery)
		}
	}
}

// TestExpiredUnpaidAfterQueryUsesStableKeyset 覆盖金额异常订单不会永久阻塞队首：
// 后续扫描必须按 expire_at、id 的稳定游标继续推进，不能反复读取同一批 LIMIT 结果。
func TestExpiredUnpaidAfterQueryUsesStableKeyset(t *testing.T) {
	for _, fragment := range []string{
		"r.expire_at > $3",
		"r.expire_at = $3 AND r.id > $4",
		"ORDER BY r.expire_at, r.id",
	} {
		if !strings.Contains(expiredUnpaidAfterQuery, fragment) {
			t.Errorf("expiredUnpaidAfterQuery 缺少稳定游标片段 %q：\n%s", fragment, expiredUnpaidAfterQuery)
		}
	}
}

// TestExpireUnpaidQueryShape 覆盖收口状态迁移语句：必须是参数化的条件更新，
// $2 是目标状态（EXPIRED）、$3 是前置状态（未付款），条件命中是号源释放的唯一凭据。
func TestExpireUnpaidQueryShape(t *testing.T) {
	for _, fragment := range []string{
		"UPDATE hospital.medical_registration",
		"SET payment_status = $2",
		"btrim(out_trade_no) = btrim($1)",
		"AND payment_status = $3",
	} {
		if !strings.Contains(expireUnpaidQuery, fragment) {
			t.Errorf("expireUnpaidQuery 缺少片段 %q：\n%s", fragment, expireUnpaidQuery)
		}
	}
}

// TestExpireTargetQueryLocksRegistrationRow 覆盖读关联编号的前置查询：必须锁住挂号行
// （FOR UPDATE），且与补偿路径同一加锁顺序（先锁挂号行，再按计划级 -> 时段级更新计数器），
// 否则收口与补偿并发时可能互相死锁。
func TestExpireTargetQueryLocksRegistrationRow(t *testing.T) {
	for _, fragment := range []string{
		"SELECT work_plan_id, doctor_schedule_id",
		"btrim(out_trade_no) = btrim($1)",
		"FOR UPDATE",
	} {
		if !strings.Contains(expireTargetQuery, fragment) {
			t.Errorf("expireTargetQuery 缺少片段 %q：\n%s", fragment, expireTargetQuery)
		}
	}
}

// readPostgresPaymentSource 读取本包生产文件 postgres_payment.go 的源码文本，
// 用于在函数体级别断言「收口复用了两条 release*Query 且在同一事务内提交」。
func readPostgresPaymentSource(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试文件路径")
	}
	path := filepath.Join(filepath.Dir(thisFile), "postgres_payment.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	return string(raw)
}

// TestExpireUnpaidReusesReleaseQuotaQueries 覆盖号源释放的实现复用：收口事务必须调用挂号域
// 的两条 release*Query（它们自带 num > 0 保护），并在同一事务内提交，
// 不允许另写一段「只减一级」或「不带上限保护」的 SQL。
func TestExpireUnpaidReusesReleaseQuotaQueries(t *testing.T) {
	for name, query := range map[string]string{
		"计划级": releasePlanQuotaQuery,
		"时段级": releaseSlotQuotaQuery,
	} {
		if !strings.Contains(query, "num = num - 1") {
			t.Errorf("%s释放语句必须自减 1：%s", name, query)
		}
		if !strings.Contains(query, "num > 0") {
			t.Errorf("%s释放语句必须带 num > 0 保护：%s", name, query)
		}
	}

	source := readPostgresPaymentSource(t)
	const signature = "func (r *PostgresPaymentRepository) ExpireUnpaid("
	start := strings.Index(source, signature)
	if start < 0 {
		t.Fatalf("postgres_payment.go 中找不到 ExpireUnpaid 实现")
	}
	body := source[start:]
	if end := strings.Index(body, "\nfunc "); end >= 0 {
		body = body[:end]
	}
	for _, fragment := range []string{
		"releasePlanQuotaQuery",
		"releaseSlotQuotaQuery",
		"tx.Commit()",
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("ExpireUnpaid 函数体缺少 %q（号源释放必须复用 release*Query 并在同一事务提交）：\n%s",
				fragment, body)
		}
	}
}

// TestExpireUnpaidRejectsMissingScheduleLink 覆盖收口事务的不变式：work_plan_id 与
// doctor_schedule_id 任一缺失时必须返回错误并整笔回滚（源码级断言：不得退化为只减一级）。
func TestExpireUnpaidRejectsMissingScheduleLink(t *testing.T) {
	source := readPostgresPaymentSource(t)
	for _, fragment := range []string{
		"!workPlanID.Valid",
		"!scheduleID.Valid",
		"收口订单缺少排班关联，拒绝只减一级号源",
	} {
		if !strings.Contains(source, fragment) {
			t.Errorf("postgres_payment.go 缺少排班关联缺失保护片段 %q", fragment)
		}
	}
	// 空交易号必须在开启事务前被拒绝，避免一次命中所有空交易号的历史脏数据行。
	if !strings.Contains(source, "strings.TrimSpace(outTradeNo) == \"\"") {
		t.Errorf("ExpireUnpaid 必须防御空 out_trade_no")
	}
}
