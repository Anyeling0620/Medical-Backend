// Package payment 定义支付域（hospital.medical_registration 的支付字段）的实体与业务规则。
//
// 本阶段不新增支付表：支付信息寄存在挂号记录上，out_trade_no 是外部交易标识，
// prepay_id 存放支付宝当面付预下单返回的付款二维码内容，三个时间点
// （precreate_at、pay_deadline、expire_at）界定支付窗口
// （spec/04-api-contract.md §6.5–§6.8、spec/创建订单与支付业务说明.md 第 2 节）。
//
// 本包只做纯领域计算，不依赖数据库、HTTP 或支付宝 SDK；持久化与外部调用分别由
// internal/repo 实现 internal/port 中声明的接口。
package payment

import (
	"errors"
	"math/big"
	"strings"
	"time"

	domainregistration "Medical-Web-Backend/internal/domain/registration"
)

// 支付状态沿用挂号域口径：数据库值 1/2/3/4 与对外字符串 UNPAID/PAID/REFUNDED/EXPIRED
// 只保留一份映射实现，避免支付域与挂号域出现两套语义漂移。
const (
	PaymentStatusUnpaid   = domainregistration.PaymentStatusUnpaid
	PaymentStatusPaid     = domainregistration.PaymentStatusPaid
	PaymentStatusRefunded = domainregistration.PaymentStatusRefunded
	PaymentStatusExpired  = domainregistration.PaymentStatusExpired
)

// 支付宝交易状态：异步通知与主动查询共用同一套判定（契约 §6.6、§6.7）。
const (
	TradeStatusSuccess      = "TRADE_SUCCESS"
	TradeStatusFinished     = "TRADE_FINISHED"
	TradeStatusWaitBuyerPay = "WAIT_BUYER_PAY"
	TradeStatusClosed       = "TRADE_CLOSED"
)

// ErrPaymentNotFound 表示按挂号编号或外部交易号找不到支付订单。
//
// 患者在患者端访问他人订单时也返回本错误（由 repository 在同一查询内加归属条件），
// 使「越权」与「不存在」无法区分，调用方统一映射为 404 PAYMENT_NOT_FOUND（契约 §1.2）。
var ErrPaymentNotFound = errors.New("payment not found")

// Payment 是支付视图，映射 hospital.medical_registration 的支付相关字段。
//
// 本实体不带 json tag，也不用于直接序列化：契约对不同接口规定了不同的字段集合
// （取支付参数返回二维码与两个时间点，查询支付状态返回交易号），对外响应统一由
// transport/http/response 的 DTO 投影，避免领域结构体调整时静默改变接口形状。
type Payment struct {
	// RegistrationID 是支付所寄生的挂号编号（medical_registration.id）。
	RegistrationID int64
	// OutTradeNo 是外部交易标识，也是本地资金核对的逻辑唯一键。
	OutTradeNo string
	// Amount 是应支付金额（numeric 的文本形式，如 "80.00"），客户端提交的金额一律不可信。
	Amount string
	// PaymentStatus 是对外字符串状态：UNPAID/PAID/REFUNDED/EXPIRED。
	PaymentStatus string
	// PrepayID 存放支付宝预下单返回的付款二维码内容（qr_code），为空表示尚未预下单。
	PrepayID string
	// PrecreateAt、PayDeadline、ExpireAt 是预下单基准时刻与两个截止时刻（契约 §6.8）。
	PrecreateAt time.Time
	PayDeadline time.Time
	ExpireAt    time.Time
	// TransactionID 是支付宝交易号（trade_no），未支付时为空。
	TransactionID string
}

// PrecreateRequest 是一次 alipay.trade.precreate 调用所需的业务参数。
// 订单号与金额都来自服务端已落库的数据，客户端不可提交。
type PrecreateRequest struct {
	OutTradeNo     string
	Amount         string
	Subject        string
	TimeoutExpress string
	NotifyURL      string
}

// PrecreateResult 是预下单结果：QRCode 即付款二维码内容，落库到 prepay_id。
type PrecreateResult struct {
	QRCode string
}

// TradeQueryResult 是 alipay.trade.query 的结果摘要；TradeStatus 为空表示
// 支付宝侧不存在该交易（ACQ.TRADE_NOT_EXIST），此时不构成任何状态迁移证据。
type TradeQueryResult struct {
	OutTradeNo  string
	TradeNo     string
	TradeStatus string
	TotalAmount string
	GmtPayment  string
}

// NotifyPayload 是验签与身份校验通过后的异步通知载荷，只保留本阶段使用的字段。
type NotifyPayload struct {
	AppID       string
	SellerID    string
	OutTradeNo  string
	TradeNo     string
	TradeStatus string
	TotalAmount string
	GmtPayment  string
	NotifyTime  string
}

// Payable 报告订单此刻是否仍可展示二维码并继续支付（契约 §6.5）：
// 仅 UNPAID 且 now() <= pay_deadline 时为 true。时间点为零值（历史脏数据）时按
// 不可支付收敛，避免把没有支付窗口的订单当成可支付订单。
func (p Payment) Payable(now time.Time) bool {
	return p.PaymentStatus == PaymentStatusUnpaid && !now.After(p.PayDeadline)
}

// Queryable 报告是否应对 provider 做一次兜底主动查询（契约 §6.6）：
// UNPAID 且 now() <= expire_at。超过 expire_at 后由 35 分钟收口任务负责。
func (p Payment) Queryable(now time.Time) bool {
	return p.PaymentStatus == PaymentStatusUnpaid && !now.After(p.ExpireAt)
}

// CanMarkPaid 报告订单当前是否允许 UNPAID -> PAID 迁移（契约 §9）：
// 订单必须仍是 UNPAID，且未超过 expire_at（超时的成功通知属于迟到通知，必须丢弃）。
func (p Payment) CanMarkPaid(now time.Time) bool {
	return p.PaymentStatus == PaymentStatusUnpaid && !now.After(p.ExpireAt)
}

// IsFinal 报告订单是否已处于终态（PAID/EXPIRED/REFUNDED）。
func (p Payment) IsFinal() bool {
	switch p.PaymentStatus {
	case PaymentStatusPaid, PaymentStatusExpired, PaymentStatusRefunded:
		return true
	default:
		return false
	}
}

// SuccessTradeStatus 报告 trade_status 是否为支付成功证据。
// TRADE_SUCCESS 与 TRADE_FINISHED 都表示用户已完成支付（契约 §6.6、§6.7）。
func SuccessTradeStatus(status string) bool {
	return status == TradeStatusSuccess || status == TradeStatusFinished
}

// AmountEquals 比较两个金额字符串是否表示同一金额。
//
// 用有理数做精确比较：两侧都可能出现 "80.0" 与 "80.00" 这类等价写法，
// 浮点比较会引入误差，纯字符串比较会把等价金额误判为不一致。
// 任一侧无法解析时返回 false，按「不一致」收敛：宁可不迁移状态，也不接受可疑金额。
func AmountEquals(a, b string) bool {
	left, ok := parseAmount(a)
	if !ok {
		return false
	}
	right, ok := parseAmount(b)
	if !ok {
		return false
	}
	return left.Cmp(right) == 0
}

func parseAmount(raw string) (*big.Rat, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, false
	}
	value, ok := new(big.Rat).SetString(trimmed)
	if !ok {
		return nil, false
	}
	return value, true
}