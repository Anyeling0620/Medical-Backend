package request

import (
	"errors"

	"github.com/gin-gonic/gin"
)

// 本文件是支付域（/api/v1/payments*）的请求绑定（spec/04-api-contract.md §6.5、§12.4）。
// 字段级校验在这里收敛成面向用户的中文文案，handler 统一映射为 422 REQUEST_VALIDATION_FAILED；
// 取值语义（支付方式、订单归属、支付窗口）由 use case 判定。

// PaymentReadRequest 是 POST /api/v1/payments 的请求体（契约 §6.5、§12.4）。
//
// RegistrationID 用指针承载：nil 表示请求未提交该字段，可据此与「提交了 0」区分，
// 给出「必传字段」而不是「取值为 0」的提示。
type PaymentReadRequest struct {
	RegistrationID *int64 `json:"registrationId"`
	Method         string `json:"method"`
}

// BindPaymentRead 严格解析取支付参数请求体：拒绝未知字段与多余内容，
// registrationId 为必传且必须为正整数；method 的取值校验放在 use case
// （与建单接口的 paymentMethod 同一口径，契约 §6.5）。
func BindPaymentRead(c *gin.Context) (PaymentReadRequest, error) {
	var body PaymentReadRequest
	if err := decodeStrict(c, &body); err != nil {
		return body, err
	}
	if body.RegistrationID == nil {
		return body, errors.New("registrationId 为必传字段")
	}
	if *body.RegistrationID < 1 {
		return body, errors.New("registrationId 必须为正整数")
	}
	return body, nil
}