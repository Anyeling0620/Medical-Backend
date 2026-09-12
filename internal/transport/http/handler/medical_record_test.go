// 病历 HTTP 契约单测：POST / GET 列表 / GET 详情 / PATCH / DELETE 的状态码、
// 错误 envelope 与响应字段集合（spec/04-api-contract.md 病历一节、§1.3、§1.4、§10、§12.4）。
//
// gin 处于 TestMode；路由按 router.go 的方式挂载；中间件里直接注入 claims 模拟
// 「访问令牌已校验通过」（mis 与 patient 两个 realm 各一套引擎）。
// 仓储用内存桩替换，但不 mock 用例服务：handler 依赖具体类型 *medicalrecordservice.Service，
// 因此这里构造真实 Service + 内存仓储，保证授权、归一化与错误码映射都被真实执行。
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainmedicalrecord "Medical-Web-Backend/internal/domain/medical_record"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/usecase/authsession"
	medicalrecordservice "Medical-Web-Backend/internal/usecase/medical_record"
)

const (
	// medicalRecordMisUserID 是管理端令牌中的操作者主键（mis_user.id）。
	medicalRecordMisUserID int64 = 7
	// medicalRecordDoctorID 是该账号绑定的医生编号（mis_user.ref_id）。
	medicalRecordDoctorID int64 = 16
	// medicalRecordOtherDoctorID 是另一位医生的编号，用于构造越权场景。
	medicalRecordOtherDoctorID int64 = 99
	// medicalRecordRegistrationID 是归属 medicalRecordDoctorID 的挂号主键。
	medicalRecordRegistrationID int64 = 1001
	// medicalRecordOtherRegistrationID 是归属 medicalRecordOtherDoctorID 的挂号主键。
	medicalRecordOtherRegistrationID int64 = 2002
)

// medicalRecordHandlerRepo 是内存版 port.MedicalRecordRepository 桩：
// 归属过滤、同一挂号唯一性与错误口径都与 PostgreSQL 实现保持一致（fail closed），
// 因此 handler 层可以在没有数据库的情况下走通真实用例逻辑。
type medicalRecordHandlerRepo struct {
	port.MedicalRecordRepository

	// doctorIDByUser 模拟 mis_user.id -> mis_user.ref_id（缺省 0 表示未绑定医生）。
	doctorIDByUser map[int64]int64
	// registrations 模拟 medical_registration 的归属信息。
	registrations map[int64]domainmedicalrecord.RegistrationOwner
	// records 模拟 hospital.doctor_prescription，按主键索引。
	records map[int64]*domainmedicalrecord.MedicalRecord
	nextID  int64

	// 故障注入。
	doctorErr error
	ownerErr  error
	createErr error
	listErr   error
	findErr   error
	updateErr error
	deleteErr error

	// 调用记录，用于断言归属条件是否正确下发。
	doctorCalls       int
	lastDoctorUserID  int64
	ownerCalls        int
	lastOwnerRegID    int64
	lastOwnerDoctorID int64
	createCalls       int
	lastCreateOwner   domainmedicalrecord.RegistrationOwner
	listCalls         int
	lastFilter        domainmedicalrecord.Filter
	lastOffset        int
	lastLimit         int
	findCalls         int
	lastFindID        int64
	lastFindDoctorID  int64
	updateCalls       int
	lastUpdateID      int64
	lastUpdateDoctor  int64
	lastUpdateInput   domainmedicalrecord.UpdateInput
	deleteCalls       int
	lastDeleteID      int64
	lastDeleteDoctor  int64
}

func (r *medicalRecordHandlerRepo) FindDoctorIDByUserID(_ context.Context, userID int64) (int64, error) {
	r.doctorCalls++
	r.lastDoctorUserID = userID
	if r.doctorErr != nil {
		return 0, r.doctorErr
	}
	return r.doctorIDByUser[userID], nil
}

func (r *medicalRecordHandlerRepo) FindRegistrationOwner(
	_ context.Context,
	registrationID, ownerDoctorID int64,
) (*domainmedicalrecord.RegistrationOwner, error) {
	r.ownerCalls++
	r.lastOwnerRegID = registrationID
	r.lastOwnerDoctorID = ownerDoctorID
	if r.ownerErr != nil {
		return nil, r.ownerErr
	}
	owner, ok := r.registrations[registrationID]
	if !ok || owner.DoctorID != ownerDoctorID {
		// 挂号不存在或不属于该医生：统一按不存在处理，避免用状态码枚举他人数据。
		return nil, domainmedicalrecord.ErrRegistrationNotFound
	}
	copied := owner
	return &copied, nil
}

func (r *medicalRecordHandlerRepo) CreateMedicalRecord(
	_ context.Context,
	owner domainmedicalrecord.RegistrationOwner,
	recordUUID, diagnosis, content string,
) (*domainmedicalrecord.MedicalRecord, error) {
	r.createCalls++
	r.lastCreateOwner = owner
	if r.createErr != nil {
		return nil, r.createErr
	}
	for _, existing := range r.records {
		if existing.RegistrationID == owner.RegistrationID {
			return nil, domainmedicalrecord.ErrDuplicate
		}
	}
	r.nextID++
	record := &domainmedicalrecord.MedicalRecord{
		ID:              r.nextID,
		UUID:            recordUUID,
		RegistrationID:  owner.RegistrationID,
		PatientCardID:   owner.PatientCardID,
		DoctorID:        owner.DoctorID,
		SubdepartmentID: owner.SubdepartmentID,
		Diagnosis:       diagnosis,
		Content:         content,
	}
	r.records[record.ID] = record
	copied := *record
	return &copied, nil
}

func (r *medicalRecordHandlerRepo) ListMedicalRecords(
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

	matched := make([]domainmedicalrecord.MedicalRecord, 0)
	for _, record := range r.records {
		owner, ok := r.registrations[record.RegistrationID]
		if !ok || owner.DoctorID != filter.OwnerDoctorID {
			continue
		}
		if filter.RegistrationID != nil && record.RegistrationID != *filter.RegistrationID {
			continue
		}
		if filter.PatientCardID != nil && record.PatientCardID != *filter.PatientCardID {
			continue
		}
		if filter.DoctorID != nil && record.DoctorID != *filter.DoctorID {
			continue
		}
		matched = append(matched, *record)
	}
	// 与 PostgreSQL 实现一致：固定按 id 倒序（最近书写的在前）。
	for i := 0; i < len(matched); i++ {
		for j := i + 1; j < len(matched); j++ {
			if matched[j].ID > matched[i].ID {
				matched[i], matched[j] = matched[j], matched[i]
			}
		}
	}
	total := int64(len(matched))
	if offset >= len(matched) {
		return []domainmedicalrecord.MedicalRecord{}, total, nil
	}
	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[offset:end], total, nil
}

func (r *medicalRecordHandlerRepo) FindMedicalRecord(
	_ context.Context,
	id, ownerDoctorID int64,
) (*domainmedicalrecord.MedicalRecord, error) {
	r.findCalls++
	r.lastFindID = id
	r.lastFindDoctorID = ownerDoctorID
	if r.findErr != nil {
		return nil, r.findErr
	}
	record, ok := r.records[id]
	if !ok {
		return nil, domainmedicalrecord.ErrNotFound
	}
	if owner, exists := r.registrations[record.RegistrationID]; !exists || owner.DoctorID != ownerDoctorID {
		return nil, domainmedicalrecord.ErrNotFound
	}
	copied := *record
	return &copied, nil
}

func (r *medicalRecordHandlerRepo) UpdateMedicalRecord(
	_ context.Context,
	id, ownerDoctorID int64,
	input domainmedicalrecord.UpdateInput,
) (*domainmedicalrecord.MedicalRecord, error) {
	r.updateCalls++
	r.lastUpdateID = id
	r.lastUpdateDoctor = ownerDoctorID
	r.lastUpdateInput = input
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	record, ok := r.records[id]
	if !ok {
		return nil, domainmedicalrecord.ErrNotFound
	}
	if owner, exists := r.registrations[record.RegistrationID]; !exists || owner.DoctorID != ownerDoctorID {
		return nil, domainmedicalrecord.ErrNotFound
	}
	if input.Diagnosis != nil {
		record.Diagnosis = *input.Diagnosis
	}
	if input.Content != nil {
		record.Content = *input.Content
	}
	copied := *record
	return &copied, nil
}

func (r *medicalRecordHandlerRepo) DeleteMedicalRecord(_ context.Context, id, ownerDoctorID int64) error {
	r.deleteCalls++
	r.lastDeleteID = id
	r.lastDeleteDoctor = ownerDoctorID
	if r.deleteErr != nil {
		return r.deleteErr
	}
	record, ok := r.records[id]
	if !ok {
		return domainmedicalrecord.ErrNotFound
	}
	if owner, exists := r.registrations[record.RegistrationID]; !exists || owner.DoctorID != ownerDoctorID {
		return domainmedicalrecord.ErrNotFound
	}
	delete(r.records, id)
	return nil
}

// seedRecord 直接写入一份病历，用于构造详情/重复书写/越权场景。
func (r *medicalRecordHandlerRepo) seedRecord(record domainmedicalrecord.MedicalRecord) *domainmedicalrecord.MedicalRecord {
	if record.ID == 0 {
		r.nextID++
		record.ID = r.nextID
	}
	r.records[record.ID] = &record
	return &record
}

// medicalRecordHandlerEnv 汇总被测处理器与三个 realm 的引擎。
type medicalRecordHandlerEnv struct {
	repo     *medicalRecordHandlerRepo
	store    *fakeIdemStore
	handler  *MedicalRecordHandler
	mis      *gin.Engine
	patient  *gin.Engine
	noClaims *gin.Engine
}

// newMedicalRecordHandlerEnv 构造 handler 测试环境：
// mis_user.id=7 绑定医生 16，挂号 1001 归属医生 16，挂号 2002 归属医生 99。
func newMedicalRecordHandlerEnv(t *testing.T) *medicalRecordHandlerEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	repo := &medicalRecordHandlerRepo{
		doctorIDByUser: map[int64]int64{medicalRecordMisUserID: medicalRecordDoctorID},
		registrations: map[int64]domainmedicalrecord.RegistrationOwner{
			medicalRecordRegistrationID: {
				RegistrationID:  medicalRecordRegistrationID,
				PatientCardID:   501,
				DoctorID:        medicalRecordDoctorID,
				SubdepartmentID: 2,
			},
			medicalRecordOtherRegistrationID: {
				RegistrationID:  medicalRecordOtherRegistrationID,
				PatientCardID:   502,
				DoctorID:        medicalRecordOtherDoctorID,
				SubdepartmentID: 3,
			},
		},
		records: make(map[int64]*domainmedicalrecord.MedicalRecord),
		nextID:  1000,
	}

	store := newFakeIdemStore()
	service := medicalrecordservice.NewService(repo)
	h := NewMedicalRecordHandler(service, store)

	return &medicalRecordHandlerEnv{
		repo:     repo,
		store:    store,
		handler:  h,
		mis:      medicalRecordHandlerEngine(t, h, domainauth.RealmMis, medicalRecordMisUserID),
		patient:  medicalRecordHandlerEngine(t, h, domainauth.RealmPatient, 20),
		noClaims: medicalRecordHandlerEngine(t, h, "", 0),
	}
}

// medicalRecordHandlerEngine 按 router.go 的挂载方式注册五个病历路由；
// realm 为空时不注入 claims，用于验证未携带令牌的运行路径。
func medicalRecordHandlerEngine(
	t *testing.T,
	h *MedicalRecordHandler,
	realm domainauth.Realm,
	userID int64,
) *gin.Engine {
	t.Helper()

	engine := gin.New()
	group := engine.Group("/api/v1/medical-records")
	if realm != "" {
		group.Use(func(c *gin.Context) {
			c.Set(middleware.ClaimsKey, &authsession.AccessClaims{
				UserID:    userID,
				Username:  "medical-record-test",
				TokenType: authsession.TokenTypeAccess,
				Realm:     realm,
			})
			c.Next()
		})
	}
	group.GET("", h.List)
	group.GET("/:medicalRecordId", h.Detail)
	group.POST("", h.Create)
	group.PATCH("/:medicalRecordId", h.Update)
	group.DELETE("/:medicalRecordId", h.Delete)
	return engine
}

// performMedicalRecord 发起病历请求；body 为空表示不带请求体，headers 为附加请求头。
func performMedicalRecord(
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

// decodeMedicalRecordBody 解析响应体为通用 JSON 对象。
func decodeMedicalRecordBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	return mustJSON(t, w.Body.String())
}

// assertMedicalRecordErrorCode 断言错误响应体中的 code 字段。
func assertMedicalRecordErrorCode(t *testing.T, w *httptest.ResponseRecorder, wantCode string) map[string]any {
	t.Helper()
	body := decodeMedicalRecordBody(t, w)
	if body["code"] != wantCode {
		t.Fatalf("code = %v, want %v；body=%s", body["code"], wantCode, w.Body.String())
	}
	return body
}

// TestMedicalRecordCreateSuccess 书写成功：201 + Location 头 + 完整资源字段，
// 归属字段（就诊卡/医生/子科室）全部来自挂号行，客户端只提交 registrationId 与内容。
func TestMedicalRecordCreateSuccess(t *testing.T) {
	env := newMedicalRecordHandlerEnv(t)

	w := performMedicalRecord(env.mis, http.MethodPost, "/api/v1/medical-records",
		`{"registrationId":1001,"diagnosis":"  牙髓炎  ","content":"  主诉：牙痛\n处理：根管治疗  "}`,
		map[string]string{"Idempotency-Key": "create-success"})

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if location := w.Header().Get("Location"); location != "/api/v1/medical-records/1001" {
		t.Errorf("Location = %q, want /api/v1/medical-records/1001", location)
	}
	body := decodeMedicalRecordBody(t, w)
	if body["id"] != float64(1001) {
		t.Errorf("id = %v, want 1001", body["id"])
	}
	uuid, _ := body["uuid"].(string)
	if len(uuid) != 32 || !strings.HasPrefix(uuid, "RX") {
		t.Errorf("uuid = %q, want RX 前缀 + 30 位十六进制", uuid)
	}
	if body["registrationId"] != float64(1001) || body["patientCardId"] != float64(501) ||
		body["doctorId"] != float64(16) || body["subdepartmentId"] != float64(2) {
		t.Errorf("归属字段 = %v，期望来自挂号行（就诊卡 501 / 医生 16 / 子科室 2）", body)
	}
	if body["diagnosis"] != "牙髓炎" {
		t.Errorf("diagnosis = %v, want 已归一化的 牙髓炎", body["diagnosis"])
	}
	if body["content"] != "主诉：牙痛\n处理：根管治疗" {
		t.Errorf("content = %v, want 裁剪首尾空白但保留内部换行", body["content"])
	}
	if env.repo.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", env.repo.createCalls)
	}
	if env.repo.lastCreateOwner.DoctorID != medicalRecordDoctorID {
		t.Errorf("落库归属医生 = %d, want %d", env.repo.lastCreateOwner.DoctorID, medicalRecordDoctorID)
	}
	if env.repo.lastOwnerDoctorID != medicalRecordDoctorID {
		t.Errorf("挂号归属过滤条件 = %d, want %d（来自令牌主体）",
			env.repo.lastOwnerDoctorID, medicalRecordDoctorID)
	}
}

// TestMedicalRecordCreateRejections 覆盖书写接口的全部拒绝分支。
func TestMedicalRecordCreateRejections(t *testing.T) {
	const validBody = `{"registrationId":1001,"diagnosis":"牙髓炎","content":"主线：牙痛"}`

	cases := []struct {
		name     string
		body     string
		headers  map[string]string
		prepare  func(env *medicalRecordHandlerEnv)
		wantCode string
		wantMsg  string
	}{
		{
			name:     "挂号不存在",
			body:     `{"registrationId":9999,"diagnosis":"牙髓炎","content":"正文"}`,
			headers:  map[string]string{"Idempotency-Key": "create-missing-registration"},
			wantCode: medicalrecordservice.CodeRegistrationNotFound,
			wantMsg:  "挂号记录不存在",
		},
		{
			name:     "挂号不由当前医生负责",
			body:     `{"registrationId":2002,"diagnosis":"牙髓炎","content":"正文"}`,
			headers:  map[string]string{"Idempotency-Key": "create-other-doctor"},
			wantCode: medicalrecordservice.CodeRegistrationNotFound,
			wantMsg:  "挂号记录不存在",
		},
		{
			name:    "同一挂号已有病历",
			body:    validBody,
			headers: map[string]string{"Idempotency-Key": "create-duplicate"},
			prepare: func(env *medicalRecordHandlerEnv) {
				env.repo.seedRecord(domainmedicalrecord.MedicalRecord{
					RegistrationID: medicalRecordRegistrationID, DoctorID: medicalRecordDoctorID,
				})
			},
			wantCode: medicalrecordservice.CodeDuplicate,
			wantMsg:  "该挂号已有病历，请改用修改接口",
		},
		{
			name:    "账号未绑定医生",
			body:    validBody,
			headers: map[string]string{"Idempotency-Key": "create-unbound-doctor"},
			prepare: func(env *medicalRecordHandlerEnv) {
				env.repo.doctorIDByUser[medicalRecordMisUserID] = 0
			},
			wantCode: medicalrecordservice.CodeForbidden,
			wantMsg:  "当前账号未关联医生，无法访问病历数据",
		},
		{
			name:    "仓储故障",
			body:    validBody,
			headers: map[string]string{"Idempotency-Key": "create-dependency"},
			prepare: func(env *medicalRecordHandlerEnv) {
				env.repo.createErr = errMedicalRecordDependency
			},
			wantCode: medicalrecordservice.CodeDependencyUnavailable,
		},
	}

	// 每种拒绝分支的期望 HTTP 状态码。
	wantStatus := map[string]int{
		medicalrecordservice.CodeRegistrationNotFound:  http.StatusNotFound,
		medicalrecordservice.CodeDuplicate:             http.StatusConflict,
		medicalrecordservice.CodeForbidden:             http.StatusForbidden,
		medicalrecordservice.CodeDependencyUnavailable: http.StatusBadGateway,
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMedicalRecordHandlerEnv(t)
			if tc.prepare != nil {
				tc.prepare(env)
			}

			w := performMedicalRecord(env.mis, http.MethodPost, "/api/v1/medical-records", tc.body, tc.headers)

			if want := wantStatus[tc.wantCode]; w.Code != want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, want, w.Body.String())
			}
			body := assertMedicalRecordErrorCode(t, w, tc.wantCode)
			if tc.wantMsg != "" && body["message"] != tc.wantMsg {
				t.Errorf("message = %v, want %q", body["message"], tc.wantMsg)
			}
		})
	}
}

// TestMedicalRecordList 覆盖列表查询：只返回当前医生负责的挂号下的病历，
// 归属条件强制下发，分页 envelope 与过滤条件与契约一致。
func TestMedicalRecordList(t *testing.T) {
	env := newMedicalRecordHandlerEnv(t)
	env.repo.seedRecord(domainmedicalrecord.MedicalRecord{
		RegistrationID: medicalRecordRegistrationID, PatientCardID: 501,
		DoctorID: medicalRecordDoctorID, SubdepartmentID: 2, Diagnosis: "旧诊断",
	})
	env.repo.seedRecord(domainmedicalrecord.MedicalRecord{
		RegistrationID: medicalRecordRegistrationID, PatientCardID: 501,
		DoctorID: medicalRecordDoctorID, SubdepartmentID: 2, Diagnosis: "新诊断",
	})
	// 他人医生的病历不得出现在结果里。
	env.repo.registrations[medicalRecordOtherRegistrationID] = domainmedicalrecord.RegistrationOwner{
		RegistrationID: medicalRecordOtherRegistrationID, DoctorID: medicalRecordOtherDoctorID,
	}
	env.repo.seedRecord(domainmedicalrecord.MedicalRecord{
		RegistrationID: medicalRecordOtherRegistrationID, DoctorID: medicalRecordOtherDoctorID,
	})

	w := performMedicalRecord(env.mis, http.MethodGet, "/api/v1/medical-records?page=1&pageSize=20", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeMedicalRecordBody(t, w)
	if body["total"] != float64(2) {
		t.Errorf("total = %v, want 2（只统计当前医生的病历）", body["total"])
	}
	if body["page"] != float64(1) || body["pageSize"] != float64(20) {
		t.Errorf("page/pageSize = %v/%v, want 1/20", body["page"], body["pageSize"])
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %v, want 长度 2 的数组", body["items"])
	}
	first := items[0].(map[string]any)
	if first["diagnosis"] != "新诊断" {
		t.Errorf("items[0].diagnosis = %v, want 新诊断（按 id 倒序）", first["diagnosis"])
	}
	if env.repo.lastFilter.OwnerDoctorID != medicalRecordDoctorID {
		t.Errorf("归属过滤 = %d, want %d", env.repo.lastFilter.OwnerDoctorID, medicalRecordDoctorID)
	}

	// 带过滤条件：只保留指定挂号下的病历。
	w = performMedicalRecord(env.mis, http.MethodGet, "/api/v1/medical-records?registrationId=1001", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("带过滤条件 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body = decodeMedicalRecordBody(t, w)
	if body["total"] != float64(2) {
		t.Errorf("registrationId=1001 total = %v, want 2", body["total"])
	}
	if env.repo.lastFilter.RegistrationID == nil || *env.repo.lastFilter.RegistrationID != medicalRecordRegistrationID {
		t.Errorf("registrationId 过滤 = %v, want %d", env.repo.lastFilter.RegistrationID, medicalRecordRegistrationID)
	}
}

// TestMedicalRecordListEmptyItems 空结果必须输出 items: []（而不是 null），并保留分页兜底值。
func TestMedicalRecordListEmptyItems(t *testing.T) {
	env := newMedicalRecordHandlerEnv(t)

	w := performMedicalRecord(env.mis, http.MethodGet, "/api/v1/medical-records", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Errorf("空列表必须输出 items:[]；body=%s", w.Body.String())
	}
}

// TestMedicalRecordListRejectsSortAndOrder 显式拒绝排序参数：
// doctor_prescription 没有时间列，排序固定为 id 倒序，提交 sort/order 一律 422，
// 且不得触达仓储（避免客户端误以为排序生效）。
func TestMedicalRecordListRejectsSortAndOrder(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		wantMsg string
	}{
		{name: "sort 参数", query: "?sort=id", wantMsg: "本接口不支持 sort 参数（固定按 id 倒序）"},
		{name: "sort 空值", query: "?sort=", wantMsg: "本接口不支持 sort 参数（固定按 id 倒序）"},
		{name: "order 参数", query: "?order=desc", wantMsg: "本接口不支持 order 参数（固定按 id 倒序）"},
		{name: "order 空值", query: "?order=", wantMsg: "本接口不支持 order 参数（固定按 id 倒序）"},
		{name: "page 合法但带 order", query: "?page=2&pageSize=10&order=desc", wantMsg: "本接口不支持 order 参数（固定按 id 倒序）"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMedicalRecordHandlerEnv(t)

			w := performMedicalRecord(env.mis, http.MethodGet, "/api/v1/medical-records"+tc.query, "", nil)

			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
			}
			body := assertMedicalRecordErrorCode(t, w, medicalrecordservice.CodeValidationFailed)
			if body["message"] != tc.wantMsg {
				t.Errorf("message = %v, want %q", body["message"], tc.wantMsg)
			}
			if env.repo.listCalls != 0 {
				t.Errorf("排序参数被拒时不得查询仓储：listCalls = %d, want 0", env.repo.listCalls)
			}
		})
	}
}

// TestMedicalRecordDetail 覆盖详情：本人负责的挂号下的病历 200；不存在或越权统一 404；
// 编号非正整数为 422。
func TestMedicalRecordDetail(t *testing.T) {
	t.Run("读取成功", func(t *testing.T) {
		env := newMedicalRecordHandlerEnv(t)
		seed := env.repo.seedRecord(domainmedicalrecord.MedicalRecord{
			UUID: "RX000000000000000000000000000001", RegistrationID: medicalRecordRegistrationID,
			PatientCardID: 501, DoctorID: medicalRecordDoctorID, SubdepartmentID: 2,
			Diagnosis: "牙髓炎", Content: "主诉：牙痛",
		})

		w := performMedicalRecord(env.mis, http.MethodGet,
			"/api/v1/medical-records/"+formatMedicalRecordID(seed.ID), "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		body := decodeMedicalRecordBody(t, w)
		if body["id"] != float64(seed.ID) || body["diagnosis"] != "牙髓炎" || body["content"] != "主诉：牙痛" {
			t.Errorf("详情字段 = %v", body)
		}
		if env.repo.lastFindDoctorID != medicalRecordDoctorID {
			t.Errorf("详情归属过滤 = %d, want %d", env.repo.lastFindDoctorID, medicalRecordDoctorID)
		}
	})

	t.Run("不存在返回 404", func(t *testing.T) {
		env := newMedicalRecordHandlerEnv(t)

		w := performMedicalRecord(env.mis, http.MethodGet, "/api/v1/medical-records/88888", "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
		}
		assertMedicalRecordErrorCode(t, w, medicalrecordservice.CodeNotFound)
	})

	t.Run("越权读取他人病历返回 404", func(t *testing.T) {
		env := newMedicalRecordHandlerEnv(t)
		seed := env.repo.seedRecord(domainmedicalrecord.MedicalRecord{
			RegistrationID: medicalRecordOtherRegistrationID, DoctorID: medicalRecordOtherDoctorID,
		})

		w := performMedicalRecord(env.mis, http.MethodGet,
			"/api/v1/medical-records/"+formatMedicalRecordID(seed.ID), "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404（越权与不存在同码）; body=%s", w.Code, w.Body.String())
		}
		assertMedicalRecordErrorCode(t, w, medicalrecordservice.CodeNotFound)
	})

	t.Run("编号非正整数返回 422", func(t *testing.T) {
		env := newMedicalRecordHandlerEnv(t)

		for _, raw := range []string{"0", "-1", "abc"} {
			w := performMedicalRecord(env.mis, http.MethodGet, "/api/v1/medical-records/"+raw, "", nil)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("编号 %q status = %d, want 422; body=%s", raw, w.Code, w.Body.String())
			}
			assertMedicalRecordErrorCode(t, w, medicalrecordservice.CodeValidationFailed)
		}
	})
}

// TestMedicalRecordUpdate 覆盖 PATCH：只更新提交的字段；两字段都为 nil 为 422；
// 空串命中领域校验；不存在或越权返回 404。
func TestMedicalRecordUpdate(t *testing.T) {
	t.Run("只修改诊断时保留原正文", func(t *testing.T) {
		env := newMedicalRecordHandlerEnv(t)
		seed := env.repo.seedRecord(domainmedicalrecord.MedicalRecord{
			RegistrationID: medicalRecordRegistrationID, DoctorID: medicalRecordDoctorID,
			Diagnosis: "旧诊断", Content: "原正文",
		})

		w := performMedicalRecord(env.mis, http.MethodPatch,
			"/api/v1/medical-records/"+formatMedicalRecordID(seed.ID),
			`{"diagnosis":"  新诊断  "}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		body := decodeMedicalRecordBody(t, w)
		if body["diagnosis"] != "新诊断" || body["content"] != "原正文" {
			t.Errorf("修改结果 = %v, want 诊断已更新且正文保留", body)
		}
		if env.repo.lastUpdateInput.Content != nil {
			t.Errorf("未提交的 content 不得下发给仓储，实际 %v", *env.repo.lastUpdateInput.Content)
		}
		if env.repo.lastUpdateDoctor != medicalRecordDoctorID {
			t.Errorf("修改归属过滤 = %d, want %d", env.repo.lastUpdateDoctor, medicalRecordDoctorID)
		}
	})

	t.Run("两字段都为 nil 返回 422", func(t *testing.T) {
		env := newMedicalRecordHandlerEnv(t)
		seed := env.repo.seedRecord(domainmedicalrecord.MedicalRecord{
			RegistrationID: medicalRecordRegistrationID, DoctorID: medicalRecordDoctorID,
		})

		w := performMedicalRecord(env.mis, http.MethodPatch,
			"/api/v1/medical-records/"+formatMedicalRecordID(seed.ID), `{}`, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		assertMedicalRecordErrorCode(t, w, medicalrecordservice.CodeValidationFailed)
	})

	t.Run("空串命中领域校验", func(t *testing.T) {
		env := newMedicalRecordHandlerEnv(t)
		seed := env.repo.seedRecord(domainmedicalrecord.MedicalRecord{
			RegistrationID: medicalRecordRegistrationID, DoctorID: medicalRecordDoctorID,
		})

		w := performMedicalRecord(env.mis, http.MethodPatch,
			"/api/v1/medical-records/"+formatMedicalRecordID(seed.ID), `{"content":"   "}`, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		assertMedicalRecordErrorCode(t, w, medicalrecordservice.CodeValidationFailed)
	})

	t.Run("不存在或越权返回 404", func(t *testing.T) {
		env := newMedicalRecordHandlerEnv(t)

		w := performMedicalRecord(env.mis, http.MethodPatch,
			"/api/v1/medical-records/88888", `{"diagnosis":"新诊断"}`, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
		}
		assertMedicalRecordErrorCode(t, w, medicalrecordservice.CodeNotFound)
	})
}

// TestMedicalRecordDelete 覆盖删除：成功 204 且无响应体并真正删除；
// 不存在或越权返回 404。
func TestMedicalRecordDelete(t *testing.T) {
	t.Run("删除成功返回 204", func(t *testing.T) {
		env := newMedicalRecordHandlerEnv(t)
		seed := env.repo.seedRecord(domainmedicalrecord.MedicalRecord{
			RegistrationID: medicalRecordRegistrationID, DoctorID: medicalRecordDoctorID,
		})

		w := performMedicalRecord(env.mis, http.MethodDelete,
			"/api/v1/medical-records/"+formatMedicalRecordID(seed.ID), "", nil)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
		}
		if w.Body.Len() != 0 {
			t.Errorf("204 不应有响应体: %q", w.Body.String())
		}
		if _, exists := env.repo.records[seed.ID]; exists {
			t.Error("删除后仓储中不应再有该病历")
		}
		if env.repo.lastDeleteDoctor != medicalRecordDoctorID {
			t.Errorf("删除归属过滤 = %d, want %d", env.repo.lastDeleteDoctor, medicalRecordDoctorID)
		}
	})

	t.Run("不存在或越权返回 404", func(t *testing.T) {
		env := newMedicalRecordHandlerEnv(t)

		w := performMedicalRecord(env.mis, http.MethodDelete, "/api/v1/medical-records/88888", "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
		}
		assertMedicalRecordErrorCode(t, w, medicalrecordservice.CodeNotFound)
	})
}

// formatMedicalRecordID 把主键转成路径片段（避免依赖其它测试文件的辅助函数）。
func formatMedicalRecordID(id int64) string {
	if id == 0 {
		return "0"
	}
	digits := make([]byte, 0, 20)
	for id > 0 {
		digits = append([]byte{byte('0' + id%10)}, digits...)
		id /= 10
	}
	return string(digits)
}
