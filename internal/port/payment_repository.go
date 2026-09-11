package port

import (
	"context"

	"Medical-Web-Backend/internal/domain/payment"
)

// PaymentRepository 描述支付字段（hospital.medical_registration）持久化所需的读写能力。
//
// 支付信息寄存在挂号记录上，因此本仓库不新建支付表：所有读写都落在挂号行上，
// 以 out_trade_no 作为外部资源标识。状态迁移的一致性由条件更新（payment_status = 1）
// 保证，repository 只负责原子执行并返回领域错误，不替 use case 决定 HTTP 语义。
type PaymentRepository interface {
	// FindPaymentByRegistrationID 按挂号编号读取支付信息。
	//
	// ownerPatientID > 0 时同时限定「该挂号属于此患者名下的就诊卡」，用于患者端的
	// 越权隐藏：他人订单与不存在的订单统一返回 payment.ErrPaymentNotFound，
	// 调用方无法据此枚举患者数据；管理端传 0 表示不限定归属（契约 §1.2、§6.5）。
	FindPaymentByRegistrationID(ctx context.Context, registrationID int64, ownerPatientID int64) (*payment.Payment, error)
	// FindPaymentByOutTradeNo 按外部交易号读取支付信息，归属规则同上（契约 §6.6）。
	FindPaymentByOutTradeNo(ctx context.Context, outTradeNo string, ownerPatientID int64) (*payment.Payment, error)
	// EnsurePaymentWindow 幂等补齐支付窗口：三个时间点为空时用数据库 now() 一次性写入
	// precreate_at、pay_deadline（+30 分钟）、expire_at（+35 分钟），并返回补齐后的支付信息。
	//
	// 这是最小闭环阶段的过渡实现：建单流程（契约 §6.2）尚未写入这三个字段，
	// 首次取支付参数时补齐可让支付窗口与契约 §6.8 的语义保持一致；
	// 建单流程接入后本方法不再产生写入（三个时间点已由建单 INSERT 写定）。
	// 只对仍为「未付款」的订单补齐，避免在已 PAID/EXPIRED/REFUNDED 的行上生成新的支付窗口。
	// 订单不存在返回 payment.ErrPaymentNotFound。
	EnsurePaymentWindow(ctx context.Context, outTradeNo string) (*payment.Payment, error)
	// SavePrepayID 回写支付宝预下单返回的付款二维码内容（medical_registration.prepay_id）。
	//
	// 只写入当前为空的行（首个预下单结果为准），重复调用不会覆盖已有二维码；
	// 返回值表示本次是否真正写入（false 表示并发请求已先行写入），调用方应据此回读库中的
	// 二维码而不是把内存值直接交付给客户端；回写失败必须向上返回
	// （契约 §6.2 第 3.2 节、§6.5）。
	SavePrepayID(ctx context.Context, outTradeNo string, prepayID string) (bool, error)
	// MarkPaid 走 UNPAID -> PAID 的条件更新并写入支付宝交易号，
	// 返回值表示本次调用是否真正完成了状态迁移（false 表示已被其它路径处理）。
	MarkPaid(ctx context.Context, outTradeNo string, transactionID string) (bool, error)
}
