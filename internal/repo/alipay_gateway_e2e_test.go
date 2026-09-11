package repo

// 本文件是支付模块的外部联调（E2E）测试，覆盖三个目标：
//
//	目标 A：用真实凭据调用支付宝网关（默认跳过，设置 ALIPAY_E2E=1 才执行），证明
//	        「本服务的请求能到支付宝」并能读到结构化响应；
//	目标 B：不依赖外网的离线用例，证明「支付宝异步通知能返回 success」——签名拼接口径
//	        按契约 §6.7 在测试内独立实现（刻意不调用被测代码的私有函数，避免自证），
//	        覆盖 VerifyNotify、useCase.HandleNotify 与 HTTP handler 三层；
//	目标 C：在真实 PostgreSQL 上验证支付仓储 SQL 可执行（默认跳过，设置 PAYMENT_DB_E2E=1
//	        才执行），所有写入都在事务内完成并回滚，不留任何痕迹。
//
// 运行方式（工作树根目录，cmd，注意 set 的引号避免尾随空格）：
//
//	go test ./internal/repo/ -run Alipay -v
//	set "ALIPAY_E2E=1" && go test ./internal/repo/ -run AlipayE2E -v
//	set "PAYMENT_DB_E2E=1" && go test ./internal/repo/ -run PaymentDB -v
//
// 安全约束：本文件只使用 .env 中的真实凭据发起调用，绝不打印、不落盘任何私钥内容。

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"

	"Medical-Web-Backend/internal/config"
	domainpayment "Medical-Web-Backend/internal/domain/payment"
	domainregistration "Medical-Web-Backend/internal/domain/registration"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/handler"
	paymentservice "Medical-Web-Backend/internal/usecase/payment"
)

// e2eNotifyPath 是支付宝异步通知的契约路径（契约 §6.7）。
const e2eNotifyPath = "/api/v1/payments/alipay/notify"

// ============================ 目标 A：真实网关联调 ============================

// TestAlipayE2EPrecreateReachesGateway 证明「本服务的请求能到支付宝」。
//
// 默认跳过，只有显式设置 ALIPAY_E2E=1 才真正发起外网调用；调用失败时把网关/网络返回的
// 原始错误逐字打出来，用于区分三个失败层级：网络不可达、网关拒绝、支付宝业务码拒绝。
// 不伪造成功：只要拿不到 qr_code，本用例就失败。
func TestAlipayE2EPrecreateReachesGateway(t *testing.T) {
	if os.Getenv("ALIPAY_E2E") != "1" {
		t.Skip("未设置 ALIPAY_E2E=1，跳过支付宝真实网关联调（默认不访问外网）")
	}
	env := e2eLoadDotEnv(t)
	appID := strings.TrimSpace(env["ALIPAY_APP_ID"])
	privateKey := env["ALIPAY_APP_PRIVATE_SECRET"]
	gatewayURL := strings.TrimSpace(env["ALIPAY_GATEWAY_URL"])
	notifyURL := strings.TrimSpace(env["NOTIFY_URL"])
	if appID == "" || privateKey == "" || gatewayURL == "" {
		t.Fatalf(".env 缺少 ALIPAY_APP_ID/ALIPAY_APP_PRIVATE_SECRET/ALIPAY_GATEWAY_URL，无法发起联调")
	}

	gateway := NewAlipayGateway(AlipayOptions{
		AppID:      appID,
		PrivateKey: privateKey,
		PublicKey:  env["ALIPAY_PUBLIC_SECRET"],
		GatewayURL: gatewayURL,
		SellerIDs:  nil,
		Timeout:    10 * time.Second,
	})
	// out_trade_no 必须不超过 out_trade_no 列的 32 字符：E2E + 14 位时间戳 = 17 字符。
	outTradeNo := "E2E" + time.Now().In(alipayLocation).Format("20060102150405")
	t.Logf("联调参数：gateway=%s app_id=%s out_trade_no=%s notify_url=%s",
		gatewayURL, appID, outTradeNo, notifyURL)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := gateway.Precreate(ctx, domainpayment.PrecreateRequest{
		OutTradeNo:     outTradeNo,
		Amount:         "0.01",
		Subject:        "医院挂号费",
		TimeoutExpress: "30m",
		NotifyURL:      notifyURL,
	})
	if err != nil {
		// 原样输出错误：业务码拒绝会带 code/sub_code/msg/sub_msg，网络故障会带底层网络错误。
		t.Errorf("Precreate 未拿到 qr_code，原始错误：%v", err)
		return
	}
	t.Logf("Precreate 成功：已拿到 qr_code（长度=%d），请求/签名/网关/响应验签全部通过；out_trade_no=%s",
		len(result.QRCode), outTradeNo)
}

// ========================= 目标 B：离线通知验签与回调 =========================

// TestAlipayOfflineNotifyVerifyAndHandleReturnsSuccess 用 .env 的应用私钥推导公钥，
// 离线验证通知验签、字段解析、useCase 处理与 HTTP handler 的 success 响应。
//
// 关键点：待签串的拼接口径（剔除 sign/sign_type 与空值、参数名升序、k=v 用 & 连接）
// 由本文件按契约 §6.7 独立实现，不复用实现代码中的 buildSignContent，避免自证。
func TestAlipayOfflineNotifyVerifyAndHandleReturnsSuccess(t *testing.T) {
	env := e2eLoadDotEnv(t)
	appID := strings.TrimSpace(env["ALIPAY_APP_ID"])
	if appID == "" || strings.TrimSpace(env["ALIPAY_APP_PRIVATE_SECRET"]) == "" {
		// .env 与进程环境变量都取不到凭据时跳过：保证干净克隆/CI 上 go test ./... 全绿。
		t.Skip(".env 与进程环境变量都没有 ALIPAY_APP_ID/ALIPAY_APP_PRIVATE_SECRET，跳过离线通知验签用例")
	}
	privateKey := e2eParsePrivateKey(t, env["ALIPAY_APP_PRIVATE_SECRET"])

	// 只用应用私钥推导出的公钥构造 gateway：验证的是「我们自己按契约拼的串」能被验签通过。
	gateway := NewAlipayGateway(AlipayOptions{
		AppID:     appID,
		PublicKey: e2ePublicKeyBase64(t, privateKey),
		SellerIDs: nil,
	})

	outTradeNo := "E2ENOTIFY" + time.Now().Format("20060102150405")
	form := e2eSignedNotifyForm(t, appID, privateKey, outTradeNo, "0.01")

	t.Run("VerifyNotify 通过并正确解析字段", func(t *testing.T) {
		payload, err := gateway.VerifyNotify(context.Background(), form)
		if err != nil {
			t.Fatalf("VerifyNotify 期望通过，实际返回错误：%v", err)
		}
		checks := []struct {
			label string
			got   string
			want  string
		}{
			{"app_id", payload.AppID, appID},
			{"seller_id", payload.SellerID, "2088621952868234"},
			{"out_trade_no", payload.OutTradeNo, outTradeNo},
			{"trade_no", payload.TradeNo, "2026091122001430000000000001"},
			{"trade_status", payload.TradeStatus, domainpayment.TradeStatusSuccess},
			{"total_amount", payload.TotalAmount, "0.01"},
			{"gmt_payment", payload.GmtPayment, "2026-09-11 18:00:01"},
			{"notify_time", payload.NotifyTime, "2026-09-11 18:00:02"},
		}
		for _, check := range checks {
			if check.got != check.want {
				t.Errorf("字段 %s 解析结果 = %q，期望 %q", check.label, check.got, check.want)
			}
		}
	})

	t.Run("篡改金额后验签必须失败", func(t *testing.T) {
		tampered := e2eCloneForm(form)
		tampered.Set("total_amount", "0.02")
		if _, err := gateway.VerifyNotify(context.Background(), tampered); !errors.Is(err, port.ErrNotifySignatureInvalid) {
			t.Fatalf("篡改 total_amount 后期望 ErrNotifySignatureInvalid，实际：%v", err)
		}
	})

	t.Run("HandleNotify 返回成功并完成 UNPAID 到 PAID 迁移", func(t *testing.T) {
		repository := e2eNewMemoryPaymentRepository(&domainpayment.Payment{
			RegistrationID: 1,
			OutTradeNo:     outTradeNo,
			Amount:         "0.01",
			PaymentStatus:  domainpayment.PaymentStatusUnpaid,
			ExpireAt:       time.Now().UTC().Add(time.Hour),
		})
		service := paymentservice.NewService(repository, gateway, paymentservice.Config{
			PaymentSubject: "医院挂号费",
		})
		outcome, err := service.HandleNotify(context.Background(), form)
		if err != nil {
			t.Fatalf("HandleNotify 期望成功，实际返回错误：%v", err)
		}
		if outcome == nil || !outcome.Marked {
			t.Fatalf("HandleNotify 未完成状态迁移，outcome=%+v", outcome)
		}
		paid, err := repository.FindPaymentByOutTradeNo(context.Background(), outTradeNo, 0)
		if err != nil {
			t.Fatalf("读取迁移后的订单失败：%v", err)
		}
		if paid.PaymentStatus != domainpayment.PaymentStatusPaid {
			t.Errorf("迁移后 payment_status = %s，期望 %s", paid.PaymentStatus, domainpayment.PaymentStatusPaid)
		}
		if paid.TransactionID != "2026091122001430000000000001" {
			t.Errorf("迁移后 transaction_id = %q，期望支付宝交易号", paid.TransactionID)
		}
	})

	t.Run("handler 对合法通知返回纯文本 success", func(t *testing.T) {
		repository := e2eNewMemoryPaymentRepository(&domainpayment.Payment{
			RegistrationID: 1,
			OutTradeNo:     outTradeNo,
			Amount:         "0.01",
			PaymentStatus:  domainpayment.PaymentStatusUnpaid,
			ExpireAt:       time.Now().UTC().Add(time.Hour),
		})
		recorder := e2ePostNotify(t, repository, gateway, form)
		if recorder.Code != http.StatusOK {
			t.Fatalf("handler 状态码 = %d，期望 200，响应体=%s", recorder.Code, recorder.Body.String())
		}
		if body := recorder.Body.String(); body != "success" {
			t.Errorf("handler 响应体 = %q，期望恰好是 success（无引号、无 JSON 包裹）", body)
		}
		if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
			t.Errorf("handler Content-Type = %q，期望 text/plain", contentType)
		}
	})
}

// TestAlipayOfflineNotifyTamperedSignatureRejectedByHandler 断言签名被篡改时，
// handler 返回 400 与统一 envelope 中的 PAYMENT_NOTIFY_SIGNATURE_INVALID（契约 §6.7 第 1 步）。
func TestAlipayOfflineNotifyTamperedSignatureRejectedByHandler(t *testing.T) {
	env := e2eLoadDotEnv(t)
	appID := strings.TrimSpace(env["ALIPAY_APP_ID"])
	if appID == "" || strings.TrimSpace(env["ALIPAY_APP_PRIVATE_SECRET"]) == "" {
		// 同上：没有凭据就不构造离线用例，跳过而不是失败。
		t.Skip(".env 与进程环境变量都没有 ALIPAY_APP_ID/ALIPAY_APP_PRIVATE_SECRET，跳过篡改签名通知用例")
	}
	privateKey := e2eParsePrivateKey(t, env["ALIPAY_APP_PRIVATE_SECRET"])
	gateway := NewAlipayGateway(AlipayOptions{
		AppID:     appID,
		PublicKey: e2ePublicKeyBase64(t, privateKey),
		SellerIDs: nil,
	})

	outTradeNo := "E2EBADSIGN" + time.Now().Format("20060102150405")
	form := e2eSignedNotifyForm(t, appID, privateKey, outTradeNo, "0.01")
	// 篡改签名本身：换成一段长度不足的 base64，RSA 验签必然失败。
	form.Set("sign", base64.StdEncoding.EncodeToString([]byte("tampered-signature")))

	repository := e2eNewMemoryPaymentRepository(&domainpayment.Payment{
		RegistrationID: 1,
		OutTradeNo:     outTradeNo,
		Amount:         "0.01",
		PaymentStatus:  domainpayment.PaymentStatusUnpaid,
		ExpireAt:       time.Now().UTC().Add(time.Hour),
	})
	recorder := e2ePostNotify(t, repository, gateway, form)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("篡改签名后 handler 状态码 = %d，期望 400，响应体=%s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("响应体不是 JSON envelope：%v，原文=%s", err, recorder.Body.String())
	}
	if envelope.Code != "PAYMENT_NOTIFY_SIGNATURE_INVALID" {
		t.Errorf("错误码 = %q，期望 PAYMENT_NOTIFY_SIGNATURE_INVALID（响应体=%s）", envelope.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("失败响应 Content-Type = %q，期望 application/json", contentType)
	}
}

// ================= 目标 C：真实 PostgreSQL 上的支付仓储 SQL =================

// 说明：目标 C 不再保留「等价 SQL 副本」，直接引用生产常量 ensurePaymentWindowQuery、
// savePrepayIDQuery、markPaidQuery（审查 P2-7）。副本会与实现静默漂移——例如实现新增了
// `AND payment_status = 未付款` 状态守卫而副本没跟上时，用例照样全绿，等于没有覆盖。

// e2eInsertRegistrationSQL 插入一行只带本用例所需字段的挂号记录（其余列保持 NULL）。
const e2eInsertRegistrationSQL = `INSERT INTO hospital.medical_registration
	(id, out_trade_no, amount, payment_status,
	 patient_card_id, work_plan_id, doctor_schedule_id, doctor_id, dept_sub_id,
	 date, slot, create_time, prepay_id, transaction_id,
	 precreate_at, pay_deadline, expire_at)
VALUES ($1, $2, 50.00, 1,
	NULL, NULL, NULL, NULL, NULL,
	NULL, NULL, NULL, NULL, NULL,
	NULL, NULL, NULL)`

// e2eInsertFinalStatusRegistrationSQL 插入一行 payment_status 已是终态（已付款=2）且三个
// 时间点仍为 NULL 的挂号记录，专门用于验证 ensurePaymentWindowQuery 的状态守卫：
// 终态订单即使缺时间点也不得被补出新的支付窗口。
const e2eInsertFinalStatusRegistrationSQL = `INSERT INTO hospital.medical_registration
        (id, out_trade_no, amount, payment_status,
         patient_card_id, work_plan_id, doctor_schedule_id, doctor_id, dept_sub_id,
         date, slot, create_time, prepay_id, transaction_id,
         precreate_at, pay_deadline, expire_at)
VALUES ($1, $2, 50.00, 2,
        NULL, NULL, NULL, NULL, NULL,
        NULL, NULL, NULL, NULL, NULL,
        NULL, NULL, NULL)`

// TestPaymentDBMedicalRegistrationSQL 在真实 PostgreSQL 上执行支付仓储的生产 SQL。
//
// 全程只用一个事务：插入夹具行、跑完断言后显式回滚，并对回滚结果做全库复核，确保零痕迹。
// 默认跳过，设置 PAYMENT_DB_E2E=1 才执行；开关已开启但数据库不可达时直接失败并报告层级，
// 不伪装成通过。
func TestPaymentDBMedicalRegistrationSQL(t *testing.T) {
	if os.Getenv("PAYMENT_DB_E2E") != "1" {
		t.Skip("未设置 PAYMENT_DB_E2E=1，跳过支付仓储 SQL 的真实数据库验证")
	}
	if err := godotenv.Load("../../.env"); err != nil {
		t.Fatalf("加载 .env 失败（失败层级：配置/文件）：%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置失败（失败层级：配置解析）：%v", err)
	}
	dsn := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.Postgres.Username, cfg.Postgres.Password),
		Host:   cfg.Postgres.Addr,
		Path:   cfg.Postgres.Database,
	}
	query := dsn.Query()
	query.Set("sslmode", cfg.Postgres.SSLMode)
	dsn.RawQuery = query.Encode()

	db, err := sql.Open("pgx", dsn.String())
	if err != nil {
		t.Fatalf("打开 PostgreSQL 连接失败（失败层级：驱动/DSN）：%v", err)
	}
	defer func() { _ = db.Close() }()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		t.Fatalf("连接 PostgreSQL 失败（失败层级：网络/DNS/认证，addr=%s db=%s）：%v",
			cfg.Postgres.Addr, cfg.Postgres.Database, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务失败（失败层级：数据库）：%v", err)
	}
	// 断言全部在事务内完成，最后统一回滚；绝不 Commit。
	defer func() { _ = tx.Rollback() }()

	// 第一步先实测 NOT NULL 约束，避免 INSERT 失败时把「约束变化」误判为「SQL 不可执行」。
	notNullColumns := e2eNotNullColumns(t, ctx, tx, "hospital", "medical_registration")
	t.Logf("hospital.medical_registration 的 NOT NULL 列：%v", notNullColumns)

	now := time.Now()
	// id 取避开现网数据范围的高位值；out_trade_no 用 E2EDB 前缀，便于人工识别夹具。
	registrationID := int64(2100000000 + now.Unix()%100000)
	outTradeNo := "E2EDB" + now.Format("20060102150405")
	transactionID := "E2ETXN" + now.Format("20060102150405") // 20 字符，不超过 CHAR(32)

	if _, err := tx.ExecContext(ctx, e2eInsertRegistrationSQL, registrationID, outTradeNo); err != nil {
		t.Fatalf("插入测试行失败（失败层级：SQL/约束，NOT NULL 列=%v）：%v", notNullColumns, err)
	}
	t.Logf("事务内已插入夹具行：id=%d out_trade_no=%s amount=50.00 payment_status=1 三个时间点=NULL",
		registrationID, outTradeNo)

	// ---------- EnsurePaymentWindow：直接引用生产常量 ----------
	result, err := tx.ExecContext(ctx, ensurePaymentWindowQuery, outTradeNo, domainregistration.PaymentCodeUnpaid)
	if err != nil {
		t.Fatalf("EnsurePaymentWindow 生产 SQL 执行失败：%v", err)
	}
	e2eAssertAffected(t, result, 1, "EnsurePaymentWindow 首次执行")
	first := e2eReadPaymentRow(t, ctx, tx, outTradeNo)
	if !first.precreateAt.Valid || !first.payDeadline.Valid || !first.expireAt.Valid {
		t.Fatalf("补齐后仍有空时间点：precreate_at=%v pay_deadline=%v expire_at=%v",
			first.precreateAt, first.payDeadline, first.expireAt)
	}
	e2eAssertDurationNear(t, first.payDeadline.Time.Sub(first.precreateAt.Time),
		30*time.Minute, 5*time.Second, "pay_deadline 与 precreate_at 的差")
	e2eAssertDurationNear(t, first.expireAt.Time.Sub(first.precreateAt.Time),
		35*time.Minute, 5*time.Second, "expire_at 与 precreate_at 的差")
	t.Logf("EnsurePaymentWindow 首次执行后：precreate_at=%s pay_deadline=%s expire_at=%s",
		first.precreateAt.Time.Format(time.RFC3339), first.payDeadline.Time.Format(time.RFC3339),
		first.expireAt.Time.Format(time.RFC3339))

	result, err = tx.ExecContext(ctx, ensurePaymentWindowQuery, outTradeNo, domainregistration.PaymentCodeUnpaid)
	if err != nil {
		t.Fatalf("EnsurePaymentWindow 幂等执行失败：%v", err)
	}
	e2eAssertAffected(t, result, 0, "EnsurePaymentWindow 二次执行")
	second := e2eReadPaymentRow(t, ctx, tx, outTradeNo)
	if !second.precreateAt.Time.Equal(first.precreateAt.Time) ||
		!second.payDeadline.Time.Equal(first.payDeadline.Time) ||
		!second.expireAt.Time.Equal(first.expireAt.Time) {
		t.Fatalf("EnsurePaymentWindow 二次执行改写了时间点：首次=%s/%s/%s 二次=%s/%s/%s",
			first.precreateAt.Time, first.payDeadline.Time, first.expireAt.Time,
			second.precreateAt.Time, second.payDeadline.Time, second.expireAt.Time)
	}
	t.Log("EnsurePaymentWindow 幂等验证通过：二次执行 affected=0 且三个时间点未被改写")

	// ---------- SavePrepayID：直接引用生产常量 ----------
	result, err = tx.ExecContext(ctx, savePrepayIDQuery, outTradeNo, "qr-code-1")
	if err != nil {
		t.Fatalf("SavePrepayID 生产 SQL 首次执行失败：%v", err)
	}
	e2eAssertAffected(t, result, 1, "SavePrepayID 首次执行")
	row := e2eReadPaymentRow(t, ctx, tx, outTradeNo)
	if row.prepayID.String != "qr-code-1" {
		t.Fatalf("SavePrepayID 首次写入后 prepay_id = %q，期望 qr-code-1", row.prepayID.String)
	}

	result, err = tx.ExecContext(ctx, savePrepayIDQuery, outTradeNo, "qr-code-2")
	if err != nil {
		t.Fatalf("SavePrepayID 生产 SQL 二次执行失败：%v", err)
	}
	e2eAssertAffected(t, result, 0, "SavePrepayID 二次执行")
	row = e2eReadPaymentRow(t, ctx, tx, outTradeNo)
	if row.prepayID.String != "qr-code-1" {
		t.Fatalf("SavePrepayID 二次写入覆盖了已有二维码：prepay_id = %q，期望仍是 qr-code-1", row.prepayID.String)
	}
	t.Log("SavePrepayID 验证通过：首写成功，二次写入不覆盖")

	// ---------- MarkPaid：直接引用生产常量 ----------
	result, err = tx.ExecContext(ctx, markPaidQuery, outTradeNo, transactionID,
		domainregistration.PaymentCodePaid, domainregistration.PaymentCodeUnpaid)
	if err != nil {
		t.Fatalf("MarkPaid 生产 SQL 首次执行失败：%v", err)
	}
	e2eAssertAffected(t, result, 1, "MarkPaid 首次执行")
	row = e2eReadPaymentRow(t, ctx, tx, outTradeNo)
	if !row.paymentStatus.Valid || row.paymentStatus.Int16 != domainregistration.PaymentCodePaid {
		t.Fatalf("MarkPaid 首次执行后 payment_status = %v，期望 %d",
			row.paymentStatus, domainregistration.PaymentCodePaid)
	}
	if row.transactionID.String != transactionID {
		t.Fatalf("MarkPaid 首次执行后 transaction_id = %q，期望 %q", row.transactionID.String, transactionID)
	}

	result, err = tx.ExecContext(ctx, markPaidQuery, outTradeNo, transactionID,
		domainregistration.PaymentCodePaid, domainregistration.PaymentCodeUnpaid)
	if err != nil {
		t.Fatalf("MarkPaid 生产 SQL 二次执行失败：%v", err)
	}
	e2eAssertAffected(t, result, 0, "MarkPaid 二次执行")
	t.Log("MarkPaid 幂等验证通过：首次 affected=1 且状态与交易号写入，二次 affected=0")

	// ---------- 不存在的 out_trade_no ----------
	result, err = tx.ExecContext(ctx, markPaidQuery, "E2EDBNOTEXIST0000", transactionID,
		domainregistration.PaymentCodePaid, domainregistration.PaymentCodeUnpaid)
	if err != nil {
		t.Fatalf("MarkPaid 对不存在订单执行失败：%v", err)
	}
	e2eAssertAffected(t, result, 0, "MarkPaid 对不存在的 out_trade_no")
	t.Log("MarkPaid 对不存在的 out_trade_no 验证通过：affected=0")

	// ---------- EnsurePaymentWindow 状态守卫：终态订单不得补支付窗口 ----------
	// 覆盖生产语句新增的 `AND payment_status = 未付款` 条件（审查 P2-7）：
	// 已 PAID 的历史脏数据行即使三个时间点仍为 NULL，也必须 affected=0 且时间点保持 NULL，
	// 否则会在终态订单上凭空生成一个新的支付窗口。
	terminalOutTradeNo := "E2EDBPAID" + now.Format("20060102150405")
	terminalRegistrationID := registrationID + 1
	if _, err := tx.ExecContext(ctx, e2eInsertFinalStatusRegistrationSQL,
		terminalRegistrationID, terminalOutTradeNo); err != nil {
		t.Fatalf("插入终态夹具行失败（失败层级：SQL/约束）：%v", err)
	}
	result, err = tx.ExecContext(ctx, ensurePaymentWindowQuery,
		terminalOutTradeNo, domainregistration.PaymentCodeUnpaid)
	if err != nil {
		t.Fatalf("EnsurePaymentWindow 状态守卫 SQL 执行失败：%v", err)
	}
	e2eAssertAffected(t, result, 0, "EnsurePaymentWindow 对终态订单")
	terminalRow := e2eReadPaymentRow(t, ctx, tx, terminalOutTradeNo)
	if !terminalRow.paymentStatus.Valid ||
		terminalRow.paymentStatus.Int16 != domainregistration.PaymentCodePaid {
		t.Fatalf("终态夹具行 payment_status = %v，期望 %d",
			terminalRow.paymentStatus, domainregistration.PaymentCodePaid)
	}
	if terminalRow.precreateAt.Valid || terminalRow.payDeadline.Valid || terminalRow.expireAt.Valid {
		t.Fatalf("终态订单被补出了支付窗口：precreate_at=%v pay_deadline=%v expire_at=%v",
			terminalRow.precreateAt, terminalRow.payDeadline, terminalRow.expireAt)
	}
	t.Log("EnsurePaymentWindow 状态守卫验证通过：终态订单 affected=0 且三个时间点仍为 NULL")

	// ---------- 回滚并复核零痕迹 ----------
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("回滚事务失败：%v", err)
	}
	var residue int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM hospital.medical_registration
WHERE btrim(out_trade_no) = btrim($1) OR btrim(out_trade_no) = btrim($2)`,
		outTradeNo, terminalOutTradeNo).Scan(&residue); err != nil {
		t.Fatalf("回滚后复核残留失败：%v", err)
	}
	if residue != 0 {
		t.Fatalf("事务回滚后仍残留 %d 行夹具数据，未做到零痕迹", residue)
	}
	t.Log("事务已回滚，零痕迹复核通过（残留行数=0）")
}

// e2eNotNullColumns 读取指定表的 NOT NULL 列名，用于插入前确认约束情况。
func e2eNotNullColumns(t *testing.T, ctx context.Context, tx *sql.Tx, schema, table string) []string {
	t.Helper()
	rows, err := tx.QueryContext(ctx, `SELECT column_name FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2 AND is_nullable = 'NO' ORDER BY ordinal_position`,
		schema, table)
	if err != nil {
		t.Fatalf("读取 NOT NULL 约束失败：%v", err)
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("读取列名失败：%v", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历列名失败：%v", err)
	}
	return columns
}

// e2ePaymentRow 是目标 C 读回的单行支付字段快照。
type e2ePaymentRow struct {
	precreateAt   sql.NullTime
	payDeadline   sql.NullTime
	expireAt      sql.NullTime
	prepayID      sql.NullString
	transactionID sql.NullString
	paymentStatus sql.NullInt16
}

// e2eReadPaymentRow 在事务内读回夹具行的支付字段。
func e2eReadPaymentRow(t *testing.T, ctx context.Context, tx *sql.Tx, outTradeNo string) e2ePaymentRow {
	t.Helper()
	var row e2ePaymentRow
	err := tx.QueryRowContext(ctx, `SELECT precreate_at, pay_deadline, expire_at,
btrim(prepay_id), btrim(transaction_id), payment_status
FROM hospital.medical_registration WHERE btrim(out_trade_no) = btrim($1)`, outTradeNo).
		Scan(&row.precreateAt, &row.payDeadline, &row.expireAt,
			&row.prepayID, &row.transactionID, &row.paymentStatus)
	if err != nil {
		t.Fatalf("读取夹具行失败 out_trade_no=%s：%v", outTradeNo, err)
	}
	return row
}

// e2eAssertAffected 断言 SQL 的受影响行数。
func e2eAssertAffected(t *testing.T, result sql.Result, want int64, action string) {
	t.Helper()
	affected, err := result.RowsAffected()
	if err != nil {
		t.Fatalf("%s：读取受影响行数失败：%v", action, err)
	}
	if affected != want {
		t.Fatalf("%s：受影响行数 = %d，期望 %d", action, affected, want)
	}
}

// e2eAssertDurationNear 断言两个时刻的间隔落在期望值 ± 容差内（秒级误差）。
func e2eAssertDurationNear(t *testing.T, got, want, tolerance time.Duration, label string) {
	t.Helper()
	if diff := got - want; diff < -tolerance || diff > tolerance {
		t.Fatalf("%s：实际间隔 %s，期望 %s（容差 %s）", label, got, want, tolerance)
	}
}

// ============================ 辅助函数与内存仓储 ============================

// e2eLoadDotEnv 读取仓库根目录的 .env，并用同名进程环境变量覆盖（便于 CI 注入）。
//
// .env 不存在不算失败：干净克隆/CI 上本来就可能没有 .env，此时只依赖进程环境变量；
// 调用方在「.env 与进程环境变量都取不到凭据」时用 t.Skip 跳过，避免 go test ./... 变红。
// 测试进程的工作目录是包目录（internal/repo），因此用本文件路径反推仓库根目录。
func e2eLoadDotEnv(t *testing.T) map[string]string {
	t.Helper()
	values := map[string]string{}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试文件路径")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	envPath := filepath.Join(root, ".env")
	if raw, err := os.ReadFile(envPath); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			index := strings.Index(line, "=")
			if index <= 0 {
				continue
			}
			key := strings.TrimSpace(line[:index])
			value := strings.TrimSpace(line[index+1:])
			value = strings.Trim(value, "\"'")
			values[key] = value
		}
	} else {
		// 只提示不失败：凭据缺失由调用方用 t.Skip 处理，不是测试失败。
		t.Logf("提示：未读取到 %s（%v），本次只使用进程环境变量", envPath, err)
	}
	for _, key := range []string{
		"ALIPAY_APP_ID", "ALIPAY_APP_PRIVATE_SECRET", "ALIPAY_PUBLIC_SECRET",
		"ALIPAY_GATEWAY_URL", "NOTIFY_URL",
	} {
		if override := strings.TrimSpace(os.Getenv(key)); override != "" {
			values[key] = override
		}
	}
	return values
}

// e2eDecodePEM 把裸 base64 或带 PEM 头尾的密钥还原为 DER 字节（独立实现，不复用被测代码）。
func e2eDecodePEM(raw, blockType string) ([]byte, error) {
	content := strings.TrimSpace(raw)
	if content == "" {
		return nil, errors.New("密钥为空")
	}
	if !strings.Contains(content, "-----BEGIN") {
		var builder strings.Builder
		builder.WriteString("-----BEGIN " + blockType + "-----\n")
		for index := 0; index < len(content); index += 64 {
			end := index + 64
			if end > len(content) {
				end = len(content)
			}
			builder.WriteString(content[index:end])
			builder.WriteString("\n")
		}
		builder.WriteString("-----END " + blockType + "-----\n")
		content = builder.String()
	}
	block, _ := pem.Decode([]byte(content))
	if block == nil {
		return nil, errors.New("密钥不是合法的 PEM 格式")
	}
	return block.Bytes, nil
}

// e2eParsePrivateKey 解析应用私钥（兼容 PKCS#8 与 PKCS#1），失败时终止用例且不回显密钥内容。
func e2eParsePrivateKey(t *testing.T, raw string) *rsa.PrivateKey {
	t.Helper()
	der, err := e2eDecodePEM(raw, "PRIVATE KEY")
	if err != nil {
		t.Fatalf("解析应用私钥失败：%v", err)
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			t.Fatal("应用私钥不是 RSA 私钥")
		}
		return key
	}
	key, err := x509.ParsePKCS1PrivateKey(der)
	if err != nil {
		t.Fatalf("应用私钥既不是 PKCS#8 也不是 PKCS#1：%v", err)
	}
	return key
}

// e2ePublicKeyBase64 从应用私钥推导公钥，并编码为裸 base64（PKIX DER），
// 供 NewAlipayGateway 的 PublicKey 参数使用。
func e2ePublicKeyBase64(t *testing.T, privateKey *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(privateKey.Public())
	if err != nil {
		t.Fatalf("导出公钥失败：%v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// e2eSignContent 按契约 §6.7 独立实现待签串：剔除 sign、sign_type 与空值参数后，
// 按参数名升序用 & 连接 k=v（值取 URL 解码后的原始值，不做二次编码）。
func e2eSignContent(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for key, value := range params {
		if key == "sign" || key == "sign_type" || value == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+params[key])
	}
	return strings.Join(parts, "&")
}

// e2eSignRSA2 用应用私钥做 SHA256withRSA 签名并 base64 编码。
func e2eSignRSA2(t *testing.T, privateKey *rsa.PrivateKey, content string) string {
	t.Helper()
	hashed := sha256.Sum256([]byte(content))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatalf("RSA2 签名失败：%v", err)
	}
	return base64.StdEncoding.EncodeToString(signature)
}

// e2eSignedNotifyForm 构造一份真实形状的支付宝异步通知表单并完成签名。
func e2eSignedNotifyForm(
	t *testing.T,
	appID string,
	privateKey *rsa.PrivateKey,
	outTradeNo string,
	totalAmount string,
) url.Values {
	t.Helper()
	params := map[string]string{
		"app_id":       appID,
		"seller_id":    "2088621952868234",
		"out_trade_no": outTradeNo,
		"trade_no":     "2026091122001430000000000001",
		"trade_status": domainpayment.TradeStatusSuccess,
		"total_amount": totalAmount,
		"gmt_payment":  "2026-09-11 18:00:01",
		"notify_time":  "2026-09-11 18:00:02",
		"sign_type":    "RSA2",
	}
	params["sign"] = e2eSignRSA2(t, privateKey, e2eSignContent(params))
	form := url.Values{}
	for key, value := range params {
		form.Set(key, value)
	}
	return form
}

// e2eCloneForm 复制一份表单，避免篡改用例污染原表单。
func e2eCloneForm(form url.Values) url.Values {
	cloned := url.Values{}
	for key, values := range form {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

// e2ePostNotify 用 httptest 直接调用 handler 的异步通知入口（表单 POST）。
func e2ePostNotify(
	t *testing.T,
	repository port.PaymentRepository,
	gateway port.AlipayGateway,
	form url.Values,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service := paymentservice.NewService(repository, gateway, paymentservice.Config{
		PaymentSubject: "医院挂号费",
	})
	router := gin.New()
	router.POST(e2eNotifyPath, handler.NewPaymentHandler(service).Notify)

	request := httptest.NewRequest(http.MethodPost, e2eNotifyPath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// e2eMemoryPaymentRepository 是 port.PaymentRepository 的内存实现，仅供离线用例使用：
// 覆盖通知处理真正会走到的读取与条件更新路径。
type e2eMemoryPaymentRepository struct {
	mu       sync.Mutex
	payments map[string]*domainpayment.Payment
}

// e2eNewMemoryPaymentRepository 用给定订单构造内存仓储。
func e2eNewMemoryPaymentRepository(items ...*domainpayment.Payment) *e2eMemoryPaymentRepository {
	repository := &e2eMemoryPaymentRepository{payments: map[string]*domainpayment.Payment{}}
	for _, item := range items {
		cloned := *item
		repository.payments[item.OutTradeNo] = &cloned
	}
	return repository
}

// FindPaymentByRegistrationID 按挂号编号读取；ownerPatientID > 0 时按「不属于该患者」处理。
func (r *e2eMemoryPaymentRepository) FindPaymentByRegistrationID(
	_ context.Context,
	registrationID int64,
	ownerPatientID int64,
) (*domainpayment.Payment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.payments {
		if item.RegistrationID == registrationID {
			// 内存仓储没有就诊卡归属信息，按契约用「不存在」统一收敛越权读取。
			if ownerPatientID > 0 {
				return nil, domainpayment.ErrPaymentNotFound
			}
			cloned := *item
			return &cloned, nil
		}
	}
	return nil, domainpayment.ErrPaymentNotFound
}

// FindPaymentByOutTradeNo 按外部交易号读取。
func (r *e2eMemoryPaymentRepository) FindPaymentByOutTradeNo(
	_ context.Context,
	outTradeNo string,
	ownerPatientID int64,
) (*domainpayment.Payment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.payments[outTradeNo]
	if !ok || ownerPatientID > 0 {
		return nil, domainpayment.ErrPaymentNotFound
	}
	cloned := *item
	return &cloned, nil
}

// EnsurePaymentWindow 幂等补齐支付窗口（内存实现按固定 30/35 分钟推导）。
func (r *e2eMemoryPaymentRepository) EnsurePaymentWindow(
	_ context.Context,
	outTradeNo string,
) (*domainpayment.Payment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.payments[outTradeNo]
	if !ok {
		return nil, domainpayment.ErrPaymentNotFound
	}
	if item.PrecreateAt.IsZero() {
		item.PrecreateAt = time.Now().UTC()
		item.PayDeadline = item.PrecreateAt.Add(30 * time.Minute)
		item.ExpireAt = item.PrecreateAt.Add(35 * time.Minute)
	}
	cloned := *item
	return &cloned, nil
}

// SavePrepayID 只在当前为空时写入，重复调用不覆盖；返回值表示本次是否真正写入
// （与生产实现一致：通知路径不关心该值，但端口签名必须对齐）。
func (r *e2eMemoryPaymentRepository) SavePrepayID(
	_ context.Context,
	outTradeNo string,
	prepayID string,
) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.payments[outTradeNo]
	if !ok {
		return false, domainpayment.ErrPaymentNotFound
	}
	saved := false
	if strings.TrimSpace(item.PrepayID) == "" {
		item.PrepayID = prepayID
		saved = true
	}
	return saved, nil
}

// MarkPaid 走 UNPAID -> PAID 的条件更新。
func (r *e2eMemoryPaymentRepository) MarkPaid(
	_ context.Context,
	outTradeNo string,
	transactionID string,
) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.payments[outTradeNo]
	if !ok {
		return false, nil
	}
	if item.PaymentStatus != domainpayment.PaymentStatusUnpaid {
		return false, nil
	}
	item.PaymentStatus = domainpayment.PaymentStatusPaid
	item.TransactionID = transactionID
	return true, nil
}

// ================= 目标 D：判定凭据所属的支付宝环境（只读探测） =================

// e2eProbeGateway 是目标 D 的一个候选网关。
type e2eProbeGateway struct {
	label string
	url   string
}

// e2eProbeResult 记录一次只读探测的结果。
type e2eProbeResult struct {
	label      string
	gatewayURL string
	// structured 为 true 表示拿到了支付宝的结构化响应（含业务码拒绝）；
	// 为 false 表示在 DNS/TLS/网络层就失败了。
	structured bool
	detail     string
}

// TestAlipayGatewayEnvironmentProbe 用只读接口 alipay.trade.query 探测 .env 凭据属于哪个环境。
//
// 对不存在的 out_trade_no 发起查询不会在支付宝侧产生任何交易记录，因此是无副作用的只读探测。
// 判定口径：拿到结构化响应（哪怕是被 isv.invalid-app-id 拒绝）即视为该网关「可达」；
// 只有 DNS/TLS/网络失败才算不可达。默认跳过，设置 PAYMENT_PROBE_GATEWAYS=1 才执行。
func TestAlipayGatewayEnvironmentProbe(t *testing.T) {
	if os.Getenv("PAYMENT_PROBE_GATEWAYS") != "1" {
		t.Skip("未设置 PAYMENT_PROBE_GATEWAYS=1，跳过支付宝网关环境只读探测")
	}
	env := e2eLoadDotEnv(t)
	appID := strings.TrimSpace(env["ALIPAY_APP_ID"])
	if appID == "" || strings.TrimSpace(env["ALIPAY_APP_PRIVATE_SECRET"]) == "" {
		t.Fatalf(".env 缺少 ALIPAY_APP_ID/ALIPAY_APP_PRIVATE_SECRET，无法探测")
	}
	candidates := []e2eProbeGateway{
		{label: "新沙箱", url: "https://openapi-sandbox.dl.alipaydev.com/gateway.do"},
		{label: "旧沙箱", url: "https://openapi.alipaydev.com/gateway.do"},
		{label: "生产", url: "https://openapi.alipay.com/gateway.do"},
	}
	probeOutTradeNo := "E2EPROBE" + time.Now().Format("20060102150405")
	results := make([]e2eProbeResult, 0, len(candidates))
	for _, candidate := range candidates {
		gateway := NewAlipayGateway(AlipayOptions{
			AppID:      appID,
			PrivateKey: env["ALIPAY_APP_PRIVATE_SECRET"],
			PublicKey:  env["ALIPAY_PUBLIC_SECRET"],
			GatewayURL: candidate.url,
			SellerIDs:  nil,
			Timeout:    15 * time.Second,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		queryResult, err := gateway.QueryTrade(ctx, probeOutTradeNo)
		cancel()

		item := e2eProbeResult{label: candidate.label, gatewayURL: candidate.url}
		switch {
		case err == nil:
			// 交易不存在是查询的正常结果，说明 app_id 被该环境接受（code=10000）。
			item.structured = true
			item.detail = "结构化响应且无错误：out_trade_no=" + queryResult.OutTradeNo +
				" trade_status=" + fmt.Sprintf("%q", queryResult.TradeStatus) +
				"（code=10000，ACQ.TRADE_NOT_EXIST 属于正常返回）"
		case e2eLooksLikeStructuredError(err):
			// 被网关按业务码拒绝：仍然证明请求到达了支付宝，是这个 app_id 不被该环境接受。
			item.structured = true
			item.detail = "结构化业务拒绝：" + err.Error()
		default:
			item.structured = false
			item.detail = "网络层失败：" + err.Error()
		}
		// 网关返回 isv.invalid-signature 时，追加一次「把 sign_type 也纳入待签串」的对照实验：
		// 若对照实验通过，说明根因是请求签名拼接口径；若仍被拒，说明应用私钥与平台公钥不匹配。
		if item.structured && strings.Contains(item.detail, "invalid-signature") {
			t.Logf("[%s] 追加对照实验（sign_type 参与待签串）：%s", item.label,
				e2eRawQueryIncludingSignType(t, candidate.url, appID, env["ALIPAY_APP_PRIVATE_SECRET"], probeOutTradeNo))
		}
		results = append(results, item)
		t.Logf("[%s] %s -> %s", item.label, item.gatewayURL, item.detail)
	}
	reachable := 0
	for _, item := range results {
		if item.structured {
			reachable++
		}
	}
	if reachable == 0 {
		t.Fatalf("三个网关都没有返回结构化响应，说明本机到支付宝的网络层（DNS/TLS/出网）整体不可用")
	}
	t.Logf("探测结论：%d/%d 个网关返回了结构化响应", reachable, len(results))
}

// e2eLooksLikeStructuredError 判断错误是否来自支付宝业务响应（而不是 DNS/TLS/网络故障）：
// 适配器里的 alipayBizError 会拼出 code=... sub_code=... msg=... sub_msg=...。
func e2eLooksLikeStructuredError(err error) bool {
	message := err.Error()
	return strings.Contains(message, "code=") && strings.Contains(message, "sub_code=")
}

// e2eSignContentIncludingSignType 与 e2eSignContent 的口径只差一点：只剔除 sign 与空值，
// 让 sign_type 参与待签串（仅用于对照实验，判定 isv.invalid-signature 的根因）。
func e2eSignContentIncludingSignType(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for key, value := range params {
		if key == "sign" || value == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+params[key])
	}
	return strings.Join(parts, "&")
}

// e2eRawQueryIncludingSignType 用「sign_type 参与签名」的口径发起一次只读 alipay.trade.query，
// 返回网关原始响应的关键字段（或网络层错误原文）。除签名口径外其余参数与适配器保持一致。
func e2eRawQueryIncludingSignType(
	t *testing.T,
	gatewayURL string,
	appID string,
	privateKeyRaw string,
	outTradeNo string,
) string {
	t.Helper()
	privateKey := e2eParsePrivateKey(t, privateKeyRaw)
	bizContent, err := json.Marshal(map[string]string{"out_trade_no": outTradeNo})
	if err != nil {
		t.Fatalf("序列化 biz_content 失败：%v", err)
	}
	params := map[string]string{
		"app_id":      appID,
		"method":      "alipay.trade.query",
		"format":      "JSON",
		"charset":     "utf-8",
		"sign_type":   "RSA2",
		"timestamp":   time.Now().In(alipayLocation).Format("2006-01-02 15:04:05"),
		"version":     "1.0",
		"biz_content": string(bizContent),
	}
	params["sign"] = e2eSignRSA2(t, privateKey, e2eSignContentIncludingSignType(params))
	form := url.Values{}
	for key, value := range params {
		form.Set(key, value)
	}
	request, err := http.NewRequest(http.MethodPost, gatewayURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "构造请求失败：" + err.Error()
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return "网络层失败：" + err.Error()
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "读取响应失败：" + err.Error()
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "响应不是 JSON：" + string(body)
	}
	payload, ok := envelope["alipay_trade_query_response"]
	if !ok {
		return "响应缺少 alipay_trade_query_response 节点：" + string(body)
	}
	var result struct {
		Code    string `json:"code"`
		Msg     string `json:"msg"`
		SubCode string `json:"sub_code"`
		SubMsg  string `json:"sub_msg"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return "解析业务节点失败：" + err.Error()
	}
	return fmt.Sprintf("code=%s sub_code=%s msg=%s sub_msg=%s",
		result.Code, result.SubCode, result.Msg, result.SubMsg)
}
