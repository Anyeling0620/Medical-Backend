package handler

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/transport/http/request"
	doctorservice "Medical-Web-Backend/internal/usecase/doctor"
)

// DoctorHandler exposes doctor search endpoints.
type DoctorHandler struct {
	service  *doctorservice.Service
	minioURL string
}

func NewDoctorHandler(service *doctorservice.Service) *DoctorHandler {
	return &DoctorHandler{
		service: service,
	}
}

// NewDoctorHandlerWithMinIO creates a doctor handler that resolves the
// relative photo path stored in the database to a MinIO URL.
func NewDoctorHandlerWithMinIO(
	service *doctorservice.Service,
	minioURL string,
) *DoctorHandler {
	return &DoctorHandler{
		service:  service,
		minioURL: strings.TrimRight(minioURL, "/"),
	}
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

	result, err := h.service.Search(
		c.Request.Context(),
		req.Filters(),
		*req.Page,
		*req.Length,
	)
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

	count, err := h.service.Count(
		c.Request.Context(),
		req.Filters(),
	)
	if err != nil {
		h.respondError(c, err)
		return
	}

	c.JSON(http.StatusOK, count)
}

func (h *DoctorHandler) ListDepts(c *gin.Context) {
	result, err := h.service.ListDepts(c.Request.Context())
	if err != nil {
		h.respondError(c, err)
		return
	}

	c.JSON(http.StatusOK, result)
}

func (h *DoctorHandler) ListDegrees(c *gin.Context) {
	result, err := h.service.ListDegrees(c.Request.Context())
	if err != nil {
		h.respondError(c, err)
		return
	}

	c.JSON(http.StatusOK, result)
}

func (h *DoctorHandler) ListJobs(c *gin.Context) {
	result, err := h.service.ListJobs(c.Request.Context())
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

	detail, err := h.service.FindByID(
		c.Request.Context(),
		id,
	)
	if err != nil {
		h.respondError(c, err)
		return
	}

	// The database stores a relative path. Convert it to a MinIO URL only
	// when returning the doctor detail response.
	detail.Photo = h.photoURL(detail.Photo)

	c.JSON(http.StatusOK, detail)
}

func (h *DoctorHandler) photoURL(photo string) string {
	if photo == "" || h.minioURL == "" {
		return photo
	}

	photoPath := strings.TrimLeft(photo, "/")

	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err == nil {
		randomValue := binary.BigEndian.Uint64(randomBytes[:])

		return h.minioURL +
			"/" +
			photoPath +
			"?random=" +
			strconv.FormatUint(randomValue, 10)
	}

	// Extremely unlikely fallback when the system random source is unavailable.
	return h.minioURL +
		"/" +
		photoPath +
		"?random=" +
		strconv.FormatInt(time.Now().UnixNano(), 10)
}

func (h *DoctorHandler) respondError(
	c *gin.Context,
	err error,
) {
	if errors.Is(err, doctorservice.ErrInvalidInput) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	if errors.Is(err, doctorservice.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusInternalServerError, gin.H{
		"error": "查询医生失败",
	})
}
