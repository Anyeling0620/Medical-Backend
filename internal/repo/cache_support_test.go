package repo

import (
	"context"
	"testing"
	"time"
)

// 本文件是公开域读缓存共享基础设施的纯函数测试（不连 Redis、不连数据库）：
// TTL 抖动区间（验收点 g）与单次 Redis 操作超时的兜底默认值。

// TestJitteredTTLWithinRange 覆盖验收点 g：TTL 抖动必须落在
// [base*(1-jitter), base*(1+jitter)] 区间内。randFloat 由测试注入，因此断言是确定性的；
// 生产代码注入的是 math/rand/v2 的 Float64（见 NewScheduleCache）。
func TestJitteredTTLWithinRange(t *testing.T) {
	const (
		base   = 8 * time.Second
		jitter = 0.2
	)
	low := time.Duration(float64(base) * (1 - jitter))
	high := time.Duration(float64(base) * (1 + jitter))

	cases := []struct {
		name string
		rand float64
		want time.Duration
	}{
		{"随机源取下界", 0, low},
		{"随机源取中点", 0.5, base},
		{"随机源取上界", 1, high},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jitteredTTL(base, jitter, func() float64 { return tc.rand })
			if got < low || got > high {
				t.Fatalf("抖动 TTL = %v 越界，应落在 [%v, %v]", got, low, high)
			}
			// 浮点乘法存在 1ns 量级的舍入误差，这里给 ±1ns 容差。
			if diff := got - tc.want; diff > time.Nanosecond || diff < -time.Nanosecond {
				t.Errorf("抖动 TTL = %v, want 约 %v", got, tc.want)
			}
		})
	}

	// 连续扫描随机源，确认任意取值都不会越界（区间边界由上面的用例锁定）。
	for step := 0; step <= 10; step++ {
		value := float64(step) / 10
		got := jitteredTTL(base, jitter, func() float64 { return value })
		if got < low || got > high {
			t.Errorf("随机源 %v 时抖动 TTL = %v 越界，应落在 [%v, %v]", value, got, low, high)
		}
	}
}

// TestJitteredTTLFallbacks 覆盖抖动关闭、随机源缺失与非法基准 TTL 的兜底：
// 不做抖动的场景必须原样返回基准 TTL；非法基准（<= 0）返回 0，由调用方（store）据此跳过回填——
// Redis 的 0 表示永不过期，余量缓存绝不能写永不过期键。
func TestJitteredTTLFallbacks(t *testing.T) {
	const base = 8 * time.Second
	half := func() float64 { return 0.5 }
	cases := []struct {
		name   string
		base   time.Duration
		jitter float64
		rand   func() float64
		want   time.Duration
	}{
		{"抖动为 0 时不抖动", base, 0, half, base},
		{"抖动为负时不抖动", base, -0.5, half, base},
		{"随机源缺失时不抖动", base, 0.2, nil, base},
		{"基准 TTL 为 0 时返回 0", 0, 0.2, half, 0},
		{"基准 TTL 为负时返回 0", -time.Second, 0.2, half, 0},
		// 抖动比例大于 1 时收敛到 1：下界会被拉到 0，实现保底 1ms，避免写出「立即过期」的键。
		{"抖动比例大于 1 时下界保底 1ms", base, 2, func() float64 { return 0 }, time.Millisecond},
		{"抖动比例大于 1 时上界为 2 倍基准", base, 2, func() float64 { return 1 }, 2 * base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jitteredTTL(tc.base, tc.jitter, tc.rand); got != tc.want {
				t.Errorf("jitteredTTL(%v, %v) = %v, want %v", tc.base, tc.jitter, got, tc.want)
			}
		})
	}
}

// TestCacheContextTimeout 覆盖单次缓存 Redis 操作的超时：
// 非正超时回落到 1 秒兜底，正超时被保留，父 context 的取消仍向后传递。
func TestCacheContextTimeout(t *testing.T) {
	t.Run("非正超时回落到缺省值（100ms）", func(t *testing.T) {
		// 缺省值必须与 config.DefaultCacheRedisTimeout 同口径：慢 Redis 不得把业务请求一起拖慢。
		// 这里同时给上界（不得超过缺省）与下界（不得明显小于缺省，避免退化成「立即超时」）。
		for _, timeout := range []time.Duration{0, -time.Second} {
			ctx, cancel := cacheContext(context.Background(), timeout)
			deadline, ok := ctx.Deadline()
			if !ok {
				cancel()
				t.Fatalf("入参超时 %v：context 必须带超时 deadline", timeout)
			}
			remain := time.Until(deadline)
			if remain > defaultCacheRedisTimeout || remain <= defaultCacheRedisTimeout/2 {
				t.Errorf("入参超时 %v：剩余超时 = %v, want (%v, %v]（缺省值应与 config.DefaultCacheRedisTimeout 同口径）",
					timeout, remain, defaultCacheRedisTimeout/2, defaultCacheRedisTimeout)
			}
			cancel()
		}
	})

	t.Run("正超时被保留", func(t *testing.T) {
		ctx, cancel := cacheContext(context.Background(), 50*time.Millisecond)
		defer cancel()
		assertContextDeadlineWithin(t, ctx, 50*time.Millisecond)
	})

	t.Run("父 context 的取消向后传递", func(t *testing.T) {
		parent, parentCancel := context.WithCancel(context.Background())
		ctx, cancel := cacheContext(parent, time.Minute)
		defer cancel()
		parentCancel()
		if ctx.Err() == nil {
			t.Fatalf("父 context 取消后子 context 必须一并取消")
		}
	})
}

// assertContextDeadlineWithin 断言 ctx 带超时且剩余时间不超过 want。
func assertContextDeadlineWithin(t *testing.T, ctx context.Context, want time.Duration) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("context 必须带超时 deadline")
	}
	remain := time.Until(deadline)
	if remain <= 0 || remain > want {
		t.Errorf("剩余超时 = %v, want (0, %v]", remain, want)
	}
}
