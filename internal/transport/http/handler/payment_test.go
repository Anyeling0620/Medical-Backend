// 支付域 HTTP 契约单测：POST /api/v1/payments、POST /api/v1/payments/orders、
// GET /api/v1/payments/{outTradeNo} 与 POST /api/v1/payments/alipay/notify 的
// 状态码、错误码与响应形状（spec/04-api-contract.md §6.5-§6.9、§10、§12.4）。
//
// gin 处于 TestMode，路由按 router.go 的路径挂载；令牌载荷通过
// c.Set(middleware.ClaimsKey, claims) 模拟「访问令牌已校验通过」；
// 仓储与支付宝适配器用内存桩替换，不依赖 PostgreSQL/Redis/支付宝。
package handler

import (
	"context"
	"encoding/json"
	"errors"
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
	// saveResult 是 SavePrepayID 的写入结果；false 模拟并发请求已先行写入二维码。
	saveResult bool
	// outTradeNoItem 是按交易号读取的专属返回（nil 表示沿用 item），
	// 用于区分「按挂号编号读取的初始值」与「预下单并发竞争时的回读值」。
	outTradeNoItem *domainpayment.Payment
	// outTradeNoErr 只让「按交易号读取」失败（nil 表示不注入故障），
	// 用于覆盖创建订单在预下单后重读最新状态时的 404/502 分支，
	// 此时按挂号编号的首次读取仍必须成功。
	outTradeNoErr error
	// ensureItem 是 EnsurePaymentWindow 的专属返回（nil 表示沿用 item），
	// 用于模拟补齐窗口期间订单被异步通知改成终态。
	ensureItem *domainpayment.Payment

	regQueries  []paymentHandlerTestRegQuery
	outQueries  []paymentHandlerTestOutQuery
	markCalls   []paymentHandlerTestMarkCall
	ensureCalls []string
	saveCalls   []paymentHandlerTestSaveCall
}

// paymentHandlerTestSaveCall 记录二维码回写入参。
type paymentHandlerTestSaveCall struct {
	outTradeNo string
	prepayID   string
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

// newPaymentHandlerTestRepo 构造默认「二维码可写入、条件更新成功」的内存桩。
func newPaymentHandlerTestRepo() *paymentHandlerTestRepo {
	return &paymentHandlerTestRepo{markPaidResult: true, saveResult: true}
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
	if r.outTradeNoErr != nil {
		return nil, r.outTradeNoErr
	}
	if r.findErr != nil {
		return nil, r.findErr
	}
	// 注入专属回读值时优先返回，供并发重放用例断言「二维码以库中既有值为准」。
	if r.outTradeNoItem != nil {
		return r.outTradeNoItem, nil
	}
	return r.item, nil
}

// EnsurePaymentWindow 记录补齐调用；注入 ensureItem 时优先返回，
// 用于模拟补齐期间订单被异步通知改成终态（CreateOrder 的 TOCTOU 复核）。
func (r *paymentHandlerTestRepo) EnsurePaymentWindow(_ context.Context, outTradeNo string) (*domainpayment.Payment, error) {
	r.ensureCalls = append(r.ensureCalls, outTradeNo)
	if r.ensureItem != nil {
		return r.ensureItem, nil
	}
	return r.item, nil
}

// SavePrepayID 记录回写入参并返回注入的写入结果。
// saveResult=false 模拟并发请求已先行写入二维码，用于覆盖 200 幂等重放分支。
func (r *paymentHandlerTestRepo) SavePrepayID(_ context.Context, outTradeNo string, prepayID string) (bool, error) {
	r.saveCalls = append(r.saveCalls, paymentHandlerTestSaveCall{outTradeNo, prepayID})
	return r.saveResult, nil
}

func (r *paymentHandlerTestRepo) MarkPaid(_ context.Context, outTradeNo string, transactionID string) (bool, error) {
	r.markCalls = append(r.markCalls, paymentHandlerTestMarkCall{outTradeNo, transactionID})
	return r.markPaidResult, nil
}

// paymentHandlerTestGateway 是内存版 port.AlipayGateway 桩：通知入口用 VerifyNotify，
// 创建支付订单入口用 Precreate（记录调用次数与入参）。
type paymentHandlerTestGateway struct {
	notifyPayload *domainpayment.NotifyPayload
	notifyErr     error
	notifyCalls   []url.Values

	// 预下单桩：precreateResult 为 nil 时回落到默认二维码，precreateErr 用于注入失败。
	precreateResult *domainpayment.PrecreateResult
	precreateErr    error
	precreateCalls  []domainpayment.PrecreateRequest
}

func (g *paymentHandlerTestGateway) Precreate(_ context.Context, req domainpayment.PrecreateRequest) (*domainpayment.PrecreateResult, error) {
	g.precreateCalls = append(g.precreateCalls, req)
	if g.precreateErr != nil {
		return nil, g.precreateErr
	}
	if g.precreateResult != nil {
		return g.precreateResult, nil
	}
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

// newPaymentHandlerTestRouter 按 router.go 的路径挂载四个支付接口。
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
	engine.POST("/api/v1/payments/orders", injectClaims, paymentHandler.Create)
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

// TestPaymentReadEmptyQRCodeIsNotPayable 覆盖「窗口仍在（UNPAID 且未过 pay_deadline）但
// prepay_id 为空」：读路径是纯读取（不补窗口、不预下单），响应必须 payable=false 且不含
// qrCode，否则会出现 payable=true 却没有任何二维码可渲染的矛盾响应（契约 §6.5）。
func TestPaymentReadEmptyQRCodeIsNotPayable(t *testing.T) {
	item := paymentHandlerTestUnpaid()
	item.PrepayID = ""

	repo := newPaymentHandlerTestRepo()
	repo.item = item
	gateway := &paymentHandlerTestGateway{}
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, gateway),
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
		t.Fatal("prepay_id 为空时 payable 必须为 false（不能声明可支付却没有二维码）")
	}
	if raw, exists := object["qrCode"]; exists {
		t.Fatalf("prepay_id 为空时响应体不得出现 qrCode 键，实际 %s", raw)
	}
	// 支付窗口仍按库中原样回传（窗口存在），便于前端展示倒计时。
	if got := requirePaymentStringField(t, object, "payableUntil"); got != "2026-09-10T01:30:00Z" {
		t.Fatalf("payableUntil = %q，期望 2026-09-10T01:30:00Z", got)
	}
	if got := requirePaymentStringField(t, object, "validUntil"); got != "2026-09-10T01:35:00Z" {
		t.Fatalf("validUntil = %q，期望 2026-09-10T01:35:00Z", got)
	}
	// 读路径不得触达 provider，也不得补写支付窗口（§6.5 纯读取）。
	if len(gateway.precreateCalls) != 0 {
		t.Fatalf("读路径不得预下单，实际调用 %d 次", len(gateway.precreateCalls))
	}
	if len(repo.ensureCalls) != 0 {
		t.Fatalf("读路径不得补写支付窗口，实际调用 %d 次", len(repo.ensureCalls))
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

// ---- POST /api/v1/payments/orders ----

// TestPaymentCreateRejectsInvalidRequests 覆盖创建支付订单的请求绑定分支（契约 §6.9、§12.4）：
// 空体、registrationId 缺失/为 0/为负与未知字段 → 422 REQUEST_VALIDATION_FAILED；
// 类型错误与截断的 JSON → 400 REQUEST_INVALID_JSON；
// 两种分支都必须返回中文文案，且不得触达仓储与支付宝。
func TestPaymentCreateRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{name: "空 body", body: ``, wantStatus: http.StatusUnprocessableEntity, wantCode: "REQUEST_VALIDATION_FAILED"},
		{name: "registrationId 缺失", body: `{}`, wantStatus: http.StatusUnprocessableEntity, wantCode: "REQUEST_VALIDATION_FAILED"},
		{name: "registrationId 为 0", body: `{"registrationId":0}`, wantStatus: http.StatusUnprocessableEntity, wantCode: "REQUEST_VALIDATION_FAILED"},
		{name: "registrationId 为负数", body: `{"registrationId":-1}`, wantStatus: http.StatusUnprocessableEntity, wantCode: "REQUEST_VALIDATION_FAILED"},
		{name: "包含未知字段", body: `{"registrationId":1001,"unknownField":"x"}`, wantStatus: http.StatusUnprocessableEntity, wantCode: "REQUEST_VALIDATION_FAILED"},
		{name: "registrationId 类型错误", body: `{"registrationId":"1001"}`, wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID_JSON"},
		{name: "截断的 JSON", body: `{"registrationId":`, wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID_JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newPaymentHandlerTestRepo()
			repo.item = paymentHandlerTestUnpaid()
			gateway := &paymentHandlerTestGateway{}
			engine := newPaymentHandlerTestRouter(
				newPaymentHandlerTestService(repo, gateway),
				paymentHandlerTestMisClaims(),
			)

			recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", tc.body)
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
			if len(gateway.precreateCalls) != 0 {
				t.Fatalf("绑定失败不应调用支付宝预下单，实际调用 %d 次", len(gateway.precreateCalls))
			}
		})
	}
}

// TestPaymentCreateRequiresAccessToken 未携带令牌时必须返回 401 AUTH_INVALID_TOKEN，
// 且不得触达仓储（契约 §1.2、§6.9）。
func TestPaymentCreateRequiresAccessToken(t *testing.T) {
	repo := newPaymentHandlerTestRepo()
	engine := newPaymentHandlerTestRouter(newPaymentHandlerTestService(repo, &paymentHandlerTestGateway{}), nil)

	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", `{"registrationId":1001}`)
	requirePaymentErrorEnvelope(t, recorder, http.StatusUnauthorized, "AUTH_INVALID_TOKEN")
	if len(repo.regQueries) != 0 {
		t.Fatalf("未认证请求不应触达仓储，实际调用 %d 次", len(repo.regQueries))
	}
}

// TestPaymentCreateFirstTimeReturns201 覆盖首次创建（契约 §1.3、§6.9）：
// prepay_id 为空时调用一次预下单，返回 201 + Location 指向查询地址，
// 响应含 qrCode 且 payable=true；本接口是管理端专用，必须按不限定归属查询。
func TestPaymentCreateFirstTimeReturns201(t *testing.T) {
	item := paymentHandlerTestUnpaid()
	item.PrepayID = ""
	repo := newPaymentHandlerTestRepo()
	repo.item = item
	gateway := &paymentHandlerTestGateway{
		precreateResult: &domainpayment.PrecreateResult{QRCode: paymentHandlerTestQRCode},
	}
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, gateway),
		paymentHandlerTestMisClaims(),
	)

	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", `{"registrationId":1001}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("HTTP 状态码 = %d，期望 201（body=%s）", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); location != "/api/v1/payments/"+paymentHandlerTestOutTradeNo {
		t.Fatalf("Location = %q，期望 %q", location, "/api/v1/payments/"+paymentHandlerTestOutTradeNo)
	}
	object := decodePaymentObject(t, recorder)
	if got := requirePaymentStringField(t, object, "outTradeNo"); got != paymentHandlerTestOutTradeNo {
		t.Fatalf("outTradeNo = %q，期望 %q", got, paymentHandlerTestOutTradeNo)
	}
	if got := requirePaymentStringField(t, object, "qrCode"); got != paymentHandlerTestQRCode {
		t.Fatalf("qrCode = %q，期望 %q", got, paymentHandlerTestQRCode)
	}
	if !requirePaymentBoolField(t, object, "payable") {
		t.Fatal("窗口内 payable 应为 true")
	}
	if len(gateway.precreateCalls) != 1 {
		t.Fatalf("预下单调用次数 = %d，期望 1", len(gateway.precreateCalls))
	}
	if len(repo.regQueries) != 1 || repo.regQueries[0].ownerPatientID != 0 {
		t.Fatalf("仓储入参 = %+v，期望管理端不限定归属（ownerPatientID=0）", repo.regQueries)
	}
}

// TestPaymentCreateIdempotentReturns200 覆盖幂等（契约 §6.9）：
// 已有 prepay_id 且在支付窗口内时返回 200 与同一个二维码，不重复预下单，也不带 Location。
func TestPaymentCreateIdempotentReturns200(t *testing.T) {
	repo := newPaymentHandlerTestRepo()
	repo.item = paymentHandlerTestUnpaid()
	gateway := &paymentHandlerTestGateway{
		precreateResult: &domainpayment.PrecreateResult{QRCode: "https://qr.alipay.com/bax-should-not-happen"},
	}
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, gateway),
		paymentHandlerTestMisClaims(),
	)

	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", `{"registrationId":1001}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); location != "" {
		t.Fatalf("幂等响应不应带 Location，实际 %q", location)
	}
	object := decodePaymentObject(t, recorder)
	if got := requirePaymentStringField(t, object, "qrCode"); got != paymentHandlerTestQRCode {
		t.Fatalf("qrCode = %q，期望 %q", got, paymentHandlerTestQRCode)
	}
	if !requirePaymentBoolField(t, object, "payable") {
		t.Fatal("窗口内 payable 应为 true")
	}
	if len(gateway.precreateCalls) != 0 {
		t.Fatalf("已有二维码时不得重复预下单，实际调用 %d 次", len(gateway.precreateCalls))
	}
	// isNew=false（本次没有写入二维码）时不得触发「预下单后重读最新状态」。
	if len(repo.outQueries) != 0 {
		t.Fatalf("未新建交易时不应按交易号重读，实际调用 %+v", repo.outQueries)
	}
}

// TestPaymentCreateTerminalStatesReturn409 覆盖终态冲突（契约 §6.8、§9、§10）：
// PAID → 409 PAYMENT_ALREADY_PAID，EXPIRED/REFUNDED → 409 PAYMENT_INVALID_TRANSITION，
// 且都不得调用支付宝。
func TestPaymentCreateTerminalStatesReturn409(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		wantCode string
	}{
		{name: "已支付", status: domainpayment.PaymentStatusPaid, wantCode: "PAYMENT_ALREADY_PAID"},
		{name: "已过期", status: domainpayment.PaymentStatusExpired, wantCode: "PAYMENT_INVALID_TRANSITION"},
		{name: "已退款", status: domainpayment.PaymentStatusRefunded, wantCode: "PAYMENT_INVALID_TRANSITION"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := paymentHandlerTestUnpaid()
			item.PaymentStatus = tc.status
			item.TransactionID = paymentHandlerTestTradeNo
			repo := newPaymentHandlerTestRepo()
			repo.item = item
			gateway := &paymentHandlerTestGateway{}
			engine := newPaymentHandlerTestRouter(
				newPaymentHandlerTestService(repo, gateway),
				paymentHandlerTestMisClaims(),
			)

			recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", `{"registrationId":1001}`)
			requirePaymentErrorEnvelope(t, recorder, http.StatusConflict, tc.wantCode)
			if len(gateway.precreateCalls) != 0 {
				t.Fatalf("终态订单不得调用支付宝预下单，实际调用 %d 次", len(gateway.precreateCalls))
			}
		})
	}
}

// TestPaymentCreatePastPayDeadlineHidesQRCode 覆盖已过 pay_deadline 但未过 expire_at：
// 200、payable=false、响应不含 qrCode，且不新建支付宝交易（契约 §6.5、§6.8、§6.9）。
func TestPaymentCreatePastPayDeadlineHidesQRCode(t *testing.T) {
	item := paymentHandlerTestUnpaid()
	item.PayDeadline = paymentHandlerTestNow.Add(-time.Minute)
	item.ExpireAt = paymentHandlerTestNow.Add(4 * time.Minute)
	repo := newPaymentHandlerTestRepo()
	repo.item = item
	gateway := &paymentHandlerTestGateway{}
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, gateway),
		paymentHandlerTestMisClaims(),
	)

	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", `{"registrationId":1001}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	object := decodePaymentObject(t, recorder)
	if requirePaymentBoolField(t, object, "payable") {
		t.Fatal("已过 pay_deadline 时 payable 应为 false")
	}
	if raw, exists := object["qrCode"]; exists {
		t.Fatalf("不可支付时响应不得包含 qrCode 字段，实际 %s", raw)
	}
	if len(gateway.precreateCalls) != 0 {
		t.Fatalf("已过支付截止不得新建支付宝交易，实际调用 %d 次", len(gateway.precreateCalls))
	}
}

// TestPaymentCreateNotFound 覆盖订单不存在：404 PAYMENT_NOT_FOUND（契约 §10）。
func TestPaymentCreateNotFound(t *testing.T) {
	repo := newPaymentHandlerTestRepo()
	repo.findErr = domainpayment.ErrPaymentNotFound
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, &paymentHandlerTestGateway{}),
		paymentHandlerTestMisClaims(),
	)

	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", `{"registrationId":1001}`)
	requirePaymentErrorEnvelope(t, recorder, http.StatusNotFound, "PAYMENT_NOT_FOUND")
}

// TestPaymentCreateProviderUnavailable 覆盖预下单失败：
// 502 PAYMENT_PROVIDER_UNAVAILABLE，且响应不得包含二维码（契约 §6.2、§10）。
func TestPaymentCreateProviderUnavailable(t *testing.T) {
	item := paymentHandlerTestUnpaid()
	item.PrepayID = ""
	repo := newPaymentHandlerTestRepo()
	repo.item = item
	gateway := &paymentHandlerTestGateway{precreateErr: port.ErrAlipayUnavailable}
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, gateway),
		paymentHandlerTestMisClaims(),
	)

	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", `{"registrationId":1001}`)
	requirePaymentErrorEnvelope(t, recorder, http.StatusBadGateway, "PAYMENT_PROVIDER_UNAVAILABLE")
	if strings.Contains(recorder.Body.String(), "qrCode") {
		t.Fatalf("预下单失败不得返回二维码，body=%s", recorder.Body.String())
	}
}

// TestPaymentCreateConcurrentReplayReturns200 覆盖并发重放（契约 §6.9）：
// SavePrepayID 返回 false 表示二维码已被另一请求先行写入，此时接口必须返回 200、
// 不带 Location，且响应里的 qrCode 为回读到的库中既有值；整个过程只预下单一次、只回写一次。
func TestPaymentCreateConcurrentReplayReturns200(t *testing.T) {
	const providerQRCode = "https://qr.alipay.com/bax-loser-provider"
	const storedQRCode = "https://qr.alipay.com/bax-winner-stored"

	item := paymentHandlerTestUnpaid()
	item.PrepayID = "" // 无二维码，触发预下单分支

	repo := newPaymentHandlerTestRepo()
	repo.item = item
	repo.saveResult = false // 回写被判定为「二维码已存在」
	stored := paymentHandlerTestUnpaid()
	stored.PrepayID = storedQRCode
	repo.outTradeNoItem = stored

	gateway := &paymentHandlerTestGateway{
		precreateResult: &domainpayment.PrecreateResult{QRCode: providerQRCode},
	}
	engine := newPaymentHandlerTestRouter(
		newPaymentHandlerTestService(repo, gateway),
		paymentHandlerTestMisClaims(),
	)

	recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", `{"registrationId":1001}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态码 = %d，期望 200（并发重放不是首次创建，body=%s）", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); location != "" {
		t.Fatalf("并发重放响应不应带 Location，实际 %q", location)
	}
	object := decodePaymentObject(t, recorder)
	if got := requirePaymentStringField(t, object, "qrCode"); got != storedQRCode {
		t.Fatalf("qrCode = %q，期望回读到的既有二维码 %q", got, storedQRCode)
	}
	if !requirePaymentBoolField(t, object, "payable") {
		t.Fatal("窗口内 payable 应为 true")
	}
	if len(gateway.precreateCalls) != 1 {
		t.Fatalf("预下单调用次数 = %d，期望 1（不得重复预下单）", len(gateway.precreateCalls))
	}
	if len(repo.saveCalls) != 1 || repo.saveCalls[0] != (paymentHandlerTestSaveCall{paymentHandlerTestOutTradeNo, providerQRCode}) {
		t.Fatalf("二维码回写调用 = %+v，期望仅一次 {%s %s}", repo.saveCalls, paymentHandlerTestOutTradeNo, providerQRCode)
	}
	if len(repo.ensureCalls) != 1 || repo.ensureCalls[0] != paymentHandlerTestOutTradeNo {
		t.Fatalf("补齐支付窗口调用 = %v，期望仅一次 [%s]", repo.ensureCalls, paymentHandlerTestOutTradeNo)
	}
	// 这里只应有 precreate 内部的一次回读：isNew=false（SavePrepayID 未写入）时，
	// CreateOrder 不得再触发一次「重读最新状态」，否则断言到的次数会是 2。
	if len(repo.outQueries) != 1 || repo.outQueries[0].ownerPatientID != 0 {
		t.Fatalf("按交易号回读调用 = %+v，期望一次且管理端不限定归属", repo.outQueries)
	}
}

// TestPaymentCreateFinalStateRecheckReturns409 覆盖补齐窗口期间的 TOCTOU 复核（契约 §6.9）：
// 初次读取仍是非终态订单，EnsurePaymentWindow 返回时订单已被异步通知改成终态，
// 接口必须返回 409（PAID → PAYMENT_ALREADY_PAID，EXPIRED/REFUNDED → PAYMENT_INVALID_TRANSITION），
// 而不是把已支付、已结束的订单当成 200 或 201 返回。
func TestPaymentCreateFinalStateRecheckReturns409(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		wantCode string
	}{
		{name: "补齐期间被标记为已支付", status: domainpayment.PaymentStatusPaid, wantCode: "PAYMENT_ALREADY_PAID"},
		{name: "补齐期间被标记为已过期", status: domainpayment.PaymentStatusExpired, wantCode: "PAYMENT_INVALID_TRANSITION"},
		{name: "补齐期间被标记为已退款", status: domainpayment.PaymentStatusRefunded, wantCode: "PAYMENT_INVALID_TRANSITION"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 初始订单非终态且缺少二维码：确保用例真正走到 EnsurePaymentWindow 之后的分支。
			item := paymentHandlerTestUnpaid()
			item.PrepayID = ""
			ensured := paymentHandlerTestUnpaid()
			ensured.PaymentStatus = tc.status
			ensured.TransactionID = paymentHandlerTestTradeNo

			repo := newPaymentHandlerTestRepo()
			repo.item = item
			repo.ensureItem = ensured
			gateway := &paymentHandlerTestGateway{}
			engine := newPaymentHandlerTestRouter(
				newPaymentHandlerTestService(repo, gateway),
				paymentHandlerTestMisClaims(),
			)

			recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", `{"registrationId":1001}`)
			requirePaymentErrorEnvelope(t, recorder, http.StatusConflict, tc.wantCode)
			if len(repo.ensureCalls) != 1 {
				t.Fatalf("补齐支付窗口调用次数 = %d，期望 1（必须走到补齐后的终态复核）", len(repo.ensureCalls))
			}
			if len(gateway.precreateCalls) != 0 {
				t.Fatalf("终态订单不得调用支付宝预下单，实际调用 %d 次", len(gateway.precreateCalls))
			}
			if len(repo.saveCalls) != 0 {
				t.Fatalf("终态订单不得回写二维码，实际回写 %d 次", len(repo.saveCalls))
			}
		})
	}
}

// TestPaymentCreateTerminalDuringPrecreateReturns409 覆盖「预下单期间订单进入终态」的重读复核
// （契约 §6.9）：初始订单 UNPAID 且无二维码，本次确实完成了预下单与二维码回写（isNew=true），
// 但按 out_trade_no 重读发现订单已 PAID / EXPIRED / REFUNDED。接口必须返回 409
// （PAID → PAYMENT_ALREADY_PAID，EXPIRED/REFUNDED → PAYMENT_INVALID_TRANSITION），
// 不设置 Location、响应体不含 qrCode 字段与二维码内容，也不得把 HTTP 状态退化成 201/200；
// 同时确认确实按交易号重读且管理端不限定归属（ownerPatientID=0）。
func TestPaymentCreateTerminalDuringPrecreateReturns409(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		wantCode string
	}{
		{name: "预下单期间被标记为已支付", status: domainpayment.PaymentStatusPaid, wantCode: "PAYMENT_ALREADY_PAID"},
		{name: "预下单期间被标记为已过期", status: domainpayment.PaymentStatusExpired, wantCode: "PAYMENT_INVALID_TRANSITION"},
		{name: "预下单期间被标记为已退款", status: domainpayment.PaymentStatusRefunded, wantCode: "PAYMENT_INVALID_TRANSITION"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := paymentHandlerTestUnpaid()
			item.PrepayID = "" // 无二维码，触发预下单分支

			// 重读返回的最新状态：预下单期间订单已进入终态，且仍带着二维码，
			// 一旦实现把二维码交付客户端，下面的内容断言会立刻失败。
			latest := paymentHandlerTestUnpaid()
			latest.PaymentStatus = tc.status
			latest.TransactionID = paymentHandlerTestTradeNo
			latest.PrepayID = paymentHandlerTestQRCode

			repo := newPaymentHandlerTestRepo()
			repo.item = item
			repo.outTradeNoItem = latest

			gateway := &paymentHandlerTestGateway{}
			engine := newPaymentHandlerTestRouter(
				newPaymentHandlerTestService(repo, gateway),
				paymentHandlerTestMisClaims(),
			)

			recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", `{"registrationId":1001}`)
			requirePaymentErrorEnvelope(t, recorder, http.StatusConflict, tc.wantCode)
			if location := recorder.Header().Get("Location"); location != "" {
				t.Fatalf("终态冲突响应不应带 Location，实际 %q", location)
			}
			object := decodePaymentObject(t, recorder)
			if _, exists := object["qrCode"]; exists {
				t.Fatalf("终态冲突响应不得包含 qrCode 字段，实际 body=%s", recorder.Body.String())
			}
			if body := recorder.Body.String(); strings.Contains(body, paymentHandlerTestQRCode) {
				t.Fatalf("终态冲突响应不得出现二维码内容，实际 body=%s", body)
			}

			// 本次确实完成了预下单与回写（isNew=true），才会走到「重读最新状态」这一步。
			if len(gateway.precreateCalls) != 1 {
				t.Fatalf("预下单调用次数 = %d，期望 1", len(gateway.precreateCalls))
			}
			if len(repo.saveCalls) != 1 || repo.saveCalls[0] != (paymentHandlerTestSaveCall{paymentHandlerTestOutTradeNo, paymentHandlerTestQRCode}) {
				t.Fatalf("二维码回写调用 = %+v，期望仅一次 {%s %s}",
					repo.saveCalls, paymentHandlerTestOutTradeNo, paymentHandlerTestQRCode)
			}
			// 必须按 out_trade_no 重读，且管理端不限定归属（ownerPatientID=0）。
			if len(repo.outQueries) != 1 || repo.outQueries[0] != (paymentHandlerTestOutQuery{paymentHandlerTestOutTradeNo, 0}) {
				t.Fatalf("按交易号重读入参 = %+v，期望一次 {%s 0}", repo.outQueries, paymentHandlerTestOutTradeNo)
			}
		})
	}
}

// TestPaymentCreateRecheckErrors 覆盖预下单后重读最新状态的失败分支（契约 §6.9、§10）：
// 重读时订单已不存在 → 404 PAYMENT_NOT_FOUND；重读报错 → 502 DEPENDENCY_UNAVAILABLE。
// 两种失败都不得把二维码交给客户端，也不得设置 Location。
func TestPaymentCreateRecheckErrors(t *testing.T) {
	cases := []struct {
		name          string
		outTradeNoErr error
		wantStatus    int
		wantCode      string
	}{
		{
			name:          "重读时订单已不存在",
			outTradeNoErr: domainpayment.ErrPaymentNotFound,
			wantStatus:    http.StatusNotFound,
			wantCode:      "PAYMENT_NOT_FOUND",
		},
		{
			name:          "重读依赖故障",
			outTradeNoErr: errors.New("postgres 连接失败"),
			wantStatus:    http.StatusBadGateway,
			wantCode:      "DEPENDENCY_UNAVAILABLE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := paymentHandlerTestUnpaid()
			item.PrepayID = ""

			repo := newPaymentHandlerTestRepo()
			repo.item = item
			repo.outTradeNoErr = tc.outTradeNoErr

			gateway := &paymentHandlerTestGateway{}
			engine := newPaymentHandlerTestRouter(
				newPaymentHandlerTestService(repo, gateway),
				paymentHandlerTestMisClaims(),
			)

			recorder := doPaymentRequest(engine, http.MethodPost, "/api/v1/payments/orders", "application/json", `{"registrationId":1001}`)
			requirePaymentErrorEnvelope(t, recorder, tc.wantStatus, tc.wantCode)
			if location := recorder.Header().Get("Location"); location != "" {
				t.Fatalf("重读失败响应不应带 Location，实际 %q", location)
			}
			if body := recorder.Body.String(); strings.Contains(body, paymentHandlerTestQRCode) {
				t.Fatalf("重读失败响应不得出现二维码内容，实际 body=%s", body)
			}
			if len(gateway.precreateCalls) != 1 || len(repo.saveCalls) != 1 {
				t.Fatalf("预下单/回写次数 = %d/%d，期望各 1（必须先完成预下单再重读）",
					len(gateway.precreateCalls), len(repo.saveCalls))
			}
			if len(repo.outQueries) != 1 || repo.outQueries[0] != (paymentHandlerTestOutQuery{paymentHandlerTestOutTradeNo, 0}) {
				t.Fatalf("按交易号重读入参 = %+v，期望一次 {%s 0}", repo.outQueries, paymentHandlerTestOutTradeNo)
			}
		})
	}
}
