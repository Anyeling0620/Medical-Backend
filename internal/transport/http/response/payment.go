package response

import (
	"time"

	domainpayment "Medical-Web-Backend/internal/domain/payment"
)

// 本文件是支付域（/api/v1/payments*）的响应 DTO（契约 §6.5、§6.6、§12.4）。
//
// 显式定义而不直接序列化领域实体：契约对不同接口规定了不同的字段集合
// （取支付参数返回二维码与两个时间点，查询支付状态返回金额与交易号），
// 字段集合本身是契约的一部分。

// PaymentReadResponse 是 POST /api/v1/payments 的 200 响应（契约 §6.5、§12.4）。
//
// payable = false 时响应中不得出现 qrCode 字段（不是空字符串，契约 §6.5），
// 因此该字段用 omitempty，并由构造函数保证不可支付时不赋值。
type PaymentReadResponse struct {
	OutTradeNo     string `json:"outTradeNo"`
	RegistrationID int64  `json:"registrationId"`
	PaymentStatus  string `json:"paymentStatus"`
	Payable        bool   `json:"payable"`
	QRCode         string `json:"qrCode,omitempty"`
	PayableUntil   string `json:"payableUntil"`
	ValidUntil     string `json:"validUntil"`
}

// NewPaymentReadResponse 把支付信息投影为取支付参数响应。
// 二维码只在可支付时回显：已支付、已过期与 30~35 分钟窗口都不返回二维码。
func NewPaymentReadResponse(item domainpayment.Payment, payable bool) PaymentReadResponse {
	result := PaymentReadResponse{
		OutTradeNo:     item.OutTradeNo,
		RegistrationID: item.RegistrationID,
		PaymentStatus:  item.PaymentStatus,
		Payable:        payable,
		PayableUntil:   formatPaymentTime(item.PayDeadline),
		ValidUntil:     formatPaymentTime(item.ExpireAt),
	}
	if payable {
		result.QRCode = item.PrepayID
	}
	return result
}

// PaymentDetailResponse 是 GET /api/v1/payments/{outTradeNo} 的 200 响应（契约 §6.6、§12.4）。
//
// transactionId 未支付时为 null，paidAt 同理。
//
// 已知取舍：paid_at 恒为 null。medical_registration 没有支付完成时刻列，
// 新增列必须先更新 spec/创建订单与支付业务说明.md 第 10 节再实施，
// 因此本切片只保留契约字段形状，不擅自变更数据库结构。
type PaymentDetailResponse struct {
	OutTradeNo     string  `json:"outTradeNo"`
	RegistrationID int64   `json:"registrationId"`
	Amount         string  `json:"amount"`
	PaymentStatus  string  `json:"paymentStatus"`
	TransactionID  *string `json:"transactionId"`
	PaidAt         *string `json:"paidAt"`
}

// NewPaymentDetailResponse 把支付信息投影为查询支付状态响应。
func NewPaymentDetailResponse(item domainpayment.Payment) PaymentDetailResponse {
	result := PaymentDetailResponse{
		OutTradeNo:     item.OutTradeNo,
		RegistrationID: item.RegistrationID,
		Amount:         item.Amount,
		PaymentStatus:  item.PaymentStatus,
	}
	if item.TransactionID != "" {
		transactionID := item.TransactionID
		result.TransactionID = &transactionID
	}
	return result
}

// formatPaymentTime 按 RFC3339 UTC 序列化支付时间点
// （契约 §6.8：对外返回的两个时间按 RFC3339 UTC 序列化）；零值输出空串。
func formatPaymentTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}