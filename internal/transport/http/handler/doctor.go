package handler

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/request"
)

// DoctorHandler exposes doctor search endpoints.
type DoctorHandler struct {
	repository port.DoctorRepository
	minioURL   string
}

func NewDoctorHandler(repository port.DoctorRepository, minioURL string) *DoctorHandler {
	return &DoctorHandler{repository: repository, minioURL: strings.TrimRight(minioURL, "/")}
}

func (h *DoctorHandler) Search(c *gin.Context) {
	req, err := request.BindDoctorSearch(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "医生查询参数不正确",
		})
		return
	}

	if err := req.ValidateList(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	if *req.Page-1 > int(^uint(0)>>1) / *req.Length {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page内容不正确"})
		return
	}

	offset := (*req.Page - 1) * *req.Length
	result, err := h.repository.Search(c.Request.Context(), req.Filters(), offset, *req.Length)
	if err != nil {
		h.respondError(c, err)
		return
	}

	c.JSON(http.StatusOK, result)
}

func (h *DoctorHandler) SearchCount(c *gin.Context) {
	req, err := request.BindDoctorSearch(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "医生查询参数不正确",
		})
		return
	}

	if err := req.ValidateCount(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	count, err := h.repository.Count(c.Request.Context(), req.Filters())
	if err != nil {
		h.respondError(c, err)
		return
	}

	c.JSON(http.StatusOK, count)
}

func (h *DoctorHandler) ListDepts(c *gin.Context) {
	result, err := h.repository.ListDepts(c.Request.Context())
	if err != nil {
		h.respondError(c, err)
		return
	}

	c.JSON(http.StatusOK, result)
}

func (h *DoctorHandler) ListDegrees(c *gin.Context) {
	result, err := h.repository.ListDegrees(c.Request.Context())
	if err != nil {
		h.respondError(c, err)
		return
	}

	c.JSON(http.StatusOK, result)
}

func (h *DoctorHandler) ListJobs(c *gin.Context) {
	result, err := h.repository.ListJobs(c.Request.Context())
	if err != nil {
		h.respondError(c, err)
		return
	}

	c.JSON(http.StatusOK, result)
}

func (h *DoctorHandler) Detail(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "医生ID不正确",
		})
		return
	}

	detail, err := h.repository.FindByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || detail == nil {
		h.respondError(c, errDoctorNotFound)
		return
	}
	if err != nil {
		h.respondError(c, err)
		return
	}
	detail.Photo = h.photoURL(detail.Photo)

	c.JSON(http.StatusOK, detail)
}

func (h *DoctorHandler) photoURL(photo string) string {
	if photo == "" || h.minioURL == "" {
		return photo
	}

	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err == nil {
		return h.minioURL + "/" + strings.TrimLeft(photo, "/") + "?random=" + strconv.FormatUint(binary.BigEndian.Uint64(randomBytes[:]), 10)
	}
	return h.minioURL + "/" + strings.TrimLeft(photo, "/")
}

func (h *DoctorHandler) respondError(
	c *gin.Context,
	err error,
) {
	if errors.Is(err, errDoctorNotFound) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "医生不存在",
		})
		return
	}

	c.JSON(http.StatusInternalServerError, gin.H{
		"error": "查询医生失败",
	})
}

var errDoctorNotFound = errors.New("doctor not found")
