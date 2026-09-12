// 病历用例（internal/usecase/medical_record）单测：覆盖医生身份解析（currentDoctorID）、
// CRUD 的错误码翻译与分页兜底（spec/04-api-contract.md §1.2、§1.4、§6.10、§10）。
//
// 授权语义为「严格医生绑定」：只接受 realm=mis 的令牌，且必须能由 mis_user.ref_id
// 解析出医生编号；未绑定医生（ref_id 为空或非正数）一律 403 AUTH_FORBIDDEN，
// 不存在 ROOT 旁路。全部用例用内存桩替换 port.MedicalRecordRepository，
// 不依赖 PostgreSQL 与 Redis，与 internal/usecase/registration/service_test.go 风格一致。
package medical_record

import (
	"context"
	"errors"
	"testing"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainmedicalrecord "Medical-Web-Backend/internal/domain/medical_record"
	"Medical-Web-Backend/internal/port"
)

const (
	// medicalRecordTestMisUserID 是管理端令牌中的操作者主键（mis_user.id）。
	medicalRecordTestMisUserID int64 = 7
	// medicalRecordTestDoctorID 是该账号绑定的医生编号（mis_user.ref_id）。
	medicalRecordTestDoctorID int64 = 16
	// medicalRecordTestRegistrationID 是归属当前医生的挂号主键。
	medicalRecordTestRegistrationID int64 = 1001
	// medicalRecordTestOtherRegistrationID 是归属其他医生（99 号）的挂号主键。
	medicalRecordTestOtherRegistrationID int64 = 2002
)

// medicalRecordFakeRepo 是内存版 port.MedicalRecordRepository 桩：
// 内嵌接口后只实现本域真正调用的方法；未实现的方法一旦被调用即 panic，
// 保证用例不会悄悄依赖仓储的其它能力。
type medicalRecordFakeRepo struct {
	port.MedicalRecordRepository

	// 读配置：FindDoctorIDByUserID 的返回值与故障注入。
	doctorID  int64
	doctorErr error

	// FindRegistrationOwner 的返回值与故障注入。
	owner    *domainmedicalrecord.RegistrationOwner
	ownerErr error

	// CreateMedicalRecord 的故障注入与结果。
	createErr error

	// ListMedicalRecords 的返回值与故障注入。
	listItems []domainmedicalrecord.MedicalRecord
	listTotal int64
	listErr   error

	// FindMedicalRecord / UpdateMedicalRecord / DeleteMedicalRecord 的故障注入与结果。
	record       *domainmedicalrecord.MedicalRecord
	findErr      error
	updateErr    error
	updateResult *domainmedicalrecord.MedicalRecord
	deleteErr    error

	// 调用记录，用于断言归属条件与分页参数是否正确下发。
	doctorCalls     int
	lastDoctorUser  int64
	ownerCalls      int
	lastRegID       int64
	lastOwnerDocID  int64
	createCalls     int
	lastCreateOwner domainmedicalrecord.RegistrationOwner
	lastCreateUUID  string
	lastDiagnosis   string
	lastContent     string
	listCalls       int
	lastFilter      domainmedicalrecord.Filter
	lastOffset      int
	lastLimit       int
	findCalls       int
	lastFindID      int64
	lastFindDocID   int64
	updateCalls     int
	lastUpdateID    int64
	lastUpdateDocID int64
	lastUpdateInput domainmedicalrecord.UpdateInput
	deleteCalls     int
	lastDeleteID    int64
	lastDeleteDocID int64
}

func (r *medicalRecordFakeRepo) FindDoctorIDByUserID(_ context.Context, userID int64) (int64, error) {
	r.doctorCalls++
	r.lastDoctorUser = userID
	if r.doctorErr != nil {
		return 0, r.doctorErr
	}
	return r.doctorID, nil
}

func (r *medicalRecordFakeRepo) FindRegistrationOwner(
	_ context.Context,
	registrationID, ownerDoctorID int64,
) (*domainmedicalrecord.RegistrationOwner, error) {
	r.ownerCalls++
	r.lastRegID = registrationID
	r.lastOwnerDocID = ownerDoctorID
	if r.ownerErr != nil {
		return nil, r.ownerErr
	}
	if r.owner == nil {
		// 与 PostgreSQL 实现一致：挂号不存在或不属于该医生时返回领域哨兵错误。
		return nil, domainmedicalrecord.ErrRegistrationNotFound
	}
	copied := *r.owner
	return &copied, nil
}

func (r *medicalRecordFakeRepo) CreateMedicalRecord(
	_ context.Context,
	owner domainmedicalrecord.RegistrationOwner,
	recordUUID, diagnosis, content string,
) (*domainmedicalrecord.MedicalRecord, error) {
	r.createCalls++
	r.lastCreateOwner = owner
	r.lastCreateUUID = recordUUID
	r.lastDiagnosis = diagnosis
	r.lastContent = content
	if r.createErr != nil {
		return nil, r.createErr
	}
	return &domainmedicalrecord.MedicalRecord{
		ID:              1001,
		UUID:            recordUUID,
		RegistrationID:  owner.RegistrationID,
		PatientCardID:   owner.PatientCardID,
		DoctorID:        owner.DoctorID,
		SubdepartmentID: owner.SubdepartmentID,
		Diagnosis:       diagnosis,
		Content:         content,
	}, nil
}

func (r *medicalRecordFakeRepo) ListMedicalRecords(
	_ context.Context,
	filter domainmedicalrecord.Filter,
	offset, limit int,
) ([]domainmedicalrecord.MedicalRecord, int64, error) {
	r.listCalls++
	r.lastFilter = filter
	r.lastOffset = offset
	r.lastLimit = limit
	if r.listErr != nil {
		return nil, 0, r.listErr
	}
	return r.listItems, r.listTotal, nil
}

func (r *medicalRecordFakeRepo) FindMedicalRecord(
	_ context.Context,
	id, ownerDoctorID int64,
) (*domainmedicalrecord.MedicalRecord, error) {
	r.findCalls++
	r.lastFindID = id
	r.lastFindDocID = ownerDoctorID
	if r.findErr != nil {
		return nil, r.findErr
	}
	if r.record == nil {
		return nil, domainmedicalrecord.ErrNotFound
	}
	copied := *r.record
	return &copied, nil
}

func (r *medicalRecordFakeRepo) UpdateMedicalRecord(
	_ context.Context,
	id, ownerDoctorID int64,
	input domainmedicalrecord.UpdateInput,
) (*domainmedicalrecord.MedicalRecord, error) {
	r.updateCalls++
	r.lastUpdateID = id
	r.lastUpdateDocID = ownerDoctorID
	r.lastUpdateInput = input
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	if r.updateResult != nil {
		copied := *r.updateResult
		return &copied, nil
	}
	record := domainmedicalrecord.MedicalRecord{ID: id, Diagnosis: "原诊断", Content: "原正文"}
	if input.Diagnosis != nil {
		record.Diagnosis = *input.Diagnosis
	}
	if input.Content != nil {
		record.Content = *input.Content
	}
	return &record, nil
}

func (r *medicalRecordFakeRepo) DeleteMedicalRecord(_ context.Context, id, ownerDoctorID int64) error {
	r.deleteCalls++
	r.lastDeleteID = id
	r.lastDeleteDocID = ownerDoctorID
	return r.deleteErr
}

// medicalRecordTestEnv 汇总被测服务与依赖桩。
type medicalRecordTestEnv struct {
	service *Service
	repo    *medicalRecordFakeRepo
}

// newMedicalRecordTestEnv 构造默认用例环境：mis_user.id=7 绑定医生 16，
// 挂号 1001 归属医生 16。
func newMedicalRecordTestEnv(t *testing.T) *medicalRecordTestEnv {
	t.Helper()
	repo := &medicalRecordFakeRepo{
		doctorID: medicalRecordTestDoctorID,
		owner: &domainmedicalrecord.RegistrationOwner{
			RegistrationID:  medicalRecordTestRegistrationID,
			PatientCardID:   501,
			DoctorID:        medicalRecordTestDoctorID,
			SubdepartmentID: 2,
		},
	}
	return &medicalRecordTestEnv{service: NewService(repo), repo: repo}
}

// medicalRecordMisActor 返回 realm=mis 的调用者（令牌主体固定为 medicalRecordTestMisUserID）。
func medicalRecordMisActor() Actor {
	return Actor{Realm: domainauth.RealmMis, UserID: medicalRecordTestMisUserID}
}

// medicalRecordValidInput 返回可通过领域校验的书写入参。
func medicalRecordValidInput() CreateInput {
	return CreateInput{
		RegistrationID: medicalRecordTestRegistrationID,
		Diagnosis:      "牙髓炎",
		Content:        "主诉：牙痛\n处理：根管治疗",
	}
}

// assertMedicalRecordServiceError 断言错误是携带指定 code 的 *ServiceError。
func assertMedicalRecordServiceError(t *testing.T, err error, wantCode string) *ServiceError {
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

func medicalRecordStringPtr(value string) *string { return &value }

func medicalRecordInt64Ptr(value int64) *int64 { return &value }

// TestCurrentDoctorIDBranches 覆盖身份解析的全部分支（严格医生绑定的核心授权逻辑）：
// 只有 realm=mis 且 mis_user.ref_id 能解析出正数医生编号时才放行。
func TestCurrentDoctorIDBranches(t *testing.T) {
	cases := []struct {
		name       string
		realm      domainauth.Realm
		prepare    func(repo *medicalRecordFakeRepo)
		nilRepo    bool
		wantDoctor int64
		wantCode   string
		wantErr    error
		wantCalls  int
	}{
		{
			name:       "已绑定医生的管理端账号放行",
			realm:      domainauth.RealmMis,
			wantDoctor: medicalRecordTestDoctorID,
			wantCalls:  1,
		},
		{
			name:      "未绑定医生（ref_id=0）返回 403",
			realm:     domainauth.RealmMis,
			prepare:   func(repo *medicalRecordFakeRepo) { repo.doctorID = 0 },
			wantCode:  CodeForbidden,
			wantCalls: 1,
		},
		{
			name:      "ref_id 为脏数据负数同样返回 403",
			realm:     domainauth.RealmMis,
			prepare:   func(repo *medicalRecordFakeRepo) { repo.doctorID = -3 },
			wantCode:  CodeForbidden,
			wantCalls: 1,
		},
		{
			name:      "读取医生绑定失败归为依赖不可用",
			realm:     domainauth.RealmMis,
			prepare:   func(repo *medicalRecordFakeRepo) { repo.doctorErr = errors.New("postgres is down") },
			wantErr:   ErrDependencyUnavailable,
			wantCalls: 1,
		},
		{
			name:      "患者域令牌不可用于授权判定",
			realm:     domainauth.RealmPatient,
			wantErr:   ErrInvalidActor,
			wantCalls: 0,
		},
		{
			name:      "零值 realm 同样按无效身份处理",
			realm:     "",
			wantErr:   ErrInvalidActor,
			wantCalls: 0,
		},
		{
			name:      "仓储缺失按依赖不可用处理",
			realm:     domainauth.RealmMis,
			nilRepo:   true,
			wantErr:   ErrDependencyUnavailable,
			wantCalls: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMedicalRecordTestEnv(t)
			if tc.prepare != nil {
				tc.prepare(env.repo)
			}
			service := env.service
			if tc.nilRepo {
				service = NewService(nil)
			}
			actor := medicalRecordMisActor()
			actor.Realm = tc.realm

			got, err := service.currentDoctorID(context.Background(), actor)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("currentDoctorID 错误 = %v, want %v", err, tc.wantErr)
				}
			} else if tc.wantCode != "" {
				assertMedicalRecordServiceError(t, err, tc.wantCode)
			} else if err != nil {
				t.Fatalf("currentDoctorID 意外报错：%v", err)
			}
			if got != tc.wantDoctor {
				t.Errorf("currentDoctorID = %d, want %d", got, tc.wantDoctor)
			}
			if env.repo.doctorCalls != tc.wantCalls {
				t.Errorf("FindDoctorIDByUserID 调用次数 = %d, want %d", env.repo.doctorCalls, tc.wantCalls)
			}
			if tc.wantCalls > 0 && env.repo.lastDoctorUser != medicalRecordTestMisUserID {
				t.Errorf("查询的账号主键 = %d, want %d", env.repo.lastDoctorUser, medicalRecordTestMisUserID)
			}
		})
	}
}

// TestCurrentDoctorIDForbiddenMessage 未绑定医生的 403 文案必须与契约 §1.2/§6.10 一致。
func TestCurrentDoctorIDForbiddenMessage(t *testing.T) {
	env := newMedicalRecordTestEnv(t)
	env.repo.doctorID = 0

	_, err := env.service.currentDoctorID(context.Background(), medicalRecordMisActor())
	serviceErr := assertMedicalRecordServiceError(t, err, CodeForbidden)
	if serviceErr.Code != "AUTH_FORBIDDEN" {
		t.Errorf("code = %q, want 字面量 AUTH_FORBIDDEN（契约 §1.2/§6.10 的既有错误码）", serviceErr.Code)
	}
	if serviceErr.Message != "当前账号未关联医生，无法访问病历数据" {
		t.Errorf("message = %q, want 当前账号未关联医生，无法访问病历数据", serviceErr.Message)
	}
}

// TestErrorCodeLiterals 业务错误码是对外契约的一部分，必须与 spec 的字面量完全一致：
// 改动常量取值而不改契约属于破坏性变更，这里用字面量把契约钉死。
func TestErrorCodeLiterals(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"参数错误", CodeValidationFailed, "REQUEST_VALIDATION_FAILED"},
		{"病历不存在", CodeNotFound, "MEDICAL_RECORD_NOT_FOUND"},
		{"挂号不存在", CodeRegistrationNotFound, "REGISTRATION_NOT_FOUND"},
		{"病历重复", CodeDuplicate, "MEDICAL_RECORD_DUPLICATE"},
		{"未绑定医生", CodeForbidden, "AUTH_FORBIDDEN"},
		{"依赖不可用", CodeDependencyUnavailable, "DEPENDENCY_UNAVAILABLE"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s：code = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestCreateSuccess 覆盖书写成功路径：归属条件取自令牌主体解析出的医生编号，
// 领域层归一化后的诊断与正文才下发仓储，业务标识形如 RX + 30 位大写十六进制。
func TestCreateSuccess(t *testing.T) {
	env := newMedicalRecordTestEnv(t)

	record, err := env.service.Create(context.Background(), medicalRecordMisActor(), CreateInput{
		RegistrationID: medicalRecordTestRegistrationID,
		Diagnosis:      "  牙髓炎  ",
		Content:        "\n 主诉：牙痛 \n",
	})
	if err != nil {
		t.Fatalf("Create 意外报错：%v", err)
	}
	if record == nil {
		t.Fatal("Create 应返回病历资源")
	}
	if env.repo.ownerCalls != 1 || env.repo.lastRegID != medicalRecordTestRegistrationID {
		t.Errorf("FindRegistrationOwner 参数 = (%d 次, registrationID=%d)", env.repo.ownerCalls, env.repo.lastRegID)
	}
	if env.repo.lastOwnerDocID != medicalRecordTestDoctorID {
		t.Errorf("归属医生编号 = %d, want %d（必须来自令牌主体，不采信客户端）",
			env.repo.lastOwnerDocID, medicalRecordTestDoctorID)
	}
	if env.repo.lastCreateOwner.DoctorID != medicalRecordTestDoctorID ||
		env.repo.lastCreateOwner.PatientCardID != 501 {
		t.Errorf("CreateMedicalRecord 归属 = %+v, want 医生 16 / 就诊卡 501", env.repo.lastCreateOwner)
	}
	if env.repo.lastDiagnosis != "牙髓炎" {
		t.Errorf("下发的 diagnosis = %q, want 已裁剪的 牙髓炎", env.repo.lastDiagnosis)
	}
	if env.repo.lastContent != "主诉：牙痛" {
		t.Errorf("下发的 content = %q, want 已裁剪的 主诉：牙痛", env.repo.lastContent)
	}
	if len(env.repo.lastCreateUUID) != 32 || env.repo.lastCreateUUID[:2] != "RX" {
		t.Errorf("业务标识 = %q, want RX 前缀 + 30 位十六进制", env.repo.lastCreateUUID)
	}
	if record.DoctorID != medicalRecordTestDoctorID || record.RegistrationID != medicalRecordTestRegistrationID {
		t.Errorf("返回病历 = %+v, want 归属挂号 %d / 医生 %d", record, medicalRecordTestRegistrationID, medicalRecordTestDoctorID)
	}
}

// TestCreateValidationSkipsRepository 领域校验失败必须先于任何仓储访问：
// 空诊断/空正文/超长内容都返回 422 参数错误码，且不得触达挂号归属与落库。
func TestCreateValidationSkipsRepository(t *testing.T) {
	cases := []struct {
		name    string
		input   CreateInput
		wantMsg string
	}{
		{
			name:    "空诊断",
			input:   CreateInput{RegistrationID: medicalRecordTestRegistrationID, Diagnosis: "  ", Content: "正文"},
			wantMsg: "diagnosis 为必传字段",
		},
		{
			name:    "空正文",
			input:   CreateInput{RegistrationID: medicalRecordTestRegistrationID, Diagnosis: "牙髓炎", Content: "\n"},
			wantMsg: "content 为必传字段",
		},
		{
			name: "诊断超长",
			input: CreateInput{
				RegistrationID: medicalRecordTestRegistrationID,
				Diagnosis:      repeatRune('诊', domainmedicalrecord.MaxDiagnosisRunes+1),
				Content:        "正文",
			},
			wantMsg: "diagnosis 不能超过 200 个字符",
		},
		{
			name: "正文超长",
			input: CreateInput{
				RegistrationID: medicalRecordTestRegistrationID,
				Diagnosis:      "牙髓炎",
				Content:        repeatRune('疗', domainmedicalrecord.MaxContentRunes+1),
			},
			wantMsg: "content 不能超过 20000 个字符",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMedicalRecordTestEnv(t)

			_, err := env.service.Create(context.Background(), medicalRecordMisActor(), tc.input)
			serviceErr := assertMedicalRecordServiceError(t, err, CodeValidationFailed)
			if serviceErr.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", serviceErr.Message, tc.wantMsg)
			}
			if env.repo.ownerCalls != 0 || env.repo.createCalls != 0 {
				t.Errorf("校验失败不得访问仓储：ownerCalls=%d createCalls=%d",
					env.repo.ownerCalls, env.repo.createCalls)
			}
		})
	}
}

// TestCreateTranslateErrors 覆盖写书接口的错误码翻译：
// 挂号不存在/非本人负责 → REGISTRATION_NOT_FOUND；同一挂号已有病历 → MEDICAL_RECORD_DUPLICATE；
// 未识别的仓储故障 → 502 DEPENDENCY_UNAVAILABLE。
func TestCreateTranslateErrors(t *testing.T) {
	cases := []struct {
		name     string
		prepare  func(repo *medicalRecordFakeRepo)
		wantCode string
		wantMsg  string
		wantErr  error
	}{
		{
			name:     "挂号不存在或不属于当前医生",
			prepare:  func(repo *medicalRecordFakeRepo) { repo.owner = nil },
			wantCode: CodeRegistrationNotFound,
			wantMsg:  "挂号记录不存在",
		},
		{
			name:     "同一挂号已有病历",
			prepare:  func(repo *medicalRecordFakeRepo) { repo.createErr = domainmedicalrecord.ErrDuplicate },
			wantCode: CodeDuplicate,
			wantMsg:  "该挂号已有病历，请改用修改接口",
		},
		{
			name:    "仓储故障",
			prepare: func(repo *medicalRecordFakeRepo) { repo.createErr = errors.New("postgres is down") },
			wantErr: ErrDependencyUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMedicalRecordTestEnv(t)
			tc.prepare(env.repo)

			_, err := env.service.Create(context.Background(), medicalRecordMisActor(), medicalRecordValidInput())
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Create 错误 = %v, want %v", err, tc.wantErr)
				}
				return
			}
			serviceErr := assertMedicalRecordServiceError(t, err, tc.wantCode)
			if serviceErr.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", serviceErr.Message, tc.wantMsg)
			}
		})
	}
}

// TestListPassesOwnerAndFilters 覆盖列表查询：归属医生编号强制来自令牌主体，
// 客户端可选的过滤条件与分页参数原样下发。
func TestListPassesOwnerAndFilters(t *testing.T) {
	env := newMedicalRecordTestEnv(t)
	env.repo.listItems = []domainmedicalrecord.MedicalRecord{{ID: 1, RegistrationID: medicalRecordTestRegistrationID}}
	env.repo.listTotal = 1

	page, err := env.service.List(context.Background(), medicalRecordMisActor(), ListQuery{
		RegistrationID: medicalRecordInt64Ptr(medicalRecordTestRegistrationID),
		PatientCardID:  medicalRecordInt64Ptr(501),
		DoctorID:       medicalRecordInt64Ptr(medicalRecordTestDoctorID),
		Page:           2,
		PageSize:       25,
	})
	if err != nil {
		t.Fatalf("List 意外报错：%v", err)
	}
	if env.repo.lastFilter.OwnerDoctorID != medicalRecordTestDoctorID {
		t.Errorf("归属医生编号 = %d, want %d", env.repo.lastFilter.OwnerDoctorID, medicalRecordTestDoctorID)
	}
	if env.repo.lastFilter.RegistrationID == nil || *env.repo.lastFilter.RegistrationID != medicalRecordTestRegistrationID {
		t.Errorf("registrationId 过滤 = %v, want %d", env.repo.lastFilter.RegistrationID, medicalRecordTestRegistrationID)
	}
	if env.repo.lastFilter.PatientCardID == nil || *env.repo.lastFilter.PatientCardID != 501 {
		t.Errorf("patientCardId 过滤 = %v, want 501", env.repo.lastFilter.PatientCardID)
	}
	if env.repo.lastFilter.DoctorID == nil || *env.repo.lastFilter.DoctorID != medicalRecordTestDoctorID {
		t.Errorf("doctorId 过滤 = %v, want %d", env.repo.lastFilter.DoctorID, medicalRecordTestDoctorID)
	}
	if env.repo.lastOffset != 25 || env.repo.lastLimit != 25 {
		t.Errorf("offset/limit = %d/%d, want 25/25", env.repo.lastOffset, env.repo.lastLimit)
	}
	if page == nil || page.Page != 2 || page.PageSize != 25 || page.Total != 1 || len(page.Items) != 1 {
		t.Errorf("列表结果 = %+v, want page=2 pageSize=25 total=1 items=1", page)
	}
}

// TestListPaginationFallback 分页兜底：越界值收敛到合法区间，任何情况都不产生负 offset。
func TestListPaginationFallback(t *testing.T) {
	cases := []struct {
		name         string
		page         int
		pageSize     int
		wantPage     int
		wantPageSize int
		wantOffset   int
		wantLimit    int
	}{
		{name: "零值兜底", page: 0, pageSize: 0, wantPage: 1, wantPageSize: 20, wantOffset: 0, wantLimit: 20},
		{name: "负数归一到第一页", page: -5, pageSize: -1, wantPage: 1, wantPageSize: 20, wantOffset: 0, wantLimit: 20},
		{name: "正常分页保持原值", page: 3, pageSize: 25, wantPage: 3, wantPageSize: 25, wantOffset: 50, wantLimit: 25},
		{name: "pageSize 超上限收敛到 100", page: 2, pageSize: 1000, wantPage: 2, wantPageSize: 100, wantOffset: 100, wantLimit: 100},
		{
			name:         "page 超上限收敛到 100000",
			page:         200000,
			pageSize:     20,
			wantPage:     100000,
			wantPageSize: 20,
			wantOffset:   1999980,
			wantLimit:    20,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMedicalRecordTestEnv(t)

			page, err := env.service.List(context.Background(), medicalRecordMisActor(), ListQuery{
				Page: tc.page, PageSize: tc.pageSize,
			})
			if err != nil {
				t.Fatalf("List 意外报错：%v", err)
			}
			if env.repo.lastOffset != tc.wantOffset || env.repo.lastLimit != tc.wantLimit {
				t.Errorf("offset/limit = %d/%d, want %d/%d",
					env.repo.lastOffset, env.repo.lastLimit, tc.wantOffset, tc.wantLimit)
			}
			if page.Page != tc.wantPage || page.PageSize != tc.wantPageSize {
				t.Errorf("返回分页 = %d/%d, want %d/%d",
					page.Page, page.PageSize, tc.wantPage, tc.wantPageSize)
			}
		})
	}
}

// TestListRepositoryFailure 列表仓储故障必须归为依赖不可用，不得伪装成空列表。
func TestListRepositoryFailure(t *testing.T) {
	env := newMedicalRecordTestEnv(t)
	env.repo.listErr = errors.New("postgres is down")

	_, err := env.service.List(context.Background(), medicalRecordMisActor(), ListQuery{Page: 1, PageSize: 20})
	if !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("List 错误 = %v, want ErrDependencyUnavailable", err)
	}
}

// TestDetail 覆盖详情：读取时同样带上令牌解析出的医生编号，
// 不存在或不属于该医生统一返回 MEDICAL_RECORD_NOT_FOUND（越权不泄漏存在性）。
func TestDetail(t *testing.T) {
	t.Run("读取成功", func(t *testing.T) {
		env := newMedicalRecordTestEnv(t)
		env.repo.record = &domainmedicalrecord.MedicalRecord{ID: 88, Diagnosis: "牙髓炎"}

		record, err := env.service.Detail(context.Background(), medicalRecordMisActor(), 88)
		if err != nil {
			t.Fatalf("Detail 意外报错：%v", err)
		}
		if record.ID != 88 || record.Diagnosis != "牙髓炎" {
			t.Errorf("Detail = %+v, want id=88", record)
		}
		if env.repo.lastFindID != 88 || env.repo.lastFindDocID != medicalRecordTestDoctorID {
			t.Errorf("FindMedicalRecord 参数 = (%d, %d), want (88, %d)",
				env.repo.lastFindID, env.repo.lastFindDocID, medicalRecordTestDoctorID)
		}
	})

	t.Run("不存在或越权统一 404", func(t *testing.T) {
		env := newMedicalRecordTestEnv(t)
		env.repo.findErr = domainmedicalrecord.ErrNotFound

		_, err := env.service.Detail(context.Background(), medicalRecordMisActor(), 999)
		assertMedicalRecordServiceError(t, err, CodeNotFound)
	})
}

// TestUpdate 覆盖修改用例：
// 两个字段都为 nil → 422；只归一化提交的字段；越权/不存在 → 404；仓储故障 → 502。
func TestUpdate(t *testing.T) {
	t.Run("两个字段都为 nil 返回 422 且不访问仓储", func(t *testing.T) {
		env := newMedicalRecordTestEnv(t)

		_, err := env.service.Update(context.Background(), medicalRecordMisActor(), 88,
			domainmedicalrecord.UpdateInput{})
		serviceErr := assertMedicalRecordServiceError(t, err, CodeValidationFailed)
		if serviceErr.Message != "至少提交 diagnosis 或 content 中的一个字段" {
			t.Errorf("message = %q", serviceErr.Message)
		}
		if env.repo.updateCalls != 0 || env.repo.doctorCalls != 0 {
			t.Errorf("参数错误不得访问仓储：updateCalls=%d doctorCalls=%d",
				env.repo.updateCalls, env.repo.doctorCalls)
		}
	})

	t.Run("只归一化并提交已提交的字段", func(t *testing.T) {
		cases := []struct {
			name          string
			input         domainmedicalrecord.UpdateInput
			wantDiagnosis *string
			wantContent   *string
		}{
			{
				name:          "只提交 diagnosis",
				input:         domainmedicalrecord.UpdateInput{Diagnosis: medicalRecordStringPtr("  新诊断 ")},
				wantDiagnosis: medicalRecordStringPtr("新诊断"),
			},
			{
				name:        "只提交 content",
				input:       domainmedicalrecord.UpdateInput{Content: medicalRecordStringPtr(" 新正文 ")},
				wantContent: medicalRecordStringPtr("新正文"),
			},
			{
				name: "两个字段都提交",
				input: domainmedicalrecord.UpdateInput{
					Diagnosis: medicalRecordStringPtr("诊断A"),
					Content:   medicalRecordStringPtr("正文B"),
				},
				wantDiagnosis: medicalRecordStringPtr("诊断A"),
				wantContent:   medicalRecordStringPtr("正文B"),
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				env := newMedicalRecordTestEnv(t)

				record, err := env.service.Update(context.Background(), medicalRecordMisActor(), 88, tc.input)
				if err != nil {
					t.Fatalf("Update 意外报错：%v", err)
				}
				if env.repo.lastUpdateID != 88 || env.repo.lastUpdateDocID != medicalRecordTestDoctorID {
					t.Errorf("UpdateMedicalRecord 参数 = (%d, %d), want (88, %d)",
						env.repo.lastUpdateID, env.repo.lastUpdateDocID, medicalRecordTestDoctorID)
				}
				got := env.repo.lastUpdateInput
				if !equalStringPtr(got.Diagnosis, tc.wantDiagnosis) {
					t.Errorf("下发的 diagnosis = %v, want %v", ptrString(got.Diagnosis), ptrString(tc.wantDiagnosis))
				}
				if !equalStringPtr(got.Content, tc.wantContent) {
					t.Errorf("下发的 content = %v, want %v", ptrString(got.Content), ptrString(tc.wantContent))
				}
				if record == nil {
					t.Fatal("Update 应返回更新后的病历")
				}
			})
		}
	})

	t.Run("提交空串命中领域校验", func(t *testing.T) {
		env := newMedicalRecordTestEnv(t)

		_, err := env.service.Update(context.Background(), medicalRecordMisActor(), 88,
			domainmedicalrecord.UpdateInput{Content: medicalRecordStringPtr("   ")})
		serviceErr := assertMedicalRecordServiceError(t, err, CodeValidationFailed)
		if serviceErr.Message != "content 为必传字段" {
			t.Errorf("message = %q, want content 为必传字段", serviceErr.Message)
		}
		if env.repo.updateCalls != 0 {
			t.Errorf("校验失败不得落库：updateCalls=%d", env.repo.updateCalls)
		}
	})

	t.Run("不存在或越权统一 404", func(t *testing.T) {
		env := newMedicalRecordTestEnv(t)
		env.repo.updateErr = domainmedicalrecord.ErrNotFound

		_, err := env.service.Update(context.Background(), medicalRecordMisActor(), 88,
			domainmedicalrecord.UpdateInput{Diagnosis: medicalRecordStringPtr("新诊断")})
		assertMedicalRecordServiceError(t, err, CodeNotFound)
	})

	t.Run("仓储故障归为依赖不可用", func(t *testing.T) {
		env := newMedicalRecordTestEnv(t)
		env.repo.updateErr = errors.New("postgres is down")

		_, err := env.service.Update(context.Background(), medicalRecordMisActor(), 88,
			domainmedicalrecord.UpdateInput{Diagnosis: medicalRecordStringPtr("新诊断")})
		if !errors.Is(err, ErrDependencyUnavailable) {
			t.Fatalf("Update 错误 = %v, want ErrDependencyUnavailable", err)
		}
	})
}

// TestDelete 覆盖删除：成功返回 nil；不存在或越权按 404 处理；重复删除同样 404。
func TestDelete(t *testing.T) {
	t.Run("删除成功", func(t *testing.T) {
		env := newMedicalRecordTestEnv(t)

		if err := env.service.Delete(context.Background(), medicalRecordMisActor(), 88); err != nil {
			t.Fatalf("Delete 意外报错：%v", err)
		}
		if env.repo.lastDeleteID != 88 || env.repo.lastDeleteDocID != medicalRecordTestDoctorID {
			t.Errorf("DeleteMedicalRecord 参数 = (%d, %d), want (88, %d)",
				env.repo.lastDeleteID, env.repo.lastDeleteDocID, medicalRecordTestDoctorID)
		}
	})

	t.Run("不存在或越权统一 404", func(t *testing.T) {
		env := newMedicalRecordTestEnv(t)
		env.repo.deleteErr = domainmedicalrecord.ErrNotFound

		err := env.service.Delete(context.Background(), medicalRecordMisActor(), 88)
		assertMedicalRecordServiceError(t, err, CodeNotFound)
	})
}

// TestOperationsRejectUnboundDoctor 未绑定医生身份的账号在五个用例上都必须 403，
// 且不得访问挂号归属或病历数据（fail closed，不存在 ROOT 旁路）。
func TestOperationsRejectUnboundDoctor(t *testing.T) {
	cases := []struct {
		name string
		call func(env *medicalRecordTestEnv) error
	}{
		{
			name: "Create",
			call: func(env *medicalRecordTestEnv) error {
				_, err := env.service.Create(context.Background(), medicalRecordMisActor(), medicalRecordValidInput())
				return err
			},
		},
		{
			name: "List",
			call: func(env *medicalRecordTestEnv) error {
				_, err := env.service.List(context.Background(), medicalRecordMisActor(), ListQuery{Page: 1, PageSize: 20})
				return err
			},
		},
		{
			name: "Detail",
			call: func(env *medicalRecordTestEnv) error {
				_, err := env.service.Detail(context.Background(), medicalRecordMisActor(), 88)
				return err
			},
		},
		{
			name: "Update",
			call: func(env *medicalRecordTestEnv) error {
				_, err := env.service.Update(context.Background(), medicalRecordMisActor(), 88,
					domainmedicalrecord.UpdateInput{Diagnosis: medicalRecordStringPtr("新诊断")})
				return err
			},
		},
		{
			name: "Delete",
			call: func(env *medicalRecordTestEnv) error {
				return env.service.Delete(context.Background(), medicalRecordMisActor(), 88)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMedicalRecordTestEnv(t)
			env.repo.doctorID = 0

			err := tc.call(env)
			assertMedicalRecordServiceError(t, err, CodeForbidden)
			if env.repo.ownerCalls != 0 || env.repo.createCalls != 0 || env.repo.listCalls != 0 ||
				env.repo.findCalls != 0 || env.repo.updateCalls != 0 || env.repo.deleteCalls != 0 {
				t.Errorf("未绑定医生不得触达病历数据：owner=%d create=%d list=%d find=%d update=%d delete=%d",
					env.repo.ownerCalls, env.repo.createCalls, env.repo.listCalls,
					env.repo.findCalls, env.repo.updateCalls, env.repo.deleteCalls)
			}
		})
	}
}

// TestNonMisRealmRejectedBeforeRepository 非管理端令牌在五个用例上都返回 ErrInvalidActor，
// 且不访问仓储（handler 层会先按 realm 拦截为 401，这里是用例层的兜底断言）。
func TestNonMisRealmRejectedBeforeRepository(t *testing.T) {
	cases := []struct {
		name string
		call func(env *medicalRecordTestEnv) error
	}{
		{
			name: "Create",
			call: func(env *medicalRecordTestEnv) error {
				_, err := env.service.Create(context.Background(), Actor{Realm: domainauth.RealmPatient, UserID: 20}, medicalRecordValidInput())
				return err
			},
		},
		{
			name: "List",
			call: func(env *medicalRecordTestEnv) error {
				_, err := env.service.List(context.Background(), Actor{Realm: domainauth.RealmPatient, UserID: 20}, ListQuery{Page: 1, PageSize: 20})
				return err
			},
		},
		{
			name: "Detail",
			call: func(env *medicalRecordTestEnv) error {
				_, err := env.service.Detail(context.Background(), Actor{Realm: domainauth.RealmPatient, UserID: 20}, 88)
				return err
			},
		},
		{
			name: "Update",
			call: func(env *medicalRecordTestEnv) error {
				_, err := env.service.Update(context.Background(), Actor{Realm: domainauth.RealmPatient, UserID: 20}, 88,
					domainmedicalrecord.UpdateInput{Diagnosis: medicalRecordStringPtr("新诊断")})
				return err
			},
		},
		{
			name: "Delete",
			call: func(env *medicalRecordTestEnv) error {
				return env.service.Delete(context.Background(), Actor{Realm: domainauth.RealmPatient, UserID: 20}, 88)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMedicalRecordTestEnv(t)

			err := tc.call(env)
			if !errors.Is(err, ErrInvalidActor) {
				t.Fatalf("错误 = %v, want ErrInvalidActor", err)
			}
			if env.repo.doctorCalls != 0 || env.repo.ownerCalls != 0 || env.repo.createCalls != 0 ||
				env.repo.listCalls != 0 || env.repo.findCalls != 0 || env.repo.updateCalls != 0 || env.repo.deleteCalls != 0 {
				t.Error("非管理端令牌不得访问仓储")
			}
		})
	}
}

// equalStringPtr 比较两个 *string 是否同时为 nil 或指向相同内容。
func equalStringPtr(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// ptrString 便于在断言消息中打印 *string。
func ptrString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// repeatRune 生成 count 个相同字符，用于构造长度边界输入。
func repeatRune(r rune, count int) string {
	buf := make([]rune, count)
	for i := range buf {
		buf[i] = r
	}
	return string(buf)
}
