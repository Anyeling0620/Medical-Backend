package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/transport/http/request"
	"Medical-Web-Backend/internal/transport/http/response"
	publiccatalogservice "Medical-Web-Backend/internal/usecase/publiccatalog"
)

// PublicCatalogHandler 提供匿名公开查询域（/api/v1/public/*）的只读接口：
// 科室、子科室、医生与可挂号时段。
//
// 该域不读取令牌：携带无效令牌或跨域令牌都不影响响应内容（测试策略「匿名公开域」），
// 因此路由不挂访问令牌与权限中间件；业务规则与参数校验都在 use case 层完成。
type PublicCatalogHandler struct {
	service *publiccatalogservice.Service
	// photoBaseURL 为 MinIO 公开访问前缀，用于把数据库里的对象名补全成可访问地址。
	photoBaseURL string
}

// NewPublicCatalogHandler 构造公开域处理器。
func NewPublicCatalogHandler(service *publiccatalogservice.Service, minioURL string) *PublicCatalogHandler {
	return &PublicCatalogHandler{service: service, photoBaseURL: strings.TrimRight(minioURL, "/")}
}

// ListDepartments 处理 GET /api/v1/public/departments。
func (h *PublicCatalogHandler) ListDepartments(c *gin.Context) {
	query, err := request.BindPublicDepartments(c)
	if err != nil {
		h.validation(c, err)
		return
	}
	items, total, err := h.service.ListDepartments(c.Request.Context(), query.Filter(), query.Page, query.PageSize)
	if err != nil {
		h.serviceError(c, err)
		return
	}
	body := response.Page[response.PublicDepartment]{
		Items:    make([]response.PublicDepartment, 0, len(items)),
		Page:     query.Page,
		PageSize: query.PageSize,
		Total:    total,
	}
	for _, item := range items {
		body.Items = append(body.Items, response.NewPublicDepartment(item))
	}
	c.JSON(http.StatusOK, body)
}

// DepartmentDetail 处理 GET /api/v1/public/departments/{departmentId}。
func (h *PublicCatalogHandler) DepartmentDetail(c *gin.Context) {
	id, err := request.ParsePositiveID(c.Param("departmentId"))
	if err != nil {
		h.validation(c, errors.New("科室编号必须为正整数"))
		return
	}
	item, err := h.service.Department(c.Request.Context(), id)
	if err != nil {
		h.serviceError(c, err)
		return
	}
	c.JSON(http.StatusOK, response.NewPublicDepartment(*item))
}

// Subdepartments 处理 GET /api/v1/public/departments/{departmentId}/subdepartments。
func (h *PublicCatalogHandler) Subdepartments(c *gin.Context) {
	id, err := request.ParsePositiveID(c.Param("departmentId"))
	if err != nil {
		h.validation(c, errors.New("科室编号必须为正整数"))
		return
	}
	query, err := request.BindPublicSubdepartments(c, id)
	if err != nil {
		h.validation(c, err)
		return
	}
	items, total, err := h.service.Subdepartments(c.Request.Context(), query.DepartmentID, query.Page, query.PageSize)
	if err != nil {
		h.serviceError(c, err)
		return
	}
	body := response.Page[response.PublicSubdepartment]{
		Items:    make([]response.PublicSubdepartment, 0, len(items)),
		Page:     query.Page,
		PageSize: query.PageSize,
		Total:    total,
	}
	for _, item := range items {
		body.Items = append(body.Items, response.NewPublicSubdepartment(item))
	}
	c.JSON(http.StatusOK, body)
}

// Doctors 处理 GET /api/v1/public/doctors。
func (h *PublicCatalogHandler) Doctors(c *gin.Context) {
	query, err := request.BindPublicDoctors(c)
	if err != nil {
		h.validation(c, err)
		return
	}
	items, total, err := h.service.Doctors(c.Request.Context(), query.Filter(), query.Page, query.PageSize)
	if err != nil {
		h.serviceError(c, err)
		return
	}
	body := response.Page[response.PublicDoctor]{
		Items:    make([]response.PublicDoctor, 0, len(items)),
		Page:     query.Page,
		PageSize: query.PageSize,
		Total:    total,
	}
	for _, item := range items {
		body.Items = append(body.Items, response.NewPublicDoctor(item, h.photoBaseURL))
	}
	c.JSON(http.StatusOK, body)
}

// DoctorDetail 处理 GET /api/v1/public/doctors/{doctorId}。
func (h *PublicCatalogHandler) DoctorDetail(c *gin.Context) {
	id, err := request.ParsePositiveID(c.Param("doctorId"))
	if err != nil {
		h.validation(c, errors.New("医生编号必须为正整数"))
		return
	}
	item, err := h.service.Doctor(c.Request.Context(), id)
	if err != nil {
		h.serviceError(c, err)
		return
	}
	c.JSON(http.StatusOK, response.NewPublicDoctorDetail(*item, h.photoBaseURL))
}

// Schedules 处理 GET /api/v1/public/schedules（契约 §8.1）。
func (h *PublicCatalogHandler) Schedules(c *gin.Context) {
	query, err := request.BindPublicSchedules(c)
	if err != nil {
		h.validation(c, err)
		return
	}
	items, total, err := h.service.Schedules(c.Request.Context(), publiccatalogservice.ScheduleQuery{
		SubdepartmentID: query.SubdepartmentID,
		DoctorID:        query.DoctorID,
		FromDate:        query.FromDate,
		ToDate:          query.ToDate,
		Page:            query.Page,
		PageSize:        query.PageSize,
	})
	if err != nil {
		h.serviceError(c, err)
		return
	}
	body := response.Page[response.PublicSchedule]{
		Items:    make([]response.PublicSchedule, 0, len(items)),
		Page:     query.Page,
		PageSize: query.PageSize,
		Total:    total,
	}
	for _, item := range items {
		body.Items = append(body.Items, response.NewPublicSchedule(item, h.photoBaseURL))
	}
	c.JSON(http.StatusOK, body)
}

// validation 输出 422 参数错误（契约 §1.3 统一错误 envelope）。
func (h *PublicCatalogHandler) validation(c *gin.Context, err error) {
	c.JSON(http.StatusUnprocessableEntity, gin.H{
		"code":    publiccatalogservice.CodeValidationFailed,
		"message": err.Error(),
	})
}

// serviceError 把 use case 错误映射为 HTTP 响应；未知错误一律 500，不泄漏内部细节。
func (h *PublicCatalogHandler) serviceError(c *gin.Context, err error) {
	var serviceErr *publiccatalogservice.ServiceError
	if errors.As(err, &serviceErr) {
		body := gin.H{"code": serviceErr.Code, "message": serviceErr.Message}
		if len(serviceErr.Details) > 0 {
			body["details"] = serviceErr.Details
		}
		c.JSON(publicErrorStatus(serviceErr.Code), body)
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL_SERVER_ERROR", "message": "查询失败"})
}

// publicErrorStatus 把公开域业务错误码映射为 HTTP 状态码。
func publicErrorStatus(code string) int {
	switch code {
	case publiccatalogservice.CodeValidationFailed:
		return http.StatusUnprocessableEntity
	case publiccatalogservice.CodeDepartmentNotFound,
		publiccatalogservice.CodeSubdepartmentNotFound,
		publiccatalogservice.CodeDoctorNotFound:
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
