package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/patient"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/transport/http/request"
	"Medical-Web-Backend/internal/transport/http/response"
	"Medical-Web-Backend/internal/usecase/authsession"
	patientauthservice "Medical-Web-Backend/internal/usecase/patientauth"
)

// PatientAuthHandler 处理 /api/v1/patient/auth/* 与 GET /api/v1/patient/me。
// 患者身份固定来自请求携带的凭据，不接受任何 body/query 传入的患者标识。
type PatientAuthHandler struct {
	service *patientauthservice.Service
	secure  bool
}

func NewPatientAuthHandler(
	service *patientauthservice.Service,
	secure bool,
) *PatientAuthHandler {
	return &PatientAuthHandler{
		service: service,
		secure:  secure,
	}
}

// WeChatLogin 处理 POST /api/v1/patient/auth/wechat-login：登录与注册合一。
// 成功响应用 isNewUser 区分本次是否为新注册，refresh token 只经 HttpOnly Cookie 返回。
func (h *PatientAuthHandler) WeChatLogin(c *gin.Context) {
	var req request.WeChatLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.bindError(c, err)
		return
	}

	// 纯空白 code 等同于未提交，与绑定失败的文案保持一致（契约 §12.5）。
	if strings.TrimSpace(req.Code) == "" {
		h.writeError(
			c,
			http.StatusUnprocessableEntity,
			"REQUEST_VALIDATION_FAILED",
			"微信登录 code 不能为空",
		)
		return
	}

	result, err := h.service.WeChatLogin(c.Request.Context(), req.Code)
	if err != nil {
		h.loginError(c, err)
		return
	}

	setTokenCookies(c, result.Tokens, h.secure)

	// cardId 为 null 表示尚未实名建卡，前端据此跳转实名流程。
	var cardID *int64
	if result.Card != nil {
		id := result.Card.ID
		cardID = &id
	}

	c.JSON(http.StatusOK, response.PatientLoginResponse{
		IsNewUser:       result.IsNewUser,
		Patient:         response.NewPatientSummary(result.Patient),
		CardID:          cardID,
		AccessToken:     result.Tokens.AccessToken,
		AccessExpiresAt: result.Tokens.AccessExpiresAt.UTC().Format(time.RFC3339),
	})
}

// Refresh 处理 POST /api/v1/patient/auth/refresh。
//
// refresh token 优先取请求体、其次取 Cookie（契约 §7.2 允许两种来源）：
// 小程序把令牌保存在本地存储，不能依赖 Cookie 通道。
func (h *PatientAuthHandler) Refresh(c *gin.Context) {
	var req request.PatientRefreshRequest
	// 请求体是可选的：只有「空请求体」按缺省处理（继续走 Cookie 通道），
	// 其余解析错误按契约返回 400 REQUEST_INVALID_JSON，不静默降级成 401。
	if err := c.ShouldBindJSON(&req); err != nil {
		if !errors.Is(err, io.EOF) {
			h.bindError(c, err)
			return
		}
		req.RefreshToken = ""
	}

	refreshToken := req.RefreshToken
	if refreshToken == "" {
		refreshToken, _ = c.Cookie(authsession.RefreshCookieName)
	}

	pair, err := h.service.Refresh(c.Request.Context(), refreshToken)
	if err != nil {
		switch {
		case errors.Is(err, patientauthservice.ErrPatientDisabled):
			h.writeError(c, http.StatusForbidden, "AUTH_FORBIDDEN", "账号已被禁用")
		case errors.Is(err, patientauthservice.ErrDependencyUnavailable):
			h.writeError(
				c,
				http.StatusBadGateway,
				"DEPENDENCY_UNAVAILABLE",
				"服务暂时不可用，请稍后重试",
			)
		default:
			h.writeError(
				c,
				http.StatusUnauthorized,
				"AUTH_INVALID_REFRESH_TOKEN",
				"刷新令牌无效或已过期",
			)
		}
		return
	}

	// 轮换后的 refresh token 覆盖 Cookie，access token 同时下发到 Cookie 与响应体。
	setTokenCookies(c, pair, h.secure)

	c.JSON(http.StatusOK, response.PatientRefreshResponse{
		AccessToken:     pair.AccessToken,
		AccessExpiresAt: pair.AccessExpiresAt.UTC().Format(time.RFC3339),
	})
}

// Logout 处理 POST /api/v1/patient/auth/logout，成功固定返回 204。
//
// 严格语义（契约 §7.2 与 §12.5）：必须携带仍可解析的患者 access token，
// 否则 401 AUTH_INVALID_TOKEN（用管理端令牌调用也走这一分支，realm 不匹配）。
// refresh token 用于一并撤销服务端会话，可来自请求体或 Cookie，允许缺省。
func (h *PatientAuthHandler) Logout(c *gin.Context) {
	accessToken := middleware.BearerToken(c.GetHeader("Authorization"))
	if accessToken == "" {
		accessToken, _ = c.Cookie(authsession.AccessCookieName)
	}

	var req request.PatientRefreshRequest
	// 登出的请求体只用于补充 refresh 令牌，畸形请求体不应让一个合法会话无法登出，
	// 因此这里容忍解析失败（凭据由 access token 决定，见下方严格校验）。
	if err := c.ShouldBindJSON(&req); err != nil {
		req.RefreshToken = ""
	}
	refreshToken := req.RefreshToken
	if refreshToken == "" {
		refreshToken, _ = c.Cookie(authsession.RefreshCookieName)
	}

	if err := h.service.Logout(
		c.Request.Context(),
		accessToken,
		refreshToken,
	); err != nil {
		if errors.Is(err, patientauthservice.ErrInvalidAccessToken) {
			h.writeError(
				c,
				http.StatusUnauthorized,
				"AUTH_INVALID_TOKEN",
				"患者访问令牌无效",
			)
			return
		}
		if errors.Is(err, patientauthservice.ErrDependencyUnavailable) {
			h.writeError(
				c,
				http.StatusBadGateway,
				"DEPENDENCY_UNAVAILABLE",
				"服务暂时不可用，请稍后重试",
			)
			return
		}
		h.writeError(
			c,
			http.StatusInternalServerError,
			"INTERNAL_SERVER_ERROR",
			"退出登录失败",
		)
		return
	}

	clearTokenCookies(c, h.secure)
	c.Status(http.StatusNoContent)
}

// Me 处理 GET /api/v1/patient/me：返回当前患者摘要与实名建卡状态。
// 主体取自访问令牌（realm=patient），越权与令牌无效统一按 401 处理。
func (h *PatientAuthHandler) Me(c *gin.Context) {
	claims, ok := middleware.ClaimsFrom(c)
	if !ok {
		h.writeError(
			c,
			http.StatusUnauthorized,
			"AUTH_INVALID_TOKEN",
			"患者访问令牌无效",
		)
		return
	}

	me, err := h.service.Me(c.Request.Context(), claims.UserID)
	if err != nil {
		if errors.Is(err, patient.ErrPatientNotFound) {
			// 令牌有效但账号已不存在：不枚举账号状态，统一按令牌无效处理。
			h.writeError(
				c,
				http.StatusUnauthorized,
				"AUTH_INVALID_TOKEN",
				"患者访问令牌无效",
			)
			return
		}
		if errors.Is(err, patientauthservice.ErrDependencyUnavailable) {
			h.writeError(
				c,
				http.StatusBadGateway,
				"DEPENDENCY_UNAVAILABLE",
				"服务暂时不可用，请稍后重试",
			)
			return
		}
		h.writeError(
			c,
			http.StatusInternalServerError,
			"INTERNAL_SERVER_ERROR",
			"获取当前患者信息失败",
		)
		return
	}

	c.JSON(http.StatusOK, response.NewPatientMeResponse(me))
}

// bindError 区分请求体格式错误（400 REQUEST_INVALID_JSON）与字段级校验失败（422）。
// 截断的 JSON（io.ErrUnexpectedEOF）属于格式错误，与 schedule 写接口的判定保持一致。
func (h *PatientAuthHandler) bindError(c *gin.Context, err error) {
	var syntaxError *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &syntaxError) ||
		errors.As(err, &typeError) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		h.writeError(c, http.StatusBadRequest, "REQUEST_INVALID_JSON", "请求体不是合法的 JSON")
		return
	}
	h.writeError(
		c,
		http.StatusUnprocessableEntity,
		"REQUEST_VALIDATION_FAILED",
		"微信登录 code 不能为空",
	)
}

// loginError 把微信登录失败映射为契约错误码
// （spec/04-api-contract.md §7.1、§10 错误码目录）。
func (h *PatientAuthHandler) loginError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, patientauthservice.ErrInvalidCode),
		errors.Is(err, patient.ErrOpenIDRequired):
		h.writeError(
			c,
			http.StatusUnprocessableEntity,
			"REQUEST_VALIDATION_FAILED",
			"微信登录 code 无效或已过期",
		)
	case errors.Is(err, patientauthservice.ErrPatientDisabled):
		h.writeError(c, http.StatusForbidden, "AUTH_FORBIDDEN", "账号已被禁用")
	case errors.Is(err, patientauthservice.ErrDependencyUnavailable),
		errors.Is(err, port.ErrWeChatUnavailable):
		h.writeError(
			c,
			http.StatusBadGateway,
			"DEPENDENCY_UNAVAILABLE",
			"微信登录服务暂时不可用，请稍后重试",
		)
	default:
		h.writeError(
			c,
			http.StatusInternalServerError,
			"INTERNAL_SERVER_ERROR",
			"登录失败，请稍后重试",
		)
	}
}

func (h *PatientAuthHandler) writeError(
	c *gin.Context,
	status int,
	code string,
	message string,
) {
	c.JSON(status, gin.H{
		"code":    code,
		"message": message,
	})
}
