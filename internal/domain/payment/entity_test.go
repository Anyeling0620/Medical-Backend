// 支付领域单测：可支付/可查询窗口、UNPAID -> PAID 迁移前置条件、终态判定、
// 交易状态白名单与金额等价比较（spec/04-api-contract.md §6.5-§6.8、§9、§12.4）。
//
// 全部用例只做纯计算，不依赖 PostgreSQL、Redis、支付宝或 HTTP；
// 时间边界统一用「恰好相等」与「晚 1 纳秒」两种写法锁定 now() <= 边界 的闭区间口径。
package payment

import (
	"testing"
	"time"
)

// paymentTestNow 是固定业务时刻：业务时区（UTC+8）2026-09-10 09:00，即 UTC 01:00。
var paymentTestNow = time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)

// TestPayableBoundaries 覆盖 Payable：仅 UNPAID 且 now() <= pay_deadline 时可支付
// （契约 §6.5 的 30 分钟窗口）。
func TestPayableBoundaries(t *testing.T) {
	deadline := paymentTestNow.Add(30 * time.Minute)
	cases := []struct {
		name string
		item Payment
		now  time.Time
		want bool
	}{
		{
			name: "UNPAID 且 now 早于 pay_deadline：可支付",
			item: Payment{PaymentStatus: PaymentStatusUnpaid, PayDeadline: deadline},
			now:  deadline.Add(-time.Minute),
			want: true,
		},
		{
			name: "UNPAID 且 now 恰好等于 pay_deadline：闭区间仍算可支付",
			item: Payment{PaymentStatus: PaymentStatusUnpaid, PayDeadline: deadline},
			now:  deadline,
			want: true,
		},
		{
			name: "UNPAID 且 now 晚 pay_deadline 一纳秒：进入 30~35 分钟窗口，不可支付",
			item: Payment{PaymentStatus: PaymentStatusUnpaid, PayDeadline: deadline},
			now:  deadline.Add(time.Nanosecond),
			want: false,
		},
		{
			name: "PAID 即使在窗口内也不可支付：已支付订单不再展示二维码",
			item: Payment{PaymentStatus: PaymentStatusPaid, PayDeadline: deadline},
			now:  deadline.Add(-time.Minute),
			want: false,
		},
		{
			name: "EXPIRED 不可支付",
			item: Payment{PaymentStatus: PaymentStatusExpired, PayDeadline: deadline},
			now:  deadline.Add(-time.Minute),
			want: false,
		},
		{
			name: "REFUNDED 不可支付",
			item: Payment{PaymentStatus: PaymentStatusRefunded, PayDeadline: deadline},
			now:  deadline.Add(-time.Minute),
			want: false,
		},
		{
			name: "pay_deadline 为零值（历史脏数据）：按不可支付收敛",
			item: Payment{PaymentStatus: PaymentStatusUnpaid},
			now:  paymentTestNow,
			want: false,
		},
		{
			name: "未知状态：按不可支付收敛",
			item: Payment{PaymentStatus: "WAITING", PayDeadline: deadline},
			now:  paymentTestNow,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.Payable(tc.now); got != tc.want {
				t.Fatalf("Payable(%s) = %v，期望 %v", tc.now.Format(time.RFC3339Nano), got, tc.want)
			}
		})
	}
}

// TestQueryableBoundaries 覆盖 Queryable：仅 UNPAID 且 now() <= expire_at 时才做
// 兜底主动查询（契约 §6.6）。
func TestQueryableBoundaries(t *testing.T) {
	expireAt := paymentTestNow.Add(35 * time.Minute)
	cases := []struct {
		name string
		item Payment
		now  time.Time
		want bool
	}{
		{
			name: "UNPAID 且未超过 expire_at：可主动查询",
			item: Payment{PaymentStatus: PaymentStatusUnpaid, ExpireAt: expireAt},
			now:  expireAt.Add(-time.Minute),
			want: true,
		},
		{
			name: "UNPAID 且 now 恰好等于 expire_at：闭区间仍可查询",
			item: Payment{PaymentStatus: PaymentStatusUnpaid, ExpireAt: expireAt},
			now:  expireAt,
			want: true,
		},
		{
			name: "UNPAID 且 now 晚 expire_at 一纳秒：不做主动查询，交给收口任务",
			item: Payment{PaymentStatus: PaymentStatusUnpaid, ExpireAt: expireAt},
			now:  expireAt.Add(time.Nanosecond),
			want: false,
		},
		{
			name: "PAID 不再查询",
			item: Payment{PaymentStatus: PaymentStatusPaid, ExpireAt: expireAt},
			now:  expireAt.Add(-time.Minute),
			want: false,
		},
		{
			name: "EXPIRED 不再查询",
			item: Payment{PaymentStatus: PaymentStatusExpired, ExpireAt: expireAt},
			now:  expireAt.Add(-time.Minute),
			want: false,
		},
		{
			name: "REFUNDED 不再查询",
			item: Payment{PaymentStatus: PaymentStatusRefunded, ExpireAt: expireAt},
			now:  expireAt.Add(-time.Minute),
			want: false,
		},
		{
			name: "expire_at 为零值（历史脏数据）：按不可查询收敛",
			item: Payment{PaymentStatus: PaymentStatusUnpaid},
			now:  paymentTestNow,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.Queryable(tc.now); got != tc.want {
				t.Fatalf("Queryable(%s) = %v，期望 %v", tc.now.Format(time.RFC3339Nano), got, tc.want)
			}
		})
	}
}

// TestCanMarkPaidBoundaries 覆盖 CanMarkPaid：通知/主动查询路径只允许
// UNPAID 且 now() <= expire_at 的订单迁移为 PAID，超时的成功通知必须丢弃
// （契约 §6.7、§6.8）。
func TestCanMarkPaidBoundaries(t *testing.T) {
	expireAt := paymentTestNow.Add(35 * time.Minute)
	cases := []struct {
		name string
		item Payment
		now  time.Time
		want bool
	}{
		{
			name: "UNPAID 且未超过 expire_at：允许迁移",
			item: Payment{PaymentStatus: PaymentStatusUnpaid, ExpireAt: expireAt},
			now:  expireAt.Add(-time.Nanosecond),
			want: true,
		},
		{
			name: "UNPAID 且 now 恰好等于 expire_at：闭区间允许迁移",
			item: Payment{PaymentStatus: PaymentStatusUnpaid, ExpireAt: expireAt},
			now:  expireAt,
			want: true,
		},
		{
			name: "UNPAID 且 now 晚 expire_at 一纳秒：迟到通知，禁止迁移",
			item: Payment{PaymentStatus: PaymentStatusUnpaid, ExpireAt: expireAt},
			now:  expireAt.Add(time.Nanosecond),
			want: false,
		},
		{
			name: "PAID 已迁移：禁止重复记账",
			item: Payment{PaymentStatus: PaymentStatusPaid, ExpireAt: expireAt},
			now:  expireAt.Add(-time.Minute),
			want: false,
		},
		{
			name: "EXPIRED 终态：禁止迁移",
			item: Payment{PaymentStatus: PaymentStatusExpired, ExpireAt: expireAt},
			now:  expireAt.Add(-time.Minute),
			want: false,
		},
		{
			name: "REFUNDED 终态：禁止迁移",
			item: Payment{PaymentStatus: PaymentStatusRefunded, ExpireAt: expireAt},
			now:  expireAt.Add(-time.Minute),
			want: false,
		},
		{
			name: "expire_at 为零值：按禁止迁移收敛",
			item: Payment{PaymentStatus: PaymentStatusUnpaid},
			now:  paymentTestNow,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.CanMarkPaid(tc.now); got != tc.want {
				t.Fatalf("CanMarkPaid(%s) = %v，期望 %v", tc.now.Format(time.RFC3339Nano), got, tc.want)
			}
		})
	}
}

// TestIsFinal 覆盖终态判定：PAID/EXPIRED/REFUNDED 为终态，UNPAID 与未知状态不是。
func TestIsFinal(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{PaymentStatusUnpaid, false},
		{PaymentStatusPaid, true},
		{PaymentStatusExpired, true},
		{PaymentStatusRefunded, true},
		{"", false},
		{"WAITING", false},
	}
	for _, tc := range cases {
		item := Payment{PaymentStatus: tc.status}
		if got := item.IsFinal(); got != tc.want {
			t.Errorf("Payment{PaymentStatus: %q}.IsFinal() = %v，期望 %v", tc.status, got, tc.want)
		}
	}
}

// TestSuccessTradeStatus 覆盖支付成功证据的判定：TRADE_SUCCESS 与 TRADE_FINISHED
// 都表示用户已完成支付，其余状态（含未知取值）都不构成迁移证据（契约 §6.6、§6.7）。
func TestSuccessTradeStatus(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{TradeStatusSuccess, true},
		{TradeStatusFinished, true},
		{TradeStatusWaitBuyerPay, false},
		{TradeStatusClosed, false},
		{"", false},
		{"trade_success", false},
		{"TRADE_UNKNOWN", false},
	}
	for _, tc := range cases {
		if got := SuccessTradeStatus(tc.status); got != tc.want {
			t.Errorf("SuccessTradeStatus(%q) = %v，期望 %v", tc.status, got, tc.want)
		}
	}
}

// TestAmountEquals 覆盖金额等价比较：
// 等价写法（"80.0" 与 "80.00"）必须判等，非法或不等金额必须判不等；
// 比较走精确有理数，不能受浮点误差或字符串长度影响（契约 §6.7 第 4 步）。
func TestAmountEquals(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "完全相同", a: "80.00", b: "80.00", want: true},
		{name: "小数位不同但等值", a: "80.0", b: "80.00", want: true},
		{name: "整数与两位小数等值", a: "80", b: "80.00", want: true},
		{name: "补零位数更多仍等值", a: "80.000", b: "80.00", want: true},
		{name: "两侧带空白按等值处理", a: " 80.00 ", b: "80.00", want: true},
		{name: "分位不同判不等", a: "80.01", b: "80.00", want: false},
		{name: "方向相反同样判不等", a: "80.00", b: "80.01", want: false},
		{name: "符号不同判不等", a: "-80.00", b: "80.00", want: false},
		{
			name: "超出浮点精度的小尾差仍判不等（精确有理数比较）",
			a:    "80.0000000000000000000001",
			b:    "80",
			want: false,
		},
		{name: "左侧非法金额判不等", a: "abc", b: "80.00", want: false},
		{name: "右侧非法金额判不等", a: "80.00", b: "8O.00", want: false},
		{name: "多个小数点判不等", a: "80.00.0", b: "80.00", want: false},
		{name: "左侧空串判不等", a: "", b: "80.00", want: false},
		{name: "两侧都是空白判不等", a: " ", b: " ", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AmountEquals(tc.a, tc.b); got != tc.want {
				t.Fatalf("AmountEquals(%q, %q) = %v，期望 %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
