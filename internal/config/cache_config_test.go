package config

import (
	"math"
	"testing"
	"time"
)

// 本文件覆盖公开域读缓存的配置收敛规则（CacheConfig.Normalize），不依赖环境变量。
// 关注点：排班余量必须新鲜，所以「实际陈旧窗口（含抖动）不超过 MaxScheduleCacheTTL」
// 这句承诺必须在配置层就成立，而不是只在缺省配置下成立。

// TestCacheConfigNormalizeZeroValue 零值配置必须收敛到可直接使用的安全值：
// TTL 与 Redis 超时缺失时回落到缺省值。
//
// 注意抖动是例外：0 是「显式关闭抖动」的合法取值（区间 [0, 0.5] 的下界），
// Normalize 不会把它改成缺省 0.2；生产环境的缺省 0.2 由 envDefault 标签在解析 env 时提供。
// 本用例把这个口径固定下来，避免后人误以为 Normalize 会把 0 抖动静默改成 0.2。
func TestCacheConfigNormalizeZeroValue(t *testing.T) {
	got := CacheConfig{}.Normalize()

	if got.ScheduleTTL != DefaultScheduleCacheTTL {
		t.Errorf("ScheduleTTL = %v, want %v", got.ScheduleTTL, DefaultScheduleCacheTTL)
	}
	if got.ScheduleJitter != 0 {
		t.Errorf("ScheduleJitter = %v, want 0（0 表示显式关闭抖动，缺省 0.2 由 envDefault 提供）", got.ScheduleJitter)
	}
	if got.RedisTimeout != DefaultCacheRedisTimeout {
		t.Errorf("RedisTimeout = %v, want %v", got.RedisTimeout, DefaultCacheRedisTimeout)
	}

	// 显式配置的抖动必须原样保留。
	configured := CacheConfig{ScheduleTTL: 8 * time.Second, ScheduleJitter: DefaultCacheJitterRatio}.Normalize()
	if configured.ScheduleJitter != DefaultCacheJitterRatio {
		t.Errorf("ScheduleJitter = %v, want %v", configured.ScheduleJitter, DefaultCacheJitterRatio)
	}
}

// TestCacheConfigNormalizeClampsOutOfRange 越界取值必须收敛回缺省值：
// TTL 落到 [1s, 10s] 之外、抖动落到 [0, 0.5] 之外、Redis 超时为非正或过大。
func TestCacheConfigNormalizeClampsOutOfRange(t *testing.T) {
	cases := []struct {
		name       string
		input      CacheConfig
		wantTTL    time.Duration
		wantJitter float64
		wantRedis  time.Duration
	}{
		{
			name:       "TTL 为 0（未配置）",
			input:      CacheConfig{ScheduleTTL: 0, ScheduleJitter: 0.2, RedisTimeout: 100 * time.Millisecond},
			wantTTL:    DefaultScheduleCacheTTL,
			wantJitter: 0.2,
			wantRedis:  100 * time.Millisecond,
		},
		{
			name:       "TTL 超过硬上限 10 秒",
			input:      CacheConfig{ScheduleTTL: 30 * time.Second, ScheduleJitter: 0.2, RedisTimeout: 100 * time.Millisecond},
			wantTTL:    DefaultScheduleCacheTTL,
			wantJitter: 0.2,
			wantRedis:  100 * time.Millisecond,
		},
		{
			name:       "TTL 低于下限 1 秒",
			input:      CacheConfig{ScheduleTTL: 100 * time.Millisecond, ScheduleJitter: 0.2, RedisTimeout: 100 * time.Millisecond},
			wantTTL:    DefaultScheduleCacheTTL,
			wantJitter: 0.2,
			wantRedis:  100 * time.Millisecond,
		},
		{
			name:       "抖动为负",
			input:      CacheConfig{ScheduleTTL: 8 * time.Second, ScheduleJitter: -1, RedisTimeout: 100 * time.Millisecond},
			wantTTL:    8 * time.Second,
			wantJitter: DefaultCacheJitterRatio,
			wantRedis:  100 * time.Millisecond,
		},
		{
			name:       "抖动超过上限 0.5",
			input:      CacheConfig{ScheduleTTL: 8 * time.Second, ScheduleJitter: 0.9, RedisTimeout: 100 * time.Millisecond},
			wantTTL:    8 * time.Second,
			wantJitter: DefaultCacheJitterRatio,
			wantRedis:  100 * time.Millisecond,
		},
		{
			name:       "Redis 超时为 0",
			input:      CacheConfig{ScheduleTTL: 8 * time.Second, ScheduleJitter: 0.2, RedisTimeout: 0},
			wantTTL:    8 * time.Second,
			wantJitter: 0.2,
			wantRedis:  DefaultCacheRedisTimeout,
		},
		{
			name:       "Redis 超时超过上限 1 秒",
			input:      CacheConfig{ScheduleTTL: 8 * time.Second, ScheduleJitter: 0.2, RedisTimeout: 5 * time.Second},
			wantTTL:    8 * time.Second,
			wantJitter: 0.2,
			wantRedis:  DefaultCacheRedisTimeout,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.input.Normalize()
			if got.ScheduleTTL != tc.wantTTL {
				t.Errorf("ScheduleTTL = %v, want %v", got.ScheduleTTL, tc.wantTTL)
			}
			if got.ScheduleJitter != tc.wantJitter {
				t.Errorf("ScheduleJitter = %v, want %v", got.ScheduleJitter, tc.wantJitter)
			}
			if got.RedisTimeout != tc.wantRedis {
				t.Errorf("RedisTimeout = %v, want %v", got.RedisTimeout, tc.wantRedis)
			}
		})
	}
}

// TestCacheConfigNormalizeCapsBaseByJitter 覆盖「按抖动比例反推基准上限」：
// 抖动会把实际 TTL 拉到 base*(1+jitter)，因此基准 TTL 的上限必须是
// MaxScheduleCacheTTL/(1+jitter)，否则基准 10s + 抖动 20% 会写出 12s 的键，
// 「陈旧窗口不超过 10 秒」的承诺就被配置本身破坏。
func TestCacheConfigNormalizeCapsBaseByJitter(t *testing.T) {
	// maxBase 按抖动比例反推基准上限 10s/(1+jitter)，向下取整到纳秒（与 Normalize 同口径）。
	maxBase := func(jitter float64) time.Duration {
		return time.Duration(float64(MaxScheduleCacheTTL) / (1 + jitter))
	}

	cases := []struct {
		name   string
		jitter float64
		input  time.Duration
		// wantBase 是期望的基准上限。
		wantBase time.Duration
	}{
		{"抖动 0.2 时基准上限约 8.33s", 0.2, MaxScheduleCacheTTL, maxBase(0.2)},
		{"抖动 0.5 时基准上限约 6.67s", 0.5, MaxScheduleCacheTTL, maxBase(0.5)},
		{"抖动 0 时基准上限就是 10s", 0, MaxScheduleCacheTTL, MaxScheduleCacheTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CacheConfig{ScheduleTTL: tc.input, ScheduleJitter: tc.jitter}.Normalize()

			if got.ScheduleTTL != tc.wantBase {
				t.Errorf("ScheduleTTL = %v, want %v（按抖动反推的基准上限）", got.ScheduleTTL, tc.wantBase)
			}
			if got.ScheduleTTL >= tc.input && tc.jitter > 0 {
				t.Errorf("基准 TTL = %v 未被收敛，输入 %v 应被压到 %v 以内", got.ScheduleTTL, tc.input, tc.wantBase)
			}
			// 核心不变量：含抖动的最大实际 TTL 不得超过硬上限。
			maxActual := time.Duration(float64(got.ScheduleTTL) * (1 + got.ScheduleJitter))
			if maxActual > MaxScheduleCacheTTL {
				t.Errorf("含抖动的最大实际 TTL = %v 超过硬上限 %v（基准=%v 抖动=%v）",
					maxActual, MaxScheduleCacheTTL, got.ScheduleTTL, got.ScheduleJitter)
			}
		})
	}

	// 正常缺省配置（8s + 20% = 9.6s）必须原样保留，不能被上限规则误伤。
	got := CacheConfig{ScheduleTTL: 8 * time.Second, ScheduleJitter: 0.2}.Normalize()
	if got.ScheduleTTL != 8*time.Second {
		t.Errorf("缺省 8s 基准不应被收敛，实际 %v", got.ScheduleTTL)
	}
}

// TestCacheConfigNormalizeNaNJitter NaN 抖动必须被单独拦截：
// NaN 与任何数比较都是 false，会绕过「< 0 || > MaxCacheJitterRatio」的区间校验，
// 随后 MaxScheduleCacheTTL/(1+NaN) 会把基准 TTL 压成负数（曾导致缓存被静默关闭）。
func TestCacheConfigNormalizeNaNJitter(t *testing.T) {
	got := CacheConfig{ScheduleTTL: MaxScheduleCacheTTL, ScheduleJitter: math.NaN()}.Normalize()

	if math.IsNaN(got.ScheduleJitter) {
		t.Fatalf("NaN 抖动必须被回落为缺省值，实际仍是 NaN")
	}
	if got.ScheduleJitter != DefaultCacheJitterRatio {
		t.Errorf("ScheduleJitter = %v, want %v", got.ScheduleJitter, DefaultCacheJitterRatio)
	}
	if got.ScheduleTTL < MinScheduleCacheTTL || got.ScheduleTTL > MaxScheduleCacheTTL {
		t.Errorf("ScheduleTTL = %v 越界，必须在 [%v, %v] 内", got.ScheduleTTL, MinScheduleCacheTTL, MaxScheduleCacheTTL)
	}
	// 基准被按抖动反推的上限收敛：10s/(1+0.2) ≈ 8.33s。
	jitter := got.ScheduleJitter
	if wantBase := time.Duration(float64(MaxScheduleCacheTTL) / (1 + jitter)); got.ScheduleTTL != wantBase {
		t.Errorf("ScheduleTTL = %v, want %v（按缺省抖动反推的基准上限）", got.ScheduleTTL, wantBase)
	}
	maxActual := time.Duration(float64(got.ScheduleTTL) * (1 + got.ScheduleJitter))
	if maxActual > MaxScheduleCacheTTL {
		t.Errorf("含抖动的最大实际 TTL = %v 超过硬上限 %v", maxActual, MaxScheduleCacheTTL)
	}
}

// TestCacheConfigNormalizePostConditions 用极端取值表锁定 Normalize 的三条后置条件：
//  1. ScheduleJitter ∈ [0, MaxCacheJitterRatio]（含 NaN / ±Inf 这类会绕过比较的取值）；
//  2. ScheduleTTL ∈ [MinScheduleCacheTTL, MaxScheduleCacheTTL]（新增的兜底保证它恒成立）；
//  3. 含抖动的最大实际 TTL = TTL*(1+jitter) 不超过 MaxScheduleCacheTTL。
func TestCacheConfigNormalizePostConditions(t *testing.T) {
	jitters := []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, -0.0001, 0, 0.2, 0.5, 0.5000001, 2}
	ttls := []time.Duration{0, -time.Second, time.Millisecond, MinScheduleCacheTTL, 8 * time.Second, MaxScheduleCacheTTL, time.Minute}
	for _, jitter := range jitters {
		for _, ttl := range ttls {
			got := CacheConfig{ScheduleTTL: ttl, ScheduleJitter: jitter}.Normalize()

			if math.IsNaN(got.ScheduleJitter) || got.ScheduleJitter < 0 || got.ScheduleJitter > MaxCacheJitterRatio {
				t.Errorf("jitter=%v ttl=%v：ScheduleJitter = %v 越界", jitter, ttl, got.ScheduleJitter)
			}
			if got.ScheduleTTL < MinScheduleCacheTTL || got.ScheduleTTL > MaxScheduleCacheTTL {
				t.Errorf("jitter=%v ttl=%v：ScheduleTTL = %v 越界，必须在 [%v, %v] 内",
					jitter, ttl, got.ScheduleTTL, MinScheduleCacheTTL, MaxScheduleCacheTTL)
			}
			maxActual := time.Duration(float64(got.ScheduleTTL) * (1 + got.ScheduleJitter))
			if maxActual > MaxScheduleCacheTTL {
				t.Errorf("jitter=%v ttl=%v：含抖动的最大实际 TTL = %v 超过硬上限 %v",
					jitter, ttl, maxActual, MaxScheduleCacheTTL)
			}
		}
	}
}
