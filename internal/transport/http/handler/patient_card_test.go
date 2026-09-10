// 就诊卡 HTTP 契约单测：GET 列表/详情、POST 创建、PATCH 修改的状态码、
// 错误码、message 与 details.fields（spec/04-api-contract.md §1.4、§7.4、§10、§12.5）。
//
// gin 处于 TestMode，路由按 router.go 的方式挂载，中间件里直接注入
// realm=patient 的 claims 模拟「访问令牌已校验通过」；仓储用内存桩替换，
// 不依赖 PostgreSQL/Redis。
package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/domain/patient"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/usecase/authsession"
	patientcardservice "Medical-Web-Backend/internal/usecase/patientcard"
)

const (
	// patientCardTestPatientID 是测试用患者账号主键，写入注入的 claims。
	patientCardTestPatientID int64 = 20
	// patientCardTestPID 是契约 §12.5 示例中的身份证号。
	patientCardTestPID = "110101199001011237"
	// patientCardTestMaskedPID 是该身份证号脱敏后的结果。
	patientCardTestMaskedPID = "110101********1237"
	// patientCardTestTel 是契约示例中的联系电话。
	patientCardTestTel = "13800138000"
	// patientCardTestUUID 是契约示例中的就诊卡 UUID。
	patientCardTestUUID = "CARD0000000000000000000000000010"
)

// patientCardTestRepo 是内存版 port.PatientCardRepository 桩。
//
// 只实现 handler 链路真正调用的四个方法，其余方法由内嵌接口提供。
type patientCardTestRepo struct {
	port.PatientCardRepository

	cards  map[int64]patient.Card
	nextID int64

	// 调用计数：用于断言参数校验失败时不触达仓储。
	listCalls   int
	findCalls   int
	createCalls int
	updateCalls int

	// 故障注入：非 nil 时对应方法恒定失败，模拟 PostgreSQL 不可用。
	listErr   error
	findErr   error
	createErr error
	updateErr error
}

func newPatientCardTestRepo() *patientCardTestRepo {
	return &patientCardTestRepo{cards: map[int64]patient.Card{}}
}

// seed 预置一张就诊卡；未指定主键时由桩自增分配。
func (r *patientCardTestRepo) seed(card patient.Card) patient.Card {
	if card.ID == 0 {
		r.nextID++
		card.ID = r.nextID
	} else if card.ID > r.nextID {
		r.nextID = card.ID
	}
	r.cards[card.ID] = card
	return card
}

// contractCard 返回契约 §12.5 示例中的就诊卡（id=10，属主为测试患者）。
func contractCard() patient.Card {
	return patient.Card{
		ID:             10,
		UserID:         patientCardTestPatientID,
		UUID:           patientCardTestUUID,
		Name:           "张三",
		Sex:            patient.SexMale,
		PID:            patientCardTestPID,
		Tel:            patientCardTestTel,
		Birthday:       "1990-01-01",
		MedicalHistory: []string{"高血压", "糖尿病"},
		InsuranceType:  "社会基本医疗保险",
	}
}

func (r *patientCardTestRepo) ListCardsByPatient(
	_ context.Context,
	patientID int64,
	offset, limit int,
) ([]patient.Card, int64, error) {
	r.listCalls++
	if r.listErr != nil {
		return nil, 0, r.listErr
	}

	owned := make([]patient.Card, 0)
	for _, card := range r.cards {
		if card.UserID == patientID {
			owned = append(owned, card)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].ID < owned[j].ID })

	total := int64(len(owned))
	if offset >= len(owned) {
		return []patient.Card{}, total, nil
	}
	end := offset + limit
	if end > len(owned) {
		end = len(owned)
	}
	return owned[offset:end], total, nil
}

func (r *patientCardTestRepo) FindCardByID(_ context.Context, cardID int64) (*patient.Card, error) {
	r.findCalls++
	if r.findErr != nil {
		return nil, r.findErr
	}
	card, ok := r.cards[cardID]
	if !ok {
		return nil, patient.ErrCardNotFound
	}
	copied := card
	return &copied, nil
}

func (r *patientCardTestRepo) CreateCard(_ context.Context, card patient.Card) (*patient.Card, error) {
	r.createCalls++
	if r.createErr != nil {
		return nil, r.createErr
	}
	for _, existing := range r.cards {
		if existing.UserID == card.UserID {
			return nil, patient.ErrCardExists
		}
	}
	r.nextID++
	card.ID = r.nextID
	r.cards[card.ID] = card
	copied := card
	return &copied, nil
}

func (r *patientCardTestRepo) UpdateCard(
	_ context.Context,
	cardID int64,
	update patient.CardUpdate,
) (*patient.Card, error) {
	r.updateCalls++
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	base, ok := r.cards[cardID]
	if !ok {
		return nil, patient.ErrCardNotFound
	}
	// 与真实仓储一致：不可修改字段与字段级校验都在 ApplyUpdate 内判定，
	// 出错时不写入任何列。
	updated, err := base.ApplyUpdate(update)
	if err != nil {
		return nil, err
	}
	r.cards[cardID] = updated
	copied := updated
	return &copied, nil
}

// patientCardTestEnv 汇总挂载了就诊卡路由的引擎与其依赖桩。
type patientCardTestEnv struct {
	service *patientcardservice.Service
	repo    *patientCardTestRepo
	// authed 模拟 realm=patient 的访问令牌已校验通过。
	authed *gin.Engine
	// noClaims 不注入 claims，模拟访问令牌中间件缺失或被绕过。
	noClaims *gin.Engine
}

func newPatientCardTestEnv(t *testing.T) *patientCardTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	repo := newPatientCardTestRepo()
	service := patientcardservice.NewService(repo)
	h := NewPatientCardHandler(service)

	return &patientCardTestEnv{
		service:  service,
		repo:     repo,
		authed:   patientCardTestEngine(t, h, true),
		noClaims: patientCardTestEngine(t, h, false),
	}
}

// patientCardTestEngine 按 router.go 的挂载方式注册四个就诊卡路由；
// withClaims=true 时模拟 realm=patient 的令牌中间件已写入 claims。
func patientCardTestEngine(t *testing.T, h *PatientCardHandler, withClaims bool) *gin.Engine {
	t.Helper()

	engine := gin.New()
	group := engine.Group("/api/v1/patient")
	if withClaims {
		group.Use(func(c *gin.Context) {
			c.Set(middleware.ClaimsKey, &authsession.AccessClaims{
				UserID:    patientCardTestPatientID,
				Username:  "patient-20",
				TokenType: authsession.TokenTypeAccess,
				Realm:     domainauth.RealmPatient,
			})
			c.Next()
		})
	}
	group.GET("/cards", h.List)
	group.POST("/cards", h.Create)
	group.GET("/cards/:cardId", h.Detail)
	group.PATCH("/cards/:cardId", h.Update)
	return engine
}

// request 以已认证引擎发起请求；body 非空时作为 JSON 请求体。
func (e *patientCardTestEnv) request(
	method, path, body string,
) *httptest.ResponseRecorder {
	return performPatientCardRequest(e.authed, method, path, body)
}

// performPatientCardRequest 在指定引擎上发起请求。
func performPatientCardRequest(
	engine *gin.Engine,
	method, path, body string,
) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// patientCardNumber 取出响应体中的数值字段（JSON 数字解码为 float64）。
func patientCardNumber(t *testing.T, body map[string]any, key string) float64 {
	t.Helper()
	value, ok := body[key].(float64)
	if !ok {
		t.Fatalf("字段 %s = %v，期望数值；body=%v", key, body[key], body)
	}
	return value
}

// patientCardObjects 取出响应体中的对象数组字段。
func patientCardObjects(t *testing.T, body map[string]any, key string) []any {
	t.Helper()
	value, ok := body[key].([]any)
	if !ok {
		t.Fatalf("字段 %s = %v，期望 JSON 数组；body=%v", key, body[key], body)
	}
	return value
}

// patientCardItem 取出数组字段的第一个元素（对象）。
func patientCardItem(t *testing.T, body map[string]any, key string) map[string]any {
	t.Helper()
	items := patientCardObjects(t, body, key)
	if len(items) != 1 {
		t.Fatalf("字段 %s 长度 = %d，期望 1；body=%v", key, len(items), body)
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("字段 %s[0] = %v，期望对象", key, items[0])
	}
	return item
}

// assertCardMessage 断言错误响应的 message 与契约文案完全一致。
func assertCardMessage(t *testing.T, body map[string]any, want string) {
	t.Helper()
	if body["message"] != want {
		t.Fatalf("message = %v，期望 %q；body=%v", body["message"], want, body)
	}
}

// assertCardFields 断言 details.fields 与期望字段清单完全一致（含顺序）。
func assertCardFields(t *testing.T, body map[string]any, want ...string) {
	t.Helper()
	details, ok := body["details"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 details：%v", body)
	}
	fields, ok := details["fields"].([]any)
	if !ok {
		t.Fatalf("响应缺少 details.fields：%v", body)
	}
	if len(fields) != len(want) {
		t.Fatalf("details.fields = %v，期望 %v", fields, want)
	}
	for i, expected := range want {
		if fields[i] != expected {
			t.Fatalf("details.fields[%d] = %v，期望 %q（完整值 %v）", i, fields[i], expected, fields)
		}
	}
}

// --- GET /api/v1/patient/cards ---

// TestPatientCardListSuccess 覆盖契约 §12.5 的正确输出：分页元数据、
// pid 脱敏、tel 明文，且响应体中不出现完整身份证号。
func TestPatientCardListSuccess(t *testing.T) {
	env := newPatientCardTestEnv(t)
	env.repo.seed(contractCard())

	w := env.request(http.MethodGet, "/api/v1/patient/cards", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	if got := patientCardNumber(t, body, "page"); got != 1 {
		t.Errorf("page = %v，期望 1", got)
	}
	if got := patientCardNumber(t, body, "pageSize"); got != 20 {
		t.Errorf("pageSize = %v，期望 20", got)
	}
	if got := patientCardNumber(t, body, "total"); got != 1 {
		t.Errorf("total = %v，期望 1", got)
	}

	item := patientCardItem(t, body, "items")
	if item["id"] != float64(10) {
		t.Errorf("items[0].id = %v，期望 10", item["id"])
	}
	if item["userId"] != float64(patientCardTestPatientID) {
		t.Errorf("items[0].userId = %v，期望 %d", item["userId"], patientCardTestPatientID)
	}
	if item["uuid"] != patientCardTestUUID {
		t.Errorf("items[0].uuid = %v，期望 %s", item["uuid"], patientCardTestUUID)
	}
	if item["name"] != "张三" || item["sex"] != patient.SexMale {
		t.Errorf("items[0] 姓名/性别 = %v/%v，期望 张三/男", item["name"], item["sex"])
	}
	if item["pid"] != patientCardTestMaskedPID {
		t.Errorf("items[0].pid = %v，期望脱敏后的 %s", item["pid"], patientCardTestMaskedPID)
	}
	if item["tel"] != patientCardTestTel {
		t.Errorf("items[0].tel = %v，期望患者本人接口明文返回 %s", item["tel"], patientCardTestTel)
	}
	if item["birthday"] != "1990-01-01" {
		t.Errorf("items[0].birthday = %v，期望 1990-01-01", item["birthday"])
	}
	if item["insuranceType"] != "社会基本医疗保险" {
		t.Errorf("items[0].insuranceType = %v，期望 社会基本医疗保险", item["insuranceType"])
	}
	if item["existFaceModel"] != false {
		t.Errorf("items[0].existFaceModel = %v，期望 false", item["existFaceModel"])
	}
	history := patientCardObjects(t, item, "medicalHistory")
	if len(history) != 2 || history[0] != "高血压" || history[1] != "糖尿病" {
		t.Errorf("items[0].medicalHistory = %v，期望 [高血压 糖尿病]", history)
	}

	if strings.Contains(w.Body.String(), patientCardTestPID) {
		t.Errorf("响应体不得出现完整身份证号：%s", w.Body.String())
	}
}

// TestPatientCardListEmptyItemsIsArray 覆盖空列表：items 必须是 []，不能是 null
// （契约 §1.4）。
func TestPatientCardListEmptyItemsIsArray(t *testing.T) {
	env := newPatientCardTestEnv(t)

	w := env.request(http.MethodGet, "/api/v1/patient/cards", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	items := patientCardObjects(t, body, "items")
	if len(items) != 0 {
		t.Fatalf("items = %v，期望空数组", items)
	}
	if got := patientCardNumber(t, body, "total"); got != 0 {
		t.Errorf("total = %v，期望 0", got)
	}
}

// TestPatientCardListValidation 覆盖分页参数校验：非法参数返回 422
// REQUEST_VALIDATION_FAILED 与契约文案，且不触达仓储。
func TestPatientCardListValidation(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		message string
	}{
		{name: "page=0", query: "?page=0", message: "page 必须从 1 开始"},
		{name: "page 非整数", query: "?page=abc", message: "page 必须为正整数"},
		{name: "pageSize=101", query: "?pageSize=101", message: "pageSize 必须在 1 到 100 之间"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientCardTestEnv(t)

			w := env.request(http.MethodGet, "/api/v1/patient/cards"+tc.query, "")
			body := assertErrorStatus(
				t,
				w,
				http.StatusUnprocessableEntity,
				"REQUEST_VALIDATION_FAILED",
			)
			assertCardMessage(t, body, tc.message)
			if env.repo.listCalls != 0 {
				t.Errorf("参数校验失败不得触达仓储，实际调用 %d 次", env.repo.listCalls)
			}
		})
	}
}

// --- GET /api/v1/patient/cards/{cardId} ---

// TestPatientCardDetailSuccess 覆盖契约 §12.5 的正确输出：字段形状完整，
// pid 脱敏、tel 明文。
func TestPatientCardDetailSuccess(t *testing.T) {
	env := newPatientCardTestEnv(t)
	env.repo.seed(contractCard())

	w := env.request(http.MethodGet, "/api/v1/patient/cards/10", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	if body["id"] != float64(10) {
		t.Errorf("id = %v，期望 10", body["id"])
	}
	if body["userId"] != float64(patientCardTestPatientID) {
		t.Errorf("userId = %v，期望 %d", body["userId"], patientCardTestPatientID)
	}
	if body["pid"] != patientCardTestMaskedPID {
		t.Errorf("pid = %v，期望 %s", body["pid"], patientCardTestMaskedPID)
	}
	if body["tel"] != patientCardTestTel {
		t.Errorf("tel = %v，期望 %s", body["tel"], patientCardTestTel)
	}
	if body["birthday"] != "1990-01-01" || body["insuranceType"] != "社会基本医疗保险" {
		t.Errorf("birthday/insuranceType = %v/%v，期望 1990-01-01/社会基本医疗保险",
			body["birthday"], body["insuranceType"])
	}
	if body["existFaceModel"] != false {
		t.Errorf("existFaceModel = %v，期望 false", body["existFaceModel"])
	}
	if strings.Contains(w.Body.String(), patientCardTestPID) {
		t.Errorf("响应体不得出现完整身份证号：%s", w.Body.String())
	}
}

// TestPatientCardDetailHidesForeignAndMissing 覆盖越权与不存在：他人就诊卡与
// 不存在的卡都返回 404 PATIENT_CARD_NOT_FOUND 与统一文案，不暴露卡是否存在。
func TestPatientCardDetailHidesForeignAndMissing(t *testing.T) {
	env := newPatientCardTestEnv(t)
	others := env.repo.seed(patient.Card{
		ID:     77,
		UserID: 99,
		Name:   "他人",
		PID:    patientCardTestPID,
		Tel:    patientCardTestTel,
	})

	cases := []struct {
		name   string
		cardID int64
	}{
		{name: "他人就诊卡", cardID: others.ID},
		{name: "就诊卡不存在", cardID: 99999},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/v1/patient/cards/" + strconv.FormatInt(tc.cardID, 10)
			w := env.request(http.MethodGet, path, "")
			body := assertErrorStatus(t, w, http.StatusNotFound, "PATIENT_CARD_NOT_FOUND")
			assertCardMessage(t, body, "就诊卡不存在")
		})
	}
}

// TestPatientCardDetailRejectsInvalidCardID 覆盖路径参数非法：
// 非正整数的就诊卡编号返回 422 REQUEST_VALIDATION_FAILED。
func TestPatientCardDetailRejectsInvalidCardID(t *testing.T) {
	for _, raw := range []string{"abc", "0"} {
		t.Run("cardId="+raw, func(t *testing.T) {
			env := newPatientCardTestEnv(t)

			w := env.request(http.MethodGet, "/api/v1/patient/cards/"+raw, "")
			body := assertErrorStatus(
				t,
				w,
				http.StatusUnprocessableEntity,
				"REQUEST_VALIDATION_FAILED",
			)
			assertCardMessage(t, body, "就诊卡编号必须为正整数")
			if env.repo.findCalls != 0 {
				t.Errorf("路径参数非法不得触达仓储，实际调用 %d 次", env.repo.findCalls)
			}
		})
	}
}

// --- POST /api/v1/patient/cards ---

// TestPatientCardCreateSuccess 覆盖契约 §12.5 的正确输出：201、归属取自令牌、
// birthday 由身份证号推导、pid 脱敏。
func TestPatientCardCreateSuccess(t *testing.T) {
	env := newPatientCardTestEnv(t)

	const bodyText = `{"name":"张三","sex":"男","pid":"110101199001011237",` +
		`"tel":"13800138000","medicalHistory":["无"],"insuranceType":"社会基本医疗保险"}`

	w := env.request(http.MethodPost, "/api/v1/patient/cards", bodyText)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	if body["userId"] != float64(patientCardTestPatientID) {
		t.Errorf("userId = %v，期望 %d（归属只能取自访问令牌）", body["userId"], patientCardTestPatientID)
	}
	if body["pid"] != patientCardTestMaskedPID {
		t.Errorf("pid = %v，期望 %s", body["pid"], patientCardTestMaskedPID)
	}
	if body["tel"] != patientCardTestTel {
		t.Errorf("tel = %v，期望 %s", body["tel"], patientCardTestTel)
	}
	if body["birthday"] != "1990-01-01" {
		t.Errorf("birthday = %v，期望由身份证号推导的 1990-01-01", body["birthday"])
	}
	if body["existFaceModel"] != false {
		t.Errorf("existFaceModel = %v，期望 false", body["existFaceModel"])
	}
	uuid, ok := body["uuid"].(string)
	if !ok || len(uuid) != 32 || strings.Contains(uuid, "-") {
		t.Errorf("uuid = %v，期望服务端生成的 32 位无横线字符串", body["uuid"])
	}
	if strings.Contains(w.Body.String(), patientCardTestPID) {
		t.Errorf("响应体不得出现完整身份证号：%s", w.Body.String())
	}
	if env.repo.createCalls != 1 {
		t.Errorf("仓储创建次数 = %d，期望 1", env.repo.createCalls)
	}
}

// TestPatientCardCreateValidation 覆盖契约 §12.5 的三个 422 示例：
// 必填缺失、pid 与 tel 同时非法、性别与身份证号不一致，
// 状态码、错误码、message 与 details.fields 都必须逐条对齐。
func TestPatientCardCreateValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		message string
		fields  []string
	}{
		{
			name:    "缺 medicalHistory 与 insuranceType",
			body:    `{"name":"张三","sex":"男","pid":"110101199001011237","tel":"13800138000"}`,
			message: "就诊卡必填字段不完整",
			fields:  []string{"medicalHistory", "insuranceType"},
		},
		{
			name: "pid 与 tel 同时非法",
			body: `{"name":"张三","sex":"男","pid":"invalid","tel":"123",` +
				`"medicalHistory":["无"],"insuranceType":"无"}`,
			message: "身份证号或联系电话格式不正确",
			fields:  []string{"pid", "tel"},
		},
		{
			name: "性别与身份证号不一致",
			body: `{"name":"张三","sex":"女","pid":"110101199001011237",` +
				`"tel":"13800138000","medicalHistory":["无"],"insuranceType":"无"}`,
			message: "性别与身份证号不一致",
			fields:  []string{"sex"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientCardTestEnv(t)

			w := env.request(http.MethodPost, "/api/v1/patient/cards", tc.body)
			body := assertErrorStatus(
				t,
				w,
				http.StatusUnprocessableEntity,
				"REQUEST_VALIDATION_FAILED",
			)
			assertCardMessage(t, body, tc.message)
			assertCardFields(t, body, tc.fields...)
			if env.repo.createCalls != 0 {
				t.Errorf("字段校验失败不得触达仓储，实际调用 %d 次", env.repo.createCalls)
			}
		})
	}
}

// TestPatientCardCreateExistingCard 覆盖契约 §12.5 的 409 示例：
// 已有就诊卡时返回 PATIENT_CARD_EXISTS。
func TestPatientCardCreateExistingCard(t *testing.T) {
	env := newPatientCardTestEnv(t)
	env.repo.seed(contractCard())

	const bodyText = `{"name":"张三","sex":"男","pid":"110101199001011237",` +
		`"tel":"13800138000","medicalHistory":["无"],"insuranceType":"无"}`

	w := env.request(http.MethodPost, "/api/v1/patient/cards", bodyText)
	body := assertErrorStatus(t, w, http.StatusConflict, "PATIENT_CARD_EXISTS")
	assertCardMessage(t, body, "该账号已存在就诊卡")
}

// TestPatientCardCreateInvalidJSON 覆盖请求体解析失败：
// 语法错误与字段类型错误都返回 400 REQUEST_INVALID_JSON，不进入业务逻辑。
func TestPatientCardCreateInvalidJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "截断 JSON", body: `{"name":"张三"`},
		{name: "语法错误", body: `{"name":}`},
		{name: "字段类型错误", body: `{"medicalHistory":"无"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientCardTestEnv(t)

			w := env.request(http.MethodPost, "/api/v1/patient/cards", tc.body)
			assertErrorStatus(t, w, http.StatusBadRequest, "REQUEST_INVALID_JSON")
			if env.repo.createCalls != 0 {
				t.Errorf("请求体解析失败不得触达仓储，实际调用 %d 次", env.repo.createCalls)
			}
		})
	}
}

// --- PATCH /api/v1/patient/cards/{cardId} ---

// TestPatientCardUpdateSuccess 覆盖契约 §12.5 的正确输出：只改提交的字段，
// 未提交字段与身份相关字段保持原值。
func TestPatientCardUpdateSuccess(t *testing.T) {
	env := newPatientCardTestEnv(t)
	env.repo.seed(contractCard())

	w := env.request(
		http.MethodPatch,
		"/api/v1/patient/cards/10",
		`{"tel":"13900139000","medicalHistory":["高血压","其他"]}`,
	)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	if body["tel"] != "13900139000" {
		t.Errorf("tel = %v，期望 13900139000", body["tel"])
	}
	history := patientCardObjects(t, body, "medicalHistory")
	if len(history) != 2 || history[0] != "高血压" || history[1] != "其他" {
		t.Errorf("medicalHistory = %v，期望 [高血压 其他]", history)
	}
	if body["name"] != "张三" || body["sex"] != patient.SexMale {
		t.Errorf("未提交字段必须保持原值，name/sex = %v/%v", body["name"], body["sex"])
	}
	if body["pid"] != patientCardTestMaskedPID || body["birthday"] != "1990-01-01" {
		t.Errorf("身份字段必须保持原值与脱敏，pid/birthday = %v/%v", body["pid"], body["birthday"])
	}
	if body["insuranceType"] != "社会基本医疗保险" {
		t.Errorf("insuranceType = %v，期望保持原值", body["insuranceType"])
	}
	if env.repo.cards[10].Tel != "13900139000" {
		t.Error("修改结果必须落库")
	}
}

// TestPatientCardUpdateImmutableFields 覆盖契约 §12.5 的两个 422 示例：
// 提交 userId 返回 REQUEST_VALIDATION_FAILED，提交 pid 返回
// PATIENT_CARD_PID_IMMUTABLE，且都不得写入任何字段。
func TestPatientCardUpdateImmutableFields(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		code    string
		message string
	}{
		{
			name:    "提交 userId",
			body:    `{"userId":99,"name":"李四"}`,
			status:  http.StatusUnprocessableEntity,
			code:    "REQUEST_VALIDATION_FAILED",
			message: "不允许修改 userId",
		},
		{
			name:    "提交 pid",
			body:    `{"pid":"110101199001019999"}`,
			status:  http.StatusUnprocessableEntity,
			code:    "PATIENT_CARD_PID_IMMUTABLE",
			message: "身份证号不可修改",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientCardTestEnv(t)
			env.repo.seed(contractCard())

			w := env.request(http.MethodPatch, "/api/v1/patient/cards/10", tc.body)
			body := assertErrorStatus(t, w, tc.status, tc.code)
			assertCardMessage(t, body, tc.message)

			stored := env.repo.cards[10]
			if stored.Name != "张三" || stored.PID != patientCardTestPID {
				t.Errorf("不可修改字段导致 422 时不得写入任何字段，实际卡 = %+v", stored)
			}
		})
	}
}

// TestPatientCardUpdateInvalidTel 覆盖修改时字段级校验失败：
// 仍按 422 与 details.fields 输出，且不写入。
func TestPatientCardUpdateInvalidTel(t *testing.T) {
	env := newPatientCardTestEnv(t)
	env.repo.seed(contractCard())

	w := env.request(http.MethodPatch, "/api/v1/patient/cards/10", `{"tel":"123"}`)
	body := assertErrorStatus(
		t,
		w,
		http.StatusUnprocessableEntity,
		"REQUEST_VALIDATION_FAILED",
	)
	assertCardMessage(t, body, "联系电话格式不正确")
	assertCardFields(t, body, "tel")

	if env.repo.cards[10].Tel != patientCardTestTel {
		t.Errorf("校验失败不得写入，tel = %q", env.repo.cards[10].Tel)
	}
}

// --- 认证与依赖故障 ---

// TestPatientCardRoutesRequireClaims 覆盖中间件缺失：四个接口在拿不到
// realm=patient claims 时必须返回 401 AUTH_INVALID_TOKEN，
// 绝不按请求参数降级放行。
func TestPatientCardRoutesRequireClaims(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "列表", method: http.MethodGet, path: "/api/v1/patient/cards"},
		{name: "详情", method: http.MethodGet, path: "/api/v1/patient/cards/10"},
		{name: "创建", method: http.MethodPost, path: "/api/v1/patient/cards", body: `{}`},
		{name: "修改", method: http.MethodPatch, path: "/api/v1/patient/cards/10", body: `{}`},
	}

	env := newPatientCardTestEnv(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := performPatientCardRequest(env.noClaims, tc.method, tc.path, tc.body)
			body := assertErrorStatus(t, w, http.StatusUnauthorized, "AUTH_INVALID_TOKEN")
			assertCardMessage(t, body, "患者访问令牌无效")
		})
	}
}

// TestPatientCardDependencyFailures 覆盖仓储故障：四个接口都必须返回
// 502 DEPENDENCY_UNAVAILABLE，不得伪装成 404/500（契约 §10）。
func TestPatientCardDependencyFailures(t *testing.T) {
	cause := errors.New("postgres unavailable")

	cases := []struct {
		name   string
		inject func(*patientCardTestRepo)
		method string
		path   string
		body   string
	}{
		{
			name:   "列表",
			inject: func(repo *patientCardTestRepo) { repo.listErr = cause },
			method: http.MethodGet,
			path:   "/api/v1/patient/cards",
		},
		{
			name:   "详情",
			inject: func(repo *patientCardTestRepo) { repo.findErr = cause },
			method: http.MethodGet,
			path:   "/api/v1/patient/cards/10",
		},
		{
			name:   "创建",
			inject: func(repo *patientCardTestRepo) { repo.createErr = cause },
			method: http.MethodPost,
			path:   "/api/v1/patient/cards",
			body: `{"name":"张三","sex":"男","pid":"110101199001011237",` +
				`"tel":"13800138000","medicalHistory":["无"],"insuranceType":"无"}`,
		},
		{
			name:   "修改",
			inject: func(repo *patientCardTestRepo) { repo.updateErr = cause },
			method: http.MethodPatch,
			path:   "/api/v1/patient/cards/10",
			body:   `{"tel":"13900139000"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientCardTestEnv(t)
			env.repo.seed(contractCard())
			tc.inject(env.repo)

			w := env.request(tc.method, tc.path, tc.body)
			body := assertErrorStatus(t, w, http.StatusBadGateway, "DEPENDENCY_UNAVAILABLE")
			assertCardMessage(t, body, "服务暂时不可用，请稍后重试")
		})
	}
}

// --- 本轮新增：显式 null、越权修改与取值非法文案 ---

// TestPatientCardUpdateImmutableFieldsSubmittedAsNull 覆盖显式提交 null 的不可修改字段：
// 「键存在」即属于提交过该字段，即使取值是 null 也必须按契约返回 422，
// 不能被当成「未提交」而静默返回 200（否则客户端会误以为修改已生效）。
func TestPatientCardUpdateImmutableFieldsSubmittedAsNull(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		code    string
		message string
	}{
		{
			name:    "pid 显式提交 null",
			body:    `{"pid":null}`,
			status:  http.StatusUnprocessableEntity,
			code:    "PATIENT_CARD_PID_IMMUTABLE",
			message: "身份证号不可修改",
		},
		{
			name:    "userId 显式提交 null",
			body:    `{"userId":null}`,
			status:  http.StatusUnprocessableEntity,
			code:    "REQUEST_VALIDATION_FAILED",
			message: "不允许修改 userId",
		},
		{
			name:    "birthday 显式提交 null",
			body:    `{"birthday":null}`,
			status:  http.StatusUnprocessableEntity,
			code:    "REQUEST_VALIDATION_FAILED",
			message: "不允许修改 birthday",
		},
		{
			name:    "三个不可修改字段同时显式提交 null",
			body:    `{"pid":null,"userId":null,"birthday":null}`,
			status:  http.StatusUnprocessableEntity,
			code:    "PATIENT_CARD_PID_IMMUTABLE",
			message: "身份证号不可修改",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientCardTestEnv(t)
			env.repo.seed(contractCard())

			w := env.request(http.MethodPatch, "/api/v1/patient/cards/10", tc.body)
			body := assertErrorStatus(t, w, tc.status, tc.code)
			assertCardMessage(t, body, tc.message)

			if stored := env.repo.cards[10]; stored.Name != "张三" || stored.Tel != patientCardTestTel {
				t.Errorf("提交不可修改字段时不得写入任何字段，实际卡 = %+v", stored)
			}
		})
	}
}

// TestPatientCardUpdateNullImmutableMixedWithMutableField 覆盖「显式 null 的不可修改字段
// 与可修改字段混用」：整条请求必须整体失败在 422，可修改字段的新值也不能落库，
// 否则客户端会看到「报错但部分字段已改」的中间态。
func TestPatientCardUpdateNullImmutableMixedWithMutableField(t *testing.T) {
	env := newPatientCardTestEnv(t)
	env.repo.seed(contractCard())

	w := env.request(
		http.MethodPatch,
		"/api/v1/patient/cards/10",
		`{"pid":null,"tel":"13900139000"}`,
	)
	body := assertErrorStatus(
		t,
		w,
		http.StatusUnprocessableEntity,
		"PATIENT_CARD_PID_IMMUTABLE",
	)
	assertCardMessage(t, body, "身份证号不可修改")

	stored := env.repo.cards[10]
	if stored.Tel != patientCardTestTel {
		t.Errorf("整体失败时 tel 不得落库，实际 %q，期望 %q", stored.Tel, patientCardTestTel)
	}
	if stored.PID != patientCardTestPID {
		t.Errorf("pid 不得被修改，实际 %q", stored.PID)
	}
	if stored.Name != "张三" {
		t.Errorf("name 不得被修改，实际 %q", stored.Name)
	}
}

// TestPatientCardUpdateEmptyBodyIsNoOp 覆盖空对象请求体：没有提交任何字段时按
// 幂等空操作处理，返回 200 与当前资源，且所有字段保持原值（不被置零或清空）。
func TestPatientCardUpdateEmptyBodyIsNoOp(t *testing.T) {
	env := newPatientCardTestEnv(t)
	seeded := env.repo.seed(contractCard())

	w := env.request(http.MethodPatch, "/api/v1/patient/cards/10", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	if body["id"] != float64(seeded.ID) || body["userId"] != float64(seeded.UserID) {
		t.Errorf(
			"id/userId = %v/%v，期望原值 %d/%d",
			body["id"], body["userId"], seeded.ID, seeded.UserID,
		)
	}
	if body["name"] != seeded.Name || body["sex"] != seeded.Sex || body["tel"] != seeded.Tel {
		t.Errorf("name/sex/tel 必须保持原值，实际 %v/%v/%v", body["name"], body["sex"], body["tel"])
	}
	if body["pid"] != patientCardTestMaskedPID {
		t.Errorf("pid = %v，期望脱敏原值 %s", body["pid"], patientCardTestMaskedPID)
	}
	if body["uuid"] != seeded.UUID {
		t.Errorf("uuid = %v，期望原值 %s", body["uuid"], seeded.UUID)
	}
	if body["birthday"] != seeded.Birthday || body["insuranceType"] != seeded.InsuranceType {
		t.Errorf(
			"birthday/insuranceType 必须保持原值，实际 %v/%v",
			body["birthday"], body["insuranceType"],
		)
	}
	if body["existFaceModel"] != false {
		t.Errorf("existFaceModel = %v，期望 false", body["existFaceModel"])
	}
	history := patientCardObjects(t, body, "medicalHistory")
	if len(history) != 2 || history[0] != "高血压" || history[1] != "糖尿病" {
		t.Errorf("medicalHistory = %v，期望原值 [高血压 糖尿病]", history)
	}

	stored := env.repo.cards[10]
	if stored.Tel != patientCardTestTel || stored.Name != "张三" || stored.MedicalHistory == nil {
		t.Errorf("空请求体不得改动任何字段，实际卡 = %+v", stored)
	}
}

// TestPatientCardUpdateNullMutableFieldsKeepValues 覆盖可修改字段显式提交 null：
// 指针字段的 null 会解成 nil，语义是「本次不提交该字段」，因此返回 200，
// 且响应体必须与请求前的卡逐字段一致——本项目没有「清空字段」的语义，
// null 不得把已有取值清空（见 UpdatePatientCardRequest 的文档注释）。
func TestPatientCardUpdateNullMutableFieldsKeepValues(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "tel 为 null", body: `{"tel":null}`},
		{name: "medicalHistory 为 null", body: `{"medicalHistory":null}`},
		{
			name: "五个可修改字段一起提交 null",
			body: `{"name":null,"sex":null,"tel":null,"medicalHistory":null,"insuranceType":null}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientCardTestEnv(t)
			env.repo.seed(contractCard())

			// 先取一次详情作为基准，再把 PATCH 响应与它逐字段对比。
			baseline := decodeBody(t, env.request(http.MethodGet, "/api/v1/patient/cards/10", ""))

			w := env.request(http.MethodPatch, "/api/v1/patient/cards/10", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			got := decodeBody(t, w)

			if !reflect.DeepEqual(got, baseline) {
				t.Errorf(
					"PATCH 响应体与请求前的卡不一致（null 被当成清空）：\n got=%v\nwant=%v",
					got,
					baseline,
				)
			}
			// 关键字段再单独断言，避免两侧同时缺失时 DeepEqual 静默通过。
			if got["tel"] != patientCardTestTel {
				t.Errorf("tel = %v，期望保持 %s", got["tel"], patientCardTestTel)
			}
			history := patientCardObjects(t, got, "medicalHistory")
			if len(history) != 2 || history[0] != "高血压" || history[1] != "糖尿病" {
				t.Errorf("medicalHistory = %v，期望保持 [高血压 糖尿病]", history)
			}

			stored := env.repo.cards[10]
			if stored.Tel != patientCardTestTel || len(stored.MedicalHistory) != 2 {
				t.Errorf("仓储中的就诊卡不得被 null 清空，实际 %+v", stored)
			}
		})
	}
}

// TestPatientCardUpdateImmutableFieldIgnoresValueType 锁定「不可修改字段保留原始 JSON」
// 的口径：取值类型不再触发 400 REQUEST_INVALID_JSON，{"pid":0}、{"userId":"abc"}
// 这类请求仍按契约 §7.4「提交即 422」处理，且不写入任何字段。
func TestPatientCardUpdateImmutableFieldIgnoresValueType(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		code    string
		message string
	}{
		{
			name:    "pid 为数字 0",
			body:    `{"pid":0}`,
			code:    "PATIENT_CARD_PID_IMMUTABLE",
			message: "身份证号不可修改",
		},
		{
			name:    "userId 为字符串",
			body:    `{"userId":"abc"}`,
			code:    "REQUEST_VALIDATION_FAILED",
			message: "不允许修改 userId",
		},
		{
			name:    "birthday 为数字 0",
			body:    `{"birthday":0}`,
			code:    "REQUEST_VALIDATION_FAILED",
			message: "不允许修改 birthday",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientCardTestEnv(t)
			env.repo.seed(contractCard())

			w := env.request(http.MethodPatch, "/api/v1/patient/cards/10", tc.body)
			if w.Code == http.StatusBadRequest {
				t.Fatalf(
					"不可修改字段的取值类型不得触发 400 REQUEST_INVALID_JSON; body=%s",
					w.Body.String(),
				)
			}
			body := assertErrorStatus(t, w, http.StatusUnprocessableEntity, tc.code)
			assertCardMessage(t, body, tc.message)

			stored := env.repo.cards[10]
			if stored.PID != patientCardTestPID || stored.Name != "张三" {
				t.Errorf("不可修改字段报错时不得写入任何字段，实际卡 = %+v", stored)
			}
		})
	}
}

// TestPatientCardUpdateHidesForeignAndMissingCard 覆盖 PATCH 的越权与不存在：
// 两者都必须返回 404 PATIENT_CARD_NOT_FOUND，且绝不能进入仓储的写入路径。
func TestPatientCardUpdateHidesForeignAndMissingCard(t *testing.T) {
	cases := []struct {
		name   string
		cardID int64
	}{
		{name: "他人就诊卡", cardID: 77},
		{name: "就诊卡不存在", cardID: 99999},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientCardTestEnv(t)
			env.repo.seed(patient.Card{
				ID:     77,
				UserID: 99,
				Name:   "他人",
				Tel:    patientCardTestTel,
			})

			path := "/api/v1/patient/cards/" + strconv.FormatInt(tc.cardID, 10)
			w := env.request(http.MethodPatch, path, `{"tel":"13900139000"}`)
			body := assertErrorStatus(t, w, http.StatusNotFound, "PATIENT_CARD_NOT_FOUND")
			assertCardMessage(t, body, "就诊卡不存在")

			if env.repo.updateCalls != 0 {
				t.Fatalf("越权/不存在的修改不得调用仓储 UpdateCard，实际 %d 次", env.repo.updateCalls)
			}
			if env.repo.cards[77].Tel != patientCardTestTel {
				t.Error("越权修改不得写入任何字段")
			}
		})
	}
}

// TestPatientCardCreateValueValidationMessages 覆盖建卡取值非法时的专用文案：
// 本轮为名称过长、性别枚举、疾病史枚举/互斥、医保类型、身份证号推导出未来日期
// 分别补齐了面向用户的 message，用例逐条锁定，避免回落到笼统的兜底句。
func TestPatientCardCreateValueValidationMessages(t *testing.T) {
	// 21 个汉字，超过 name 列 VARCHAR(20) 的领域上限。
	longName := strings.Repeat("张", 21)

	cases := []struct {
		name    string
		body    string
		message string
		fields  []string
	}{
		{
			name: "性别取值非法",
			body: `{"name":"张三","sex":"未知","pid":"110101199001011237",` +
				`"tel":"13800138000","medicalHistory":["无"],"insuranceType":"无"}`,
			message: "性别取值不正确",
			fields:  []string{"sex"},
		},
		{
			name: "疾病史含非法取值",
			body: `{"name":"张三","sex":"男","pid":"110101199001011237",` +
				`"tel":"13800138000","medicalHistory":["感冒"],"insuranceType":"无"}`,
			message: "疾病史含不支持的取值",
			fields:  []string{"medicalHistory"},
		},
		{
			name: "疾病史的「无」与其他取值互斥",
			body: `{"name":"张三","sex":"男","pid":"110101199001011237",` +
				`"tel":"13800138000","medicalHistory":["无","高血压"],"insuranceType":"无"}`,
			message: "疾病史「无」不能与其他选项同时提交",
			fields:  []string{"medicalHistory"},
		},
		{
			name: "医保类型取值非法",
			body: `{"name":"张三","sex":"男","pid":"110101199001011237",` +
				`"tel":"13800138000","medicalHistory":["无"],"insuranceType":"大病保险"}`,
			message: "医保类型取值不正确",
			fields:  []string{"insuranceType"},
		},
		{
			name: "姓名超出列长度",
			body: `{"name":"` + longName + `","sex":"男","pid":"110101199001011237",` +
				`"tel":"13800138000","medicalHistory":["无"],"insuranceType":"无"}`,
			message: "姓名长度超出限制",
			fields:  []string{"name"},
		},
		{
			name: "身份证号推导出未来出生日期",
			body: `{"name":"张三","sex":"男","pid":"11010120990101013X",` +
				`"tel":"13800138000","medicalHistory":["无"],"insuranceType":"无"}`,
			message: "身份证号推导的出生日期不合法",
			fields:  []string{"pid"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientCardTestEnv(t)

			w := env.request(http.MethodPost, "/api/v1/patient/cards", tc.body)
			body := assertErrorStatus(
				t,
				w,
				http.StatusUnprocessableEntity,
				"REQUEST_VALIDATION_FAILED",
			)
			assertCardMessage(t, body, tc.message)
			assertCardFields(t, body, tc.fields...)

			if env.repo.createCalls != 0 {
				t.Errorf("取值校验失败不得触达仓储，实际调用 %d 次", env.repo.createCalls)
			}
		})
	}
}
