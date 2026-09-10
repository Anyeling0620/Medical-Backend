package schedule

import (
	"testing"
	"time"
)

// 本文件覆盖匿名公开域时段模型（internal/domain/schedule/public.go）：
// remaining 计算（含超卖夹紧）、业务时区（Asia/Shanghai）当天日期归一与日期布局常量。

// TestPublicScheduleRecalculate remaining = maximum - used，且不得为负数（超卖夹紧为 0）。
func TestPublicScheduleRecalculate(t *testing.T) {
	cases := []struct {
		name    string
		maximum int16
		used    int16
		want    int16
	}{
		{"正常相减", 3, 1, 2},
		{"全部用完", 3, 3, 0},
		{"used 超过 maximum 夹紧为 0", 3, 5, 0},
		{"maximum 为 0", 0, 0, 0},
		{"maximum 为 0 但已有挂号", 0, 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := PublicSchedule{Maximum: tc.maximum, Remaining: 99}
			item.Recalculate(tc.used)
			if item.Remaining != tc.want {
				t.Errorf("maximum=%d used=%d → remaining=%d, want %d", tc.maximum, tc.used, item.Remaining, tc.want)
			}
		})
	}
}

// TestPublicScheduleRecalculateNilReceiver 空指针接收者不应 panic（防御性分支）。
func TestPublicScheduleRecalculateNilReceiver(t *testing.T) {
	var item *PublicSchedule
	item.Recalculate(1)
}

// TestBusinessTodayShanghaiCrossDay UTC 2025-12-31 16:30（上海 2026-01-01 00:30）的业务当天是 2026-01-01。
func TestBusinessTodayShanghaiCrossDay(t *testing.T) {
	now := time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC)
	got := BusinessToday(now)
	want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("BusinessToday = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	// 返回的是 UTC 零点的 date-only，便于直接与数据库 date 列比较。
	if got.Location() != time.UTC || got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
		t.Errorf("BusinessToday 应为 UTC 零点 date-only：%s (%s)", got, got.Location())
	}
}

// TestBusinessTodayUsesShanghaiCalendar 业务当天按 Asia/Shanghai 日历日归一，
// 不由部署机时区（或传入时刻的时区）决定。
func TestBusinessTodayUsesShanghaiCalendar(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{"上海上午", time.Date(2026, 9, 9, 8, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60)), "2026-09-09"},
		{"UTC 晚间仍是上海次日", time.Date(2026, 9, 9, 23, 30, 0, 0, time.UTC), "2026-09-10"},
		{"UTC 早间是上海同日", time.Date(2026, 9, 9, 1, 0, 0, 0, time.UTC), "2026-09-09"},
		{"上海跨年前夜", time.Date(2025, 12, 31, 23, 59, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60)), "2025-12-31"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BusinessToday(tc.now).Format(DateLayout); got != tc.want {
				t.Errorf("BusinessToday = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestPublicDateLayout 公开域日期布局与数据库 date 列一致（YYYY-MM-DD）。
func TestPublicDateLayout(t *testing.T) {
	if DateLayout != "2006-01-02" {
		t.Errorf("DateLayout = %q, want %q", DateLayout, "2006-01-02")
	}
}
