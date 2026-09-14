package registration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"Medical-Web-Backend/internal/domain/patient"
	"Medical-Web-Backend/internal/port"
)

// 本文件覆盖挂号建单 → 公开排班读缓存的失效契约（T4b），不连数据库、不连 Redis：
//
//	验收点 c：建单事务提交成功后必须立即失效，作用域取建单结果的 subdepartmentId / doctorId，
//	          「挂号成功后余量立即可见」不依赖 5~10 秒的缓存 TTL。
//	验收点 e（写路径）：失效失败只记日志，建单必须照常成功交付二维码。
//	补偿路径：预下单失败执行整单补偿（退回号源）后必须再失效一次。

// versionScope 是一次 BumpSchedules 调用的作用域；0 表示该维度未知或不适用。
type versionScope struct {
	subdepartmentID int64
	doctorID        int64
}

// recordingScheduleVersioner 是 port.ScheduleCacheVersioner 的记录桩：
// 记录每次失效调用的作用域与次数，并可注入错误。
type recordingScheduleVersioner struct {
	mu    sync.Mutex
	calls []versionScope
	err   error
}

func (v *recordingScheduleVersioner) BumpSchedules(_ context.Context, subdepartmentID, doctorID int64) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, versionScope{subdepartmentID: subdepartmentID, doctorID: doctorID})
	return v.err
}

func (v *recordingScheduleVersioner) recorded() []versionScope {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]versionScope(nil), v.calls...)
}

// registrationTestScope 是假仓储/快照写死的「子科室 2 + 医生 16」作用域
// （见 service_test.go 的 completeSnapshot 与 registrationFakeRepo.CreateRegistration）。
var registrationTestScope = versionScope{subdepartmentID: 2, doctorID: 16}

// newCacheInvalidationEnv 构造注入了失效桩的挂号用例环境：
// 复用 service_test.go 的内存桩（仓储/就诊卡/患者/预下单），只额外注入 versioner。
// versioner 传 nil 表示缓存未启用。
func newCacheInvalidationEnv(
	t *testing.T,
	versioner port.ScheduleCacheVersioner,
	precreator *registrationFakePrecreator,
) *registrationTestEnv {
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

	var hook port.PaymentPrecreator
	if precreator != nil {
		hook = precreator
	}
	service := NewService(repo, cards, patients, hook,
		func() time.Time { return registrationTestNow },
		WithScheduleCacheVersioner(versioner))
	service.newTradeNo = func(time.Time) string { return registrationTestTradeNo }
	return &registrationTestEnv{
		service:    service,
		repo:       repo,
		cards:      cards,
		patients:   patients,
		precreator: precreator,
	}
}

// validCreateInput 返回一个可成功建单的入参（本文件所有建单用例共用）。
func validCreateInput() CreateInput {
	return CreateInput{
		PatientCardID: int64Ptr(registrationTestCardID),
		ScheduleID:    registrationTestScheduleID,
		PaymentMethod: PaymentMethodAlipay,
	}
}

// TestCreateBumpsScheduleCacheVersionAfterBooking 覆盖验收点 c：
// 建单事务提交成功后必须立即失效公开排班缓存，作用域取建单结果的 subdepartmentId / doctorId，
// 「挂号成功后余量立即可见」不依赖 TTL 到期（否则患者会看到陈旧余量）。
func TestCreateBumpsScheduleCacheVersionAfterBooking(t *testing.T) {
	versioner := &recordingScheduleVersioner{}
	precreator := &registrationFakePrecreator{}
	env := newCacheInvalidationEnv(t, versioner, precreator)

	result, err := env.service.Create(context.Background(), patientActor(), validCreateInput())
	if err != nil {
		t.Fatalf("建单应当成功: %v", err)
	}
	if result == nil {
		t.Fatalf("建单成功必须返回结果")
	}
	if precreator.calls != 1 {
		t.Fatalf("建单成功后必须完成一次预下单并交付二维码，实际 %d 次", precreator.calls)
	}

	calls := versioner.recorded()
	if len(calls) != 1 {
		t.Fatalf("建单成功后应恰好失效一次，实际 %d 次: %+v", len(calls), calls)
	}
	// 作用域必须与建单结果一致（而不是硬编码或从入参推导）。
	want := versionScope{
		subdepartmentID: result.Registration.SubdepartmentID,
		doctorID:        result.Registration.DoctorID,
	}
	if calls[0] != want {
		t.Errorf("失效作用域 = %+v, want %+v（取自建单结果）", calls[0], want)
	}
	if calls[0] != registrationTestScope {
		t.Errorf("失效作用域 = %+v, want %+v（假仓储写死的建单结果）", calls[0], registrationTestScope)
	}
}

// TestCreateVersionerFailureDoesNotFailBooking 覆盖验收点 e 的写路径：
// 失效失败只记日志，建单必须照常成功、照常返回二维码（缓存是性能依赖而不是正确性依赖）。
func TestCreateVersionerFailureDoesNotFailBooking(t *testing.T) {
	versioner := &recordingScheduleVersioner{err: errors.New("redis 不可用")}
	precreator := &registrationFakePrecreator{}
	env := newCacheInvalidationEnv(t, versioner, precreator)

	result, err := env.service.Create(context.Background(), patientActor(), validCreateInput())
	if err != nil {
		t.Fatalf("失效失败不得让建单失败: %v", err)
	}
	if result == nil || result.Payment.PrepayID == "" {
		t.Fatalf("建单成功必须交付二维码，实际 %+v", result)
	}
	if precreator.calls != 1 {
		t.Errorf("预下单必须照常执行，实际 %d 次", precreator.calls)
	}
	if len(versioner.recorded()) != 1 {
		t.Errorf("即使失效失败也必须尝试过一次失效")
	}
}

// TestCreateCompensationBumpsScheduleCacheVersionAgain 覆盖补偿后的二次失效：
// 预下单失败会执行整单补偿（退回刚扣掉的号源），补偿后必须再失效一次，
// 否则「扣减后立刻回填」的缓存会把余量少算一个，一直压到 TTL 到期。
func TestCreateCompensationBumpsScheduleCacheVersionAgain(t *testing.T) {
	versioner := &recordingScheduleVersioner{}
	precreator := &registrationFakePrecreator{err: errors.New("支付宝预下单失败")}
	env := newCacheInvalidationEnv(t, versioner, precreator)

	result, err := env.service.Create(context.Background(), patientActor(), validCreateInput())
	if result != nil {
		t.Errorf("预下单失败不得返回二维码，实际 %+v", result)
	}
	assertServiceError(t, err, CodePaymentProviderUnavailable)
	if env.repo.compensateCalls != 1 {
		t.Fatalf("必须执行一次整单补偿，实际 %d 次", env.repo.compensateCalls)
	}

	calls := versioner.recorded()
	if len(calls) != 2 {
		t.Fatalf("建单后与补偿后应各失效一次，实际 %d 次: %+v", len(calls), calls)
	}
	for i, call := range calls {
		if call != registrationTestScope {
			t.Errorf("第 %d 次失效作用域 = %+v, want %+v", i+1, call, registrationTestScope)
		}
	}
}

// TestCreateFailureDoesNotBumpScheduleCacheVersion 建单在提交前失败（号源已满）时不得失效：
// 既避免无谓回源，也证明上面的断言不是「任何调用都算通过」的假阳性。
func TestCreateFailureDoesNotBumpScheduleCacheVersion(t *testing.T) {
	versioner := &recordingScheduleVersioner{}
	env := newCacheInvalidationEnv(t, versioner, &registrationFakePrecreator{})
	// 号源已满：前置校验直接返回 409，不会建单也不会扣减号源。
	env.repo.snapshot.SlotUsed = env.repo.snapshot.SlotMaximum

	result, err := env.service.Create(context.Background(), patientActor(), validCreateInput())
	if result != nil {
		t.Errorf("建单失败不得返回结果，实际 %+v", result)
	}
	assertServiceError(t, err, CodeSlotSoldOut)
	if env.repo.createCalls != 0 {
		t.Errorf("前置校验失败时不得触达建单事务，实际 %d 次", env.repo.createCalls)
	}
	if calls := versioner.recorded(); len(calls) != 0 {
		t.Errorf("建单失败不得失效缓存，实际 %d 次: %+v", len(calls), calls)
	}
}

// TestCreateWithoutScheduleCacheVersioner 缓存未启用（versioner=nil）时建单流程照常工作、不 panic。
func TestCreateWithoutScheduleCacheVersioner(t *testing.T) {
	env := newCacheInvalidationEnv(t, nil, &registrationFakePrecreator{})

	result, err := env.service.Create(context.Background(), patientActor(), validCreateInput())
	if err != nil {
		t.Fatalf("未启用缓存时建单不应失败: %v", err)
	}
	if result == nil || result.Payment.PrepayID == "" {
		t.Fatalf("建单成功必须交付二维码，实际 %+v", result)
	}
}
