package request

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/schedule"
)

// ScheduleDateLayout 排班接口统一的日期格式（YYYY-MM-DD）。
const ScheduleDateLayout = "2006-01-02"

// PlanListQuery 是 GET /schedule/plans 的查询参数模型。
// 除分页与排序外全部可选；指针字段用于区分“未传”与“非法值”。
type PlanListQuery struct {
	DoctorID        *int64
	DepartmentID    *int64
	SubdepartmentID *int64
	FromDate        string
	ToDate          string
	IncludeSlots    bool
	Page            int
	PageSize        int
	Sort            string
	Order           string
}

// BindPlanListErr 以真实 HTTP 请求驱动 BindPlanList 校验，仅供测试复用。
func BindPlanListErr(rawQuery string) (PlanListQuery, error) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var result PlanListQuery
	var bindErr error
	engine.GET("/api/v1/schedule/plans", func(c *gin.Context) {
		result, bindErr = BindPlanList(c)
	})
	target := "/api/v1/schedule/plans"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	return result, bindErr
}

// BindPlanList 解析并校验排班列表查询参数。默认值：page=1、pageSize=20、sort=id、order=asc、
// includeSlots=false；除分页与排序外全部可选。非法值返回 error，handler 统一转 422。
func BindPlanList(c *gin.Context) (PlanListQuery, error) {
	q := PlanListQuery{Page: 1, PageSize: 20, Sort: "id", Order: "asc"}
	if raw, exists := c.GetQuery("doctorId"); exists {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 1 {
			return q, errors.New("doctorId必须为正整数")
		}
		q.DoctorID = &v
	}
	if raw, exists := c.GetQuery("departmentId"); exists {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 1 {
			return q, errors.New("departmentId必须为正整数")
		}
		q.DepartmentID = &v
	}
	if raw, exists := c.GetQuery("subdepartmentId"); exists {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 1 {
			return q, errors.New("subdepartmentId必须为正整数")
		}
		q.SubdepartmentID = &v
	}
	var err error
	if q.FromDate, err = optionalDateParam(c, "fromDate"); err != nil {
		return q, err
	}
	if q.ToDate, err = optionalDateParam(c, "toDate"); err != nil {
		return q, err
	}
	if raw, exists := c.GetQuery("includeSlots"); exists {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return q, errors.New("includeSlots必须为布尔值")
		}
		q.IncludeSlots = v
	}
	if raw, exists := c.GetQuery("page"); exists {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return q, errors.New("page必须为正整数")
		}
		if v > 100000 {
			return q, errors.New("page不能超过100000")
		}
		q.Page = v
	}
	if raw, exists := c.GetQuery("pageSize"); exists {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 100 {
			return q, errors.New("pageSize必须在1到100之间")
		}
		q.PageSize = v
	}
	if raw, exists := c.GetQuery("sort"); exists {
		if raw != "date" && raw != "doctorId" && raw != "id" {
			return q, errors.New("sort只支持date/doctorId/id")
		}
		q.Sort = raw
	}
	if raw, exists := c.GetQuery("order"); exists {
		if raw != "asc" && raw != "desc" {
			return q, errors.New("order只支持asc/desc")
		}
		q.Order = raw
	}
	return q, nil
}

// ToFilter 转换为领域层过滤条件。
func (q PlanListQuery) ToFilter() schedule.PlanFilter {
	return schedule.PlanFilter{
		DoctorID:        q.DoctorID,
		DepartmentID:    q.DepartmentID,
		SubdepartmentID: q.SubdepartmentID,
		FromDate:        q.FromDate,
		ToDate:          q.ToDate,
		IncludeSlots:    q.IncludeSlots,
		Sort:            q.Sort,
		Order:           q.Order,
	}
}

// CreatePlanBody 是 POST /schedule/plans 的请求体。
type CreatePlanBody struct {
	DoctorID        int64  `json:"doctorId"`
	SubdepartmentID int64  `json:"subdepartmentId"`
	Date            string `json:"date"`
	Maximum         int    `json:"maximum"`
}

// BindCreatePlan 绑定并校验创建计划请求体：id 必须为正整数、date 为 YYYY-MM-DD、
// maximum 为 1..32767。
func BindCreatePlan(c *gin.Context) (CreatePlanBody, error) {
	var body CreatePlanBody
	if err := decodeJSONBody(c, &body); err != nil {
		return body, err
	}
	if body.DoctorID < 1 {
		return body, errors.New("医生编号必须为正整数")
	}
	if body.SubdepartmentID < 1 {
		return body, errors.New("子科室编号必须为正整数")
	}
	if _, err := time.Parse(ScheduleDateLayout, body.Date); err != nil {
		return body, errors.New("日期格式必须为 YYYY-MM-DD")
	}
	if body.Maximum < 1 {
		return body, errors.New("最大号源必须大于 0")
	}
	if body.Maximum > 32767 {
		return body, errors.New("最大号源不能超过 32767")
	}
	return body, nil
}

// UpdatePlanBody 是 PATCH /schedule/plans/{planId} 的请求体：只允许 maximum。
type UpdatePlanBody struct {
	Maximum int `json:"maximum"`
}

// BindUpdatePlanMaximum 绑定更新请求体：仅允许 maximum 字段；未知字段返回 422。
// maximum 为 1..32767（服务层还会结合 used 校验并发约束）。
func BindUpdatePlanMaximum(c *gin.Context) (UpdatePlanBody, error) {
	var body UpdatePlanBody
	if err := decodeJSONBody(c, &body); err != nil {
		return body, err
	}
	if body.Maximum < 1 {
		return body, errors.New("最大号源必须大于 0")
	}
	if body.Maximum > 32767 {
		return body, errors.New("最大号源不能超过 32767")
	}
	return body, nil
}

// BindIdempotencyKey 校验 Idempotency-Key：1-128 个可打印 ASCII 字符，缺失即 422。
func BindIdempotencyKey(c *gin.Context) (string, error) {
	raw := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if raw == "" {
		return "", errors.New("Idempotency-Key 请求头必填")
	}
	if len(raw) > 128 {
		return "", errors.New("Idempotency-Key 长度必须在 1 到 128 之间")
	}
	for _, b := range []byte(raw) {
		if b < 0x20 || b > 0x7E {
			return "", errors.New("Idempotency-Key 只能包含可打印 ASCII 字符")
		}
	}
	return raw, nil
}

// OptionalIdempotencyKey 兼容“可选幂等键”的写接口（PATCH/DELETE）：
// 请求头缺失时返回 present=false（不强制）；存在时按必填键相同的规则校验，非法值仍 422。
func OptionalIdempotencyKey(c *gin.Context) (key string, present bool, err error) {
	if strings.TrimSpace(c.GetHeader("Idempotency-Key")) == "" {
		return "", false, nil
	}
	key, err = BindIdempotencyKey(c)
	return key, true, err
}

// optionalDateParam 读取可选的 YYYY-MM-DD 日期参数，缺省为空字符串。
func optionalDateParam(c *gin.Context, name string) (string, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return "", nil
	}
	if _, err := time.Parse(ScheduleDateLayout, raw); err != nil {
		return "", errors.New(name + "格式必须为 YYYY-MM-DD")
	}
	return raw, nil
}

// decodeJSONBody 解析请求体：字段类型错误或未知字段由调用方统一转 422 参数错误。
func decodeJSONBody(c *gin.Context, dst any) error {
	if c.Request.Body == nil {
		return errors.New("请求体不能为空")
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}
