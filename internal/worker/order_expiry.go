// Package worker 承载服务进程内的后台定时任务。
//
// 当前只有一个任务：35 分钟订单过期收口（契约 §6.8、spec/创建订单与支付业务说明.md 第 7 节）。
// 任务的业务规则在 usecase/payment（ExpiryCollector），本包只负责触发方式：
// 进程启动即执行一轮（覆盖进程重启期间漏扫的订单），随后按固定间隔循环；单轮内部逐笔串行处理，
// 因此天生不会与上一轮重叠。任务本身的可用性本期不做兜底（不实现失活告警），
// 执行与失败信息只输出终端日志。
package worker

import (
	"context"
	"log"
	"time"

	paymentservice "Medical-Web-Backend/internal/usecase/payment"
)

// defaultOrderExpiryInterval 是收口任务的缺省扫描间隔：规格建议每 10~30 秒一轮。
const defaultOrderExpiryInterval = 20 * time.Second

// ExpirySweeper 是收口任务的业务能力依赖，由 usecase/payment.ExpiryCollector 实现。
// 定义成接口是为了让调度逻辑可以用测试桩验证「启动即扫、按间隔循环、ctx 取消即退出」，
// 不必依赖真实数据库与支付宝。
type ExpirySweeper interface {
	// SweepOnce 执行一轮收口并返回本轮计数；扫描本身失败时返回错误。
	SweepOnce(ctx context.Context) (paymentservice.SweepStats, error)
}

// OrderExpiryConfig 是调度器的可调参数。
type OrderExpiryConfig struct {
	// Interval 是扫描间隔；非正数时收敛为缺省值（20 秒）。
	Interval time.Duration
}

// OrderExpiryWorker 是订单过期收口任务的调度器。
type OrderExpiryWorker struct {
	sweeper ExpirySweeper
	cfg     OrderExpiryConfig
}

// NewOrderExpiryWorker 构造调度器。
func NewOrderExpiryWorker(sweeper ExpirySweeper, cfg OrderExpiryConfig) *OrderExpiryWorker {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultOrderExpiryInterval
	}
	return &OrderExpiryWorker{sweeper: sweeper, cfg: cfg}
}

// Run 阻塞运行任务直到 ctx 结束：先扫一轮（覆盖重启期间漏扫的订单），再按 Interval 循环。
// 调用方（bootstrap）负责在独立 goroutine 中启动它。
func (w *OrderExpiryWorker) Run(ctx context.Context) {
	if w == nil || w.sweeper == nil {
		log.Printf("告警：订单收口任务的业务依赖未配置，任务未启动")
		return
	}
	log.Printf("订单收口任务已启动：启动即扫一轮，之后每 %s 一轮", w.cfg.Interval)
	w.sweep(ctx)
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("订单收口任务已停止")
			return
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

// sweep 执行一轮并输出终端日志：本期不做任务失活告警，日志是唯一的观察手段。
// 本轮没有扫描到订单时不输出（避免空轮刷屏），失败与收口动作逐条输出。
func (w *OrderExpiryWorker) sweep(ctx context.Context) {
	// 后台任务里的一次 panic 会带走整个服务进程，而收口本身是尽力而为的补偿动作：
	// 这里兜住 panic，只把本轮标记为失败，下一轮继续。
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("告警：订单收口任务本轮发生 panic，已恢复：%v", recovered)
		}
	}()
	if err := ctx.Err(); err != nil {
		return
	}
	stats, err := w.sweeper.SweepOnce(ctx)
	if err != nil {
		log.Printf("告警：订单收口任务本轮扫描失败：%v", err)
		return
	}
	if stats.Scanned == 0 {
		return
	}
	log.Printf(
		"订单收口任务本轮完成：扫描 %d 单，补记已支付 %d 单，置为 EXPIRED 并释放号源 %d 单，金额异常保留 UNPAID %d 单，跳过 %d 单，失败 %d 单",
		stats.Scanned, stats.MarkedPaid, stats.Expired, stats.AmountMismatch, stats.Skipped, stats.Failed)
}
