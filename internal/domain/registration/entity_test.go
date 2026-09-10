package registration

import (
	"strings"
	"testing"
	"time"
)

// 本文件覆盖挂号领域（internal/domain/registration）的纯函数规则：
// 支付状态编码映射、时段是否已开始、时段级/计划级余量取值、业务日历日与外部交易号
// （spec/04-api-contract.md §6.1、§6.2、§6.8、§12.4）。
// 全部用例只做纯计算，不依赖 PostgreSQL、Redis 或 HTTP。

// registrationTestNow 是固定业务时刻：业务时区（UTC+8）2026-09-10 09:00。
var registrationTestNow = time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)

// TestPaymentStatusLabelMapsKnownCodes 数据库编码 1/2/3/4 必须映射为契约 §6.8 的
// 四个对外字符串语义（UNPAID、PAID、REFUNDED、EXPIRED）。
func TestPaymentStatusLabelMapsKnownCodes(t *testing.T) {
	cases := []struct {
		code int16
		want string
	}{
		{PaymentCodeUnpaid, PaymentStatusUnpaid},
		{PaymentCodePaid, PaymentStatusPaid},
		{PaymentCodeRefunded, PaymentStatusRefunded},
		{PaymentCodeExpired, PaymentStatusExpired},
	}
	for _, tc := range cases {
		if got := PaymentStatusLabel(tc.code); got != tc.want {
			t.Errorf("PaymentStatusLabel(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// TestPaymentStatusLabelConvergesUnknownCodes 未知编码与负数（脏数据或后续新增状态）
// 一律按 UNPAID 收敛：既不返回空字符串，也不把仍占用号源的状态展示成可再挂。
func TestPaymentStatusLabelConvergesUnknownCodes(t *testing.T) {
	for _, code := range []int16{0, -1, 5, 99} {
		if got := PaymentStatusLabel(code); got != PaymentStatusUnpaid {
			t.Errorf("PaymentStatusLabel(%d) = %q, want %q", code, got, PaymentStatusUnpaid)
		}
	}
}

// TestPaymentStatusCodeParsesWhitelist 查询参数 paymentStatus 只接受词表内的四个取值；
// 其他取值（如契约 §12.4 的 WAITING）返回 false，由请求层映射为 422「支付状态不支持」。
func TestPaymentStatusCodeParsesWhitelist(t *testing.T) {
	valid := map[string]int16{
		PaymentStatusUnpaid:   PaymentCodeUnpaid,
		PaymentStatusPaid:     PaymentCodePaid,
		PaymentStatusRefunded: PaymentCodeRefunded,
		PaymentStatusExpired:  PaymentCodeExpired,
	}
	for label, want := range valid {
		code, ok := PaymentStatusCode(label)
		if !ok || code != want {
			t.Errorf("PaymentStatusCode(%q) = (%d,%t), want (%d,true)", label, code, ok, want)
		}
	}

	for _, label := range []string{"WAITING", "", "unpaid", "已支付", "PAID "} {
		if code, ok := PaymentStatusCode(label); ok {
			t.Errorf("PaymentStatusCode(%q) = (%d,true), want (0,false)", label, code)
		}
	}
}

// TestPaymentStatusRoundTrip 编码与字符串语义必须互为逆运算，避免列表筛选与展示不一致。
func TestPaymentStatusRoundTrip(t *testing.T) {
	for _, code := range []int16{
		PaymentCodeUnpaid, PaymentCodePaid, PaymentCodeRefunded, PaymentCodeExpired,
	} {
		label := PaymentStatusLabel(code)
		back, ok := PaymentStatusCode(label)
		if !ok || back != code {
			t.Errorf("往返不一致：编码 %d -> %q -> (%d,%t)", code, label, back, ok)
		}
	}
}

// TestStarted 时段是否已开始的判定：当天与更早的日期都算已开始，未来日期可挂；
// 日期解析失败属于脏数据，向「不可挂号」一侧收敛（契约 §6.1 的 SCHEDULE_STARTED）。
func TestStarted(t *testing.T) {
	cases := []struct {
		name string
		date string
		want bool
	}{
		{"业务当天视为已开始", "2026-09-10", true},
		{"已过期日期视为已开始", "2026-09-01", true},
		{"昨天视为已开始", "2026-09-09", true},
		{"未来日期可挂", "2026-09-11", false},
		{"远期日期可挂", "2026-10-20", false},
		{"非 YYYY-MM-DD 脏日期收敛为已开始", "2026/09/20", true},
		{"空日期收敛为已开始", "", true},
		{"不存在的日期收敛为已开始", "2026-02-30", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Started(tc.date, registrationTestNow); got != tc.want {
				t.Errorf("Started(%q) = %t, want %t", tc.date, got, tc.want)
			}
		})
	}
}

// TestStartedUsesBusinessCalendarDay 日界必须按医院所在地（UTC+8）判定，
// 不能用 UTC 日界：UTC 夜间（业务次日）时，业务次日的时段已经算「已开始」。
func TestStartedUsesBusinessCalendarDay(t *testing.T) {
	// UTC 2026-09-10 16:30 已是业务时区 2026-09-11 00:30。
	lateUTC := time.Date(2026, 9, 10, 16, 30, 0, 0, time.UTC)
	if !Started("2026-09-11", lateUTC) {
		t.Error("业务次日凌晨时 2026-09-11 的时段必须算已开始（按 +08:00 日历日）")
	}
	// UTC 2026-09-10 15:59 仍是业务时区 2026-09-10 23:59。
	earlyUTC := time.Date(2026, 9, 10, 15, 59, 0, 0, time.UTC)
	if Started("2026-09-11", earlyUTC) {
		t.Error("业务当天 23:59 时次日时段仍应可挂")
	}
}

// TestScheduleSnapshotRemaining 余量取时段级与计划级的较小值（两级上限同时生效），
// 且任何负数都归零，避免出现「资格通过但建单被拒」。
func TestScheduleSnapshotRemaining(t *testing.T) {
	cases := []struct {
		name                  string
		slotMaximum, slotUsed int16
		planMaximum, planUsed int16
		want                  int16
	}{
		{"两级相同", 3, 1, 10, 8, 2},
		{"时段级更小", 3, 2, 10, 0, 1},
		{"计划级更小", 5, 1, 10, 9, 1},
		{"时段级超卖归零", 3, 5, 10, 0, 0},
		{"计划级超卖归零", 5, 1, 3, 5, 0},
		{"两级都超卖归零", 2, 6, 1, 9, 0},
		{"已满归零", 3, 3, 10, 0, 0},
		{"脏数据全为零归零", 0, 0, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := ScheduleSnapshot{
				SlotMaximum: tc.slotMaximum,
				SlotUsed:    tc.slotUsed,
				PlanMaximum: tc.planMaximum,
				PlanUsed:    tc.planUsed,
			}
			if got := snapshot.Remaining(); got != tc.want {
				t.Errorf("Remaining() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBusinessDateUsesHospitalTimeZone BusinessDate/BusinessToday 必须按 +08:00 归一到日历日，
// 与 medical_registration.date（date 列）比较时不会因 UTC 日界错位。
func TestBusinessDateUsesHospitalTimeZone(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{"UTC 凌晨等于业务当日", time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC), "2026-09-10"},
		{"UTC 15:59 仍是业务当日", time.Date(2026, 9, 10, 15, 59, 0, 0, time.UTC), "2026-09-10"},
		{"UTC 16:00 已是业务次日", time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC), "2026-09-11"},
		{"带偏移的时刻按业务时区换算", time.Date(2026, 9, 10, 20, 0, 0, 0, time.FixedZone("UTC+12", 12*3600)), "2026-09-10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BusinessDate(tc.now); got != tc.want {
				t.Errorf("BusinessDate(%s) = %q, want %q", tc.now, got, tc.want)
			}
			want, err := time.Parse("2006-01-02", tc.want)
			if err != nil {
				t.Fatalf("解析期望日期失败：%v", err)
			}
			if got := BusinessToday(tc.now); !got.Equal(want) {
				t.Errorf("BusinessToday(%s) = %s, want %s", tc.now, got, want)
			}
		})
	}
}

// TestNewOutTradeNoFormat 外部交易号格式固定为「14 位业务时间戳 + 12 位十六进制」，
// 长度 26 位不会超出 medical_registration.out_trade_no 的 CHAR(32) 列宽。
func TestNewOutTradeNoFormat(t *testing.T) {
	tradeNo := NewOutTradeNo(registrationTestNow)

	if len(tradeNo) != 26 {
		t.Fatalf("NewOutTradeNo 长度 = %d, want 26；值 %q", len(tradeNo), tradeNo)
	}
	if prefix := tradeNo[:14]; prefix != "20260910090000" {
		t.Errorf("时间戳前缀 = %q, want 20260910090000（业务时区 2026-09-10 09:00）", prefix)
	}
	for i, r := range tradeNo[14:] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("后缀第 %d 位 %q 不是小写十六进制字符：%q", i, r, tradeNo)
		}
	}
	if err := ValidateOutTradeNo(tradeNo); err != nil {
		t.Errorf("ValidateOutTradeNo(%q) = %v, want nil", tradeNo, err)
	}
}

// TestNewOutTradeNoUniqueWithinSameInstant 库中 out_trade_no 没有唯一约束，唯一性由服务端保证：
// 同一时刻（同一毫秒）并发建单也不能撞号。
func TestNewOutTradeNoUniqueWithinSameInstant(t *testing.T) {
	const count = 500
	seen := make(map[string]struct{}, count)
	for i := 0; i < count; i++ {
		tradeNo := NewOutTradeNo(registrationTestNow)
		if _, exists := seen[tradeNo]; exists {
			t.Fatalf("同一时刻生成了重复交易号：%q（第 %d 次）", tradeNo, i)
		}
		seen[tradeNo] = struct{}{}
		if len(tradeNo) > 32 {
			t.Fatalf("交易号超出 CHAR(32) 列宽：%q", tradeNo)
		}
	}
}

// TestNewOutTradeNoUsesBusinessTimeZone 时间戳必须取业务时区（UTC+8）的时分秒，
// 与 createDate 的日历日口径一致。
func TestNewOutTradeNoUsesBusinessTimeZone(t *testing.T) {
	// UTC 2026-09-10 16:30:15 是业务时区 2026-09-11 00:30:15。
	now := time.Date(2026, 9, 10, 16, 30, 15, 0, time.UTC)
	if got := NewOutTradeNo(now)[:14]; got != "20260911003015" {
		t.Errorf("时间戳前缀 = %q, want 20260911003015", got)
	}
}

// TestValidateOutTradeNo ValidateOutTradeNo 只在「去掉首尾空白后非空且不超过 32 字符」时通过。
func TestValidateOutTradeNo(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"空字符串", "", true},
		{"纯空白", "    ", true},
		{"26 位交易号", strings.Repeat("a", 26), false},
		{"恰好 32 位", strings.Repeat("a", 32), false},
		{"33 位超列宽", strings.Repeat("a", 33), true},
		{"首尾空白不算长度", "  " + strings.Repeat("a", 32) + "  ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOutTradeNo(tc.value)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateOutTradeNo(%q) = nil, want error", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateOutTradeNo(%q) = %v, want nil", tc.value, err)
			}
		})
	}
}
