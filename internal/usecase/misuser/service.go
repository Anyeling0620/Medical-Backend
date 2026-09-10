package misuser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainuser "Medical-Web-Backend/internal/domain/user"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/usecase/authsession"
)

const (
	// Cookie 名与令牌类型由共用会话层统一定义：管理端与患者端必须一致，
	// 否则同一份中间件无法对两个域都提供 Cookie 兜底通道。
	AccessCookieName  = authsession.AccessCookieName
	RefreshCookieName = authsession.RefreshCookieName
	TokenTypeAccess   = authsession.TokenTypeAccess
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrInvalidInput       = errors.New("username and password are required")
	ErrInactiveUser       = errors.New("user is inactive")
	// ErrInvalidToken 直接指向共用会话层的哨兵错误，
	// 保证管理端与患者端对「令牌无效」的判定与处理完全一致。
	ErrInvalidToken = authsession.ErrInvalidToken
)

// 类型别名：handler、middleware 与既有测试继续通过 userservice 引用这些类型，
// 但实现只有共用会话层（authsession）一份。
type (
	AccessClaims = authsession.AccessClaims
	TokenPair    = authsession.TokenPair
)

type Config struct {
	JWTSecret    string
	AccessTTL    time.Duration
	RefreshTTL   time.Duration
	CookieSecure bool
}

type LoginResult struct {
	User        *domainuser.User
	Permissions []string
	Tokens      TokenPair
}

// Service 是管理端（realm=mis）认证 use case：负责 mis_user 主体解析与权限加载，
// 令牌签发、解析、轮换与撤销全部委托给共用会话层。
type Service struct {
	users   port.UserRepository
	tokens  port.TokenRepository
	session *authsession.Manager
}

func NewService(
	users port.UserRepository,
	tokens port.TokenRepository,
	cfg Config,
) *Service {
	return &Service{
		users:  users,
		tokens: tokens,
		session: authsession.NewManager(
			tokens,
			authsession.Config{
				JWTSecret:  cfg.JWTSecret,
				AccessTTL:  cfg.AccessTTL,
				RefreshTTL: cfg.RefreshTTL,
			},
		),
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

	if err := s.saveRefreshSession(
		ctx,
		pair,
		sessionID,
		u,
		domainauth.RealmMis,
	); err != nil {
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
// 只应传 RealmMis；患者域刷新走 patientauth.Service，否则会用患者 ID 去命中同号
// 管理用户（spec/02-architecture.md「认证与令牌边界」）。
func (s *Service) Refresh(
	ctx context.Context,
	refreshToken string,
	realm domainauth.Realm,
) (*LoginResult, error) {
	if refreshToken == "" || s.tokens == nil || s.users == nil || !realm.Valid() {
		return nil, ErrInvalidToken
	}

	// 轮换：旧 refresh token 立即失效，并校验会话 realm 与请求域一致。
	session, err := s.session.RotateRefreshSession(ctx, refreshToken, realm)
	if err != nil {
		return nil, err
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

// Logout 撤销当前会话，幂等；是否容忍无效 access token 由 handler 决定。
func (s *Service) Logout(
	ctx context.Context,
	accessToken string,
	refreshToken string,
	realm domainauth.Realm,
) error {
	return s.session.Logout(ctx, accessToken, refreshToken, realm)
}

func (s *Service) ParseAccessToken(
	raw string,
	expectedRealm domainauth.Realm,
) (*AccessClaims, error) {
	return s.session.ParseAccessToken(raw, expectedRealm)
}

func (s *Service) IsRevoked(
	ctx context.Context,
	jti string,
) (bool, error) {
	return s.session.IsRevoked(ctx, jti)
}

// issueTokens 按 realm 签发令牌对，主体固定为管理端用户。
func (s *Service) issueTokens(
	u *domainuser.User,
	realm domainauth.Realm,
) (TokenPair, string, error) {
	return s.session.Issue(realm, authsession.Subject{
		ID:   u.ID,
		Name: u.Username,
	})
}

// saveRefreshSession 写入 refresh 会话，主体信息取管理端用户名。
func (s *Service) saveRefreshSession(
	ctx context.Context,
	pair TokenPair,
	sessionID string,
	u *domainuser.User,
	realm domainauth.Realm,
) error {
	return s.session.SaveRefreshSession(ctx, pair, sessionID, authsession.Subject{
		ID:   u.ID,
		Name: u.Username,
	}, realm)
}
