// 本文件实现 35 分钟订单过期收口任务的业务规则（契约 §6.8、
// spec/创建订单与支付业务说明.md 第 7 节）：扫描已过 expire_at 且仍未付款的订单，按
// 「主动查询补记 PAID -> 必要时关单 -> 条件更新置 EXPIRED 并在同一事务释放两级号源」收敛。
//
// 触发方式（进程启动即扫一轮、随后每 10~30 秒一轮）由 internal/worker 负责；本文件只承载规则，
// 使「收口」「异步通知」「兜底主动查询」共用同一段状态迁移实现（settleFromTradeQuery +
// 仓储条件更新），不出现两套口径。
package payment

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	domainpayment "Medical-Web-Backend/internal/domain/payment"
	"Medical-Web-Backend/internal/port"
)

// 收口任务的缺省参数：批量与重试次数都刻意取小值，避免单轮把压力打到支付宝或数据库；
// 规格只要求「查询重试超过阈值后仍继续收口」（业务说明第 14.2 节第 2 点）。
// 单笔时限必须覆盖「查询尝试次数 × 网关超时 + 重试等待 + 一次关单」：
// 缺省约 3×5s + 2×500ms 查询、再加一次关单，因此取 30s；调小它会掐掉重试与关单，
// 调大它会让极端情况下的一轮扫描变长（单轮逐笔串行处理）。
const (
	defaultSweepBatchSize       = 100
	defaultSweepQueryAttempts   = 3
	defaultSweepQueryRetryDelay = 500 * time.Millisecond
	defaultSweepOrderTimeout    = 30 * time.Second
)

// SweepConfig 是收口任务的可调参数；非正数在构造时收敛为缺省值。
type SweepConfig struct {
	// BatchSize 是单轮扫描的订单上限。
	BatchSize int
	// QueryAttempts 是单笔订单调用 alipay.trade.query 的最大尝试次数（含首次，至少 1 次）。
	QueryAttempts int
	// QueryRetryInterval 是两次查询尝试之间的等待间隔。
	QueryRetryInterval time.Duration
	// OrderTimeout 是单笔订单外部调用（查询 + 关单）的总时限。
	OrderTimeout time.Duration
}

// SweepStats 是一轮收口的处理计数，用于终端日志（本期不做任务失活告警）。
type SweepStats struct {
	// Scanned 是本轮扫描到的过期未付款订单数。
	Scanned int
	// MarkedPaid 是查询到支付成功并补记为 PAID 的订单数（补记不释放号源）。
	MarkedPaid int
	// Expired 是置为 EXPIRED 并释放号源的订单数。
	Expired int
	// Skipped 是被跳过的订单数：订单已被其它路径（通知、兜底查询或另一实例）收敛。
	Skipped int
	// AmountMismatch 是金额与支付宝查询结果不一致、因而不补记 PAID 也不释放号源的订单数。
	AmountMismatch int
	// Failed 是处理失败的订单数（仓储或外部依赖异常），下一轮会重试。
	Failed int
}

// ExpiryCollector 执行收口任务的一轮扫描。依赖都是接口，便于测试注入桩实现。
type ExpiryCollector struct {
	payments port.PaymentRepository
	gateway  port.AlipayGateway
	cfg      SweepConfig
	// 游标按 expire_at、registration_id 稳定推进；金额异常订单即使保留 UNPAID，也不会永久阻塞队首。
	cursorMu            sync.Mutex
	afterExpireAt       time.Time
	afterRegistrationID int64
}

// NewExpiryCollector 构造收口任务，并把非法或缺失的参数收敛为缺省值。
func NewExpiryCollector(
	payments port.PaymentRepository,
	gateway port.AlipayGateway,
	cfg SweepConfig,
) *ExpiryCollector {
	if cfg.BatchSize < 1 {
		cfg.BatchSize = defaultSweepBatchSize
	}
	if cfg.QueryAttempts < 1 {
		cfg.QueryAttempts = defaultSweepQueryAttempts
	}
	if cfg.QueryRetryInterval <= 0 {
		cfg.QueryRetryInterval = defaultSweepQueryRetryDelay
	}
	if cfg.OrderTimeout <= 0 {
		cfg.OrderTimeout = defaultSweepOrderTimeout
	}
	return &ExpiryCollector{payments: payments, gateway: gateway, cfg: cfg}
}

// SweepOnce 执行一轮收口：扫描一批过期未付款订单并逐笔收敛，返回本轮计数。
//
// 只有「扫描本身失败」才返回错误；单笔订单的外部依赖失败不中断整轮（由计数与日志体现），
// 否则一笔支付宝超时就可能挡住后面所有订单的号源释放。
func (c *ExpiryCollector) SweepOnce(ctx context.Context) (SweepStats, error) {
	var stats SweepStats
	if c == nil || c.payments == nil {
		return stats, errors.New("收口任务依赖未配置")
	}
	afterExpireAt, afterRegistrationID := c.cursor()
	items, err := c.payments.ListExpiredUnpaid(ctx, c.cfg.BatchSize, afterExpireAt, afterRegistrationID)
	if err != nil {
		return stats, fmt.Errorf("扫描过期未付款订单失败: %w", err)
	}
	// 游标之后没有数据时立即回绕队首，避免新插入或上轮跳过的订单要额外等待一轮。
	if len(items) == 0 && !afterExpireAt.IsZero() {
		c.setCursor(time.Time{}, 0)
		items, err = c.payments.ListExpiredUnpaid(ctx, c.cfg.BatchSize, time.Time{}, 0)
		if err != nil {
			return stats, fmt.Errorf("回绕扫描过期未付款订单失败: %w", err)
		}
	}
	if len(items) == c.cfg.BatchSize {
		last := items[len(items)-1]
		c.setCursor(last.ExpireAt, last.RegistrationID)
	} else {
		c.setCursor(time.Time{}, 0)
	}
	for index := range items {
		stats.Scanned++
		if err := c.settle(ctx, items[index], &stats); err != nil {
			stats.Failed++
			log.Printf("告警：收口订单失败 out_trade_no=%s：%v", items[index].OutTradeNo, err)
		}
	}
	return stats, nil
}

func (c *ExpiryCollector) cursor() (time.Time, int64) {
	c.cursorMu.Lock()
	defer c.cursorMu.Unlock()
	return c.afterExpireAt, c.afterRegistrationID
}

func (c *ExpiryCollector) setCursor(expireAt time.Time, registrationID int64) {
	c.cursorMu.Lock()
	defer c.cursorMu.Unlock()
	c.afterExpireAt = expireAt
	c.afterRegistrationID = registrationID
}

// settle 按第 7 节顺序收敛单笔订单：查询 ->（成功则补记 PAID）-> 必要时关单 -> 收口事务。
func (c *ExpiryCollector) settle(
	ctx context.Context,
	item domainpayment.Payment,
	stats *SweepStats,
) error {
	// 单笔订单的外部调用单独设时限：一笔卡住的查询不能拖住整轮扫描。
	orderCtx, cancel := context.WithTimeout(ctx, c.cfg.OrderTimeout)
	defer cancel()

	result, queryErr := c.queryTrade(orderCtx, item.OutTradeNo)
	switch {
	case queryErr != nil:
		// 查询本身失败（网络故障、支付宝不可用或系统级错误）：重试后仍失败也继续收口，
		// 不允许因为支付宝不可用让号源被永久占用；由此产生的资金差异由每日核查兜底
		// （业务说明第 7 节、第 14.2 节第 2 点）。
		log.Printf("告警：收口任务查询交易状态失败，仍继续收口 out_trade_no=%s：%v",
			item.OutTradeNo, queryErr)
	default:
		settlement, err := settleFromTradeQuery(ctx, c.payments, &item, result)
		if err != nil {
			return err
		}
		switch {
		case settlement.AmountMismatch:
			// 资金异常时不能确认这笔支付宝成功交易是否属于本地订单：只告警并保留 UNPAID，
			// 不得释放号源，否则可能在资金未人工核对前造成重复售号。
			stats.AmountMismatch++
			stats.Skipped++
			return nil
		case settlement.Migrated:
			// 用户实际已完成支付，只是通知丢失或迟到：补记 PAID，**不释放号源**（第 7 节第 2 步）。
			stats.MarkedPaid++
			log.Printf("订单收口补记已支付：out_trade_no=%s transaction_id=%s",
				item.OutTradeNo, result.TradeNo)
			return nil
		case settlement.SuccessEvidence:
			// 成功证据但条件更新未生效：状态已被通知路径或另一实例改写，不做任何释放。
			stats.Skipped++
			return nil
		case result != nil && result.TradeStatus == domainpayment.TradeStatusWaitBuyerPay:
			// 支付宝侧仍可支付：先尽力关单；关单失败（含超时、交易已关闭）不阻塞收口。
			if err := c.cancelTrade(orderCtx, item.OutTradeNo); err != nil {
				log.Printf("告警：收口任务关闭支付宝交易失败，仍继续收口 out_trade_no=%s：%v",
					item.OutTradeNo, err)
			}
		}
	}

	// 收口事务：条件更新置 EXPIRED 并释放两级号源；未命中说明已被其它路径处理，不释放。
	expired, err := c.payments.ExpireUnpaid(ctx, item.OutTradeNo)
	if err != nil {
		return fmt.Errorf("收口事务执行失败: %w", err)
	}
	if !expired {
		stats.Skipped++
		return nil
	}
	stats.Expired++
	log.Printf("订单已收口：out_trade_no=%s 置为 EXPIRED 并释放计划级与时段级号源", item.OutTradeNo)
	return nil
}

// queryTrade 调用 alipay.trade.query 并做有限次重试；ctx 结束（整轮取消或单笔超时）立即停止。
func (c *ExpiryCollector) queryTrade(
	ctx context.Context,
	outTradeNo string,
) (*domainpayment.TradeQueryResult, error) {
	if c.gateway == nil {
		return nil, errors.New("未配置支付宝网关")
	}
	var lastErr error
	for attempt := 1; attempt <= c.cfg.QueryAttempts; attempt++ {
		result, err := c.gateway.QueryTrade(ctx, outTradeNo)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if attempt == c.cfg.QueryAttempts || !sleepContext(ctx, c.cfg.QueryRetryInterval) {
			break
		}
	}
	return nil, lastErr
}

// cancelTrade 关单；网关未配置时返回错误，由调用方按「关单失败不阻塞收口」处理。
func (c *ExpiryCollector) cancelTrade(ctx context.Context, outTradeNo string) error {
	if c.gateway == nil {
		return errors.New("未配置支付宝网关")
	}
	return c.gateway.CancelTrade(ctx, outTradeNo)
}

// sleepContext 在 ctx 未结束时等待 d；返回 false 表示 ctx 已结束（不必再重试）。
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// tradeQuerySettlement 描述一次 alipay.trade.query 结果对本地订单的意义。
type tradeQuerySettlement struct {
	// SuccessEvidence 报告查询结果是否构成支付成功证据（交易状态成功且金额一致）。
	SuccessEvidence bool
	// AmountMismatch 报告状态成功但金额与订单不一致，属于资金异常：只告警、不迁移状态。
	AmountMismatch bool
	// Migrated 报告本次调用是否真正完成了 UNPAID -> PAID 迁移。
	Migrated bool
}

// settleFromTradeQuery 是「依据一次主动查询结果补记 PAID」的唯一实现（契约 §6.6、§6.8）：
// 兜底主动查询与收口任务都调用它，禁止各写一套。它只做两件事——判定成功证据（含金额校验）
// 与走仓储的条件更新，绝不绕过 use case 直接改库。
func settleFromTradeQuery(
	ctx context.Context,
	payments port.PaymentRepository,
	item *domainpayment.Payment,
	result *domainpayment.TradeQueryResult,
) (tradeQuerySettlement, error) {
	// result 为 nil 属于网关实现违反端口契约（交易不存在时必须返回空状态结果）：
	// 这里按「不构成任何支付证据」收敛，避免一次异常返回值把调用方解引用打崩。
	if result == nil || !domainpayment.SuccessTradeStatus(result.TradeStatus) {
		return tradeQuerySettlement{}, nil
	}
	if !domainpayment.AmountEquals(result.TotalAmount, item.Amount) {
		// 金额对不上说明查询结果与本地订单不是同一笔：只告警不迁移，避免错误地标记为已支付。
		log.Printf(
			"告警：支付宝主动查询金额与订单不一致 out_trade_no=%s 查询金额=%s 订单金额=%s，不迁移状态",
			item.OutTradeNo, result.TotalAmount, item.Amount)
		return tradeQuerySettlement{AmountMismatch: true}, nil
	}
	updated, err := payments.MarkPaid(ctx, item.OutTradeNo, result.TradeNo)
	if err != nil {
		return tradeQuerySettlement{}, err
	}
	return tradeQuerySettlement{SuccessEvidence: true, Migrated: updated}, nil
}
