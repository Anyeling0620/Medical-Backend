// 本文件是订单过期收口（PostgresPaymentRepository.ExpireUnpaid / ListExpiredUnpaid）的
// 真实 PostgreSQL 集成测试（契约 §6.8、创建订单与支付业务说明.md 第 4、7 节）：覆盖
// 「收口事务置 EXPIRED 并在同一事务释放两级 num」「重复调用不重复释放」
// 「已被通知路径抢先置 PAID 后不再释放」「收口事务与 MarkPaid 并发只有一方成功」
// 「扫描只返回已过期且 UNPAID 的订单」「释放后同一时段可重新挂号」，
// 「金额异常订单不释放且扫描游标可继续推进」「缺少排班关联时返回错误并整笔回滚」，
// 每个用例都读库核对状态与两级计数器，不只断言方法返回值。
//
// 运行方式（工作树根目录，cmd，注意 set 的引号避免尾随空格）：
//
//	set "PGSQL_INTEGRATION_TEST=1" && go test ./internal/repo/ -run TestPostgresPaymentExpiry -count=1 -v
//
// 未设置开关或连不上数据库时一律 t.Skip，不计为失败。
//
// 夹具安全：本文件不新建夹具装配，而是复用 postgres_registration_integration_test.go 的
// registrationIT（同一份 itest-reg / ITREG 标记、同一份按主键清理与残留复核）创建医生、价目、
// 计划、时段、患者账号、就诊卡与挂号行；所有写入都带主键条件，只动本用例创建的行。
package repo

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	domainpayment "Medical-Web-Backend/internal/domain/payment"
	"Medical-Web-Backend/internal/domain/registration"
	"Medical-Web-Backend/internal/port"
	paymentservice "Medical-Web-Backend/internal/usecase/payment"
)

// paymentExpiryTradeNo 是并发用例写入的支付宝交易号；transaction_id 是 CHAR(32)，读取时 btrim。
const paymentExpiryTradeNo = "2026091322001234567890"

// paymentExpiryIT 在挂号集成测试夹具之上补充被测支付仓库：收口用例既要建真实挂号单，
// 又要直接调用收口与扫描实现，两者必须共用同一条数据库连接与同一份清理逻辑。
type paymentExpiryIT struct {
	*registrationIT
	// payments 是被测的支付仓储（收口事务与过期扫描实现）。
	payments *PostgresPaymentRepository
}

// newPaymentExpiryIT 组装真实 PostgreSQL 连接与支付仓库；未开启集成测试开关或连不上库时跳过。
func newPaymentExpiryIT(t *testing.T) (*paymentExpiryIT, context.Context) {
	t.Helper()
	it, ctx := newRegistrationIT(t)
	return &paymentExpiryIT{registrationIT: it, payments: NewPostgresPaymentRepository(it.db)}, ctx
}

// paymentExpiryFixture 是一次收口用例创建的夹具编号与建单结果。
type paymentExpiryFixture struct {
	planID  int64
	slotID  int64
	cardID  int64
	pid     string
	created *registration.Registration
}

// newFixture 创建「医生 + 价目 + 计划 + 时段 + 患者账号 + 就诊卡」并建一条 UNPAID 挂号单：
// 计划级与时段级 maximum 都取 5（建单后 num 各为 1），保证释放后仍有号可再挂；
// 排班日期取未来第 3 天，避免建单先命中 ErrScheduleStarted 而不是被测分支。
func (it *paymentExpiryIT) newFixture(t *testing.T, ctx context.Context) paymentExpiryFixture {
	t.Helper()
	doctorID := it.insertDoctor(t, ctx)
	it.insertDoctorPrice(t, ctx, doctorID, "80.00", "80.00")
	planID := it.insertPlan(t, ctx, doctorID, it.deptSubID(t, ctx), registrationITFutureDate(3), 5)
	slotID := it.insertSlot(t, ctx, planID, 1, 5)
	patientID := it.insertPatientUser(t, ctx)
	pid := registrationITNewPID()
	cardID := it.insertCard(t, ctx, patientID, pid)
	created, err := it.createRegistration(ctx, cardID, pid, slotID)
	if err != nil {
		t.Fatalf("夹具建单失败：%v", err)
	}
	return paymentExpiryFixture{planID: planID, slotID: slotID, cardID: cardID, pid: pid, created: created}
}

// forceExpireAt 只按主键把夹具订单的 expire_at 前移到 1 分钟前，模拟已越过 35 分钟收口边界；
// 用数据库 now() 计算，保证与收口扫描的过期判定共用同一时钟。
// 严禁去掉 id 条件：共享库上只允许按本用例创建的主键写入。
func (it *paymentExpiryIT) forceExpireAt(t *testing.T, ctx context.Context, registrationID int64) {
	t.Helper()
	if _, err := it.db.ExecContext(ctx,
		`UPDATE hospital.medical_registration SET expire_at = now() - interval '1 minute' WHERE id = $1`,
		registrationID); err != nil {
		t.Fatalf("前移夹具订单 expire_at 失败（id=%d）：%v", registrationID, err)
	}
}

// paymentCode 读库返回挂号行的 payment_status 编码：收口结果必须读库核对，不能只看返回值。
func (it *paymentExpiryIT) paymentCode(t *testing.T, ctx context.Context, registrationID int64) int16 {
	t.Helper()
	var code int16
	if err := it.db.QueryRowContext(ctx,
		`SELECT payment_status FROM hospital.medical_registration WHERE id = $1`,
		registrationID).Scan(&code); err != nil {
		t.Fatalf("查询夹具挂号 payment_status 失败（id=%d）：%v", registrationID, err)
	}
	return code
}

// paymentTransactionID 读库返回挂号行的支付宝交易号（CHAR(32)，btrim 去补位空格），
// 用于证明收口不会清掉通知路径已写入的支付证据。
func (it *paymentExpiryIT) paymentTransactionID(t *testing.T, ctx context.Context, registrationID int64) string {
	t.Helper()
	var transactionID string
	if err := it.db.QueryRowContext(ctx,
		`SELECT btrim(transaction_id) FROM hospital.medical_registration WHERE id = $1`,
		registrationID).Scan(&transactionID); err != nil {
		t.Fatalf("查询夹具挂号 transaction_id 失败（id=%d）：%v", registrationID, err)
	}
	return transactionID
}

// requirePaymentExpiryQuota 读库核对两级计数器，任何一方不符都给出实际值，证明释放确实落库。
func requirePaymentExpiryQuota(
	t *testing.T,
	it *paymentExpiryIT,
	ctx context.Context,
	fixture paymentExpiryFixture,
	wantPlan int16,
	wantSlot int16,
) {
	t.Helper()
	if got := it.planUsed(t, ctx, fixture.planID); got != wantPlan {
		t.Fatalf("doctor_work_plan.num = %d，期望 %d", got, wantPlan)
	}
	if got := it.slotUsed(t, ctx, fixture.slotID); got != wantSlot {
		t.Fatalf("doctor_work_plan_schedule.num = %d，期望 %d", got, wantSlot)
	}
}

// TestPostgresPaymentExpiryReleasesQuota 覆盖收口主路径：UNPAID -> EXPIRED（4）必须同时把
// 计划级与时段级 num 各减 1，两次减法与状态变更在同一事务内（契约 §6.8、业务说明第 4 节）。
func TestPostgresPaymentExpiryReleasesQuota(t *testing.T) {
	it, ctx := newPaymentExpiryIT(t)
	fixture := it.newFixture(t, ctx)
	// 建单成功后先确认两级 num 各为 1，释放断言才有参照。
	requirePaymentExpiryQuota(t, it, ctx, fixture, 1, 1)

	it.forceExpireAt(t, ctx, fixture.created.ID)
	expired, err := it.payments.ExpireUnpaid(ctx, fixture.created.OutTradeNo)
	if err != nil {
		t.Fatalf("收口事务执行失败：%v", err)
	}
	if !expired {
		t.Fatal("首次收口必须命中条件更新并返回 true，实际 false")
	}
	if got := it.paymentCode(t, ctx, fixture.created.ID); got != registration.PaymentCodeExpired {
		t.Fatalf("落库 payment_status = %d，期望 %d（EXPIRED）", got, registration.PaymentCodeExpired)
	}
	requirePaymentExpiryQuota(t, it, ctx, fixture, 0, 0)
}

// TestPostgresPaymentExpiryIsIdempotent 覆盖重复扫描：第二次 ExpireUnpaid 因条件更新未命中
// 返回 false，两级 num 不再变化，证明「条件更新成功是释放的唯一凭据」且计数器不会变负。
func TestPostgresPaymentExpiryIsIdempotent(t *testing.T) {
	it, ctx := newPaymentExpiryIT(t)
	fixture := it.newFixture(t, ctx)
	it.forceExpireAt(t, ctx, fixture.created.ID)

	first, err := it.payments.ExpireUnpaid(ctx, fixture.created.OutTradeNo)
	if err != nil {
		t.Fatalf("首次收口执行失败：%v", err)
	}
	if !first {
		t.Fatal("首次收口必须返回 true，实际 false")
	}
	requirePaymentExpiryQuota(t, it, ctx, fixture, 0, 0)

	second, err := it.payments.ExpireUnpaid(ctx, fixture.created.OutTradeNo)
	if err != nil {
		t.Fatalf("重复收口不应报错：%v", err)
	}
	if second {
		t.Fatal("重复收口必须返回 false，实际 true")
	}
	requirePaymentExpiryQuota(t, it, ctx, fixture, 0, 0)
	if got := it.paymentCode(t, ctx, fixture.created.ID); got != registration.PaymentCodeExpired {
		t.Fatalf("落库 payment_status = %d，期望 %d（EXPIRED）", got, registration.PaymentCodeExpired)
	}
}

// TestPostgresPaymentExpiryKeepsPaidOrder 覆盖通知路径抢先：订单已被条件更新为 PAID 后再收口
// 必须返回 false，状态保持 PAID、交易号保留、两级 num 都不释放（可支付时不误置 EXPIRED）。
func TestPostgresPaymentExpiryKeepsPaidOrder(t *testing.T) {
	it, ctx := newPaymentExpiryIT(t)
	fixture := it.newFixture(t, ctx)
	it.forceExpireAt(t, ctx, fixture.created.ID)

	paid, err := it.payments.MarkPaid(ctx, fixture.created.OutTradeNo, paymentExpiryTradeNo)
	if err != nil {
		t.Fatalf("通知路径迁移状态失败：%v", err)
	}
	if !paid {
		t.Fatal("UNPAID -> PAID 条件更新必须命中，实际 false")
	}

	expired, err := it.payments.ExpireUnpaid(ctx, fixture.created.OutTradeNo)
	if err != nil {
		t.Fatalf("已支付订单收口不应报错：%v", err)
	}
	if expired {
		t.Fatal("已支付订单不得被收口置为 EXPIRED，实际返回 true")
	}
	if got := it.paymentCode(t, ctx, fixture.created.ID); got != registration.PaymentCodePaid {
		t.Fatalf("落库 payment_status = %d，期望 %d（PAID）", got, registration.PaymentCodePaid)
	}
	if got := it.paymentTransactionID(t, ctx, fixture.created.ID); got != paymentExpiryTradeNo {
		t.Fatalf("transaction_id = %q，期望 %q", got, paymentExpiryTradeNo)
	}
	requirePaymentExpiryQuota(t, it, ctx, fixture, 1, 1)
}

// TestPostgresPaymentExpiryConcurrentWithMarkPaid 覆盖收口任务与通知路径并发：两个操作都带
// payment_status = UNPAID 前置条件，同一订单只可能有一方成功；成功方的语义必须与落库状态、
// 两级 num 自洽（PAID 不释放、EXPIRED 释放）。
func TestPostgresPaymentExpiryConcurrentWithMarkPaid(t *testing.T) {
	it, ctx := newPaymentExpiryIT(t)
	fixture := it.newFixture(t, ctx)
	it.forceExpireAt(t, ctx, fixture.created.ID)

	var (
		wg                    sync.WaitGroup
		markedWon, expiredWon bool
		markErr, expireErr    error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		markedWon, markErr = it.payments.MarkPaid(ctx, fixture.created.OutTradeNo, paymentExpiryTradeNo)
	}()
	go func() {
		defer wg.Done()
		expiredWon, expireErr = it.payments.ExpireUnpaid(ctx, fixture.created.OutTradeNo)
	}()
	wg.Wait()

	if markErr != nil {
		t.Fatalf("通知路径迁移状态失败：%v", markErr)
	}
	if expireErr != nil {
		t.Fatalf("收口事务执行失败：%v", expireErr)
	}
	if markedWon == expiredWon {
		t.Fatalf("同一订单并发下必须恰好一方成功，实际 MarkPaid=%t ExpireUnpaid=%t", markedWon, expiredWon)
	}

	code := it.paymentCode(t, ctx, fixture.created.ID)
	t.Logf("并发结果：MarkPaid=%t ExpireUnpaid=%t 落库 payment_status=%d", markedWon, expiredWon, code)
	switch code {
	case registration.PaymentCodePaid:
		if !markedWon {
			t.Fatalf("落库为 PAID 时必须由 MarkPaid 成功，实际 MarkPaid=%t", markedWon)
		}
		requirePaymentExpiryQuota(t, it, ctx, fixture, 1, 1)
	case registration.PaymentCodeExpired:
		if !expiredWon {
			t.Fatalf("落库为 EXPIRED 时必须由 ExpireUnpaid 成功，实际 ExpireUnpaid=%t", expiredWon)
		}
		requirePaymentExpiryQuota(t, it, ctx, fixture, 0, 0)
	default:
		t.Fatalf("落库 payment_status = %d，期望 PAID(2) 或 EXPIRED(4)", code)
	}
}

// TestPostgresPaymentExpiryListReturnsOnlyExpiredUnpaid 覆盖扫描条件：只返回「expire_at <= now()
// （数据库时钟）且 payment_status = UNPAID」的订单，未过期的与已支付的都不返回
// （契约 §6.8、业务说明第 7 节扫描条件）。
func TestPostgresPaymentExpiryListReturnsOnlyExpiredUnpaid(t *testing.T) {
	it, ctx := newPaymentExpiryIT(t)
	expiredUnpaid := it.newFixture(t, ctx) // 已过期且未付款：应被扫到
	futureUnpaid := it.newFixture(t, ctx)  // 未过期未付款：不应被扫到
	expiredPaid := it.newFixture(t, ctx)   // 已过期但已付款：不应被扫到
	it.forceExpireAt(t, ctx, expiredUnpaid.created.ID)
	it.forceExpireAt(t, ctx, expiredPaid.created.ID)
	it.setPaymentStatus(t, ctx, expiredPaid.created.ID,
		registration.PaymentCodePaid, registration.BusinessDate(it.now))

	items, err := it.payments.ListExpiredUnpaid(ctx, 1000, time.Time{}, 0)
	if err != nil {
		t.Fatalf("扫描过期未付款订单失败：%v", err)
	}

	byOutTradeNo := make(map[string]domainpayment.Payment, len(items))
	for _, item := range items {
		byOutTradeNo[item.OutTradeNo] = item
		// 扫描条件不变式：返回集里不允许出现非 UNPAID 的订单，否则收口会误释放号源。
		if item.PaymentStatus != domainpayment.PaymentStatusUnpaid {
			t.Fatalf("扫描返回了非 UNPAID 的订单：out_trade_no=%s paymentStatus=%s",
				item.OutTradeNo, item.PaymentStatus)
		}
	}
	if _, ok := byOutTradeNo[expiredUnpaid.created.OutTradeNo]; !ok {
		t.Fatalf("已过期未付款的订单未被扫到：out_trade_no=%s", expiredUnpaid.created.OutTradeNo)
	}
	if _, ok := byOutTradeNo[futureUnpaid.created.OutTradeNo]; ok {
		t.Fatalf("未过期的订单不应被扫到：out_trade_no=%s", futureUnpaid.created.OutTradeNo)
	}
	if _, ok := byOutTradeNo[expiredPaid.created.OutTradeNo]; ok {
		t.Fatalf("已付款的订单不应被扫到：out_trade_no=%s", expiredPaid.created.OutTradeNo)
	}
}

// TestPostgresPaymentExpiryAllowsReRegistration 覆盖号源确实被释放：收口前同一身份证号在同一
// 时段再挂号必须命中占用判重；收口置 EXPIRED 并释放后必须能重新建单，两级 num 回到 1
// （契约 §6.2 判重口径、业务说明第 4 节：EXPIRED 不占用，允许重新挂号）。
func TestPostgresPaymentExpiryAllowsReRegistration(t *testing.T) {
	it, ctx := newPaymentExpiryIT(t)
	fixture := it.newFixture(t, ctx)

	// 收口前 UNPAID 仍占号：同一 pid、同一时段再挂号必须被拒绝，证明后面的成功不是白名单放行。
	if _, err := it.createRegistration(ctx, fixture.cardID, fixture.pid, fixture.slotID); !errors.Is(err, registration.ErrDuplicate) {
		t.Fatalf("收口前重复挂号必须返回 ErrDuplicate，实际 err = %v", err)
	}

	it.forceExpireAt(t, ctx, fixture.created.ID)
	expired, err := it.payments.ExpireUnpaid(ctx, fixture.created.OutTradeNo)
	if err != nil {
		t.Fatalf("收口事务执行失败：%v", err)
	}
	if !expired {
		t.Fatal("首次收口必须返回 true，实际 false")
	}
	requirePaymentExpiryQuota(t, it, ctx, fixture, 0, 0)

	again, err := it.createRegistration(ctx, fixture.cardID, fixture.pid, fixture.slotID)
	if err != nil {
		t.Fatalf("号源释放后重新挂号必须成功，实际 %v", err)
	}
	if again.ID == fixture.created.ID {
		t.Fatalf("重新挂号必须生成新的挂号主键，实际复用 id=%d", again.ID)
	}
	requirePaymentExpiryQuota(t, it, ctx, fixture, 1, 1)
}

// ---- 金额异常保护：不迁移状态、不释放号源 ----

// expirySweepFakeGateway 是资金异常用例的假支付宝网关：按交易号返回「交易成功但金额不一致」
// （驱动 usecase 的 AmountMismatch 分支）或「交易不存在」（无支付证据，直接收口），
// 不访问网络、不写数据库，关单只记录调用。
//
// 金额异常订单保留 UNPAID 且不释放；扫描推进由 use case 的稳定 keyset 游标保证。
type expirySweepFakeGateway struct {
	mismatchTradeNos map[string]bool
	queryCalls       []string
	cancelCalls      []string
}

// 编译期断言假网关满足用例依赖的端口契约。
var _ port.AlipayGateway = (*expirySweepFakeGateway)(nil)

// Precreate 在收口用例里不应被调用。
func (g *expirySweepFakeGateway) Precreate(context.Context, domainpayment.PrecreateRequest) (*domainpayment.PrecreateResult, error) {
	return nil, errors.New("收口用例不应调用预下单")
}

// QueryTrade 按交易号返回预设结果，并记录调用顺序。
func (g *expirySweepFakeGateway) QueryTrade(_ context.Context, outTradeNo string) (*domainpayment.TradeQueryResult, error) {
	g.queryCalls = append(g.queryCalls, outTradeNo)
	if g.mismatchTradeNos[outTradeNo] {
		return &domainpayment.TradeQueryResult{
			OutTradeNo:  outTradeNo,
			TradeStatus: domainpayment.TradeStatusSuccess,
			TotalAmount: "0.01",
		}, nil
	}
	return &domainpayment.TradeQueryResult{OutTradeNo: outTradeNo}, nil
}

// CancelTrade 只记录调用：本用例的正常订单走「交易不存在」分支，不需要关单。
func (g *expirySweepFakeGateway) CancelTrade(_ context.Context, outTradeNo string) error {
	g.cancelCalls = append(g.cancelCalls, outTradeNo)
	return nil
}

// VerifyNotify 在收口用例里不应被调用。
func (g *expirySweepFakeGateway) VerifyNotify(context.Context, url.Values) (*domainpayment.NotifyPayload, error) {
	return nil, errors.New("收口用例不应调用通知验签")
}

// forceExpireAtDaysAgo 只按主键把夹具订单的 expire_at 前移到指定天数前：让夹具订单在
// 「按 expire_at 升序」的全局扫描里稳定排在共享库中的其它历史订单之前。
func (it *paymentExpiryIT) forceExpireAtDaysAgo(t *testing.T, ctx context.Context, registrationID int64, days int) {
	t.Helper()
	if _, err := it.db.ExecContext(ctx,
		`UPDATE hospital.medical_registration SET expire_at = now() - make_interval(days => $2::int) WHERE id = $1`,
		registrationID, days); err != nil {
		t.Fatalf("前移夹具订单 expire_at 失败（id=%d days=%d）：%v", registrationID, days, err)
	}
}

// TestPostgresPaymentExpiryAmountMismatchDoesNotRelease 覆盖资金异常保护：支付宝报告交易成功但
// 金额与本地订单不一致时只告警，订单保持 UNPAID，两级号源均不得释放，等待人工核对。
// 共享库上若存在更早订单则跳过，避免扫描修改非本用例数据。
func TestPostgresPaymentExpiryAmountMismatchDoesNotRelease(t *testing.T) {
	it, ctx := newPaymentExpiryIT(t)
	mismatch := it.newFixture(t, ctx)
	it.forceExpireAtDaysAgo(t, ctx, mismatch.created.ID, 30)

	gateway := &expirySweepFakeGateway{mismatchTradeNos: map[string]bool{
		mismatch.created.OutTradeNo: true,
	}}
	const batchSize = 1

	// 前置守卫：队首必须是本用例创建的金额异常订单，否则跳过而不是误动他人数据。
	head, err := it.payments.ListExpiredUnpaid(ctx, batchSize, time.Time{}, 0)
	if err != nil {
		t.Fatalf("扫描过期未付款订单失败：%v", err)
	}
	if len(head) != 1 || head[0].OutTradeNo != mismatch.created.OutTradeNo {
		t.Skip("共享库上存在更早的过期未付款订单，跳过以免修改非本用例数据")
	}

	collector := paymentservice.NewExpiryCollector(it.payments, gateway, paymentservice.SweepConfig{
		BatchSize:          batchSize,
		QueryAttempts:      1,
		QueryRetryInterval: time.Millisecond,
		OrderTimeout:       5 * time.Second,
	})
	stats, err := collector.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("金额异常处理不应返回错误：%v", err)
	}
	if want := (paymentservice.SweepStats{Scanned: 1, Skipped: 1, AmountMismatch: 1}); stats != want {
		t.Fatalf("收口计数 = %+v，期望 %+v", stats, want)
	}
	if got := it.paymentCode(t, ctx, mismatch.created.ID); got != registration.PaymentCodeUnpaid {
		t.Fatalf("金额异常订单 payment_status=%d，期望 %d（UNPAID）", got, registration.PaymentCodeUnpaid)
	}
	requirePaymentExpiryQuota(t, it, ctx, mismatch, 1, 1)
}

// ---- 脏数据不变式：缺少排班关联必须整笔回滚 ----

// nullRegistrationLink 只按主键把本用例夹具的某个关联列置空（work_plan_id / doctor_schedule_id），
// 模拟历史脏数据。列名不接受外部输入，用固定分支保证只可能是这两列之一。
func (it *paymentExpiryIT) nullRegistrationLink(t *testing.T, ctx context.Context, registrationID int64, column string) {
	t.Helper()
	var query string
	switch column {
	case "work_plan_id":
		query = `UPDATE hospital.medical_registration SET work_plan_id = NULL WHERE id = $1`
	case "doctor_schedule_id":
		query = `UPDATE hospital.medical_registration SET doctor_schedule_id = NULL WHERE id = $1`
	default:
		t.Fatalf("只允许置空 work_plan_id 或 doctor_schedule_id，实际 %q", column)
	}
	if _, err := it.db.ExecContext(ctx, query, registrationID); err != nil {
		t.Fatalf("置空夹具挂号 %s 失败（id=%d）：%v", column, registrationID, err)
	}
}

// TestPostgresPaymentExpiryRejectsMissingScheduleLink 覆盖收口事务的新增不变式：两级号源必须
// 同进同退，work_plan_id 或 doctor_schedule_id 任一缺失（脏数据）时必须返回错误并整笔回滚：
// 订单保持 UNPAID、两级 num 都不变化，不允许只减其中一级（契约 §6.8、业务说明第 4 节）。
func TestPostgresPaymentExpiryRejectsMissingScheduleLink(t *testing.T) {
	for _, column := range []string{"work_plan_id", "doctor_schedule_id"} {
		t.Run(column+" 缺失", func(t *testing.T) {
			it, ctx := newPaymentExpiryIT(t)
			fixture := it.newFixture(t, ctx)
			it.forceExpireAt(t, ctx, fixture.created.ID)
			it.nullRegistrationLink(t, ctx, fixture.created.ID, column)

			expired, err := it.payments.ExpireUnpaid(ctx, fixture.created.OutTradeNo)
			if err == nil {
				t.Fatalf("%s 缺失时必须返回错误，实际 err = nil（expired=%t）", column, expired)
			}
			if !strings.Contains(err.Error(), "缺少排班关联") {
				t.Fatalf("错误信息应说明缺少排班关联，实际：%v", err)
			}
			if expired {
				t.Fatalf("%s 缺失时不得返回 true", column)
			}
			if got := it.paymentCode(t, ctx, fixture.created.ID); got != registration.PaymentCodeUnpaid {
				t.Fatalf("整笔回滚后 payment_status 应保持 UNPAID(%d)，实际 %d", registration.PaymentCodeUnpaid, got)
			}
			requirePaymentExpiryQuota(t, it, ctx, fixture, 1, 1)
		})
	}
}

// TestPostgresPaymentExpiryRejectsDuplicateOutTradeNo 覆盖数据库中交易号重复的脏数据：
// 状态条件更新命中多行时必须整笔回滚，不能只按前置查询读到的第一行释放一次号源。
func TestPostgresPaymentExpiryRejectsDuplicateOutTradeNo(t *testing.T) {
	it, ctx := newPaymentExpiryIT(t)
	first := it.newFixture(t, ctx)
	second := it.newFixture(t, ctx)
	if _, err := it.db.ExecContext(ctx,
		`UPDATE hospital.medical_registration SET out_trade_no = $1 WHERE id = $2`,
		first.created.OutTradeNo, second.created.ID); err != nil {
		t.Fatalf("构造重复 out_trade_no 夹具失败：%v", err)
	}

	expired, err := it.payments.ExpireUnpaid(ctx, first.created.OutTradeNo)
	if err == nil || !strings.Contains(err.Error(), "期望恰好 1 行") {
		t.Fatalf("重复交易号必须返回命中行数错误，实际 expired=%t err=%v", expired, err)
	}
	for _, fixture := range []paymentExpiryFixture{first, second} {
		if got := it.paymentCode(t, ctx, fixture.created.ID); got != registration.PaymentCodeUnpaid {
			t.Fatalf("整笔回滚后 id=%d payment_status=%d，期望 UNPAID", fixture.created.ID, got)
		}
		requirePaymentExpiryQuota(t, it, ctx, fixture, 1, 1)
	}
}

// TestPostgresPaymentExpiryRejectsIncompleteQuotaRelease 覆盖两级计数器任一无法自减的脏数据：
// 即使状态 UPDATE 已命中，也必须回滚为 UNPAID，不能提交半完成的号源释放。
func TestPostgresPaymentExpiryRejectsIncompleteQuotaRelease(t *testing.T) {
	cases := []struct {
		name  string
		table string
		id    func(paymentExpiryFixture) int64
	}{
		{name: "计划级 num 已为 0", table: "hospital.doctor_work_plan", id: func(f paymentExpiryFixture) int64 { return f.planID }},
		{name: "时段级 num 已为 0", table: "hospital.doctor_work_plan_schedule", id: func(f paymentExpiryFixture) int64 { return f.slotID }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it, ctx := newPaymentExpiryIT(t)
			fixture := it.newFixture(t, ctx)
			query := "UPDATE " + tc.table + " SET num = 0 WHERE id = $1"
			if _, err := it.db.ExecContext(ctx, query, tc.id(fixture)); err != nil {
				t.Fatalf("构造计数器脏数据失败：%v", err)
			}

			expired, err := it.payments.ExpireUnpaid(ctx, fixture.created.OutTradeNo)
			if err == nil || !strings.Contains(err.Error(), "期望恰好 1 行") {
				t.Fatalf("号源未完整释放必须返回错误，实际 expired=%t err=%v", expired, err)
			}
			if got := it.paymentCode(t, ctx, fixture.created.ID); got != registration.PaymentCodeUnpaid {
				t.Fatalf("整笔回滚后 payment_status=%d，期望 UNPAID", got)
			}
		})
	}
}
