package payment

import (
	"context"
	"errors"
	"log"
	"strings"

	domainpayment "Medical-Web-Backend/internal/domain/payment"
)

// 本文件实现「创建支付订单」模块：为一条已存在的挂号记录建立支付订单，
// 即补齐三个支付时间点、调用 alipay.trade.precreate 预下单并回写 prepay_id。
//
// 预下单实现只有一份（ensurePaymentOrder + precreate），有两个入口：
//   - 外部接口 POST /api/v1/payments/orders（CreateOrder）：仅 ROOT 可以直接调用
//     （契约 §6.9），用于补齐历史订单与线下排查；
//   - 建单流程（PrecreateForOrder）：POST /api/v1/registrations 在建单事务提交后、
//     同一个 HTTP 请求内调用，实现「建单即预下单」（契约 §6.2、业务说明第 3 节）。
//
// 其他角色不能经外部 API 直接创建支付订单：路由层用 realm=mis + ROOT 权限码限定，
// 患者令牌在令牌校验阶段、非 ROOT 管理令牌在权限校验阶段被拒绝（契约 §1.2、§6.9）。

// CreateOrderInput 是创建支付订单的用例入参（契约 §6.9、§12.4）。
type CreateOrderInput struct {
	RegistrationID int64
}

// CreateOrderResult 是创建支付订单的结果。
//
// Created 表示本次调用是否真正写入了二维码，也就是本次新建了支付订单；handler 据此区分
// 201 与 200：并发下若二维码已被其它请求写库，本次返回 Created=false（回读库中的二维码，
// 不重复新建交易），同一挂号重复调用也返回同一条资源（契约 §6.9）。
type CreateOrderResult struct {
	Payment domainpayment.Payment
	Payable bool
	Created bool
}

// CreateOrder 实现 POST /api/v1/payments/orders：为指定挂号创建支付订单（契约 §6.9）。
//
// 幂等：同一挂号重复调用返回同一条支付订单与同一个二维码，不会重复预下单
// （二维码以首次预下单结果为准，见 SavePrepayID 的「只写空行」语义）。
// 终态订单不再创建：已支付返回 409 PAYMENT_ALREADY_PAID，已过期或已退款返回
// 409 PAYMENT_INVALID_TRANSITION，两者都不做状态迁移（契约 §6.8、§9、§10）。
func (s *Service) CreateOrder(
	ctx context.Context,
	actor Actor,
	input CreateOrderInput,
) (*CreateOrderResult, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := requireUsableActor(actor); err != nil {
		return nil, err
	}
	if input.RegistrationID < 1 {
		return nil, validationError("registrationId 必须为正整数")
	}

	// 本接口是管理端专用（ROOT），按挂号编号定位订单，不做患者归属限定。
	item, err := s.payments.FindPaymentByRegistrationID(ctx, input.RegistrationID, 0)
	if errors.Is(err, domainpayment.ErrPaymentNotFound) {
		return nil, notFoundError()
	}
	if err != nil {
		return nil, dataError(err)
	}

	if err := rejectedFinalState(item); err != nil {
		return nil, err
	}

	created, isNew, err := s.ensurePaymentOrder(ctx, item)
	if err != nil {
		return nil, err
	}
	if isNew {
		// 预下单是一次外部网络往返，期间订单可能已被异步通知改成终态；而 precreate 只覆盖
		// 二维码、不刷新状态，因此这里按 out_trade_no 重读一次最新状态再判定，
		// 避免对已支付、已结束的订单返回 201 与二维码（契约 §6.9）。
		latest, err := s.payments.FindPaymentByOutTradeNo(ctx, created.OutTradeNo, 0)
		if errors.Is(err, domainpayment.ErrPaymentNotFound) {
			return nil, notFoundError()
		}
		if err != nil {
			return nil, dataError(err)
		}
		created = latest
	}
	if err := rejectedFinalState(created); err != nil {
		return nil, err
	}
	return &CreateOrderResult{
		Payment: *created,
		Payable: created.Payable(s.now()),
		Created: isNew,
	}, nil
}

// ensurePaymentOrder 幂等建立支付订单：补齐支付窗口并完成预下单，返回最新支付信息
// 与「本次是否真正写入了二维码」。
//
// 这是支付订单创建的唯一实现，创建接口与建单流程共用它，
// 避免出现两套预下单口径（契约 §6.2、§6.9）：
//   - 三个时间点或二维码缺失时补齐：EnsurePaymentWindow 只写缺失列，已有值不被覆盖，
//     三个时间点之间 +30/+35 分钟的固定关系保持不变（契约 §6.8）；
//   - 只有「订单仍为 UNPAID、确实没有二维码、且未过 pay_deadline」时才调用
//     alipay.trade.precreate：已有二维码直接复用，重复预下单会被支付宝判为交易已存在
//     （不可重试冲突）；已过支付截止的订单不再新建交易，只按现状回读。
func (s *Service) ensurePaymentOrder(
	ctx context.Context,
	item *domainpayment.Payment,
) (*domainpayment.Payment, bool, error) {
	if item.PrepayID != "" && !item.PayDeadline.IsZero() && !item.ExpireAt.IsZero() {
		return item, false, nil
	}

	ensured, err := s.payments.EnsurePaymentWindow(ctx, item.OutTradeNo)
	if errors.Is(err, domainpayment.ErrPaymentNotFound) {
		return nil, false, notFoundError()
	}
	if err != nil {
		return nil, false, dataError(err)
	}

	// 只有「确实还没有二维码」时才预下单：prepay_id 已存在但时间点缺失属于脏数据，
	// 补窗口后直接回读已有二维码即可。
	if ensured.PaymentStatus != domainpayment.PaymentStatusUnpaid ||
		ensured.PrepayID != "" ||
		s.now().After(ensured.PayDeadline) {
		return ensured, false, nil
	}

	created, wrote, err := s.precreate(ctx, ensured)
	if err != nil {
		return nil, false, err
	}
	return created, wrote, nil
}

// rejectedFinalState 把终态订单映射为「不能创建支付订单」的冲突错误（契约 §6.9）：
// 已支付用 PAYMENT_ALREADY_PAID，已过期或已退款用 PAYMENT_INVALID_TRANSITION。
// 两种终态都不做任何状态迁移、也不返回二维码；非终态返回 nil。
func rejectedFinalState(item *domainpayment.Payment) error {
	switch item.PaymentStatus {
	case domainpayment.PaymentStatusPaid:
		return &ServiceError{Code: CodePaymentAlreadyPaid, Message: "订单已支付，不能创建支付订单"}
	case domainpayment.PaymentStatusExpired, domainpayment.PaymentStatusRefunded:
		return &ServiceError{
			Code:    CodePaymentInvalidTransition,
			Message: "订单已结束，不能创建支付订单",
		}
	default:
		return nil
	}
}

// PrecreateForOrder 实现 port.PaymentPrecreator：建单流程（契约 §6.2）在建单事务提交后，
// 于同一个 HTTP 请求内为刚建好的订单执行预下单并回写二维码。
//
// 与创建支付订单接口（§6.9）共用同一段预下单模块（ensurePaymentOrder + precreate），
// 禁止各写一套。只有 qr_code 已落库（PrepayID 非空）才算成功；任何其它结果都返回错误，
// 调用方必须据此执行整单补偿并返回 502 PAYMENT_PROVIDER_UNAVAILABLE，且不得把二维码
// 交付给客户端（创建订单与支付业务说明.md 第 3.3 节）。
func (s *Service) PrecreateForOrder(
	ctx context.Context,
	registrationID int64,
) (*domainpayment.Payment, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if registrationID < 1 {
		return nil, validationError("registrationId 必须为正整数")
	}

	// 建单流程按挂号编号定位刚写入的订单；这是服务内部调用，不做患者归属限定。
	item, err := s.payments.FindPaymentByRegistrationID(ctx, registrationID, 0)
	if errors.Is(err, domainpayment.ErrPaymentNotFound) {
		return nil, notFoundError()
	}
	if err != nil {
		return nil, dataError(err)
	}

	ensured, _, err := s.ensurePaymentOrder(ctx, item)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ensured.PrepayID) == "" {
		// 已有二维码直接复用（不重复预下单），但「没有二维码」绝不能当作成功：
		// 建单响应必须携带 qrCode，否则客户端会拿到一个无法支付的订单（契约 §6.2）。
		log.Printf("告警：建单预下单未取得二维码 registration_id=%d out_trade_no=%s",
			registrationID, ensured.OutTradeNo)
		return nil, providerUnavailableError()
	}
	return ensured, nil
}
