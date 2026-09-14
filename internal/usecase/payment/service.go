// Package payment 实现支付域用例：创建支付订单（契约 §6.9）、取支付参数（§6.5）、
// 查询支付状态（§6.6）与支付宝异步通知处理（§6.7）。
//
// 支付信息寄存在挂号记录上，本包负责：支付窗口判定、预下单（建单流程 §6.2 与创建支付订单
// 接口 §6.9 共用同一段实现）、兜底主动查询、通知验签后的金额校验与状态迁移编排。
// 状态迁移与号源占用的原子性由 repository 的条件更新保证；provider 交互全部经由
// port.AlipayGateway，通知路径与主动查询共用同一段迁移逻辑
// （spec/创建订单与支付业务说明.md 第 6、9 节）。
package payment

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainpayment "Medical-Web-Backend/internal/domain/payment"
	"Medical-Web-Backend/internal/port"
)

// 业务错误码：handler 依据这些稳定编码映射 HTTP 响应（契约 §10 错误码目录）。
const (
	CodeValidationFailed       = "REQUEST_VALIDATION_FAILED"
	CodePaymentNotFound        = "PAYMENT_NOT_FOUND"
	CodeAmountMismatch         = "PAYMENT_AMOUNT_MISMATCH"
	CodeNotifySignatureInvalid = "PAYMENT_NOTIFY_SIGNATURE_INVALID"
	CodeNotifyIdentityMismatch = "PAYMENT_NOTIFY_IDENTITY_MISMATCH"
	CodeProviderUnavailable    = "PAYMENT_PROVIDER_UNAVAILABLE"
	// CodeDependencyUnavailable 表示仓储（PostgreSQL）不可用，映射为 502 DEPENDENCY_UNAVAILABLE。
	CodeDependencyUnavailable = "DEPENDENCY_UNAVAILABLE"
	// CodePaymentAlreadyPaid / CodePaymentInvalidTransition 是创建支付订单（§6.9）在终态订单
	// 上的冲突错误：已支付返回 PAYMENT_ALREADY_PAID，已过期或已退款返回 PAYMENT_INVALID_TRANSITION，
	// 两者都映射为 409，且都不做状态迁移（契约 §6.8、§9、§10）。
	CodePaymentAlreadyPaid       = "PAYMENT_ALREADY_PAID"
	CodePaymentInvalidTransition = "PAYMENT_INVALID_TRANSITION"
)

// PaymentMethodAlipay 是本阶段唯一支持的支付方式：其他取值（含历史示例中的 WECHAT）
// 一律返回 422 REQUEST_VALIDATION_FAILED（契约 §6.5、§12.4）。
const PaymentMethodAlipay = "ALIPAY"

// paymentTimeoutExpress 与 pay_deadline 对应，固定 30 分钟（契约 §6.8）。
const paymentTimeoutExpress = "30m"

// defaultPaymentSubject 是支付宝订单标题的兜底值：支付宝要求标题非空且不含 / = & 等特殊字符。
const defaultPaymentSubject = "医院挂号费"

// maxOutTradeNoLength 与 medical_registration.out_trade_no 的列宽（CHAR(32)）一致。
const maxOutTradeNoLength = 32

// maxPrepayIDLength 与 medical_registration.prepay_id 的列宽（CHAR(64)）一致。
// PostgreSQL 对超出 character(n) 列宽的写入直接报错（不是截断），若不在这里前置拦截，
// 超长二维码会以数据库错误的形式冒泡成 502，客户端既拿不到二维码也看不到原因。
const maxPrepayIDLength = 64

// prepayWriteAttempts / prepayWriteInterval 控制 prepay_id 回写的有限重试：
// 事务提交后回写二维码是唯一允许的支付字段写操作，回写失败必须重试，仍失败则整单补偿
// （创建订单与支付业务说明.md 第 3.2 节）。重试次数与间隔都刻意取小值，
// 避免把请求时长绑到数据库的尾延迟上。
const (
	prepayWriteAttempts = 3
	prepayWriteInterval = 50 * time.Millisecond
)

// ErrDependencyUnavailable 表示支付仓储不可用（连接失败等），映射为 502 DEPENDENCY_UNAVAILABLE。
// 依赖故障不能伪装成「订单不存在」，否则客户端会把可重试的故障当成业务结论（契约 §10）。
var ErrDependencyUnavailable = errors.New("payment dependency is unavailable")

// ErrInvalidActor 表示调用者身份自相矛盾：声明为患者域却缺少当前患者标识。
//
// 与挂号域同一取舍：仓储用 ownerPatientID > 0 作为「是否限定归属」的开关，
// 属性一旦缺失就会静默关闭归属条件、读到全量订单，因此这里显式拒绝（映射为 500）。
var ErrInvalidActor = errors.New("payment actor is not usable for authorization")

// Details 是可选的错误细节（仅非敏感字段）。
type Details map[string]any

// ServiceError 是 use case 返回给 handler 的业务错误，携带稳定 code 与面向用户的 message。
type ServiceError struct {
	Code    string
	Message string
	Details Details
}

func (e *ServiceError) Error() string { return e.Message }

// Actor 是本次调用的调用者身份。
//
// Realm 决定授权路径：patient 域的主体固定为令牌中的当前患者（PatientID），
// 只能访问本人挂号关联的订单；mis 域已由中间件完成权限编码校验，这里只按资源存在性处理。
type Actor struct {
	Realm     domainauth.Realm
	PatientID int64
	UserID    int64
}

// IsPatient 报告调用者是否来自患者域。
func (a Actor) IsPatient() bool { return a.Realm == domainauth.RealmPatient }

// Config 是支付用例的可调参数。
type Config struct {
	// PaymentSubject 是支付宝订单标题，缺省「医院挂号费」。
	PaymentSubject string
	// NotifyURL 是支付宝异步通知地址；未配置时不向支付宝传递该参数，
	// 系统降级为主动查询模式（契约 §6.7）。
	NotifyURL string
	// LogDroppedNotify 控制迟到/丢弃通知的终端日志，仅开发环境开启（契约 §6.7、第 8 节）。
	LogDroppedNotify bool
	// Now 是时钟，供测试注入固定时间；缺省为 UTC 当前时间。
	// 支付窗口比较的是本机时钟与数据库 now() 写入的绝对时刻，因此部署环境必须按
	// 创建订单与支付业务说明.md 第 2 节做 NTP 同步，避免本机时钟漂移导致窗口判定失真。
	Now func() time.Time
}

// Service 编排支付域用例。
type Service struct {
	payments port.PaymentRepository
	gateway  port.AlipayGateway
	cfg      Config
	now      func() time.Time
}

// NewService 构造支付用例。
func NewService(
	payments port.PaymentRepository,
	gateway port.AlipayGateway,
	cfg Config,
) *Service {
	if strings.TrimSpace(cfg.PaymentSubject) == "" {
		cfg.PaymentSubject = defaultPaymentSubject
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{payments: payments, gateway: gateway, cfg: cfg, now: now}
}

// ReadInput 是取支付参数的用例入参（契约 §6.5、§12.4）。
type ReadInput struct {
	RegistrationID int64
	Method         string
}

// ReadResult 是取支付参数的结果：Payment 为订单支付信息，Payable 决定响应是否包含二维码。
type ReadResult struct {
	Payment domainpayment.Payment
	Payable bool
}

// ReadPayable 实现 POST /api/v1/payments：按 registrationId 幂等读取支付参数（契约 §6.5）。
//
// 语义是「只读」：不调用支付宝，也不补写支付窗口。二维码与两个时间点由建单流程（§6.2）
// 在同一次预下单内写定，本接口只按 registrationId 回读；这样订单有效期与二维码有效期
// 同起点，不会出现「先建单、稍后再取码」的降级路径（契约 §6.5、
// 创建订单与支付业务说明.md 第 3.3 节）。历史数据缺少二维码时，由创建支付订单接口
// （§6.9，仅 ROOT 可直接调用）或重新挂号收敛。
func (s *Service) ReadPayable(
	ctx context.Context,
	actor Actor,
	input ReadInput,
) (*ReadResult, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := requireUsableActor(actor); err != nil {
		return nil, err
	}
	if input.RegistrationID < 1 {
		return nil, validationError("registrationId 必须为正整数")
	}
	if err := validateMethod(input.Method); err != nil {
		return nil, err
	}

	ownerPatientID := ownerPatientIDOf(actor)
	item, err := s.payments.FindPaymentByRegistrationID(ctx, input.RegistrationID, ownerPatientID)
	if errors.Is(err, domainpayment.ErrPaymentNotFound) {
		return nil, notFoundError()
	}
	if err != nil {
		return nil, dataError(err)
	}

	return &ReadResult{Payment: *item, Payable: item.Payable(s.now())}, nil
}

// Detail 实现 GET /api/v1/payments/{outTradeNo}：查询支付状态（契约 §6.6）。
//
// 订单仍为 UNPAID 且未超过 expire_at 时触发一次 alipay.trade.query 作为异步通知的兜底；
// 查到成功证据走与通知完全相同的迁移规则补记 PAID 且不释放号源。超过 expire_at 后
// 不做主动查询也不迁移状态，收口由 35 分钟任务负责。
func (s *Service) Detail(
	ctx context.Context,
	actor Actor,
	outTradeNo string,
) (*domainpayment.Payment, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := requireUsableActor(actor); err != nil {
		return nil, err
	}
	if err := validateOutTradeNo(outTradeNo); err != nil {
		return nil, err
	}

	ownerPatientID := ownerPatientIDOf(actor)
	item, err := s.payments.FindPaymentByOutTradeNo(ctx, outTradeNo, ownerPatientID)
	if errors.Is(err, domainpayment.ErrPaymentNotFound) {
		return nil, notFoundError()
	}
	if err != nil {
		return nil, dataError(err)
	}

	now := s.now()
	if s.gateway == nil || !item.Queryable(now) {
		return item, nil
	}
	result, err := s.gateway.QueryTrade(ctx, item.OutTradeNo)
	if err != nil {
		// 主动查询超时可重试（契约 §6.6、§10）。
		return nil, providerError(err, "主动查询", item.OutTradeNo)
	}
	if !domainpayment.SuccessTradeStatus(result.TradeStatus) {
		return item, nil
	}
	// 与通知路径同一口径：判定成功证据（含金额校验）并走条件更新。实现只有一份
	// （settleFromTradeQuery），收口任务也用同一段逻辑，禁止各写一套（契约 §6.6、§6.8）。
	settlement, err := settleFromTradeQuery(ctx, s.payments, item, result)
	if err != nil {
		return nil, dataError(err)
	}
	if !settlement.SuccessEvidence || settlement.AmountMismatch {
		// 不是支付成功证据，或金额与订单对不上：只告警不迁移，按原状态回显（契约 §6.6）。
		return item, nil
	}
	if !settlement.Migrated {
		// 条件更新未生效说明状态已被通知路径改写：重新读一次真实状态，不覆盖他人结果。
		latest, err := s.payments.FindPaymentByOutTradeNo(ctx, item.OutTradeNo, ownerPatientID)
		if errors.Is(err, domainpayment.ErrPaymentNotFound) {
			return nil, notFoundError()
		}
		if err != nil {
			return nil, dataError(err)
		}
		return latest, nil
	}
	item.PaymentStatus = domainpayment.PaymentStatusPaid
	item.TransactionID = result.TradeNo
	return item, nil
}

// NotifyOutcome 描述一次异步通知的处理结果，供 handler 决定日志与响应。
type NotifyOutcome struct {
	// Marked 表示本次通知完成了 UNPAID -> PAID 迁移。
	Marked bool
	// Dropped 表示通知被丢弃（幂等重放、终态、超时或未知状态），仍必须返回 success。
	Dropped bool
}

// HandleNotify 实现 POST /api/v1/payments/alipay/notify（契约 §6.7）。
//
// 处理顺序固定为：验签 -> 身份校验 -> 定位订单 -> 金额校验 -> 状态判定。
// 除验签、身份、金额和订单不存在四类异常外一律返回成功，避免支付宝无意义重试；
// 通知路径不把订单置为 EXPIRED，过期统一由 35 分钟收口任务处理（契约 §6.8）。
func (s *Service) HandleNotify(ctx context.Context, form map[string][]string) (*NotifyOutcome, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if s.gateway == nil {
		return nil, providerUnavailableError()
	}
	payload, err := s.gateway.VerifyNotify(ctx, form)
	switch {
	case errors.Is(err, port.ErrNotifySignatureInvalid):
		log.Printf("告警：支付宝异步通知验签失败，已拒绝处理")
		return nil, &ServiceError{Code: CodeNotifySignatureInvalid, Message: "支付通知验签失败"}
	case errors.Is(err, port.ErrNotifyIdentityMismatch):
		log.Printf("告警：支付宝异步通知身份校验失败（app_id/seller_id 与服务端配置不一致）")
		return nil, &ServiceError{Code: CodeNotifyIdentityMismatch, Message: "支付通知身份校验失败"}
	case err != nil:
		return nil, providerError(err, "通知验签", "")
	}

	// 通知路径按 out_trade_no 定位订单，不限定归属（没有用户令牌，契约 §1.2）。
	item, err := s.payments.FindPaymentByOutTradeNo(ctx, payload.OutTradeNo, 0)
	if errors.Is(err, domainpayment.ErrPaymentNotFound) {
		log.Printf("告警：支付宝异步通知的订单不存在 out_trade_no=%s", payload.OutTradeNo)
		return nil, notFoundError()
	}
	if err != nil {
		return nil, dataError(err)
	}

	if !domainpayment.AmountEquals(payload.TotalAmount, item.Amount) {
		log.Printf(
			"告警：支付宝异步通知金额与订单不一致 out_trade_no=%s 通知金额=%s 订单金额=%s",
			payload.OutTradeNo, payload.TotalAmount, item.Amount)
		return nil, &ServiceError{Code: CodeAmountMismatch, Message: "支付金额校验失败"}
	}

	now := s.now()
	switch {
	case item.PaymentStatus == domainpayment.PaymentStatusPaid:
		// 重复的成功通知：幂等，不重复记账。
		s.logNotifyDrop("订单已是 PAID，幂等忽略", payload)
		return &NotifyOutcome{Dropped: true}, nil
	case item.IsFinal():
		// 已 EXPIRED/REFUNDED：丢弃，不迁移状态、不写交易号、不释放或恢复号源。
		s.logNotifyDrop("订单已处于终态，丢弃通知", payload)
		return &NotifyOutcome{Dropped: true}, nil
	case !domainpayment.SuccessTradeStatus(payload.TradeStatus):
		if payload.TradeStatus == domainpayment.TradeStatusWaitBuyerPay ||
			payload.TradeStatus == domainpayment.TradeStatusClosed {
			// WAIT_BUYER_PAY/TRADE_CLOSED 都保持 UNPAID，过期由收口任务统一处理。
			return &NotifyOutcome{}, nil
		}
		s.logNotifyDrop("通知状态不在已知集合内，记录告警并保持原状态", payload)
		return &NotifyOutcome{}, nil
	case !item.CanMarkPaid(now):
		// 迟到通知：订单仍是 UNPAID 但已超过 expire_at，不得补记 PAID。
		s.logNotifyDrop("成功通知已超过 expire_at，丢弃", payload)
		return &NotifyOutcome{Dropped: true}, nil
	}

	updated, err := s.payments.MarkPaid(ctx, item.OutTradeNo, payload.TradeNo)
	if err != nil {
		return nil, dataError(err)
	}
	if !updated {
		// 条件更新未生效：订单已被其它路径处理，按当前状态重新判定，不得覆盖。
		s.logNotifyDrop("条件更新未生效，订单已被其它路径处理", payload)
		return &NotifyOutcome{Dropped: true}, nil
	}
	return &NotifyOutcome{Marked: true}, nil
}

// precreate 调用支付宝预下单并回写二维码；拿不到本地落库凭证时不得把二维码交付给
// 客户端（创建订单与支付业务说明.md 第 3.2 节）。预下单失败与回写失败分别映射为
// PAYMENT_PROVIDER_UNAVAILABLE 与 DEPENDENCY_UNAVAILABLE，两种情况都不返回二维码。
//
// 返回值 wrote 表示本次调用是否真正写入了二维码：false 说明并发请求已先行写入，
// 返回的是按 out_trade_no 回读到的整行最新数据，不是本条调用产生的支付宝交易，
// 调用方不得据此判定「新建成功」（创建支付订单接口据此区分 201 与 200，契约 §6.9）。
func (s *Service) precreate(
	ctx context.Context,
	item *domainpayment.Payment,
) (*domainpayment.Payment, bool, error) {
	if s.gateway == nil {
		return nil, false, providerUnavailableError()
	}
	result, err := s.gateway.Precreate(ctx, domainpayment.PrecreateRequest{
		OutTradeNo:     item.OutTradeNo,
		Amount:         item.Amount,
		Subject:        s.cfg.PaymentSubject,
		TimeoutExpress: paymentTimeoutExpress,
		NotifyURL:      s.cfg.NotifyURL,
	})
	if err != nil {
		return nil, false, providerError(err, "预下单", item.OutTradeNo)
	}
	if strings.TrimSpace(result.QRCode) == "" {
		return nil, false, providerUnavailableError()
	}
	if len(result.QRCode) > maxPrepayIDLength {
		log.Printf(
			"告警：支付宝预下单二维码长度 %d 超过 prepay_id 列宽 %d，无法落库 out_trade_no=%s",
			len(result.QRCode), maxPrepayIDLength, item.OutTradeNo)
		return nil, false, providerUnavailableError()
	}
	var (
		saved   bool
		saveErr error
	)
	for attempt := 0; attempt < prepayWriteAttempts; attempt++ {
		saved, saveErr = s.payments.SavePrepayID(ctx, item.OutTradeNo, result.QRCode)
		if saveErr == nil {
			break
		}
		log.Printf("告警：回写支付宝预下单二维码失败（第 %d/%d 次）out_trade_no=%s: %v",
			attempt+1, prepayWriteAttempts, item.OutTradeNo, saveErr)
		if attempt < prepayWriteAttempts-1 {
			time.Sleep(prepayWriteInterval)
		}
	}
	if saveErr != nil {
		// 重试后仍回写失败：本地没有对应二维码记录，绝不能把二维码返回给客户端。
		return nil, false, dataError(saveErr)
	}
	if !saved {
		// 并发请求已先行写入二维码：回读库中的值作为响应，
		// 保证返回给客户端的二维码与已落库的那一个是同一个（契约 §6.5、§6.2）。
		stored, err := s.payments.FindPaymentByOutTradeNo(ctx, item.OutTradeNo, 0)
		if err != nil {
			log.Printf("告警：回读支付宝预下单二维码失败 out_trade_no=%s: %v", item.OutTradeNo, err)
			return nil, false, dataError(err)
		}
		if strings.TrimSpace(stored.PrepayID) == "" {
			log.Printf("告警：预下单二维码未回写且库中无记录 out_trade_no=%s", item.OutTradeNo)
			return nil, false, providerUnavailableError()
		}
		// 返回整行回读结果而不是只回填二维码：竞争方可能在这段时间把订单改成终态，
		// 调用方需要基于最新状态决定响应（契约 §6.9）。
		return stored, false, nil
	}
	item.PrepayID = result.QRCode
	return item, true, nil
}

// logNotifyDrop 输出被丢弃通知的关键字段（仅开发环境开启）：
// out_trade_no、trade_no、total_amount、trade_status、gmt_payment 与接收时间（契约 §6.7 第 8 节）。
func (s *Service) logNotifyDrop(reason string, payload *domainpayment.NotifyPayload) {
	if !s.cfg.LogDroppedNotify {
		return
	}
	log.Printf(
		"支付宝通知已丢弃（%s）out_trade_no=%s trade_no=%s total_amount=%s trade_status=%s gmt_payment=%s 接收时间=%s",
		reason, payload.OutTradeNo, payload.TradeNo, payload.TotalAmount,
		payload.TradeStatus, payload.GmtPayment, s.now().Format(time.RFC3339))
}

// ready 校验用例依赖是否齐备；缺失属于启动期配置错误。
func (s *Service) ready() error {
	if s == nil || s.payments == nil {
		return errors.New("支付用例依赖未配置")
	}
	return nil
}

// requireUsableActor 校验调用者身份自洽，防止归属条件被静默跳过（见 ErrInvalidActor）。
func requireUsableActor(actor Actor) error {
	if actor.IsPatient() && actor.PatientID < 1 {
		return ErrInvalidActor
	}
	return nil
}

// ownerPatientIDOf 把调用者身份翻译为仓储的归属开关：患者域只读本人订单，管理端传 0。
func ownerPatientIDOf(actor Actor) int64 {
	if actor.IsPatient() {
		return actor.PatientID
	}
	return 0
}

// validateMethod 校验支付方式：本阶段只支持 ALIPAY（契约 §6.5、§12.4）。
//
// 这里是精确匹配，不做大小写归一化：契约要求「其他取值一律返回 422」，
// 与本仓库其它枚举入参（paymentStatus/sort/order 均为精确匹配）保持同一口径。
func validateMethod(method string) error {
	if strings.TrimSpace(method) == "" {
		return validationError("method 为必传字段")
	}
	if method != PaymentMethodAlipay {
		return validationError("暂不支持该支付方式")
	}
	return nil
}

// validateOutTradeNo 校验路径参数可用：非空且不超过 out_trade_no 的列宽。
func validateOutTradeNo(outTradeNo string) error {
	trimmed := strings.TrimSpace(outTradeNo)
	if trimmed == "" {
		return validationError("outTradeNo 为必传字段")
	}
	if len(trimmed) > maxOutTradeNoLength {
		return validationError("outTradeNo 过长")
	}
	return nil
}

// providerError 把支付宝适配器错误映射为契约错误：交易已关闭/已成功不可重试，
// 其余按 502 PAYMENT_PROVIDER_UNAVAILABLE（可重试）。provider 原文与 sub_code 只进日志，
// 不透传给客户端（契约 §6.2、§10）。
func providerError(err error, action, outTradeNo string) error {
	if errors.Is(err, port.ErrAlipayTradeClosed) {
		log.Printf("告警：支付宝%s冲突（交易已关闭或已成功）out_trade_no=%s: %v", action, outTradeNo, err)
		return &ServiceError{
			Code:    CodeProviderUnavailable,
			Message: "该订单在支付渠道已不可支付，请重新挂号",
		}
	}
	log.Printf("告警：支付宝%s失败 out_trade_no=%s: %v", action, outTradeNo, err)
	return providerUnavailableError()
}

// dataError 把仓储错误归类为依赖不可用（502）。
func dataError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrDependencyUnavailable, err)
}

func validationError(message string) error {
	return &ServiceError{Code: CodeValidationFailed, Message: message}
}

func notFoundError() error {
	return &ServiceError{Code: CodePaymentNotFound, Message: "支付订单不存在"}
}

func providerUnavailableError() error {
	return &ServiceError{Code: CodeProviderUnavailable, Message: "支付渠道暂时不可用，请稍后重试"}
}
