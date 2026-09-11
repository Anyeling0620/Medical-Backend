// 支付响应投影单测：PaymentReadResponse 是取支付参数（§6.5）与创建支付订单（§6.9）共用的
// 响应 DTO，本文件锁定它的 payable 守卫与 qrCode 可见性。
//
// 这里直接断言投影函数，从而覆盖 §6.9 在执行路径上无法构造出的状态：窗口仍在但 prepay_id 为空。
// CreateOrder 遇到该状态会先补窗口并预下单，因此该组合只可能落在投影层，必须在投影层保证
// payable=false 且不输出 qrCode（契约 §6.5 规定 payable=true 时响应必须含 qrCode）。
package response

import (
	"testing"
	"time"

	domainpayment "Medical-Web-Backend/internal/domain/payment"
)

// paymentResponseTestNow 是固定基准时刻（UTC），与其它包固定时钟口径一致。
var paymentResponseTestNow = time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)

// paymentResponseTestPayment 构造窗口内的 UNPAID 订单，prepay_id 由参数决定。
func paymentResponseTestPayment(prepayID string) domainpayment.Payment {
	return domainpayment.Payment{
		RegistrationID: 1001,
		OutTradeNo:     "202609080001",
		Amount:         "80.00",
		PaymentStatus:  domainpayment.PaymentStatusUnpaid,
		PrepayID:       prepayID,
		PrecreateAt:    paymentResponseTestNow,
		PayDeadline:    paymentResponseTestNow.Add(30 * time.Minute),
		ExpireAt:       paymentResponseTestNow.Add(35 * time.Minute),
	}
}

// TestNewPaymentReadResponsePayableRequiresQRCode 投影层不得输出 payable=true 却无 qrCode 的
// 矛盾响应：只有二维码确实存在（去空白后非空）时 payable 才为 true（契约 §6.5、§6.9）。
func TestNewPaymentReadResponsePayableRequiresQRCode(t *testing.T) {
	const qrCode = "https://qr.alipay.com/bax0123456789"

	cases := []struct {
		name        string
		prepayID    string
		payable     bool
		wantPayable bool
		wantQRCode  string
	}{
		{"窗口存在但二维码为空：payable 强制为 false", "", true, false, ""},
		{"窗口存在但二维码只有空白：payable 强制为 false", "   ", true, false, ""},
		{"窗口存在且二维码已落库：保持 payable=true", qrCode, true, true, qrCode},
		{"已过 pay_deadline：保持 payable=false", qrCode, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := NewPaymentReadResponse(paymentResponseTestPayment(tc.prepayID), tc.payable)
			if result.Payable != tc.wantPayable {
				t.Errorf("payable = %v, want %v", result.Payable, tc.wantPayable)
			}
			if result.QRCode != tc.wantQRCode {
				t.Errorf("qrCode = %q, want %q", result.QRCode, tc.wantQRCode)
			}
			// 两个时间点始终按 RFC3339、UTC 输出，与 payable 与否无关。
			if result.PayableUntil != "2026-09-10T01:30:00Z" {
				t.Errorf("payableUntil = %q, want 2026-09-10T01:30:00Z", result.PayableUntil)
			}
			if result.ValidUntil != "2026-09-10T01:35:00Z" {
				t.Errorf("validUntil = %q, want 2026-09-10T01:35:00Z", result.ValidUntil)
			}
		})
	}
}

// TestNewPaymentReadResponseZeroWindowTimes 历史数据缺少时间点时，两个时间字段输出空串，
// 而不是零值时间文本（契约 §6.8 的序列化口径）。
func TestNewPaymentReadResponseZeroWindowTimes(t *testing.T) {
	item := paymentResponseTestPayment("")
	item.PayDeadline = time.Time{}
	item.ExpireAt = time.Time{}

	result := NewPaymentReadResponse(item, false)
	if result.PayableUntil != "" || result.ValidUntil != "" {
		t.Errorf("零值时间应输出空串：payableUntil=%q validUntil=%q", result.PayableUntil, result.ValidUntil)
	}
}
