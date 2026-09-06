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
	UserID    int64  `json:"uid"`
	Username  string `json:"username"`
	TokenType string `json:"token_type"`
	SessionID string `json:"sid"`
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

	pair, sessionID, err := s.issueTokens(u)
	if err != nil {
		return nil, err
	}

	if err := s.saveRefreshSession(ctx, pair, sessionID, u); err != nil {
		return nil, err
	}

	return &LoginResult{
		User:        u,
		Permissions: permissions,
		Tokens:      pair,
	}, nil
}

func (s *Service) Refresh(
	ctx context.Context,
	refreshToken string,
) (*LoginResult, error) {
	if refreshToken == "" || s.tokens == nil || s.users == nil {
		return nil, ErrInvalidToken
	}

	tokenHash := hashRefreshToken(refreshToken)

	session, err := s.tokens.GetRefreshSession(ctx, tokenHash)
	if err != nil || session == nil {
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

	pair, sessionID, err := s.issueTokens(u)
	if err != nil {
		return nil, err
	}

	if err := s.saveRefreshSession(ctx, pair, sessionID, u); err != nil {
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
) error {
	if refreshToken != "" && s.tokens != nil {
		if err := s.tokens.DeleteRefreshSession(
			ctx,
			hashRefreshToken(refreshToken),
		); err != nil {
			return err
		}
	}

	if accessToken == "" {
		return nil
	}

	claims, err := s.ParseAccessToken(accessToken)
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

func (s *Service) ParseAccessToken(
	raw string,
) (*AccessClaims, error) {
	if raw == "" || s.cfg.JWTSecret == "" {
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
		claims.ID == "" {
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
