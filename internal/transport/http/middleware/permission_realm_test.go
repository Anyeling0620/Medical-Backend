package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/port"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

// realmPermissionUserRepo 是 UserRepository 桩：固定返回给定权限，并记录
// Permissions 被调用的次数——用于断言非 mis 主体绝不会触发管理端权限查询。
type realmPermissionUserRepo struct {
	port.UserRepository
	permissions []string
	queried     int
}

func (r *realmPermissionUserRepo) Permissions(
	_ context.Context,
	_ int64,
) ([]string, error) {
	r.queried++
	return r.permissions, nil
}

// newRealmPermissionEngine 预置 claims 后挂载 RequirePermissions，
// 终点 handler 被调用即证明权限校验通过（claims 为 nil 时不写入上下文）。
func newRealmPermissionEngine(
	t *testing.T,
	claims *userservice.AccessClaims,
	repo *realmPermissionUserRepo,
) (*gin.Engine, *bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	reached := false
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if claims != nil {
			c.Set(ClaimsKey, claims)
		}
		c.Next()
	})
	engine.Use(RequirePermissions(repo, []string{"ROOT", "CATALOG:SELECT"}))
	engine.GET("/permission-probe", func(c *gin.Context) {
		reached = true
		c.Status(http.StatusOK)
	})
	return engine, &reached
}

// TestRequirePermissionsRequiresMisRealm 覆盖权限中间件自身的 realm 断言
// （spec/02-architecture.md：必须先断言 realm=mis，再按主体查询管理端权限表）：
// 患者域或缺少 realm 的 claims 即使权限命中也要返回 401 AUTH_INVALID_TOKEN，
// 且不得触发任何管理端权限查询；只有 realm=mis 才继续走权限判定。
func TestRequirePermissionsRequiresMisRealm(t *testing.T) {
	cases := []struct {
		name            string
		claims          *userservice.AccessClaims
		repoPermissions []string
		wantStatus      int
		wantCode        string
		wantQueried     int
		wantReach       bool
	}{
		{
			name:            "患者域 claims 即使权限命中也必须 401",
			claims:          &userservice.AccessClaims{UserID: 7, Realm: domainauth.RealmPatient},
			repoPermissions: []string{"ROOT", "CATALOG:SELECT"},
			wantStatus:      http.StatusUnauthorized,
			wantCode:        "AUTH_INVALID_TOKEN",
			wantQueried:     0,
			wantReach:       false,
		},
		{
			name:            "缺少 realm 的 claims 必须 401",
			claims:          &userservice.AccessClaims{UserID: 7},
			repoPermissions: []string{"ROOT", "CATALOG:SELECT"},
			wantStatus:      http.StatusUnauthorized,
			wantCode:        "AUTH_INVALID_TOKEN",
			wantQueried:     0,
			wantReach:       false,
		},
		{
			name:            "无 claims 时保持 401",
			claims:          nil,
			repoPermissions: []string{"ROOT", "CATALOG:SELECT"},
			wantStatus:      http.StatusUnauthorized,
			wantCode:        "AUTH_INVALID_TOKEN",
			wantQueried:     0,
			wantReach:       false,
		},
		{
			name:            "管理域 claims 且权限命中时放行",
			claims:          &userservice.AccessClaims{UserID: 7, Realm: domainauth.RealmMis},
			repoPermissions: []string{"CATALOG:SELECT"},
			wantStatus:      http.StatusOK,
			wantQueried:     1,
			wantReach:       true,
		},
		{
			name:            "管理域 claims 但权限不命中仍 403",
			claims:          &userservice.AccessClaims{UserID: 7, Realm: domainauth.RealmMis},
			repoPermissions: []string{},
			wantStatus:      http.StatusForbidden,
			wantCode:        "AUTH_FORBIDDEN",
			wantQueried:     1,
			wantReach:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &realmPermissionUserRepo{permissions: tc.repoPermissions}
			engine, reached := newRealmPermissionEngine(t, tc.claims, repo)

			req := httptest.NewRequest(http.MethodGet, "/permission-probe", nil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf(
					"status = %d, want %d; body=%s",
					w.Code,
					tc.wantStatus,
					w.Body.String(),
				)
			}
			if tc.wantCode != "" {
				if code := decodeMisProbeBody(t, w)["code"]; code != tc.wantCode {
					t.Errorf("code = %v, want %v", code, tc.wantCode)
				}
			}
			if repo.queried != tc.wantQueried {
				t.Errorf(
					"Permissions 调用次数 = %d, want %d（非 mis 主体不得查询管理端权限表）",
					repo.queried,
					tc.wantQueried,
				)
			}
			if *reached != tc.wantReach {
				t.Errorf("是否进入 handler = %t, want %t", *reached, tc.wantReach)
			}
		})
	}
}
