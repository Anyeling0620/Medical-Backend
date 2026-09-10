// 挂号用例层单测：资格校验（含 7 个 reasons）、建单、列表与详情的领域规则与错误归类
// （spec/04-api-contract.md §1.2、§1.4、§6.1–§6.4、§10、§12.4）。
//
// 全部用例用内存桩替换 port.RegistrationRepository / port.PatientCardRepository /
// port.PatientUserRepository，不依赖 PostgreSQL 与 Redis。
package registration

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/domain/patient"
	domainregistration "Medical-Web-Backend/internal/domain/registration"
	"Medical-Web-Backend/internal/port"
)

const (
	// registrationTestPatientID 是令牌中的当前患者主键（realm=patient 时主体固定为它）。
	registrationTestPatientID int64 = 20
	// registrationTestCardID 是当前患者名下就诊卡主键。
	registrationTestCardID int64 = 10
	// registrationTestOtherCardID 是他人名下就诊卡主键。
	registrationTestOtherCardID int64 = 11
	// registrationTestScheduleID 是被挂号的时段主键。
	registrationTestScheduleID int64 = 12
	// registrationTestTradeNo 是注入的固定外部交易号，保证断言可重复。
	registrationTestTradeNo = "20260910090000abcdef123456"
	// registrationTestAmount 是排班快照给出的应付金额。
	registrationTestAmount = "80.00"
)

// registrationTestNow 是注入 Service 的固定业务时刻：业务时区（UTC+8）2026-09-10 09:00。
var registrationTestNow = time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)

// registrationFakeRepo 是内存版 port.RegistrationRepository 桩：
// 记录调用参数（用于断言归属过滤、offset/limit 与占用判定入参），并按需注入故障。
type registrationFakeRepo struct {
	port.RegistrationRepository

	snapshot     *domainregistration.ScheduleSnapshot
	snapshotErr  error
	occupied     bool
	occupiedErr  error
	createResult *domainregistration.Registration
	createErr    error
	items        []domainregistration.Registration
	total        int64
	listErr      error
	detail       *domainregistration.Detail
	detailErr    error

	// 调用记录。
	snapshotCalls          int
	lastSnapshotID         int64
	occupiedCalls          int
	lastOccupiedPID        string
	lastOccupiedScheduleID int64
	lastOccupiedNow        time.Time
	createCalls            int
	lastCreateInput        domainregistration.CreateInput
	listCalls              int
	lastFilter             domainregistration.Filter
	lastOffset             int
	lastLimit              int
	detailCalls            int
	lastDetailID           int64
	lastOwnerPatientID     int64
}

func (r *registrationFakeRepo) FindScheduleSnapshot(
	_ context.Context,
	scheduleID int64,
) (*domainregistration.ScheduleSnapshot, error) {
	r.snapshotCalls++
	r.lastSnapshotID = scheduleID
	if r.snapshotErr != nil {
		return nil, r.snapshotErr
	}
	if r.snapshot == nil {
		// 与 PostgresRegistrationRepository 一致：时段不存在返回领域哨兵错误。
		return nil, domainregistration.ErrScheduleNotFound
	}
	copied := *r.snapshot
	return &copied, nil
}

func (r *registrationFakeRepo) HasOccupyingRegistration(
	_ context.Context,
	pid string,
	scheduleID int64,
	now time.Time,
) (bool, error) {
	r.occupiedCalls++
	r.lastOccupiedPID = pid
	r.lastOccupiedScheduleID = scheduleID
	r.lastOccupiedNow = now
	if r.occupiedErr != nil {
		return false, r.occupiedErr
	}
	return r.occupied, nil
}

func (r *registrationFakeRepo) CreateRegistration(
	_ context.Context,
	input domainregistration.CreateInput,
) (*domainregistration.Registration, error) {
	r.createCalls++
	r.lastCreateInput = input
	if r.createErr != nil {
		return nil, r.createErr
	}
	if r.createResult != nil {
		return r.createResult, nil
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
		Amount:          registrationTestAmount,
		OutTradeNo:      input.OutTradeNo,
		PaymentStatus:   domainregistration.PaymentStatusUnpaid,
		CreateDate:      "2026-09-10",
	}, nil
}

func (r *registrationFakeRepo) ListRegistrations(
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

func (r *registrationFakeRepo) FindRegistrationDetail(
	_ context.Context,
	registrationID int64,
	ownerPatientID int64,
) (*domainregistration.Detail, error) {
	r.detailCalls++
	r.lastDetailID = registrationID
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

// registrationFakeCards 是内存版 port.PatientCardRepository 桩（只实现本用例真正调用的两个读取方法）。
type registrationFakeCards struct {
	port.PatientCardRepository

	byID             map[int64]patient.Card
	byPatientID      map[int64]patient.Card
	findErr          error
	findByPatientErr error

	findByIDCalls        int
	findByPatientIDCalls int
}

func (r *registrationFakeCards) FindCardByID(_ context.Context, cardID int64) (*patient.Card, error) {
	r.findByIDCalls++
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

func (r *registrationFakeCards) FindCardByPatientID(_ context.Context, patientID int64) (*patient.Card, error) {
	r.findByPatientIDCalls++
	if r.findByPatientErr != nil {
		return nil, r.findByPatientErr
	}
	card, ok := r.byPatientID[patientID]
	if !ok {
		return nil, patient.ErrCardNotFound
	}
	copied := card
	return &copied, nil
}

// registrationFakePatients 是内存版 port.PatientUserRepository 桩（只实现 FindPatientByID）。
type registrationFakePatients struct {
	port.PatientUserRepository

	patients map[int64]patient.Patient
	findErr  error
}

func (r *registrationFakePatients) FindPatientByID(_ context.Context, patientID int64) (*patient.Patient, error) {
	if r.findErr != nil {
		return nil, r.findErr
	}
	value, ok := r.patients[patientID]
	if !ok {
		return nil, patient.ErrPatientNotFound
	}
	copied := value
	return &copied, nil
}

// registrationTestEnv 汇总被测 Service 与三个内存桩。
type registrationTestEnv struct {
	service  *Service
	repo     *registrationFakeRepo
	cards    *registrationFakeCards
	patients *registrationFakePatients
}

// completeCard 返回资料完整、属于 registrationTestPatientID 的就诊卡。
func completeCard() patient.Card {
	return patient.Card{
		ID:             registrationTestCardID,
		UserID:         registrationTestPatientID,
		Name:           "张三",
		Sex:            patient.SexMale,
		PID:            "110101199001011237",
		Tel:            "13800138000",
		Birthday:       "1990-01-01",
		MedicalHistory: []string{"高血压"},
		InsuranceType:  "社会基本医疗保险",
	}
}

// completeSnapshot 返回可挂号的时段快照：时段级剩余 2、计划级剩余 2。
func completeSnapshot() domainregistration.ScheduleSnapshot {
	return domainregistration.ScheduleSnapshot{
		ScheduleID:        registrationTestScheduleID,
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
		Amount:            registrationTestAmount,
	}
}

// newRegistrationTestEnv 构造被测环境：固定业务时刻与固定外部交易号。
func newRegistrationTestEnv(t *testing.T) *registrationTestEnv {
	t.Helper()

	card := completeCard()
	snapshot := completeSnapshot()
	repo := &registrationFakeRepo{snapshot: &snapshot}
	cards := &registrationFakeCards{
		byID: map[int64]patient.Card{
			registrationTestCardID:      card,
			registrationTestOtherCardID: {ID: registrationTestOtherCardID, UserID: 99},
		},
		byPatientID: map[int64]patient.Card{registrationTestPatientID: card},
	}
	patients := &registrationFakePatients{
		patients: map[int64]patient.Patient{
			registrationTestPatientID: {ID: registrationTestPatientID, Status: patient.StatusActive},
		},
	}

	service := NewService(repo, cards, patients, func() time.Time { return registrationTestNow })
	service.newTradeNo = func(time.Time) string { return registrationTestTradeNo }
	return &registrationTestEnv{service: service, repo: repo, cards: cards, patients: patients}
}

// patientActor 返回 realm=patient 的调用者（患者端主体固定为令牌中的当前患者）。
func patientActor() Actor {
	return Actor{
		Realm:     domainauth.RealmPatient,
		PatientID: registrationTestPatientID,
		UserID:    registrationTestPatientID,
	}
}

// misActor 返回 realm=mis 的调用者（管理端可代查代建任意就诊卡）。
func misActor() Actor {
	return Actor{Realm: domainauth.RealmMis, UserID: 1}
}

// assertServiceError 断言错误是携带指定 code 的 *ServiceError。
func assertServiceError(t *testing.T, err error, wantCode string) *ServiceError {
	t.Helper()
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) {
		t.Fatalf("错误 = %v (%T), want *ServiceError(code=%s)", err, err, wantCode)
	}
	if serviceErr.Code != wantCode {
		t.Errorf("code = %q, want %q（message=%q）", serviceErr.Code, wantCode, serviceErr.Message)
	}
	return serviceErr
}

// TestEligibilityReasons 覆盖契约 §6.1 的全部 reasons 词表取值：
// 资格不通过时返回 200 语义的领域结果（err == nil）而不是错误。
func TestEligibilityReasons(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(env *registrationTestEnv)
		want    []string
	}{
		{
			name: "就诊卡不存在",
			prepare: func(env *registrationTestEnv) {
				env.cards.findErr = patient.ErrCardNotFound
			},
			want: []string{domainregistration.ReasonCardInvalid},
		},
		{
			name: "患者端提交他人就诊卡",
			prepare: func(env *registrationTestEnv) {
				// 直接让 FindCardByID 返回他人的卡。
			},
			want: []string{domainregistration.ReasonCardInvalid},
		},
		{
			name: "就诊卡资料不完整",
			prepare: func(env *registrationTestEnv) {
				incomplete := completeCard()
				incomplete.Tel = ""
				env.cards.byID[registrationTestCardID] = incomplete
				env.cards.byPatientID[registrationTestPatientID] = incomplete
			},
			want: []string{domainregistration.ReasonProfileIncomplete},
		},
		{
			name: "患者账号已禁用",
			prepare: func(env *registrationTestEnv) {
				env.patients.patients[registrationTestPatientID] = patient.Patient{
					ID: registrationTestPatientID, Status: patient.StatusDisabled,
				}
			},
			want: []string{domainregistration.ReasonPatientDisabled},
		},
		{
			name: "时段不存在",
			prepare: func(env *registrationTestEnv) {
				env.repo.snapshotErr = domainregistration.ErrScheduleNotFound
			},
			want: []string{domainregistration.ReasonScheduleNotFound},
		},
		{
			name: "医生不在诊",
			prepare: func(env *registrationTestEnv) {
				env.repo.snapshot.DoctorActive = false
			},
			want: []string{domainregistration.ReasonScheduleNotFound},
		},
		{
			name: "时段已开始（业务当天）",
			prepare: func(env *registrationTestEnv) {
				env.repo.snapshot.Date = "2026-09-10"
			},
			want: []string{domainregistration.ReasonScheduleStarted},
		},
		{
			name: "时段已过期",
			prepare: func(env *registrationTestEnv) {
				env.repo.snapshot.Date = "2026-09-01"
			},
			want: []string{domainregistration.ReasonScheduleStarted},
		},
		{
			name: "号源已满",
			prepare: func(env *registrationTestEnv) {
				env.repo.snapshot.SlotUsed = env.repo.snapshot.SlotMaximum
			},
			want: []string{domainregistration.ReasonSoldOut},
		},
		{
			name: "同一身份证号已占用该时段",
			prepare: func(env *registrationTestEnv) {
				env.repo.occupied = true
			},
			want: []string{domainregistration.ReasonDuplicate},
		},
		{
			name: "多个原因按固定顺序追加",
			prepare: func(env *registrationTestEnv) {
				incomplete := completeCard()
				incomplete.Birthday = ""
				env.cards.byID[registrationTestCardID] = incomplete
				env.cards.byPatientID[registrationTestPatientID] = incomplete
				env.patients.patients[registrationTestPatientID] = patient.Patient{
					ID: registrationTestPatientID, Status: patient.StatusDisabled,
				}
				env.repo.snapshot.Date = "2026-09-10"
				env.repo.snapshot.SlotUsed = env.repo.snapshot.SlotMaximum
				env.repo.occupied = true
			},
			want: []string{
				domainregistration.ReasonProfileIncomplete,
				domainregistration.ReasonPatientDisabled,
				domainregistration.ReasonScheduleStarted,
				domainregistration.ReasonSoldOut,
				domainregistration.ReasonDuplicate,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRegistrationTestEnv(t)
			tc.prepare(env)

			cardID := int64(registrationTestCardID)
			if tc.name == "患者端提交他人就诊卡" {
				cardID = registrationTestOtherCardID
			}
			result, err := env.service.Eligibility(
				context.Background(), patientActor(), cardID, registrationTestScheduleID)
			if err != nil {
				t.Fatalf("资格不通过不得返回错误，got %v", err)
			}
			if result == nil {
				t.Fatal("结果不得为 nil")
			}
			if result.Eligible {
				t.Errorf("eligible = true, want false；reasons=%#v", result.Reasons)
			}
			if !reflect.DeepEqual(result.Reasons, tc.want) {
				t.Errorf("reasons = %#v, want %#v", result.Reasons, tc.want)
			}
			if result.Reasons == nil {
				t.Error("reasons 不得为 nil（响应会输出 null）")
			}
		})
	}
}

// TestEligibilityAllows 资格通过时返回剩余号源与应付金额，并按身份证号做占用判重。
func TestEligibilityAllows(t *testing.T) {
	env := newRegistrationTestEnv(t)

	result, err := env.service.Eligibility(
		context.Background(), patientActor(), registrationTestCardID, registrationTestScheduleID)
	if err != nil {
		t.Fatalf("Eligibility 返回错误：%v", err)
	}
	if !result.Eligible {
		t.Errorf("eligible = false, want true；reasons=%#v", result.Reasons)
	}
	if len(result.Reasons) != 0 {
		t.Errorf("reasons = %#v, want 空", result.Reasons)
	}
	if result.Remaining != 2 {
		t.Errorf("remaining = %d, want 2（时段级 3-1 与计划级 10-8 取较小值）", result.Remaining)
	}
	if result.Amount == nil || *result.Amount != registrationTestAmount {
		t.Errorf("amount = %v, want %q", result.Amount, registrationTestAmount)
	}
	if env.repo.lastOccupiedPID != completeCard().PID {
		t.Errorf("判重入参 pid = %q, want %q", env.repo.lastOccupiedPID, completeCard().PID)
	}
	if env.repo.lastOccupiedScheduleID != registrationTestScheduleID {
		t.Errorf("判重入参 scheduleId = %d, want %d", env.repo.lastOccupiedScheduleID, registrationTestScheduleID)
	}
}

// TestEligibilityUnavailableWhenScheduleMissing 时段不存在时无法给出金额与余量：
// remaining 输出 0、amount 输出 null（契约 §12.4 的 SCHEDULE_NOT_FOUND 样例）。
func TestEligibilityUnavailableWhenScheduleMissing(t *testing.T) {
	env := newRegistrationTestEnv(t)
	env.repo.snapshotErr = domainregistration.ErrScheduleNotFound

	result, err := env.service.Eligibility(
		context.Background(), patientActor(), registrationTestCardID, registrationTestScheduleID)
	if err != nil {
		t.Fatalf("Eligibility 返回错误：%v", err)
	}
	if result.Remaining != 0 {
		t.Errorf("remaining = %d, want 0", result.Remaining)
	}
	if result.Amount != nil {
		t.Errorf("amount = %v, want nil", *result.Amount)
	}
}

// TestEligibilityValidation 就诊卡与时段编号必须是正整数（请求层已校验，用例层兜底）。
func TestEligibilityValidation(t *testing.T) {
	env := newRegistrationTestEnv(t)

	if _, err := env.service.Eligibility(context.Background(), patientActor(), 0, registrationTestScheduleID); err == nil {
		t.Error("patientCardId=0 应返回参数错误")
	} else {
		assertServiceError(t, err, CodeValidationFailed)
	}
	if _, err := env.service.Eligibility(context.Background(), patientActor(), registrationTestCardID, 0); err == nil {
		t.Error("scheduleId=0 应返回参数错误")
	} else {
		assertServiceError(t, err, CodeValidationFailed)
	}
	if env.repo.snapshotCalls != 0 {
		t.Errorf("参数校验失败不得触达仓储，FindScheduleSnapshot 调用 %d 次", env.repo.snapshotCalls)
	}
}

// TestEligibilityDependencyUnavailable 仓储故障必须归类为 DEPENDENCY_UNAVAILABLE（502），
// 不能伪装成「资格不通过」或「资源不存在」。
func TestEligibilityDependencyUnavailable(t *testing.T) {
	boom := errors.New("postgres is down")
	cases := []struct {
		name    string
		prepare func(env *registrationTestEnv)
	}{
		{"就诊卡读取失败", func(env *registrationTestEnv) { env.cards.findErr = boom }},
		{"患者账号读取失败", func(env *registrationTestEnv) { env.patients.findErr = boom }},
		{"时段快照读取失败", func(env *registrationTestEnv) { env.repo.snapshotErr = boom }},
		{"占用判定失败", func(env *registrationTestEnv) { env.repo.occupiedErr = boom }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRegistrationTestEnv(t)
			tc.prepare(env)

			result, err := env.service.Eligibility(
				context.Background(), patientActor(), registrationTestCardID, registrationTestScheduleID)
			if result != nil {
				t.Errorf("依赖故障时不应返回资格结果：%+v", result)
			}
			if !errors.Is(err, ErrDependencyUnavailable) {
				t.Fatalf("错误 = %v, want ErrDependencyUnavailable", err)
			}
		})
	}
}

// int64Ptr 返回 int64 取值的指针，用于构造可选的入参字段。
func int64Ptr(value int64) *int64 { return &value }

// validPaymentMethod 是契约 §6.2 唯一允许的支付方式取值（与 §12.4 建单样例一致）。
// 其它取值（含历史示例中的 WECHAT）必须返回 422「暂不支持该支付方式」，
// 白名单行为由 TestCreatePaymentMethodContract 逐条断言。
const validPaymentMethod = PaymentMethodAlipay

// TestCreateSuccess 建单成功：服务端从快照推导关联与金额，交易号由服务端生成，
// 客户端只能提交就诊卡与时段（契约 §6.2）。
func TestCreateSuccess(t *testing.T) {
	env := newRegistrationTestEnv(t)

	created, err := env.service.Create(context.Background(), patientActor(), CreateInput{
		PatientCardID: int64Ptr(registrationTestCardID),
		ScheduleID:    registrationTestScheduleID,
		PaymentMethod: validPaymentMethod,
	})
	if err != nil {
		t.Fatalf("Create 返回错误：%v", err)
	}
	if created == nil {
		t.Fatal("created 不得为 nil")
	}
	if created.ID != 1001 {
		t.Errorf("id = %d, want 1001", created.ID)
	}
	if created.PatientCardID != registrationTestCardID {
		t.Errorf("patientCardId = %d, want %d", created.PatientCardID, registrationTestCardID)
	}
	if created.ScheduleID != registrationTestScheduleID {
		t.Errorf("scheduleId = %d, want %d", created.ScheduleID, registrationTestScheduleID)
	}
	if created.PaymentStatus != domainregistration.PaymentStatusUnpaid {
		t.Errorf("paymentStatus = %q, want %q（插入时固定为未付款）",
			created.PaymentStatus, domainregistration.PaymentStatusUnpaid)
	}
	if created.OutTradeNo != registrationTestTradeNo {
		t.Errorf("outTradeNo = %q, want 服务端生成的 %q", created.OutTradeNo, registrationTestTradeNo)
	}
	if err := domainregistration.ValidateOutTradeNo(created.OutTradeNo); err != nil {
		t.Errorf("生成的交易号不可落库：%v", err)
	}
	if env.repo.createCalls != 1 {
		t.Errorf("CreateRegistration 调用次数 = %d, want 1", env.repo.createCalls)
	}
	if env.repo.lastCreateInput.PID != completeCard().PID {
		t.Errorf("落库 pid = %q, want %q（取自已校验归属的就诊卡）",
			env.repo.lastCreateInput.PID, completeCard().PID)
	}
	if !env.repo.lastCreateInput.Now.Equal(registrationTestNow) {
		t.Errorf("落库 now = %s, want %s", env.repo.lastCreateInput.Now, registrationTestNow)
	}
}

// TestCreateDerivesCardForPatientAccount 患者端可省略 patientCardId（每个账号最多一张卡），
// 由服务端按当前患者推导；账号无卡时按就诊卡不存在处理（契约 §6.2、§8.2 第 6 步）。
func TestCreateDerivesCardForPatientAccount(t *testing.T) {
	t.Run("省略 patientCardId 时按当前患者取卡", func(t *testing.T) {
		env := newRegistrationTestEnv(t)

		created, err := env.service.Create(context.Background(), patientActor(), CreateInput{
			ScheduleID:    registrationTestScheduleID,
			PaymentMethod: validPaymentMethod,
		})
		if err != nil {
			t.Fatalf("Create 返回错误：%v", err)
		}
		if env.cards.findByPatientIDCalls != 1 {
			t.Errorf("FindCardByPatientID 调用次数 = %d, want 1", env.cards.findByPatientIDCalls)
		}
		if created.PatientCardID != registrationTestCardID {
			t.Errorf("patientCardId = %d, want 推导出的 %d", created.PatientCardID, registrationTestCardID)
		}
	})

	t.Run("当前账号尚未建卡", func(t *testing.T) {
		env := newRegistrationTestEnv(t)
		delete(env.cards.byPatientID, registrationTestPatientID)

		_, err := env.service.Create(context.Background(), patientActor(), CreateInput{
			ScheduleID:    registrationTestScheduleID,
			PaymentMethod: validPaymentMethod,
		})
		assertServiceError(t, err, CodeCardNotFound)
		if env.repo.createCalls != 0 {
			t.Errorf("就诊卡不可用时不得建单，CreateRegistration 调用 %d 次", env.repo.createCalls)
		}
	})
}

// TestCreateRejections 覆盖建单前置校验的 404/409/422 归类（契约 §6.2、§10）。
func TestCreateRejections(t *testing.T) {
	cases := []struct {
		name     string
		prepare  func(env *registrationTestEnv)
		actor    Actor
		input    CreateInput
		wantCode string
	}{
		{
			name:     "患者端提交他人就诊卡按不存在处理",
			actor:    patientActor(),
			input:    CreateInput{PatientCardID: int64Ptr(registrationTestOtherCardID), ScheduleID: registrationTestScheduleID, PaymentMethod: validPaymentMethod},
			wantCode: CodeCardNotFound,
		},
		{
			name:     "提交的就诊卡不存在",
			actor:    patientActor(),
			input:    CreateInput{PatientCardID: int64Ptr(9999), ScheduleID: registrationTestScheduleID, PaymentMethod: validPaymentMethod},
			wantCode: CodeCardNotFound,
		},
		{
			name: "患者账号已被禁用",
			prepare: func(env *registrationTestEnv) {
				env.patients.patients[registrationTestPatientID] = patient.Patient{
					ID: registrationTestPatientID, Status: patient.StatusDisabled,
				}
			},
			actor:    patientActor(),
			input:    CreateInput{PatientCardID: int64Ptr(registrationTestCardID), ScheduleID: registrationTestScheduleID, PaymentMethod: validPaymentMethod},
			wantCode: CodeValidationFailed,
		},
		{
			name: "就诊卡资料不完整",
			prepare: func(env *registrationTestEnv) {
				incomplete := completeCard()
				incomplete.MedicalHistory = nil
				env.cards.byID[registrationTestCardID] = incomplete
				env.cards.byPatientID[registrationTestPatientID] = incomplete
			},
			actor:    patientActor(),
			input:    CreateInput{PatientCardID: int64Ptr(registrationTestCardID), ScheduleID: registrationTestScheduleID, PaymentMethod: validPaymentMethod},
			wantCode: CodeValidationFailed,
		},
		{
			name: "持卡账号不存在属脏数据",
			prepare: func(env *registrationTestEnv) {
				env.patients.findErr = patient.ErrPatientNotFound
			},
			actor:    patientActor(),
			input:    CreateInput{PatientCardID: int64Ptr(registrationTestCardID), ScheduleID: registrationTestScheduleID, PaymentMethod: validPaymentMethod},
			wantCode: CodeCardNotFound,
		},
		{
			name:     "管理端代建缺少 patientCardId",
			actor:    misActor(),
			input:    CreateInput{ScheduleID: registrationTestScheduleID, PaymentMethod: validPaymentMethod},
			wantCode: CodeValidationFailed,
		},
		{
			name:     "scheduleId 非正整数",
			actor:    patientActor(),
			input:    CreateInput{PatientCardID: int64Ptr(registrationTestCardID), ScheduleID: 0, PaymentMethod: validPaymentMethod},
			wantCode: CodeValidationFailed,
		},
		{
			name: "时段不存在",
			prepare: func(env *registrationTestEnv) {
				env.repo.snapshotErr = domainregistration.ErrScheduleNotFound
			},
			actor:    patientActor(),
			input:    CreateInput{PatientCardID: int64Ptr(registrationTestCardID), ScheduleID: registrationTestScheduleID, PaymentMethod: validPaymentMethod},
			wantCode: CodeSlotNotFound,
		},
		{
			name: "医生不在诊按时段不存在收敛",
			prepare: func(env *registrationTestEnv) {
				env.repo.snapshot.DoctorActive = false
			},
			actor:    patientActor(),
			input:    CreateInput{PatientCardID: int64Ptr(registrationTestCardID), ScheduleID: registrationTestScheduleID, PaymentMethod: validPaymentMethod},
			wantCode: CodeSlotNotFound,
		},
		{
			name: "时段已开始按时段不存在收敛",
			prepare: func(env *registrationTestEnv) {
				env.repo.snapshot.Date = "2026-09-10"
			},
			actor:    patientActor(),
			input:    CreateInput{PatientCardID: int64Ptr(registrationTestCardID), ScheduleID: registrationTestScheduleID, PaymentMethod: validPaymentMethod},
			wantCode: CodeSlotNotFound,
		},
		{
			name: "号源已满",
			prepare: func(env *registrationTestEnv) {
				env.repo.snapshot.SlotUsed = env.repo.snapshot.SlotMaximum
			},
			actor:    patientActor(),
			input:    CreateInput{PatientCardID: int64Ptr(registrationTestCardID), ScheduleID: registrationTestScheduleID, PaymentMethod: validPaymentMethod},
			wantCode: CodeSlotSoldOut,
		},
		{
			name: "同一身份证号重复挂号",
			prepare: func(env *registrationTestEnv) {
				env.repo.occupied = true
			},
			actor:    patientActor(),
			input:    CreateInput{PatientCardID: int64Ptr(registrationTestCardID), ScheduleID: registrationTestScheduleID, PaymentMethod: validPaymentMethod},
			wantCode: CodeDuplicate,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRegistrationTestEnv(t)
			if tc.prepare != nil {
				tc.prepare(env)
			}

			created, err := env.service.Create(context.Background(), tc.actor, tc.input)
			if err == nil {
				t.Fatalf("期望错误 code=%s，实际建单成功：%+v", tc.wantCode, created)
			}
			assertServiceError(t, err, tc.wantCode)
			if created != nil {
				t.Errorf("失败时不得返回挂号记录：%+v", created)
			}
			if env.repo.createCalls != 0 {
				t.Errorf("前置校验失败不得建单，CreateRegistration 调用 %d 次", env.repo.createCalls)
			}
		})
	}
}

// TestCreateSoldOutDetails 号源不足必须携带契约 §12.4 的 details：{scheduleId, remaining}。
func TestCreateSoldOutDetails(t *testing.T) {
	env := newRegistrationTestEnv(t)
	env.repo.snapshot.SlotUsed = env.repo.snapshot.SlotMaximum

	_, err := env.service.Create(context.Background(), patientActor(), CreateInput{
		PatientCardID: int64Ptr(registrationTestCardID),
		ScheduleID:    registrationTestScheduleID,
		PaymentMethod: validPaymentMethod,
	})
	serviceErr := assertServiceError(t, err, CodeSlotSoldOut)
	if got := serviceErr.Details["scheduleId"]; got != registrationTestScheduleID {
		t.Errorf("details.scheduleId = %v, want %d", got, registrationTestScheduleID)
	}
	if got := serviceErr.Details["remaining"]; got != int16(0) {
		t.Errorf("details.remaining = %v, want 0", got)
	}
}

// TestCreateRepositoryErrorMapping 事务内判定是最终判定：仓储返回的领域错误
// 必须映射为契约错误码（409/404/502），不能冒泡成 500（契约 §10、§11）。
func TestCreateRepositoryErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		createErr  error
		wantCode   string
		wantDetail map[string]any
	}{
		{
			name:       "事务内号源不足带实时余量",
			createErr:  &domainregistration.SlotSoldOutError{ScheduleID: registrationTestScheduleID, Remaining: 3},
			wantCode:   CodeSlotSoldOut,
			wantDetail: map[string]any{"scheduleId": int64(registrationTestScheduleID), "remaining": int16(3)},
		},
		{
			name:       "事务内号源不足但未带时段编号时回退请求参数",
			createErr:  &domainregistration.SlotSoldOutError{Remaining: 0},
			wantCode:   CodeSlotSoldOut,
			wantDetail: map[string]any{"scheduleId": int64(registrationTestScheduleID), "remaining": int16(0)},
		},
		{name: "事务内判定重复挂号", createErr: domainregistration.ErrDuplicate, wantCode: CodeDuplicate},
		{name: "事务内时段不存在", createErr: domainregistration.ErrScheduleNotFound, wantCode: CodeSlotNotFound},
		{name: "事务内时段已开始", createErr: domainregistration.ErrScheduleStarted, wantCode: CodeSlotNotFound},
		{name: "事务内医生不在诊", createErr: domainregistration.ErrDoctorInactive, wantCode: CodeSlotNotFound},
		{name: "事务内就诊卡失效", createErr: domainregistration.ErrCardNotFound, wantCode: CodeCardNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRegistrationTestEnv(t)
			env.repo.createErr = tc.createErr

			created, err := env.service.Create(context.Background(), patientActor(), CreateInput{
				PatientCardID: int64Ptr(registrationTestCardID),
				ScheduleID:    registrationTestScheduleID,
				PaymentMethod: validPaymentMethod,
			})
			if created != nil {
				t.Errorf("失败时不得返回挂号记录：%+v", created)
			}
			serviceErr := assertServiceError(t, err, tc.wantCode)
			for key, want := range tc.wantDetail {
				if got := serviceErr.Details[key]; got != want {
					t.Errorf("details[%q] = %#v, want %#v", key, got, want)
				}
			}
			if env.repo.createCalls != 1 {
				t.Errorf("CreateRegistration 调用次数 = %d, want 1", env.repo.createCalls)
			}
		})
	}
}

// TestCreateDependencyUnavailable 非领域错误（连接失败、语句失败等）必须归类为
// DEPENDENCY_UNAVAILABLE，不能伪装成「号源已满」或「挂号不存在」。
func TestCreateDependencyUnavailable(t *testing.T) {
	boom := errors.New("postgres is down")
	cases := []struct {
		name    string
		prepare func(env *registrationTestEnv)
	}{
		{"时段快照读取失败", func(env *registrationTestEnv) { env.repo.snapshotErr = boom }},
		{"占用判定失败", func(env *registrationTestEnv) { env.repo.occupiedErr = boom }},
		{"建单事务失败", func(env *registrationTestEnv) { env.repo.createErr = boom }},
		{"就诊卡读取失败", func(env *registrationTestEnv) { env.cards.findErr = boom }},
		{"患者账号读取失败", func(env *registrationTestEnv) { env.patients.findErr = boom }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRegistrationTestEnv(t)
			tc.prepare(env)

			_, err := env.service.Create(context.Background(), patientActor(), CreateInput{
				PatientCardID: int64Ptr(registrationTestCardID),
				ScheduleID:    registrationTestScheduleID,
				PaymentMethod: validPaymentMethod,
			})
			if !errors.Is(err, ErrDependencyUnavailable) {
				t.Fatalf("错误 = %v, want ErrDependencyUnavailable", err)
			}
		})
	}
}

// TestCreatePaymentMethodContract 支付方式白名单必须与契约 §6.2 一致：
// 第一阶段只接受 ALIPAY；其他取值（含历史示例中的 WECHAT）返回 422 REQUEST_VALIDATION_FAILED
// 且文案为「暂不支持该支付方式」；该字段只用于校验渠道，不落库。
// 判定采用精确匹配（不做大小写/空白归一化）："alipay"、" ALIPAY " 之类的变体一律 422。
//
// 契约 §12.4 建单样例原文为
// `{ "patientCardId": 10, "scheduleId": 12, "paymentMethod": "ALIPAY" }`，
// 本包常量 registrationTestCardID=10、registrationTestScheduleID=12 与之一一对应，
// 所以「ALIPAY 必须被接受」子测试同时就是该样例的绑定证据。
// §12.4 另有一条支付域样例 { "registrationId": 1001, "method": "WECHAT" }，
// 属于支付切片（POST /api/v1/payments）范围，不在本包断言内。
func TestCreatePaymentMethodContract(t *testing.T) {
	t.Run("ALIPAY 按契约必须被接受", func(t *testing.T) {
		env := newRegistrationTestEnv(t)
		created, err := env.service.Create(context.Background(), patientActor(), CreateInput{
			PatientCardID: int64Ptr(registrationTestCardID),
			ScheduleID:    registrationTestScheduleID,
			PaymentMethod: "ALIPAY",
		})
		if err != nil {
			t.Fatalf("契约 §6.2 要求 ALIPAY 被接受，实际返回错误：%v", err)
		}
		if created == nil || env.repo.createCalls != 1 {
			t.Errorf("ALIPAY 建单应落库一次，created=%+v createCalls=%d", created, env.repo.createCalls)
		}
		// §12.4 样例的 patientCardId=10 / scheduleId=12 与测试常量一致，逐字段核对。
		if created != nil && (created.PatientCardID != registrationTestCardID ||
			created.ScheduleID != registrationTestScheduleID) {
			t.Errorf("建单结果与 §12.4 样例不一致：%+v", created)
		}
	})

	t.Run("WECHAT 按契约必须被拒绝为 422", func(t *testing.T) {
		env := newRegistrationTestEnv(t)
		_, err := env.service.Create(context.Background(), patientActor(), CreateInput{
			PatientCardID: int64Ptr(registrationTestCardID),
			ScheduleID:    registrationTestScheduleID,
			PaymentMethod: "WECHAT",
		})
		serviceErr := assertServiceError(t, err, CodeValidationFailed)
		if serviceErr.Message != "暂不支持该支付方式" {
			t.Errorf("message = %q, want 暂不支持该支付方式", serviceErr.Message)
		}
	})

	t.Run("未知取值返回 422 与契约文案", func(t *testing.T) {
		for _, method := range []string{"CASH", "ALIPAYX", "信用卡"} {
			env := newRegistrationTestEnv(t)
			_, err := env.service.Create(context.Background(), patientActor(), CreateInput{
				PatientCardID: int64Ptr(registrationTestCardID),
				ScheduleID:    registrationTestScheduleID,
				PaymentMethod: method,
			})
			serviceErr := assertServiceError(t, err, CodeValidationFailed)
			if serviceErr.Message != "暂不支持该支付方式" {
				t.Errorf("paymentMethod=%q 的 message = %q, want 暂不支持该支付方式", method, serviceErr.Message)
			}
			if env.repo.createCalls != 0 {
				t.Errorf("paymentMethod=%q 非法时不建单，CreateRegistration 调用 %d 次", method, env.repo.createCalls)
			}
		}
	})

	t.Run("缺少 paymentMethod 为必填 422", func(t *testing.T) {
		// 字段缺省（零值）与显式空串走同一个空值分支：都返回「paymentMethod 必填」且不建单。
		cases := []struct {
			name  string
			input CreateInput
		}{
			{
				name: "字段缺省",
				input: CreateInput{
					PatientCardID: int64Ptr(registrationTestCardID),
					ScheduleID:    registrationTestScheduleID,
				},
			},
			{
				name: "显式空串",
				input: CreateInput{
					PatientCardID: int64Ptr(registrationTestCardID),
					ScheduleID:    registrationTestScheduleID,
					PaymentMethod: "",
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				env := newRegistrationTestEnv(t)
				_, err := env.service.Create(context.Background(), patientActor(), tc.input)
				serviceErr := assertServiceError(t, err, CodeValidationFailed)
				if serviceErr.Message != "paymentMethod 必填" {
					t.Errorf("message = %q, want paymentMethod 必填", serviceErr.Message)
				}
				if env.repo.createCalls != 0 {
					t.Errorf("paymentMethod 缺省时不建单，CreateRegistration 调用 %d 次", env.repo.createCalls)
				}
			})
		}
	})

	t.Run("大小写与空白变体不得被归一化放行", func(t *testing.T) {
		// 契约只接受字面量 ALIPAY；实现与 paymentStatus/sort/order 等枚举入参一致采用精确匹配。
		// 下列取值在「先 ToUpper/TrimSpace 再比较」的写法下会被静默放行，因此必须全部 422，
		// 并且走「暂不支持该支付方式」分支而不是「paymentMethod 必填」分支。
		// 其中纯空白取值用于固定「不做 TrimSpace」，避免空值判定被空白绕过。
		for _, method := range []string{"alipay", "Alipay", "ALIPAY ", " ALIPAY", " ALIPAY ", "   "} {
			env := newRegistrationTestEnv(t)
			_, err := env.service.Create(context.Background(), patientActor(), CreateInput{
				PatientCardID: int64Ptr(registrationTestCardID),
				ScheduleID:    registrationTestScheduleID,
				PaymentMethod: method,
			})
			serviceErr := assertServiceError(t, err, CodeValidationFailed)
			if serviceErr.Message != "暂不支持该支付方式" {
				t.Errorf("paymentMethod=%q 的 message = %q, want 暂不支持该支付方式", method, serviceErr.Message)
			}
			if env.repo.createCalls != 0 {
				t.Errorf("paymentMethod=%q 非契约取值时不建单，CreateRegistration 调用 %d 次", method, env.repo.createCalls)
			}
		}
	})
}

// TestListPatientOwnershipFilter 患者端强制归属过滤：ownerPatientID 必须写进筛选条件，
// 并且 page/pageSize 正确换算为 offset/limit（契约 §1.4、§6.3）。
func TestListPatientOwnershipFilter(t *testing.T) {
	env := newRegistrationTestEnv(t)
	env.repo.items = []domainregistration.Registration{{ID: 1001, PatientCardID: registrationTestCardID}}
	env.repo.total = 1

	page, err := env.service.List(context.Background(), patientActor(), ListQuery{
		Page:     2,
		PageSize: 20,
		Sort:     domainregistration.SortCreateDate,
		Order:    "desc",
	})
	if err != nil {
		t.Fatalf("List 返回错误：%v", err)
	}
	if page.Total != 1 || page.Page != 2 || page.PageSize != 20 {
		t.Errorf("分页元数据 = %+v, want total=1 page=2 pageSize=20", page)
	}
	if len(page.Items) != 1 {
		t.Errorf("items 长度 = %d, want 1", len(page.Items))
	}
	if env.repo.lastFilter.OwnerPatientID == nil {
		t.Fatal("患者端必须写入归属过滤条件 ownerPatientID")
	}
	if got := *env.repo.lastFilter.OwnerPatientID; got != registrationTestPatientID {
		t.Errorf("ownerPatientID = %d, want %d", got, registrationTestPatientID)
	}
	if env.repo.lastOffset != 20 || env.repo.lastLimit != 20 {
		t.Errorf("offset/limit = %d/%d, want 20/20", env.repo.lastOffset, env.repo.lastLimit)
	}
	if env.repo.lastFilter.Sort != domainregistration.SortCreateDate || env.repo.lastFilter.Order != "desc" {
		t.Errorf("排序 = (%q,%q), want (createDate,desc)", env.repo.lastFilter.Sort, env.repo.lastFilter.Order)
	}
}

// TestListOwnershipAndFilterPassThrough 患者端提交本人卡可继续按卡过滤，他人卡按不存在处理；
// 管理端不带归属条件，其余筛选条件原样下发（契约 §1.2、§6.3）。
func TestListOwnershipAndFilterPassThrough(t *testing.T) {
	t.Run("患者端提交本人卡", func(t *testing.T) {
		env := newRegistrationTestEnv(t)
		ownCard := int64(registrationTestCardID)

		if _, err := env.service.List(context.Background(), patientActor(), ListQuery{
			PatientCardID: &ownCard, Page: 1, PageSize: 20,
		}); err != nil {
			t.Fatalf("List 返回错误：%v", err)
		}
		if env.repo.lastFilter.PatientCardID == nil || *env.repo.lastFilter.PatientCardID != ownCard {
			t.Errorf("patientCardId 过滤未下发：%+v", env.repo.lastFilter)
		}
		if env.repo.lastFilter.OwnerPatientID == nil {
			t.Error("患者端必须同时带归属过滤")
		}
	})

	t.Run("患者端提交他人卡按不存在处理", func(t *testing.T) {
		env := newRegistrationTestEnv(t)
		otherCard := int64(registrationTestOtherCardID)

		_, err := env.service.List(context.Background(), patientActor(), ListQuery{
			PatientCardID: &otherCard, Page: 1, PageSize: 20,
		})
		assertServiceError(t, err, CodeCardNotFound)
		if env.repo.listCalls != 0 {
			t.Errorf("越权查询不得触达仓储，ListRegistrations 调用 %d 次", env.repo.listCalls)
		}
	})

	t.Run("管理端不带归属条件且筛选条件原样下发", func(t *testing.T) {
		env := newRegistrationTestEnv(t)
		doctor := int64(16)
		status := domainregistration.PaymentCodePaid

		if _, err := env.service.List(context.Background(), misActor(), ListQuery{
			DoctorID:      &doctor,
			FromDate:      "2026-09-01",
			ToDate:        "2026-09-30",
			PaymentStatus: &status,
			Page:          1,
			PageSize:      20,
			Sort:          domainregistration.SortDate,
			Order:         "asc",
		}); err != nil {
			t.Fatalf("List 返回错误：%v", err)
		}
		if env.repo.lastFilter.OwnerPatientID != nil {
			t.Errorf("管理端不应带归属过滤：%+v", env.repo.lastFilter.OwnerPatientID)
		}
		if env.repo.lastFilter.DoctorID == nil || *env.repo.lastFilter.DoctorID != doctor {
			t.Errorf("doctorId 过滤未下发：%+v", env.repo.lastFilter)
		}
		if env.repo.lastFilter.FromDate != "2026-09-01" || env.repo.lastFilter.ToDate != "2026-09-30" {
			t.Errorf("日期范围未下发：%+v", env.repo.lastFilter)
		}
		if env.repo.lastFilter.PaymentStatus == nil || *env.repo.lastFilter.PaymentStatus != status {
			t.Errorf("paymentStatus 过滤未下发：%+v", env.repo.lastFilter)
		}
		if env.repo.lastFilter.Sort != domainregistration.SortDate || env.repo.lastFilter.Order != "asc" {
			t.Errorf("排序 = (%q,%q), want (date,asc)", env.repo.lastFilter.Sort, env.repo.lastFilter.Order)
		}
	})
}

// TestListNormalizesEmptyItems 空列表必须返回空切片而不是 nil（契约 §1.4：items: []）。
func TestListNormalizesEmptyItems(t *testing.T) {
	env := newRegistrationTestEnv(t)
	env.repo.items = nil

	page, err := env.service.List(context.Background(), patientActor(), ListQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("List 返回错误：%v", err)
	}
	if page.Items == nil {
		t.Fatal("items 不得为 nil（响应会输出 null）")
	}
	if len(page.Items) != 0 {
		t.Errorf("items = %#v, want 空切片", page.Items)
	}
}

// TestListValidation 分页、日期范围与排序白名单的用例层兜底校验（契约 §1.4）。
func TestListValidation(t *testing.T) {
	cases := []struct {
		name  string
		query ListQuery
	}{
		{"page 为 0", ListQuery{Page: 0, PageSize: 20}},
		{"page 为负", ListQuery{Page: -1, PageSize: 20}},
		{"page 超过上限", ListQuery{Page: 100001, PageSize: 20}},
		{"pageSize 为 0", ListQuery{Page: 1, PageSize: 0}},
		{"pageSize 超过上限", ListQuery{Page: 1, PageSize: 101}},
		{"fromDate 晚于 toDate", ListQuery{Page: 1, PageSize: 20, FromDate: "2026-09-30", ToDate: "2026-09-01"}},
		{"sort 不在白名单", ListQuery{Page: 1, PageSize: 20, Sort: "amount"}},
		{"order 不在白名单", ListQuery{Page: 1, PageSize: 20, Order: "random"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRegistrationTestEnv(t)
			_, err := env.service.List(context.Background(), misActor(), tc.query)
			assertServiceError(t, err, CodeValidationFailed)
			if env.repo.listCalls != 0 {
				t.Errorf("参数非法不得触达仓储，ListRegistrations 调用 %d 次", env.repo.listCalls)
			}
		})
	}
}

// TestListDependencyUnavailable 列表仓储故障归类为 DEPENDENCY_UNAVAILABLE。
func TestListDependencyUnavailable(t *testing.T) {
	env := newRegistrationTestEnv(t)
	env.repo.listErr = errors.New("postgres is down")

	_, err := env.service.List(context.Background(), misActor(), ListQuery{Page: 1, PageSize: 20})
	if !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("错误 = %v, want ErrDependencyUnavailable", err)
	}
}

// TestDetailOwnership 详情必须把归属条件交给仓储：患者端传 ownerPatientID，
// 管理端传 0（不限归属），越权与不存在统一 404（契约 §1.2、§6.4）。
func TestDetailOwnership(t *testing.T) {
	env := newRegistrationTestEnv(t)
	env.repo.detail = &domainregistration.Detail{ID: 1001, PatientCardID: registrationTestCardID}

	if _, err := env.service.Detail(context.Background(), patientActor(), 1001); err != nil {
		t.Fatalf("Detail 返回错误：%v", err)
	}
	if env.repo.lastOwnerPatientID != registrationTestPatientID {
		t.Errorf("患者端 ownerPatientID = %d, want %d",
			env.repo.lastOwnerPatientID, registrationTestPatientID)
	}

	if _, err := env.service.Detail(context.Background(), misActor(), 1001); err != nil {
		t.Fatalf("Detail 返回错误：%v", err)
	}
	if env.repo.lastOwnerPatientID != 0 {
		t.Errorf("管理端 ownerPatientID = %d, want 0（不限定归属）", env.repo.lastOwnerPatientID)
	}
	if env.repo.lastDetailID != 1001 {
		t.Errorf("registrationId = %d, want 1001", env.repo.lastDetailID)
	}
}

// TestDetailNotFoundAndFailure 仓储返回 REGISTRATION_NOT_FOUND（含越权隐藏）映射 404，
// 其他故障映射 DEPENDENCY_UNAVAILABLE，编号非正数映射 422。
func TestDetailNotFoundAndFailure(t *testing.T) {
	t.Run("越权或不存在统一 404", func(t *testing.T) {
		env := newRegistrationTestEnv(t)
		env.repo.detailErr = domainregistration.ErrRegistrationNotFound

		_, err := env.service.Detail(context.Background(), patientActor(), 1001)
		assertServiceError(t, err, CodeRegistrationNotFound)
	})

	t.Run("仓储故障归类为依赖不可用", func(t *testing.T) {
		env := newRegistrationTestEnv(t)
		env.repo.detailErr = errors.New("postgres is down")

		_, err := env.service.Detail(context.Background(), misActor(), 1001)
		if !errors.Is(err, ErrDependencyUnavailable) {
			t.Fatalf("错误 = %v, want ErrDependencyUnavailable", err)
		}
	})

	t.Run("编号非正整数为参数错误", func(t *testing.T) {
		env := newRegistrationTestEnv(t)
		_, err := env.service.Detail(context.Background(), misActor(), 0)
		assertServiceError(t, err, CodeValidationFailed)
		if env.repo.detailCalls != 0 {
			t.Errorf("参数非法不得触达仓储，FindRegistrationDetail 调用 %d 次", env.repo.detailCalls)
		}
	})
}

// TestServiceRequiresDependencies 依赖未配置属于启动期配置错误：返回普通错误，
// 不能伪装成业务错误码（否则客户端会得到误导性的 404/409）。
func TestServiceRequiresDependencies(t *testing.T) {
	env := newRegistrationTestEnv(t)
	service := NewService(nil, env.cards, env.patients, func() time.Time { return registrationTestNow })

	_, err := service.Create(context.Background(), patientActor(), CreateInput{
		PatientCardID: int64Ptr(registrationTestCardID),
		ScheduleID:    registrationTestScheduleID,
		PaymentMethod: validPaymentMethod,
	})
	if err == nil {
		t.Fatal("依赖缺失时应返回错误")
	}
	var serviceErr *ServiceError
	if errors.As(err, &serviceErr) {
		t.Errorf("依赖缺失不应映射为业务错误码：%+v", serviceErr)
	}
}

// TestServiceRejectsInconsistentActor 守住一条跨层隐式契约：仓储用 ownerPatientID>0
// 作为「是否限定归属」的开关，所以 realm=patient 却缺少当前患者标识时必须显式拒绝
// （ErrInvalidActor），不能静默关闭归属条件读到全量数据（契约 §1.2）。
// 四个用例入口都必须先做这道防御性校验。
func TestServiceRejectsInconsistentActor(t *testing.T) {
	// 声明为患者域但缺少令牌主体：正常中间件不会产生这种组合，这里模拟被绕过的调用方。
	broken := Actor{Realm: domainauth.RealmPatient, UserID: registrationTestPatientID}

	cases := []struct {
		name string
		call func(env *registrationTestEnv) error
	}{
		{
			name: "资格校验",
			call: func(env *registrationTestEnv) error {
				_, err := env.service.Eligibility(
					context.Background(), broken, registrationTestCardID, registrationTestScheduleID)
				return err
			},
		},
		{
			name: "建单",
			call: func(env *registrationTestEnv) error {
				_, err := env.service.Create(context.Background(), broken, CreateInput{
					PatientCardID: int64Ptr(registrationTestCardID),
					ScheduleID:    registrationTestScheduleID,
					PaymentMethod: validPaymentMethod,
				})
				return err
			},
		},
		{
			name: "列表",
			call: func(env *registrationTestEnv) error {
				_, err := env.service.List(context.Background(), broken, ListQuery{Page: 1, PageSize: 20})
				return err
			},
		},
		{
			name: "详情",
			call: func(env *registrationTestEnv) error {
				_, err := env.service.Detail(context.Background(), broken, 1001)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRegistrationTestEnv(t)

			err := tc.call(env)
			if !errors.Is(err, ErrInvalidActor) {
				t.Fatalf("错误 = %v, want ErrInvalidActor", err)
			}
			var serviceErr *ServiceError
			if errors.As(err, &serviceErr) {
				t.Errorf("身份自相矛盾属内部调用错误，不应映射为业务错误码：%+v", serviceErr)
			}
			// 关键断言：身份校验不通过时不得触达仓储，否则归属条件可能已被静默跳过。
			touched := env.repo.snapshotCalls + env.repo.occupiedCalls + env.repo.createCalls +
				env.repo.listCalls + env.repo.detailCalls
			if touched != 0 {
				t.Errorf("身份校验失败时不得触达仓储，实际调用 %d 次", touched)
			}
		})
	}
}
