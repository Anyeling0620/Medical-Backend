// 本文件是 35 分钟订单过期收口任务（ExpiryCollector.SweepOnce）的单元测试
// （契约 §6.8、spec/创建订单与支付业务说明.md 第 7 节）：用 service_test.go 的内存桩覆盖
// 「查询补记 PAID、关单、收口事务」三条分支与统计计数，不依赖 PostgreSQL、Redis、
// 真实支付宝与系统时间。
package payment

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	domainpayment "Medical-Web-Backend/internal/domain/payment"
	"Medical-Web-Backend/internal/port"
)

// sweepTestConfig 返回测试用收口配置：查询重试间隔压到 1 毫秒，
// 避免「查询连续失败」用例真的等待缺省的 500 毫秒。
func sweepTestConfig() SweepConfig {
	return SweepConfig{
		BatchSize:          10,
		QueryAttempts:      3,
		QueryRetryInterval: time.Millisecond,
		OrderTimeout:       time.Second,
	}
}

// sweepTestExpiredItem 返回一笔「已过 expire_at 且仍为 UNPAID」的订单：
// 金额与 paymentTestAmount 一致，expire_at 提前 1 分钟，模拟收口任务的扫描结果。
func sweepTestExpiredItem() domainpayment.Payment {
	item := *paymentTestUnpaid()
	item.ExpireAt = paymentTestServiceNow.Add(-time.Minute)
	return item
}

// sweepTestQueryResult 构造一次 alipay.trade.query 结果；totalAmount 传空串表示该结果
// 不含金额（例如 WAIT_BUYER_PAY），传其它值用于覆盖金额不一致分支。
func sweepTestQueryResult(tradeStatus, totalAmount string) *domainpayment.TradeQueryResult {
	return &domainpayment.TradeQueryResult{
		OutTradeNo:  paymentTestOutTradeNo,
		TradeNo:     paymentTestTradeNo,
		TradeStatus: tradeStatus,
		TotalAmount: totalAmount,
	}
}

// requireSweepStats 断言一轮收口的完整计数，任何一项不符都打印完整口径便于定位。
func requireSweepStats(t *testing.T, got SweepStats, want SweepStats) {
	t.Helper()
	if got != want {
		t.Fatalf("本轮收口计数 = %+v，期望 %+v", got, want)
	}
}

// requireSingleSweepCall 断言某个外部依赖或仓储方法恰好被调用一次，且入参为夹具交易号。
func requireSingleSweepCall(t *testing.T, label string, calls []string) {
	t.Helper()
	if len(calls) != 1 || calls[0] != paymentTestOutTradeNo {
		t.Fatalf("%s 调用记录 = %v，期望 [%s]", label, calls, paymentTestOutTradeNo)
	}
}

// requireNoSweepCall 断言某个外部依赖或仓储方法完全没有被调用。
func requireNoSweepCall(t *testing.T, label string, calls []string) {
	t.Helper()
	if len(calls) != 0 {
		t.Fatalf("%s 不应被调用，实际调用 %v", label, calls)
	}
}

// TestSweepOnceWaitBuyerPayCancelsThenExpires 覆盖「支付宝侧仍可支付」分支：
// 查询返回 WAIT_BUYER_PAY 时必须先调用 alipay.trade.cancel 关单，再进入收口事务置 EXPIRED
// 并释放号源（Expired=1），且不得补记 PAID（业务说明第 7 节第 3、4 步）。
func TestSweepOnceWaitBuyerPayCancelsThenExpires(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.expiredItems = []domainpayment.Payment{sweepTestExpiredItem()}
	repo.expireResult = true
	gateway := &fakeAlipayGateway{
		queryResult: sweepTestQueryResult(domainpayment.TradeStatusWaitBuyerPay, ""),
	}
	collector := NewExpiryCollector(repo, gateway, sweepTestConfig())

	stats, err := collector.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("收口一轮不应返回错误，实际 %v", err)
	}
	requireSingleSweepCall(t, "主动查询", gateway.queryCalls)
	requireSingleSweepCall(t, "关单", gateway.cancelCalls)
	requireSingleSweepCall(t, "收口事务", repo.expireCalls)
	if len(repo.markCalls) != 0 {
		t.Fatalf("WAIT_BUYER_PAY 不得补记 PAID，实际迁移 %+v", repo.markCalls)
	}
	requireSweepStats(t, stats, SweepStats{Scanned: 1, Expired: 1})
}

// TestSweepOnceSuccessTradeIsRecordedWithoutRelease 覆盖「查询到支付成功」分支：
// TRADE_SUCCESS/TRADE_FINISHED 且金额一致时补记 PAID（MarkedPaid=1），
// 且**不得**调用收口事务——可支付的订单一旦误置 EXPIRED 就会错误释放号源。
func TestSweepOnceSuccessTradeIsRecordedWithoutRelease(t *testing.T) {
	for _, tradeStatus := range []string{domainpayment.TradeStatusSuccess, domainpayment.TradeStatusFinished} {
		t.Run(tradeStatus, func(t *testing.T) {
			repo := newFakePaymentRepository()
			repo.expiredItems = []domainpayment.Payment{sweepTestExpiredItem()}
			repo.markPaidResult = true
			gateway := &fakeAlipayGateway{
				queryResult: sweepTestQueryResult(tradeStatus, paymentTestAmount),
			}
			collector := NewExpiryCollector(repo, gateway, sweepTestConfig())

			stats, err := collector.SweepOnce(context.Background())
			if err != nil {
				t.Fatalf("收口一轮不应返回错误，实际 %v", err)
			}
			if len(repo.markCalls) != 1 ||
				repo.markCalls[0] != (markPaidCall{paymentTestOutTradeNo, paymentTestTradeNo}) {
				t.Fatalf("补记入参 = %+v，期望 outTradeNo=%s tradeNo=%s",
					repo.markCalls, paymentTestOutTradeNo, paymentTestTradeNo)
			}
			requireNoSweepCall(t, "收口事务", repo.expireCalls)
			requireNoSweepCall(t, "关单", gateway.cancelCalls)
			requireSweepStats(t, stats, SweepStats{Scanned: 1, MarkedPaid: 1})
		})
	}
}

// TestSweepOnceAmountMismatchDoesNotRelease 覆盖资金异常：查询状态成功但金额与订单不一致时
// 不补记 PAID，也不得置 EXPIRED 或释放号源，等待人工核对资金差异。
func TestSweepOnceAmountMismatchDoesNotRelease(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.expiredItems = []domainpayment.Payment{sweepTestExpiredItem()}
	repo.expireResult = true
	gateway := &fakeAlipayGateway{
		queryResult: sweepTestQueryResult(domainpayment.TradeStatusSuccess, "0.01"),
	}
	collector := NewExpiryCollector(repo, gateway, sweepTestConfig())

	stats, err := collector.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("收口一轮不应返回错误，实际 %v", err)
	}
	if len(repo.markCalls) != 0 {
		t.Fatalf("金额不一致不得补记 PAID，实际迁移 %+v", repo.markCalls)
	}
	requireNoSweepCall(t, "收口事务", repo.expireCalls)
	requireNoSweepCall(t, "关单", gateway.cancelCalls)
	requireSweepStats(t, stats, SweepStats{Scanned: 1, Skipped: 1, AmountMismatch: 1})
}

// TestSweepOnceCursorAdvancesPastAmountMismatch 覆盖金额异常队首的饥饿反例：前两笔保持
// UNPAID 后，稳定游标仍应让下一轮扫描到第三笔正常订单并完成收口。
func TestSweepOnceCursorAdvancesPastAmountMismatch(t *testing.T) {
	base := sweepTestExpiredItem()
	items := make([]domainpayment.Payment, 3)
	for index := range items {
		items[index] = base
		items[index].RegistrationID = int64(index + 1)
		items[index].OutTradeNo = fmt.Sprintf("cursor-order-%d", index+1)
		items[index].ExpireAt = base.ExpireAt.Add(time.Duration(index) * time.Second)
	}
	repo := newFakePaymentRepository()
	repo.expiredItems = items
	repo.expireResult = true
	gateway := &fakeAlipayGateway{queryResults: map[string]*domainpayment.TradeQueryResult{
		items[0].OutTradeNo: {TradeStatus: domainpayment.TradeStatusSuccess, TotalAmount: "0.01"},
		items[1].OutTradeNo: {TradeStatus: domainpayment.TradeStatusSuccess, TotalAmount: "0.01"},
		items[2].OutTradeNo: {},
	}}
	cfg := sweepTestConfig()
	cfg.BatchSize = 2
	collector := NewExpiryCollector(repo, gateway, cfg)

	first, err := collector.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("第一轮扫描失败：%v", err)
	}
	requireSweepStats(t, first, SweepStats{Scanned: 2, Skipped: 2, AmountMismatch: 2})
	second, err := collector.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("第二轮扫描失败：%v", err)
	}
	requireSweepStats(t, second, SweepStats{Scanned: 1, Expired: 1})
	if len(repo.expireCalls) != 1 || repo.expireCalls[0] != items[2].OutTradeNo {
		t.Fatalf("游标后正常订单未被收口，实际调用 %v", repo.expireCalls)
	}
}

// TestSweepOnceNilQueryResultDoesNotPanic 覆盖网关违反端口约定返回 (nil, nil) 的防御路径：
// nil 结果按无支付证据处理，当前订单继续收口，且不会中断同一批次的后续订单。
func TestSweepOnceNilQueryResultDoesNotPanic(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.expiredItems = []domainpayment.Payment{sweepTestExpiredItem(), sweepTestExpiredItem()}
	repo.expireResult = true
	gateway := &fakeAlipayGateway{queryResult: nil}
	collector := NewExpiryCollector(repo, gateway, sweepTestConfig())

	stats, err := collector.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("nil 查询结果不应中断整轮，实际 %v", err)
	}
	if len(repo.expireCalls) != 2 {
		t.Fatalf("同批两笔订单都应进入收口事务，实际调用 %d 次", len(repo.expireCalls))
	}
	requireSweepStats(t, stats, SweepStats{Scanned: 2, Expired: 2})
}

// TestSweepOnceClosedTradeExpiresWithoutCancel 覆盖不需要关单的两种查询结果：
// TRADE_CLOSED（支付宝侧已关闭）与交易不存在（TradeStatus 为空）都必须直接进入收口事务，
// 不调用 alipay.trade.cancel（业务说明第 7 节第 4 步）。
func TestSweepOnceClosedTradeExpiresWithoutCancel(t *testing.T) {
	cases := []struct {
		name        string
		tradeStatus string
	}{
		{name: "TRADE_CLOSED", tradeStatus: domainpayment.TradeStatusClosed},
		{name: "交易不存在（空状态）", tradeStatus: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakePaymentRepository()
			repo.expiredItems = []domainpayment.Payment{sweepTestExpiredItem()}
			repo.expireResult = true
			gateway := &fakeAlipayGateway{
				queryResult: sweepTestQueryResult(tc.tradeStatus, ""),
			}
			collector := NewExpiryCollector(repo, gateway, sweepTestConfig())

			stats, err := collector.SweepOnce(context.Background())
			if err != nil {
				t.Fatalf("收口一轮不应返回错误，实际 %v", err)
			}
			requireNoSweepCall(t, "关单", gateway.cancelCalls)
			requireSingleSweepCall(t, "收口事务", repo.expireCalls)
			requireSweepStats(t, stats, SweepStats{Scanned: 1, Expired: 1})
		})
	}
}

// TestSweepOnceQueryFailureStillExpires 覆盖查询失败：按配置次数重试后仍然继续收口，
// 不允许因为支付宝不可用让号源被永久占用；调用次数必须等于配置的尝试次数。
func TestSweepOnceQueryFailureStillExpires(t *testing.T) {
	cases := []struct {
		name     string
		attempts int
	}{
		{name: "单次尝试", attempts: 1},
		{name: "三次尝试", attempts: 3},
		{name: "五次尝试", attempts: 5},
	}
	for _, tc := range cases {
		attempts := tc.attempts
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakePaymentRepository()
			repo.expiredItems = []domainpayment.Payment{sweepTestExpiredItem()}
			repo.expireResult = true
			gateway := &fakeAlipayGateway{queryErr: port.ErrAlipayUnavailable}
			cfg := sweepTestConfig()
			cfg.QueryAttempts = attempts
			collector := NewExpiryCollector(repo, gateway, cfg)

			stats, err := collector.SweepOnce(context.Background())
			if err != nil {
				t.Fatalf("查询失败不应中断整轮，实际 %v", err)
			}
			if len(gateway.queryCalls) != attempts {
				t.Fatalf("查询调用次数 = %d，期望配置值 %d", len(gateway.queryCalls), attempts)
			}
			requireNoSweepCall(t, "关单", gateway.cancelCalls)
			requireSingleSweepCall(t, "收口事务", repo.expireCalls)
			requireSweepStats(t, stats, SweepStats{Scanned: 1, Expired: 1})
		})
	}
}

// TestSweepOnceCancelFailureStillExpires 覆盖关单失败：alipay.trade.cancel 报错（含超时）
// 不阻塞收口，仍然执行收口事务释放号源，资金差异由每日核查兜底（业务说明第 7 节）。
func TestSweepOnceCancelFailureStillExpires(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.expiredItems = []domainpayment.Payment{sweepTestExpiredItem()}
	repo.expireResult = true
	gateway := &fakeAlipayGateway{
		queryResult: sweepTestQueryResult(domainpayment.TradeStatusWaitBuyerPay, ""),
		cancelErr:   errors.New("关单超时"),
	}
	collector := NewExpiryCollector(repo, gateway, sweepTestConfig())

	stats, err := collector.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("关单失败不应中断整轮，实际 %v", err)
	}
	requireSingleSweepCall(t, "关单", gateway.cancelCalls)
	requireSingleSweepCall(t, "收口事务", repo.expireCalls)
	requireSweepStats(t, stats, SweepStats{Scanned: 1, Expired: 1})
}

// TestSweepOnceExpireLosesRaceIsSkipped 覆盖收口事务未命中：订单已被通知路径、主动查询
// 或另一实例收敛时 ExpireUnpaid 返回 false，本轮既不计数 Expired 也不重复释放号源。
func TestSweepOnceExpireLosesRaceIsSkipped(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.expiredItems = []domainpayment.Payment{sweepTestExpiredItem()}
	repo.expireResult = false
	gateway := &fakeAlipayGateway{
		queryResult: sweepTestQueryResult(domainpayment.TradeStatusClosed, ""),
	}
	collector := NewExpiryCollector(repo, gateway, sweepTestConfig())

	stats, err := collector.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("收口一轮不应返回错误，实际 %v", err)
	}
	requireSingleSweepCall(t, "收口事务", repo.expireCalls)
	requireSweepStats(t, stats, SweepStats{Scanned: 1, Skipped: 1})
}

// TestSweepOnceBatchLimitAndDefaults 覆盖参数透传与收敛：
// BatchSize 必须原样传给 ListExpiredUnpaid；传 0 时批量与查询尝试次数都收敛为缺省值。
func TestSweepOnceBatchLimitAndDefaults(t *testing.T) {
	t.Run("BatchSize 透传", func(t *testing.T) {
		repo := newFakePaymentRepository()
		cfg := sweepTestConfig()
		cfg.BatchSize = 7
		collector := NewExpiryCollector(repo, &fakeAlipayGateway{}, cfg)

		if _, err := collector.SweepOnce(context.Background()); err != nil {
			t.Fatalf("空轮扫描不应返回错误，实际 %v", err)
		}
		if repo.listExpiredLimit != 7 {
			t.Fatalf("扫描批量 = %d，期望配置值 7", repo.listExpiredLimit)
		}
	})

	t.Run("BatchSize 缺省收敛", func(t *testing.T) {
		repo := newFakePaymentRepository()
		collector := NewExpiryCollector(repo, &fakeAlipayGateway{}, SweepConfig{})

		if _, err := collector.SweepOnce(context.Background()); err != nil {
			t.Fatalf("空轮扫描不应返回错误，实际 %v", err)
		}
		if repo.listExpiredLimit != defaultSweepBatchSize {
			t.Fatalf("扫描批量 = %d，期望缺省值 %d", repo.listExpiredLimit, defaultSweepBatchSize)
		}
	})

	t.Run("QueryAttempts 缺省收敛", func(t *testing.T) {
		repo := newFakePaymentRepository()
		repo.expiredItems = []domainpayment.Payment{sweepTestExpiredItem()}
		repo.expireResult = true
		gateway := &fakeAlipayGateway{queryErr: port.ErrAlipayUnavailable}
		collector := NewExpiryCollector(repo, gateway, SweepConfig{QueryRetryInterval: time.Millisecond})

		if _, err := collector.SweepOnce(context.Background()); err != nil {
			t.Fatalf("查询失败不应中断整轮，实际 %v", err)
		}
		if len(gateway.queryCalls) != defaultSweepQueryAttempts {
			t.Fatalf("查询调用次数 = %d，期望缺省值 %d", len(gateway.queryCalls), defaultSweepQueryAttempts)
		}
	})
}

// TestSweepOnceScanFailureReturnsError 覆盖只有扫描失败才中断整轮：仓储读失败必须向上返回
// 错误交给调用方记日志，而不是当成空轮悄悄成功（否则任务停摆不会被发现）。
func TestSweepOnceScanFailureReturnsError(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.listExpiredErr = errors.New("数据库不可用")
	collector := NewExpiryCollector(repo, &fakeAlipayGateway{}, sweepTestConfig())

	if _, err := collector.SweepOnce(context.Background()); err == nil {
		t.Fatal("扫描失败必须向上返回错误，实际 err = nil")
	}
}

// TestSweepOnceSuccessEvidenceWithoutMigrationIsSkipped 覆盖「查询到成功证据但条件更新未生效」：
// 通知路径或另一实例已先完成 UNPAID -> PAID 迁移时，本轮不得重复补记、也不得置 EXPIRED，
// 只按 Skipped 计数，避免与通知路径竞争同一状态迁移（业务说明第 7 节）。
func TestSweepOnceSuccessEvidenceWithoutMigrationIsSkipped(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.expiredItems = []domainpayment.Payment{sweepTestExpiredItem()}
	repo.markPaidResult = false
	gateway := &fakeAlipayGateway{
		queryResult: sweepTestQueryResult(domainpayment.TradeStatusSuccess, paymentTestAmount),
	}
	collector := NewExpiryCollector(repo, gateway, sweepTestConfig())

	stats, err := collector.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("收口一轮不应返回错误，实际 %v", err)
	}
	if len(repo.markCalls) != 1 {
		t.Fatalf("成功证据必须尝试一次条件更新，实际 %d 次", len(repo.markCalls))
	}
	requireNoSweepCall(t, "收口事务", repo.expireCalls)
	requireNoSweepCall(t, "关单", gateway.cancelCalls)
	requireSweepStats(t, stats, SweepStats{Scanned: 1, Skipped: 1})
}
