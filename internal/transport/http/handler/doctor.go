package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/transport/http/request"
	doctorservice "Medical-Web-Backend/internal/usecase/doctor"
)

// DoctorHandler exposes doctor search endpoints.
type DoctorHandler struct {
	service *doctorservice.Service
}

func NewDoctorHandler(service *doctorservice.Service) *DoctorHandler {
	return &DoctorHandler{service: service}
}

func (h *DoctorHandler) Search(c *gin.Context) {
	req, err := request.BindDoctorSearch(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "医生查询参数不正确"})
		return
	}
	if err := req.ValidateList(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	result, err := h.service.Search(c.Request.Context(), req.Filters(), *req.Page, *req.Length)
	if err != nil {
		h.respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *DoctorHandler) SearchCount(c *gin.Context) {
	req, err := request.BindDoctorSearch(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "医生查询参数不正确"})
		return
	}
	if err := req.ValidateCount(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	count, err := h.service.Count(c.Request.Context(), req.Filters())
	if err != nil {
		h.respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, count)
}

func (h *DoctorHandler) respondError(c *gin.Context, err error) {
	if errors.Is(err, doctorservice.ErrInvalidInput) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "查询医生失败"})
}
