// Package patientauth 实现患者端（微信小程序）的认证与会话用例：
// 微信登录（登录与注册合一）、刷新、登出与当前患者摘要
// （spec/04-api-contract.md §7、spec/流程.md「患者身份」批次）。
//
// 令牌签发、解析、轮换与撤销复用共用会话层 authsession，
// 本包只负责患者主体（patient_user）的解析与患者域的响应组装。
package patientauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/domain/patient"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/usecase/authsession"
)

// 微信登录 code 的长度约束（spec/04-api-contract.md §7.1：1-128 字符）。
const (
	minLoginCodeLength = 1
	maxLoginCodeLength = 128
)

var (
	// ErrInvalidCode 表示微信登录 code 缺失、超长，或微信判定其无效/已被使用。
	// 映射为 422 REQUEST_VALIDATION_FAILED。
	ErrInvalidCode = errors.New("wechat login code is invalid")
	// ErrPatientDisabled 表示 patient_user.status 非 ACTIVE（2=禁用），
	// 不签发任何令牌，映射为 403 AUTH_FORBIDDEN。
	ErrPatientDisabled = errors.New("patient account is disabled")
	// ErrInvalidRefreshToken 表示 refresh 令牌缺失、无效、过期、已撤销，
	// 或属于其他认证域，映射为 401 AUTH_INVALID_REFRESH_TOKEN。
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
	// ErrInvalidAccessToken 表示 access 令牌缺失、无效、过期或 realm 不匹配，
	// 映射为 401 AUTH_INVALID_TOKEN。
	ErrInvalidAccessToken = errors.New("invalid access token")
	// ErrDependencyUnavailable 表示患者仓储（PostgreSQL）或会话存储（Redis）不可用，
	// 映射为 502 DEPENDENCY_UNAVAILABLE（可重试）。依赖故障不能被伪装成凭据无效，
	// 否则客户端会误以为需要重新登录并立即重试（spec/04-api-contract.md §10）。
	ErrDependencyUnavailable = errors.New("patient dependency is unavailable")
)

// dependencyError 把仓储/会话层返回的非领域错误归类为依赖不可用。
// 领域错误（如 patient.ErrPatientNotFound）不经过这里，保持既有 401/404 语义。
func dependencyError(err error) error {
	if err == nil {
		return nil
	}
	// 第二个 %w 保留原因错误链（Go 1.20+ 支持多个 %w）：
	// 调用方既能用 errors.Is 识别 ErrDependencyUnavailable（→502），
	// 也能识别 port.ErrWeChatUnavailable 等根因哨兵，日志不丢原始错误。
	return fmt.Errorf("%w: %w", ErrDependencyUnavailable, err)
}

// Config 是患者端认证所需的签名与有效期配置，取值与管理端一致（同一会话层）。
type Config struct {
	JWTSecret  string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
}

// LoginResult 是微信登录（登录与注册合一）成功后的用例结果。
// Card 为 nil 表示账号尚未实名建卡，响应中的 cardId 输出 null。
type LoginResult struct {
	IsNewUser bool
	Patient   *patient.Patient
	Card      *patient.Card
	Tokens    authsession.TokenPair
}

// Service 是患者端认证 use case，只处理 realm=patient 的会话。
type Service struct {
	patients port.PatientRepository
	tokens   port.TokenRepository
	wechat   port.WeChatAuthenticator
	session  *authsession.Manager
}

// NewService 构造患者端认证 use case。
// patients 提供患者账号与就诊卡读写；tokens 与 session 是两个认证域共用的会话层。
func NewService(
	patients port.PatientRepository,
	tokens port.TokenRepository,
	wechat port.WeChatAuthenticator,
	cfg Config,
) *Service {
	return &Service{
		patients: patients,
		tokens:   tokens,
		wechat:   wechat,
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

// WeChatLogin 处理 POST /api/v1/patient/auth/wechat-login：按 openId 登录或注册。
//
// openid 只能由微信签发：本方法不接受、也不缓存客户端提交的任何身份标识，
// 因此不存在「开发期直通」实现，测试通过替换 port.WeChatAuthenticator 隔离外部依赖。
func (s *Service) WeChatLogin(
	ctx context.Context,
	code string,
) (*LoginResult, error) {
	code = strings.TrimSpace(code)
	// 长度按 Unicode 字符数校验（契约 §7.1「1-128 字符」）。
	codeLength := utf8.RuneCountInString(code)
	if codeLength < minLoginCodeLength || codeLength > maxLoginCodeLength {
		return nil, ErrInvalidCode
	}
	if s.patients == nil || s.tokens == nil || s.wechat == nil {
		return nil, errors.New("patient authentication dependencies are not configured")
	}

	openID, err := s.wechat.Code2Session(ctx, code)
	if err != nil {
		// port 层已区分「code 不可用」与「微信不可用」，这里只做用例级归类。
		if errors.Is(err, port.ErrWeChatCodeInvalid) {
			return nil, ErrInvalidCode
		}
		// 其余（含 port.ErrWeChatUnavailable）统一归为依赖不可用 → 502。
		return nil, dependencyError(err)
	}
	if openID == "" {
		return nil, patient.ErrOpenIDRequired
	}

	// 登录与注册合一：open_id 在库中没有唯一约束，
	// 同一 openId 的并发请求由 repository 在事务内串行化（契约 §7.1）。
	profile, isNewUser, err := s.patients.FindOrCreatePatientByOpenID(
		ctx,
		openID,
		time.Now(),
	)
	if err != nil {
		return nil, dependencyError(err)
	}
	if profile == nil {
		return nil, patient.ErrPatientNotFound
	}
	// 禁用账号不得签发令牌。
	if !profile.IsActive() {
		return nil, ErrPatientDisabled
	}

	pair, sessionID, err := s.issueTokens(profile)
	if err != nil {
		return nil, err
	}
	if err := s.saveRefreshSession(ctx, pair, sessionID, profile); err != nil {
		return nil, dependencyError(err)
	}

	card, err := s.loadCard(ctx, profile.ID)
	if err != nil {
		return nil, dependencyError(err)
	}

	return &LoginResult{
		IsNewUser: isNewUser,
		Patient:   profile,
		Card:      card,
		Tokens:    pair,
	}, nil
}

// Refresh 处理 POST /api/v1/patient/auth/refresh：
// 轮换 refresh 会话并签发新的 access 令牌。
//
// 其他 realm 的 refresh 令牌（例如管理端）一律按无效处理，
// 不会在本域被解释，也不会被撤销（spec/04-api-contract.md §1.2）。
func (s *Service) Refresh(
	ctx context.Context,
	refreshToken string,
) (authsession.TokenPair, error) {
	if s.patients == nil || s.session == nil ||
		strings.TrimSpace(refreshToken) == "" {
		return authsession.TokenPair{}, ErrInvalidRefreshToken
	}

	// 轮换：旧 refresh token 立即失效，并校验会话 realm=patient。
	session, err := s.session.RotateRefreshSession(
		ctx,
		refreshToken,
		domainauth.RealmPatient,
	)
	if err != nil {
		if errors.Is(err, authsession.ErrInvalidToken) {
			return authsession.TokenPair{}, ErrInvalidRefreshToken
		}
		return authsession.TokenPair{}, dependencyError(err)
	}

	profile, err := s.patients.FindPatientByID(ctx, session.UserID)
	switch {
	case errors.Is(err, patient.ErrPatientNotFound), err == nil && profile == nil:
		// 账号已被删除或读取不到时按刷新失败处理：客户端回到静默登录。
		return authsession.TokenPair{}, ErrInvalidRefreshToken
	case err != nil:
		// 依赖故障必须原样上报（502），不能伪装成「刷新令牌无效」。
		return authsession.TokenPair{}, dependencyError(err)
	}
	if !profile.IsActive() {
		// 账号已被禁用：旧 refresh 会话已随上面的轮换被撤销，且不再写入新会话，
		// 因此禁用账号会立即失去续期能力（403 AUTH_FORBIDDEN 在 refresh 上同样成立）。
		return authsession.TokenPair{}, ErrPatientDisabled
	}

	pair, sessionID, err := s.issueTokens(profile)
	if err != nil {
		return authsession.TokenPair{}, err
	}
	if err := s.saveRefreshSession(ctx, pair, sessionID, profile); err != nil {
		return authsession.TokenPair{}, dependencyError(err)
	}

	return pair, nil
}

// Logout 处理 POST /api/v1/patient/auth/logout：撤销当前患者会话，幂等。
//
// 严格语义（spec/04-api-contract.md §7.2 与 §12.5）：必须携带仍可解析的
// 患者 access token；缺失、无效、过期或 realm 不匹配一律返回
// ErrInvalidAccessToken，且该分支不产生任何副作用。
// refresh 令牌只用于确定撤销范围（可来自 Cookie 或请求体），允许缺省。
func (s *Service) Logout(
	ctx context.Context,
	accessToken string,
	refreshToken string,
) error {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" || s.session == nil || s.tokens == nil {
		return ErrInvalidAccessToken
	}

	// 先校验再动会话状态：校验失败时不得留下任何副作用。
	if _, err := s.session.ParseAccessToken(
		accessToken,
		domainauth.RealmPatient,
	); err != nil {
		return ErrInvalidAccessToken
	}

	if err := s.session.Logout(
		ctx,
		accessToken,
		refreshToken,
		domainauth.RealmPatient,
	); err != nil {
		if errors.Is(err, authsession.ErrInvalidToken) {
			return ErrInvalidAccessToken
		}
		return dependencyError(err)
	}

	return nil
}

// Me 组装当前患者摘要（spec/04-api-contract.md §7.3）。
//
// cardId 为 null 表示尚未实名建卡，前端据此跳转实名流程；cardCount 只会是 0 或 1；
// tel 取本人就诊卡的明文，无卡时为 null。
func (s *Service) Me(
	ctx context.Context,
	patientID int64,
) (*patient.PatientMe, error) {
	if s.patients == nil || patientID <= 0 {
		return nil, patient.ErrPatientNotFound
	}

	profile, err := s.patients.FindPatientByID(ctx, patientID)
	if err != nil {
		if errors.Is(err, patient.ErrPatientNotFound) {
			return nil, patient.ErrPatientNotFound
		}
		return nil, dependencyError(err)
	}
	if profile == nil {
		return nil, patient.ErrPatientNotFound
	}

	card, err := s.loadCard(ctx, patientID)
	if err != nil {
		return nil, dependencyError(err)
	}

	me := &patient.PatientMe{
		ID:         profile.ID,
		Nickname:   profile.Nickname,
		Photo:      profile.Photo,
		Sex:        profile.Sex,
		Status:     profile.Status,
		CreateDate: profile.CreateDate,
	}
	if card != nil {
		cardID := card.ID
		me.CardID = &cardID
		me.CardCount = 1
		if card.Tel != "" {
			tel := card.Tel
			me.Tel = &tel
		}
	}

	return me, nil
}

// ParseAccessToken 供访问令牌中间件校验 realm=patient 的令牌。
func (s *Service) ParseAccessToken(
	raw string,
	expectedRealm domainauth.Realm,
) (*authsession.AccessClaims, error) {
	return s.session.ParseAccessToken(raw, expectedRealm)
}

// IsRevoked 供访问令牌中间件查询令牌是否已被撤销。
func (s *Service) IsRevoked(
	ctx context.Context,
	jti string,
) (bool, error) {
	return s.session.IsRevoked(ctx, jti)
}

// issueTokens 按患者主体签发令牌对，realm 固定为 patient。
func (s *Service) issueTokens(
	profile *patient.Patient,
) (authsession.TokenPair, string, error) {
	return s.session.Issue(domainauth.RealmPatient, authsession.Subject{
		ID:   profile.ID,
		Name: patientDisplayName(profile),
	})
}

// saveRefreshSession 写入患者域的 refresh 会话。
func (s *Service) saveRefreshSession(
	ctx context.Context,
	pair authsession.TokenPair,
	sessionID string,
	profile *patient.Patient,
) error {
	return s.session.SaveRefreshSession(
		ctx,
		pair,
		sessionID,
		authsession.Subject{
			ID:   profile.ID,
			Name: patientDisplayName(profile),
		},
		domainauth.RealmPatient,
	)
}

// loadCard 读取患者名下唯一的就诊卡；没有卡时返回 (nil, nil) 表示尚未实名建卡。
func (s *Service) loadCard(
	ctx context.Context,
	patientID int64,
) (*patient.Card, error) {
	card, err := s.patients.FindCardByPatientID(ctx, patientID)
	switch {
	case errors.Is(err, patient.ErrCardNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return card, nil
}

// patientDisplayName 返回写入 refresh 会话的展示名：优先昵称，缺失时留空。
// 不使用 openId：它属于只能服务端保存的登录凭据（spec §2.2）。
func patientDisplayName(profile *patient.Patient) string {
	if profile == nil || profile.Nickname == nil {
		return ""
	}
	return *profile.Nickname
}
