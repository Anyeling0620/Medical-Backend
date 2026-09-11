package port

import (
	"context"

	"Medical-Web-Backend/internal/domain/payment"
)

// PaymentPrecreator 描述「建单即预下单」所需的支付预下单能力（契约 §6.2）。
//
// 建单流程（usecase/registration）在建单事务提交后，必须在同一个 HTTP 请求内完成支付宝
// 预下单并回写二维码，避免出现「先建单、稍后再取码」而让订单有效期与二维码有效期不同起点
// （创建订单与支付业务说明.md 第 3 节）。本接口把这段能力抽象出来，由 usecase/payment.Service
// 实现，使挂号用例只依赖接口、不与支付用例的具体实现耦合。
type PaymentPrecreator interface {
	// PrecreateForOrder 为刚建单的挂号订单执行支付宝预下单并回写二维码。
	//
	// registrationID 是 hospital.medical_registration 主键。实现必须与创建支付订单接口
	// （契约 §6.9）共用同一段预下单模块，禁止各写一套。
	//
	// 成功返回时 Payment.PrepayID 必然非空且已落库（调用方可以据此交付二维码）；任何失败
	// （预下单失败、二维码回写失败、订单已不可预下单）都返回错误，由调用方按业务说明第 3.3 节
	// 执行整单补偿，并对外返回 502 PAYMENT_PROVIDER_UNAVAILABLE，且不得交付二维码（契约 §6.2）。
	// 错误原文只用于日志，不得透传给客户端；返回错误时可能已产生新交易，但二维码必然未交付，
	// 因此补偿删除记录不会造成「用户可能已付款、记录却已被删除」的情况。
	PrecreateForOrder(ctx context.Context, registrationID int64) (*payment.Payment, error)
}
