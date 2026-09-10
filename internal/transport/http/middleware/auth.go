package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/usecase/authsession"
)

const ClaimsKey = "auth.claims"

// ClaimsFrom 读取中间件写入上下文的访问令牌载荷。
// 第二个返回值为 false 表示请求未经 RequireAccessToken 校验或载荷类型异常，
// 调用方必须按未认证处理，不得降级放行。
func ClaimsFrom(c *gin.Context) (*authsession.AccessClaims, bool) {
	value, exists := c.Get(ClaimsKey)
	if !exists {
		return nil, false
	}
	claims, ok := value.(*authsession.AccessClaims)
	if !ok || claims == nil {
		return nil, false
	}
	return claims, true
}

// AccessTokenVerifier 是访问令牌校验所需的全部能力。
// 管理端 use case（misuser.Service）与患者端 use case（patientauth.Service）
// 都通过共用会话层实现它，因此中间件不绑定任何一个域的具体实现。
type AccessTokenVerifier interface {
	ParseAccessToken(raw string, expectedRealm domainauth.Realm) (*authsession.AccessClaims, error)
	IsRevoked(ctx context.Context, jti string) (bool, error)
}

// RequireAccessToken 构造访问令牌校验中间件。
// 只有 realm 与 expectedRealm 一致的令牌才会被接受：管理域传 RealmMis，
// 患者域传 RealmPatient。令牌 realm 不匹配（例如患者令牌访问管理端接口）时
// 统一按无效令牌返回 401 AUTH_INVALID_TOKEN，绝不回退到另一域解释同一个令牌。
func RequireAccessToken(
	verifier AccessTokenVerifier,
	expectedRealm domainauth.Realm,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		var claims *authsession.AccessClaims

		// 优先读取 Authorization 请求头。
		// 如果请求头中的 token 无效，再尝试读取 Cookie 中的 access token。
		for _, rawToken := range accessTokenCandidates(c) {
			parsedClaims, err := verifier.ParseAccessToken(rawToken, expectedRealm)
			if err != nil {
				continue
			}

			claims = parsedClaims
			break
		}

		if claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "AUTH_INVALID_TOKEN",
				"message": tokenInvalidMessage(expectedRealm),
			})
			return
		}

		// 检查 access token 是否已经被注销或加入 Redis 黑名单。
		revoked, err := verifier.IsRevoked(
			c.Request.Context(),
			claims.ID,
		)
		if err != nil || revoked {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "AUTH_INVALID_TOKEN",
				"message": tokenInvalidMessage(expectedRealm),
			})
			return
		}

		c.Set(ClaimsKey, claims)
		c.Next()
	}
}

// tokenInvalidMessage 按认证域返回契约中的 401 文案
// （spec/04-api-contract.md §12.1 管理端、§12.5 患者端）。
func tokenInvalidMessage(realm domainauth.Realm) string {
	if realm == domainauth.RealmPatient {
		return "患者访问令牌无效"
	}
	return "访问令牌无效或已过期"
}

// accessTokenCandidates 返回请求中可能存在的 access token。
// Authorization 优先，Cookie 作为备用来源。
func accessTokenCandidates(c *gin.Context) []string {
	candidates := make([]string, 0, 2)

	// 读取：
	// Authorization: Bearer eyJ...
	if token := BearerToken(c.GetHeader("Authorization")); token != "" {
		candidates = append(candidates, token)
	}

	// 读取：
	// Cookie: medical_access_token=eyJ...
	if token, err := c.Cookie(authsession.AccessCookieName); err == nil {
		if token != "" {
			candidates = append(candidates, token)
		}
	}

	return candidates
}

func BearerToken(header string) string {
	parts := strings.Fields(header)

	if len(parts) == 2 &&
		strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}

	return ""
}
