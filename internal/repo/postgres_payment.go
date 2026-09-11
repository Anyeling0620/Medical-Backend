package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"Medical-Web-Backend/internal/domain/payment"
	domainregistration "Medical-Web-Backend/internal/domain/registration"
)

// PostgresPaymentRepository 实现支付字段（hospital.medical_registration）的 PostgreSQL 持久化。
//
// 支付信息寄存在挂号记录上，因此本仓库不新建支付表：读取按挂号编号或 out_trade_no 定位挂号行，
// 状态迁移用带前置条件（payment_status = 未付款）的条件更新完成，保证「同一订单最多一次
// UNPAID -> PAID」（契约 §9、§6.7）。
type PostgresPaymentRepository struct {
	db *sql.DB
}

// NewPostgresPaymentRepository 构造支付仓库。
func NewPostgresPaymentRepository(db *sql.DB) *PostgresPaymentRepository {
	return &PostgresPaymentRepository{db: db}
}

// paymentColumns 是支付读取的字段投影；out_trade_no、prepay_id、transaction_id 都是定长
// CHAR 列，读取时 btrim 去掉尾部补位空格；金额取 numeric 的文本形式，由 registrationAmount
// 统一为两位小数，避免浮点误差（与挂号域同一口径）。
const paymentColumns = `r.id, btrim(r.out_trade_no) AS out_trade_no, r.amount::text,
r.payment_status, btrim(r.prepay_id) AS prepay_id, r.precreate_at, r.pay_deadline, r.expire_at,
btrim(r.transaction_id) AS transaction_id`

// paymentOwnerCondition 生成患者端的归属限定条件：在同一查询内用 EXISTS 限定
// 「挂号属于该患者名下的就诊卡」，使他人订单与不存在的订单返回同一个错误，
// 调用方无法据此枚举患者数据（契约 §1.2）；ownerPatientID < 1 表示管理端不限定归属。
func paymentOwnerCondition(args []any, ownerPatientID int64) (string, []any) {
	if ownerPatientID < 1 {
		return "", args
	}
	args = append(args, ownerPatientID)
	return fmt.Sprintf(` AND EXISTS (SELECT 1 FROM hospital.patient_user_info_card c
WHERE c.id = r.patient_card_id AND c.user_id = $%d)`, len(args)), args
}

// FindPaymentByRegistrationID 按挂号编号读取支付信息（契约 §6.5）。
func (r *PostgresPaymentRepository) FindPaymentByRegistrationID(
	ctx context.Context,
	registrationID int64,
	ownerPatientID int64,
) (*payment.Payment, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	query := `SELECT ` + paymentColumns + ` FROM hospital.medical_registration r WHERE r.id = $1`
	args := []any{registrationID}
	ownerCondition, args := paymentOwnerCondition(args, ownerPatientID)
	query += ownerCondition
	return r.scanPayment(r.db.QueryRowContext(ctx, query, args...))
}

// FindPaymentByOutTradeNo 按外部交易号读取支付信息（契约 §6.6、§6.7）。
//
// out_trade_no 是定长 CHAR(32)，比较前两侧都做 btrim，避免补位空格造成匹配失败。
func (r *PostgresPaymentRepository) FindPaymentByOutTradeNo(
	ctx context.Context,
	outTradeNo string,
	ownerPatientID int64,
) (*payment.Payment, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	query := `SELECT ` + paymentColumns + ` FROM hospital.medical_registration r
WHERE btrim(r.out_trade_no) = btrim($1)`
	args := []any{outTradeNo}
	ownerCondition, args := paymentOwnerCondition(args, ownerPatientID)
	query += ownerCondition
	return r.scanPayment(r.db.QueryRowContext(ctx, query, args...))
}

// ensurePaymentWindowQuery 幂等补齐三个支付时间点。
//
// 建单流程（契约 §6.2）已在建单 INSERT 内用数据库 now() 写定这三个字段，因此对新建订单本
// 语句不产生写入；它只用于创建支付订单接口（契约 §6.9）补齐历史订单的支付窗口：COALESCE
// 保证已有值不被覆盖，且三个表达式引用的是同一行旧值，因此 pay_deadline、expire_at 与
// precreate_at 仍保持 +30、+35 分钟的固定关系（契约 §6.8）。只更新存在缺失的行，
// 已写定窗口的订单不会被反复写。
//
// 条件里的 payment_status = 未付款 用于把写入收敛在「仍可支付」的订单上：
// 已 PAID/EXPIRED/REFUNDED 的历史脏数据行即使三个时间点为空也不得补窗口，
// 否则会在终态订单上凭空生成一个新的支付窗口。
const ensurePaymentWindowQuery = `UPDATE hospital.medical_registration
SET precreate_at = COALESCE(precreate_at, now()),
    pay_deadline  = COALESCE(pay_deadline, COALESCE(precreate_at, now()) + interval '30 minutes'),
    expire_at     = COALESCE(expire_at, COALESCE(precreate_at, now()) + interval '35 minutes')
WHERE btrim(out_trade_no) = btrim($1)
  AND payment_status = $2
  AND (precreate_at IS NULL OR pay_deadline IS NULL OR expire_at IS NULL)`

// EnsurePaymentWindow 补齐支付窗口后返回最新支付信息。
func (r *PostgresPaymentRepository) EnsurePaymentWindow(
	ctx context.Context,
	outTradeNo string,
) (*payment.Payment, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	if _, err := r.db.ExecContext(ctx, ensurePaymentWindowQuery,
		outTradeNo, domainregistration.PaymentCodeUnpaid); err != nil {
		return nil, err
	}
	return r.FindPaymentByOutTradeNo(ctx, outTradeNo, 0)
}

// savePrepayIDQuery 回写付款二维码内容；只写入当前为空的行，重复调用不覆盖已有二维码。
const savePrepayIDQuery = `UPDATE hospital.medical_registration
SET prepay_id = $2
WHERE btrim(out_trade_no) = btrim($1)
  AND (prepay_id IS NULL OR btrim(prepay_id) = '')`

// SavePrepayID 回写付款二维码内容；只写入当前为空的行，重复调用不覆盖已有二维码。
// 返回值表示本次是否真正写入（false 表示二维码已由并发请求先行写入），
// 调用方据此决定响应里回显哪一个二维码，避免内存值与库中记录不一致（契约 §6.5、§6.2）。
func (r *PostgresPaymentRepository) SavePrepayID(
	ctx context.Context,
	outTradeNo string,
	prepayID string,
) (bool, error) {
	if r == nil || r.db == nil {
		return false, sql.ErrConnDone
	}
	result, err := r.db.ExecContext(ctx, savePrepayIDQuery, outTradeNo, prepayID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// markPaidQuery 是 UNPAID -> PAID 的条件更新语句：$1 out_trade_no、$2 交易号、
// $3 目标状态（已付款）、$4 前置状态（未付款）。
const markPaidQuery = `UPDATE hospital.medical_registration
SET payment_status = $3, transaction_id = $2
WHERE btrim(out_trade_no) = btrim($1) AND payment_status = $4`

// MarkPaid 以条件更新完成 UNPAID -> PAID 迁移并写入支付宝交易号。
//
// 条件 payment_status = 未付款 是幂等的唯一凭据：返回 false 表示状态已被通知路径、
// 兜底查询或收口任务改写，本次调用不得产生任何副作用（契约 §6.7、§9）。
// 本方法不释放号源：支付成功必须保持号源占用。
func (r *PostgresPaymentRepository) MarkPaid(
	ctx context.Context,
	outTradeNo string,
	transactionID string,
) (bool, error) {
	if r == nil || r.db == nil {
		return false, sql.ErrConnDone
	}
	result, err := r.db.ExecContext(ctx, markPaidQuery,
		outTradeNo, transactionIDParam(transactionID),
		domainregistration.PaymentCodePaid, domainregistration.PaymentCodeUnpaid)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// transactionIDParam 把空的支付宝交易号转换为 NULL：契约要求未支付时 transactionId 为 null，
// 空串写进 CHAR(32) 列会变成一串补位空格，读取时还要再次 btrim 归一，不如直接写 NULL 干净。
func transactionIDParam(transactionID string) any {
	if strings.TrimSpace(transactionID) == "" {
		return nil
	}
	return transactionID
}

// scanPayment 把一行挂号记录的支付字段转换为支付视图；
// 订单不存在（含患者端越权被归属条件过滤）统一返回 payment.ErrPaymentNotFound。
func (r *PostgresPaymentRepository) scanPayment(scanner registrationScanner) (*payment.Payment, error) {
	var (
		item                             payment.Payment
		outTradeNo, amount, prepayID     sql.NullString
		transactionID                    sql.NullString
		paymentStatus                    sql.NullInt16
		precreateAt, payDeadline, expiry sql.NullTime
	)
	err := scanner.Scan(
		&item.RegistrationID, &outTradeNo, &amount, &paymentStatus, &prepayID,
		&precreateAt, &payDeadline, &expiry, &transactionID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, payment.ErrPaymentNotFound
	}
	if err != nil {
		return nil, err
	}
	item.OutTradeNo = outTradeNo.String
	item.Amount = registrationAmount(amount)
	item.PaymentStatus = domainregistration.PaymentStatusLabel(nullInt16(paymentStatus))
	item.PrepayID = prepayID.String
	item.PrecreateAt = precreateAt.Time
	item.PayDeadline = payDeadline.Time
	item.ExpireAt = expiry.Time
	item.TransactionID = transactionID.String
	return &item, nil
}
