package request

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 本文件覆盖匿名公开域查询参数绑定（internal/transport/http/request/public_catalog.go）。
// 驱动方式与 BindPlanListErr 一致：gin.New() + httptest 发真实请求，
// 只断言绑定结果与错误文案（handler 会把绑定 error 统一转成 422 REQUEST_VALIDATION_FAILED）。

// publicTestTarget 拼接路径与查询串；查询串为空时保持路径原样。
func publicTestTarget(path, rawQuery string) string {
	if rawQuery == "" {
		return path
	}
	return path + "?" + rawQuery
}

// bindPublicDepartmentsErr 用真实 HTTP 请求驱动科室列表参数绑定。
func bindPublicDepartmentsErr(rawQuery string) (PublicDepartmentListQuery, error) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var (
		result  PublicDepartmentListQuery
		bindErr error
	)
	engine.GET("/api/v1/public/departments", func(c *gin.Context) {
		result, bindErr = BindPublicDepartments(c)
	})
	engine.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, publicTestTarget("/api/v1/public/departments", rawQuery), nil))
	return result, bindErr
}

// bindPublicSubdepartmentsErr 用真实 HTTP 请求驱动子科室列表参数绑定（departmentId 来自路径参数）。
func bindPublicSubdepartmentsErr(rawQuery string, departmentID int64) (PublicSubdepartmentListQuery, error) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var (
		result  PublicSubdepartmentListQuery
		bindErr error
	)
	engine.GET("/api/v1/public/departments/:departmentId/subdepartments", func(c *gin.Context) {
		result, bindErr = BindPublicSubdepartments(c, departmentID)
	})
	engine.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, publicTestTarget("/api/v1/public/departments/7/subdepartments", rawQuery), nil))
	return result, bindErr
}

// bindPublicDoctorsErr 用真实 HTTP 请求驱动医生列表参数绑定。
func bindPublicDoctorsErr(rawQuery string) (PublicDoctorListQuery, error) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var (
		result  PublicDoctorListQuery
		bindErr error
	)
	engine.GET("/api/v1/public/doctors", func(c *gin.Context) {
		result, bindErr = BindPublicDoctors(c)
	})
	engine.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, publicTestTarget("/api/v1/public/doctors", rawQuery), nil))
	return result, bindErr
}

// bindPublicSchedulesErr 用真实 HTTP 请求驱动可挂号时段参数绑定。
func bindPublicSchedulesErr(rawQuery string) (PublicScheduleListQuery, error) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var (
		result  PublicScheduleListQuery
		bindErr error
	)
	engine.GET("/api/v1/public/schedules", func(c *gin.Context) {
		result, bindErr = BindPublicSchedules(c)
	})
	engine.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, publicTestTarget("/api/v1/public/schedules", rawQuery), nil))
	return result, bindErr
}

// TestBindPublicDepartmentsDefaults 缺省分页为 page=1、pageSize=20，排序为 name/asc，过滤条件为空。
func TestBindPublicDepartmentsDefaults(t *testing.T) {
	q, err := bindPublicDepartmentsErr("")
	if err != nil {
		t.Fatalf("缺省绑定不应报错：%v", err)
	}
	if q.Page != 1 || q.PageSize != 20 {
		t.Errorf("page/pageSize = %d/%d, want 1/20", q.Page, q.PageSize)
	}
	if q.Sort != "name" || q.Order != "asc" {
		t.Errorf("sort/order = %s/%s, want name/asc", q.Sort, q.Order)
	}
	if q.Outpatient != nil || q.Recommended != nil {
		t.Errorf("布尔过滤缺省应为未传：%+v", q)
	}
	filter := q.Filter()
	if filter.Outpatient != nil || filter.Recommended != nil || filter.Sort != "name" || filter.Order != "asc" {
		t.Errorf("Filter 转换错误：%+v", filter)
	}
}

// TestBindPublicDoctorsDefaults 医生列表缺省排序为 id/asc。
func TestBindPublicDoctorsDefaults(t *testing.T) {
	q, err := bindPublicDoctorsErr("")
	if err != nil {
		t.Fatalf("缺省绑定不应报错：%v", err)
	}
	if q.Page != 1 || q.PageSize != 20 || q.Sort != "id" || q.Order != "asc" {
		t.Errorf("缺省值错误：%+v", q)
	}
	if q.DepartmentID != nil || q.SubdepartmentID != nil || q.Name != nil {
		t.Errorf("过滤条件缺省应为空：%+v", q)
	}
}

// TestBindPublicSubdepartmentsDefaults 子科室列表沿用统一分页缺省值，并透传路径中的科室编号。
func TestBindPublicSubdepartmentsDefaults(t *testing.T) {
	q, err := bindPublicSubdepartmentsErr("", 7)
	if err != nil {
		t.Fatalf("缺省绑定不应报错：%v", err)
	}
	if q.DepartmentID != 7 || q.Page != 1 || q.PageSize != 20 {
		t.Errorf("绑定结果错误：%+v", q)
	}
}

// TestBindPublicSchedulesDefaults 时段查询缺省不带日期与过滤，交由 use case 推导窗口。
func TestBindPublicSchedulesDefaults(t *testing.T) {
	q, err := bindPublicSchedulesErr("")
	if err != nil {
		t.Fatalf("缺省绑定不应报错：%v", err)
	}
	if q.FromDate != "" || q.ToDate != "" || q.Page != 1 || q.PageSize != 20 {
		t.Errorf("缺省值错误：%+v", q)
	}
	if q.SubdepartmentID != nil || q.DoctorID != nil {
		t.Errorf("缺省过滤应为空：%+v", q)
	}
}

// TestBindPublicPageValidation 分页参数边界：page 与 pageSize 的上下限与错误文案。
func TestBindPublicPageValidation(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		message string
	}{
		{"page 为 0", "page=0", "page必须为正整数"},
		{"page 为负数", "page=-1", "page必须为正整数"},
		{"page 非数字", "page=abc", "page必须为正整数"},
		{"page 超上限", "page=100001", "page不能超过100000"},
		{"pageSize 为 0", "pageSize=0", "pageSize必须在1到100之间"},
		{"pageSize 超上限", "pageSize=101", "pageSize必须在1到100之间"},
		{"pageSize 非数字", "pageSize=abc", "pageSize必须在1到100之间"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bindPublicDepartmentsErr(tc.query); err == nil || err.Error() != tc.message {
				t.Fatalf("error = %v, want %q", err, tc.message)
			}
			if _, err := bindPublicDoctorsErr(tc.query); err == nil || err.Error() != tc.message {
				t.Fatalf("医生列表 error = %v, want %q", err, tc.message)
			}
			if _, err := bindPublicSchedulesErr(tc.query); err == nil || err.Error() != tc.message {
				t.Fatalf("时段查询 error = %v, want %q", err, tc.message)
			}
		})
	}
}

// TestBindPublicDepartmentsValidation 科室列表排序白名单与布尔参数校验文案。
func TestBindPublicDepartmentsValidation(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		message string
	}{
		{"sort 不在白名单", "sort=password", "排序字段或排序方向不支持"},
		{"order 非法", "order=drop", "排序字段或排序方向不支持"},
		{"outpatient 非布尔", "outpatient=maybe", "outpatient必须为布尔值"},
		{"recommended 非布尔", "recommended=yes", "recommended必须为布尔值"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bindPublicDepartmentsErr(tc.query); err == nil || err.Error() != tc.message {
				t.Fatalf("error = %v, want %q", err, tc.message)
			}
		})
	}
}

// TestBindPublicDepartmentsParsesQuery 合法查询参数解析：布尔过滤、排序白名单与分页。
func TestBindPublicDepartmentsParsesQuery(t *testing.T) {
	query := url.Values{}
	query.Set("outpatient", "true")
	query.Set("recommended", "false")
	query.Set("sort", "id")
	query.Set("order", "desc")
	query.Set("page", "2")
	query.Set("pageSize", "5")

	q, err := bindPublicDepartmentsErr(query.Encode())
	if err != nil {
		t.Fatalf("合法参数不应报错：%v", err)
	}
	if q.Outpatient == nil || !*q.Outpatient {
		t.Errorf("outpatient = %v, want true", q.Outpatient)
	}
	if q.Recommended == nil || *q.Recommended {
		t.Errorf("recommended = %v, want false", q.Recommended)
	}
	if q.Sort != "id" || q.Order != "desc" || q.Page != 2 || q.PageSize != 5 {
		t.Errorf("绑定结果错误：%+v", q)
	}
	filter := q.Filter()
	if filter.Outpatient == nil || !*filter.Outpatient ||
		filter.Recommended == nil || *filter.Recommended {
		t.Errorf("Filter 转换错误：%+v", filter)
	}
}

// TestBindPublicDoctorsValidation 医生列表的编号、排序与名称长度校验文案。
func TestBindPublicDoctorsValidation(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		message string
	}{
		{"departmentId 为 0", "departmentId=0", "departmentId必须为正整数"},
		{"departmentId 非数字", "departmentId=abc", "departmentId必须为正整数"},
		{"subdepartmentId 负数", "subdepartmentId=-2", "subdepartmentId必须为正整数"},
		{"sort 不在白名单", "sort=password", "排序字段或排序方向不支持"},
		{"order 非法", "order=drop", "排序字段或排序方向不支持"},
		{"name 超过 50 字符", "name=" + url.QueryEscape(strings.Repeat("熊", 51)), "name最多50个字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bindPublicDoctorsErr(tc.query); err == nil || err.Error() != tc.message {
				t.Fatalf("error = %v, want %q", err, tc.message)
			}
		})
	}
}

// TestBindPublicDoctorsNameLengthBoundary name 长度按 rune 计：50 个汉字允许，51 个报错。
func TestBindPublicDoctorsNameLengthBoundary(t *testing.T) {
	ascii50 := strings.Repeat("a", 50)
	if _, err := bindPublicDoctorsErr("name=" + ascii50); err != nil {
		t.Errorf("50 个 ASCII 字符应允许：%v", err)
	}
	cjk50 := strings.Repeat("熊", 50)
	q, err := bindPublicDoctorsErr("name=" + url.QueryEscape(cjk50))
	if err != nil {
		t.Fatalf("50 个汉字应允许：%v", err)
	}
	if q.Name == nil || *q.Name != cjk50 {
		t.Errorf("name = %v, want %q", q.Name, cjk50)
	}
}

// TestBindPublicDoctorsParsesQuery 医生列表合法参数解析：编号过滤、名称模糊、排序与分页。
func TestBindPublicDoctorsParsesQuery(t *testing.T) {
	query := url.Values{}
	query.Set("departmentId", "3")
	query.Set("subdepartmentId", "9")
	query.Set("name", "熊")
	query.Set("sort", "hireDate")
	query.Set("order", "desc")
	query.Set("page", "2")
	query.Set("pageSize", "10")

	q, err := bindPublicDoctorsErr(query.Encode())
	if err != nil {
		t.Fatalf("合法参数不应报错：%v", err)
	}
	if q.DepartmentID == nil || *q.DepartmentID != 3 {
		t.Errorf("departmentId = %v", q.DepartmentID)
	}
	if q.SubdepartmentID == nil || *q.SubdepartmentID != 9 {
		t.Errorf("subdepartmentId = %v", q.SubdepartmentID)
	}
	if q.Name == nil || *q.Name != "熊" {
		t.Errorf("name = %v", q.Name)
	}
	if q.Sort != "hireDate" || q.Order != "desc" || q.Page != 2 || q.PageSize != 10 {
		t.Errorf("绑定结果错误：%+v", q)
	}
	filter := q.Filter()
	if filter.DepartmentID == nil || *filter.DepartmentID != 3 ||
		filter.SubdepartmentID == nil || *filter.SubdepartmentID != 9 ||
		filter.Name == nil || *filter.Name != "熊" ||
		filter.Sort != "hireDate" || filter.Order != "desc" {
		t.Errorf("Filter 转换错误：%+v", filter)
	}
}

// TestBindPublicDoctorsEmptyNameIgnored 空字符串 name 视为未传（避免 LIKE '%%' 之外的歧义）。
func TestBindPublicDoctorsEmptyNameIgnored(t *testing.T) {
	q, err := bindPublicDoctorsErr("name=")
	if err != nil {
		t.Fatalf("空 name 不应报错：%v", err)
	}
	if q.Name != nil {
		t.Errorf("name = %v, want nil", q.Name)
	}
}

// TestBindPublicSchedulesValidation 时段查询编号与日期格式校验文案。
func TestBindPublicSchedulesValidation(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		message string
	}{
		{"subdepartmentId 为 0", "subdepartmentId=0", "subdepartmentId必须为正整数"},
		{"doctorId 为 -1", "doctorId=-1", "doctorId必须为正整数"},
		{"doctorId 非数字", "doctorId=abc", "doctorId必须为正整数"},
		{"fromDate 非 YYYY-MM-DD", "fromDate=2026-9-1", "fromDate格式必须为YYYY-MM-DD"},
		{"fromDate 用斜杠", "fromDate=2026/09/01", "fromDate格式必须为YYYY-MM-DD"},
		{"toDate 非法日期", "toDate=2026-02-30", "toDate格式必须为YYYY-MM-DD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bindPublicSchedulesErr(tc.query); err == nil || err.Error() != tc.message {
				t.Fatalf("error = %v, want %q", err, tc.message)
			}
		})
	}
}

// TestBindPublicSchedulesParsesQuery 合法参数解析：编号过滤与日期原样透传（不做窗口推导）。
func TestBindPublicSchedulesParsesQuery(t *testing.T) {
	query := url.Values{}
	query.Set("subdepartmentId", "2")
	query.Set("doctorId", "16")
	query.Set("fromDate", "2026-09-20")
	query.Set("toDate", "2026-09-26")
	query.Set("page", "2")
	query.Set("pageSize", "5")

	q, err := bindPublicSchedulesErr(query.Encode())
	if err != nil {
		t.Fatalf("合法参数不应报错：%v", err)
	}
	if q.SubdepartmentID == nil || *q.SubdepartmentID != 2 {
		t.Errorf("subdepartmentId = %v", q.SubdepartmentID)
	}
	if q.DoctorID == nil || *q.DoctorID != 16 {
		t.Errorf("doctorId = %v", q.DoctorID)
	}
	if q.FromDate != "2026-09-20" || q.ToDate != "2026-09-26" {
		t.Errorf("日期透传 = %s..%s", q.FromDate, q.ToDate)
	}
	if q.Page != 2 || q.PageSize != 5 {
		t.Errorf("分页 = %d/%d, want 2/5", q.Page, q.PageSize)
	}
}
