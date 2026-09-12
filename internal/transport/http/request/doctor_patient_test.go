package request

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/doctorpatient"
)

// 本文件覆盖医生工作台「我的患者」查询参数绑定
// （internal/transport/http/request/doctor_patient.go，契约 §6.10、§1.4）。
// 驱动方式与 public_catalog_test.go 一致：gin.New() + httptest 发真实请求，
// 只断言绑定结果与错误文案（handler 会把绑定 error 统一转成 422 REQUEST_VALIDATION_FAILED）。

// bindDoctorPatientListErr 用真实 HTTP 请求驱动「我的患者」列表参数绑定。
func bindDoctorPatientListErr(rawQuery string) (DoctorPatientListQuery, error) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var (
		result  DoctorPatientListQuery
		bindErr error
	)
	engine.GET("/api/v1/mis/doctor/patients", func(c *gin.Context) {
		result, bindErr = BindDoctorPatientList(c)
	})
	engine.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet,
			publicTestTarget("/api/v1/mis/doctor/patients", rawQuery), nil))
	return result, bindErr
}

// TestBindDoctorPatientListDefaults 断言缺省值：
// page=1、pageSize=20、sort=lastVisitDate、order=desc，keyword 为空串（不过滤）。
func TestBindDoctorPatientListDefaults(t *testing.T) {
	q, err := bindDoctorPatientListErr("")
	if err != nil {
		t.Fatalf("缺省绑定不应报错：%v", err)
	}
	if q.Keyword != "" {
		t.Errorf("keyword = %q, want 空串", q.Keyword)
	}
	if q.Page != 1 || q.PageSize != 20 {
		t.Errorf("page/pageSize = %d/%d, want 1/20", q.Page, q.PageSize)
	}
	if q.Sort != doctorpatient.SortLastVisitDate || q.Order != "desc" {
		t.Errorf("sort/order = %s/%s, want lastVisitDate/desc", q.Sort, q.Order)
	}
}

// TestBindDoctorPatientListParsesQuery 覆盖合法参数：
// keyword 首尾空白被裁剪，三个白名单排序字段都接受，order/page/pageSize 原样解析。
func TestBindDoctorPatientListParsesQuery(t *testing.T) {
	for _, sort := range []string{
		doctorpatient.SortLastVisitDate,
		doctorpatient.SortName,
		doctorpatient.SortRegistrationCount,
	} {
		t.Run("sort="+sort, func(t *testing.T) {
			query := url.Values{}
			query.Set("keyword", "  张  ")
			query.Set("sort", sort)
			query.Set("order", "asc")
			query.Set("page", "2")
			query.Set("pageSize", "10")

			q, err := bindDoctorPatientListErr(query.Encode())
			if err != nil {
				t.Fatalf("合法参数不应报错：%v", err)
			}
			if q.Keyword != "张" {
				t.Errorf("keyword = %q, want %q（首尾空白必须裁剪）", q.Keyword, "张")
			}
			if q.Sort != sort || q.Order != "asc" || q.Page != 2 || q.PageSize != 10 {
				t.Errorf("绑定结果错误：%+v", q)
			}
		})
	}
}

// TestBindDoctorPatientListValidation 覆盖非法参数（handler 统一映射 422）：
// 排序白名单、order 取值、page/pageSize 取值范围与 keyword 长度上限。
func TestBindDoctorPatientListValidation(t *testing.T) {
	tooLongKeyword := url.QueryEscape(strings.Repeat("熊", maxDoctorPatientKeywordLength+1))

	cases := []struct {
		name    string
		query   string
		message string
	}{
		{"sort 不在白名单", "sort=pid", "排序字段或排序方向不支持"},
		{"sort 为其它业务字段", "sort=password", "排序字段或排序方向不支持"},
		{"sort 大小写与白名单不一致", "sort=lastvisitdate", "排序字段或排序方向不支持"},
		{"order 非法取值", "order=drop", "排序字段或排序方向不支持"},
		{"order 大写不匹配", "order=DESC", "排序字段或排序方向不支持"},
		{"page 为 0", "page=0", "page必须为正整数"},
		{"page 为负数", "page=-1", "page必须为正整数"},
		{"page 非数字", "page=abc", "page必须为正整数"},
		{"page 超上限", "page=100001", "page不能超过100000"},
		{"pageSize 为 0", "pageSize=0", "pageSize必须在1到100之间"},
		{"pageSize 超上限", "pageSize=101", "pageSize必须在1到100之间"},
		{"pageSize 非数字", "pageSize=abc", "pageSize必须在1到100之间"},
		{"keyword 超过 50 个字符", "keyword=" + tooLongKeyword, "keyword 长度不能超过 50 个字符"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bindDoctorPatientListErr(tc.query); err == nil || err.Error() != tc.message {
				t.Fatalf("error = %v, want %q", err, tc.message)
			}
		})
	}
}

// TestBindDoctorPatientListKeywordBoundary 覆盖 keyword 长度边界（按字符而不是字节计）：
// 50 个 ASCII 与 50 个汉字都允许；长度按裁剪后的值计算。
func TestBindDoctorPatientListKeywordBoundary(t *testing.T) {
	cjk50 := strings.Repeat("熊", maxDoctorPatientKeywordLength)

	t.Run("50 个 ASCII 字符", func(t *testing.T) {
		q, err := bindDoctorPatientListErr(
			"keyword=" + strings.Repeat("a", maxDoctorPatientKeywordLength))
		if err != nil {
			t.Fatalf("50 个 ASCII 字符应允许：%v", err)
		}
		if len([]rune(q.Keyword)) != maxDoctorPatientKeywordLength {
			t.Errorf("keyword 字符数 = %d, want %d",
				len([]rune(q.Keyword)), maxDoctorPatientKeywordLength)
		}
	})

	t.Run("50 个汉字", func(t *testing.T) {
		q, err := bindDoctorPatientListErr("keyword=" + url.QueryEscape(cjk50))
		if err != nil {
			t.Fatalf("50 个汉字应允许：%v", err)
		}
		if q.Keyword != cjk50 {
			t.Errorf("keyword = %q, want %q", q.Keyword, cjk50)
		}
	})

	t.Run("裁剪后仍为 50 个汉字", func(t *testing.T) {
		q, err := bindDoctorPatientListErr("keyword=" + url.QueryEscape("  "+cjk50+"  "))
		if err != nil {
			t.Fatalf("应先裁剪首尾空白再计长度：%v", err)
		}
		if q.Keyword != cjk50 {
			t.Errorf("keyword = %q, want %q", q.Keyword, cjk50)
		}
	})

	t.Run("全空白 keyword 视为空串", func(t *testing.T) {
		q, err := bindDoctorPatientListErr("keyword=" + url.QueryEscape("   "))
		if err != nil {
			t.Fatalf("全空白 keyword 不应报错：%v", err)
		}
		if q.Keyword != "" {
			t.Errorf("keyword = %q, want 空串", q.Keyword)
		}
		// 顺带锁定缺省分页：若 handler 未被执行（拿到零值结构体与 nil 错误），
		// 上面的 keyword 断言同样会通过，必须靠缺省值证明绑定确实跑过。
		if q.Page != 1 || q.PageSize != 20 || q.Sort != doctorpatient.SortLastVisitDate {
			t.Errorf("缺省分页未生效：%+v", q)
		}
	})
}

// TestBindDoctorPatientListRejectsInvalidKeywordBytes 覆盖 keyword 的字节级校验：
// NUL 字节与非法 UTF-8 会被 PostgreSQL 与驱动拒绝，必须在绑定层拦成 422，
// 否则会在仓储层变成 500/502，把参数问题错报成依赖故障。
func TestBindDoctorPatientListRejectsInvalidKeywordBytes(t *testing.T) {
	cases := []struct {
		name    string
		keyword string
	}{
		{"中间含 NUL 字节", "a\x00b"},
		{"以 NUL 结尾", "张\x00"},
		{"含非法 UTF-8 字节", string([]byte{0xff, 0xfe})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := bindDoctorPatientListErr("keyword=" + url.QueryEscape(tc.keyword))
			if err == nil || err.Error() != "keyword 含有非法字符" {
				t.Fatalf("error = %v, want %q", err, "keyword 含有非法字符")
			}
			if q.Keyword != "" {
				t.Errorf("非法 keyword 不得回填查询条件，got %q", q.Keyword)
			}
		})
	}
}

// TestBindDoctorPatientListAcceptsValidKeyword 合法关键词（含 LIKE 通配符字符、反斜杠与单引号）
// 不受影响：绑定层只做字节合法性判断，转义交给仓储层的 ILIKE ... ESCAPE。
func TestBindDoctorPatientListAcceptsValidKeyword(t *testing.T) {
	for _, keyword := range []string{
		"张",
		"a_b",
		"100%",
		`C:\temp`,
		"张三 李四",
		"O'Brien",
	} {
		t.Run(keyword, func(t *testing.T) {
			q, err := bindDoctorPatientListErr("keyword=" + url.QueryEscape(keyword))
			if err != nil {
				t.Fatalf("合法 keyword %q 不应报错：%v", keyword, err)
			}
			if q.Keyword != keyword {
				t.Errorf("keyword = %q, want %q（绑定层不得改写用户输入）", q.Keyword, keyword)
			}
		})
	}
}
