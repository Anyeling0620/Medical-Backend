package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"Medical-Web-Backend/internal/config"
	domainauth "Medical-Web-Backend/internal/domain/auth"
	domaindoctorpatient "Medical-Web-Backend/internal/domain/doctorpatient"
	domainuser "Medical-Web-Backend/internal/domain/user"
	"Medical-Web-Backend/internal/port"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

// 本文件覆盖医生工作台「我的患者」的路由级鉴权矩阵
// （GET /api/v1/mis/doctor/patients，契约 §1.2、§6.10、§12.4）。
// 构造方式沿用 router_test.go 的契约测试桩与 router_realm_test.go 的令牌签发方式：
// 只断言状态码、错误码与到达 handler 后的响应结构，不依赖真实数据库与 Redis。

// doctorPatientsRouterUserRepo 是 UserRepository 桩：固定返回预置账号与权限，
// 用于分别驱动「无权限 403」「未绑定医生 403」「已绑定 200」三条分支。
type doctorPatientsRouterUserRepo struct {
	account     *domainuser.User
	permissions []string
}

func (doctorPatientsRouterUserRepo) FindByUsername(
	_ context.Context,
	_ string,
) (*domainuser.User, error) {
	return nil, nil
}

func (r doctorPatientsRouterUserRepo) FindByID(
	_ context.Context,
	userID int64,
) (*domainuser.User, error) {
	if r.account == nil || r.account.ID != userID {
		return nil, nil
	}
	return r.account, nil
}

func (r doctorPatientsRouterUserRepo) Permissions(
	_ context.Context,
	_ int64,
) ([]string, error) {
	return r.permissions, nil
}

// doctorPatientsRouterRepo 是 DoctorPatientRepository 桩：
// 返回预置患者并记录收到的过滤条件与 offset/limit。
type doctorPatientsRouterRepo struct {
	items      []domaindoctorpatient.Patient
	total      int64
	calls      int
	lastFilter domaindoctorpatient.Filter
	lastOffset int
	lastLimit  int
}

func (r *doctorPatientsRouterRepo) ListDoctorPatients(
	_ context.Context,
	filter domaindoctorpatient.Filter,
	offset, limit int,
) ([]domaindoctorpatient.Patient, int64, error) {
	r.calls++
	r.lastFilter, r.lastOffset, r.lastLimit = filter, offset, limit
	return r.items, r.total, nil
}

// newDoctorPatientsTestRouter 构造带 JWT 密钥、令牌桩与可配置用户仓库的完整路由树。
func newDoctorPatientsTestRouter(
	t *testing.T,
	users port.UserRepository,
	patients port.DoctorPatientRepository,
) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return NewRouter(
		func() map[string]any { return map[string]any{"status": "ok"} },
		config.Config{Auth: config.AuthConfig{JWTSecret: contractTestJWTSecret}},
		users,
		contractTokenRepo{},
		nil, // doctorRepository
		nil, // scheduleRepository
		nil, // idempotencyStore
		nil, // patientRepository
		nil, // wechatAuthenticator
		nil, // registrationRepository
		nil, // paymentRepository
		nil, // alipayGateway
		patients,
	)
}

// signDoctorPatientsAccessToken 用契约测试密钥签发指定 realm 的合法 access token。
func signDoctorPatientsAccessToken(
	t *testing.T,
	realm domainauth.Realm,
	userID int64,
) string {
	t.Helper()
	claims := &userservice.AccessClaims{
		UserID:    userID,
		Username:  "mis-doctor",
		TokenType: userservice.TokenTypeAccess,
		Realm:     realm,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "doctor-patients-jti-" + string(realm),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(contractTestJWTSecret))
	if err != nil {
		t.Fatalf("签发医生工作台测试 access token 失败：%v", err)
	}
	return signed
}

// performDoctorPatientsList 发起 GET /api/v1/mis/doctor/patients；token 非空时以 Bearer 附加。
func performDoctorPatientsList(
	router *gin.Engine,
	token string,
	rawQuery string,
) *httptest.ResponseRecorder {
	path := "/api/v1/mis/doctor/patients"
	if rawQuery != "" {
		path += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestRouterDoctorPatientsAuthMatrix 覆盖 GET /api/v1/mis/doctor/patients 的鉴权矩阵：
// 无令牌 401；患者域令牌 401（realm 不匹配）；管理端令牌无 REGISTRATION:SELECT 403；
// 有权限但 ref_id 未绑定 403 AUTH_FORBIDDEN；有权限且已绑定 200 返回分页结构；
// 参数非法 422。数据范围必须来自令牌主体绑定的 doctor.id。
func TestRouterDoctorPatientsAuthMatrix(t *testing.T) {
	const misUserID = 7
	doctorID := int64(16)

	boundAccount := &domainuser.User{ID: misUserID, Username: "mis-doctor", RefID: &doctorID}
	unboundAccount := &domainuser.User{ID: misUserID, Username: "mis-admin"}
	seedItems := []domaindoctorpatient.Patient{{
		PatientCardID:     10,
		Name:              "张三",
		Sex:               "男",
		Tel:               "13800000000",
		Birthday:          "1990-01-01",
		MedicalHistory:    []string{"高血压"},
		InsuranceType:     "城镇职工",
		RegistrationCount: 3,
		LastVisitDate:     "2026-09-20",
		LastPaymentStatus: "PAID",
	}}
	selectPermission := []string{"REGISTRATION:SELECT"}

	t.Run("无令牌返回 401", func(t *testing.T) {
		router := newDoctorPatientsTestRouter(t,
			doctorPatientsRouterUserRepo{account: boundAccount, permissions: selectPermission},
			&doctorPatientsRouterRepo{})

		w := performDoctorPatientsList(router, "", "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
		}
		body := decodeRealmRouterBody(t, w)
		if body["code"] != "AUTH_INVALID_TOKEN" {
			t.Errorf("code = %v, want AUTH_INVALID_TOKEN", body["code"])
		}
		if body["message"] != "访问令牌无效或已过期" {
			t.Errorf("message = %v, want 访问令牌无效或已过期", body["message"])
		}
	})

	t.Run("患者域令牌因 realm 不匹配返回 401", func(t *testing.T) {
		router := newDoctorPatientsTestRouter(t,
			doctorPatientsRouterUserRepo{account: boundAccount, permissions: selectPermission},
			&doctorPatientsRouterRepo{})

		token := signDoctorPatientsAccessToken(t, domainauth.RealmPatient, misUserID)
		w := performDoctorPatientsList(router, token, "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（realm 不匹配不得进入权限中间件）; body=%s",
				w.Code, w.Body.String())
		}
		if code := decodeRealmRouterBody(t, w)["code"]; code != "AUTH_INVALID_TOKEN" {
			t.Errorf("code = %v, want AUTH_INVALID_TOKEN", code)
		}
	})

	t.Run("管理端令牌缺少 REGISTRATION:SELECT 返回 403", func(t *testing.T) {
		router := newDoctorPatientsTestRouter(t,
			doctorPatientsRouterUserRepo{
				account:     boundAccount,
				permissions: []string{"CATALOG:SELECT"},
			},
			&doctorPatientsRouterRepo{})

		token := signDoctorPatientsAccessToken(t, domainauth.RealmMis, misUserID)
		w := performDoctorPatientsList(router, token, "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
		}
		body := decodeRealmRouterBody(t, w)
		if body["code"] != "AUTH_FORBIDDEN" {
			t.Errorf("code = %v, want AUTH_FORBIDDEN", body["code"])
		}
		if body["message"] != "没有访问权限" {
			t.Errorf("message = %v, want 没有访问权限（权限中间件拒绝）", body["message"])
		}
	})

	t.Run("未绑定 ref_id 返回 403 AUTH_FORBIDDEN", func(t *testing.T) {
		repo := &doctorPatientsRouterRepo{items: seedItems, total: 1}
		router := newDoctorPatientsTestRouter(t,
			doctorPatientsRouterUserRepo{account: unboundAccount, permissions: selectPermission},
			repo)

		token := signDoctorPatientsAccessToken(t, domainauth.RealmMis, misUserID)
		w := performDoctorPatientsList(router, token, "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
		}
		body := decodeRealmRouterBody(t, w)
		if body["code"] != "AUTH_FORBIDDEN" {
			t.Errorf("code = %v, want AUTH_FORBIDDEN", body["code"])
		}
		if body["message"] != "当前账号未关联医生，无法查看医生工作台数据" {
			t.Errorf("message = %v, want 契约 §12.4 的未绑定医生文案", body["message"])
		}
		if repo.calls != 0 {
			t.Errorf("未绑定医生时不得查询仓储，实际调用 %d 次", repo.calls)
		}
	})

	t.Run("已绑定医生返回 200 分页结构", func(t *testing.T) {
		repo := &doctorPatientsRouterRepo{items: seedItems, total: 1}
		router := newDoctorPatientsTestRouter(t,
			doctorPatientsRouterUserRepo{account: boundAccount, permissions: selectPermission},
			repo)

		token := signDoctorPatientsAccessToken(t, domainauth.RealmMis, misUserID)
		w := performDoctorPatientsList(router, token, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}

		body := decodeRealmRouterBody(t, w)
		if code, exists := body["code"]; exists {
			t.Fatalf("成功响应不应包含错误码：code=%v body=%s", code, w.Body.String())
		}
		for _, key := range []string{"items", "page", "pageSize", "total"} {
			if _, exists := body[key]; !exists {
				t.Errorf("响应缺少字段 %q：%s", key, w.Body.String())
			}
		}
		if body["page"] != float64(1) || body["pageSize"] != float64(20) || body["total"] != float64(1) {
			t.Errorf("分页字段 = page:%v pageSize:%v total:%v, want 1/20/1",
				body["page"], body["pageSize"], body["total"])
		}

		items, ok := body["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("items = %v, want 一条记录", body["items"])
		}
		item, ok := items[0].(map[string]any)
		if !ok {
			t.Fatalf("items[0] 不是对象：%v", items[0])
		}
		// 逐个字段核对 §12.4 的映射：字段名与取值都锁死，避免改名或错配静默通过。
		wantItem := map[string]any{
			"patientCardId":     float64(10),
			"name":              "张三",
			"sex":               "男",
			"tel":               "13800000000",
			"birthday":          "1990-01-01",
			"insuranceType":     "城镇职工",
			"registrationCount": float64(3),
			"lastVisitDate":     "2026-09-20",
			"lastPaymentStatus": "PAID",
		}
		for field, want := range wantItem {
			if item[field] != want {
				t.Errorf("字段 %s = %v, want %v", field, item[field], want)
			}
		}
		// 疾病史按契约 §2 以字符串数组暴露：JSON 数组不能用 != 直接比较，单独核对。
		history, ok := item["medicalHistory"].([]any)
		if !ok || len(history) != 1 || history[0] != "高血压" {
			t.Errorf("字段 medicalHistory = %v, want [高血压]", item["medicalHistory"])
		}
		// 字段数量必须恰好等于 DTO 定义：多返回任何字段（如身份证号、user_id）都要被拦下。
		// wantItem 不含 medicalHistory（数组单独比对），因此期望字段数需要 +1。
		if len(item) != len(wantItem)+1 {
			t.Errorf("响应字段数 = %d, want %d；实际字段：%v", len(item), len(wantItem)+1, item)
		}
		// 数据范围必须来自令牌主体绑定的 doctor.id，而不是任何客户端参数。
		if repo.lastFilter.DoctorID != doctorID {
			t.Errorf("Filter.DoctorID = %d, want %d", repo.lastFilter.DoctorID, doctorID)
		}
		if repo.lastOffset != 0 || repo.lastLimit != 20 {
			t.Errorf("offset/limit = %d/%d, want 0/20", repo.lastOffset, repo.lastLimit)
		}
	})

	t.Run("关键词与分页参数透传到仓储", func(t *testing.T) {
		repo := &doctorPatientsRouterRepo{items: seedItems, total: 1}
		router := newDoctorPatientsTestRouter(t,
			doctorPatientsRouterUserRepo{account: boundAccount, permissions: []string{"ROOT"}},
			repo)

		token := signDoctorPatientsAccessToken(t, domainauth.RealmMis, misUserID)
		w := performDoctorPatientsList(router, token,
			"keyword=%E5%BC%A0&sort=name&order=asc&page=2&pageSize=5")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if repo.lastFilter.Keyword != "张" ||
			repo.lastFilter.Sort != domaindoctorpatient.SortName ||
			repo.lastFilter.Order != "asc" {
			t.Errorf("查询条件透传错误：%+v", repo.lastFilter)
		}
		if repo.lastOffset != 5 || repo.lastLimit != 5 {
			t.Errorf("offset/limit = %d/%d, want 5/5", repo.lastOffset, repo.lastLimit)
		}
		body := decodeRealmRouterBody(t, w)
		if body["page"] != float64(2) || body["pageSize"] != float64(5) {
			t.Errorf("响应分页 = %v/%v, want 2/5", body["page"], body["pageSize"])
		}
	})

	t.Run("客户端提交 doctorId 被忽略", func(t *testing.T) {
		repo := &doctorPatientsRouterRepo{items: seedItems, total: 1}
		router := newDoctorPatientsTestRouter(t,
			doctorPatientsRouterUserRepo{account: boundAccount, permissions: selectPermission},
			repo)

		token := signDoctorPatientsAccessToken(t, domainauth.RealmMis, misUserID)
		// doctorId 不是契约参数（§1.2、§6.10）：传入也不得改变数据范围，更不能退化为全量。
		w := performDoctorPatientsList(router, token, "doctorId=999")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（未知参数应被忽略而不是报错）; body=%s",
				w.Code, w.Body.String())
		}
		if repo.lastFilter.DoctorID != doctorID {
			t.Errorf("Filter.DoctorID = %d, want %d（必须来自令牌主体）",
				repo.lastFilter.DoctorID, doctorID)
		}
	})

	t.Run("空结果返回空数组而不是 null", func(t *testing.T) {
		repo := &doctorPatientsRouterRepo{} // 空结果：items 为 nil、total 为 0
		router := newDoctorPatientsTestRouter(t,
			doctorPatientsRouterUserRepo{account: boundAccount, permissions: selectPermission},
			repo)

		token := signDoctorPatientsAccessToken(t, domainauth.RealmMis, misUserID)
		w := performDoctorPatientsList(router, token, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		body := decodeRealmRouterBody(t, w)
		rawItems, exists := body["items"]
		if !exists {
			t.Fatalf("响应缺少 items 字段：%s", w.Body.String())
		}
		items, ok := rawItems.([]any)
		if !ok {
			t.Fatalf("items = %v, want JSON 数组（空列表必须是 [] 而不是 null）", rawItems)
		}
		if len(items) != 0 {
			t.Errorf("items 长度 = %d, want 0", len(items))
		}
		if body["total"] != float64(0) {
			t.Errorf("total = %v, want 0", body["total"])
		}
	})

	t.Run("非法 sort 返回 422", func(t *testing.T) {
		router := newDoctorPatientsTestRouter(t,
			doctorPatientsRouterUserRepo{account: boundAccount, permissions: selectPermission},
			&doctorPatientsRouterRepo{})

		token := signDoctorPatientsAccessToken(t, domainauth.RealmMis, misUserID)
		w := performDoctorPatientsList(router, token, "sort=pid")
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		body := decodeRealmRouterBody(t, w)
		if body["code"] != "REQUEST_VALIDATION_FAILED" {
			t.Errorf("code = %v, want REQUEST_VALIDATION_FAILED", body["code"])
		}
		if body["message"] != "排序字段或排序方向不支持" {
			t.Errorf("message = %v, want 排序字段或排序方向不支持", body["message"])
		}
	})
}

// doctorPatientsRouterFailingRepo 是 DoctorPatientRepository 桩：固定返回仓储错误。
// 只使用本文件内定义的自定义错误类型，避免为一个测试新增导入。
type doctorPatientsRouterFailingRepo struct{}

// ListDoctorPatients 固定返回「数据库故障」，用于驱动依赖不可用分支。
func (doctorPatientsRouterFailingRepo) ListDoctorPatients(
	_ context.Context,
	_ domaindoctorpatient.Filter,
	_, _ int,
) ([]domaindoctorpatient.Patient, int64, error) {
	return nil, 0, doctorPatientsRouterRepoDown{}
}

// doctorPatientsRouterRepoDown 模拟数据库/连接层故障（不是 ErrDoctorNotBound）。
type doctorPatientsRouterRepoDown struct{}

func (doctorPatientsRouterRepoDown) Error() string { return "db down" }

// TestRouterDoctorPatientsDependencyUnavailable 覆盖仓储返回普通错误时的响应：
// 用例把仓储错误归类为 ErrDependencyUnavailable，handler 必须映射成
// 502 DEPENDENCY_UNAVAILABLE（契约 §10），既不能是 403，也不能是笼统的 500。
func TestRouterDoctorPatientsDependencyUnavailable(t *testing.T) {
	const misUserID = 7
	doctorID := int64(16)
	account := &domainuser.User{ID: misUserID, Username: "mis-doctor", RefID: &doctorID}

	router := newDoctorPatientsTestRouter(t,
		doctorPatientsRouterUserRepo{
			account:     account,
			permissions: []string{"REGISTRATION:SELECT"},
		},
		doctorPatientsRouterFailingRepo{},
	)

	token := signDoctorPatientsAccessToken(t, domainauth.RealmMis, misUserID)
	w := performDoctorPatientsList(router, token, "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	body := decodeRealmRouterBody(t, w)
	if body["code"] != "DEPENDENCY_UNAVAILABLE" {
		t.Errorf("code = %v, want DEPENDENCY_UNAVAILABLE", body["code"])
	}
	if body["message"] != "服务暂时不可用，请稍后重试" {
		t.Errorf("message = %v, want 服务暂时不可用，请稍后重试", body["message"])
	}
}
