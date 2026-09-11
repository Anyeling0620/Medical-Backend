// 支付用例单测：取支付参数（契约 §6.5）、查询支付状态（§6.6）与支付宝异步通知（§6.7）。
//
// 仓储与支付宝适配器都用内存 fake 替换，fake 记录「调用次数 + 传入参数」，
// 避免只断言返回值而漏掉「是否调用、调用几次、传了什么」；时钟通过 Config.Now 注入，
// 用例不依赖 PostgreSQL、Redis、真实支付宝与系统时间。
package payment

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainpayment "Medical-Web-Backend/internal/domain/payment"
	"Medical-Web-Backend/internal/port"
)

// paymentTestServiceNow 是注入用例的固定时钟：业务时区（UTC+8）2026-09-10 09:00。
var paymentTestServiceNow = time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)

const (
	paymentTestRegistrationID = int64(1001)
	paymentTestOutTradeNo     = "202609080001"
	paymentTestTradeNo        = "2026090822001456789012"
	paymentTestAmount         = "80.00"
	paymentTestSubject        = "医院挂号费"
	paymentTestNotifyURL      = "https://api.example.test/api/v1/payments/alipay/notify"
	paymentTestQRCode         = "https://qr.alipay.com/bax0123456789"
)

// paymentTestMisActor 返回管理端调用者：仓储不限定归属。
func paymentTestMisActor() Actor {
	return Actor{Realm: domainauth.RealmMis, UserID: 9}
}

// paymentTestPatientActor 返回患者端调用者：只能访问本人订单。
func paymentTestPatientActor(patientID int64) Actor {
	return Actor{Realm: domainauth.RealmPatient, PatientID: patientID, UserID: patientID}
}

// paymentTestUnpaid 构造窗口内的 UNPAID 订单：
// pay_deadline = now+30m、expire_at = now+35m，prepay_id 已写入（默认不触发预下单）。
func paymentTestUnpaid() *domainpayment.Payment {
	return &domainpayment.Payment{
		RegistrationID: paymentTestRegistrationID,
		OutTradeNo:     paymentTestOutTradeNo,
		Amount:         paymentTestAmount,
		PaymentStatus:  domainpayment.PaymentStatusUnpaid,
		PrepayID:       paymentTestQRCode,
		PrecreateAt:    paymentTestServiceNow,
		PayDeadline:    paymentTestServiceNow.Add(30 * time.Minute),
		ExpireAt:       paymentTestServiceNow.Add(35 * time.Minute),
	}
}

// paymentTestNotifyPayload 构造验签与身份校验通过后的通知载荷，金额默认与订单一致。
func paymentTestNotifyPayload() *domainpayment.NotifyPayload {
	return &domainpayment.NotifyPayload{
		AppID:       "2021000000000000",
		SellerID:    "2088101106499364",
		OutTradeNo:  paymentTestOutTradeNo,
		TradeNo:     paymentTestTradeNo,
		TradeStatus: domainpayment.TradeStatusSuccess,
		TotalAmount: paymentTestAmount,
		GmtPayment:  "2026-09-10 09:00:00",
		NotifyTime:  "2026-09-10 09:00:01",
	}
}

// newPaymentTestService 构造支付用例：注入固定时钟与显式配置，便于断言传给 provider 的参数。
func newPaymentTestService(repo port.PaymentRepository, gateway port.AlipayGateway) *Service {
	return NewService(repo, gateway, Config{
		PaymentSubject:   paymentTestSubject,
		NotifyURL:        paymentTestNotifyURL,
		LogDroppedNotify: false,
		Now:              func() time.Time { return paymentTestServiceNow },
	})
}

// ---- 内存 fake：记录调用次数与传入参数 ----

// fakePaymentRepository 是 port.PaymentRepository 的内存桩。
//
// 读取结果、故障与条件更新结果都可注入；每次调用都记录入参，供用例断言
// 「是否调用、调用几次、归属开关传了什么、写入了什么」。
type fakePaymentRepository struct {
	// 读取结果：订单与故障注入。
	item    *domainpayment.Payment
	findErr error
	// latestItem 是条件更新未生效时重读返回的最新状态（nil 表示沿用 item）。
	latestItem *domainpayment.Payment
	// outTradeNoItem 是按交易号读取的专属返回（nil 表示沿用 item）。
	// 用于区分「按挂号编号读取的初始值」与「按交易号回读的值」：预下单并发竞争时，
	// 初始读取还没有二维码，但回读必须能得到并发请求已写入的那个二维码。
	outTradeNoItem *domainpayment.Payment
	// 窗口补齐：返回值与故障注入。
	ensureItem *domainpayment.Payment
	ensureErr  error
	// 回写与迁移：写入结果、故障与条件更新结果。
	saveResult     bool
	saveErr        error
	markPaidResult bool
	markPaidErr    error

	// 调用记录。
	regQueries  []findByRegistrationCall
	outQueries  []findByOutTradeNoCall
	ensureCalls []string
	saveCalls   []savePrepayIDCall
	markCalls   []markPaidCall
}

// findByRegistrationCall 记录按挂号编号读取的入参。
type findByRegistrationCall struct {
	registrationID int64
	ownerPatientID int64
}

// findByOutTradeNoCall 记录按外部交易号读取的入参。
type findByOutTradeNoCall struct {
	outTradeNo     string
	ownerPatientID int64
}

// savePrepayIDCall 记录二维码回写入参。
type savePrepayIDCall struct {
	outTradeNo string
	prepayID   string
}

// markPaidCall 记录状态迁移入参。
type markPaidCall struct {
	outTradeNo    string
	transactionID string
}

// newFakePaymentRepository 构造默认「订单可读、二维码可写入、条件更新成功」的内存桩。
func newFakePaymentRepository() *fakePaymentRepository {
	return &fakePaymentRepository{saveResult: true, markPaidResult: true}
}

func (r *fakePaymentRepository) FindPaymentByRegistrationID(
	_ context.Context,
	registrationID int64,
	ownerPatientID int64,
) (*domainpayment.Payment, error) {
	r.regQueries = append(r.regQueries, findByRegistrationCall{registrationID, ownerPatientID})
	if r.findErr != nil {
		return nil, r.findErr
	}
	return clonePayment(r.item), nil
}

func (r *fakePaymentRepository) FindPaymentByOutTradeNo(
	_ context.Context,
	outTradeNo string,
	ownerPatientID int64,
) (*domainpayment.Payment, error) {
	r.outQueries = append(r.outQueries, findByOutTradeNoCall{outTradeNo, ownerPatientID})
	if r.findErr != nil {
		return nil, r.findErr
	}
	// 注入了「按交易号读取」的专属值时必须优先返回，供预下单回读分支断言使用。
	if r.outTradeNoItem != nil {
		return clonePayment(r.outTradeNoItem), nil
	}
	// 第二次调用是「条件更新未生效」后的重读，返回注入的最新状态。
	if len(r.outQueries) > 1 && r.latestItem != nil {
		return clonePayment(r.latestItem), nil
	}
	return clonePayment(r.item), nil
}

func (r *fakePaymentRepository) EnsurePaymentWindow(_ context.Context, outTradeNo string) (*domainpayment.Payment, error) {
	r.ensureCalls = append(r.ensureCalls, outTradeNo)
	if r.ensureErr != nil {
		return nil, r.ensureErr
	}
	if r.ensureItem != nil {
		return clonePayment(r.ensureItem), nil
	}
	return clonePayment(r.item), nil
}

// SavePrepayID 记录回写入参并返回注入的写入结果与故障。
// saved=false 模拟「并发请求已先行写入二维码」，用于覆盖回读库中已有二维码的分支。
func (r *fakePaymentRepository) SavePrepayID(_ context.Context, outTradeNo string, prepayID string) (bool, error) {
	r.saveCalls = append(r.saveCalls, savePrepayIDCall{outTradeNo, prepayID})
	return r.saveResult, r.saveErr
}

func (r *fakePaymentRepository) MarkPaid(_ context.Context, outTradeNo string, transactionID string) (bool, error) {
	r.markCalls = append(r.markCalls, markPaidCall{outTradeNo, transactionID})
	return r.markPaidResult, r.markPaidErr
}

// clonePayment 返回订单副本，避免用例之间通过指针互相影响。
func clonePayment(item *domainpayment.Payment) *domainpayment.Payment {
	if item == nil {
		return nil
	}
	copied := *item
	return &copied
}

// fakeAlipayGateway 是 port.AlipayGateway 的内存桩，同样记录调用次数与传入参数。
type fakeAlipayGateway struct {
	precreateResult *domainpayment.PrecreateResult
	precreateErr    error

	queryResult *domainpayment.TradeQueryResult
	queryErr    error

	verifyPayload *domainpayment.NotifyPayload
	verifyErr     error

	precreateCalls []domainpayment.PrecreateRequest
	queryCalls     []string
	verifyCalls    []url.Values
}

func (g *fakeAlipayGateway) Precreate(_ context.Context, req domainpayment.PrecreateRequest) (*domainpayment.PrecreateResult, error) {
	g.precreateCalls = append(g.precreateCalls, req)
	if g.precreateErr != nil {
		return nil, g.precreateErr
	}
	return g.precreateResult, nil
}

func (g *fakeAlipayGateway) QueryTrade(_ context.Context, outTradeNo string) (*domainpayment.TradeQueryResult, error) {
	g.queryCalls = append(g.queryCalls, outTradeNo)
	if g.queryErr != nil {
		return nil, g.queryErr
	}
	return g.queryResult, nil
}

func (g *fakeAlipayGateway) VerifyNotify(_ context.Context, form url.Values) (*domainpayment.NotifyPayload, error) {
	g.verifyCalls = append(g.verifyCalls, form)
	if g.verifyErr != nil {
		return nil, g.verifyErr
	}
	return g.verifyPayload, nil
}

// 编译期确认两个 fake 完整实现端口契约。
var (
	_ port.PaymentRepository = (*fakePaymentRepository)(nil)
	_ port.AlipayGateway     = (*fakeAlipayGateway)(nil)
)

// ---- 断言辅助 ----

// requireServiceError 断言 err 是携带指定 code 的业务错误，并返回它以便继续校验 message。
func requireServiceError(t *testing.T, err error, wantCode string) *ServiceError {
	t.Helper()
	if err == nil {
		t.Fatalf("期望返回 %s 业务错误，实际 err = nil", wantCode)
	}
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) {
		t.Fatalf("期望 *ServiceError，实际 %T：%v", err, err)
	}
	if serviceErr.Code != wantCode {
		t.Fatalf("业务错误码 = %q，期望 %q（message=%q）", serviceErr.Code, wantCode, serviceErr.Message)
	}
	if strings.TrimSpace(serviceErr.Message) == "" {
		t.Fatal("业务错误的 message 不应为空")
	}
	return serviceErr
}

// requireErrIs 断言 err 的错误链包含指定哨兵错误。
func requireErrIs(t *testing.T, err error, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("期望错误链包含 %v，实际 %T：%v", target, err, err)
	}
}

// ---- ReadPayable：POST /api/v1/payments ----

// TestReadPayableRejectsUnsupportedMethod 覆盖 method 校验：本阶段只接受精确的 ALIPAY，
// 其他取值（含小写与空值）返回 422 REQUEST_VALIDATION_FAILED，且不得触达仓储与 provider。
func TestReadPayableRejectsUnsupportedMethod(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		wantMessage string
	}{
		{name: "WECHAT 暂不支持", method: "WECHAT", wantMessage: "暂不支持该支付方式"},
		{name: "小写 alipay 不做归一化", method: "alipay", wantMessage: "暂不支持该支付方式"},
		{name: "method 为空", method: "", wantMessage: "method 为必传字段"},
		{name: "method 仅空白字符", method: "   ", wantMessage: "method 为必传字段"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakePaymentRepository()
			gateway := &fakeAlipayGateway{}
			service := newPaymentTestService(repo, gateway)

			result, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
				RegistrationID: paymentTestRegistrationID,
				Method:         tc.method,
			})
			serviceErr := requireServiceError(t, err, CodeValidationFailed)
			if serviceErr.Message != tc.wantMessage {
				t.Fatalf("message = %q，期望 %q", serviceErr.Message, tc.wantMessage)
			}
			if result != nil {
				t.Fatalf("校验失败时不应返回结果，实际 %+v", result)
			}
			if len(repo.regQueries) != 0 {
				t.Fatalf("校验失败不应触达仓储，实际调用 %d 次", len(repo.regQueries))
			}
			if len(gateway.precreateCalls) != 0 {
				t.Fatalf("校验失败不应调用支付宝预下单，实际调用 %d 次", len(gateway.precreateCalls))
			}
		})
	}
}

// TestReadPayableRejectsInvalidRegistrationID 覆盖 registrationId 边界：
// 非正整数返回 422，且不触达仓储。
func TestReadPayableRejectsInvalidRegistrationID(t *testing.T) {
	for _, registrationID := range []int64{0, -1} {
		repo := newFakePaymentRepository()
		service := newPaymentTestService(repo, &fakeAlipayGateway{})

		_, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
			RegistrationID: registrationID,
			Method:         PaymentMethodAlipay,
		})
		if message := requireServiceError(t, err, CodeValidationFailed).Message; message != "registrationId 必须为正整数" {
			t.Fatalf("registrationId=%d 的 message = %q", registrationID, message)
		}
		if len(repo.regQueries) != 0 {
			t.Fatalf("registrationId=%d 不应触达仓储，实际调用 %d 次", registrationID, len(repo.regQueries))
		}
	}
}

// TestReadPayableNotFound 覆盖订单不存在：返回 404 PAYMENT_NOT_FOUND（契约 §10）。
func TestReadPayableNotFound(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.findErr = domainpayment.ErrPaymentNotFound
	service := newPaymentTestService(repo, &fakeAlipayGateway{})

	_, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
		RegistrationID: paymentTestRegistrationID,
		Method:         PaymentMethodAlipay,
	})
	requireServiceError(t, err, CodePaymentNotFound)
	if len(repo.regQueries) != 1 {
		t.Fatalf("仓储调用次数 = %d，期望 1", len(repo.regQueries))
	}
	if repo.regQueries[0] != (findByRegistrationCall{paymentTestRegistrationID, 0}) {
		t.Fatalf("仓储入参 = %+v，期望 registrationID=%d ownerPatientID=0", repo.regQueries[0], paymentTestRegistrationID)
	}
}

// TestReadPayableRepositoryFailureIsDependencyUnavailable 覆盖仓储故障：
// 归类为 502 DEPENDENCY_UNAVAILABLE，不得伪装成订单不存在。
func TestReadPayableRepositoryFailureIsDependencyUnavailable(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.findErr = errors.New("postgres 连接失败")
	service := newPaymentTestService(repo, &fakeAlipayGateway{})

	_, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
		RegistrationID: paymentTestRegistrationID,
		Method:         PaymentMethodAlipay,
	})
	requireErrIs(t, err, ErrDependencyUnavailable)
	if errors.Is(err, domainpayment.ErrPaymentNotFound) {
		t.Fatal("依赖故障不得被判定为订单不存在")
	}
}

// TestReadPayableOwnershipSwitch 覆盖患者端归属限定：
// 患者域必须把令牌中的 patientID 作为 ownerPatientID 传给仓储；管理端传 0 表示不限定。
func TestReadPayableOwnershipSwitch(t *testing.T) {
	cases := []struct {
		name      string
		actor     Actor
		wantOwner int64
	}{
		{name: "患者端限定本人订单", actor: paymentTestPatientActor(77), wantOwner: 77},
		{name: "管理端不限定归属", actor: paymentTestMisActor(), wantOwner: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakePaymentRepository()
			repo.item = paymentTestUnpaid()
			service := newPaymentTestService(repo, &fakeAlipayGateway{})

			if _, err := service.ReadPayable(context.Background(), tc.actor, ReadInput{
				RegistrationID: paymentTestRegistrationID,
				Method:         PaymentMethodAlipay,
			}); err != nil {
				t.Fatalf("读取支付参数不应报错，实际 %v", err)
			}
			if len(repo.regQueries) != 1 {
				t.Fatalf("仓储调用次数 = %d，期望 1", len(repo.regQueries))
			}
			if repo.regQueries[0].ownerPatientID != tc.wantOwner {
				t.Fatalf("ownerPatientID = %d，期望 %d", repo.regQueries[0].ownerPatientID, tc.wantOwner)
			}
			if repo.regQueries[0].registrationID != paymentTestRegistrationID {
				t.Fatalf("registrationID = %d，期望 %d", repo.regQueries[0].registrationID, paymentTestRegistrationID)
			}
		})
	}
}

// TestReadPayablePrecreatesWhenPrepayIDMissing 覆盖最小闭环：prepay_id 为空且在
// pay_deadline 内时补做一次 alipay.trade.precreate，回写二维码并返回 payable=true。
func TestReadPayablePrecreatesWhenPrepayIDMissing(t *testing.T) {
	item := paymentTestUnpaid()
	item.PrepayID = ""
	const newQRCode = "https://qr.alipay.com/bax-new-precreate"

	repo := newFakePaymentRepository()
	repo.item = item
	gateway := &fakeAlipayGateway{
		precreateResult: &domainpayment.PrecreateResult{QRCode: newQRCode},
	}
	service := newPaymentTestService(repo, gateway)

	result, err := service.ReadPayable(context.Background(), paymentTestPatientActor(77), ReadInput{
		RegistrationID: paymentTestRegistrationID,
		Method:         PaymentMethodAlipay,
	})
	if err != nil {
		t.Fatalf("prepay_id 为空时应补做预下单，实际报错 %v", err)
	}
	if len(repo.ensureCalls) != 1 || repo.ensureCalls[0] != paymentTestOutTradeNo {
		t.Fatalf("补齐支付窗口调用 = %v，期望 [%s]", repo.ensureCalls, paymentTestOutTradeNo)
	}
	if len(gateway.precreateCalls) != 1 {
		t.Fatalf("预下单调用次数 = %d，期望 1", len(gateway.precreateCalls))
	}
	wantReq := domainpayment.PrecreateRequest{
		OutTradeNo:     paymentTestOutTradeNo,
		Amount:         paymentTestAmount,
		Subject:        paymentTestSubject,
		TimeoutExpress: "30m",
		NotifyURL:      paymentTestNotifyURL,
	}
	if gateway.precreateCalls[0] != wantReq {
		t.Fatalf("预下单入参 = %+v，期望 %+v", gateway.precreateCalls[0], wantReq)
	}
	if len(repo.saveCalls) != 1 || repo.saveCalls[0] != (savePrepayIDCall{paymentTestOutTradeNo, newQRCode}) {
		t.Fatalf("二维码回写入参 = %+v，期望 {%s %s}", repo.saveCalls, paymentTestOutTradeNo, newQRCode)
	}
	if !result.Payable {
		t.Fatal("窗口内且预下单成功时 payable 应为 true")
	}
	if result.Payment.PrepayID != newQRCode {
		t.Fatalf("返回的 prepay_id = %q，期望 %q", result.Payment.PrepayID, newQRCode)
	}
	if result.Payment.PaymentStatus != domainpayment.PaymentStatusUnpaid {
		t.Fatalf("paymentStatus = %q，期望 %q", result.Payment.PaymentStatus, domainpayment.PaymentStatusUnpaid)
	}
}

// TestReadPayableWindowAndStatusSkipPrecreate 覆盖不触发预下单的分支：
// 已过 pay_deadline、已是终态、prepay_id 已存在等情况都不得调用 provider（契约 §6.5）。
//
// ensureItem 可选注入 EnsurePaymentWindow 的返回值，用于模拟「补窗口后读到的订单」；
// 为 nil 时仓储沿用初始查询结果。wantPrepayID 非空时额外断言返回的 prepay_id，
// 用于确认脏数据补窗口后回读的是库里已有二维码，而不是本次 provider 返回值。
func TestReadPayableWindowAndStatusSkipPrecreate(t *testing.T) {
	cases := []struct {
		name            string
		mutate          func(*domainpayment.Payment)
		ensureItem      *domainpayment.Payment
		wantPrepayID    string
		wantEnsureCalls int
		wantPayable     bool
	}{
		{
			name: "prepay_id 为空且已过 pay_deadline：补窗口但不预下单",
			mutate: func(item *domainpayment.Payment) {
				item.PrepayID = ""
				item.PayDeadline = paymentTestServiceNow.Add(-time.Minute)
				item.ExpireAt = paymentTestServiceNow.Add(4 * time.Minute)
			},
			wantEnsureCalls: 1,
			wantPayable:     false,
		},
		{
			name: "prepay_id 已存在且已过 pay_deadline：不回写也不预下单",
			mutate: func(item *domainpayment.Payment) {
				item.PayDeadline = paymentTestServiceNow.Add(-time.Minute)
				item.ExpireAt = paymentTestServiceNow.Add(4 * time.Minute)
			},
			wantEnsureCalls: 0,
			wantPayable:     false,
		},
		{
			name:            "prepay_id 已存在且在窗口内：直接回读",
			mutate:          func(item *domainpayment.Payment) {},
			wantEnsureCalls: 0,
			wantPayable:     true,
		},
		{
			name: "已 PAID 且 prepay_id 已存在：不回写也不预下单",
			mutate: func(item *domainpayment.Payment) {
				item.PaymentStatus = domainpayment.PaymentStatusPaid
				item.TransactionID = paymentTestTradeNo
			},
			wantEnsureCalls: 0,
			wantPayable:     false,
		},
		{
			name: "已 PAID 但 prepay_id 为空（历史脏数据）：只补窗口，不预下单",
			mutate: func(item *domainpayment.Payment) {
				item.PaymentStatus = domainpayment.PaymentStatusPaid
				item.PrepayID = ""
			},
			wantEnsureCalls: 1,
			wantPayable:     false,
		},
		{
			name: "已 EXPIRED：不回写也不预下单",
			mutate: func(item *domainpayment.Payment) {
				item.PaymentStatus = domainpayment.PaymentStatusExpired
			},
			wantEnsureCalls: 0,
			wantPayable:     false,
		},
		{
			name: "prepay_id 已存在但三个时间点为空（脏数据）：只补窗口、不重复预下单",
			mutate: func(item *domainpayment.Payment) {
				// 保留库里已有二维码，只清空三个时间点，模拟历史脏数据。
				item.PrecreateAt = time.Time{}
				item.PayDeadline = time.Time{}
				item.ExpireAt = time.Time{}
			},
			// 补窗口后订单窗口完整，二维码仍是库里那个：应直接回读，不得重复预下单。
			ensureItem:      paymentTestUnpaid(),
			wantPrepayID:    paymentTestQRCode,
			wantEnsureCalls: 1,
			wantPayable:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := paymentTestUnpaid()
			tc.mutate(item)

			repo := newFakePaymentRepository()
			repo.item = item
			repo.ensureItem = tc.ensureItem
			gateway := &fakeAlipayGateway{
				precreateResult: &domainpayment.PrecreateResult{QRCode: "https://qr.alipay.com/bax-should-not-happen"},
			}
			service := newPaymentTestService(repo, gateway)

			result, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
				RegistrationID: paymentTestRegistrationID,
				Method:         PaymentMethodAlipay,
			})
			if err != nil {
				t.Fatalf("读取支付参数不应报错，实际 %v", err)
			}
			if len(gateway.precreateCalls) != 0 {
				t.Fatalf("该分支不应调用预下单，实际调用 %d 次", len(gateway.precreateCalls))
			}
			if len(repo.saveCalls) != 0 {
				t.Fatalf("该分支不应回写二维码，实际回写 %d 次", len(repo.saveCalls))
			}
			if len(repo.ensureCalls) != tc.wantEnsureCalls {
				t.Fatalf("补齐支付窗口调用次数 = %d，期望 %d", len(repo.ensureCalls), tc.wantEnsureCalls)
			}
			if result.Payable != tc.wantPayable {
				t.Fatalf("payable = %v，期望 %v", result.Payable, tc.wantPayable)
			}
			if result.Payment.PaymentStatus != item.PaymentStatus {
				t.Fatalf("paymentStatus = %q，期望 %q", result.Payment.PaymentStatus, item.PaymentStatus)
			}
			if tc.wantPrepayID != "" && result.Payment.PrepayID != tc.wantPrepayID {
				t.Fatalf("返回的 prepay_id = %q，期望 %q", result.Payment.PrepayID, tc.wantPrepayID)
			}
		})
	}
}

// TestReadPayableProviderUnavailable 覆盖 provider 与回写异常：
// 预下单失败、二维码为空与回写失败都必须返回错误，且绝不把二维码交付给客户端。
func TestReadPayableProviderUnavailable(t *testing.T) {
	t.Run("预下单返回服务不可用", func(t *testing.T) {
		repo, gateway, service := newPrecreateFailureFixture()
		gateway.precreateErr = port.ErrAlipayUnavailable

		_, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
			RegistrationID: paymentTestRegistrationID,
			Method:         PaymentMethodAlipay,
		})
		serviceErr := requireServiceError(t, err, CodeProviderUnavailable)
		if serviceErr.Message != "支付渠道暂时不可用，请稍后重试" {
			t.Fatalf("message = %q", serviceErr.Message)
		}
		if len(repo.saveCalls) != 0 {
			t.Fatalf("预下单失败不应回写二维码，实际回写 %d 次", len(repo.saveCalls))
		}
	})

	t.Run("预下单返回交易已关闭", func(t *testing.T) {
		_, gateway, service := newPrecreateFailureFixture()
		gateway.precreateErr = port.ErrAlipayTradeClosed

		_, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
			RegistrationID: paymentTestRegistrationID,
			Method:         PaymentMethodAlipay,
		})
		serviceErr := requireServiceError(t, err, CodeProviderUnavailable)
		if !strings.Contains(serviceErr.Message, "不可支付") {
			t.Fatalf("message = %q，期望包含「不可支付」", serviceErr.Message)
		}
	})

	t.Run("预下单返回空二维码", func(t *testing.T) {
		repo, gateway, service := newPrecreateFailureFixture()
		gateway.precreateResult = &domainpayment.PrecreateResult{QRCode: "   "}

		_, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
			RegistrationID: paymentTestRegistrationID,
			Method:         PaymentMethodAlipay,
		})
		requireServiceError(t, err, CodeProviderUnavailable)
		if len(repo.saveCalls) != 0 {
			t.Fatalf("空二维码不应回写，实际回写 %d 次", len(repo.saveCalls))
		}
	})

	t.Run("二维码回写失败", func(t *testing.T) {
		repo, gateway, service := newPrecreateFailureFixture()
		repo.saveErr = errors.New("update 失败")

		_, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
			RegistrationID: paymentTestRegistrationID,
			Method:         PaymentMethodAlipay,
		})
		requireErrIs(t, err, ErrDependencyUnavailable)
		if len(gateway.precreateCalls) != 1 {
			t.Fatalf("预下单调用次数 = %d，期望 1", len(gateway.precreateCalls))
		}
		if len(repo.saveCalls) != 1 {
			t.Fatalf("二维码回写次数 = %d，期望 1", len(repo.saveCalls))
		}
	})
}

// newPrecreateFailureFixture 构造「prepay_id 为空且在窗口内」的固定装置，
// 供预下单失败的四个子用例复用。
func newPrecreateFailureFixture() (*fakePaymentRepository, *fakeAlipayGateway, *Service) {
	item := paymentTestUnpaid()
	item.PrepayID = ""
	repo := newFakePaymentRepository()
	repo.item = item
	gateway := &fakeAlipayGateway{
		precreateResult: &domainpayment.PrecreateResult{QRCode: paymentTestQRCode},
	}
	return repo, gateway, newPaymentTestService(repo, gateway)
}

// TestReadPayablePrepayIDWriteRace 覆盖预下单二维码回写的并发分支（契约 §6.5、§6.2 第 3.2 节）：
// SavePrepayID 返回 saved=false 表示并发请求已先行写入，此时响应必须回显库中已有的二维码，
// 而不是本次 provider 新返回的值；库中也没有二维码时只能返回 502，绝不把内存值交付给客户端；
// 二维码超过 prepay_id 列宽（64）时必须在回写前拦截，避免数据库报错冒泡成不可解释的 502。
func TestReadPayablePrepayIDWriteRace(t *testing.T) {
	const (
		providerQRCode = "https://qr.alipay.com/bax-provider-new"
		storedQRCode   = "qr-old"
	)

	t.Run("并发请求已写入：响应回显库中二维码而非本次 provider 值", func(t *testing.T) {
		repo := newFakePaymentRepository()
		repo.item = paymentTestUnpaid()
		repo.item.PrepayID = ""
		// 回读时库中已有并发请求写入的二维码，且与本次 provider 返回值不同。
		repo.outTradeNoItem = paymentTestUnpaid()
		repo.outTradeNoItem.PrepayID = storedQRCode
		repo.saveResult = false
		gateway := &fakeAlipayGateway{
			precreateResult: &domainpayment.PrecreateResult{QRCode: providerQRCode},
		}
		service := newPaymentTestService(repo, gateway)

		result, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
			RegistrationID: paymentTestRegistrationID,
			Method:         PaymentMethodAlipay,
		})
		if err != nil {
			t.Fatalf("并发写入场景不应报错，实际 %v", err)
		}
		if len(repo.saveCalls) != 1 || repo.saveCalls[0].prepayID != providerQRCode {
			t.Fatalf("回写入参 = %+v，期望尝试写入本次 provider 二维码 %q", repo.saveCalls, providerQRCode)
		}
		if len(repo.outQueries) != 1 {
			t.Fatalf("回读库中二维码的次数 = %d，期望 1", len(repo.outQueries))
		}
		if result.Payment.PrepayID != storedQRCode {
			t.Fatalf("响应 prepay_id = %q，期望库中已有二维码 %q（不得回显本次 provider 值 %q）",
				result.Payment.PrepayID, storedQRCode, providerQRCode)
		}
	})

	t.Run("并发写入未生效且库中为空：返回 502", func(t *testing.T) {
		repo := newFakePaymentRepository()
		repo.item = paymentTestUnpaid()
		repo.item.PrepayID = ""
		// 回读到的二维码仍为空：库中没有可用二维码。
		repo.outTradeNoItem = paymentTestUnpaid()
		repo.outTradeNoItem.PrepayID = ""
		repo.saveResult = false
		gateway := &fakeAlipayGateway{
			precreateResult: &domainpayment.PrecreateResult{QRCode: providerQRCode},
		}
		service := newPaymentTestService(repo, gateway)

		_, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
			RegistrationID: paymentTestRegistrationID,
			Method:         PaymentMethodAlipay,
		})
		requireServiceError(t, err, CodeProviderUnavailable)
		if len(repo.outQueries) != 1 {
			t.Fatalf("回读库中二维码的次数 = %d，期望 1", len(repo.outQueries))
		}
	})

	t.Run("二维码超过列宽：502 且不回写", func(t *testing.T) {
		repo := newFakePaymentRepository()
		repo.item = paymentTestUnpaid()
		repo.item.PrepayID = ""
		gateway := &fakeAlipayGateway{
			// prepay_id 列宽为 64，65 个字符必然写不进去，必须在回写前拦截。
			precreateResult: &domainpayment.PrecreateResult{QRCode: strings.Repeat("q", 65)},
		}
		service := newPaymentTestService(repo, gateway)

		_, err := service.ReadPayable(context.Background(), paymentTestMisActor(), ReadInput{
			RegistrationID: paymentTestRegistrationID,
			Method:         PaymentMethodAlipay,
		})
		requireServiceError(t, err, CodeProviderUnavailable)
		if len(repo.saveCalls) != 0 {
			t.Fatalf("超长二维码不得回写，实际回写 %d 次", len(repo.saveCalls))
		}
	})
}

// ---- Detail：GET /api/v1/payments/{outTradeNo} ----

// TestDetailQueryMarksPaid 覆盖兜底主动查询：UNPAID 且未超过 expire_at 时查询一次，
// 命中 TRADE_SUCCESS/TRADE_FINISHED 时走与通知相同的迁移并回显 PAID（契约 §6.6）。
func TestDetailQueryMarksPaid(t *testing.T) {
	for _, tradeStatus := range []string{domainpayment.TradeStatusSuccess, domainpayment.TradeStatusFinished} {
		t.Run(tradeStatus, func(t *testing.T) {
			repo := newFakePaymentRepository()
			repo.item = paymentTestUnpaid()
			gateway := &fakeAlipayGateway{
				queryResult: &domainpayment.TradeQueryResult{
					OutTradeNo:  paymentTestOutTradeNo,
					TradeNo:     paymentTestTradeNo,
					TradeStatus: tradeStatus,
					TotalAmount: paymentTestAmount,
				},
			}
			service := newPaymentTestService(repo, gateway)

			item, err := service.Detail(context.Background(), paymentTestPatientActor(77), paymentTestOutTradeNo)
			if err != nil {
				t.Fatalf("兜底查询命中成功状态不应报错，实际 %v", err)
			}
			if len(repo.outQueries) != 1 || repo.outQueries[0] != (findByOutTradeNoCall{paymentTestOutTradeNo, 77}) {
				t.Fatalf("仓储入参 = %+v，期望 outTradeNo=%s ownerPatientID=77", repo.outQueries, paymentTestOutTradeNo)
			}
			if len(gateway.queryCalls) != 1 || gateway.queryCalls[0] != paymentTestOutTradeNo {
				t.Fatalf("主动查询入参 = %v，期望 [%s]", gateway.queryCalls, paymentTestOutTradeNo)
			}
			if len(repo.markCalls) != 1 || repo.markCalls[0] != (markPaidCall{paymentTestOutTradeNo, paymentTestTradeNo}) {
				t.Fatalf("迁移入参 = %+v，期望 outTradeNo=%s tradeNo=%s", repo.markCalls, paymentTestOutTradeNo, paymentTestTradeNo)
			}
			if item.PaymentStatus != domainpayment.PaymentStatusPaid {
				t.Fatalf("paymentStatus = %q，期望 %q", item.PaymentStatus, domainpayment.PaymentStatusPaid)
			}
			if item.TransactionID != paymentTestTradeNo {
				t.Fatalf("transactionId = %q，期望 %q", item.TransactionID, paymentTestTradeNo)
			}
		})
	}
}

// TestDetailSkipsQueryWhenNotQueryable 覆盖不做主动查询的分支：
// 已 PAID 或已超过 expire_at 时只回显当前状态，不得调用 provider，也不得迁移状态。
func TestDetailSkipsQueryWhenNotQueryable(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domainpayment.Payment)
	}{
		{
			name: "已 PAID",
			mutate: func(item *domainpayment.Payment) {
				item.PaymentStatus = domainpayment.PaymentStatusPaid
				item.TransactionID = paymentTestTradeNo
			},
		},
		{
			name: "UNPAID 但已超过 expire_at",
			mutate: func(item *domainpayment.Payment) {
				item.ExpireAt = paymentTestServiceNow.Add(-time.Nanosecond)
			},
		},
		{
			name: "已 EXPIRED",
			mutate: func(item *domainpayment.Payment) {
				item.PaymentStatus = domainpayment.PaymentStatusExpired
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := paymentTestUnpaid()
			tc.mutate(item)
			repo := newFakePaymentRepository()
			repo.item = item
			gateway := &fakeAlipayGateway{
				queryResult: &domainpayment.TradeQueryResult{
					TradeStatus: domainpayment.TradeStatusSuccess,
					TradeNo:     paymentTestTradeNo,
				},
			}
			service := newPaymentTestService(repo, gateway)

			got, err := service.Detail(context.Background(), paymentTestMisActor(), paymentTestOutTradeNo)
			if err != nil {
				t.Fatalf("查询支付状态不应报错，实际 %v", err)
			}
			if len(gateway.queryCalls) != 0 {
				t.Fatalf("该分支不应调用主动查询，实际调用 %d 次", len(gateway.queryCalls))
			}
			if len(repo.markCalls) != 0 {
				t.Fatalf("该分支不应迁移状态，实际迁移 %d 次", len(repo.markCalls))
			}
			if got.PaymentStatus != item.PaymentStatus {
				t.Fatalf("paymentStatus = %q，期望 %q", got.PaymentStatus, item.PaymentStatus)
			}
		})
	}
}

// TestDetailWithoutGatewaySkipsQuery 未配置支付宝适配器时（本地联调）只回显状态，
// 不因 provider 缺失而报错。
func TestDetailWithoutGatewaySkipsQuery(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.item = paymentTestUnpaid()
	service := newPaymentTestService(repo, nil)

	item, err := service.Detail(context.Background(), paymentTestMisActor(), paymentTestOutTradeNo)
	if err != nil {
		t.Fatalf("未配置 provider 时不应报错，实际 %v", err)
	}
	if item.PaymentStatus != domainpayment.PaymentStatusUnpaid {
		t.Fatalf("paymentStatus = %q，期望 %q", item.PaymentStatus, domainpayment.PaymentStatusUnpaid)
	}
	if len(repo.markCalls) != 0 {
		t.Fatalf("未配置 provider 时不应迁移状态，实际迁移 %d 次", len(repo.markCalls))
	}
}

// TestDetailProviderFailureIsUnavailable 覆盖主动查询失败：返回 502 PAYMENT_PROVIDER_UNAVAILABLE，
// 不迁移状态（契约 §6.6、§10）。
func TestDetailProviderFailureIsUnavailable(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.item = paymentTestUnpaid()
	gateway := &fakeAlipayGateway{queryErr: port.ErrAlipayUnavailable}
	service := newPaymentTestService(repo, gateway)

	_, err := service.Detail(context.Background(), paymentTestMisActor(), paymentTestOutTradeNo)
	requireServiceError(t, err, CodeProviderUnavailable)
	if len(repo.markCalls) != 0 {
		t.Fatalf("主动查询失败不应迁移状态，实际迁移 %d 次", len(repo.markCalls))
	}
}

// TestDetailNonSuccessTradeStatusKeepsUnpaid 覆盖非成功查询结果：
// WAIT_BUYER_PAY 与「支付宝侧无此交易」（trade_status 为空）都不构成迁移证据。
func TestDetailNonSuccessTradeStatusKeepsUnpaid(t *testing.T) {
	cases := []struct {
		name        string
		tradeStatus string
	}{
		{name: "WAIT_BUYER_PAY", tradeStatus: domainpayment.TradeStatusWaitBuyerPay},
		{name: "TRADE_CLOSED", tradeStatus: domainpayment.TradeStatusClosed},
		{name: "支付宝侧无此交易（空状态）", tradeStatus: ""},
		{name: "未知状态", tradeStatus: "TRADE_UNKNOWN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakePaymentRepository()
			repo.item = paymentTestUnpaid()
			gateway := &fakeAlipayGateway{
				queryResult: &domainpayment.TradeQueryResult{
					OutTradeNo:  paymentTestOutTradeNo,
					TradeStatus: tc.tradeStatus,
				},
			}
			service := newPaymentTestService(repo, gateway)

			item, err := service.Detail(context.Background(), paymentTestMisActor(), paymentTestOutTradeNo)
			if err != nil {
				t.Fatalf("非成功状态不应报错，实际 %v", err)
			}
			if len(repo.markCalls) != 0 {
				t.Fatalf("非成功状态不应迁移，实际迁移 %d 次", len(repo.markCalls))
			}
			if item.PaymentStatus != domainpayment.PaymentStatusUnpaid {
				t.Fatalf("paymentStatus = %q，期望 UNPAID", item.PaymentStatus)
			}
		})
	}
}

// TestDetailAmountGuard 覆盖主动查询的金额校验（审查 P2-3）：
// 查询金额与订单金额不一致时只告警、不迁移状态；"80.0" 与 "80.00" 这类等价写法
// 必须被判定为一致并正常迁移（契约 §6.6「走与通知完全相同的状态迁移」）。
func TestDetailAmountGuard(t *testing.T) {
	t.Run("金额不一致：不迁移状态且保持 UNPAID", func(t *testing.T) {
		repo := newFakePaymentRepository()
		repo.item = paymentTestUnpaid() // 订单金额 80.00
		gateway := &fakeAlipayGateway{
			queryResult: &domainpayment.TradeQueryResult{
				OutTradeNo:  paymentTestOutTradeNo,
				TradeNo:     paymentTestTradeNo,
				TradeStatus: domainpayment.TradeStatusSuccess,
				TotalAmount: "80.01",
			},
		}
		service := newPaymentTestService(repo, gateway)

		item, err := service.Detail(context.Background(), paymentTestPatientActor(77), paymentTestOutTradeNo)
		if err != nil {
			t.Fatalf("金额不一致只告警不迁移，不应报错，实际 %v", err)
		}
		if len(repo.markCalls) != 0 {
			t.Fatalf("金额不一致不得迁移状态，实际迁移 %+v", repo.markCalls)
		}
		if item.PaymentStatus != domainpayment.PaymentStatusUnpaid {
			t.Fatalf("paymentStatus = %q，期望 %q", item.PaymentStatus, domainpayment.PaymentStatusUnpaid)
		}
		if item.TransactionID != "" {
			t.Fatalf("transactionId = %q，期望保持为空", item.TransactionID)
		}
	})

	for _, amount := range []string{"80.0", "80.00"} {
		t.Run("金额等价写法 "+amount+"：正常迁移为 PAID", func(t *testing.T) {
			repo := newFakePaymentRepository()
			repo.item = paymentTestUnpaid()
			gateway := &fakeAlipayGateway{
				queryResult: &domainpayment.TradeQueryResult{
					OutTradeNo:  paymentTestOutTradeNo,
					TradeNo:     paymentTestTradeNo,
					TradeStatus: domainpayment.TradeStatusSuccess,
					TotalAmount: amount,
				},
			}
			service := newPaymentTestService(repo, gateway)

			item, err := service.Detail(context.Background(), paymentTestMisActor(), paymentTestOutTradeNo)
			if err != nil {
				t.Fatalf("等价金额不应报错，实际 %v", err)
			}
			if len(repo.markCalls) != 1 || repo.markCalls[0] != (markPaidCall{paymentTestOutTradeNo, paymentTestTradeNo}) {
				t.Fatalf("迁移入参 = %+v，期望 outTradeNo=%s tradeNo=%s",
					repo.markCalls, paymentTestOutTradeNo, paymentTestTradeNo)
			}
			if item.PaymentStatus != domainpayment.PaymentStatusPaid {
				t.Fatalf("paymentStatus = %q，期望 %q", item.PaymentStatus, domainpayment.PaymentStatusPaid)
			}
		})
	}
}

// TestDetailRereadsWhenConditionalUpdateLoses 覆盖并发竞争：条件更新未生效时
// 必须重读真实状态，不得覆盖其它路径的结果（契约 §6.6、§9）。
func TestDetailRereadsWhenConditionalUpdateLoses(t *testing.T) {
	latest := paymentTestUnpaid()
	latest.PaymentStatus = domainpayment.PaymentStatusPaid
	latest.TransactionID = paymentTestTradeNo

	repo := newFakePaymentRepository()
	repo.item = paymentTestUnpaid()
	repo.latestItem = latest
	repo.markPaidResult = false
	gateway := &fakeAlipayGateway{
		queryResult: &domainpayment.TradeQueryResult{
			TradeStatus: domainpayment.TradeStatusSuccess,
			TradeNo:     paymentTestTradeNo,
			// 金额必须与订单一致：主动查询结果先过金额校验，才会走条件更新。
			TotalAmount: paymentTestAmount,
		},
	}
	service := newPaymentTestService(repo, gateway)

	item, err := service.Detail(context.Background(), paymentTestMisActor(), paymentTestOutTradeNo)
	if err != nil {
		t.Fatalf("条件更新未生效不应报错，实际 %v", err)
	}
	if len(repo.markCalls) != 1 {
		t.Fatalf("迁移调用次数 = %d，期望 1", len(repo.markCalls))
	}
	if len(repo.outQueries) != 2 {
		t.Fatalf("仓储读取次数 = %d，期望 2（首次读取 + 竞争后重读）", len(repo.outQueries))
	}
	if item.PaymentStatus != domainpayment.PaymentStatusPaid || item.TransactionID != paymentTestTradeNo {
		t.Fatalf("重读结果 = %+v，期望 PAID/%s", item, paymentTestTradeNo)
	}
}

// TestDetailHidesForeignOrderAsNotFound 覆盖患者端越权：仓储以归属条件过滤，
// 越权与不存在统一返回 404 PAYMENT_NOT_FOUND（契约 §1.2、§6.6）。
func TestDetailHidesForeignOrderAsNotFound(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.findErr = domainpayment.ErrPaymentNotFound
	service := newPaymentTestService(repo, &fakeAlipayGateway{})

	_, err := service.Detail(context.Background(), paymentTestPatientActor(77), paymentTestOutTradeNo)
	requireServiceError(t, err, CodePaymentNotFound)
	if len(repo.outQueries) != 1 || repo.outQueries[0] != (findByOutTradeNoCall{paymentTestOutTradeNo, 77}) {
		t.Fatalf("仓储入参 = %+v，期望 outTradeNo=%s ownerPatientID=77", repo.outQueries, paymentTestOutTradeNo)
	}
}

// TestDetailRejectsInvalidOutTradeNo 覆盖路径参数校验：
// 空值与超过 out_trade_no 列宽（32）的取值返回 422，且不触达仓储。
func TestDetailRejectsInvalidOutTradeNo(t *testing.T) {
	for _, outTradeNo := range []string{"", "   ", strings.Repeat("a", 33)} {
		repo := newFakePaymentRepository()
		service := newPaymentTestService(repo, &fakeAlipayGateway{})

		_, err := service.Detail(context.Background(), paymentTestMisActor(), outTradeNo)
		requireServiceError(t, err, CodeValidationFailed)
		if len(repo.outQueries) != 0 {
			t.Fatalf("outTradeNo=%q 不应触达仓储，实际调用 %d 次", outTradeNo, len(repo.outQueries))
		}
	}
}

// TestServicesRejectPatientActorWithoutPatientID 覆盖身份自洽校验：
// 声明患者域却缺少 patientID 时必须拒绝，避免归属条件被静默关闭。
func TestServicesRejectPatientActorWithoutPatientID(t *testing.T) {
	actor := Actor{Realm: domainauth.RealmPatient, UserID: 20}
	repo := newFakePaymentRepository()
	service := newPaymentTestService(repo, &fakeAlipayGateway{})

	if _, err := service.ReadPayable(context.Background(), actor, ReadInput{
		RegistrationID: paymentTestRegistrationID,
		Method:         PaymentMethodAlipay,
	}); !errors.Is(err, ErrInvalidActor) {
		t.Fatalf("ReadPayable 期望 ErrInvalidActor，实际 %T：%v", err, err)
	}
	if _, err := service.Detail(context.Background(), actor, paymentTestOutTradeNo); !errors.Is(err, ErrInvalidActor) {
		t.Fatalf("Detail 期望 ErrInvalidActor，实际 %T：%v", err, err)
	}
	if len(repo.regQueries) != 0 || len(repo.outQueries) != 0 {
		t.Fatalf("身份非法时不应触达仓储，实际 reg=%d out=%d", len(repo.regQueries), len(repo.outQueries))
	}
}

// ---- HandleNotify：POST /api/v1/payments/alipay/notify ----

// TestHandleNotifySignatureAndIdentityErrors 覆盖验签与身份异常：
// 验签失败返回 400 PAYMENT_NOTIFY_SIGNATURE_INVALID，身份不一致返回
// 400 PAYMENT_NOTIFY_IDENTITY_MISMATCH，其余 provider 故障按 502 处理（契约 §6.7、§10）。
func TestHandleNotifySignatureAndIdentityErrors(t *testing.T) {
	cases := []struct {
		name       string
		gatewayErr error
		wantCode   string
	}{
		{name: "验签失败", gatewayErr: port.ErrNotifySignatureInvalid, wantCode: CodeNotifySignatureInvalid},
		{name: "身份不一致", gatewayErr: port.ErrNotifyIdentityMismatch, wantCode: CodeNotifyIdentityMismatch},
		{name: "provider 其它故障", gatewayErr: errors.New("网络超时"), wantCode: CodeProviderUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakePaymentRepository()
			repo.item = paymentTestUnpaid()
			gateway := &fakeAlipayGateway{verifyErr: tc.gatewayErr}
			service := newPaymentTestService(repo, gateway)
			form := url.Values{
				"out_trade_no": {paymentTestOutTradeNo},
				"trade_status": {domainpayment.TradeStatusSuccess},
			}

			outcome, err := service.HandleNotify(context.Background(), form)
			requireServiceError(t, err, tc.wantCode)
			if outcome != nil {
				t.Fatalf("异常通知不应返回处理结果，实际 %+v", outcome)
			}
			if len(gateway.verifyCalls) != 1 || !reflect.DeepEqual(gateway.verifyCalls[0], form) {
				t.Fatalf("验签入参 = %+v，期望 %+v", gateway.verifyCalls, form)
			}
			if len(repo.outQueries) != 0 {
				t.Fatalf("异常通知不应触达仓储，实际调用 %d 次", len(repo.outQueries))
			}
			if len(repo.markCalls) != 0 {
				t.Fatalf("异常通知不应迁移状态，实际迁移 %d 次", len(repo.markCalls))
			}
		})
	}
}

// TestHandleNotifyOrderNotFound 覆盖订单不存在：返回 404 PAYMENT_NOT_FOUND；
// 通知路径没有用户令牌，仓储归属开关必须传 0（契约 §1.2、§6.7）。
func TestHandleNotifyOrderNotFound(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.findErr = domainpayment.ErrPaymentNotFound
	gateway := &fakeAlipayGateway{verifyPayload: paymentTestNotifyPayload()}
	service := newPaymentTestService(repo, gateway)

	outcome, err := service.HandleNotify(context.Background(), url.Values{})
	requireServiceError(t, err, CodePaymentNotFound)
	if outcome != nil {
		t.Fatalf("订单不存在不应返回处理结果，实际 %+v", outcome)
	}
	if len(repo.outQueries) != 1 || repo.outQueries[0] != (findByOutTradeNoCall{paymentTestOutTradeNo, 0}) {
		t.Fatalf("仓储入参 = %+v，期望 outTradeNo=%s ownerPatientID=0", repo.outQueries, paymentTestOutTradeNo)
	}
	if len(repo.markCalls) != 0 {
		t.Fatalf("订单不存在不应迁移状态，实际迁移 %d 次", len(repo.markCalls))
	}
}

// TestHandleNotifyAmountMismatch 覆盖金额校验：与订单金额不一致（含非法金额）返回
// 400 PAYMENT_AMOUNT_MISMATCH，且不得迁移状态（契约 §6.7 第 4 步）。
func TestHandleNotifyAmountMismatch(t *testing.T) {
	for _, notifyAmount := range []string{"80.01", "0.01", "abc", ""} {
		t.Run("total_amount="+notifyAmount, func(t *testing.T) {
			repo := newFakePaymentRepository()
			repo.item = paymentTestUnpaid()
			payload := paymentTestNotifyPayload()
			payload.TotalAmount = notifyAmount
			service := newPaymentTestService(repo, &fakeAlipayGateway{verifyPayload: payload})

			_, err := service.HandleNotify(context.Background(), url.Values{})
			serviceErr := requireServiceError(t, err, CodeAmountMismatch)
			if serviceErr.Message != "支付金额校验失败" {
				t.Fatalf("message = %q，期望 %q", serviceErr.Message, "支付金额校验失败")
			}
			if len(repo.markCalls) != 0 {
				t.Fatalf("金额不一致不应迁移状态，实际迁移 %d 次", len(repo.markCalls))
			}
		})
	}
}

// TestHandleNotifySuccessMarksPaid 覆盖主路径：UNPAID 且未超过 expire_at 的成功通知
// 迁移为 PAID 并写入支付宝交易号（契约 §6.7、§9）；
// 同时覆盖 "80.0" 与订单 "80.00" 的等价金额写法。
func TestHandleNotifySuccessMarksPaid(t *testing.T) {
	for _, notifyAmount := range []string{paymentTestAmount, "80.0"} {
		t.Run("total_amount="+notifyAmount, func(t *testing.T) {
			repo := newFakePaymentRepository()
			repo.item = paymentTestUnpaid()
			payload := paymentTestNotifyPayload()
			payload.TotalAmount = notifyAmount
			service := newPaymentTestService(repo, &fakeAlipayGateway{verifyPayload: payload})

			outcome, err := service.HandleNotify(context.Background(), url.Values{})
			if err != nil {
				t.Fatalf("成功通知不应报错，实际 %v", err)
			}
			if !outcome.Marked || outcome.Dropped {
				t.Fatalf("处理结果 = %+v，期望 Marked=true Dropped=false", outcome)
			}
			if len(repo.markCalls) != 1 || repo.markCalls[0] != (markPaidCall{paymentTestOutTradeNo, paymentTestTradeNo}) {
				t.Fatalf("迁移入参 = %+v，期望 outTradeNo=%s tradeNo=%s", repo.markCalls, paymentTestOutTradeNo, paymentTestTradeNo)
			}
		})
	}
}

// TestHandleNotifyFinalStatesAreDiscarded 覆盖幂等与终态：已 PAID 的重复通知不重复记账，
// EXPIRED/REFUNDED 的订单收到任何通知都丢弃，既不迁移也不写 transaction_id（契约 §6.7）。
func TestHandleNotifyFinalStatesAreDiscarded(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{name: "重复的成功通知（已 PAID）", status: domainpayment.PaymentStatusPaid},
		{name: "已 EXPIRED", status: domainpayment.PaymentStatusExpired},
		{name: "已 REFUNDED", status: domainpayment.PaymentStatusRefunded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := paymentTestUnpaid()
			item.PaymentStatus = tc.status
			item.TransactionID = paymentTestTradeNo
			repo := newFakePaymentRepository()
			repo.item = item
			service := newPaymentTestService(repo, &fakeAlipayGateway{verifyPayload: paymentTestNotifyPayload()})

			outcome, err := service.HandleNotify(context.Background(), url.Values{})
			if err != nil {
				t.Fatalf("终态通知必须静默丢弃并返回成功，实际报错 %v", err)
			}
			if !outcome.Dropped || outcome.Marked {
				t.Fatalf("处理结果 = %+v，期望 Dropped=true Marked=false", outcome)
			}
			if len(repo.markCalls) != 0 {
				t.Fatalf("终态通知不应迁移状态，实际迁移 %d 次", len(repo.markCalls))
			}
		})
	}
}

// TestHandleNotifyLateSuccessIsDiscarded 覆盖迟到通知：now() 已超过 expire_at 时
// 即使收到 TRADE_SUCCESS 也必须丢弃，不得补记 PAID（契约 §6.7、§6.8）。
func TestHandleNotifyLateSuccessIsDiscarded(t *testing.T) {
	item := paymentTestUnpaid()
	item.ExpireAt = paymentTestServiceNow.Add(-time.Second)
	repo := newFakePaymentRepository()
	repo.item = item
	service := newPaymentTestService(repo, &fakeAlipayGateway{verifyPayload: paymentTestNotifyPayload()})

	outcome, err := service.HandleNotify(context.Background(), url.Values{})
	if err != nil {
		t.Fatalf("迟到通知必须静默丢弃并返回成功，实际报错 %v", err)
	}
	if !outcome.Dropped || outcome.Marked {
		t.Fatalf("处理结果 = %+v，期望 Dropped=true Marked=false", outcome)
	}
	if len(repo.markCalls) != 0 {
		t.Fatalf("迟到通知不应迁移状态，实际迁移 %d 次", len(repo.markCalls))
	}
}

// TestHandleNotifyNonSuccessStatusKeepsUnpaid 覆盖非成功交易状态：
// WAIT_BUYER_PAY、TRADE_CLOSED 与未知状态都保持 UNPAID，不迁移也不置 EXPIRED（契约 §6.7、§6.8）。
func TestHandleNotifyNonSuccessStatusKeepsUnpaid(t *testing.T) {
	for _, tradeStatus := range []string{
		domainpayment.TradeStatusWaitBuyerPay,
		domainpayment.TradeStatusClosed,
		"TRADE_UNKNOWN",
		"",
	} {
		name := tradeStatus
		if name == "" {
			name = "空状态"
		}
		t.Run(name, func(t *testing.T) {
			repo := newFakePaymentRepository()
			repo.item = paymentTestUnpaid()
			payload := paymentTestNotifyPayload()
			payload.TradeStatus = tradeStatus
			service := newPaymentTestService(repo, &fakeAlipayGateway{verifyPayload: payload})

			outcome, err := service.HandleNotify(context.Background(), url.Values{})
			if err != nil {
				t.Fatalf("非成功状态必须返回成功，实际报错 %v", err)
			}
			if outcome.Marked || outcome.Dropped {
				t.Fatalf("处理结果 = %+v，期望 Marked=false Dropped=false（保持 UNPAID）", outcome)
			}
			if len(repo.markCalls) != 0 {
				t.Fatalf("非成功状态不应迁移，实际迁移 %d 次", len(repo.markCalls))
			}
		})
	}
}

// TestHandleNotifyConditionalUpdateRace 覆盖并发竞争：条件更新未生效时按幂等丢弃处理，
// 不报错也不覆盖其它路径写入的结果（契约 §6.7、§9）。
func TestHandleNotifyConditionalUpdateRace(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.item = paymentTestUnpaid()
	repo.markPaidResult = false
	service := newPaymentTestService(repo, &fakeAlipayGateway{verifyPayload: paymentTestNotifyPayload()})

	outcome, err := service.HandleNotify(context.Background(), url.Values{})
	if err != nil {
		t.Fatalf("条件更新未生效不应报错，实际 %v", err)
	}
	if !outcome.Dropped || outcome.Marked {
		t.Fatalf("处理结果 = %+v，期望 Dropped=true Marked=false", outcome)
	}
	if len(repo.markCalls) != 1 {
		t.Fatalf("迁移尝试次数 = %d，期望 1", len(repo.markCalls))
	}
}

// TestHandleNotifyWithoutGateway 未配置支付宝适配器时通知无法验签，
// 按 502 PAYMENT_PROVIDER_UNAVAILABLE 处理，不得伪造成 success。
func TestHandleNotifyWithoutGateway(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.item = paymentTestUnpaid()
	service := newPaymentTestService(repo, nil)

	_, err := service.HandleNotify(context.Background(), url.Values{})
	requireServiceError(t, err, CodeProviderUnavailable)
	if len(repo.markCalls) != 0 {
		t.Fatalf("未验签时不应迁移状态，实际迁移 %d 次", len(repo.markCalls))
	}
}
