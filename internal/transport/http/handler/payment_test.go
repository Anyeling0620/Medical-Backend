// 支付域 HTTP 契约单测：POST /api/v1/payments、GET /api/v1/payments/{outTradeNo} 与
// POST /api/v1/payments/alipay/notify 的状态码、错误码与响应形状
// （spec/04-api-contract.md §6.5-§6.7、§10、§12.4）。
//
// gin 处于 TestMode，路由按 router.go 的路径挂载；令牌载荷通过
// c.Set(middleware.ClaimsKey, claims) 模拟「访问令牌已校验通过」；
// 仓储与支付宝适配器用内存桩替换，不依赖 PostgreSQL/Redis/支付宝。
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainpayment "Medical-Web-Backend/internal/domain/payment"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/usecase/authsession"
	paymentservice "Medical-Web-Backend/internal/usecase/payment"
)

const (
	// paymentHandlerTestPatientID 是测试用患者账号主键，写入注入的 claims。
	paymentHandlerTestPatientID = int64(77)
	// paymentHandlerTestRegistrationID 是测试用挂号编号。
	paymentHandlerTestRegistrationID = int64(1001)
	// paymentHandlerTestOutTradeNo 是测试用外部交易号。
	paymentHandlerTestOutTradeNo = "202609080001"
	// paymentHandlerTestTradeNo 是测试用支付宝交易号。
	paymentHandlerTestTradeNo = "2026090822001456789012"
	// paymentHandlerTestAmount 是测试用金额。
	paymentHandlerTestAmount = "80.00"
	// paymentHandlerTestQRCode 是测试用付款二维码内容。
	paymentHandlerTestQRCode = "https://qr.alipay.com/bax0123456789"
)

// paymentHandlerTestNow 是注入用例的固定时钟：业务时区（UTC+8）2026-09-10 09:00。
var paymentHandlerTestNow = time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)

// paymentHandlerTestRepo 是内存版 port.PaymentRepository 桩：
// 记录查询入参（用于断言归属开关）与迁移调用，其余方法返回预置订单。
type paymentHandlerTestRepo struct {
	item           *domainpayment.Payment
	findErr        error
	markPaidResult bool

	regQueries []paymentHandlerTestRegQuery
	outQueries []paymentHandlerTestOutQuery
	markCalls  []paymentHandlerTestMarkCall
}

// paymentHandlerTestRegQuery 记录按挂号编号读取的入参。
type paymentHandlerTestRegQuery struct {
	registrationID int64
	ownerPatientID int64
}

// paymentHandlerTestOutQuery 记录按外部交易号读取的入参。
type paymentHandlerTestOutQuery struct {
	outTradeNo     string
	ownerPatientID int64
}

// paymentHandlerTestMarkCall 记录状态迁移入参。
type paymentHandlerTestMarkCall struct {
	outTradeNo    string
	transactionID string
}

// newPaymentHandlerTestRepo 构造默认「条件更新成功」的内存桩。
func newPaymentHandlerTestRepo() *paymentHandlerTestRepo {
	return &paymentHandlerTestRepo{markPaidResult: true}
}

func (r *paymentHandlerTestRepo) FindPaymentByRegistrationID(
	_ context.Context,
	registrationID int64,
	ownerPatientID int64,
) (*domainpayment.Payment, error) {
	r.regQueries = append(r.regQueries, paymentHandlerTestRegQuery{registrationID, ownerPatientID})
	if r.findErr != nil {
		return nil, r.findErr
	}
	return r.item, nil
}

func (r *paymentHandlerTestRepo) FindPaymentByOutTradeNo(
	_ context.Context,
	outTradeNo string,
	ownerPatientID int64,
) (*domainpayment.Payment, error) {
	r.outQueries = append(r.outQueries, paymentHandlerTestOutQuery{outTradeNo, ownerPatientID})
	if r.findErr != nil {
		return nil, r.findErr
	}
	return r.item, nil
}

func (r *paymentHandlerTestRepo) EnsurePaymentWindow(_ context.Context, _ string) (*domainpayment.Payment, error) {
	return r.item, nil
}

// SavePrepayID 端口签名要求返回「本次是否真正写入」；handler 用例不覆盖并发分支，固定返回 true。
func (r *paymentHandlerTestRepo) SavePrepayID(_ context.Context, _ string, _ string) (bool, error) {
	return true, nil
}

func (r *paymentHandlerTestRepo) MarkPaid(_ context.Context, outTradeNo string, transactionID string) (bool, error) {
	r.markCalls = append(r.markCalls, paymentHandlerTestMarkCall{outTradeNo, transactionID})
	return r.markPaidResult, nil
}

// paymentHandlerTestGateway 是内存版 port.AlipayGateway 桩；通知入口只用到 VerifyNotify。
type paymentHandlerTestGateway struct {
	notifyPayload *domainpayment.NotifyPayload
	notifyErr     error
	notifyCalls   []url.Values
}

func (g *paymentHandlerTestGateway) Precreate(_ context.Context, _ domainpayment.PrecreateRequest) (*domainpayment.PrecreateResult, error) {
	return &domainpayment.PrecreateResult{QRCode: paymentHandlerTestQRCode}, nil
}

func (g *paymentHandlerTestGateway) QueryTrade(_ context.Context, _ string) (*domainpayment.TradeQueryResult, error) {
	return &domainpayment.TradeQueryResult{}, nil
}

func (g *paymentHandlerTestGateway) VerifyNotify(_ context.Context, form url.Values) (*domainpayment.NotifyPayload, error) {
	g.notifyCalls = append(g.notifyCalls, form)
	if g.notifyErr != nil {
		return nil, g.notifyErr
	}
	return g.notifyPayload, nil
}

// 编译期确认两个桩完整实现端口契约。
var (
	_ port.PaymentRepository = (*paymentHandlerTestRepo)(nil)
	_ port.AlipayGateway     = (*paymentHandlerTestGateway)(nil)
)

// newPaymentHandlerTestService 构造注入固定时钟的支付用例。
func newPaymentHandlerTestService(repo port.PaymentRepository, gateway port.AlipayGateway) *paymentservice.Service {
	return paymentservice.NewService(repo, gateway, paymentservice.Config{
		PaymentSubject: "医院挂号费",
		NotifyURL:      "https://example.test/api/v1/payments/alipay/notify",
		Now:            func() time.Time { return paymentHandlerTestNow },
	})
}

// newPaymentHandlerTestRouter 按 router.go 的路径挂载三个支付接口。
// claims 为 nil 时不注入令牌载荷，用于覆盖未认证分支；
// 支付宝异步通知入口始终不读用户令牌（契约 §1.2、§6.7）。
func newPaymentHandlerTestRouter(service *paymentservice.Service, claims *authsession.AccessClaims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	paymentHandler := NewPaymentHandler(service)
	injectClaims := func(c *gin.Context) {
		if claims != nil {
			c.Set(middleware.ClaimsKey, claims)
		}
		c.Next()
	}
	engine.POST("/api/v1/payments", injectClaims, paymentHandler.Read)
	engine.GET("/api/v1/payments/:outTradeNo", injectClaims, paymentHandler.Detail)
	engine.POST("/api/v1/payments/alipay/notify", paymentHandler.Notify)
	return engine
}

// paymentHandlerTestPatientClaims 返回 realm=patient 的令牌载荷。
func paymentHandlerTestPatientClaims() *authsession.AccessClaims {
	return &authsession.AccessClaims{UserID: paymentHandlerTestPatientID, Realm: domainauth.RealmPatient}
}

// paymentHandlerTestMisClaims 返回 realm=mis 的令牌载荷。
func paymentHandlerTestMisClaims() *authsession.AccessClaims {
	return &authsession.AccessClaims{UserID: 9, Username: "mis-operator", Realm: domainauth.RealmMis}
}

// paymentHandlerTestUnpaid 构造窗口内的 UNPAID 订单，prepay_id 已写入。
func paymentHandlerTestUnpaid() *domainpayment.Payment {
	return &domainpayment.Payment{
		RegistrationID: paymentHandlerTestRegistrationID,
		OutTradeNo:     paymentHandlerTestOutTradeNo,
		Amount:         paymentHandlerTestAmount,
		PaymentStatus:  domainpayment.PaymentStatusUnpaid,
		PrepayID:       paymentHandlerTestQRCode,
		PrecreateAt:    paymentHandlerTestNow,
		PayDeadline:    paymentHandlerTestNow.Add(30 * time.Minute),
		ExpireAt:       paymentHandlerTestNow.Add(35 * time.Minute),
	}
}

// doPaymentRequest 在测试路由上执行一次请求；contentType 为空表示不设置请求头。
func doPaymentRequest(engine *gin.Engine, method, target, contentType, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

// decodePaymentObject 把成功响应体解析为字段字典，保留「键是否存在」的信息。
func decodePaymentObject(t *testing.T, recorder *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &object); err != nil {
		t.Fatalf("响应体不是 JSON 对象：%v（body=%s）", err, recorder.Body.String())
	}
	return object
}

// requirePaymentStringField 断言字段存在且为字符串。
func requirePaymentStringField(t *testing.T, object map[string]json.RawMessage, field string) string {
	t.Helper()
	raw, exists := object[field]
	if !exists {
		t.Fatalf("响应体缺少字段 %s", field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("字段 %s 不是字符串：%s", field, raw)
	}
	return value
}

// requirePaymentBoolField 断言字段存在且为布尔值。
func requirePaymentBoolField(t *testing.T, object map[string]json.RawMessage, field string) bool {
	t.Helper()
	raw, exists := object[field]
	if !exists {
		t.Fatalf("响应体缺少字段 %s", field)
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("字段 %s 不是布尔值：%s", field, raw)
	}
	return value
}

// requirePaymentErrorEnvelope 断言统一错误体 {code,message}、HTTP 状态码与 JSON Content-Type，
// 并确认业务异常没有被伪装成纯文本 success（契约 §1.3、§6.7）。
func requirePaymentErrorEnvelope(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("HTTP 状态码 = %d，期望 %d（body=%s）", recorder.Code, wantStatus, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("Content-Type = %q，期望 application/json", contentType)
	}
	if body := strings.TrimSpace(recorder.Body.String()); body == "success" {
		t.Fatal("业务异常不得返回纯文本 success")
	}
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("错误响应不是 JSON envelope：%v（body=%s）", err, recorder.Body.String())
	}
	if envelope.Code != wantCode {
		t.Fatalf("错误码 = %q，期望 %q", envelope.Code, wantCode)
	}
	if strings.TrimSpace(envelope.Message) == "" {
		t.Fatal("错误响应的 message 不应为空")
	}
}

// ---- POST /api/v1/payments ----

// requireChineseMessage 断言对客文案是中文，且不含 Go 标准库的英文内部原文
// （契约 §1.3：message 面向用户，不得透出 json: cannot unmarshal 之类的实现细节）。
func requireChineseMessage(t *testing.T, message string) {
	t.Helper()
	hasChinese := false
	for _, r := range message {
		if r >= '\u4e00' && r <= '\u9fff' {
			hasChinese = true
			break
		}
	}
	if !hasChinese {
		t.Fatalf("错误 message = %q，期望中文文案", message)
	}
	for _, fragment := range []string{"json:", "cannot unmarshal", "unexpected EOF", "invalid character"} {
		if strings.Contains(message, fragment) {
			t.Fatalf("错误 message = %q，不得包含英文内部原文 %q", message, fragment)
		}
	}
}

// TestPaymentReadRejectsMalformedBody 覆盖 POST /api/v1/payments 的绑定失败分支（审查阻塞项 1）：
// JSON 字段类型错误与截断的请求体 → 400 REQUEST_INVALID_JSON；空体、未知字段与语义错误仍为
// 422 REQUEST_VALIDATION_FAILED。两个分支都必须返回中文文案，且不得触达仓储。
func TestPaymentReadRejectsMalformedBody(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "registrationId 类型错误返回 400",
			body:       `{"registrationId":"1001"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "REQUEST_INVALID_JSON",
		},
		{
			name:       "截断的 JSON 返回 400",
			body:       `{"registrationId":1`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "REQUEST_INVALID_JSON",
		},
		{
			name:       "空 body 仍返回 422",
			body:       ``,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "REQUEST_VALIDATION_FAILED",
		},
		{
			name:       "包含未知字段仍返回 422",
			body:       `{"registrationId":1001,"method":"ALIPAY","unknownField":"x"}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "REQUEST_VALIDATION_FAILED",
		},
		{
			name:       "method 非 ALIPAY 仍返回 422",
			body:       `{"registrationId":1001,"method":"WECHAT"}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "REQUEST_VALIDATION_FAILED",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newPaymentHandlerTestRepo()
			repo.item = paymentHandlerTestUnpaid()
			engine := newPaymentHandlerTestRouter(
				newPaymentHandlerTestService(repo, &paymentHandlerTestGateway{}),
				paymentHandlerTestPatientClaims(),
			)

			recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments", "application/json", tc.body)
			requirePaymentErrorEnvelope(t, recorder, tc.wantStatus, tc.wantCode)

			var envelope struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("错误响应不是 JSON envelope：%v（body=%s）", err, recorder.Body.String())
			}
			requireChineseMessage(t, envelope.Message)

			if len(repo.regQueries) != 0 {
				t.Fatalf("绑定失败不应触达仓储，实际调用 %d 次", len(repo.regQueries))
			}
		})
	}
}

// TestPaymentReadRequiresAccessToken 未携带令牌时必须返回 401 AUTH_INVALID_TOKEN，
// 且不得触达仓储（契约 §1.2、§10）。
func TestPaymentReadRequiresAccessToken(t *testing.T) {
	repo := newPaymentHandlerTestRepo()
	engine := newPaymentHandlerTestRouter(newPaymentHandlerTestService(repo, &paymentHandlerTestGateway{}), nil)

	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments", "application/json",
		`{"registrationId":1001,"method":"ALIPAY"}`)
	requirePaymentErrorEnvelope(t, recorder, http.StatusUnauthorized, "AUTH_INVALID_TOKEN")
	if len(repo.regQueries) != 0 {
		t.Fatalf("未认证请求不应触达仓储，实际调用 %d 次", len(repo.regQueries))
	}
}

// TestPaymentReadPayableShape 覆盖可支付响应形状：
// 字段集合与 §12.4 一致，payableUntil/validUntil 为 pay_deadline/expire_at 的 RFC3339 UTC；
// 患者端必须把当前患者作为归属开关传给仓储。
func TestPaymentReadPayableShape(t *testing.T) {
	repo := newPaymentHandlerTestRepo()
	repo.item = paymentHandlerTestUnpaid()
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, &paymentHandlerTestGateway{}),
		paymentHandlerTestPatientClaims(),
	)

	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments", "application/json",
		`{"registrationId":1001,"method":"ALIPAY"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	object := decodePaymentObject(t, recorder)
	if got := requirePaymentStringField(t, object, "outTradeNo"); got != paymentHandlerTestOutTradeNo {
		t.Fatalf("outTradeNo = %q，期望 %q", got, paymentHandlerTestOutTradeNo)
	}
	if got := requirePaymentStringField(t, object, "paymentStatus"); got != domainpayment.PaymentStatusUnpaid {
		t.Fatalf("paymentStatus = %q，期望 %q", got, domainpayment.PaymentStatusUnpaid)
	}
	if !requirePaymentBoolField(t, object, "payable") {
		t.Fatal("窗口内 payable 应为 true")
	}
	if got := requirePaymentStringField(t, object, "qrCode"); got != paymentHandlerTestQRCode {
		t.Fatalf("qrCode = %q，期望 %q", got, paymentHandlerTestQRCode)
	}
	if got := requirePaymentStringField(t, object, "payableUntil"); got != "2026-09-10T01:30:00Z" {
		t.Fatalf("payableUntil = %q，期望 2026-09-10T01:30:00Z", got)
	}
	if got := requirePaymentStringField(t, object, "validUntil"); got != "2026-09-10T01:35:00Z" {
		t.Fatalf("validUntil = %q，期望 2026-09-10T01:35:00Z", got)
	}
	if raw, exists := object["registrationId"]; !exists || string(raw) != "1001" {
		t.Fatalf("registrationId = %s，期望 1001", raw)
	}
	if len(repo.regQueries) != 1 || repo.regQueries[0].ownerPatientID != paymentHandlerTestPatientID {
		t.Fatalf("仓储入参 = %+v，期望 ownerPatientID=%d", repo.regQueries, paymentHandlerTestPatientID)
	}
}

// TestPaymentReadHidesQRCodeWhenNotPayable 覆盖 30~35 分钟窗口：
// 仍返回 200 与 paymentStatus=UNPAID，但 payable=false 且响应体中不得出现 qrCode 键（契约 §6.5）。
func TestPaymentReadHidesQRCodeWhenNotPayable(t *testing.T) {
	item := paymentHandlerTestUnpaid()
	item.PayDeadline = paymentHandlerTestNow.Add(-time.Minute)
	item.ExpireAt = paymentHandlerTestNow.Add(4 * time.Minute)
	repo := newPaymentHandlerTestRepo()
	repo.item = item
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, &paymentHandlerTestGateway{}),
		paymentHandlerTestPatientClaims(),
	)

	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments", "application/json",
		`{"registrationId":1001,"method":"ALIPAY"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	object := decodePaymentObject(t, recorder)
	if got := requirePaymentStringField(t, object, "paymentStatus"); got != domainpayment.PaymentStatusUnpaid {
		t.Fatalf("paymentStatus = %q，期望 %q", got, domainpayment.PaymentStatusUnpaid)
	}
	if requirePaymentBoolField(t, object, "payable") {
		t.Fatal("超过 pay_deadline 后 payable 应为 false")
	}
	if raw, exists := object["qrCode"]; exists {
		t.Fatalf("payable=false 时响应体不得出现 qrCode 键，实际 %s", raw)
	}
}

// TestPaymentReadMisActorHasNoOwnershipFilter 管理端令牌已由中间件完成权限校验，
// 用例层不再限定归属（ownerPatientID 传 0，契约 §1.2）。
func TestPaymentReadMisActorHasNoOwnershipFilter(t *testing.T) {
	repo := newPaymentHandlerTestRepo()
	repo.item = paymentHandlerTestUnpaid()
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, &paymentHandlerTestGateway{}),
		paymentHandlerTestMisClaims(),
	)

	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments", "application/json",
		`{"registrationId":1001,"method":"ALIPAY"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	if len(repo.regQueries) != 1 || repo.regQueries[0].ownerPatientID != 0 {
		t.Fatalf("仓储入参 = %+v，期望 ownerPatientID=0", repo.regQueries)
	}
}

// paymentHandlerTestNotifyPayload 构造验签通过后的通知载荷，金额与订单一致。
func paymentHandlerTestNotifyPayload() *domainpayment.NotifyPayload {
	return &domainpayment.NotifyPayload{
		AppID:       "2021000000000000",
		SellerID:    "2088101106499364",
		OutTradeNo:  paymentHandlerTestOutTradeNo,
		TradeNo:     paymentHandlerTestTradeNo,
		TradeStatus: domainpayment.TradeStatusSuccess,
		TotalAmount: paymentHandlerTestAmount,
		GmtPayment:  "2026-09-10 09:00:00",
		NotifyTime:  "2026-09-10 09:00:01",
	}
}

// ---- POST /api/v1/payments/alipay/notify ----

// TestPaymentNotifySuccessIsPlainText 成功通知必须返回纯文本 success
// （无引号、无 JSON 包裹），Content-Type 为 text/plain；同时断言表单字段确实
// 被解析并原样交给验签（契约 §6.7、§12.4）。
func TestPaymentNotifySuccessIsPlainText(t *testing.T) {
	repo := newPaymentHandlerTestRepo()
	repo.item = paymentHandlerTestUnpaid()
	gateway := &paymentHandlerTestGateway{notifyPayload: paymentHandlerTestNotifyPayload()}
	engine := newPaymentHandlerTestRouter(newPaymentHandlerTestService(repo, gateway), nil)

	form := url.Values{
		"out_trade_no": {paymentHandlerTestOutTradeNo},
		"trade_no":     {paymentHandlerTestTradeNo},
		"trade_status": {domainpayment.TradeStatusSuccess},
		"total_amount": {paymentHandlerTestAmount},
		"sign_type":    {"RSA2"},
		"sign":         {"truncated-sign"},
	}
	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/alipay/notify",
		"application/x-www-form-urlencoded", form.Encode())

	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("Content-Type = %q，期望 text/plain", contentType)
	}
	if body := recorder.Body.String(); body != "success" {
		t.Fatalf("响应体 = %q，期望恰好 success（无引号、无 JSON 包裹）", body)
	}
	if len(gateway.notifyCalls) != 1 {
		t.Fatalf("验签调用次数 = %d，期望 1", len(gateway.notifyCalls))
	}
	for field, want := range map[string]string{
		"out_trade_no": paymentHandlerTestOutTradeNo,
		"trade_no":     paymentHandlerTestTradeNo,
		"trade_status": domainpayment.TradeStatusSuccess,
		"total_amount": paymentHandlerTestAmount,
	} {
		if got := gateway.notifyCalls[0].Get(field); got != want {
			t.Fatalf("验签收到的表单字段 %s = %q，期望 %q", field, got, want)
		}
	}
	if len(repo.markCalls) != 1 || repo.markCalls[0] != (paymentHandlerTestMarkCall{paymentHandlerTestOutTradeNo, paymentHandlerTestTradeNo}) {
		t.Fatalf("迁移入参 = %+v，期望 outTradeNo=%s tradeNo=%s", repo.markCalls, paymentHandlerTestOutTradeNo, paymentHandlerTestTradeNo)
	}
}

// TestPaymentNotifyBusinessErrors 覆盖通知路径的业务异常：
// 验签/身份/金额异常映射 400，订单不存在映射 404，且都返回统一 envelope
// 而不是 success（契约 §6.7、§10）。
func TestPaymentNotifyBusinessErrors(t *testing.T) {
	cases := []struct {
		name       string
		wantStatus int
		wantCode   string
		setup      func(*paymentHandlerTestRepo, *paymentHandlerTestGateway)
	}{
		{
			name:       "验签失败",
			wantStatus: http.StatusBadRequest,
			wantCode:   "PAYMENT_NOTIFY_SIGNATURE_INVALID",
			setup: func(_ *paymentHandlerTestRepo, gateway *paymentHandlerTestGateway) {
				gateway.notifyErr = port.ErrNotifySignatureInvalid
			},
		},
		{
			name:       "身份不一致",
			wantStatus: http.StatusBadRequest,
			wantCode:   "PAYMENT_NOTIFY_IDENTITY_MISMATCH",
			setup: func(_ *paymentHandlerTestRepo, gateway *paymentHandlerTestGateway) {
				gateway.notifyErr = port.ErrNotifyIdentityMismatch
			},
		},
		{
			name:       "金额不一致",
			wantStatus: http.StatusBadRequest,
			wantCode:   "PAYMENT_AMOUNT_MISMATCH",
			setup: func(_ *paymentHandlerTestRepo, gateway *paymentHandlerTestGateway) {
				payload := paymentHandlerTestNotifyPayload()
				payload.TotalAmount = "0.01"
				gateway.notifyPayload = payload
			},
		},
		{
			name:       "订单不存在",
			wantStatus: http.StatusNotFound,
			wantCode:   "PAYMENT_NOT_FOUND",
			setup: func(repo *paymentHandlerTestRepo, gateway *paymentHandlerTestGateway) {
				repo.findErr = domainpayment.ErrPaymentNotFound
				gateway.notifyPayload = paymentHandlerTestNotifyPayload()
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newPaymentHandlerTestRepo()
			repo.item = paymentHandlerTestUnpaid()
			gateway := &paymentHandlerTestGateway{}
			tc.setup(repo, gateway)
			engine := newPaymentHandlerTestRouter(newPaymentHandlerTestService(repo, gateway), nil)

			recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/alipay/notify",
				"application/x-www-form-urlencoded",
				"out_trade_no="+paymentHandlerTestOutTradeNo+"&trade_status=TRADE_SUCCESS&total_amount="+paymentHandlerTestAmount)
			requirePaymentErrorEnvelope(t, recorder, tc.wantStatus, tc.wantCode)
			if len(repo.markCalls) != 0 {
				t.Fatalf("异常通知不应迁移状态，实际迁移 %d 次", len(repo.markCalls))
			}
		})
	}
}

// ---- GET /api/v1/payments/{outTradeNo} ----

// TestPaymentDetailHidesForeignOrderAsNotFound 覆盖患者端越权：
// 仓储按归属条件过滤后统一返回 404 PAYMENT_NOT_FOUND，且必须把当前患者作为
// 归属开关传给仓储（契约 §1.2、§6.6）。
func TestPaymentDetailHidesForeignOrderAsNotFound(t *testing.T) {
	repo := newPaymentHandlerTestRepo()
	repo.findErr = domainpayment.ErrPaymentNotFound
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, &paymentHandlerTestGateway{}),
		paymentHandlerTestPatientClaims(),
	)

	recorder := doPaymentRequest(engine, http.MethodGet, "/api/v1/payments/"+paymentHandlerTestOutTradeNo, "", "")
	requirePaymentErrorEnvelope(t, recorder, http.StatusNotFound, "PAYMENT_NOT_FOUND")
	if len(repo.outQueries) != 1 {
		t.Fatalf("仓储调用次数 = %d，期望 1", len(repo.outQueries))
	}
	if repo.outQueries[0] != (paymentHandlerTestOutQuery{paymentHandlerTestOutTradeNo, paymentHandlerTestPatientID}) {
		t.Fatalf("仓储入参 = %+v，期望 outTradeNo=%s ownerPatientID=%d",
			repo.outQueries[0], paymentHandlerTestOutTradeNo, paymentHandlerTestPatientID)
	}
	if len(repo.markCalls) != 0 {
		t.Fatalf("订单不存在不应迁移状态，实际迁移 %d 次", len(repo.markCalls))
	}
}

// TestPaymentDetailShape 覆盖查询支付状态的 200 响应形状：
// 含 amount 与 paymentStatus，已支付时返回 transactionId；paidAt 当前恒为 null，
// 且详情响应不包含取支付参数专属的 qrCode 字段（契约 §6.6、§12.4）。
func TestPaymentDetailShape(t *testing.T) {
	item := paymentHandlerTestUnpaid()
	item.PaymentStatus = domainpayment.PaymentStatusPaid
	item.TransactionID = paymentHandlerTestTradeNo
	repo := newPaymentHandlerTestRepo()
	repo.item = item
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, &paymentHandlerTestGateway{}),
		paymentHandlerTestMisClaims(),
	)

	recorder := doPaymentRequest(engine, http.MethodGet, "/api/v1/payments/"+paymentHandlerTestOutTradeNo, "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	object := decodePaymentObject(t, recorder)
	if got := requirePaymentStringField(t, object, "outTradeNo"); got != paymentHandlerTestOutTradeNo {
		t.Fatalf("outTradeNo = %q，期望 %q", got, paymentHandlerTestOutTradeNo)
	}
	if got := requirePaymentStringField(t, object, "amount"); got != paymentHandlerTestAmount {
		t.Fatalf("amount = %q，期望 %q", got, paymentHandlerTestAmount)
	}
	if got := requirePaymentStringField(t, object, "paymentStatus"); got != domainpayment.PaymentStatusPaid {
		t.Fatalf("paymentStatus = %q，期望 %q", got, domainpayment.PaymentStatusPaid)
	}
	if got := requirePaymentStringField(t, object, "transactionId"); got != paymentHandlerTestTradeNo {
		t.Fatalf("transactionId = %q，期望 %q", got, paymentHandlerTestTradeNo)
	}
	if raw, exists := object["paidAt"]; !exists || string(raw) != "null" {
		t.Fatalf("paidAt = %s（exists=%v），期望 null", raw, exists)
	}
	if raw, exists := object["qrCode"]; exists {
		t.Fatalf("详情响应不应包含 qrCode 字段，实际 %s", raw)
	}
	if len(repo.outQueries) != 1 || repo.outQueries[0].ownerPatientID != 0 {
		t.Fatalf("仓储入参 = %+v，期望 ownerPatientID=0", repo.outQueries)
	}
}
