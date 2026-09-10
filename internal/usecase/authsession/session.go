// Package authsession 提供管理端（mis）与患者端（patient）共用的令牌与会话基础设施。
//
// 两个认证域共用同一份 JWT 签发/解析、Redis refresh 会话轮换与撤销逻辑，只用 realm
// 区分令牌主体归属，患者域因此不需要复制一套与管理域平行的认证实现
// （spec/02-architecture.md「职责约束」、spec/04-api-contract.md §1.2 与 §7）。
// 会话语义（Cookie 名、TTL、轮换、撤销）的调整只需改动本包。
package authsession

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/port"
)

const (
	// AccessCookieName / RefreshCookieName 是两个认证域共用的 Cookie 名。
	// 管理端用浏览器打开、患者端用小程序打开，两端凭据不会落在同一个容器里，
	// 因此复用同一组名字，让 access token 的 Cookie 兜底通道对两个域都成立。
	AccessCookieName  = "medical_access_token"
	RefreshCookieName = "medical_refresh_token"

	// TokenTypeAccess 是 access token 的 token_type 取值；refresh token 不是 JWT。
	TokenTypeAccess = "access"
)

// ErrInvalidToken 表示令牌、刷新会话或 realm 不满足要求。
// 调用方据此返回 401：access token 相关用 AUTH_INVALID_TOKEN，
// refresh token 相关用 AUTH_INVALID_REFRESH_TOKEN。
var ErrInvalidToken = errors.New("invalid token")

// Config 是共用会话层需要的签名与有效期配置。
type Config struct {
	JWTSecret  string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
}

// TokenPair 是一次签发产生的 access/refresh 令牌对。
type TokenPair struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

// Subject 是令牌主体：管理端对应 mis_user，患者端对应 patient_user。
// Name 只写入 refresh 会话记录用于排障展示（管理端存 username，患者端存昵称），
// 不参与任何授权判断。
type Subject struct {
	ID   int64
	Name string
}

// AccessClaims 是 access token 的载荷。
//
// realm 是必填字段：缺少 realm 的旧令牌一律按无效令牌处理，不得回退到另一域解释。
type AccessClaims struct {
	UserID    int64            `json:"uid"`
	Username  string           `json:"username"`
	TokenType string           `json:"token_type"`
	Realm     domainauth.Realm `json:"realm"`
	SessionID string           `json:"sid"`
	jwt.RegisteredClaims
}

// Manager 持有令牌签发与 Redis 会话读写能力，由两个认证域的 use case 共用。
type Manager struct {
	tokens port.TokenRepository
	cfg    Config
}

// NewManager 构造共用会话层。
func NewManager(tokens port.TokenRepository, cfg Config) *Manager {
	return &Manager{tokens: tokens, cfg: cfg}
}

// Issue 按 realm 与主体签发 access/refresh 令牌对，并返回本次会话 ID。
// sessionID 同时写入 access token 的 sid claim 与 refresh 会话记录：
// 登出时即使只携带 access token，也能凭 sid 撤销对应的 refresh 会话。
func (m *Manager) Issue(
	realm domainauth.Realm,
	subject Subject,
) (TokenPair, string, error) {
	// 签发是 realm 隔离的起点：守卫放在这里，任何调用点都签不出非法 realm 的令牌。
	if m == nil || !realm.Valid() {
		return TokenPair{}, "", ErrInvalidToken
	}

	if m.cfg.JWTSecret == "" ||
		m.cfg.AccessTTL <= 0 ||
		m.cfg.RefreshTTL <= 0 {
		return TokenPair{}, "", errors.New(
			"JWT authentication configuration is incomplete",
		)
	}

	now := time.Now()
	sessionID := uuid.NewString()
	accessExpiresAt := now.Add(m.cfg.AccessTTL)

	claims := AccessClaims{
		UserID:    subject.ID,
		Username:  subject.Name,
		TokenType: TokenTypeAccess,
		Realm:     realm,
		SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "medical-backend",
			Subject:   fmt.Sprintf("%d", subject.ID),
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(accessExpiresAt),
		},
	}

	accessToken, err := jwt.NewWithClaims(
		jwt.SigningMethodHS256,
		claims,
	).SignedString([]byte(m.cfg.JWTSecret))
	if err != nil {
		return TokenPair{}, "", err
	}

	refreshBytes := make([]byte, 32)
	if _, err := rand.Read(refreshBytes); err != nil {
		return TokenPair{}, "", err
	}

	return TokenPair{
		AccessToken:      accessToken,
		RefreshToken:     hex.EncodeToString(refreshBytes),
		AccessExpiresAt:  accessExpiresAt,
		RefreshExpiresAt: now.Add(m.cfg.RefreshTTL),
	}, sessionID, nil
}

// SaveRefreshSession 把 refresh 会话写入 Redis，TTL 到会话过期为止。
// 存的是 refresh token 的摘要而不是原文，Redis 中不出现可用的凭据原文。
func (m *Manager) SaveRefreshSession(
	ctx context.Context,
	pair TokenPair,
	sessionID string,
	subject Subject,
	realm domainauth.Realm,
) error {
	if m == nil || m.tokens == nil {
		return errors.New("token repository is not configured")
	}

	ttlSeconds := int64(time.Until(pair.RefreshExpiresAt) / time.Second)
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}

	err := m.tokens.SaveRefreshSession(
		ctx,
		port.RefreshSession{
			TokenHash: HashRefreshToken(pair.RefreshToken),
			SessionID: sessionID,
			UserID:    subject.ID,
			Username:  subject.Name,
			Realm:     realm,
		},
		ttlSeconds,
	)
	if err != nil {
		return fmt.Errorf("save refresh session: %w", err)
	}

	return nil
}

// ParseAccessToken 解析并校验 access token。
// 签名算法、token_type、jti 与 realm 任一不符都返回 ErrInvalidToken：
// 其他认证域的令牌绝不在这里按本域主体解释（spec/04-api-contract.md §1.2）。
func (m *Manager) ParseAccessToken(
	raw string,
	expectedRealm domainauth.Realm,
) (*AccessClaims, error) {
	if m == nil ||
		raw == "" ||
		m.cfg.JWTSecret == "" ||
		!expectedRealm.Valid() {
		return nil, ErrInvalidToken
	}

	claims := &AccessClaims{}

	token, err := jwt.ParseWithClaims(
		raw,
		claims,
		func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, ErrInvalidToken
			}
			return []byte(m.cfg.JWTSecret), nil
		},
	)

	if err != nil ||
		!token.Valid ||
		claims.TokenType != TokenTypeAccess ||
		claims.ID == "" ||
		claims.Realm != expectedRealm {
		return nil, ErrInvalidToken
	}

	return claims, nil
}

// IsRevoked 查询 access token 是否已被撤销（命中 Redis 黑名单）。
func (m *Manager) IsRevoked(
	ctx context.Context,
	jti string,
) (bool, error) {
	if m == nil || m.tokens == nil {
		return false, errors.New("token repository is not configured")
	}

	return m.tokens.IsAccessTokenRevoked(ctx, jti)
}

// RotateRefreshSession 读取 refresh 会话并立即撤销它（refresh token rotation）。
// 返回的会话供调用方重新加载主体；会话不存在、已过期、已轮换或 realm 不匹配
// 一律返回 ErrInvalidToken（不可区分，避免用状态码枚举他人会话）。
func (m *Manager) RotateRefreshSession(
	ctx context.Context,
	refreshToken string,
	realm domainauth.Realm,
) (*port.RefreshSession, error) {
	if m == nil ||
		refreshToken == "" ||
		m.tokens == nil ||
		!realm.Valid() {
		return nil, ErrInvalidToken
	}

	tokenHash := HashRefreshToken(refreshToken)

	session, err := m.tokens.GetRefreshSession(ctx, tokenHash)
	switch {
	case errors.Is(err, port.ErrRefreshSessionNotFound),
		err == nil && session == nil:
		// 会话不存在、已过期或已轮换：一律按无效令牌处理。
		return nil, ErrInvalidToken
	case err != nil:
		// 读取失败（例如 Redis 不可用）不等于「没有这个会话」：必须原样上报，
		// 调用方才能按 502 DEPENDENCY_UNAVAILABLE 而不是 401 处理。
		return nil, fmt.Errorf("load refresh session: %w", err)
	case session.Realm != realm:
		// 刷新会话同样按 realm 隔离：其他认证域的 refresh token 不得在本域换取新令牌；
		// 上面的读取没有副作用，因此跨域调用不会破坏对方的会话。
		return nil, ErrInvalidToken
	}

	// 已知限制（M3）：轮换是「读取 -> 校验 realm -> 删除」，三步之间不是原子操作。
	// 同一 refresh token 被并发提交时，两个请求可能都通过校验并各签发一份新会话
	// （表现为同一令牌被使用两次，而不是其中一方被拒绝）。要彻底关闭该窗口需要
	// 存储层提供「按值比较后再删除」的原子消费（例如 Lua 脚本或 GETDEL + 前缀校验），
	// 当前接口未提供，故保留为已知限制并记录在此。
	if err := m.tokens.DeleteRefreshSession(ctx, tokenHash, session.SessionID); err != nil {
		return nil, fmt.Errorf("rotate refresh session: %w", err)
	}

	return session, nil
}

// RevokeRefreshSession 撤销属于指定认证域的 refresh 会话。
// 会话不存在或已过期时按幂等空操作处理；会话属于其他 realm 时不解释也不撤销，
// 避免用两个域的令牌互相破坏对方的会话；其余读取失败必须如实上报，不得静默成功。
func (m *Manager) RevokeRefreshSession(
	ctx context.Context,
	refreshToken string,
	realm domainauth.Realm,
) error {
	if m == nil || m.tokens == nil {
		return errors.New("token repository is not configured")
	}

	tokenHash := HashRefreshToken(refreshToken)

	session, err := m.tokens.GetRefreshSession(ctx, tokenHash)
	switch {
	case errors.Is(err, port.ErrRefreshSessionNotFound),
		err == nil && session == nil:
		// 会话不存在、已过期或已轮换：登出保持幂等成功。
		return nil
	case err != nil:
		// 读取失败（例如 Redis 不可用）不等于「没有会话」，
		// 此时无法判断会话归属，必须上报而不是继续删除。
		return err
	case session.Realm != realm:
		// 其他认证域的会话不属于本次登出范围。
		return nil
	}

	return m.tokens.DeleteRefreshSession(ctx, tokenHash, session.SessionID)
}

// Logout 撤销 access token 与 refresh 会话，两种令牌都可缺省，保证登出幂等。
//
// 处理顺序：
//  1. 携带 access token 时先解析：realm/签名/有效期任一不符即返回 ErrInvalidToken，
//     且此时**尚未产生任何副作用**（患者端据此满足契约 §7.2 的严格语义）。
//     空 access token 是管理端「只带 refresh Cookie 登出」的合法用法，跳过本步；
//  2. 携带 refresh token 时按 realm 撤销该会话（其他认证域的会话不动）；
//  3. 撤销 access token 本身（安全关键，单条 SET，最可靠），再用 claims.sid 尽力
//     撤销本次登录的 refresh 会话。第 3 步不依赖调用方提交 refresh token，使「只带
//     access token 登出」时 refresh 令牌也立即失效，而不是留到自然过期
//     （契约 §3.3、§7.2）；反查索引缺失（会话已过期或已被轮换）或令牌没有 sid
//     claim 时退化为只撤 jti，不因此让登出失败。
//
// 容忍与否由调用方决定：管理端把 ErrInvalidToken 视作幂等成功（仍返回 204），
// 患者端按 401 AUTH_INVALID_TOKEN 处理。
//
// 会话不存在、access token 已过期或已撤销都算成功；只有存储层故障才返回错误
// （按 refresh token 撤销时的会话读取失败、写 jti 撤销名单失败，或按 sid 清理失败）。
// 由于 access token 的撤销先于按 sid 清理，后者失败时返回错误但令牌已经失效，
// 属于「已登出、refresh 会话清理不完整」，重试登出即可收敛。
func (m *Manager) Logout(
	ctx context.Context,
	accessToken string,
	refreshToken string,
	realm domainauth.Realm,
) error {
	if m == nil || !realm.Valid() {
		return ErrInvalidToken
	}

	// 严格语义（患者端契约 §7.2）：调用方提交了 access token 就先校验，
	// 校验失败必须在任何撤销动作之前返回，保证「无效令牌」分支零副作用。
	var claims *AccessClaims
	if accessToken != "" {
		parsed, err := m.ParseAccessToken(accessToken, realm)
		if err != nil {
			return ErrInvalidToken
		}
		claims = parsed

		// 令牌仓库缺失属于服务端配置错误，但必须在解析成功之后判定：
		// 否则「无效令牌 + 未配置仓库」会被误报成 500 而不是 401。
		if m.tokens == nil {
			return errors.New("token repository is not configured")
		}
	}

	if refreshToken != "" && m.tokens != nil {
		if err := m.RevokeRefreshSession(ctx, refreshToken, realm); err != nil {
			return err
		}
	}

	if claims == nil {
		return nil
	}

	// 先撤销 access token 本身：这是安全关键动作（单条 SET，最可靠），
	// 不能被下面「尽力而为」的 refresh 会话清理失败拦住，
	// 否则已登出的 access token 会在 TTL 内继续可用。
	revokeErr := m.revokeAccessToken(ctx, claims)

	if claims.SessionID == "" {
		// 缺少 sid 的令牌无法定位 refresh 会话，退化为只撤 jti。
		return revokeErr
	}

	// 再用 claims.sid 撤销本次登录的 refresh 会话；索引不存在时是幂等空操作。
	// 返回值 bool 仅用于审计与测试：登出语义下 false 等同于「会话已不存在」。
	if _, err := m.tokens.DeleteRefreshSessionBySessionID(
		ctx,
		claims.SessionID,
	); err != nil {
		if revokeErr != nil {
			return fmt.Errorf("revoke access token: %w", revokeErr)
		}
		return fmt.Errorf("revoke refresh session by session id: %w", err)
	}

	return revokeErr
}

// revokeAccessToken 把 access token 的 jti 写入撤销名单，TTL 取令牌剩余有效期。
// 令牌已过期或缺少过期时间时无需撤销（到期即失效），返回 nil。
func (m *Manager) revokeAccessToken(
	ctx context.Context,
	claims *AccessClaims,
) error {
	if claims.ExpiresAt == nil {
		return nil
	}

	remaining := time.Until(claims.ExpiresAt.Time)
	if remaining <= 0 {
		return nil
	}

	ttlSeconds := int64(remaining / time.Second)
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}

	return m.tokens.RevokeAccessToken(
		ctx,
		claims.ID,
		ttlSeconds+1,
	)
}

// HashRefreshToken 对 refresh token 做不可逆摘要，
// 使 Redis 只保存摘要、日志中也不会出现凭据原文。
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
