package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domainauth "Medical-Web-Backend/internal/domain/auth"
)

// anonymousAllowedRoutePaths 是不做访问令牌校验的公开路由（按路由模板路径匹配）。
// 白名单之外的路由一律必须拒绝匿名请求；后续新增公开路由时必须同步登记到这里。
var anonymousAllowedRoutePaths = map[string]bool{
	"/":                        true,
	"/health":                  true,
	"/api/v1/mis/auth/login":   true,
	"/api/v1/mis/auth/refresh": true,
	"/api/v1/mis/auth/logout":  true,
	// 患者端认证接口自带凭据校验：wechat-login 用微信 code，
	// refresh 用 refresh token，logout 在 handler 内按严格语义校验患者 access token，
	// 因此都不挂 RequireAccessToken（spec/04-api-contract.md §7.1、§7.2）。
	"/api/v1/patient/auth/wechat-login": true,
	"/api/v1/patient/auth/refresh":      true,
	"/api/v1/patient/auth/logout":       true,

	// 匿名公开查询域（spec/04-api-contract.md §1.2、§8）：/public/* 是匿名只读域，
	// 不读取也不要求令牌，携带无效或跨域令牌同样必须返回正常业务响应；
	// 因此这些路由不属于「必须拒绝匿名请求」的受保护路由。
	"/api/v1/public/departments":                              true,
	"/api/v1/public/departments/:departmentId":                true,
	"/api/v1/public/departments/:departmentId/subdepartments": true,
	"/api/v1/public/doctors":                                  true,
	"/api/v1/public/doctors/:doctorId":                        true,
	"/api/v1/public/schedules":                                true,

	// 支付宝异步通知入口是 provider 回调：不读取用户 token、不进入权限矩阵，
	// 信任来源只有 RSA2 验签，因此同样不属于「必须拒绝匿名请求」的受保护路由
	// （spec/04-api-contract.md §1.2、§6.7）。
	"/api/v1/payments/alipay/notify": true,
}

// resolveRouteParams 把 gin 路由模板中的参数段替换为可请求的占位值，
// 例如 /api/v1/schedule/plans/:planId -> /api/v1/schedule/plans/1。
func resolveRouteParams(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, ":") || strings.HasPrefix(segment, "*") {
			segments[i] = "1"
		}
	}
	return strings.Join(segments, "/")
}

// TestRouterEveryProtectedRouteRejectsAnonymousRequest 遍历整张路由表做保护性断言：
// 除公开白名单外，每条路由的匿名请求都必须返回 401 AUTH_INVALID_TOKEN。
// 目的是防止后续新增路由漏挂 requireMisAccess（只校验若干代表路径的用例会漏掉新路由）。
func TestRouterEveryProtectedRouteRejectsAnonymousRequest(t *testing.T) {
	router, _ := newRealmTestRouter(t, domainauth.RealmMis)

	seenAllowed := make(map[string]bool, len(anonymousAllowedRoutePaths))
	protected := 0

	for _, route := range router.Routes() {
		if anonymousAllowedRoutePaths[route.Path] {
			seenAllowed[route.Path] = true
			continue
		}

		protected++
		requestPath := resolveRouteParams(route.Path)
		req := httptest.NewRequest(route.Method, requestPath, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf(
				"%s %s 匿名请求 status = %d, want 401（该路由可能漏挂访问令牌校验）; body=%s",
				route.Method,
				requestPath,
				w.Code,
				w.Body.String(),
			)
			continue
		}

		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Errorf(
				"%s %s 解析响应体失败：%v body=%s",
				route.Method,
				requestPath,
				err,
				w.Body.String(),
			)
			continue
		}
		if body["code"] != "AUTH_INVALID_TOKEN" {
			t.Errorf(
				"%s %s code = %v, want AUTH_INVALID_TOKEN",
				route.Method,
				requestPath,
				body["code"],
			)
		}
	}

	// 白名单路径必须真实存在，否则路由改名后白名单会静默失效、用例形同虚设。
	for path := range anonymousAllowedRoutePaths {
		if !seenAllowed[path] {
			t.Errorf("公开白名单路径 %s 不在路由表中，请同步维护白名单", path)
		}
	}

	// 至少要覆盖契约要求的全部管理端受保护路由，防止路由表意外缩水导致用例空转。
	if protected < 16 {
		t.Errorf("受保护路由数 = %d, want >= 16（契约要求的管理端受保护路由数量）", protected)
	}
}

// patientRealmExemptPrefixes 是允许接受患者域令牌的路由前缀。
// 该前缀之外的路由一律不得接受 realm=patient 的令牌；后续新增患者域路由时
// 必须把它的前缀登记到这里，否则会被本用例拦下（提示同步维护白名单）。
//
// 除患者域自身外，双 realm 共享业务路由（/api/v1/registrations/*，以及后续的
// /payments/*、/consultations/*、/prescriptions/*）也接受患者域令牌：
// 这些路由是「管理端带权限码、患者端只能访问本人资源」的共享业务接口
// （spec/04-api-contract.md §1.2），因此同样登记在此。
var patientRealmExemptPrefixes = []string{
	"/api/v1/patient/",
	// 注意不带结尾斜杠：共享业务集合自身（POST /api/v1/registrations）与它的子路径
	// 都属于同一组共享业务路由。
	"/api/v1/registrations",
	"/api/v1/payments",
}

// TestRouterNonPatientRoutesRejectPatientRealmToken 遍历整张路由表，断言除公开白名单
// 与患者域前缀外的每条路由都拒绝 realm=patient 的合法令牌（401 AUTH_INVALID_TOKEN）。
// 与匿名遍历相比，本用例用「签名正确但 realm 不对」的令牌进一步排除
// 「路由漏挂 realm 门禁、仅靠 claims 缺失兜底」的情况。
func TestRouterNonPatientRoutesRejectPatientRealmToken(t *testing.T) {
	router, patientToken := newRealmTestRouter(t, domainauth.RealmPatient)

	checked := 0
	for _, route := range router.Routes() {
		if anonymousAllowedRoutePaths[route.Path] {
			continue
		}
		if hasExemptPrefix(route.Path, patientRealmExemptPrefixes) {
			continue
		}

		checked++
		requestPath := resolveRouteParams(route.Path)
		req := httptest.NewRequest(route.Method, requestPath, nil)
		req.Header.Set("Authorization", "Bearer "+patientToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf(
				"%s %s 携带患者域令牌 status = %d, want 401; body=%s",
				route.Method,
				requestPath,
				w.Code,
				w.Body.String(),
			)
			continue
		}

		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Errorf(
				"%s %s 解析响应体失败：%v body=%s",
				route.Method,
				requestPath,
				err,
				w.Body.String(),
			)
			continue
		}
		if body["code"] != "AUTH_INVALID_TOKEN" {
			t.Errorf(
				"%s %s code = %v, want AUTH_INVALID_TOKEN",
				route.Method,
				requestPath,
				body["code"],
			)
		}
	}

	if checked < 16 {
		t.Errorf("被检查的非患者域路由数 = %d, want >= 16", checked)
	}
}

// hasExemptPrefix 判断路由模板路径是否落在任一豁免前缀下。
func hasExemptPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// TestRouterAlipayNotifyReachableAnonymously 单独覆盖支付宝异步通知入口的匿名可达性（审查 P3-3）：
// 该路由是 provider 回调，不读取任何用户令牌，因此匿名请求绝不能因为缺少令牌返回 401
// （返回 401 会让支付宝认为通知失败并持续重试）。用例不注入支付仓储，因此走不到业务成功分支，
// 重点断言「请求确实进入了业务处理、没有被令牌中间件拦下」。
func TestRouterAlipayNotifyReachableAnonymously(t *testing.T) {
	router := newContractTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments/alipay/notify",
		strings.NewReader("out_trade_no=202609080001&trade_status=TRADE_SUCCESS&total_amount=80.00"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// 允许的状态码：400/422 是验签或表单相关失败，500 是本用例未注入支付仓储导致的内部错误；
	// 唯独不允许 401（被令牌中间件拦下）与 404（路由未注册）。
	switch w.Code {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusInternalServerError:
	default:
		t.Fatalf("匿名通知状态码 = %d，期望 400/422/500（进入业务处理且不读令牌）; body=%s",
			w.Code, w.Body.String())
	}

	// 响应体可能是纯文本 success，也可能是统一错误 envelope；是 JSON 时不得是令牌错误。
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err == nil {
		if body["code"] == "AUTH_INVALID_TOKEN" {
			t.Fatalf("匿名通知被令牌中间件拦截：code=%v; body=%s", body["code"], w.Body.String())
		}
	}
}
