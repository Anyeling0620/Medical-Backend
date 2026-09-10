package misuser

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/golang-jwt/jwt/v5"
	"strings"
	"time"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainuser "Medical-Web-Backend/internal/domain/user"
	"Medical-Web-Backend/internal/port"
	"github.com/google/uuid"
)

const (
	AccessCookieName  = "medical_access_token"
	RefreshCookieName = "medical_refresh_token"
	TokenTypeAccess   = "access"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrInvalidInput       = errors.New("username and password are required")
	ErrInactiveUser       = errors.New("user is inactive")
	ErrInvalidToken       = errors.New("invalid token")
)

type Config struct {
	JWTSecret    string
	AccessTTL    time.Duration
	RefreshTTL   time.Duration
	CookieSecure bool
}

type TokenPair struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

type LoginResult struct {
	User        *domainuser.User
	Permissions []string
	Tokens      TokenPair
}

type AccessClaims struct {
	UserID    int64            `json:"uid"`
	Username  string           `json:"username"`
	TokenType string           `json:"token_type"`
	Realm     domainauth.Realm `json:"realm"`
	SessionID string           `json:"sid"`
	jwt.RegisteredClaims
}

type Service struct {
	users  port.UserRepository
	tokens port.TokenRepository
	cfg    Config
}

func NewService(
	users port.UserRepository,
	tokens port.TokenRepository,
	cfg Config,
) *Service {
	return &Service{
		users:  users,
		tokens: tokens,
		cfg:    cfg,
	}
}

func (s *Service) Authenticate(
	ctx context.Context,
	username string,
	password string,
) (*LoginResult, error) {
	username = strings.TrimSpace(username)

	if username == "" || password == "" {
		return nil, ErrInvalidInput
	}

	if s.users == nil || s.tokens == nil {
		return nil, errors.New("authentication repositories are not configured")
	}

	u, err := s.users.FindByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrInvalidCredentials
	}

	if comparePassword(u.PasswordHash, password) != nil {
		return nil, ErrInvalidCredentials
	}

	if u.Status != 1 {
		return nil, ErrInactiveUser
	}

	permissions, err := s.users.Permissions(ctx, u.ID)
	if err != nil {
		return nil, fmt.Errorf("load permissions: %w", err)
	}

	pair, sessionID, err := s.issueTokens(u, domainauth.RealmMis)
	if err != nil {
		return nil, err
	}

	if err := s.saveRefreshSession(ctx, pair, sessionID, u, domainauth.RealmMis); err != nil {
		return nil, err
	}

	return &LoginResult{
		User:        u,
		Permissions: permissions,
		Tokens:      pair,
	}, nil
}

// Refresh 用 refresh 会话换取新的令牌对，会话所属域与请求域必须一致。
//
// 注意：当前主体解析只走管理端仓库（mis_user 与管理端权限表），因此调用方现阶段
// 只应传 RealmMis；患者域刷新必须在扩展 patient_user 的主体解析后再开放，否则会
// 用患者 ID 去命中同号管理用户（spec/02-architecture.md「认证与令牌边界」）。
func (s *Service) Refresh(
	ctx context.Context,
	refreshToken string,
	realm domainauth.Realm,
) (*LoginResult, error) {
	if refreshToken == "" || s.tokens == nil || s.users == nil || !realm.Valid() {
		return nil, ErrInvalidToken
	}

	tokenHash := hashRefreshToken(refreshToken)

	session, err := s.tokens.GetRefreshSession(ctx, tokenHash)
	if err != nil || session == nil {
		return nil, ErrInvalidToken
	}

	// 刷新会话同样按 realm 隔离：其他认证域的 refresh token 不得在本域换取新令牌。
	if session.Realm != realm {
		return nil, ErrInvalidToken
	}

	// Refresh token rotation: old token is immediately invalidated.
	if err := s.tokens.DeleteRefreshSession(ctx, tokenHash); err != nil {
		return nil, fmt.Errorf("rotate refresh session: %w", err)
	}

	u, err := s.users.FindByID(ctx, session.UserID)
	if err != nil || u == nil || u.Status != 1 {
		return nil, ErrInvalidToken
	}

	permissions, err := s.users.Permissions(ctx, u.ID)
	if err != nil {
		return nil, fmt.Errorf("load permissions: %w", err)
	}

	pair, sessionID, err := s.issueTokens(u, realm)
	if err != nil {
		return nil, err
	}

	if err := s.saveRefreshSession(ctx, pair, sessionID, u, realm); err != nil {
		return nil, err
	}

	return &LoginResult{
		User:        u,
		Permissions: permissions,
		Tokens:      pair,
	}, nil
}

func (s *Service) Logout(
	ctx context.Context,
	accessToken string,
	refreshToken string,
	realm domainauth.Realm,
) error {
	if !realm.Valid() {
		return ErrInvalidToken
	}

	if refreshToken != "" && s.tokens != nil {
		if err := s.revokeRefreshSession(ctx, refreshToken, realm); err != nil {
			return err
		}
	}

	if accessToken == "" {
		return nil
	}

	claims, err := s.ParseAccessToken(accessToken, realm)
	if err != nil {
		return ErrInvalidToken
	}

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

	return s.tokens.RevokeAccessToken(
		ctx,
		claims.ID,
		ttlSeconds+1,
	)
}

// revokeRefreshSession 撤销属于指定认证域的 refresh 会话。
// 会话不存在或已过期时按幂等空操作处理；会话属于其他 realm 时不解释也不撤销，
// 避免用两个域的令牌互相破坏对方的会话；其余读取失败必须如实上报，不得静默成功。
func (s *Service) revokeRefreshSession(
	ctx context.Context,
	refreshToken string,
	realm domainauth.Realm,
) error {
	tokenHash := hashRefreshToken(refreshToken)

	session, err := s.tokens.GetRefreshSession(ctx, tokenHash)
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

	return s.tokens.DeleteRefreshSession(ctx, tokenHash)
}

func (s *Service) ParseAccessToken(
	raw string,
	expectedRealm domainauth.Realm,
) (*AccessClaims, error) {
	if raw == "" || s.cfg.JWTSecret == "" || !expectedRealm.Valid() {
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
			return []byte(s.cfg.JWTSecret), nil
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

func (s *Service) IsRevoked(
	ctx context.Context,
	jti string,
) (bool, error) {
	if s.tokens == nil {
		return false, errors.New("token repository is not configured")
	}

	return s.tokens.IsAccessTokenRevoked(ctx, jti)
}

func (s *Service) issueTokens(
	u *domainuser.User,
	realm domainauth.Realm,
) (TokenPair, string, error) {
	if s.cfg.JWTSecret == "" ||
		s.cfg.AccessTTL <= 0 ||
		s.cfg.RefreshTTL <= 0 {
		return TokenPair{}, "", errors.New(
			"JWT authentication configuration is incomplete",
		)
	}

	now := time.Now()
	sessionID := uuid.NewString()
	accessExpiresAt := now.Add(s.cfg.AccessTTL)

	claims := AccessClaims{
		UserID:    u.ID,
		Username:  u.Username,
		TokenType: TokenTypeAccess,
		Realm:     realm,
		SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "medical-backend",
			Subject:   fmt.Sprintf("%d", u.ID),
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(accessExpiresAt),
		},
	}

	accessToken, err := jwt.NewWithClaims(
		jwt.SigningMethodHS256,
		claims,
	).SignedString([]byte(s.cfg.JWTSecret))
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
		RefreshExpiresAt: now.Add(s.cfg.RefreshTTL),
	}, sessionID, nil
}

func (s *Service) saveRefreshSession(
	ctx context.Context,
	pair TokenPair,
	sessionID string,
	u *domainuser.User,
	realm domainauth.Realm,
) error {
	ttlSeconds := int64(
		time.Until(pair.RefreshExpiresAt) / time.Second,
	)
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}

	err := s.tokens.SaveRefreshSession(
		ctx,
		port.RefreshSession{
			TokenHash: hashRefreshToken(pair.RefreshToken),
			SessionID: sessionID,
			UserID:    u.ID,
			Username:  u.Username,
			Realm:     realm,
		},
		ttlSeconds,
	)
	if err != nil {
		return fmt.Errorf("save refresh session: %w", err)
	}

	return nil
}

func hashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
