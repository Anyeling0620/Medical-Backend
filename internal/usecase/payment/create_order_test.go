// 创建支付订单用例单测：Service.CreateOrder（spec/04-api-contract.md §6.9、§6.8、§9、§10）。
//
// 复用 service_test.go 的内存 fake 与固定时钟，重点覆盖：入参校验、订单定位失败、
// 终态订单冲突（已支付/已过期/已退款）、已有二维码的幂等复用、首次预下单、
// 支付窗口「只写缺失列」的补齐语义、已过 pay_deadline 时的只读收敛与预下单失败。
package payment

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainpayment "Medical-Web-Backend/internal/domain/payment"
	"Medical-Web-Backend/internal/port"
)

// windowFillingRepository 在共享 fake 之上模拟 EnsurePaymentWindow 的「只写缺失列」语义：
// 以 COALESCE(precreate_at, now()) 为基准补齐 pay_deadline（+30 分钟）与 expire_at（+35 分钟），
// 已有值原样保留；同时记录本次实际补齐的列，供用例断言写入范围。
type windowFillingRepository struct {
	*fakePaymentRepository
	writtenColumns []string
}

// 编译期确认它仍完整实现支付仓储端口契约。
var _ port.PaymentRepository = (*windowFillingRepository)(nil)

// EnsurePaymentWindow 只对空列写入，模拟 SQL 中 COALESCE 的语义。
func (r *windowFillingRepository) EnsurePaymentWindow(
	_ context.Context,
	outTradeNo string,
) (*domainpayment.Payment, error) {
	r.ensureCalls = append(r.ensureCalls, outTradeNo)
	if r.ensureErr != nil {
		return nil, r.ensureErr
	}
	if r.ensureItem != nil {
		return clonePayment(r.ensureItem), nil
	}
	item := clonePayment(r.item)
	if item == nil {
		return nil, domainpayment.ErrPaymentNotFound
	}
	if item.PrecreateAt.IsZero() {
		item.PrecreateAt = paymentTestServiceNow
		r.writtenColumns = append(r.writtenColumns, "precreateAt")
	}
	base := item.PrecreateAt
	if item.PayDeadline.IsZero() {
		item.PayDeadline = base.Add(30 * time.Minute)
		r.writtenColumns = append(r.writtenColumns, "payDeadline")
	}
	if item.ExpireAt.IsZero() {
		item.ExpireAt = base.Add(35 * time.Minute)
		r.writtenColumns = append(r.writtenColumns, "expireAt")
	}
	return item, nil
}

// TestCreateOrderRejectsInvalidRegistrationID 覆盖 registrationId 边界：
// 非正整数返回 422 REQUEST_VALIDATION_FAILED，且不触达仓储与支付宝。
func TestCreateOrderRejectsInvalidRegistrationID(t *testing.T) {
	for _, registrationID := range []int64{0, -1} {
		repo := newFakePaymentRepository()
		gateway := &fakeAlipayGateway{}
		service := newPaymentTestService(repo, gateway)

		result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
			RegistrationID: registrationID,
		})
		if message := requireServiceError(t, err, CodeValidationFailed).Message; message != "registrationId 必须为正整数" {
			t.Fatalf("registrationId=%d 的 message = %q", registrationID, message)
		}
		if result != nil {
			t.Fatalf("registrationId=%d 校验失败时不应返回结果，实际 %+v", registrationID, result)
		}
		if len(repo.regQueries) != 0 {
			t.Fatalf("registrationId=%d 不应触达仓储，实际调用 %d 次", registrationID, len(repo.regQueries))
		}
		if len(gateway.precreateCalls) != 0 {
			t.Fatalf("registrationId=%d 不应调用支付宝预下单，实际调用 %d 次", registrationID, len(gateway.precreateCalls))
		}
	}
}

// TestCreateOrderRejectsUnusablePatientActor 覆盖身份自相矛盾：声明为患者域却缺少
// 当前患者标识时返回 ErrInvalidActor（映射为 500），不得静默关闭归属条件
// （与 ReadPayable/Detail 同一取舍）。
func TestCreateOrderRejectsUnusablePatientActor(t *testing.T) {
	repo := newFakePaymentRepository()
	service := newPaymentTestService(repo, &fakeAlipayGateway{})

	_, err := service.CreateOrder(context.Background(), Actor{Realm: domainauth.RealmPatient}, CreateOrderInput{
		RegistrationID: paymentTestRegistrationID,
	})
	requireErrIs(t, err, ErrInvalidActor)
	if len(repo.regQueries) != 0 {
		t.Fatalf("身份不可用时不应触达仓储，实际调用 %d 次", len(repo.regQueries))
	}
}

// TestCreateOrderNotFound 覆盖订单不存在：返回 404 PAYMENT_NOT_FOUND；
// 本接口是管理端专用，必须按「不限定归属」查询（ownerPatientID=0）。
func TestCreateOrderNotFound(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.findErr = domainpayment.ErrPaymentNotFound
	service := newPaymentTestService(repo, &fakeAlipayGateway{})

	result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
		RegistrationID: paymentTestRegistrationID,
	})
	requireServiceError(t, err, CodePaymentNotFound)
	if result != nil {
		t.Fatalf("订单不存在时不应返回结果，实际 %+v", result)
	}
	if len(repo.regQueries) != 1 {
		t.Fatalf("仓储调用次数 = %d，期望 1", len(repo.regQueries))
	}
	if repo.regQueries[0] != (findByRegistrationCall{paymentTestRegistrationID, 0}) {
		t.Fatalf("仓储入参 = %+v，期望 registrationID=%d ownerPatientID=0", repo.regQueries[0], paymentTestRegistrationID)
	}
}

// TestCreateOrderRepositoryFailureIsDependencyUnavailable 覆盖仓储故障：
// 归类为 502 DEPENDENCY_UNAVAILABLE，不得伪装成订单不存在。
func TestCreateOrderRepositoryFailureIsDependencyUnavailable(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.findErr = errors.New("postgres 连接失败")
	service := newPaymentTestService(repo, &fakeAlipayGateway{})

	_, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
		RegistrationID: paymentTestRegistrationID,
	})
	requireErrIs(t, err, ErrDependencyUnavailable)
	if errors.Is(err, domainpayment.ErrPaymentNotFound) {
		t.Fatal("依赖故障不得被判定为订单不存在")
	}
}

// TestCreateOrderTerminalStatesAreRejected 覆盖终态冲突（契约 §6.8、§9、§10）：
// PAID 返回 PAYMENT_ALREADY_PAID，EXPIRED/REFUNDED 返回 PAYMENT_INVALID_TRANSITION，
// 两者都不得补窗口、不得预下单、不得回写二维码。
func TestCreateOrderTerminalStatesAreRejected(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		wantCode string
	}{
		{name: "已支付", status: domainpayment.PaymentStatusPaid, wantCode: CodePaymentAlreadyPaid},
		{name: "已过期", status: domainpayment.PaymentStatusExpired, wantCode: CodePaymentInvalidTransition},
		{name: "已退款", status: domainpayment.PaymentStatusRefunded, wantCode: CodePaymentInvalidTransition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := paymentTestUnpaid()
			item.PaymentStatus = tc.status
			item.TransactionID = paymentTestTradeNo
			repo := newFakePaymentRepository()
			repo.item = item
			gateway := &fakeAlipayGateway{
				precreateResult: &domainpayment.PrecreateResult{QRCode: "https://qr.alipay.com/bax-should-not-happen"},
			}
			service := newPaymentTestService(repo, gateway)

			result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
				RegistrationID: paymentTestRegistrationID,
			})
			requireServiceError(t, err, tc.wantCode)
			if result != nil {
				t.Fatalf("终态冲突时不应返回结果，实际 %+v", result)
			}
			if len(repo.ensureCalls) != 0 {
				t.Fatalf("终态订单不得补支付窗口，实际调用 %d 次", len(repo.ensureCalls))
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

// TestCreateOrderIdempotentReusesExistingPrepayID 覆盖幂等复用：
// 已有 prepay_id 且在支付窗口内时直接返回同一条订单与同一个二维码，Created=false，
// 不补窗口也不预下单（对应 HTTP 200，契约 §6.9）。
func TestCreateOrderIdempotentReusesExistingPrepayID(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.item = paymentTestUnpaid()
	gateway := &fakeAlipayGateway{
		precreateResult: &domainpayment.PrecreateResult{QRCode: "https://qr.alipay.com/bax-should-not-happen"},
	}
	service := newPaymentTestService(repo, gateway)

	result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
		RegistrationID: paymentTestRegistrationID,
	})
	if err != nil {
		t.Fatalf("幂等复用不应报错，实际 %v", err)
	}
	if result.Created {
		t.Fatal("已有二维码时 Created 应为 false（对应 HTTP 200 幂等语义）")
	}
	if !result.Payable {
		t.Fatal("窗口内已有二维码时 payable 应为 true")
	}
	if result.Payment.PrepayID != paymentTestQRCode {
		t.Fatalf("返回的 prepay_id = %q，期望 %q", result.Payment.PrepayID, paymentTestQRCode)
	}
	if len(repo.ensureCalls) != 0 {
		t.Fatalf("窗口与二维码齐全时不应补窗口，实际调用 %d 次", len(repo.ensureCalls))
	}
	if len(gateway.precreateCalls) != 0 {
		t.Fatalf("已有二维码时不得重复预下单，实际调用 %d 次", len(gateway.precreateCalls))
	}
	if len(repo.saveCalls) != 0 {
		t.Fatalf("已有二维码时不得回写，实际回写 %d 次", len(repo.saveCalls))
	}
	// isNew=false（本次没有写入二维码）时不得触发「预下单后重读最新状态」：
	// 这次重读只服务于本次真的写入二维码的场景，避免并发重放多一次数据库往返。
	if len(repo.outQueries) != 0 {
		t.Fatalf("未新建交易时不应按交易号重读，实际调用 %+v", repo.outQueries)
	}
}

// TestCreateOrderFirstCreatePrecreatesOnce 覆盖首次创建：prepay_id 为空时调用一次
// alipay.trade.precreate，回写二维码并返回 Created=true（对应 HTTP 201）。
func TestCreateOrderFirstCreatePrecreatesOnce(t *testing.T) {
	const newQRCode = "https://qr.alipay.com/bax-create-order"
	item := paymentTestUnpaid()
	item.PrepayID = ""

	repo := newFakePaymentRepository()
	repo.item = item
	gateway := &fakeAlipayGateway{
		precreateResult: &domainpayment.PrecreateResult{QRCode: newQRCode},
	}
	service := newPaymentTestService(repo, gateway)

	result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
		RegistrationID: paymentTestRegistrationID,
	})
	if err != nil {
		t.Fatalf("首次创建不应报错，实际 %v", err)
	}
	if !result.Created {
		t.Fatal("首次完成预下单时 Created 应为 true（对应 HTTP 201 语义）")
	}
	if !result.Payable {
		t.Fatal("窗口内预下单成功时 payable 应为 true")
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
	if result.Payment.PrepayID != newQRCode {
		t.Fatalf("返回的 prepay_id = %q，期望 %q", result.Payment.PrepayID, newQRCode)
	}
	if result.Payment.PaymentStatus != domainpayment.PaymentStatusUnpaid {
		t.Fatalf("paymentStatus = %q，期望 %q", result.Payment.PaymentStatus, domainpayment.PaymentStatusUnpaid)
	}
}

// TestCreateOrderFillsOnlyMissingWindowColumns 覆盖支付窗口补齐（契约 §6.8）：
// 三个时间点为空时调用 EnsurePaymentWindow 一次，且只写缺失列；
// 已有时间点不被覆盖，+30/+35 分钟的固定关系以已有 precreate_at 为基准保持不变。
func TestCreateOrderFillsOnlyMissingWindowColumns(t *testing.T) {
	t.Run("三个时间点全空：补齐三列且不重复预下单", func(t *testing.T) {
		item := paymentTestUnpaid()
		item.PrecreateAt = time.Time{}
		item.PayDeadline = time.Time{}
		item.ExpireAt = time.Time{}

		repo := &windowFillingRepository{fakePaymentRepository: newFakePaymentRepository()}
		repo.item = item
		gateway := &fakeAlipayGateway{
			precreateResult: &domainpayment.PrecreateResult{QRCode: "https://qr.alipay.com/bax-should-not-happen"},
		}
		service := newPaymentTestService(repo, gateway)

		result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
			RegistrationID: paymentTestRegistrationID,
		})
		if err != nil {
			t.Fatalf("补齐支付窗口不应报错，实际 %v", err)
		}
		if len(repo.ensureCalls) != 1 || repo.ensureCalls[0] != paymentTestOutTradeNo {
			t.Fatalf("补齐支付窗口调用 = %v，期望 [%s]", repo.ensureCalls, paymentTestOutTradeNo)
		}
		wantColumns := []string{"precreateAt", "payDeadline", "expireAt"}
		if !reflect.DeepEqual(repo.writtenColumns, wantColumns) {
			t.Fatalf("补齐写入的列 = %v，期望 %v", repo.writtenColumns, wantColumns)
		}
		if result.Payment.PrecreateAt != paymentTestServiceNow {
			t.Fatalf("precreateAt = %s，期望 %s", result.Payment.PrecreateAt, paymentTestServiceNow)
		}
		if want := paymentTestServiceNow.Add(30 * time.Minute); result.Payment.PayDeadline != want {
			t.Fatalf("payDeadline = %s，期望 %s", result.Payment.PayDeadline, want)
		}
		if want := paymentTestServiceNow.Add(35 * time.Minute); result.Payment.ExpireAt != want {
			t.Fatalf("expireAt = %s，期望 %s", result.Payment.ExpireAt, want)
		}
		if result.Created {
			t.Fatal("二维码已存在时补齐窗口不应判定为新建")
		}
		if len(gateway.precreateCalls) != 0 {
			t.Fatalf("已有二维码时不得重复预下单，实际调用 %d 次", len(gateway.precreateCalls))
		}
	})

	t.Run("已有 precreate_at：只补两个缺失列且不覆盖已有值", func(t *testing.T) {
		existingPrecreateAt := paymentTestServiceNow.Add(-5 * time.Minute)
		item := paymentTestUnpaid()
		item.PrecreateAt = existingPrecreateAt
		item.PayDeadline = time.Time{}
		item.ExpireAt = time.Time{}

		repo := &windowFillingRepository{fakePaymentRepository: newFakePaymentRepository()}
		repo.item = item
		service := newPaymentTestService(repo, &fakeAlipayGateway{})

		result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
			RegistrationID: paymentTestRegistrationID,
		})
		if err != nil {
			t.Fatalf("补齐支付窗口不应报错，实际 %v", err)
		}
		wantColumns := []string{"payDeadline", "expireAt"}
		if !reflect.DeepEqual(repo.writtenColumns, wantColumns) {
			t.Fatalf("补齐写入的列 = %v，期望 %v", repo.writtenColumns, wantColumns)
		}
		if result.Payment.PrecreateAt != existingPrecreateAt {
			t.Fatalf("已有 precreateAt = %s 被覆盖为 %s", existingPrecreateAt, result.Payment.PrecreateAt)
		}
		if want := existingPrecreateAt.Add(30 * time.Minute); result.Payment.PayDeadline != want {
			t.Fatalf("payDeadline = %s，期望 %s（以已有 precreate_at 为基准 +30 分钟）", result.Payment.PayDeadline, want)
		}
		if want := existingPrecreateAt.Add(35 * time.Minute); result.Payment.ExpireAt != want {
			t.Fatalf("expireAt = %s，期望 %s（以已有 precreate_at 为基准 +35 分钟）", result.Payment.ExpireAt, want)
		}
	})

	t.Run("三个时间点齐全：不调用补齐", func(t *testing.T) {
		repo := &windowFillingRepository{fakePaymentRepository: newFakePaymentRepository()}
		repo.item = paymentTestUnpaid()
		service := newPaymentTestService(repo, &fakeAlipayGateway{})

		if _, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
			RegistrationID: paymentTestRegistrationID,
		}); err != nil {
			t.Fatalf("读取已有窗口不应报错，实际 %v", err)
		}
		if len(repo.ensureCalls) != 0 {
			t.Fatalf("三个时间点齐全时不应补窗口，实际调用 %d 次", len(repo.ensureCalls))
		}
		if len(repo.writtenColumns) != 0 {
			t.Fatalf("三个时间点齐全时不应写入任何列，实际 %v", repo.writtenColumns)
		}
	})
}

// TestCreateOrderPastPayDeadlineSkipsPrecreate 覆盖已过 pay_deadline 但未过 expire_at：
// 不再新建支付宝交易，只按现状回读（对应 HTTP 200、payable=false、不返回二维码）。
func TestCreateOrderPastPayDeadlineSkipsPrecreate(t *testing.T) {
	t.Run("已有二维码：只回读", func(t *testing.T) {
		item := paymentTestUnpaid()
		item.PayDeadline = paymentTestServiceNow.Add(-time.Minute)
		item.ExpireAt = paymentTestServiceNow.Add(4 * time.Minute)

		repo := newFakePaymentRepository()
		repo.item = item
		gateway := &fakeAlipayGateway{
			precreateResult: &domainpayment.PrecreateResult{QRCode: "https://qr.alipay.com/bax-should-not-happen"},
		}
		service := newPaymentTestService(repo, gateway)

		result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
			RegistrationID: paymentTestRegistrationID,
		})
		if err != nil {
			t.Fatalf("已过支付截止的订单应可回读，实际 %v", err)
		}
		if result.Created || result.Payable {
			t.Fatalf("结果 = %+v，期望 Created=false Payable=false", result)
		}
		if len(repo.ensureCalls) != 0 {
			t.Fatalf("窗口与二维码齐全时不应补窗口，实际调用 %d 次", len(repo.ensureCalls))
		}
		if len(gateway.precreateCalls) != 0 {
			t.Fatalf("已过支付截止不得新建支付宝交易，实际调用 %d 次", len(gateway.precreateCalls))
		}
	})

	t.Run("二维码为空：补窗口但不预下单", func(t *testing.T) {
		item := paymentTestUnpaid()
		item.PrepayID = ""
		item.PayDeadline = paymentTestServiceNow.Add(-time.Minute)
		item.ExpireAt = paymentTestServiceNow.Add(4 * time.Minute)

		repo := newFakePaymentRepository()
		repo.item = item
		gateway := &fakeAlipayGateway{
			precreateResult: &domainpayment.PrecreateResult{QRCode: "https://qr.alipay.com/bax-should-not-happen"},
		}
		service := newPaymentTestService(repo, gateway)

		result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
			RegistrationID: paymentTestRegistrationID,
		})
		if err != nil {
			t.Fatalf("已过支付截止的订单应可回读，实际 %v", err)
		}
		if len(repo.ensureCalls) != 1 || repo.ensureCalls[0] != paymentTestOutTradeNo {
			t.Fatalf("补齐支付窗口调用 = %v，期望 [%s]", repo.ensureCalls, paymentTestOutTradeNo)
		}
		if result.Created || result.Payable {
			t.Fatalf("结果 = %+v，期望 Created=false Payable=false", result)
		}
		if result.Payment.PrepayID != "" {
			t.Fatalf("已过支付截止不得产生新二维码，实际 %q", result.Payment.PrepayID)
		}
		if len(gateway.precreateCalls) != 0 {
			t.Fatalf("已过支付截止不得预下单，实际调用 %d 次", len(gateway.precreateCalls))
		}
		if len(repo.saveCalls) != 0 {
			t.Fatalf("已过支付截止不得回写二维码，实际回写 %d 次", len(repo.saveCalls))
		}
	})
}

// TestCreateOrderProviderUnavailable 覆盖预下单失败（契约 §6.2、§10）：
// provider 不可用映射为 502 PAYMENT_PROVIDER_UNAVAILABLE，且绝不回写/返回二维码。
func TestCreateOrderProviderUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		gateway error
	}{
		{name: "支付宝不可用", gateway: port.ErrAlipayUnavailable},
		{name: "交易已关闭不可重试", gateway: port.ErrAlipayTradeClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := paymentTestUnpaid()
			item.PrepayID = ""

			repo := newFakePaymentRepository()
			repo.item = item
			gateway := &fakeAlipayGateway{precreateErr: tc.gateway}
			service := newPaymentTestService(repo, gateway)

			result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
				RegistrationID: paymentTestRegistrationID,
			})
			requireServiceError(t, err, CodeProviderUnavailable)
			if result != nil {
				t.Fatalf("预下单失败不应返回结果（二维码必须缺失），实际 %+v", result)
			}
			if len(gateway.precreateCalls) != 1 {
				t.Fatalf("预下单调用次数 = %d，期望 1", len(gateway.precreateCalls))
			}
			if len(repo.saveCalls) != 0 {
				t.Fatalf("预下单失败不得回写二维码，实际回写 %d 次", len(repo.saveCalls))
			}
		})
	}
}

// TestCreateOrderConcurrentReplayReusesStoredQRCode 覆盖并发重放（契约 §6.9）：
// 预下单期间另一请求已先行写入二维码时，SavePrepayID 返回 false，用例必须回读库中
// 已有二维码并返回 Created=false（对应 HTTP 200、无 Location），不得把本次生成的
// 二维码当作新建结果返回；整个流程只预下单一次、只回写一次。
func TestCreateOrderConcurrentReplayReusesStoredQRCode(t *testing.T) {
	const providerQRCode = "https://qr.alipay.com/bax-loser-provider"
	const storedQRCode = "https://qr.alipay.com/bax-winner-stored"

	item := paymentTestUnpaid()
	item.PrepayID = "" // 无二维码，触发预下单分支

	repo := newFakePaymentRepository()
	repo.item = item
	// 模拟并发竞争失败方：回写被判定为「二维码已存在」，按交易号回读得到既有二维码。
	repo.saveResult = false
	repo.outTradeNoItem = paymentTestUnpaid()
	repo.outTradeNoItem.PrepayID = storedQRCode
	// 第二次按交易号读取会返回这条终态订单：若 CreateOrder 在 isNew=false 时误加
	// 「重读最新状态」，结果会从 200 变成 409 PAYMENT_ALREADY_PAID，用例立刻失败；
	// 正确的实现只会有 precreate 内部的一次回读（见下面的 outQueries 断言）。
	poisoned := paymentTestUnpaid()
	poisoned.PaymentStatus = domainpayment.PaymentStatusPaid
	poisoned.TransactionID = paymentTestTradeNo
	repo.latestItem = poisoned

	gateway := &fakeAlipayGateway{
		precreateResult: &domainpayment.PrecreateResult{QRCode: providerQRCode},
	}
	service := newPaymentTestService(repo, gateway)

	result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
		RegistrationID: paymentTestRegistrationID,
	})
	if err != nil {
		t.Fatalf("并发重放不应报错，实际 %v", err)
	}
	if result.Created {
		t.Fatal("并发重放未真正写入二维码时 Created 应为 false（对应 HTTP 200 而非 201）")
	}
	if result.Payment.PrepayID != storedQRCode {
		t.Fatalf("返回的 prepay_id = %q，期望回读到的既有二维码 %q", result.Payment.PrepayID, storedQRCode)
	}
	if !result.Payable {
		t.Fatal("窗口内并发重放返回的订单应为可支付")
	}
	if len(gateway.precreateCalls) != 1 {
		t.Fatalf("预下单调用次数 = %d，期望 1（不得重复预下单）", len(gateway.precreateCalls))
	}
	if len(repo.saveCalls) != 1 || repo.saveCalls[0] != (savePrepayIDCall{paymentTestOutTradeNo, providerQRCode}) {
		t.Fatalf("二维码回写调用 = %+v，期望仅一次 {%s %s}", repo.saveCalls, paymentTestOutTradeNo, providerQRCode)
	}
	if len(repo.ensureCalls) != 1 || repo.ensureCalls[0] != paymentTestOutTradeNo {
		t.Fatalf("补齐支付窗口调用 = %v，期望仅一次 [%s]", repo.ensureCalls, paymentTestOutTradeNo)
	}
	if len(repo.outQueries) != 1 || repo.outQueries[0] != (findByOutTradeNoCall{paymentTestOutTradeNo, 0}) {
		t.Fatalf("按交易号读取入参 = %+v，期望仅一次 {%s 0}（precreate 内部回读，isNew=false 不得再重读）",
			repo.outQueries, paymentTestOutTradeNo)
	}
}

// TestCreateOrderFinalStateRecheckAfterEnsureWindow 覆盖补齐窗口期间的 TOCTOU 复核（契约 §6.9）：
// 初次读取仍是非终态订单，EnsurePaymentWindow 返回时订单已被异步通知改成终态，
// 此时必须按终态映射 409，而不是把已支付、已结束的订单当成创建成功或幂等重放返回。
func TestCreateOrderFinalStateRecheckAfterEnsureWindow(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		wantCode string
	}{
		{name: "补齐期间被标记为已支付", status: domainpayment.PaymentStatusPaid, wantCode: CodePaymentAlreadyPaid},
		{name: "补齐期间被标记为已过期", status: domainpayment.PaymentStatusExpired, wantCode: CodePaymentInvalidTransition},
		{name: "补齐期间被标记为已退款", status: domainpayment.PaymentStatusRefunded, wantCode: CodePaymentInvalidTransition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 初始订单非终态且缺少二维码：确保用例真正走到 EnsurePaymentWindow 之后的分支。
			item := paymentTestUnpaid()
			item.PrepayID = ""

			ensured := paymentTestUnpaid()
			ensured.PaymentStatus = tc.status
			ensured.TransactionID = paymentTestTradeNo

			repo := newFakePaymentRepository()
			repo.item = item
			repo.ensureItem = ensured
			gateway := &fakeAlipayGateway{
				precreateResult: &domainpayment.PrecreateResult{QRCode: "https://qr.alipay.com/bax-should-not-happen"},
			}
			service := newPaymentTestService(repo, gateway)

			result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
				RegistrationID: paymentTestRegistrationID,
			})
			requireServiceError(t, err, tc.wantCode)
			if result != nil {
				t.Fatalf("终态复核失败时不应返回结果，实际 %+v", result)
			}
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

// TestCreateOrderPaidDuringPrecreate 覆盖「预下单期间被支付」的 TOCTOU 复核（契约 §6.9）：
// 初始订单 UNPAID 且无二维码（触发 EnsurePaymentWindow + precreate），本次确实完成了
// 预下单与二维码回写（isNew=true），但按 out_trade_no 重读发现订单已被异步通知改成 PAID。
// 此时必须返回 409 PAYMENT_ALREADY_PAID 且不返回任何结果，绝不能把刚生成的二维码
// 当成 201 交付给客户端。
func TestCreateOrderPaidDuringPrecreate(t *testing.T) {
	const newQRCode = "https://qr.alipay.com/bax-paid-during-precreate"

	// 初始订单 UNPAID 且无二维码：确保用例真正走到预下单分支。
	item := paymentTestUnpaid()
	item.PrepayID = ""

	// 重读返回的最新状态：预下单期间已被支付。
	latest := paymentTestUnpaid()
	latest.PaymentStatus = domainpayment.PaymentStatusPaid
	latest.TransactionID = paymentTestTradeNo
	latest.PrepayID = newQRCode

	repo := newFakePaymentRepository()
	repo.item = item
	repo.outTradeNoItem = latest

	gateway := &fakeAlipayGateway{
		precreateResult: &domainpayment.PrecreateResult{QRCode: newQRCode},
	}
	service := newPaymentTestService(repo, gateway)

	result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
		RegistrationID: paymentTestRegistrationID,
	})
	requireServiceError(t, err, CodePaymentAlreadyPaid)
	if result != nil {
		t.Fatalf("预下单期间被支付时不得返回结果（二维码不得交付客户端），实际 %+v", result)
	}
	// 本次确实完成了预下单与回写，说明 isNew=true，走的正是「重读最新状态」这条路径。
	if len(gateway.precreateCalls) != 1 {
		t.Fatalf("预下单调用次数 = %d，期望 1", len(gateway.precreateCalls))
	}
	if len(repo.saveCalls) != 1 || repo.saveCalls[0] != (savePrepayIDCall{paymentTestOutTradeNo, newQRCode}) {
		t.Fatalf("二维码回写调用 = %+v，期望仅一次 {%s %s}", repo.saveCalls, paymentTestOutTradeNo, newQRCode)
	}
	// 必须按 out_trade_no 重读最新状态，且管理端不限定归属（ownerPatientID=0）。
	if len(repo.outQueries) != 1 || repo.outQueries[0] != (findByOutTradeNoCall{paymentTestOutTradeNo, 0}) {
		t.Fatalf("按交易号重读入参 = %+v，期望一次 {%s 0}", repo.outQueries, paymentTestOutTradeNo)
	}
}

// TestCreateOrderEndedDuringPrecreate 覆盖同一重读路径下的另外两种终态（契约 §6.9）：
// 预下单期间订单被置为 EXPIRED 或 REFUNDED 时，都必须返回 409 PAYMENT_INVALID_TRANSITION，
// 不返回结果、不迁移状态，也绝不把二维码交给客户端。
//
// 两种状态都单独覆盖：虽然都映射到同一个错误码，但它们是两个独立的数据状态，
// 后续若有人只为其中一种加分支（例如只处理 REFUNDED），本用例能分别定位到。
func TestCreateOrderEndedDuringPrecreate(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{name: "预下单期间被标记为已过期", status: domainpayment.PaymentStatusExpired},
		{name: "预下单期间被标记为已退款", status: domainpayment.PaymentStatusRefunded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const newQRCode = "https://qr.alipay.com/bax-ended-during-precreate"

			item := paymentTestUnpaid()
			item.PrepayID = ""

			latest := paymentTestUnpaid()
			latest.PaymentStatus = tc.status
			latest.TransactionID = paymentTestTradeNo
			latest.PrepayID = newQRCode

			repo := newFakePaymentRepository()
			repo.item = item
			repo.outTradeNoItem = latest

			gateway := &fakeAlipayGateway{
				precreateResult: &domainpayment.PrecreateResult{QRCode: newQRCode},
			}
			service := newPaymentTestService(repo, gateway)

			result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
				RegistrationID: paymentTestRegistrationID,
			})
			requireServiceError(t, err, CodePaymentInvalidTransition)
			if result != nil {
				t.Fatalf("终态订单不得返回结果，实际 %+v", result)
			}
			if len(repo.markCalls) != 0 {
				t.Fatalf("终态复核失败不得迁移状态，实际迁移 %d 次", len(repo.markCalls))
			}
			if len(gateway.precreateCalls) != 1 || len(repo.saveCalls) != 1 {
				t.Fatalf("预下单/回写次数 = %d/%d，期望各 1（必须先走完预下单再重读判定）",
					len(gateway.precreateCalls), len(repo.saveCalls))
			}
			if len(repo.outQueries) != 1 || repo.outQueries[0] != (findByOutTradeNoCall{paymentTestOutTradeNo, 0}) {
				t.Fatalf("按交易号重读入参 = %+v，期望一次 {%s 0}", repo.outQueries, paymentTestOutTradeNo)
			}
		})
	}
}

// TestCreateOrderPrecreateRecheckErrors 覆盖预下单后重读最新状态的失败分支（契约 §6.9、§10）：
// 重读时订单已不存在 → 404 PAYMENT_NOT_FOUND；重读报错 → 502 DEPENDENCY_UNAVAILABLE
// （用例层以 ErrDependencyUnavailable 表达，由 handler 映射为 502）。
// 两种失败都不得把二维码交付客户端，也不得继续当成创建成功。
func TestCreateOrderPrecreateRecheckErrors(t *testing.T) {
	newFixture := func(t *testing.T, outTradeNoErr error) (*fakePaymentRepository, *fakeAlipayGateway, *Service) {
		t.Helper()
		const newQRCode = "https://qr.alipay.com/bax-recheck-error"

		item := paymentTestUnpaid()
		item.PrepayID = ""

		repo := newFakePaymentRepository()
		repo.item = item
		repo.outTradeNoErr = outTradeNoErr
		gateway := &fakeAlipayGateway{
			precreateResult: &domainpayment.PrecreateResult{QRCode: newQRCode},
		}
		return repo, gateway, newPaymentTestService(repo, gateway)
	}

	t.Run("重读返回订单不存在：404", func(t *testing.T) {
		repo, gateway, service := newFixture(t, domainpayment.ErrPaymentNotFound)

		result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
			RegistrationID: paymentTestRegistrationID,
		})
		requireServiceError(t, err, CodePaymentNotFound)
		if result != nil {
			t.Fatalf("重读失败时不得返回结果，实际 %+v", result)
		}
		if len(gateway.precreateCalls) != 1 || len(repo.saveCalls) != 1 {
			t.Fatalf("预下单/回写次数 = %d/%d，期望各 1（必须先完成预下单再重读）",
				len(gateway.precreateCalls), len(repo.saveCalls))
		}
		if len(repo.outQueries) != 1 || repo.outQueries[0] != (findByOutTradeNoCall{paymentTestOutTradeNo, 0}) {
			t.Fatalf("按交易号重读入参 = %+v，期望一次 {%s 0}", repo.outQueries, paymentTestOutTradeNo)
		}
	})

	t.Run("重读报错：502 依赖不可用", func(t *testing.T) {
		repo, gateway, service := newFixture(t, errors.New("postgres 连接失败"))

		result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
			RegistrationID: paymentTestRegistrationID,
		})
		requireErrIs(t, err, ErrDependencyUnavailable)
		if errors.Is(err, domainpayment.ErrPaymentNotFound) {
			t.Fatal("依赖故障不得被判定为订单不存在")
		}
		if result != nil {
			t.Fatalf("重读失败时不得返回结果，实际 %+v", result)
		}
		if len(gateway.precreateCalls) != 1 || len(repo.saveCalls) != 1 {
			t.Fatalf("预下单/回写次数 = %d/%d，期望各 1（必须先完成预下单再重读）",
				len(gateway.precreateCalls), len(repo.saveCalls))
		}
	})
}

// TestCreateOrderPrepayIDWriteRaceFinalState 覆盖「预下单二维码回写抢输」后按交易号回读到终态行的分支
// （契约 §6.9、§9、§10）：SavePrepayID 返回 saved=false 时 precreate 会回读整行，
// 若竞争方已把订单改成 PAID，CreateOrder 必须返回 409 PAYMENT_ALREADY_PAID；
// 改成 EXPIRED 时必须返回 409 PAYMENT_INVALID_TRANSITION。
//
// 两种变体共同断言：结果必须为 nil（不得是 200/201 成功语义）、错误信息不得携带回读到的二维码，
// 且不得产生额外写入（补齐支付窗口 1 次、预下单 1 次、回写 1 次、状态迁移 0 次）。
// 与 TestCreateOrderEndedDuringPrecreate 的区别：那条路径回写成功（isNew=true）后由 CreateOrder
// 主动重读最新状态；本用例是回写抢输（isNew=false），终态行由 precreate 内部回读后直接返回，
// 因此按交易号读取恰好 1 次，不应再触发第二次重读。
func TestCreateOrderPrepayIDWriteRaceFinalState(t *testing.T) {
	cases := []struct {
		name   string
		status string
		code   string
	}{
		{name: "回写抢输且库中订单已支付", status: domainpayment.PaymentStatusPaid, code: CodePaymentAlreadyPaid},
		{name: "回写抢输且库中订单已过期", status: domainpayment.PaymentStatusExpired, code: CodePaymentInvalidTransition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const racedQRCode = "https://qr.alipay.com/bax-write-race-final"

			// 初始订单为非终态（UNPAID）且无二维码：确保走到 EnsurePaymentWindow + precreate。
			item := paymentTestUnpaid()
			item.PrepayID = ""

			// 竞争方已把订单改成终态并写入自己的二维码，SavePrepayID 抢输后按交易号回读到的就是这一行。
			stored := paymentTestUnpaid()
			stored.PaymentStatus = tc.status
			stored.PrepayID = racedQRCode

			repo := newFakePaymentRepository()
			repo.item = item
			repo.outTradeNoItem = stored
			repo.saveResult = false

			gateway := &fakeAlipayGateway{
				precreateResult: &domainpayment.PrecreateResult{QRCode: racedQRCode},
			}
			service := newPaymentTestService(repo, gateway)

			result, err := service.CreateOrder(context.Background(), paymentTestMisActor(), CreateOrderInput{
				RegistrationID: paymentTestRegistrationID,
			})

			serviceErr := requireServiceError(t, err, tc.code)
			// 不是成功路径：既没有 200/201 的结果，也不把回读到的二维码交付客户端。
			if result != nil {
				t.Fatalf("终态订单不得返回结果（不得是 200/201 语义），实际 %+v", result)
			}
			if strings.Contains(serviceErr.Message, racedQRCode) {
				t.Fatalf("错误信息不得携带二维码，实际 message = %q", serviceErr.Message)
			}
			if len(repo.ensureCalls) != 1 || repo.ensureCalls[0] != paymentTestOutTradeNo {
				t.Fatalf("补齐支付窗口调用 = %+v，期望恰 1 次 %s", repo.ensureCalls, paymentTestOutTradeNo)
			}
			if len(gateway.precreateCalls) != 1 {
				t.Fatalf("预下单调用次数 = %d，期望恰 1 次", len(gateway.precreateCalls))
			}
			if len(repo.saveCalls) != 1 || repo.saveCalls[0] != (savePrepayIDCall{paymentTestOutTradeNo, racedQRCode}) {
				t.Fatalf("二维码回写调用 = %+v，期望恰 1 次 {%s %s}", repo.saveCalls, paymentTestOutTradeNo, racedQRCode)
			}
			if len(repo.outQueries) != 1 || repo.outQueries[0] != (findByOutTradeNoCall{paymentTestOutTradeNo, 0}) {
				t.Fatalf("按交易号回读调用 = %+v，期望恰 1 次 {%s 0}（回写抢输时不应二次重读）",
					repo.outQueries, paymentTestOutTradeNo)
			}
			if len(repo.markCalls) != 0 {
				t.Fatalf("终态订单不得迁移状态，实际迁移 %d 次", len(repo.markCalls))
			}
		})
	}
}

// ---- PrecreateForOrder：建单流程在同一个 HTTP 请求内完成预下单（契约 §6.2） ----

// TestPrecreateForOrderSuccess 首次预下单：补齐支付窗口、调用一次支付宝、回写二维码并返回
// 可用的 Payment（二维码非空）；读取按挂号编号进行，不做患者归属限定（服务内部调用）。
func TestPrecreateForOrderSuccess(t *testing.T) {
	item := paymentTestUnpaid()
	item.PrepayID = ""

	repo := newFakePaymentRepository()
	repo.item = item
	gateway := &fakeAlipayGateway{
		precreateResult: &domainpayment.PrecreateResult{QRCode: paymentTestQRCode},
	}
	service := newPaymentTestService(repo, gateway)

	pay, err := service.PrecreateForOrder(context.Background(), paymentTestRegistrationID)
	if err != nil {
		t.Fatalf("首次预下单不应报错，实际 %v", err)
	}
	if pay == nil || pay.PrepayID != paymentTestQRCode {
		t.Fatalf("返回的 payment.prepayId = %v，期望 %q", pay, paymentTestQRCode)
	}
	if pay.PaymentStatus != domainpayment.PaymentStatusUnpaid {
		t.Errorf("paymentStatus = %q，期望 UNPAID", pay.PaymentStatus)
	}
	if len(repo.ensureCalls) != 1 || repo.ensureCalls[0] != paymentTestOutTradeNo {
		t.Errorf("补齐支付窗口调用 = %v，期望恰 1 次 %s", repo.ensureCalls, paymentTestOutTradeNo)
	}
	if len(gateway.precreateCalls) != 1 {
		t.Errorf("预下单调用 %d 次，期望 1", len(gateway.precreateCalls))
	}
	if len(repo.saveCalls) != 1 || repo.saveCalls[0] != (savePrepayIDCall{paymentTestOutTradeNo, paymentTestQRCode}) {
		t.Errorf("二维码回写 = %+v，期望恰 1 次 {%s %s}", repo.saveCalls, paymentTestOutTradeNo, paymentTestQRCode)
	}
	if len(repo.regQueries) != 1 || repo.regQueries[0] != (findByRegistrationCall{paymentTestRegistrationID, 0}) {
		t.Errorf("按挂号编号读取 = %+v，期望恰 1 次 {%d 0}", repo.regQueries, paymentTestRegistrationID)
	}
}

// TestPrecreateForOrderReusesExistingQRCode 已有二维码时不重复预下单：直接回读库中二维码，
// 既不补窗口也不调用 provider（契约 §6.9 的幂等语义在建单路径同样成立）。
func TestPrecreateForOrderReusesExistingQRCode(t *testing.T) {
	repo := newFakePaymentRepository()
	repo.item = paymentTestUnpaid()
	gateway := &fakeAlipayGateway{
		precreateResult: &domainpayment.PrecreateResult{QRCode: "https://qr.alipay.com/bax-should-not-happen"},
	}
	service := newPaymentTestService(repo, gateway)

	pay, err := service.PrecreateForOrder(context.Background(), paymentTestRegistrationID)
	if err != nil {
		t.Fatalf("已有二维码不应报错，实际 %v", err)
	}
	if pay == nil || pay.PrepayID != paymentTestQRCode {
		t.Fatalf("返回的 payment.prepayId = %v，期望库中已有 %q", pay, paymentTestQRCode)
	}
	if len(repo.ensureCalls) != 0 || len(gateway.precreateCalls) != 0 || len(repo.saveCalls) != 0 {
		t.Errorf("已有二维码时不得补窗口/预下单/回写：ensure=%d precreate=%d save=%d",
			len(repo.ensureCalls), len(gateway.precreateCalls), len(repo.saveCalls))
	}
}

// TestPrecreateForOrderProviderFailures provider 失败与空二维码都必须返回错误，
// 且不把任何二维码交付给调用方（调用方据此执行整单补偿）。
func TestPrecreateForOrderProviderFailures(t *testing.T) {
	cases := []struct {
		name     string
		gateway  *fakeAlipayGateway
		wantCode string
	}{
		{
			name:     "provider 返回服务不可用",
			gateway:  &fakeAlipayGateway{precreateErr: port.ErrAlipayUnavailable},
			wantCode: CodeProviderUnavailable,
		},
		{
			name:     "provider 返回交易已关闭",
			gateway:  &fakeAlipayGateway{precreateErr: port.ErrAlipayTradeClosed},
			wantCode: CodeProviderUnavailable,
		},
		{
			name:     "provider 返回空二维码",
			gateway:  &fakeAlipayGateway{precreateResult: &domainpayment.PrecreateResult{QRCode: "   "}},
			wantCode: CodeProviderUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := paymentTestUnpaid()
			item.PrepayID = ""
			repo := newFakePaymentRepository()
			repo.item = item
			service := newPaymentTestService(repo, tc.gateway)

			pay, err := service.PrecreateForOrder(context.Background(), paymentTestRegistrationID)
			if pay != nil {
				t.Errorf("失败时不得返回 Payment：%+v", pay)
			}
			requireServiceError(t, err, tc.wantCode)
			if len(repo.saveCalls) != 0 {
				t.Errorf("失败时不得回写二维码，实际回写 %d 次", len(repo.saveCalls))
			}
		})
	}
}

// TestPrecreateForOrderPrepayIDWriteRetries 回写二维码失败必须有限重试（业务说明第 3.2 节）：
// 前两次失败、第三次成功时返回二维码；三次都失败时返回 DEPENDENCY_UNAVAILABLE 且不交付二维码。
// 重试间隔固定 50ms，整个用例远低于 1 秒。
func TestPrecreateForOrderPrepayIDWriteRetries(t *testing.T) {
	t.Run("前两次失败第三次成功：仍返回二维码", func(t *testing.T) {
		item := paymentTestUnpaid()
		item.PrepayID = ""
		repo := newFakePaymentRepository()
		repo.item = item
		repo.saveFailsBeforeOK = 2
		gateway := &fakeAlipayGateway{
			precreateResult: &domainpayment.PrecreateResult{QRCode: paymentTestQRCode},
		}
		service := newPaymentTestService(repo, gateway)

		pay, err := service.PrecreateForOrder(context.Background(), paymentTestRegistrationID)
		if err != nil {
			t.Fatalf("第三次回写成功不应报错，实际 %v", err)
		}
		if pay == nil || pay.PrepayID != paymentTestQRCode {
			t.Fatalf("返回的 payment.prepayId = %v，期望 %q", pay, paymentTestQRCode)
		}
		if len(repo.saveCalls) != prepayWriteAttempts {
			t.Errorf("回写尝试 %d 次，期望 %d 次", len(repo.saveCalls), prepayWriteAttempts)
		}
		if len(gateway.precreateCalls) != 1 {
			t.Errorf("预下单调用 %d 次，期望 1（重试只针对回写，不重复预下单）", len(gateway.precreateCalls))
		}
	})

	t.Run("三次回写全部失败：返回依赖不可用且不交付二维码", func(t *testing.T) {
		item := paymentTestUnpaid()
		item.PrepayID = ""
		repo := newFakePaymentRepository()
		repo.item = item
		repo.saveErr = errors.New("回写持续失败")
		gateway := &fakeAlipayGateway{
			precreateResult: &domainpayment.PrecreateResult{QRCode: paymentTestQRCode},
		}
		service := newPaymentTestService(repo, gateway)

		pay, err := service.PrecreateForOrder(context.Background(), paymentTestRegistrationID)
		if pay != nil {
			t.Errorf("回写失败不得交付二维码：%+v", pay)
		}
		requireErrIs(t, err, ErrDependencyUnavailable)
		if len(repo.saveCalls) != prepayWriteAttempts {
			t.Errorf("回写尝试 %d 次，期望 %d 次", len(repo.saveCalls), prepayWriteAttempts)
		}
		if len(gateway.precreateCalls) != 1 {
			t.Errorf("预下单调用 %d 次，期望 1", len(gateway.precreateCalls))
		}
	})
}
