// 本文件是订单过期收口任务调度器（OrderExpiryWorker.Run）的单元测试：用测试桩实现
// ExpirySweeper，验证「启动即扫一轮、按间隔循环、ctx 取消立即退出、依赖缺失不 panic」，
// 不依赖 PostgreSQL、Redis 与真实支付宝。
package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	paymentservice "Medical-Web-Backend/internal/usecase/payment"
)

// sweepStub 是 ExpirySweeper 的测试桩：记录累计调用次数，并在每次调用后向 called 通道发一个
// 信号。用例靠信号 + 超时等待轮次，而不是固定 sleep，避免机器负载造成的偶发失败。
type sweepStub struct {
	mu sync.Mutex
	// panicOnCall 为 true 时每次 SweepOnce 都 panic，用于验证调度器的 recover 兜底。
	panicOnCall bool
	count       int
	called      chan struct{}
}

// newSweepStub 构造测试桩；通道带缓冲，避免调度器在用例还没开始等待时被阻塞。
func newSweepStub() *sweepStub {
	return &sweepStub{called: make(chan struct{}, 16)}
}

// SweepOnce 记录一次调用并发送信号；固定返回空计数（不输出逐轮日志，保持测试输出干净）。
func (s *sweepStub) SweepOnce(_ context.Context) (paymentservice.SweepStats, error) {
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
	select {
	case s.called <- struct{}{}:
	default:
	}
	if s.panicOnCall {
		panic("收口任务桩：模拟单轮 panic")
	}
	return paymentservice.SweepStats{}, nil
}

// calls 返回累计调用次数。
func (s *sweepStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// waitForCalls 等待累计调用次数达到 want 次，最多等待 timeout；超时即判定任务没有按预期触发。
func waitForCalls(t *testing.T, stub *sweepStub, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for stub.calls() < want {
		select {
		case <-stub.called:
		case <-deadline:
			t.Fatalf("等待收口任务第 %d 轮扫描超时，当前仅调用 %d 次", want, stub.calls())
		}
	}
}

// TestOrderExpiryWorkerSweepsOnStartAndOnInterval 覆盖调度方式：Run 启动后必须立刻扫一轮
// （覆盖进程重启期间漏扫的订单），随后每 Interval 再扫一轮（业务说明第 7 节）。
func TestOrderExpiryWorkerSweepsOnStartAndOnInterval(t *testing.T) {
	stub := newSweepStub()
	worker := NewOrderExpiryWorker(stub, OrderExpiryConfig{Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()

	waitForCalls(t, stub, 1, 2*time.Second)
	waitForCalls(t, stub, 2, 2*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 未在 2 秒内返回")
	}
}

// TestOrderExpiryWorkerStopsOnContextCancel 覆盖退出方式：间隔取 1 小时，Run 的返回只能由
// ctx 取消驱动；取消后再确认没有新的扫描（进程退出时任务必须收尾，不能在数据库连接关闭后继续跑）。
func TestOrderExpiryWorkerStopsOnContextCancel(t *testing.T) {
	stub := newSweepStub()
	worker := NewOrderExpiryWorker(stub, OrderExpiryConfig{Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	waitForCalls(t, stub, 1, 2*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 未在 2 秒内返回")
	}
	if got := stub.calls(); got != 1 {
		t.Fatalf("ctx 取消后不应再扫描，实际调用 %d 次", got)
	}
}

// TestOrderExpiryWorkerWithoutSweeperDoesNotPanic 覆盖依赖缺失（bootstrap 未接线）：
// 业务依赖为 nil 时 Run 只输出告警后立即返回，nil 接收者调用同一个方法也不得 panic。
func TestOrderExpiryWorkerWithoutSweeperDoesNotPanic(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewOrderExpiryWorker(nil, OrderExpiryConfig{}).Run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("业务依赖缺失时 Run 应立即返回")
	}

	var nilWorker *OrderExpiryWorker
	nilWorker.Run(context.Background())
}

// TestNewOrderExpiryWorkerConvergesInterval 覆盖参数收敛：Interval 传非正数时收敛为缺省值
// （20 秒），避免误配成 0 造成紧凑空转。
func TestNewOrderExpiryWorkerConvergesInterval(t *testing.T) {
	worker := NewOrderExpiryWorker(newSweepStub(), OrderExpiryConfig{})
	if worker.cfg.Interval != defaultOrderExpiryInterval {
		t.Fatalf("扫描间隔 = %s，期望缺省值 %s", worker.cfg.Interval, defaultOrderExpiryInterval)
	}
	if defaultOrderExpiryInterval != 20*time.Second {
		t.Fatalf("缺省扫描间隔 = %s，期望 20s（规格建议 10~30 秒）", defaultOrderExpiryInterval)
	}
}

// TestOrderExpiryWorkerRecoversFromPanic 覆盖新增的单轮 panic 兜底：后台任务里一次 panic
// 会带走整个服务进程，而收口本身是尽力而为的补偿动作，因此 sweep 必须 recover 并让 Run
// 继续按间隔跑下一轮，不能因为单轮异常让任务彻底停摆。

// TestOrderExpiryWorkerRecoversFromPanic 覆盖新增的单轮 panic 兜底：后台任务里一次 panic
// 会带走整个服务进程，而收口本身是尽力而为的补偿动作，因此 sweep 必须 recover 并让 Run
// 继续按间隔跑下一轮，不能因为单轮异常让任务彻底停摆。
func TestOrderExpiryWorkerRecoversFromPanic(t *testing.T) {
	stub := newSweepStub()
	stub.panicOnCall = true
	worker := NewOrderExpiryWorker(stub, OrderExpiryConfig{Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()

	// panic 被 recover 后调度器必须继续存活：第二轮（间隔到点）仍然会被触发。
	waitForCalls(t, stub, 2, 2*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 未在 2 秒内返回（panic 兜底后调度器必须仍可正常退出）")
	}
}
