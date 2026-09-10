package request

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// 本文件覆盖医生列表 status 查询参数的可见性语义（“查询医生列表能展示离职或退休等状态医生”）：
// ACTIVE/RESIGNED/RETIRED/HIDDEN 四个合法值应原样透传，缺省与空串回落 ACTIVE
// （契约：列表默认只返回 ACTIVE），非法值必须报错而不是静默放行。
// 测试函数与 helper 均使用独特命名，避免与其它文件/未跟踪文件重名。

// bindCatalogDoctorsInTest 用 gin 测试上下文解析医生列表查询参数；
// query 为不含 '?' 的原始查询串，为空表示不带任何查询参数。
func bindCatalogDoctorsInTest(t *testing.T, query string) (CatalogDoctorsRequest, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	target := "/api/v1/catalog/doctors"
	if query != "" {
		target += "?" + query
	}
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	return BindCatalogDoctors(c)
}

// TestCatalogDoctorListStatusVisibility 验证 status 参数接受 ACTIVE/RESIGNED/RETIRED/HIDDEN，
// 缺省与空串都为 ACTIVE，非法值（小写、ALL、数字编码、未知状态）返回错误。
func TestCatalogDoctorListStatusVisibility(t *testing.T) {
	valid := []struct {
		name     string
		query    string
		expected string
	}{
		{"缺省为 ACTIVE", "", "ACTIVE"},
		{"空串回落 ACTIVE", "status=", "ACTIVE"},
		{"显式 ACTIVE", "status=ACTIVE", "ACTIVE"},
		{"显式 RESIGNED（离职医生可展示）", "status=RESIGNED", "RESIGNED"},
		{"显式 RETIRED（退休医生可展示）", "status=RETIRED", "RETIRED"},
		{"显式 HIDDEN（其它状态医生可展示）", "status=HIDDEN", "HIDDEN"},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			r, err := bindCatalogDoctorsInTest(t, tc.query)
			if err != nil {
				t.Fatalf("query=%q 意外错误：%v", tc.query, err)
			}
			if r.Status != tc.expected {
				t.Fatalf("Status = %q，期望 %q", r.Status, tc.expected)
			}
			// status 的取值不应影响其它默认分页/排序参数。
			if r.Page != 1 || r.PageSize != 20 || r.Sort != "id" || r.Order != "asc" {
				t.Fatalf("默认分页/排序被改变：page=%d pageSize=%d sort=%s order=%s",
					r.Page, r.PageSize, r.Sort, r.Order)
			}
		})
	}

	invalid := []struct {
		name  string
		query string
	}{
		{"小写 active", "status=active"},
		{"ALL 通配", "status=ALL"},
		{"数字编码 1", "status=1"},
		{"未知状态 DELETED", "status=DELETED"},
		{"前后含空格", "status=%20ACTIVE"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindCatalogDoctorsInTest(t, tc.query)
			if err == nil {
				t.Fatalf("query=%q 应返回校验错误", tc.query)
			}
			if err.Error() != "status只支持ACTIVE/RESIGNED/RETIRED/HIDDEN" {
				t.Fatalf("error = %q，期望固定文案", err.Error())
			}
		})
	}
}
