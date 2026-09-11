// 挂号 HTTP 契约单测：POST /eligibility、POST、GET 列表、GET 详情的状态码、错误 envelope、
// 响应字段集合与幂等重放（spec/04-api-contract.md §1.3、§1.4、§6.1–§6.4、§10、§12.4）。
//
// gin 处于 TestMode；路由按 router.go 的方式挂载；中间件里直接注入 claims 模拟
// 「访问令牌已校验通过」（patient 与 mis 两个 realm 各一套引擎）。
// 仓储用内存桩替换，不依赖 PostgreSQL/Redis；幂等存储复用 schedule_test.go 的内存桩。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/domain/patient"
	domainpayment "Medical-Web-Backend/internal/domain/payment"
	domainregistration "Medical-Web-Backend/internal/domain/registration"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/usecase/authsession"
	registrationservice "Medical-Web-Backend/internal/usecase/registration"
)

// errRegistrationDependency 模拟 PostgreSQL 不可用（非领域错误）。
var errRegistrationDependency = errors.New("postgres is down")

const (
	// registrationHandlerTestPatientID 是患者域令牌中的当前患者主键。
	registrationHandlerTestPatientID int64 = 20
	// registrationHandlerTestMisUserID 是管理域令牌中的操作者主键。
	registrationHandlerTestMisUserID int64 = 7
	// registrationHandlerTestCardID 是当前患者名下就诊卡主键。
	registrationHandlerTestCardID int64 = 10
	// registrationHandlerTestOtherCardID 是他人名下就诊卡主键。
	registrationHandlerTestOtherCardID int64 = 11
	// registrationHandlerTestScheduleID 是被挂号的时段主键。
	registrationHandlerTestScheduleID int64 = 12
	// registrationHandlerTestTradeNo 是核对格式用的交易号样例（14 位时间戳 + 12 位十六进制）。
	registrationHandlerTestTradeNoPattern = `^[0-9]{14}[0-9a-f]{12}$`
	// registrationHandlerTestQRCode 是假预下单桩返回并"已落库"的二维码。
	registrationHandlerTestQRCode = "https://qr.alipay.com/bax-registration-handler"
	// registrationHandlerTestPayOffset / ValidOffset 是预下单结果相对固定时钟的支付窗口。
	registrationHandlerTestPayOffset   = 30 * time.Minute
	registrationHandlerTestValidOffset = 35 * time.Minute
)

// registrationHandlerNow 是固定业务时刻（业务时区 2026-09-10 09:00）。
var registrationHandlerNow = time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)

// registrationHandlerRepo 是内存版 port.RegistrationRepository 桩。
type registrationHandlerRepo struct {
	port.RegistrationRepository

	snapshot    *domainregistration.ScheduleSnapshot
	snapshotErr error
	occupied    bool
	occupiedErr error
	createErr   error
	items       []domainregistration.Registration
	total       int64
	listErr     error
	detail      *domainregistration.Detail
	detailErr   error

	// compensateErr 注入整单补偿失败，用于断言补偿失败仍返回 502。
	compensateErr error

	createCalls        int
	lastCreateInput    domainregistration.CreateInput
	listCalls          int
	lastFilter         domainregistration.Filter
	lastOffset         int
	lastLimit          int
	detailCalls        int
	lastOwnerPatientID int64

	compensateCalls  int
	lastCompensateID int64
}

func (r *registrationHandlerRepo) FindScheduleSnapshot(
	_ context.Context,
	scheduleID int64,
) (*domainregistration.ScheduleSnapshot, error) {
	if r.snapshotErr != nil {
		return nil, r.snapshotErr
	}
	if r.snapshot == nil || r.snapshot.ScheduleID != scheduleID {
		return nil, domainregistration.ErrScheduleNotFound
	}
	copied := *r.snapshot
	return &copied, nil
}

func (r *registrationHandlerRepo) HasOccupyingRegistration(
	_ context.Context,
	_ string,
	_ int64,
	_ time.Time,
) (bool, error) {
	if r.occupiedErr != nil {
		return false, r.occupiedErr
	}
	return r.occupied, nil
}

func (r *registrationHandlerRepo) CreateRegistration(
	_ context.Context,
	input domainregistration.CreateInput,
) (*domainregistration.Registration, error) {
	r.createCalls++
	r.lastCreateInput = input
	if r.createErr != nil {
		return nil, r.createErr
	}
	return &domainregistration.Registration{
		ID:              1001,
		PatientCardID:   input.PatientCardID,
		WorkPlanID:      4,
		ScheduleID:      input.ScheduleID,
		DoctorID:        16,
		SubdepartmentID: 2,
		Date:            "2026-09-20",
		Slot:            1,
		Amount:          "80.00",
		OutTradeNo:      input.OutTradeNo,
		PaymentStatus:   domainregistration.PaymentStatusUnpaid,
		CreateDate:      "2026-09-10",
	}, nil
}

func (r *registrationHandlerRepo) ListRegistrations(
	_ context.Context,
	filter domainregistration.Filter,
	offset, limit int,
) ([]domainregistration.Registration, int64, error) {
	r.listCalls++
	r.lastFilter = filter
	r.lastOffset = offset
	r.lastLimit = limit
	if r.listErr != nil {
		return nil, 0, r.listErr
	}
	return r.items, r.total, nil
}

func (r *registrationHandlerRepo) FindRegistrationDetail(
	_ context.Context,
	registrationID int64,
	ownerPatientID int64,
) (*domainregistration.Detail, error) {
	r.detailCalls++
	r.lastOwnerPatientID = ownerPatientID
	if r.detailErr != nil {
		return nil, r.detailErr
	}
	if r.detail == nil {
		return nil, domainregistration.ErrRegistrationNotFound
	}
	copied := *r.detail
	return &copied, nil
}

// CompensateRegistration 记录补偿入参并返回注入的故障，供建单预下单失败用例断言。
func (r *registrationHandlerRepo) CompensateRegistration(
	_ context.Context,
	registrationID int64,
) error {
	r.compensateCalls++
	r.lastCompensateID = registrationID
	return r.compensateErr
}

// registrationHandlerCards 是内存版 port.PatientCardRepository 桩（只实现两个读取方法）。
type registrationHandlerCards struct {
	port.PatientCardRepository

	byID        map[int64]patient.Card
	byPatientID map[int64]patient.Card
	findErr     error
}

func (r *registrationHandlerCards) FindCardByID(_ context.Context, cardID int64) (*patient.Card, error) {
	if r.findErr != nil {
		return nil, r.findErr
	}
	card, ok := r.byID[cardID]
	if !ok {
		return nil, patient.ErrCardNotFound
	}
	copied := card
	return &copied, nil
}

func (r *registrationHandlerCards) FindCardByPatientID(
	_ context.Context,
	patientID int64,
) (*patient.Card, error) {
	card, ok := r.byPatientID[patientID]
	if !ok {
		return nil, patient.ErrCardNotFound
	}
	copied := card
	return &copied, nil
}

// registrationHandlerPatients 是内存版 port.PatientUserRepository 桩。
type registrationHandlerPatients struct {
	port.PatientUserRepository

	patients map[int64]patient.Patient
}

func (r *registrationHandlerPatients) FindPatientByID(
	_ context.Context,
	patientID int64,
) (*patient.Patient, error) {
	value, ok := r.patients[patientID]
	if !ok {
		return nil, patient.ErrPatientNotFound
	}
	copied := value
	return &copied, nil
}

// registrationHandlerPrecreator 是内存版 port.PaymentPrecreator 桩：
// 记录调用次数与挂号编号，并可按需注入返回值或故障。
type registrationHandlerPrecreator struct {
	payment *domainpayment.Payment
	err     error

	calls            int
	lastRegistration int64
}

func (p *registrationHandlerPrecreator) PrecreateForOrder(
	_ context.Context,
	registrationID int64,
) (*domainpayment.Payment, error) {
	p.calls++
	p.lastRegistration = registrationID
	if p.err != nil {
		return nil, p.err
	}
	if p.payment != nil {
		copied := *p.payment
		return &copied, nil
	}
	return &domainpayment.Payment{
		RegistrationID: registrationID,
		OutTradeNo:     "20260910090000abcdef123456",
		PaymentStatus:  domainpayment.PaymentStatusUnpaid,
		PrepayID:       registrationHandlerTestQRCode,
		PrecreateAt:    registrationHandlerNow,
		PayDeadline:    registrationHandlerNow.Add(registrationHandlerTestPayOffset),
		ExpireAt:       registrationHandlerNow.Add(registrationHandlerTestValidOffset),
	}, nil
}

// registrationHandlerEnv 汇总四个路由、两个 realm 的引擎与依赖桩。
type registrationHandlerEnv struct {
	repo       *registrationHandlerRepo
	store      *fakeIdemStore
	precreator *registrationHandlerPrecreator
	patient    *gin.Engine
	mis        *gin.Engine
	noClaims   *gin.Engine
}

// registrationHandlerCompleteCard 返回资料完整、属于测试患者的就诊卡。
func registrationHandlerCompleteCard() patient.Card {
	return patient.Card{
		ID:             registrationHandlerTestCardID,
		UserID:         registrationHandlerTestPatientID,
		Name:           "张三",
		Sex:            patient.SexMale,
		PID:            "110101199001011237",
		Tel:            "13800138000",
		Birthday:       "1990-01-01",
		MedicalHistory: []string{"高血压"},
		InsuranceType:  "社会基本医疗保险",
	}
}

// registrationHandlerSnapshot 返回可挂号的时段快照。
func registrationHandlerSnapshot() domainregistration.ScheduleSnapshot {
	return domainregistration.ScheduleSnapshot{
		ScheduleID:        registrationHandlerTestScheduleID,
		WorkPlanID:        4,
		Slot:              1,
		SlotMaximum:       3,
		SlotUsed:          1,
		PlanMaximum:       10,
		PlanUsed:          8,
		Date:              "2026-09-20",
		DoctorID:          16,
		DoctorName:        "熊佳钰",
		DoctorJob:         "主任医师",
		DoctorActive:      true,
		SubdepartmentID:   2,
		SubdepartmentName: "口腔颌面外科",
		Amount:            "80.00",
	}
}

// newRegistrationHandlerEnv 构造 handler 测试环境（固定业务时刻）。
func newRegistrationHandlerEnv(t *testing.T) *registrationHandlerEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	card := registrationHandlerCompleteCard()
	snapshot := registrationHandlerSnapshot()
	repo := &registrationHandlerRepo{snapshot: &snapshot}
	cards := &registrationHandlerCards{
		byID: map[int64]patient.Card{
			registrationHandlerTestCardID:      card,
			registrationHandlerTestOtherCardID: {ID: registrationHandlerTestOtherCardID, UserID: 99},
		},
		byPatientID: map[int64]patient.Card{registrationHandlerTestPatientID: card},
	}
	patients := &registrationHandlerPatients{
		patients: map[int64]patient.Patient{
			registrationHandlerTestPatientID: {
				ID: registrationHandlerTestPatientID, Status: patient.StatusActive,
			},
		},
	}

	precreator := &registrationHandlerPrecreator{}
	service := registrationservice.NewService(
		repo, cards, patients, precreator, func() time.Time { return registrationHandlerNow })
	store := newFakeIdemStore()
	h := NewRegistrationHandler(service, store)

	return &registrationHandlerEnv{
		repo:       repo,
		store:      store,
		precreator: precreator,
		patient:    registrationHandlerEngine(t, h, domainauth.RealmPatient, registrationHandlerTestPatientID),
		mis:        registrationHandlerEngine(t, h, domainauth.RealmMis, registrationHandlerTestMisUserID),
		noClaims:   registrationHandlerEngine(t, h, "", 0),
	}
}

// registrationHandlerEngine 按 router.go 的挂载方式注册四个挂号路由；
// realm 为空时不写入 claims，模拟访问令牌中间件缺失或被绕过。
func registrationHandlerEngine(
	t *testing.T,
	h *RegistrationHandler,
	realm domainauth.Realm,
	userID int64,
) *gin.Engine {
	t.Helper()

	engine := gin.New()
	group := engine.Group("/api/v1/registrations")
	if realm != "" {
		group.Use(func(c *gin.Context) {
			c.Set(middleware.ClaimsKey, &authsession.AccessClaims{
				UserID:    userID,
				Username:  "registration-test",
				TokenType: authsession.TokenTypeAccess,
				Realm:     realm,
			})
			c.Next()
		})
	}
	group.POST("/eligibility", h.Eligibility)
	group.POST("", h.Create)
	group.GET("", h.List)
	group.GET("/:registrationId", h.Detail)
	return engine
}

// performRegistration 发起挂号请求；body 非空时作为 JSON 请求体，headers 为附加请求头。
func performRegistration(
	engine *gin.Engine,
	method, target, body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// decodeRegistrationBody 解析响应体为通用 JSON 对象。
func decodeRegistrationBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应体失败：%v body=%s", err, w.Body.String())
	}
	return body
}

// assertRegistrationKeys 断言 JSON 对象的字段集合与契约一致（多一个字段也算违约）。
func assertRegistrationKeys(t *testing.T, prefix string, body map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(body))
	for key := range body {
		got = append(got, key)
	}
	sort.Strings(got)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if strings.Join(got, ",") != strings.Join(wantSorted, ",") {
		t.Errorf("%s 字段集合 = %v, want %v", prefix, got, wantSorted)
	}
}

// registrationErrorCode 读取错误 envelope 的 code（契约 §1.3）。
func registrationErrorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeRegistrationBody(t, w)
	code, _ := body["code"].(string)
	if code == "" {
		t.Fatalf("错误响应缺少 code：%s", w.Body.String())
	}
	return code
}

// TestRegistrationEligibilityEndpoint 资格校验：200 + eligible/reasons，参数错误 422，
// 依赖故障 502（契约 §6.1、§12.4）。
func TestRegistrationEligibilityEndpoint(t *testing.T) {
	t.Run("资格通过返回 200 与剩余号源", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations/eligibility",
			`{"patientCardId":10,"scheduleId":12}`, nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		assertRegistrationKeys(t, "资格校验响应", body, []string{"eligible", "remaining", "amount", "reasons"})
		if body["eligible"] != true {
			t.Errorf("eligible = %v, want true", body["eligible"])
		}
		if body["remaining"] != float64(2) {
			t.Errorf("remaining = %v, want 2", body["remaining"])
		}
		if body["amount"] != "80.00" {
			t.Errorf("amount = %v, want 80.00", body["amount"])
		}
		if reasons, ok := body["reasons"].([]any); !ok || len(reasons) != 0 {
			t.Errorf("reasons = %#v, want []（不能为 null）", body["reasons"])
		}
	})

	t.Run("时段不存在仍返回 200 与 reasons 词表", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.snapshotErr = domainregistration.ErrScheduleNotFound

		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations/eligibility",
			`{"patientCardId":10,"scheduleId":99}`, nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		if body["eligible"] != false {
			t.Errorf("eligible = %v, want false", body["eligible"])
		}
		if body["remaining"] != float64(0) {
			t.Errorf("remaining = %v, want 0", body["remaining"])
		}
		if body["amount"] != nil {
			t.Errorf("amount = %v, want null", body["amount"])
		}
		reasons, _ := body["reasons"].([]any)
		if len(reasons) != 1 || reasons[0] != "SCHEDULE_NOT_FOUND" {
			t.Errorf("reasons = %#v, want [SCHEDULE_NOT_FOUND]", body["reasons"])
		}
	})

	t.Run("缺少必传字段返回 422", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations/eligibility",
			`{"patientCardId":10}`, nil)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		if code := registrationErrorCode(t, w); code != "REQUEST_VALIDATION_FAILED" {
			t.Errorf("code = %q, want REQUEST_VALIDATION_FAILED", code)
		}
		if message := decodeRegistrationBody(t, w)["message"]; message != "scheduleId 为必传字段" {
			t.Errorf("message = %v, want scheduleId 为必传字段", message)
		}
	})

	t.Run("依赖故障返回 502 DEPENDENCY_UNAVAILABLE", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.snapshotErr = errRegistrationDependency

		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations/eligibility",
			`{"patientCardId":10,"scheduleId":12}`, nil)

		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
		}
		if code := registrationErrorCode(t, w); code != "DEPENDENCY_UNAVAILABLE" {
			t.Errorf("code = %q, want DEPENDENCY_UNAVAILABLE", code)
		}
	})
}

// TestRegistrationListEndpoint 列表：分页 envelope、字段集合（不含 workPlanId/scheduleId）、
// 患者端归属过滤与查询参数校验（契约 §1.4、§6.3、§12.4）。
func TestRegistrationListEndpoint(t *testing.T) {
	t.Run("患者端只返回本人记录且字段集合符合契约", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.items = []domainregistration.Registration{{
			ID:              1001,
			PatientCardID:   registrationHandlerTestCardID,
			WorkPlanID:      4,
			ScheduleID:      registrationHandlerTestScheduleID,
			DoctorID:        16,
			SubdepartmentID: 2,
			Date:            "2026-09-20",
			Slot:            1,
			Amount:          "80.00",
			OutTradeNo:      "202609080001",
			PaymentStatus:   domainregistration.PaymentStatusUnpaid,
			CreateDate:      "2026-09-08",
			// 列表项必须回传支付窗口：payableUntil = pay_deadline、validUntil = expire_at（§6.3）。
			PayDeadline: registrationHandlerNow.Add(registrationHandlerTestPayOffset),
			ExpireAt:    registrationHandlerNow.Add(registrationHandlerTestValidOffset),
		}}
		env.repo.total = 1

		w := performRegistration(env.patient, http.MethodGet, "/api/v1/registrations?page=1&pageSize=20", "", nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		assertRegistrationKeys(t, "列表 envelope", body, []string{"items", "page", "pageSize", "total"})
		if body["page"] != float64(1) || body["pageSize"] != float64(20) || body["total"] != float64(1) {
			t.Errorf("分页元数据 = %#v, want page=1 pageSize=20 total=1", body)
		}
		items, ok := body["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("items = %#v, want 单元素数组", body["items"])
		}
		item, ok := items[0].(map[string]any)
		if !ok {
			t.Fatalf("items[0] = %#v, want 对象", items[0])
		}
		assertRegistrationKeys(t, "列表项", item, []string{
			"id", "patientCardId", "doctorId", "subdepartmentId", "date", "slot",
			"amount", "outTradeNo", "paymentStatus", "createDate",
			"payableUntil", "validUntil",
		})
		// 二维码/交易号只允许出现在建单（§6.2）与取支付参数（§6.5）两个受保护响应中，
		// 列表项（§6.3）不得出现 qrCode/prepayId/transactionId。
		for _, forbidden := range []string{"workPlanId", "scheduleId", "qrCode", "prepayId", "transactionId"} {
			if _, exists := item[forbidden]; exists {
				t.Errorf("列表项不得包含 %s（契约 §6.3）", forbidden)
			}
		}
		// 与建单 201 同一口径：RFC3339、UTC，取值来自 pay_deadline / expire_at。
		wantPayableUntil := registrationHandlerNow.Add(registrationHandlerTestPayOffset).UTC().Format(time.RFC3339)
		if item["payableUntil"] != wantPayableUntil {
			t.Errorf("payableUntil = %v, want %q（等于 pay_deadline）", item["payableUntil"], wantPayableUntil)
		}
		wantValidUntil := registrationHandlerNow.Add(registrationHandlerTestValidOffset).UTC().Format(time.RFC3339)
		if item["validUntil"] != wantValidUntil {
			t.Errorf("validUntil = %v, want %q（等于 expire_at）", item["validUntil"], wantValidUntil)
		}
		if env.repo.lastFilter.OwnerPatientID == nil ||
			*env.repo.lastFilter.OwnerPatientID != registrationHandlerTestPatientID {
			t.Errorf("患者端归属过滤 = %#v, want ownerPatientID=%d",
				env.repo.lastFilter.OwnerPatientID, registrationHandlerTestPatientID)
		}
		if env.repo.lastOffset != 0 || env.repo.lastLimit != 20 {
			t.Errorf("offset/limit = %d/%d, want 0/20", env.repo.lastOffset, env.repo.lastLimit)
		}
	})

	t.Run("空列表输出 items 空数组", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.items = nil

		w := performRegistration(env.patient, http.MethodGet, "/api/v1/registrations", "", nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"items":[]`) {
			t.Errorf("空列表必须输出 items:[] 而不是 null：%s", w.Body.String())
		}
	})

	t.Run("历史数据缺少支付窗口时输出空串", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		// pay_deadline/expire_at 为空（NULL）：列表项仍须返回两个键，但值为空串。
		env.repo.items = []domainregistration.Registration{{
			ID:            1001,
			PatientCardID: registrationHandlerTestCardID,
			DoctorID:      16,
			Date:          "2026-09-20",
			Slot:          1,
			Amount:        "80.00",
			OutTradeNo:    "202609080001",
			PaymentStatus: domainregistration.PaymentStatusUnpaid,
			CreateDate:    "2026-09-08",
		}}
		env.repo.total = 1

		w := performRegistration(env.patient, http.MethodGet, "/api/v1/registrations?page=1&pageSize=20", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		items, ok := body["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("items = %#v, want 单元素数组", body["items"])
		}
		item, ok := items[0].(map[string]any)
		if !ok {
			t.Fatalf("items[0] = %#v, want 对象", items[0])
		}
		if item["payableUntil"] != "" || item["validUntil"] != "" {
			t.Errorf("零值支付窗口应序列化为空串：payableUntil=%v validUntil=%v",
				item["payableUntil"], item["validUntil"])
		}
	})

	t.Run("管理端按条件查询且不带归属过滤", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		w := performRegistration(env.mis, http.MethodGet,
			"/api/v1/registrations?doctorId=16&paymentStatus=PAID&page=2&pageSize=10&sort=date&order=asc", "", nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if env.repo.lastFilter.OwnerPatientID != nil {
			t.Errorf("管理端不应带归属过滤：%#v", env.repo.lastFilter.OwnerPatientID)
		}
		if env.repo.lastFilter.DoctorID == nil || *env.repo.lastFilter.DoctorID != 16 {
			t.Errorf("doctorId 过滤未下发：%#v", env.repo.lastFilter)
		}
		if env.repo.lastFilter.PaymentStatus == nil ||
			*env.repo.lastFilter.PaymentStatus != domainregistration.PaymentCodePaid {
			t.Errorf("paymentStatus 过滤未下发：%#v", env.repo.lastFilter)
		}
		if env.repo.lastFilter.Sort != domainregistration.SortDate || env.repo.lastFilter.Order != "asc" {
			t.Errorf("排序 = (%q,%q), want (date,asc)", env.repo.lastFilter.Sort, env.repo.lastFilter.Order)
		}
		if env.repo.lastOffset != 10 || env.repo.lastLimit != 10 {
			t.Errorf("offset/limit = %d/%d, want 10/10", env.repo.lastOffset, env.repo.lastLimit)
		}
	})

	t.Run("查询参数校验失败返回 422", func(t *testing.T) {
		cases := []struct {
			name        string
			query       string
			wantMessage string
		}{
			{"paymentStatus 不在词表", "paymentStatus=WAITING", "支付状态不支持"},
			{"page 从 0 开始", "page=0", "page 必须从 1 开始"},
			{"pageSize 越界", "pageSize=101", "pageSize 必须在 1 到 100 之间"},
			{"fromDate 晚于 toDate", "fromDate=2026-09-30&toDate=2026-09-01", "日期范围无效"},
			{"sort 不在白名单", "sort=amount", "sort 只支持 createDate/date/id"},
			{"order 不在白名单", "order=random", "order 只支持 asc/desc"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				env := newRegistrationHandlerEnv(t)
				w := performRegistration(env.mis, http.MethodGet, "/api/v1/registrations?"+tc.query, "", nil)

				if w.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
				}
				body := decodeRegistrationBody(t, w)
				if body["code"] != "REQUEST_VALIDATION_FAILED" {
					t.Errorf("code = %v, want REQUEST_VALIDATION_FAILED", body["code"])
				}
				if body["message"] != tc.wantMessage {
					t.Errorf("message = %v, want %q", body["message"], tc.wantMessage)
				}
				if env.repo.listCalls != 0 {
					t.Errorf("参数非法不得触达仓储，ListRegistrations 调用 %d 次", env.repo.listCalls)
				}
			})
		}
	})

	t.Run("依赖故障返回 502", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.listErr = errRegistrationDependency

		w := performRegistration(env.mis, http.MethodGet, "/api/v1/registrations", "", nil)

		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
		}
		if code := registrationErrorCode(t, w); code != "DEPENDENCY_UNAVAILABLE" {
			t.Errorf("code = %q, want DEPENDENCY_UNAVAILABLE", code)
		}
	})
}

// TestRegistrationDetailEndpoint 详情：医生/科室摘要与时段容量字段集合、患者端归属参数、
// 越权与不存在统一 404（契约 §6.4、§12.4）。
func TestRegistrationDetailEndpoint(t *testing.T) {
	detailFixture := func() *domainregistration.Detail {
		return &domainregistration.Detail{
			ID:            1001,
			PatientCardID: registrationHandlerTestCardID,
			Doctor: domainregistration.DoctorSummary{
				ID: 16, Name: "熊佳钰", Job: "主任医师",
			},
			Subdepartment: domainregistration.SubdepartmentSummary{ID: 2, Name: "口腔颌面外科"},
			Date:          "2026-09-20",
			Slot:          1,
			Capacity:      domainregistration.Capacity{Maximum: 3, Used: 1, Remaining: 2},
			Amount:        "80.00",
			OutTradeNo:    "202609080001",
			PaymentStatus: domainregistration.PaymentStatusUnpaid,
		}
	}

	t.Run("患者端返回详情并传入本人归属", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.detail = detailFixture()

		w := performRegistration(env.patient, http.MethodGet, "/api/v1/registrations/1001", "", nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		assertRegistrationKeys(t, "详情响应", body, []string{
			"id", "patientCardId", "doctor", "subdepartment", "date", "slot",
			"capacity", "amount", "outTradeNo", "paymentStatus",
		})
		doctor, ok := body["doctor"].(map[string]any)
		if !ok {
			t.Fatalf("doctor = %#v, want 对象", body["doctor"])
		}
		assertRegistrationKeys(t, "详情 doctor", doctor, []string{"id", "name", "job"})
		subdepartment, ok := body["subdepartment"].(map[string]any)
		if !ok {
			t.Fatalf("subdepartment = %#v, want 对象", body["subdepartment"])
		}
		assertRegistrationKeys(t, "详情 subdepartment", subdepartment, []string{"id", "name"})
		capacity, ok := body["capacity"].(map[string]any)
		if !ok {
			t.Fatalf("capacity = %#v, want 对象", body["capacity"])
		}
		assertRegistrationKeys(t, "详情 capacity", capacity, []string{"maximum", "used", "remaining"})
		if capacity["maximum"] != float64(3) || capacity["used"] != float64(1) || capacity["remaining"] != float64(2) {
			t.Errorf("capacity = %#v, want maximum=3 used=1 remaining=2", capacity)
		}
		if _, exists := body["prepayId"]; exists {
			t.Error("prepayId 不得出现在挂号详情中（契约 §6.3）")
		}
		if env.repo.lastOwnerPatientID != registrationHandlerTestPatientID {
			t.Errorf("患者端 ownerPatientID = %d, want %d",
				env.repo.lastOwnerPatientID, registrationHandlerTestPatientID)
		}
	})

	t.Run("管理端不限定归属", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.detail = detailFixture()

		w := performRegistration(env.mis, http.MethodGet, "/api/v1/registrations/1001", "", nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if env.repo.lastOwnerPatientID != 0 {
			t.Errorf("管理端 ownerPatientID = %d, want 0", env.repo.lastOwnerPatientID)
		}
	})

	t.Run("越权或不存在统一 404", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.detailErr = domainregistration.ErrRegistrationNotFound

		w := performRegistration(env.patient, http.MethodGet, "/api/v1/registrations/9999", "", nil)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		if body["code"] != "REGISTRATION_NOT_FOUND" {
			t.Errorf("code = %v, want REGISTRATION_NOT_FOUND", body["code"])
		}
		if body["message"] != "挂号记录不存在" {
			t.Errorf("message = %v, want 挂号记录不存在", body["message"])
		}
	})

	t.Run("路径参数非正整数返回 422", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		w := performRegistration(env.patient, http.MethodGet, "/api/v1/registrations/abc", "", nil)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		if code := registrationErrorCode(t, w); code != "REQUEST_VALIDATION_FAILED" {
			t.Errorf("code = %q, want REQUEST_VALIDATION_FAILED", code)
		}
	})

	t.Run("依赖故障返回 502", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.detailErr = errRegistrationDependency

		w := performRegistration(env.patient, http.MethodGet, "/api/v1/registrations/1001", "", nil)

		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
		}
		if code := registrationErrorCode(t, w); code != "DEPENDENCY_UNAVAILABLE" {
			t.Errorf("code = %q, want DEPENDENCY_UNAVAILABLE", code)
		}
	})
}

// TestRegistrationEndpointsRequireClaims 缺少令牌载荷时四个接口都必须返回 401
// AUTH_INVALID_TOKEN，不得降级放行（契约 §1.2）。
func TestRegistrationEndpointsRequireClaims(t *testing.T) {
	cases := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"资格校验", http.MethodPost, "/api/v1/registrations/eligibility", `{"patientCardId":10,"scheduleId":12}`},
		{"建单", http.MethodPost, "/api/v1/registrations", `{"patientCardId":10,"scheduleId":12,"paymentMethod":"ALIPAY"}`},
		{"列表", http.MethodGet, "/api/v1/registrations", ""},
		{"详情", http.MethodGet, "/api/v1/registrations/1001", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRegistrationHandlerEnv(t)
			w := performRegistration(env.noClaims, tc.method, tc.target, tc.body,
				map[string]string{"Idempotency-Key": "registration-noclaims"})

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
			if code := registrationErrorCode(t, w); code != "AUTH_INVALID_TOKEN" {
				t.Errorf("code = %q, want AUTH_INVALID_TOKEN", code)
			}
		})
	}
}

// TestRegistrationCreateEndpoint 建单：201 + Location + registration 资源字段集合，
// 幂等键缺失 422、同 key 重放第一次结果（契约 §6.2、§12.4）。
func TestRegistrationCreateEndpoint(t *testing.T) {
	// 契约 §6.2 规定第一阶段只接受 paymentMethod=ALIPAY（含历史示例中的 WECHAT 一律 422）。
	// 本测试环境的就诊卡 10、时段 12 与 §12.4 建单样例完全对应。
	requestBody := `{"patientCardId":10,"scheduleId":12,"paymentMethod":"ALIPAY"}`

	t.Run("契约 §12.4 样例字面请求成功", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		// 逐字使用 §12.4 的请求样例（保留样例中的空格），确保测试绑定的是契约文本。
		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations",
			`{ "patientCardId": 10, "scheduleId": 12, "paymentMethod": "ALIPAY" }`,
			map[string]string{"Idempotency-Key": "registration-create-12-4-sample"})

		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		if body["patientCardId"] != float64(10) || body["scheduleId"] != float64(12) {
			t.Errorf("body = %#v, want patientCardId=10 且 scheduleId=12（§12.4 样例）", body)
		}
	})

	t.Run("WECHAT 按契约 §6.2 返回 422 且不建单", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations",
			`{"patientCardId":10,"scheduleId":12,"paymentMethod":"WECHAT"}`,
			map[string]string{"Idempotency-Key": "registration-create-wechat"})

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		if body["code"] != "REQUEST_VALIDATION_FAILED" || body["message"] != "暂不支持该支付方式" {
			t.Errorf("body = %#v, want REQUEST_VALIDATION_FAILED/暂不支持该支付方式", body)
		}
		if env.repo.createCalls != 0 {
			t.Errorf("非法支付方式不得建单，CreateRegistration 调用 %d 次", env.repo.createCalls)
		}
	})

	t.Run("小写 alipay 按精确匹配返回 422 且不建单", func(t *testing.T) {
		// 请求层不做归一化，use case 也采用精确匹配：大小写变体必须与 WECHAT 同样被拒。
		env := newRegistrationHandlerEnv(t)
		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations",
			`{"patientCardId":10,"scheduleId":12,"paymentMethod":"alipay"}`,
			map[string]string{"Idempotency-Key": "registration-create-alipay-lower"})

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		if body["code"] != "REQUEST_VALIDATION_FAILED" || body["message"] != "暂不支持该支付方式" {
			t.Errorf("body = %#v, want REQUEST_VALIDATION_FAILED/暂不支持该支付方式", body)
		}
		if env.repo.createCalls != 0 {
			t.Errorf("非法支付方式不得建单，CreateRegistration 调用 %d 次", env.repo.createCalls)
		}
	})

	t.Run("建单成功返回 201 与 Location", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations",
			requestBody, map[string]string{"Idempotency-Key": "registration-create-1"})

		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
		}
		if location := w.Header().Get("Location"); location != "/api/v1/registrations/1001" {
			t.Errorf("Location = %q, want /api/v1/registrations/1001", location)
		}
		body := decodeRegistrationBody(t, w)
		assertRegistrationKeys(t, "建单响应", body, []string{
			"id", "patientCardId", "workPlanId", "scheduleId", "doctorId", "subdepartmentId",
			"date", "slot", "amount", "outTradeNo", "paymentStatus", "createDate",
			"qrCode", "payableUntil", "validUntil",
		})
		if body["id"] != float64(1001) {
			t.Errorf("id = %v, want 1001", body["id"])
		}
		if body["patientCardId"] != float64(registrationHandlerTestCardID) {
			t.Errorf("patientCardId = %v, want %d", body["patientCardId"], registrationHandlerTestCardID)
		}
		if body["scheduleId"] != float64(registrationHandlerTestScheduleID) {
			t.Errorf("scheduleId = %v, want %d", body["scheduleId"], registrationHandlerTestScheduleID)
		}
		if body["paymentStatus"] != domainregistration.PaymentStatusUnpaid {
			t.Errorf("paymentStatus = %v, want UNPAID", body["paymentStatus"])
		}
		if body["amount"] != "80.00" {
			t.Errorf("amount = %v, want 80.00（金额由服务端从价目推导）", body["amount"])
		}
		tradeNo, _ := body["outTradeNo"].(string)
		if !regexp.MustCompile(registrationHandlerTestTradeNoPattern).MatchString(tradeNo) {
			t.Errorf("outTradeNo = %q, want 14 位时间戳 + 12 位十六进制", tradeNo)
		}
		if _, exists := body["prepayId"]; exists {
			t.Error("prepayId 不得出现在建单响应中（契约 §6.2、§6.3）")
		}
		// 201 必须回传同一次请求内完成的预下单结果：二维码与两个时间点（RFC3339、UTC）。
		if body["qrCode"] != registrationHandlerTestQRCode {
			t.Errorf("qrCode = %v, want %q", body["qrCode"], registrationHandlerTestQRCode)
		}
		wantPayableUntil := registrationHandlerNow.Add(registrationHandlerTestPayOffset).UTC().Format(time.RFC3339)
		if body["payableUntil"] != wantPayableUntil {
			t.Errorf("payableUntil = %v, want %q", body["payableUntil"], wantPayableUntil)
		}
		wantValidUntil := registrationHandlerNow.Add(registrationHandlerTestValidOffset).UTC().Format(time.RFC3339)
		if body["validUntil"] != wantValidUntil {
			t.Errorf("validUntil = %v, want %q", body["validUntil"], wantValidUntil)
		}
		if env.repo.createCalls != 1 {
			t.Errorf("CreateRegistration 调用次数 = %d, want 1", env.repo.createCalls)
		}
		if env.precreator.calls != 1 || env.precreator.lastRegistration != 1001 {
			t.Errorf("预下单调用 %d 次（registrationId=%d），want 1 次（registrationId=1001）",
				env.precreator.calls, env.precreator.lastRegistration)
		}
		if env.repo.lastCreateInput.PatientCardID != registrationHandlerTestCardID {
			t.Errorf("落库 patientCardId = %d, want %d",
				env.repo.lastCreateInput.PatientCardID, registrationHandlerTestCardID)
		}
	})

	t.Run("缺少 Idempotency-Key 返回 422 且不建单", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations", requestBody, nil)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		if body["code"] != "REQUEST_VALIDATION_FAILED" {
			t.Errorf("code = %v, want REQUEST_VALIDATION_FAILED", body["code"])
		}
		if body["message"] != "Idempotency-Key 请求头必填" {
			t.Errorf("message = %v, want Idempotency-Key 请求头必填", body["message"])
		}
		if env.repo.createCalls != 0 {
			t.Errorf("缺少幂等键时不得建单，CreateRegistration 调用 %d 次", env.repo.createCalls)
		}
	})

	t.Run("同一幂等键重放第一次结果且只建单一次", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		headers := map[string]string{"Idempotency-Key": "registration-create-replay"}

		first := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations", requestBody, headers)
		second := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations", requestBody, headers)

		if first.Code != http.StatusCreated {
			t.Fatalf("首次 status = %d, want 201; body=%s", first.Code, first.Body.String())
		}
		if second.Code != http.StatusCreated {
			t.Fatalf("重放 status = %d, want 201; body=%s", second.Code, second.Body.String())
		}
		if first.Body.String() != second.Body.String() {
			t.Errorf("重放响应体不一致：\nfirst=%s\nsecond=%s", first.Body.String(), second.Body.String())
		}
		if location := second.Header().Get("Location"); location != "/api/v1/registrations/1001" {
			t.Errorf("重放 Location = %q, want /api/v1/registrations/1001", location)
		}
		if env.repo.createCalls != 1 {
			t.Errorf("同一幂等键只应建单一次，CreateRegistration 调用 %d 次", env.repo.createCalls)
		}
	})

	t.Run("非法 JSON 返回 400 REQUEST_INVALID_JSON", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations",
			`{"patientCardId":`, map[string]string{"Idempotency-Key": "registration-create-bad-json"})

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
		}
		if code := registrationErrorCode(t, w); code != "REQUEST_INVALID_JSON" {
			t.Errorf("code = %q, want REQUEST_INVALID_JSON", code)
		}
	})

	t.Run("号源不足返回 409 与 details", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.createErr = &domainregistration.SlotSoldOutError{
			ScheduleID: registrationHandlerTestScheduleID,
			Remaining:  0,
		}

		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations",
			requestBody, map[string]string{"Idempotency-Key": "registration-create-soldout"})

		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		if body["code"] != "REGISTRATION_SLOT_SOLD_OUT" {
			t.Errorf("code = %v, want REGISTRATION_SLOT_SOLD_OUT", body["code"])
		}
		if body["message"] != "号源已满" {
			t.Errorf("message = %v, want 号源已满", body["message"])
		}
		details, ok := body["details"].(map[string]any)
		if !ok {
			t.Fatalf("details = %#v, want 对象", body["details"])
		}
		if details["scheduleId"] != float64(registrationHandlerTestScheduleID) {
			t.Errorf("details.scheduleId = %v, want %d", details["scheduleId"], registrationHandlerTestScheduleID)
		}
		if details["remaining"] != float64(0) {
			t.Errorf("details.remaining = %v, want 0", details["remaining"])
		}
	})

	t.Run("资格判重命中返回 409 REGISTRATION_DUPLICATE", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.occupied = true

		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations",
			requestBody, map[string]string{"Idempotency-Key": "registration-create-duplicate"})

		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
		}
		if code := registrationErrorCode(t, w); code != "REGISTRATION_DUPLICATE" {
			t.Errorf("code = %q, want REGISTRATION_DUPLICATE", code)
		}
	})

	t.Run("管理端代建缺少 patientCardId 返回 422", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		w := performRegistration(env.mis, http.MethodPost, "/api/v1/registrations",
			`{"scheduleId":12,"paymentMethod":"ALIPAY"}`,
			map[string]string{"Idempotency-Key": "registration-create-mis-nocard"})

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		body := decodeRegistrationBody(t, w)
		if body["code"] != "REQUEST_VALIDATION_FAILED" || body["message"] != "patientCardId 必填" {
			t.Errorf("body = %#v, want REQUEST_VALIDATION_FAILED/patientCardId 必填", body)
		}
	})

	t.Run("依赖故障返回 502", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.repo.createErr = errRegistrationDependency

		w := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations",
			requestBody, map[string]string{"Idempotency-Key": "registration-create-down"})

		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
		}
		if code := registrationErrorCode(t, w); code != "DEPENDENCY_UNAVAILABLE" {
			t.Errorf("code = %q, want DEPENDENCY_UNAVAILABLE", code)
		}
	})

	t.Run("预下单失败返回 502 且不写入幂等存储", func(t *testing.T) {
		env := newRegistrationHandlerEnv(t)
		env.precreator.err = errors.New("alipay precreate down")
		headers := map[string]string{"Idempotency-Key": "registration-create-precreate-fail"}

		first := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations", requestBody, headers)
		if first.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%s", first.Code, first.Body.String())
		}
		body := decodeRegistrationBody(t, first)
		if body["code"] != "PAYMENT_PROVIDER_UNAVAILABLE" {
			t.Errorf("code = %v, want PAYMENT_PROVIDER_UNAVAILABLE", body["code"])
		}
		if _, exists := body["qrCode"]; exists {
			t.Error("预下单失败响应不得包含 qrCode")
		}
		if env.repo.compensateCalls != 1 || env.repo.lastCompensateID != 1001 {
			t.Errorf("预下单失败应补偿一次（registrationId=1001），实际 %d 次（id=%d）",
				env.repo.compensateCalls, env.repo.lastCompensateID)
		}
		if env.store.saves != 0 || len(env.store.results) != 0 {
			t.Errorf("5xx 不得写入幂等存储：saves=%d results=%d", env.store.saves, len(env.store.results))
		}

		// 同一幂等键再次请求：5xx 不入库，应重新执行业务而不是重放第一次响应。
		second := performRegistration(env.patient, http.MethodPost, "/api/v1/registrations", requestBody, headers)
		if second.Code != http.StatusBadGateway {
			t.Fatalf("重试 status = %d, want 502; body=%s", second.Code, second.Body.String())
		}
		if env.repo.createCalls != 2 {
			t.Errorf("5xx 未入幂等存储，重试应重新建单，CreateRegistration 调用 %d 次，want 2",
				env.repo.createCalls)
		}
	})
}
