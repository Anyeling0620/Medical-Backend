package port

import (
	"context"
	"errors"
	"net/url"

	"Medical-Web-Backend/internal/domain/payment"
)

// 支付宝适配器返回的稳定错误：调用方据此区分「可重试的服务不可用」与
// 「不可重试的通知异常」，不得把 provider 原文或 sub_code 透传给客户端（契约 §6.2、§10）。
var (
	// ErrAlipayUnavailable 表示支付宝不可达、返回系统级错误（如 code=20000）、
	// 响应验签失败，或本服务未配置支付宝凭据；上层映射为 502 PAYMENT_PROVIDER_UNAVAILABLE（可重试）。
	ErrAlipayUnavailable = errors.New("alipay service is unavailable")
	// ErrAlipayTradeClosed 表示支付宝判定该 out_trade_no 的交易已关闭或已成功
	// （ACQ.TRADE_HAS_CLOSE / ACQ.TRADE_HAS_SUCCESS），属于不可重试的冲突。
	ErrAlipayTradeClosed = errors.New("alipay trade is closed or already succeeded")
	// ErrNotifySignatureInvalid 表示异步通知验签失败；上层映射为 400 PAYMENT_NOTIFY_SIGNATURE_INVALID 并告警。
	ErrNotifySignatureInvalid = errors.New("alipay notify signature is invalid")
	// ErrNotifyIdentityMismatch 表示通知的 app_id 或 seller_id 与服务端配置不一致；
	// 上层映射为 400 PAYMENT_NOTIFY_IDENTITY_MISMATCH 并告警。
	ErrNotifyIdentityMismatch = errors.New("alipay notify app id or seller id mismatch")
)

// AlipayGateway 描述支付宝当面付所需的外部服务能力：预下单、主动查询与异步通知验签。
//
// 真实实现与测试桩使用同一份契约，便于在自动化测试中隔离外部依赖
// （spec/02-architecture.md「外部服务适配」）。
type AlipayGateway interface {
	// Precreate 调用 alipay.trade.precreate 生成付款二维码。
	//
	// 未配置凭据或网络失败返回 ErrAlipayUnavailable；支付宝判定该 out_trade_no
	// 的交易已关闭或已成功返回 ErrAlipayTradeClosed（不可重试）。
	Precreate(ctx context.Context, req payment.PrecreateRequest) (*payment.PrecreateResult, error)
	// QueryTrade 调用 alipay.trade.query 查询交易状态。
	//
	// 交易不存在（ACQ.TRADE_NOT_EXIST）不是错误：返回 TradeStatus 为空的结果；
	// 未配置凭据、网络失败或系统级错误返回 ErrAlipayUnavailable。
	QueryTrade(ctx context.Context, outTradeNo string) (*payment.TradeQueryResult, error)
	// VerifyNotify 校验异步通知的 RSA2 签名与身份（app_id、seller_id）。
	//
	// 验签失败返回 ErrNotifySignatureInvalid，身份不一致返回 ErrNotifyIdentityMismatch；
	// 两者都是不可恢复异常，调用方必须告警且不得返回 success（契约 §6.7）。
	VerifyNotify(ctx context.Context, form url.Values) (*payment.NotifyPayload, error)
}