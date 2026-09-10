package request

import (
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/domain/schedule"
)

// 本文件绑定匿名公开查询域（/api/v1/public/*）的查询参数。
// 该域是只读匿名域，参数非法一律返回 422 REQUEST_VALIDATION_FAILED（契约 §1.4、§10），
// 由 handler 输出统一错误 envelope；这里只负责把参数解析成结构化模型并给出中文文案。

// 统一分页约定（契约 §1.4）：page 从 1 开始、上限 100000，pageSize 默认 20、上限 100。
const (
	publicDefaultPage     = 1
	publicDefaultPageSize = 20
	publicMaxPage         = 100000
	publicMaxPageSize     = 100
)

// publicPageQuery 是公开域分页接口共用的分页参数。
type publicPageQuery struct {
	Page     int
	PageSize int
}

// bindPublicPage 解析 page/pageSize；缺省取默认值，非法值返回 error。
func bindPublicPage(c *gin.Context) (publicPageQuery, error) {
	q := publicPageQuery{Page: publicDefaultPage, PageSize: publicDefaultPageSize}
	if raw, exists := c.GetQuery("page"); exists {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return q, errors.New("page必须为正整数")
		}
		if v > publicMaxPage {
			return q, errors.New("page不能超过100000")
		}
		q.Page = v
	}
	if raw, exists := c.GetQuery("pageSize"); exists {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > publicMaxPageSize {
			return q, errors.New("pageSize必须在1到100之间")
		}
		q.PageSize = v
	}
	return q, nil
}

// bindPublicOrder 解析统一排序参数 sort/order：只接受白名单值，禁止把用户输入拼进 SQL。
func bindPublicOrder(c *gin.Context, sortWhitelist []string, defaultSort, defaultOrder string) (string, string, error) {
	sort, order := defaultSort, defaultOrder
	if raw, exists := c.GetQuery("sort"); exists {
		if !containsString(sortWhitelist, raw) {
			return "", "", errors.New("排序字段或排序方向不支持")
		}
		sort = raw
	}
	if raw, exists := c.GetQuery("order"); exists {
		if raw != "asc" && raw != "desc" {
			return "", "", errors.New("排序字段或排序方向不支持")
		}
		order = raw
	}
	return sort, order, nil
}

// containsString 判断白名单是否包含给定值。
func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// PublicDepartmentListQuery 对应 GET /api/v1/public/departments 的查询参数。
type PublicDepartmentListQuery struct {
	Outpatient  *bool
	Recommended *bool
	Page        int
	PageSize    int
	Sort        string
	Order       string
}

// BindPublicDepartments 解析科室列表参数：outpatient、recommended、page、pageSize、sort=name|id、order。
func BindPublicDepartments(c *gin.Context) (PublicDepartmentListQuery, error) {
	q := PublicDepartmentListQuery{}
	page, err := bindPublicPage(c)
	if err != nil {
		return q, err
	}
	q.Page, q.PageSize = page.Page, page.PageSize
	if raw, exists := c.GetQuery("outpatient"); exists {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return q, errors.New("outpatient必须为布尔值")
		}
		q.Outpatient = &v
	}
	if raw, exists := c.GetQuery("recommended"); exists {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return q, errors.New("recommended必须为布尔值")
		}
		q.Recommended = &v
	}
	if q.Sort, q.Order, err = bindPublicOrder(c, []string{"name", "id"}, "name", "asc"); err != nil {
		return q, err
	}
	return q, nil
}

// Filter 转换为目录过滤条件。
func (q PublicDepartmentListQuery) Filter() catalog.DepartmentFilter {
	return catalog.DepartmentFilter{
		Outpatient:  q.Outpatient,
		Recommended: q.Recommended,
		Sort:        q.Sort,
		Order:       q.Order,
	}
}

// PublicSubdepartmentListQuery 对应 GET /api/v1/public/departments/{departmentId}/subdepartments。
type PublicSubdepartmentListQuery struct {
	DepartmentID int64
	Page         int
	PageSize     int
}

// BindPublicSubdepartments 解析子科室列表参数；departmentId 由路径参数给出。
func BindPublicSubdepartments(c *gin.Context, departmentID int64) (PublicSubdepartmentListQuery, error) {
	q := PublicSubdepartmentListQuery{DepartmentID: departmentID}
	page, err := bindPublicPage(c)
	if err != nil {
		return q, err
	}
	q.Page, q.PageSize = page.Page, page.PageSize
	return q, nil
}

// PublicDoctorListQuery 对应 GET /api/v1/public/doctors 的查询参数。
type PublicDoctorListQuery struct {
	DepartmentID    *int64
	SubdepartmentID *int64
	Name            *string
	Page            int
	PageSize        int
	Sort            string
	Order           string
}

// BindPublicDoctors 解析医生列表参数：departmentId、subdepartmentId、name（模糊，最多 50 字符）
// 以及统一分页与排序参数（sort=name|hireDate|recommended|id、order=asc|desc）。
func BindPublicDoctors(c *gin.Context) (PublicDoctorListQuery, error) {
	q := PublicDoctorListQuery{}
	page, err := bindPublicPage(c)
	if err != nil {
		return q, err
	}
	q.Page, q.PageSize = page.Page, page.PageSize
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
	if raw, exists := c.GetQuery("name"); exists {
		if len([]rune(raw)) > 50 {
			return q, errors.New("name最多50个字符")
		}
		if raw != "" {
			q.Name = &raw
		}
	}
	if q.Sort, q.Order, err = bindPublicOrder(c, []string{"name", "hireDate", "recommended", "id"}, "id", "asc"); err != nil {
		return q, err
	}
	return q, nil
}

// Filter 转换为公开医生过滤条件。
func (q PublicDoctorListQuery) Filter() catalog.PublicDoctorFilter {
	return catalog.PublicDoctorFilter{
		DepartmentID:    q.DepartmentID,
		SubdepartmentID: q.SubdepartmentID,
		Name:            q.Name,
		Sort:            q.Sort,
		Order:           q.Order,
	}
}

// PublicScheduleListQuery 对应 GET /api/v1/public/schedules 的查询参数。
type PublicScheduleListQuery struct {
	SubdepartmentID *int64
	DoctorID        *int64
	FromDate        string
	ToDate          string
	Page            int
	PageSize        int
}

// BindPublicSchedules 解析可挂号时段查询参数。
// 日期只做格式校验（YYYY-MM-DD）；「至少一个过滤条件」「窗口顺序与跨度」由 use case 判定，
// 以便缺省窗口按业务时区的当天推导（契约 §8.1）。
func BindPublicSchedules(c *gin.Context) (PublicScheduleListQuery, error) {
	q := PublicScheduleListQuery{}
	page, err := bindPublicPage(c)
	if err != nil {
		return q, err
	}
	q.Page, q.PageSize = page.Page, page.PageSize
	if raw, exists := c.GetQuery("subdepartmentId"); exists {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 1 {
			return q, errors.New("subdepartmentId必须为正整数")
		}
		q.SubdepartmentID = &v
	}
	if raw, exists := c.GetQuery("doctorId"); exists {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 1 {
			return q, errors.New("doctorId必须为正整数")
		}
		q.DoctorID = &v
	}
	if q.FromDate, err = publicDateParam(c, "fromDate"); err != nil {
		return q, err
	}
	if q.ToDate, err = publicDateParam(c, "toDate"); err != nil {
		return q, err
	}
	return q, nil
}

// publicDateParam 读取可选的 YYYY-MM-DD 日期参数，缺省为空字符串。
func publicDateParam(c *gin.Context, name string) (string, error) {
	raw := c.Query(name)
	if raw == "" {
		return "", nil
	}
	if _, err := time.Parse(schedule.DateLayout, raw); err != nil {
		return "", errors.New(name + "格式必须为YYYY-MM-DD")
	}
	return raw, nil
}
