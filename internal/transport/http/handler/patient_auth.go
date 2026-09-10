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
// 成功响应用 isNewUser 区分本次是否为新注册；refresh token 同时经 HttpOnly Cookie
// （浏览器通道）与响应体（小程序通道，wx.request 不携带 Cookie）下发。
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
		IsNewUser:        result.IsNewUser,
		Patient:          response.NewPatientSummary(result.Patient),
		CardID:           cardID,
		AccessToken:      result.Tokens.AccessToken,
		AccessExpiresAt:  result.Tokens.AccessExpiresAt.UTC().Format(time.RFC3339),
		RefreshToken:     result.Tokens.RefreshToken,
		RefreshExpiresAt: result.Tokens.RefreshExpiresAt.UTC().Format(time.RFC3339),
	})
}

// Refresh 处理 POST /api/v1/patient/auth/refresh。
//
// refresh token 允许两条通道携带（契约 §7.2）：请求体（小程序，无法使用 Cookie）
// 与 Cookie（浏览器）。请求体优先；请求体令牌无效时继续尝试 Cookie，
// 两条通道都拿不出有效令牌才返回 401，避免「携带过期请求体令牌但持有有效 Cookie」
// 的浏览器被误判成未认证。
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

	candidates := refreshTokenCandidates(req.RefreshToken, c)
	if len(candidates) == 0 {
		// 未携带凭据与凭据无效返回同一分支：不泄露令牌是否存在。
		h.refreshError(c, patientauthservice.ErrInvalidRefreshToken)
		return
	}

	var (
		pair authsession.TokenPair
		err  error
	)
	for _, candidate := range candidates {
		pair, err = h.service.Refresh(c.Request.Context(), candidate)
		// 只有「令牌无效」才继续尝试下一条通道；轮换成功或账号禁用、依赖故障
		// 都必须立即结束循环，后者不能被另一条通道的结果掩盖（契约 §7.2、§10）。
		if err == nil || !errors.Is(err, patientauthservice.ErrInvalidRefreshToken) {
			break
		}
	}
	if err != nil {
		h.refreshError(c, err)
		return
	}

	// 轮换后的 refresh token 同时覆盖 Cookie 与响应体：浏览器沿用 Cookie 通道，
	// 小程序从响应体读取新令牌并本地保存（契约 §7.2）。
	setTokenCookies(c, pair, h.secure)

	c.JSON(http.StatusOK, response.PatientRefreshResponse{
		AccessToken:      pair.AccessToken,
		AccessExpiresAt:  pair.AccessExpiresAt.UTC().Format(time.RFC3339),
		RefreshToken:     pair.RefreshToken,
		RefreshExpiresAt: pair.RefreshExpiresAt.UTC().Format(time.RFC3339),
	})
}

// refreshTokenCandidates 收集请求中可能携带的 refresh token（契约 §7.2 允许两种来源）：
// 请求体优先（小程序没有 Cookie 通道），Cookie 作为备用来源（浏览器）。
// 空白值按未携带处理，避免把空串当成凭据去校验。
func refreshTokenCandidates(bodyToken string, c *gin.Context) []string {
	candidates := make([]string, 0, 2)

	if token := strings.TrimSpace(bodyToken); token != "" {
		candidates = append(candidates, token)
	}

	if token, err := c.Cookie(authsession.RefreshCookieName); err == nil {
		if token = strings.TrimSpace(token); token != "" {
			candidates = append(candidates, token)
		}
	}

	return candidates
}

// refreshError 把刷新失败映射为契约错误码（spec/04-api-contract.md §7.2、§10）。
func (h *PatientAuthHandler) refreshError(c *gin.Context, err error) {
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
	// refresh token 只用于确定撤销范围，取首个可用通道（请求体优先、Cookie 备用）；
	// access token 的 sid 兜底撤销不依赖它，因此缺省或失配都不影响登出幂等。
	candidates := refreshTokenCandidates(req.RefreshToken, c)
	refreshToken := ""
	if len(candidates) > 0 {
		refreshToken = candidates[0]
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
