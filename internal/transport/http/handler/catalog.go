package handler

import (
	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/transport/http/request"
	misuser "Medical-Web-Backend/internal/usecase/misuser"
	"database/sql"
	"errors"
	"github.com/gin-gonic/gin"
	"strings"
)

type CatalogHandler struct {
	repository port.CatalogRepository
	users      port.UserRepository
	minioURL   string
}

func NewCatalogHandler(r port.CatalogRepository, users port.UserRepository, minioURL string) *CatalogHandler {
	return &CatalogHandler{repository: r, users: users, minioURL: strings.TrimRight(minioURL, "/")}
}

func (h *CatalogHandler) DoctorDetail(c *gin.Context) {
	id, err := request.ParsePositiveID(c.Param("doctorId"))
	if err != nil {
		h.validation(c, errors.New("医生编号必须为正整数"))
		return
	}
	item, err := h.repository.FindDoctor(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(404, gin.H{"code": "CATALOG_DOCTOR_NOT_FOUND", "message": "医生不存在"})
			return
		}
		h.internal(c)
		return
	}
	if item == nil {
		c.JSON(404, gin.H{"code": "CATALOG_DOCTOR_NOT_FOUND", "message": "医生不存在"})
		return
	}
	if item.Status == "HIDDEN" && !h.isRoot(c) {
		c.JSON(404, gin.H{"code": "CATALOG_DOCTOR_NOT_FOUND", "message": "医生不存在"})
		return
	}
	item.PhotoURL = h.photoURL(item.PhotoURL)
	c.JSON(200, item)
}

func (h *CatalogHandler) DoctorOptions(c *gin.Context) {
	if c.Request.URL.Query().Get("page") != "" || c.Request.URL.Query().Get("pageSize") != "" {
		h.validation(c, errors.New("不支持分页参数"))
		return
	}
	item, err := h.repository.ListDoctorOptions(c.Request.Context())
	if err != nil {
		h.internal(c)
		return
	}
	c.JSON(200, item)
}

func (h *CatalogHandler) DoctorPrices(c *gin.Context) {
	r, err := request.BindDoctorPrices(c)
	if err != nil {
		h.validation(c, err)
		return
	}
	offset := (*r.Page - 1) * (*r.PageSize)
	items, total, err := h.repository.ListDoctorPrices(c.Request.Context(), *r.DoctorID, offset, *r.PageSize)
	if err != nil {
		h.internal(c)
		return
	}
	c.JSON(200, catalog.Page[catalog.DoctorPrice]{Items: items, Page: *r.Page, PageSize: *r.PageSize, Total: total})
}
func (h *CatalogHandler) ListDepartments(c *gin.Context) {
	r, err := request.BindDepartmentList(c)
	if err != nil || r.Validate() != nil {
		c.JSON(422, gin.H{"code": "REQUEST_VALIDATION_FAILED", "message": "查询参数不正确"})
		return
	}
	p, ps, sort, order := r.Values()
	items, total, err := h.repository.ListDepartments(c.Request.Context(), catalog.DepartmentFilter{Outpatient: r.Outpatient, Recommended: r.Recommended, Sort: sort, Order: order}, (p-1)*ps, ps)
	if err != nil {
		h.internal(c)
		return
	}
	c.JSON(200, catalog.Page[catalog.Department]{Items: items, Page: p, PageSize: ps, Total: total})
}
func (h *CatalogHandler) Detail(c *gin.Context) {
	id, err := request.ParsePositiveID(c.Param("departmentId"))
	if err != nil {
		h.validation(c, err)
		return
	}
	item, err := h.repository.FindDepartment(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || item == nil {
		h.notFound(c)
		return
	}
	if err != nil {
		h.internal(c)
		return
	}
	c.JSON(200, item)
}
func (h *CatalogHandler) Subdepartments(c *gin.Context) {
	id, err := request.ParsePositiveID(c.Param("departmentId"))
	if err != nil {
		h.validation(c, err)
		return
	}
	p, ps := 1, 20
	if v, e := request.ParsePositiveID(c.DefaultQuery("page", "1")); e == nil {
		p = int(v)
	} else {
		h.validation(c, e)
		return
	}
	if v, e := request.ParsePositiveID(c.DefaultQuery("pageSize", "20")); e == nil && v <= 100 {
		ps = int(v)
	} else {
		h.validation(c, errors.New("pageSize必须在1到100之间"))
		return
	}
	items, total, err := h.repository.ListSubdepartments(c.Request.Context(), id, (p-1)*ps, ps)
	if errors.Is(err, sql.ErrNoRows) {
		h.notFound(c)
		return
	}
	if err != nil {
		h.internal(c)
		return
	}
	c.JSON(200, catalog.Page[catalog.Subdepartment]{Items: items, Page: p, PageSize: ps, Total: total})
}
func (h *CatalogHandler) validation(c *gin.Context, e error) {
	c.JSON(422, gin.H{"code": "REQUEST_VALIDATION_FAILED", "message": e.Error()})
}
func (h *CatalogHandler) notFound(c *gin.Context) {
	c.JSON(404, gin.H{"code": "CATALOG_DEPARTMENT_NOT_FOUND", "message": "科室不存在"})
}
func (h *CatalogHandler) internal(c *gin.Context) {
	c.JSON(500, gin.H{"code": "INTERNAL_SERVER_ERROR", "message": "查询失败"})
}

func (h *CatalogHandler) photoURL(photo string) string {
	if photo == "" || h.minioURL == "" {
		return photo
	}
	return h.minioURL + "/" + strings.TrimLeft(photo, "/")
}

func (h *CatalogHandler) isRoot(c *gin.Context) bool {
	value, exists := c.Get(middleware.ClaimsKey)
	claims, ok := value.(*misuser.AccessClaims)
	if !exists || !ok || claims == nil || h.users == nil {
		return false
	}
	permissions, err := h.users.Permissions(c.Request.Context(), claims.UserID)
	if err != nil {
		return false
	}
	for _, permission := range permissions {
		if permission == "ROOT" {
			return true
		}
	}
	return false
}
