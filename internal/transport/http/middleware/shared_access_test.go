package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/usecase/authsession"
)

// 本文件覆盖双 realm 共享业务路由的两个中间件（spec/04-api-contract.md §1.2）：
// RequireSharedAccess（先 patient、后 mis 的令牌解析与撤销检查）与
// RequirePermissionOrPatient（患者放行、管理端按权限编码校验）。
// 全部用例用手写桩替换 AccessTokenVerifier 与 UserRepository，不依赖 Redis/PostgreSQL。

const (
	// sharedAccessPatientID 是患者域令牌的主体主键。
	sharedAccessPatientID int64 = 20
	// sharedAccessMisUserID 是管理域令牌的主体主键。
	sharedAccessMisUserID int64 = 7
)

// sharedAccessVerifierStub 是 AccessTokenVerifier 桩：按令牌原文返回载荷，
// 记录调用次数与收到的 expectedRealm（用于断言 realm 绑定与撤销检查来源）。
type sharedAccessVerifierStub struct {
	realm     domainauth.Realm
	tokens    map[string]*authsession.AccessClaims
	revoked   map[string]bool
	revokeErr error

	parseCalls  int
	revokeCalls int
	seenRealms  []domainauth.Realm
}

func newSharedAccessVerifierStub(
	realm domainauth.Realm,
	tokens map[string]*authsession.AccessClaims,
) *sharedAccessVerifierStub {
	return &sharedAccessVerifierStub{
		realm:   realm,
		tokens:  tokens,
		revoked: map[string]bool{},
	}
}

// ParseAccessToken 只在 expectedRealm 与本桩 realm 一致时接受令牌，
// 模拟真实校验器「realm 不匹配即无效」的行为。
func (v *sharedAccessVerifierStub) ParseAccessToken(
	raw string,
	expectedRealm domainauth.Realm,
) (*authsession.AccessClaims, error) {
	v.parseCalls++
	v.seenRealms = append(v.seenRealms, expectedRealm)
	if expectedRealm != v.realm {
		return nil, errors.New("realm mismatch")
	}
	claims, ok := v.tokens[raw]
	if !ok {
		return nil, errors.New("invalid access token")
	}
	return claims, nil
}

func (v *sharedAccessVerifierStub) IsRevoked(_ context.Context, jti string) (bool, error) {
	v.revokeCalls++
	if v.revokeErr != nil {
		return false, v.revokeErr
	}
	return v.revoked[jti], nil
}

// sharedAccessClaims 构造指定 realm 的合法载荷（jti 用于撤销判定）。
func sharedAccessClaims(realm domainauth.Realm, userID int64, jti string) *authsession.AccessClaims {
	return &authsession.AccessClaims{
		UserID:    userID,
		Username:  "shared-access-user",
		TokenType: authsession.TokenTypeAccess,
		Realm:     realm,
		SessionID: "shared-access-session",
		RegisteredClaims: jwt.RegisteredClaims{
			ID: jti,
		},
	}
}

// sharedAccessEnv 汇总共享业务路由的测试引擎与两个校验器桩。
type sharedAccessEnv struct {
	engine  *gin.Engine
	patient *sharedAccessVerifierStub
	mis     *sharedAccessVerifierStub
	reached *bool
}

// newSharedAccessEnv 构造「RequireSharedAccess + 终点 handler」的最小引擎。
func newSharedAccessEnv(t *testing.T) *sharedAccessEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	patient := newSharedAccessVerifierStub(domainauth.RealmPatient, map[string]*authsession.AccessClaims{
		"patient-token":                 sharedAccessClaims(domainauth.RealmPatient, sharedAccessPatientID, "jti-patient"),
		"patient-token-without-subject": sharedAccessClaims(domainauth.RealmPatient, 0, "jti-patient-empty"),
	})
	mis := newSharedAccessVerifierStub(domainauth.RealmMis, map[string]*authsession.AccessClaims{
		"mis-token": sharedAccessClaims(domainauth.RealmMis, sharedAccessMisUserID, "jti-mis"),
	})

	reached := false
	engine := gin.New()
	engine.Use(RequireSharedAccess(patient, mis))
	engine.GET("/shared-probe", func(c *gin.Context) {
		reached = true
		claims, ok := ClaimsFrom(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"code": "TEST_CLAIMS_MISSING"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"realm": claims.Realm, "userId": claims.UserID})
	})
	return &sharedAccessEnv{engine: engine, patient: patient, mis: mis, reached: &reached}
}

// performSharedProbe 发起共享业务探测请求；mutate 用于附加 Authorization 头或 Cookie。
func performSharedProbe(
	engine *gin.Engine,
	mutate func(*http.Request),
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/shared-probe", nil)
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// TestRequireSharedAccessAcceptsBothRealms 合法的患者令牌与管理端令牌都必须放行，
// 并把含 realm 的载荷写入上下文供后续中间件使用（契约 §1.2）。
func TestRequireSharedAccessAcceptsBothRealms(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*http.Request)
		wantRealm string
		wantUser  float64
	}{
		{
			name:      "患者令牌（Authorization 头）",
			mutate:    withBearer("patient-token"),
			wantRealm: "patient",
			wantUser:  float64(sharedAccessPatientID),
		},
		{
			name: "患者令牌（Cookie 备用通道）",
			mutate: func(req *http.Request) {
				req.AddCookie(&http.Cookie{Name: authsession.AccessCookieName, Value: "patient-token"})
			},
			wantRealm: "patient",
			wantUser:  float64(sharedAccessPatientID),
		},
		{
			name:      "管理端令牌",
			mutate:    withBearer("mis-token"),
			wantRealm: "mis",
			wantUser:  float64(sharedAccessMisUserID),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newSharedAccessEnv(t)
			w := performSharedProbe(env.engine, tc.mutate)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if !*env.reached {
				t.Fatal("合法令牌应放行到后续 handler")
			}
			body := decodeMisProbeBody(t, w)
			if body["realm"] != tc.wantRealm {
				t.Errorf("上下文 claims.realm = %v, want %v", body["realm"], tc.wantRealm)
			}
			if body["userId"] != tc.wantUser {
				t.Errorf("上下文 claims.userId = %v, want %v", body["userId"], tc.wantUser)
			}
		})
	}
}

// TestRequireSharedAccessUsesMatchingVerifierForRevocation 撤销检查必须用解析该令牌的
// 同一会话层：患者令牌只查患者域、管理端令牌只查管理域，绝不交叉解释同一个 jti。
func TestRequireSharedAccessUsesMatchingVerifierForRevocation(t *testing.T) {
	t.Run("患者令牌只查患者域会话", func(t *testing.T) {
		env := newSharedAccessEnv(t)

		w := performSharedProbe(env.engine, withBearer("patient-token"))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if env.patient.revokeCalls != 1 || env.mis.revokeCalls != 0 {
			t.Errorf("撤销查询次数 patient=%d mis=%d, want 1/0",
				env.patient.revokeCalls, env.mis.revokeCalls)
		}
		if env.mis.parseCalls != 0 {
			t.Errorf("患者令牌已被接受后不应再尝试管理端校验，mis.parseCalls=%d", env.mis.parseCalls)
		}
	})

	t.Run("管理端令牌只查管理域会话", func(t *testing.T) {
		env := newSharedAccessEnv(t)

		w := performSharedProbe(env.engine, withBearer("mis-token"))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if env.mis.revokeCalls != 1 || env.patient.revokeCalls != 0 {
			t.Errorf("撤销查询次数 patient=%d mis=%d, want 0/1",
				env.patient.revokeCalls, env.mis.revokeCalls)
		}
	})
}

// TestRequireSharedAccessDoesNotCrossInterpretRealm 每个校验器只会收到自己 realm 的
// expectedRealm，中间件不会用另一域的语义解释同一个令牌（契约 §1.2）。
func TestRequireSharedAccessDoesNotCrossInterpretRealm(t *testing.T) {
	env := newSharedAccessEnv(t)

	if w := performSharedProbe(env.engine, withBearer("mis-token")); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	for _, realm := range env.patient.seenRealms {
		if realm != domainauth.RealmPatient {
			t.Errorf("患者校验器收到 expectedRealm=%q, want patient", realm)
		}
	}
	for _, realm := range env.mis.seenRealms {
		if realm != domainauth.RealmMis {
			t.Errorf("管理端校验器收到 expectedRealm=%q, want mis", realm)
		}
	}
}

// TestRequireSharedAccessRejectsInvalidTokens 匿名、未知、已撤销与缺少主体的令牌
// 统一返回 401 AUTH_INVALID_TOKEN，且不得进入后续 handler。
func TestRequireSharedAccessRejectsInvalidTokens(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(env *sharedAccessEnv)
		mutate  func(*http.Request)
	}{
		{name: "匿名请求", mutate: nil},
		{name: "未知令牌", mutate: withBearer("unknown-token")},
		{
			name:   "患者令牌缺少主体",
			mutate: withBearer("patient-token-without-subject"),
		},
		{
			name: "患者令牌已撤销",
			prepare: func(env *sharedAccessEnv) {
				env.patient.revoked["jti-patient"] = true
			},
			mutate: withBearer("patient-token"),
		},
		{
			name: "管理端令牌已撤销",
			prepare: func(env *sharedAccessEnv) {
				env.mis.revoked["jti-mis"] = true
			},
			mutate: withBearer("mis-token"),
		},
		{
			name: "撤销检查失败时按无效处理",
			prepare: func(env *sharedAccessEnv) {
				env.patient.revokeErr = errors.New("redis is down")
			},
			mutate: withBearer("patient-token"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newSharedAccessEnv(t)
			if tc.prepare != nil {
				tc.prepare(env)
			}

			w := performSharedProbe(env.engine, tc.mutate)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
			if code := decodeMisProbeBody(t, w)["code"]; code != "AUTH_INVALID_TOKEN" {
				t.Errorf("code = %v, want AUTH_INVALID_TOKEN", code)
			}
			if *env.reached {
				t.Error("非法令牌不得进入后续 handler")
			}
		})
	}
}

// sharedPermissionUserRepo 是 UserRepository 桩：固定返回给定权限或错误，
// 并记录权限查询次数（用于断言患者令牌不会触发管理端权限查询）。
type sharedPermissionUserRepo struct {
	port.UserRepository

	permissions []string
	err         error
	queried     int
}

func (r *sharedPermissionUserRepo) Permissions(_ context.Context, _ int64) ([]string, error) {
	r.queried++
	if r.err != nil {
		return nil, r.err
	}
	return r.permissions, nil
}

// newSharedPermissionEngine 预置 claims 后挂载 RequirePermissionOrPatient，
// 终点 handler 被调用即证明授权通过（claims 为 nil 时不写入上下文）。
func newSharedPermissionEngine(
	t *testing.T,
	claims *authsession.AccessClaims,
	repo port.UserRepository,
	allowed []string,
) (*gin.Engine, *bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	reached := false
	engine := gin.New()
	if claims != nil {
		engine.Use(func(c *gin.Context) {
			c.Set(ClaimsKey, claims)
			c.Next()
		})
	}
	engine.Use(RequirePermissionOrPatient(repo, allowed))
	engine.GET("/permission-or-patient", func(c *gin.Context) {
		reached = true
		c.Status(http.StatusOK)
	})
	return engine, &reached
}

// TestRequirePermissionOrPatient 授权矩阵：患者令牌直接放行（越权由用例层按 404 隐藏），
// 管理端令牌必须命中权限编码（含 ROOT），否则 403 AUTH_FORBIDDEN（契约 §1.2、§10）。
func TestRequirePermissionOrPatient(t *testing.T) {
	allowed := []string{"ROOT", "REGISTRATION:SELECT"}

	cases := []struct {
		name            string
		claims          *authsession.AccessClaims
		repoPermissions []string
		repoErr         error
		nilRepo         bool
		wantStatus      int
		wantCode        string
		wantQueried     int
		wantReach       bool
	}{
		{
			name:        "患者令牌直接放行且不查管理端权限",
			claims:      sharedAccessClaims(domainauth.RealmPatient, sharedAccessPatientID, "jti-patient"),
			wantStatus:  http.StatusOK,
			wantQueried: 0,
			wantReach:   true,
		},
		{
			name:            "管理端令牌命中 REGISTRATION:SELECT",
			claims:          sharedAccessClaims(domainauth.RealmMis, sharedAccessMisUserID, "jti-mis"),
			repoPermissions: []string{"REGISTRATION:SELECT"},
			wantStatus:      http.StatusOK,
			wantQueried:     1,
			wantReach:       true,
		},
		{
			name:            "管理端 ROOT 可放行任意共享业务路由",
			claims:          sharedAccessClaims(domainauth.RealmMis, sharedAccessMisUserID, "jti-mis"),
			repoPermissions: []string{"ROOT"},
			wantStatus:      http.StatusOK,
			wantQueried:     1,
			wantReach:       true,
		},
		{
			name:            "管理端无权限返回 403",
			claims:          sharedAccessClaims(domainauth.RealmMis, sharedAccessMisUserID, "jti-mis"),
			repoPermissions: []string{"REGISTRATION:INSERT"},
			wantStatus:      http.StatusForbidden,
			wantCode:        "AUTH_FORBIDDEN",
			wantQueried:     1,
			wantReach:       false,
		},
		{
			name:            "管理端权限为空返回 403",
			claims:          sharedAccessClaims(domainauth.RealmMis, sharedAccessMisUserID, "jti-mis"),
			repoPermissions: []string{},
			wantStatus:      http.StatusForbidden,
			wantCode:        "AUTH_FORBIDDEN",
			wantQueried:     1,
			wantReach:       false,
		},
		{
			name:       "缺少 claims 返回 401",
			claims:     nil,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "AUTH_INVALID_TOKEN",
			wantReach:  false,
		},
		{
			name:       "非法 realm 返回 401",
			claims:     sharedAccessClaims("admin", 7, "jti-other"),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "AUTH_INVALID_TOKEN",
			wantReach:  false,
		},
		{
			name:        "权限查询失败返回 500",
			claims:      sharedAccessClaims(domainauth.RealmMis, sharedAccessMisUserID, "jti-mis"),
			repoErr:     errors.New("postgres is down"),
			wantStatus:  http.StatusInternalServerError,
			wantCode:    "INTERNAL_SERVER_ERROR",
			wantQueried: 1,
			wantReach:   false,
		},
		{
			name:       "缺少用户仓储返回 500",
			claims:     sharedAccessClaims(domainauth.RealmMis, sharedAccessMisUserID, "jti-mis"),
			nilRepo:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "INTERNAL_SERVER_ERROR",
			wantReach:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var repo port.UserRepository
			if !tc.nilRepo {
				repo = &sharedPermissionUserRepo{permissions: tc.repoPermissions, err: tc.repoErr}
			}
			engine, reached := newSharedPermissionEngine(t, tc.claims, repo, allowed)

			req := httptest.NewRequest(http.MethodGet, "/permission-or-patient", nil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantCode != "" {
				if code := decodeMisProbeBody(t, w)["code"]; code != tc.wantCode {
					t.Errorf("code = %v, want %v", code, tc.wantCode)
				}
			}
			queried := 0
			if stub, ok := repo.(*sharedPermissionUserRepo); ok {
				queried = stub.queried
			}
			if queried != tc.wantQueried {
				t.Errorf("Permissions 调用次数 = %d, want %d", queried, tc.wantQueried)
			}
			if *reached != tc.wantReach {
				t.Errorf("是否进入 handler = %t, want %t", *reached, tc.wantReach)
			}
		})
	}
}
