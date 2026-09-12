// 病历 HTTP 契约单测（续）：幂等重放、请求体非法、realm 守卫与依赖故障映射
// （spec/04-api-contract.md §1.5、§10、§12.4）。
package handler

import (
	"errors"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	medicalrecordservice "Medical-Web-Backend/internal/usecase/medical_record"
)

// errMedicalRecordDependency 模拟 PostgreSQL 不可用（非领域错误），
// 用例层必须把它归为 502 DEPENDENCY_UNAVAILABLE，而不是业务结论。
var errMedicalRecordDependency = errors.New("postgres is down")

// TestMedicalRecordCreateIdempotencyReplay 同一 Idempotency-Key 重放必须原样返回首次结果：
// 不重复落库、不改变状态码与 Location 头，即使重放请求的请求体不同。
func TestMedicalRecordCreateIdempotencyReplay(t *testing.T) {
	env := newMedicalRecordHandlerEnv(t)
	headers := map[string]string{"Idempotency-Key": "replay-key"}

	first := performMedicalRecord(env.mis, http.MethodPost, "/api/v1/medical-records",
		`{"registrationId":1001,"diagnosis":"牙髓炎","content":"第一次"}`,
		map[string]string{"Idempotency-Key": headers["Idempotency-Key"]})
	if first.Code != http.StatusCreated {
		t.Fatalf("首次 status = %d, want 201; body=%s", first.Code, first.Body.String())
	}
	if env.repo.createCalls != 1 {
		t.Fatalf("首次 createCalls = %d, want 1", env.repo.createCalls)
	}

	// 重放：换成不同请求体，仍应返回第一次保存的结果。
	second := performMedicalRecord(env.mis, http.MethodPost, "/api/v1/medical-records",
		`{"registrationId":1001,"diagnosis":"完全不同的诊断","content":"第二次"}`,
		map[string]string{"Idempotency-Key": headers["Idempotency-Key"]})

	if second.Code != first.Code {
		t.Errorf("重放 status = %d, want %d", second.Code, first.Code)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("重放响应体必须与首次一致：\ngot  %s\nwant %s", second.Body.String(), first.Body.String())
	}
	if second.Header().Get("Location") != first.Header().Get("Location") {
		t.Errorf("重放 Location = %q, want %q",
			second.Header().Get("Location"), first.Header().Get("Location"))
	}
	if env.repo.createCalls != 1 {
		t.Errorf("重放不得重复落库：createCalls = %d, want 1", env.repo.createCalls)
	}
}

// TestMedicalRecordCreateRequiresIdempotencyKey 缺少 Idempotency-Key 时返回 422，
// 且不得触碰幂等存储（契约 §1.5：创建接口必须带幂等键）。
func TestMedicalRecordCreateRequiresIdempotencyKey(t *testing.T) {
	env := newMedicalRecordHandlerEnv(t)

	w := performMedicalRecord(env.mis, http.MethodPost, "/api/v1/medical-records",
		`{"registrationId":1001,"diagnosis":"牙髓炎","content":"正文"}`, nil)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	assertMedicalRecordErrorCode(t, w, medicalrecordservice.CodeValidationFailed)
	if env.store.claims != 0 {
		t.Errorf("参数错误不得占用幂等键：claims = %d, want 0", env.store.claims)
	}
	if env.repo.createCalls != 0 {
		t.Errorf("参数错误不得落库：createCalls = %d, want 0", env.repo.createCalls)
	}
}

// TestMedicalRecordCreateInvalidJSON 非法 JSON 返回 400 REQUEST_INVALID_JSON
// （不得伪造成 422 参数错误，也不得暴露内部英文错误信息）。
func TestMedicalRecordCreateInvalidJSON(t *testing.T) {
	cases := []struct {
		name string
		key  string
		body string
	}{
		{name: "语法错误", key: "invalid-json-syntax", body: `{"registrationId":1001,"diagnosis":}`},
		{name: "截断的 JSON", key: "invalid-json-truncated", body: `{"registrationId":`},
		{name: "字段类型错误", key: "invalid-json-type", body: `{"registrationId":"1001","diagnosis":"牙髓炎","content":"正文"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMedicalRecordHandlerEnv(t)

			w := performMedicalRecord(env.mis, http.MethodPost, "/api/v1/medical-records", tc.body,
				map[string]string{"Idempotency-Key": tc.key})

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			assertMedicalRecordErrorCode(t, w, "REQUEST_INVALID_JSON")
		})
	}
}

// TestMedicalRecordHandlerRejectsNonMisClaims 病历接口只服务 realm=mis 的令牌：
// 未携带令牌或携带患者域令牌都必须 401 AUTH_INVALID_TOKEN（不得落到 handler 内部 500）。
func TestMedicalRecordHandlerRejectsNonMisClaims(t *testing.T) {
	env := newMedicalRecordHandlerEnv(t)

	cases := []struct {
		name   string
		engine *gin.Engine
	}{
		{name: "未携带令牌", engine: env.noClaims},
		{name: "患者域令牌", engine: env.patient},
	}
	routes := []struct {
		method string
		target string
		body   string
	}{
		{method: http.MethodPost, target: "/api/v1/medical-records", body: `{"registrationId":1001,"diagnosis":"牙痛","content":"正文"}`},
		{method: http.MethodGet, target: "/api/v1/medical-records"},
		{method: http.MethodGet, target: "/api/v1/medical-records/1001"},
		{method: http.MethodPatch, target: "/api/v1/medical-records/1001", body: `{"diagnosis":"牙痛"}`},
		{method: http.MethodDelete, target: "/api/v1/medical-records/1001"},
	}

	for _, tc := range cases {
		for _, route := range routes {
			t.Run(tc.name+" "+route.method+" "+route.target, func(t *testing.T) {
				// 即使带了幂等键也不得被放行。
				w := performMedicalRecord(tc.engine, route.method, route.target, route.body,
					map[string]string{"Idempotency-Key": "realm-guard"})

				if w.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
				}
				body := assertMedicalRecordErrorCode(t, w, "AUTH_INVALID_TOKEN")
				if body["message"] != "访问令牌无效或已过期" {
					t.Errorf("message = %v, want 访问令牌无效或已过期", body["message"])
				}
			})
		}
	}
}

// TestMedicalRecordDependencyFailures 仓储故障统一映射为 502 DEPENDENCY_UNAVAILABLE，
// 不得伪装成 404（可重试故障不能被当成业务结论）。
func TestMedicalRecordDependencyFailures(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(env *medicalRecordHandlerEnv)
		method  string
		target  string
		body    string
		headers map[string]string
	}{
		{
			name:    "读取医生绑定失败",
			prepare: func(env *medicalRecordHandlerEnv) { env.repo.doctorErr = errMedicalRecordDependency },
			method:  http.MethodGet,
			target:  "/api/v1/medical-records",
		},
		{
			name:    "挂号归属读取失败",
			prepare: func(env *medicalRecordHandlerEnv) { env.repo.ownerErr = errMedicalRecordDependency },
			method:  http.MethodPost,
			target:  "/api/v1/medical-records",
			body:    `{"registrationId":1001,"diagnosis":"牙髓炎","content":"正文"}`,
			headers: map[string]string{"Idempotency-Key": "dep-owner"},
		},
		{
			name:    "书写作失败",
			prepare: func(env *medicalRecordHandlerEnv) { env.repo.createErr = errMedicalRecordDependency },
			method:  http.MethodPost,
			target:  "/api/v1/medical-records",
			body:    `{"registrationId":1001,"diagnosis":"牙髓炎","content":"正文"}`,
			headers: map[string]string{"Idempotency-Key": "dep-create"},
		},
		{
			name:    "列表读取失败",
			prepare: func(env *medicalRecordHandlerEnv) { env.repo.listErr = errMedicalRecordDependency },
			method:  http.MethodGet,
			target:  "/api/v1/medical-records",
		},
		{
			name:    "详情读取失败",
			prepare: func(env *medicalRecordHandlerEnv) { env.repo.findErr = errMedicalRecordDependency },
			method:  http.MethodGet,
			target:  "/api/v1/medical-records/1001",
		},
		{
			name:    "修改失败",
			prepare: func(env *medicalRecordHandlerEnv) { env.repo.updateErr = errMedicalRecordDependency },
			method:  http.MethodPatch,
			target:  "/api/v1/medical-records/1001",
			body:    `{"diagnosis":"新诊断"}`,
		},
		{
			name:    "删除失败",
			prepare: func(env *medicalRecordHandlerEnv) { env.repo.deleteErr = errMedicalRecordDependency },
			method:  http.MethodDelete,
			target:  "/api/v1/medical-records/1001",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMedicalRecordHandlerEnv(t)
			tc.prepare(env)

			w := performMedicalRecord(env.mis, tc.method, tc.target, tc.body, tc.headers)

			if w.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
			}
			assertMedicalRecordErrorCode(t, w, medicalrecordservice.CodeDependencyUnavailable)
		})
	}
}

// TestMedicalRecordWriteServiceErrorInvalidActorMapsToInternal 用例层的 ErrInvalidActor
// 属于内部调用错误（realm 已在中间件与 handler.actor 拦截），映射为 500 而不是 4xx。
func TestMedicalRecordWriteServiceErrorInvalidActorMapsToInternal(t *testing.T) {
	env := newMedicalRecordHandlerEnv(t)
	engine := gin.New()
	engine.GET("/probe", func(c *gin.Context) {
		env.handler.writeServiceError(c, medicalrecordservice.ErrInvalidActor)
	})

	w := performMedicalRecord(engine, http.MethodGet, "/probe", "", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	assertMedicalRecordErrorCode(t, w, "INTERNAL_SERVER_ERROR")
}
