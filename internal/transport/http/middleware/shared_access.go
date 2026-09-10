package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/usecase/authsession"
)

// 本文件是双 realm 共享业务路由的访问令牌与授权中间件。
//
// `/api/v1/registrations/*`（以及后续的 `/payments/*`、`/consultations/*`、
// `/prescriptions/*`）同时接受 realm=mis 与 realm=patient 的令牌：管理端令牌必须带
// 对应权限编码，患者令牌只能访问当前患者自己的资源，越权一律按资源不存在处理
// （spec/04-api-contract.md §1.2、§02-architecture.md「认证与令牌边界」）。
// 这就是不能复用 RequireAccessToken(verifier, realm) 的原因：它只接受单一 realm。

// RequireSharedAccess 构造共享业务路由的令牌校验中间件。
//
// 解析顺序固定为「先 patient、后 mis」，并把 realm 与对应校验器绑定：
// 撤销检查必须用解析该令牌的同一会话层，避免用另一域的存储解释同一个 jti。
// 两种 realm 都不匹配（含令牌被撤销、或患者令牌缺少主体）时统一返回
// 401 AUTH_INVALID_TOKEN，绝不回退到另一域的主体解释令牌。
func RequireSharedAccess(patientVerifier, misVerifier AccessTokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		verifier, claims := parseSharedAccessToken(c, patientVerifier, misVerifier)
		if claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "AUTH_INVALID_TOKEN",
				"message": "访问令牌无效或已过期",
			})
			return
		}

		revoked, err := verifier.IsRevoked(c.Request.Context(), claims.ID)
		if err != nil || revoked {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "AUTH_INVALID_TOKEN",
				"message": "访问令牌无效或已过期",
			})
			return
		}

		c.Set(ClaimsKey, claims)
		c.Next()
	}
}

// parseSharedAccessToken 依次尝试患者域与管理端令牌，返回命中的校验器与载荷。
func parseSharedAccessToken(
	c *gin.Context,
	patientVerifier AccessTokenVerifier,
	misVerifier AccessTokenVerifier,
) (AccessTokenVerifier, *authsession.AccessClaims) {
	for _, rawToken := range accessTokenCandidates(c) {
		if parsed, err := patientVerifier.ParseAccessToken(rawToken, domainauth.RealmPatient); err == nil {
			// 患者令牌必须携带患者主键：缺少 subject 的载荷无法定位资源，按无效令牌处理。
			if parsed != nil && parsed.UserID > 0 {
				return patientVerifier, parsed
			}
			continue
		}
		if parsed, err := misVerifier.ParseAccessToken(rawToken, domainauth.RealmMis); err == nil && parsed != nil {
			return misVerifier, parsed
		}
	}
	return nil, nil
}

// RequirePermissionOrPatient 构造共享业务路由的授权中间件。
//
// 患者令牌直接放行：主体固定为令牌中的当前患者，越权隐藏由 use case 按
// 「资源不存在」（404）完成，而不是 403——避免用状态码枚举他人资源（契约 §1.2）。
// 管理端令牌必须命中 allowed 中的一个权限编码，否则 403 AUTH_FORBIDDEN。
// 权限判定与管理端专用路由完全一致（同样先断言 realm=mis，再用主体查询权限表）。
func RequirePermissionOrPatient(
	userRepository port.UserRepository,
	allowed []string,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := ClaimsFrom(c)
		if !ok || claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "AUTH_INVALID_TOKEN",
				"message": "访问令牌无效或已过期",
			})
			return
		}

		switch claims.Realm {
		case domainauth.RealmPatient:
			c.Next()
			return
		case domainauth.RealmMis:
			// 继续做权限编码校验。
		default:
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "AUTH_INVALID_TOKEN",
				"message": "访问令牌无效或已过期",
			})
			return
		}

		if userRepository == nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    "INTERNAL_SERVER_ERROR",
				"message": "权限校验失败",
			})
			return
		}

		permissions, err := userRepository.Permissions(
			c.Request.Context(),
			claims.UserID,
		)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    "INTERNAL_SERVER_ERROR",
				"message": "权限校验失败",
			})
			return
		}

		if !hasAllowedPermission(permissions, allowed) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    "AUTH_FORBIDDEN",
				"message": "没有访问权限",
			})
			return
		}

		c.Next()
	}
}
