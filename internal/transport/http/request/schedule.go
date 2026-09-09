package request

import (
	"encoding/json"
	"errors"
	"io"
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
	if err := decodeStrict(c, &body); err != nil {
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
	if err := decodeStrict(c, &body); err != nil {
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

// CreateSlotRequest 对应 POST /api/v1/schedule/plans/{planId}/slots 的请求体：{slot, maximum}。
// 使用 int 承载，便于在绑定层输出契约要求的中文范围文案（时段编号与容量均为小整数）。
type CreateSlotRequest struct {
	Slot    int `json:"slot"`
	Maximum int `json:"maximum"`
}

// UpdateSlotRequest 对应 PATCH /api/v1/schedule/slots/{slotId} 的请求体，只允许修改 maximum。
type UpdateSlotRequest struct {
	Maximum int `json:"maximum"`
}

// BindCreateSlotRequest 严格解析创建时段请求体：拒绝未知字段与格式错误；
// 时段编号与最大号源的范围校验输出契约样例的中文文案。
func BindCreateSlotRequest(c *gin.Context) (CreateSlotRequest, error) {
	var req CreateSlotRequest
	if err := decodeStrict(c, &req); err != nil {
		return req, err
	}
	if req.Slot < 1 {
		return req, errors.New("时段编号必须为正整数")
	}
	if req.Slot > 32767 {
		return req, errors.New("时段编号不能超过 32767")
	}
	if req.Maximum < 1 {
		return req, errors.New("时段最大号源必须大于 0")
	}
	if req.Maximum > 32767 {
		return req, errors.New("时段最大号源不能超过 32767")
	}
	return req, nil
}

// BindUpdateSlotRequest 严格解析更新时段请求体：只允许修改 maximum（1-32767）。
func BindUpdateSlotRequest(c *gin.Context) (UpdateSlotRequest, error) {
	var req UpdateSlotRequest
	if err := decodeStrict(c, &req); err != nil {
		return req, err
	}
	if req.Maximum < 1 {
		return req, errors.New("时段最大号源必须大于 0")
	}
	if req.Maximum > 32767 {
		return req, errors.New("时段最大号源不能超过 32767")
	}
	return req, nil
}

// decodeStrict 使用 DisallowUnknownFields 解析 JSON，并在空 body / 多余 JSON 时报错；
// 底层解码细节被归一为面向用户的中文文案，避免把内部错误直接返回给调用方。
func decodeStrict(c *gin.Context, target any) error {
	if c.Request.Body == nil {
		return errors.New("请求体不能为空")
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("请求体不能为空")
		}
		// 保留 json.SyntaxError/json.UnmarshalTypeError/io.ErrUnexpectedEOF
		// 的原始类型，供 handler 映射 400 REQUEST_INVALID_JSON（与 auth 处理约定一致）；
		// 其余（未知字段等）在这里归一为面向用户的稳定文案（422）。
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) || errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		return errors.New("请求体格式不正确（包含未知字段）")
	}
	// 防止“合法 JSON 后跟多余内容或非 JSON 垃圾”被静默忽略。
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("请求体包含多余内容")
	} else if !errors.Is(err, io.EOF) {
		return errors.New("请求体包含多余内容")
	}
	return nil
}
