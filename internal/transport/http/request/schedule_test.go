package request

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// scheduleTestEngine 注册排班四个接口的绑定处理路由，便于以真实 HTTP 请求驱动校验。
func scheduleTestEngine(t *testing.T) (*gin.Engine, *PlanListQuery, *CreatePlanBody, *UpdatePlanBody, *string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var (
		list   PlanListQuery
		create CreatePlanBody
		update UpdatePlanBody
		key    string
	)
	engine := gin.New()
	engine.GET("/api/v1/schedule/plans", func(c *gin.Context) {
		list, _ = BindPlanList(c)
	})
	engine.POST("/api/v1/schedule/plans", func(c *gin.Context) {
		create, _ = BindCreatePlan(c)
		key, _ = BindIdempotencyKey(c)
	})
	engine.PATCH("/api/v1/schedule/plans/:planId", func(c *gin.Context) {
		update, _ = BindUpdatePlanMaximum(c)
	})
	return engine, &list, &create, &update, &key
}

// performPlanRequest 执行一次请求并丢弃响应（只关心绑定结果）。
func performPlanRequest(engine *gin.Engine, method, target string, body string, header map[string]string) {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	engine.ServeHTTP(httptest.NewRecorder(), req)
}

func TestBindPlanListDefaults(t *testing.T) {
	engine, list, _, _, _ := scheduleTestEngine(t)
	performPlanRequest(engine, http.MethodGet, "/api/v1/schedule/plans", "", nil)
	if list.Page != 1 || list.PageSize != 20 {
		t.Errorf("page/pageSize = %d/%d, want 1/20", list.Page, list.PageSize)
	}
	if list.Sort != "id" || list.Order != "asc" {
		t.Errorf("sort/order = %s/%s, want id/asc", list.Sort, list.Order)
	}
	if list.IncludeSlots {
		t.Errorf("includeSlots 默认应为 false")
	}
	if list.DoctorID != nil || list.DepartmentID != nil || list.SubdepartmentID != nil {
		t.Errorf("过滤条件默认应为空：%+v", list)
	}
}

func TestBindPlanListParsesQuery(t *testing.T) {
	engine, list, _, _, _ := scheduleTestEngine(t)
	q := url.Values{}
	q.Set("doctorId", "16")
	q.Set("departmentId", "2")
	q.Set("subdepartmentId", "9")
	q.Set("fromDate", "2026-09-20")
	q.Set("toDate", "2026-09-30")
	q.Set("includeSlots", "true")
	q.Set("page", "3")
	q.Set("pageSize", "25")
	q.Set("sort", "date")
	q.Set("order", "desc")
	performPlanRequest(engine, http.MethodGet, "/api/v1/schedule/plans?"+q.Encode(), "", nil)
	if list.DoctorID == nil || *list.DoctorID != 16 {
		t.Errorf("doctorId = %v", list.DoctorID)
	}
	if list.DepartmentID == nil || *list.DepartmentID != 2 {
		t.Errorf("departmentId = %v", list.DepartmentID)
	}
	if list.SubdepartmentID == nil || *list.SubdepartmentID != 9 {
		t.Errorf("subdepartmentId = %v", list.SubdepartmentID)
	}
	if list.FromDate != "2026-09-20" || list.ToDate != "2026-09-30" {
		t.Errorf("date range = %s..%s", list.FromDate, list.ToDate)
	}
	if !list.IncludeSlots {
		t.Errorf("includeSlots 应解析为 true")
	}
	if list.Page != 3 || list.PageSize != 25 || list.Sort != "date" || list.Order != "desc" {
		t.Errorf("paging/sort = %+v", list)
	}
	filter := list.ToFilter()
	if filter.DoctorID == nil || filter.FromDate != "2026-09-20" || !filter.IncludeSlots {
		t.Errorf("ToFilter 转换错误: %+v", filter)
	}
}

func TestBindCreatePlanAndIdempotencyKey(t *testing.T) {
	engine, _, create, _, key := scheduleTestEngine(t)
	header := map[string]string{"Idempotency-Key": "client-request-001"}
	performPlanRequest(engine, http.MethodPost, "/api/v1/schedule/plans",
		`{"doctorId":16,"subdepartmentId":2,"date":"2026-09-20","maximum":45}`, header)
	if create.DoctorID != 16 || create.SubdepartmentID != 2 || create.Date != "2026-09-20" || create.Maximum != 45 {
		t.Errorf("create = %+v", create)
	}
	if *key != "client-request-001" {
		t.Errorf("idempotency key = %q", *key)
	}
}

// TestValidateIdempotencyKey 覆盖长度边界、可打印 ASCII 边界与非法字符。
func TestValidateIdempotencyKey(t *testing.T) {
	for _, raw := range []string{"k", "client-key-1"} {
		if len(raw) < 1 || len(raw) > 128 {
			t.Fatalf("fixture out of range: %q", raw)
		}
	}
	ok := strings.Repeat("a", 128)
	for _, r := range ok {
		if r < 0x20 || r > 0x7E {
			t.Fatalf("fixture has non-printable char")
		}
	}
	engine := gin.New()
	var got string
	var gotErr error
	engine.GET("/check", func(c *gin.Context) { got, gotErr = BindIdempotencyKey(c) })
	req := httptest.NewRequest(http.MethodGet, "/check", nil)
	req.Header.Set("Idempotency-Key", ok)
	engine.ServeHTTP(httptest.NewRecorder(), req)
	if gotErr != nil || got != ok {
		t.Errorf("128 个可打印 ASCII 应通过: got=%v err=%v", got, gotErr)
	}
}

// bindCreateError 提供创建请求体绑定的错误断言。
func bindCreateError(t *testing.T, body string) error {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var bindErr error
	engine.POST("/api/v1/schedule/plans", func(c *gin.Context) { _, bindErr = BindCreatePlan(c) })
	performPlanRequest(engine, http.MethodPost, "/api/v1/schedule/plans", body, nil)
	return bindErr
}

// bindUpdateError 提供更新请求体绑定的错误断言。
func bindUpdateError(t *testing.T, body string) error {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var bindErr error
	engine.PATCH("/api/v1/schedule/plans/:planId", func(c *gin.Context) { _, bindErr = BindUpdatePlanMaximum(c) })
	performPlanRequest(engine, http.MethodPatch, "/api/v1/schedule/plans/4", body, nil)
	return bindErr
}

func TestBindCreatePlanValidationMessages(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		message string
	}{
		{"missing doctor", `{"subdepartmentId":2,"date":"2026-09-20","maximum":45}`, "医生编号必须为正整数"},
		{"zero doctor", `{"doctorId":0,"subdepartmentId":2,"date":"2026-09-20","maximum":45}`, "医生编号必须为正整数"},
		{"missing subdepartment", `{"doctorId":16,"date":"2026-09-20","maximum":45}`, "子科室编号必须为正整数"},
		{"bad date", `{"doctorId":16,"subdepartmentId":2,"date":"09/20/2026","maximum":45}`, "日期格式必须为 YYYY-MM-DD"},
		{"invalid calendar date", `{"doctorId":16,"subdepartmentId":2,"date":"2026-02-30","maximum":45}`, "日期格式必须为 YYYY-MM-DD"},
		{"zero maximum", `{"doctorId":16,"subdepartmentId":2,"date":"2026-09-20","maximum":0}`, "最大号源必须大于 0"},
		{"oversize maximum", `{"doctorId":16,"subdepartmentId":2,"date":"2026-09-20","maximum":32768}`, "最大号源不能超过 32767"},
		{"unknown field", `{"doctorId":16,"subdepartmentId":2,"date":"2026-09-20","maximum":45,"extra":1}`, ""},
		{"malformed json", `{"doctorId":`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := bindCreateError(t, tc.body)
			if tc.message == "" {
				if err == nil {
					t.Fatalf("expected binding error, got nil")
				}
				return
			}
			if err == nil || err.Error() != tc.message {
				t.Fatalf("error = %v, want %q", err, tc.message)
			}
		})
	}
}

func TestBindUpdatePlanValidationMessages(t *testing.T) {
	for _, body := range []string{`{"maximum":0}`, `{"maximum":-1}`, `{}`} {
		if err := bindUpdateError(t, body); err == nil || err.Error() != "最大号源必须大于 0" {
			t.Errorf("body=%s error=%v, want 最大号源必须大于 0", body, err)
		}
	}
	// 超过 32767 使用独立文案。
	if err := bindUpdateError(t, `{"maximum":32768}`); err == nil || err.Error() != "最大号源不能超过 32767" {
		t.Errorf("body=32768 error=%v, want 最大号源不能超过 32767", err)
	}
	// 未知字段不允许。
	if err := bindUpdateError(t, `{"maximum":60,"doctorId":9}`); err == nil {
		t.Errorf("未知字段应报错")
	}
}

// TestBindPlanListValidationMessages 覆盖列表查询参数校验错误文案。
func TestBindPlanListValidationMessages(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		message string
	}{
		{"doctorId 非数字", "doctorId=abc", "doctorId必须为正整数"},
		{"doctorId 为 0", "doctorId=0", "doctorId必须为正整数"},
		{"departmentId 为 0", "departmentId=0", "departmentId必须为正整数"},
		{"subdepartmentId 负数", "subdepartmentId=-2", "subdepartmentId必须为正整数"},
		{"fromDate 格式错误", "fromDate=2026/09/20", "fromDate格式必须为 YYYY-MM-DD"},
		{"toDate 非法日期", "toDate=2026-02-30", "toDate格式必须为 YYYY-MM-DD"},
		{"includeSlots 非布尔", "includeSlots=yes", "includeSlots必须为布尔值"},
		{"page 为 0", "page=0", "page必须为正整数"},
		{"page 超上限", "page=100001", "page不能超过100000"},
		{"pageSize 超上限", "pageSize=101", "pageSize必须在1到100之间"},
		{"sort 不在白名单", "sort=name", "sort只支持date/doctorId/id"},
		{"order 非法", "order=drop", "order只支持asc/desc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BindPlanListErr(tc.query)
			if err == nil || err.Error() != tc.message {
				t.Fatalf("error = %v, want %q", err, tc.message)
			}
		})
	}
}

// TestOptionalIdempotencyKey 覆盖 PATCH/DELETE 可选幂等键语义：缺省 present=false 直接执行；
// 提供键时按与创建一致的规则校验，非法键仍报错。
func TestOptionalIdempotencyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var (
		key     string
		present bool
		bindErr error
	)
	engine.GET("/check", func(c *gin.Context) { key, present, bindErr = OptionalIdempotencyKey(c) })

	req := httptest.NewRequest(http.MethodGet, "/check", nil)
	engine.ServeHTTP(httptest.NewRecorder(), req)
	if bindErr != nil || present || key != "" {
		t.Errorf("缺省应为 present=false: key=%q present=%v err=%v", key, present, bindErr)
	}

	req = httptest.NewRequest(http.MethodGet, "/check", nil)
	req.Header.Set("Idempotency-Key", "patch-key-1")
	engine.ServeHTTP(httptest.NewRecorder(), req)
	if bindErr != nil || !present || key != "patch-key-1" {
		t.Errorf("提供键应解析并放行: key=%q present=%v err=%v", key, present, bindErr)
	}

	req = httptest.NewRequest(http.MethodGet, "/check", nil)
	req.Header.Set("Idempotency-Key", "bad\nkey")
	engine.ServeHTTP(httptest.NewRecorder(), req)
	if bindErr == nil {
		t.Errorf("含控制字符的键应报错")
	}
}
