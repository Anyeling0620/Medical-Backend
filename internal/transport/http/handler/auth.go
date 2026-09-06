package handler

import (
	"Medical-Web-Backend/internal/transport/http/request"
	"Medical-Web-Backend/internal/transport/http/response"
	userservice "Medical-Web-Backend/internal/usecase/user"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http"
)

type AuthHandler struct {
	service *userservice.Service
}

func NewAuthHandler(service *userservice.Service) *AuthHandler {
	return &AuthHandler{service: service}
}

func (h *AuthHandler) Login(c *gin.Context) {
	var req request.LoginRequest
	// 尝试绑定返回体
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username and password are required"})
		return
	}
	// 获取用户 权限 错误
	u, err := h.service.Authenticate(c.Request.Context(), req.Username, req.Password)
	// 前往校验时出错
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, userservice.ErrInvalidInput) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, response.LoginResponse{UserID: u.ID})
}
