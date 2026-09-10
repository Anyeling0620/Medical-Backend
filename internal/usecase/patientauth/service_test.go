// 患者端认证用例单测：微信登录（登录与注册合一）、refresh 轮换、
// 严格语义的 logout 与当前患者摘要（spec/04-api-contract.md §7.1-§7.3）。
//
// 全部用例使用内存桩替换 port.PatientRepository / port.TokenRepository /
// port.WeChatAuthenticator，不依赖 PostgreSQL、Redis 与微信网络。
package patientauth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/domain/patient"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/usecase/authsession"
)

// patientAuthTestSecret 是本文件所有用例共用的 JWT 签名密钥。
const patientAuthTestSecret = "patientauth-unit-test-secret"

// patientAuthTestOpenID 是微信桩默认返回的 openid（仅服务端内部流转）。
const patientAuthTestOpenID = "openid-unit-test"

// patientAuthTestTokenRepo 是内存版 port.TokenRepository 桩：
// refresh 会话保存在 map 中，并记录保存/删除/撤销动作，
// 供“禁用账号不得签发令牌”“跨域登出不得有副作用”这类否定性断言使用。
type patientAuthTestTokenRepo struct {
	port.TokenRepository

	sessions   map[string]port.RefreshSession
	saved      []port.RefreshSession
	deleted    []string
	revoked    []string
	sidDeleted []string
	sidMissed  []string
	// deletedSessionIDs 与 deletedSIDs 一一对应，记录 DeleteRefreshSession 收到的
	// sessionID：生产实现靠它一并删除反查索引，桩若丢弃该参数，
	// 「忘记传 sessionID」的回归就无法被测出。
	deletedSessionIDs []string

	// getErr 非 nil 时 GetRefreshSession 固定返回该错误，用于模拟 Redis 故障。
	getErr error
	// sidErr 非 nil 时 DeleteRefreshSessionBySessionID 固定返回该错误。
	sidErr error
}

func newPatientAuthTestTokenRepo() *patientAuthTestTokenRepo {
	return &patientAuthTestTokenRepo{sessions: map[string]port.RefreshSession{}}
}

func (r *patientAuthTestTokenRepo) SaveRefreshSession(
	_ context.Context,
	session port.RefreshSession,
	_ int64,
) error {
	r.sessions[session.TokenHash] = session
	r.saved = append(r.saved, session)
	return nil
}

func (r *patientAuthTestTokenRepo) GetRefreshSession(
	_ context.Context,
	tokenHash string,
) (*port.RefreshSession, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	session, ok := r.sessions[tokenHash]
	if !ok {
		// 与 repo.RedisTokenRepository 一致：键不存在时返回哨兵错误。
		return nil, port.ErrRefreshSessionNotFound
	}
	copied := session
	return &copied, nil
}

func (r *patientAuthTestTokenRepo) DeleteRefreshSession(
	_ context.Context,
	tokenHash string,
	sessionID string,
) error {
	r.deleted = append(r.deleted, tokenHash)
	r.deletedSessionIDs = append(r.deletedSessionIDs, sessionID)
	delete(r.sessions, tokenHash)
	return nil
}

// DeleteRefreshSessionBySessionID 按 sessionID 在内存里反查会话并删除，
// 对应生产实现里 Redis 的 sessionID -> tokenHash 反查索引：
// 命中返回 (true, nil)，未命中返回 (false, nil)（登出保持幂等）。
func (r *patientAuthTestTokenRepo) DeleteRefreshSessionBySessionID(
	_ context.Context,
	sessionID string,
) (bool, error) {
	if r.sidErr != nil {
		return false, r.sidErr
	}
	if sessionID == "" {
		return false, nil
	}
	for tokenHash, session := range r.sessions {
		if session.SessionID != sessionID {
			continue
		}
		r.deleted = append(r.deleted, tokenHash)
		r.sidDeleted = append(r.sidDeleted, sessionID)
		delete(r.sessions, tokenHash)
		return true, nil
	}
	r.sidMissed = append(r.sidMissed, sessionID)
	return false, nil
}

func (r *patientAuthTestTokenRepo) RevokeAccessToken(
	_ context.Context,
	jti string,
	_ int64,
) error {
	r.revoked = append(r.revoked, jti)
	return nil
}

func (r *patientAuthTestTokenRepo) IsAccessTokenRevoked(
	_ context.Context,
	_ string,
) (bool, error) {
	return false, nil
}

// patientAuthTestPatientRepo 是内存版 port.PatientRepository 桩。
// 未用到的能力沿用内嵌接口（一旦被调用会 panic，便于发现用例越界）。
type patientAuthTestPatientRepo struct {
	port.PatientRepository

	patients   map[int64]*patient.Patient
	openIDToID map[string]int64
	cards      map[int64]*patient.Card
	nextID     int64

	findPatientErr  error
	findCardErr     error
	findOrCreateErr error
}

func newPatientAuthTestPatientRepo() *patientAuthTestPatientRepo {
	return &patientAuthTestPatientRepo{
		patients:   map[int64]*patient.Patient{},
		openIDToID: map[string]int64{},
		cards:      map[int64]*patient.Card{},
		nextID:     20,
	}
}

func (r *patientAuthTestPatientRepo) FindPatientByID(
	_ context.Context,
	patientID int64,
) (*patient.Patient, error) {
	if r.findPatientErr != nil {
		return nil, r.findPatientErr
	}
	profile, ok := r.patients[patientID]
	if !ok {
		return nil, patient.ErrPatientNotFound
	}
	copied := *profile
	return &copied, nil
}

func (r *patientAuthTestPatientRepo) FindOrCreatePatientByOpenID(
	_ context.Context,
	openID string,
	now time.Time,
) (*patient.Patient, bool, error) {
	if openID == "" {
		return nil, false, patient.ErrOpenIDRequired
	}
	if r.findOrCreateErr != nil {
		return nil, false, r.findOrCreateErr
	}
	if id, ok := r.openIDToID[openID]; ok {
		profile, err := r.FindPatientByID(context.Background(), id)
		return profile, false, err
	}

	id := r.nextID
	r.nextID++
	profile := &patient.Patient{
		ID:         id,
		OpenID:     openID,
		Status:     patient.StatusActive,
		CreateDate: now.Format("2006-01-02"),
	}
	r.patients[id] = profile
	r.openIDToID[openID] = id

	copied := *profile
	return &copied, true, nil
}

func (r *patientAuthTestPatientRepo) FindCardByPatientID(
	_ context.Context,
	patientID int64,
) (*patient.Card, error) {
	if r.findCardErr != nil {
		return nil, r.findCardErr
	}
	card, ok := r.cards[patientID]
	if !ok {
		return nil, patient.ErrCardNotFound
	}
	copied := *card
	return &copied, nil
}

// seedPatient 预置一个患者账号（status 由调用方指定）。
func (r *patientAuthTestPatientRepo) seedPatient(id int64, status string) *patient.Patient {
	nickname := "小明"
	profile := &patient.Patient{
		ID:         id,
		OpenID:     patientAuthTestOpenID,
		Nickname:   &nickname,
		Status:     status,
		CreateDate: "2026-09-08",
	}
	r.patients[id] = profile
	r.openIDToID[patientAuthTestOpenID] = id
	return profile
}

// seedCard 为指定账号预置一张就诊卡。
func (r *patientAuthTestPatientRepo) seedCard(patientID int64, tel string) *patient.Card {
	card := &patient.Card{
		ID:             10,
		UserID:         patientID,
		UUID:           "CARD0000000000000000000000000010",
		Name:           "张三",
		Sex:            "男",
		PID:            "110101199001011237",
		Tel:            tel,
		Birthday:       "1990-01-01",
		MedicalHistory: []string{"无"},
		InsuranceType:  "无",
	}
	r.cards[patientID] = card
	return card
}

// patientAuthTestWeChat 是 port.WeChatAuthenticator 桩。
type patientAuthTestWeChat struct {
	openID string
	err    error
	calls  []string
}

func (w *patientAuthTestWeChat) Code2Session(
	_ context.Context,
	code string,
) (string, error) {
	w.calls = append(w.calls, code)
	return w.openID, w.err
}

// patientAuthTestEnv 汇总一次用例需要的桩与 use case。
type patientAuthTestEnv struct {
	service *Service
	repo    *patientAuthTestPatientRepo
	tokens  *patientAuthTestTokenRepo
	wechat  *patientAuthTestWeChat
}

func newPatientAuthTestEnv(t *testing.T) *patientAuthTestEnv {
	t.Helper()
	repo := newPatientAuthTestPatientRepo()
	tokens := newPatientAuthTestTokenRepo()
	wechat := &patientAuthTestWeChat{openID: patientAuthTestOpenID}

	return &patientAuthTestEnv{
		service: NewService(repo, tokens, wechat, Config{
			JWTSecret:  patientAuthTestSecret,
			AccessTTL:  15 * time.Minute,
			RefreshTTL: 24 * time.Hour,
		}),
		repo:   repo,
		tokens: tokens,
		wechat: wechat,
	}
}

// misAccessToken 用同一签名密钥签发 realm=mis 的 access token，
// 用于验证患者域绝不解释其他认证域的令牌。
func (e *patientAuthTestEnv) misAccessToken(t *testing.T) string {
	t.Helper()
	manager := authsession.NewManager(e.tokens, authsession.Config{
		JWTSecret:  patientAuthTestSecret,
		AccessTTL:  15 * time.Minute,
		RefreshTTL: time.Hour,
	})
	pair, _, err := manager.Issue(
		domainauth.RealmMis,
		authsession.Subject{ID: 7, Name: "mis-admin"},
	)
	if err != nil {
		t.Fatalf("签发管理端测试令牌失败：%v", err)
	}
	return pair.AccessToken
}

// expiredPatientAccessToken 用同一签名密钥签发一枚「签名正确但已过期」的 patient
// access token。authsession.Issue 不接受非正 TTL，因此这里直接构造 JWT，
// 用于覆盖严格语义里「过期令牌」这一类输入。
func (e *patientAuthTestEnv) expiredPatientAccessToken(t *testing.T) string {
	t.Helper()
	now := time.Now()
	claims := authsession.AccessClaims{
		UserID:    20,
		Username:  "小明",
		TokenType: authsession.TokenTypeAccess,
		Realm:     domainauth.RealmPatient,
		SessionID: "session-expired",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "medical-backend",
			Subject:   "20",
			ID:        "expired-patient-jti",
			IssuedAt:  jwt.NewNumericDate(now.Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(now.Add(-time.Hour)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(patientAuthTestSecret))
	if err != nil {
		t.Fatalf("签发过期测试令牌失败：%v", err)
	}
	return token
}

// tamperJWTSignature 改动 JWT 签名段的第一个字符，模拟被篡改的令牌。
// 不能改最后一个字符：base64url 的末位可能只承载未使用的填充位，
// 改动后解码出的签名字节不变，令牌仍会通过校验（本用例曾因此假通过）。
func tamperJWTSignature(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[2] == "" {
		return token
	}
	signature := []byte(parts[2])
	if signature[0] == 'A' {
		signature[0] = 'B'
	} else {
		signature[0] = 'A'
	}
	parts[2] = string(signature)
	return strings.Join(parts, ".")
}

// assertNoLogoutSideEffects 断言登出失败分支零副作用：
// 不撤销任何 jti、不删除任何 refresh 会话，也不触发按 sid 的反查/删除。
func assertNoLogoutSideEffects(t *testing.T, env *patientAuthTestEnv) {
	t.Helper()
	if len(env.tokens.revoked) != 0 {
		t.Errorf("不得撤销任何 jti，实际 %v", env.tokens.revoked)
	}
	if len(env.tokens.deleted) != 0 {
		t.Errorf("不得删除任何 refresh 会话，实际 %v", env.tokens.deleted)
	}
	if len(env.tokens.sidDeleted) != 0 || len(env.tokens.sidMissed) != 0 {
		t.Errorf(
			"不得按 sid 反查/删除会话，命中=%v 未命中=%v",
			env.tokens.sidDeleted,
			env.tokens.sidMissed,
		)
	}
}

// seedPatientSession 预置一条属于指定 realm 的 refresh 会话，返回 refresh token 原文。
func (e *patientAuthTestEnv) seedPatientSession(
	realm domainauth.Realm,
	patientID int64,
	refreshToken string,
) {
	e.tokens.sessions[authsession.HashRefreshToken(refreshToken)] = port.RefreshSession{
		TokenHash: authsession.HashRefreshToken(refreshToken),
		SessionID: "session-" + string(realm),
		UserID:    patientID,
		Username:  "小明",
		Realm:     realm,
	}
}

// containsString 判断切片中是否包含目标值。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// --- 微信登录 ---

// TestWeChatLoginFirstRegistrationIsNewUser 覆盖首次注册：响应 isNewUser=true，
// access token 与 refresh 会话都必须带 realm=patient。
func TestWeChatLoginFirstRegistrationIsNewUser(t *testing.T) {
	env := newPatientAuthTestEnv(t)

	result, err := env.service.WeChatLogin(context.Background(), "wx_code_abc123")
	if err != nil {
		t.Fatalf("首次登录应成功，实际 err=%v", err)
	}

	if !result.IsNewUser {
		t.Error("首次注册 isNewUser 应为 true")
	}
	if result.Patient == nil || result.Patient.ID != 20 {
		t.Fatalf("患者主体 = %+v，期望 ID=20", result.Patient)
	}
	if len(env.wechat.calls) != 1 || env.wechat.calls[0] != "wx_code_abc123" {
		t.Errorf("code2Session 调用记录 = %v，期望 [wx_code_abc123]", env.wechat.calls)
	}

	// access token 必须携带 realm=patient，且不得在管理域被解释。
	claims, err := env.service.ParseAccessToken(
		result.Tokens.AccessToken,
		domainauth.RealmPatient,
	)
	if err != nil {
		t.Fatalf("患者 access token 应可解析，实际 err=%v", err)
	}
	if claims.Realm != domainauth.RealmPatient {
		t.Errorf("access token realm = %q，期望 patient", claims.Realm)
	}
	if _, err := env.service.ParseAccessToken(
		result.Tokens.AccessToken,
		domainauth.RealmMis,
	); !errors.Is(err, authsession.ErrInvalidToken) {
		t.Errorf("患者令牌在管理域应被拒绝，实际 err=%v", err)
	}

	// refresh 会话必须写入，且 realm=patient、摘要对应本次下发的 refresh token。
	if len(env.tokens.saved) != 1 {
		t.Fatalf("refresh 会话写入次数 = %d，期望 1", len(env.tokens.saved))
	}
	saved := env.tokens.saved[0]
	if saved.Realm != domainauth.RealmPatient {
		t.Errorf("refresh 会话 realm = %q，期望 patient", saved.Realm)
	}
	if saved.TokenHash != authsession.HashRefreshToken(result.Tokens.RefreshToken) {
		t.Error("refresh 会话摘要必须对应本次下发的 refresh token")
	}
	if saved.UserID != 20 {
		t.Errorf("refresh 会话 userId = %d，期望 20", saved.UserID)
	}
	if result.Tokens.AccessToken == "" || result.Tokens.RefreshToken == "" {
		t.Error("登录成功必须同时下发 access token 与 refresh token")
	}
}

// TestWeChatLoginExistingAccountIsNotNewUser 覆盖已注册账号再次登录。
func TestWeChatLoginExistingAccountIsNotNewUser(t *testing.T) {
	env := newPatientAuthTestEnv(t)
	env.repo.seedPatient(20, patient.StatusActive)

	result, err := env.service.WeChatLogin(context.Background(), "wx_code_existing")
	if err != nil {
		t.Fatalf("已注册账号登录应成功，实际 err=%v", err)
	}

	if result.IsNewUser {
		t.Error("已存在账号 isNewUser 应为 false")
	}
	if result.Patient == nil || result.Patient.ID != 20 {
		t.Fatalf("患者主体 = %+v，期望 ID=20", result.Patient)
	}
}

// TestWeChatLoginCardProjection 覆盖有卡/无卡两种登录响应投影。
func TestWeChatLoginCardProjection(t *testing.T) {
	cases := []struct {
		name     string
		withCard bool
	}{
		{name: "有卡时返回就诊卡", withCard: true},
		{name: "无卡时 Card 为 nil", withCard: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientAuthTestEnv(t)
			if tc.withCard {
				env.repo.seedCard(20, "13800138000")
			}

			result, err := env.service.WeChatLogin(context.Background(), "wx_code_card")
			if err != nil {
				t.Fatalf("登录应成功，实际 err=%v", err)
			}

			if !tc.withCard {
				if result.Card != nil {
					t.Errorf("无卡时 Card 应为 nil，实际 %+v", result.Card)
				}
				return
			}
			if result.Card == nil {
				t.Fatal("有卡时 Card 不应为 nil")
			}
			if result.Card.ID != 10 {
				t.Errorf("Card.ID = %d，期望 10", result.Card.ID)
			}
		})
	}
}

// TestWeChatLoginRejectsInvalidCodeLocally 覆盖 code 长度校验：
// 本地校验失败时不得调用微信适配器。
func TestWeChatLoginRejectsInvalidCodeLocally(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{name: "空 code", code: ""},
		{name: "仅空白 code", code: "   "},
		{name: "超过 128 字符", code: strings.Repeat("a", 129)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientAuthTestEnv(t)

			_, err := env.service.WeChatLogin(context.Background(), tc.code)
			if !errors.Is(err, ErrInvalidCode) {
				t.Fatalf("err = %v，期望 ErrInvalidCode", err)
			}
			if len(env.wechat.calls) != 0 {
				t.Errorf("本地校验失败不得调用微信适配器，实际调用 %v", env.wechat.calls)
			}
			if len(env.tokens.saved) != 0 {
				t.Error("code 非法时不得写入 refresh 会话")
			}
		})
	}
}

// TestWeChatLoginMapsWeChatErrors 覆盖适配器错误的用例级归类：
// code 不可用归为 ErrInvalidCode；微信不可用归为 ErrDependencyUnavailable
// （handler 据此映射 502 DEPENDENCY_UNAVAILABLE）；
// openid 为空归为 patient.ErrOpenIDRequired。
func TestWeChatLoginMapsWeChatErrors(t *testing.T) {
	unavailable := errors.New("wechat gateway down")

	cases := []struct {
		name      string
		wechatErr error
		wechatID  string
		wantErr   error
	}{
		{
			name:      "微信判定 code 不可用",
			wechatErr: port.ErrWeChatCodeInvalid,
			wantErr:   ErrInvalidCode,
		},
		{
			name:      "微信服务不可用归为依赖不可用",
			wechatErr: unavailable,
			wantErr:   ErrDependencyUnavailable,
		},
		{
			name:      "适配器返回依赖不可用哨兵",
			wechatErr: port.ErrWeChatUnavailable,
			wantErr:   ErrDependencyUnavailable,
		},
		{
			name:     "适配器返回空 openid",
			wechatID: "",
			wantErr:  patient.ErrOpenIDRequired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientAuthTestEnv(t)
			env.wechat.openID = tc.wechatID
			env.wechat.err = tc.wechatErr

			_, err := env.service.WeChatLogin(context.Background(), "wx_code_error")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if len(env.tokens.saved) != 0 {
				t.Error("登录失败不得写入 refresh 会话")
			}
		})
	}
}

// TestWeChatLoginDependencyFailure 断言微信不可用时返回 ErrDependencyUnavailable
// （handler 据此映射 502 DEPENDENCY_UNAVAILABLE），且不得被归类为凭据类错误。
func TestWeChatLoginDependencyFailure(t *testing.T) {
	env := newPatientAuthTestEnv(t)
	env.wechat.err = port.ErrWeChatUnavailable

	_, err := env.service.WeChatLogin(context.Background(), "wx_code_unavailable")
	if !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("err = %v，期望 ErrDependencyUnavailable", err)
	}
	if errors.Is(err, ErrInvalidCode) {
		t.Error("微信不可用不得归类为 code 非法")
	}
	if errors.Is(err, ErrPatientDisabled) || errors.Is(err, ErrInvalidRefreshToken) {
		t.Error("依赖故障不得被归类为账号/凭据类错误")
	}
	if len(env.tokens.saved) != 0 {
		t.Error("依赖故障时不得写入 refresh 会话")
	}
}

// TestWeChatLoginDependencyErrorPreservesCauseSentinel 断言 dependencyError 同时保留
// 「依赖不可用」与根因两个哨兵：errors.Is 对 ErrDependencyUnavailable（→502）与
// port.ErrWeChatUnavailable 都能命中，错误文本也同时包含两层信息，日志不丢原始错误。
func TestWeChatLoginDependencyErrorPreservesCauseSentinel(t *testing.T) {
	env := newPatientAuthTestEnv(t)
	env.wechat.err = port.ErrWeChatUnavailable

	_, err := env.service.WeChatLogin(context.Background(), "wx_code_unavailable")
	if !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("err = %v，期望命中 ErrDependencyUnavailable（handler →502）", err)
	}
	if !errors.Is(err, port.ErrWeChatUnavailable) {
		t.Fatalf("err = %v，期望命中根因哨兵 port.ErrWeChatUnavailable", err)
	}
	if !strings.Contains(err.Error(), ErrDependencyUnavailable.Error()) ||
		!strings.Contains(err.Error(), port.ErrWeChatUnavailable.Error()) {
		t.Errorf("错误文本必须同时包含依赖不可用与根因信息，实际 %q", err.Error())
	}
}

// TestWeChatLoginRepositoryFailureIsDependencyUnavailable 覆盖患者仓储故障：
// 同样归为依赖不可用，而不是 500/401。
func TestWeChatLoginRepositoryFailureIsDependencyUnavailable(t *testing.T) {
	env := newPatientAuthTestEnv(t)
	cause := errors.New("postgres unavailable")
	env.repo.findOrCreateErr = cause

	_, err := env.service.WeChatLogin(context.Background(), "wx_code_db_down")
	if !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("err = %v，期望 ErrDependencyUnavailable", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("依赖故障必须保留根因，err = %v", err)
	}
	if len(env.tokens.saved) != 0 {
		t.Error("仓储故障时不得写入 refresh 会话")
	}
}

// TestWeChatLoginDisabledPatientIssuesNothing 覆盖禁用账号：
// 返回 ErrPatientDisabled，且既不签发令牌也不写入 refresh 会话。
func TestWeChatLoginDisabledPatientIssuesNothing(t *testing.T) {
	env := newPatientAuthTestEnv(t)
	env.repo.seedPatient(20, patient.StatusDisabled)

	result, err := env.service.WeChatLogin(context.Background(), "wx_code_disabled")
	if !errors.Is(err, ErrPatientDisabled) {
		t.Fatalf("err = %v，期望 ErrPatientDisabled", err)
	}
	if result != nil {
		t.Errorf("禁用账号不得返回登录结果，实际 %+v", result)
	}
	if len(env.tokens.saved) != 0 {
		t.Errorf("禁用账号不得写入 refresh 会话，实际 %+v", env.tokens.saved)
	}
	if len(env.tokens.revoked) != 0 {
		t.Error("禁用账号不得产生任何令牌撤销动作")
	}
}

// --- refresh ---

// TestRefreshRotatesSession 覆盖正常轮换：旧会话被删除、新会话 realm=patient、
// 新 access token 仍属患者域。
func TestRefreshRotatesSession(t *testing.T) {
	env := newPatientAuthTestEnv(t)
	env.repo.seedPatient(20, patient.StatusActive)

	const oldRefresh = "old-refresh-token"
	env.seedPatientSession(domainauth.RealmPatient, 20, oldRefresh)

	pair, err := env.service.Refresh(context.Background(), oldRefresh)
	if err != nil {
		t.Fatalf("refresh 应成功，实际 err=%v", err)
	}

	oldHash := authsession.HashRefreshToken(oldRefresh)
	if !containsString(env.tokens.deleted, oldHash) {
		t.Errorf("旧 refresh 会话必须被删除，实际 deleted=%v", env.tokens.deleted)
	}
	if !containsString(env.tokens.deletedSessionIDs, "session-patient") {
		t.Errorf(
			"轮换必须把会话 sessionID 传给仓储，实际 %v",
			env.tokens.deletedSessionIDs,
		)
	}
	if _, ok := env.tokens.sessions[oldHash]; ok {
		t.Error("旧 refresh 会话不应留在存储中")
	}

	if len(env.tokens.saved) != 1 {
		t.Fatalf("新 refresh 会话写入次数 = %d，期望 1", len(env.tokens.saved))
	}
	saved := env.tokens.saved[0]
	if saved.Realm != domainauth.RealmPatient {
		t.Errorf("轮换后的会话 realm = %q，期望 patient", saved.Realm)
	}
	if saved.TokenHash != authsession.HashRefreshToken(pair.RefreshToken) {
		t.Error("轮换后的会话摘要必须对应新下发的 refresh token")
	}
	if pair.RefreshToken == oldRefresh {
		t.Error("refresh token 必须轮换，不得复用旧值")
	}

	claims, err := env.service.ParseAccessToken(
		pair.AccessToken,
		domainauth.RealmPatient,
	)
	if err != nil {
		t.Fatalf("轮换后的 access token 应可解析，实际 err=%v", err)
	}
	if claims.Realm != domainauth.RealmPatient {
		t.Errorf("轮换后的 access token realm = %q，期望 patient", claims.Realm)
	}
}

// TestRefreshRejectsInvalidTokens 覆盖 refresh 失败分支：
// 会话不存在、属于 mis realm（且不得删除）、账号已删除、空令牌。
func TestRefreshRejectsInvalidTokens(t *testing.T) {
	t.Run("会话不存在", func(t *testing.T) {
		env := newPatientAuthTestEnv(t)
		env.repo.seedPatient(20, patient.StatusActive)

		_, err := env.service.Refresh(context.Background(), "unknown-refresh-token")
		if !errors.Is(err, ErrInvalidRefreshToken) {
			t.Fatalf("err = %v，期望 ErrInvalidRefreshToken", err)
		}
		if len(env.tokens.deleted) != 0 {
			t.Errorf("会话不存在时不得删除任何会话，实际 %v", env.tokens.deleted)
		}
	})

	t.Run("mis realm 的 refresh 令牌", func(t *testing.T) {
		env := newPatientAuthTestEnv(t)
		env.repo.seedPatient(7, patient.StatusActive)

		const misRefresh = "mis-refresh-token"
		env.seedPatientSession(domainauth.RealmMis, 7, misRefresh)
		misHash := authsession.HashRefreshToken(misRefresh)

		_, err := env.service.Refresh(context.Background(), misRefresh)
		if !errors.Is(err, ErrInvalidRefreshToken) {
			t.Fatalf("err = %v，期望 ErrInvalidRefreshToken", err)
		}
		if containsString(env.tokens.deleted, misHash) {
			t.Error("其他认证域的 refresh 会话不得被患者域删除")
		}
		if _, ok := env.tokens.sessions[misHash]; !ok {
			t.Error("其他认证域的 refresh 会话必须保留在存储中")
		}
		if len(env.tokens.saved) != 0 {
			t.Error("跨域 refresh 不得签发新会话")
		}
	})

	t.Run("账号已删除", func(t *testing.T) {
		env := newPatientAuthTestEnv(t)
		// 会话仍然存在，但账号已被删除（仓储返回 ErrPatientNotFound）。
		const orphanRefresh = "orphan-refresh-token"
		env.seedPatientSession(domainauth.RealmPatient, 99, orphanRefresh)

		_, err := env.service.Refresh(context.Background(), orphanRefresh)
		if !errors.Is(err, ErrInvalidRefreshToken) {
			t.Fatalf("err = %v，期望 ErrInvalidRefreshToken", err)
		}
		if len(env.tokens.saved) != 0 {
			t.Error("账号不存在时不得签发新会话")
		}
	})

	t.Run("空 refresh token", func(t *testing.T) {
		env := newPatientAuthTestEnv(t)

		_, err := env.service.Refresh(context.Background(), "   ")
		if !errors.Is(err, ErrInvalidRefreshToken) {
			t.Fatalf("err = %v，期望 ErrInvalidRefreshToken", err)
		}
	})
}

// TestRefreshRejectsDisabledPatient 覆盖 refresh 时账号被禁用：返回 ErrPatientDisabled。
func TestRefreshRejectsDisabledPatient(t *testing.T) {
	env := newPatientAuthTestEnv(t)
	env.repo.seedPatient(20, patient.StatusDisabled)

	const refresh = "disabled-refresh-token"
	env.seedPatientSession(domainauth.RealmPatient, 20, refresh)

	_, err := env.service.Refresh(context.Background(), refresh)
	if !errors.Is(err, ErrPatientDisabled) {
		t.Fatalf("err = %v，期望 ErrPatientDisabled", err)
	}
	if len(env.tokens.saved) != 0 {
		t.Error("禁用账号不得签发新会话")
	}
}

// --- logout ---

// TestLogoutRequiresPatientAccessToken 覆盖严格语义：缺失或 realm 不匹配的
// access token 一律返回 ErrInvalidAccessToken，且不得产生任何副作用。
func TestLogoutRequiresPatientAccessToken(t *testing.T) {
	t.Run("无 access token", func(t *testing.T) {
		env := newPatientAuthTestEnv(t)

		err := env.service.Logout(context.Background(), "  ", "any-refresh-token")
		if !errors.Is(err, ErrInvalidAccessToken) {
			t.Fatalf("err = %v，期望 ErrInvalidAccessToken", err)
		}
		if len(env.tokens.deleted) != 0 || len(env.tokens.revoked) != 0 {
			t.Errorf("无令牌登出不得产生副作用，deleted=%v revoked=%v", env.tokens.deleted, env.tokens.revoked)
		}
	})

	t.Run("mis realm 的 access token 无副作用", func(t *testing.T) {
		env := newPatientAuthTestEnv(t)
		env.repo.seedPatient(20, patient.StatusActive)

		// 患者域仍在使用中的 refresh 会话必须原样保留。
		const patientRefresh = "patient-refresh-token"
		env.seedPatientSession(domainauth.RealmPatient, 20, patientRefresh)
		patientHash := authsession.HashRefreshToken(patientRefresh)

		err := env.service.Logout(
			context.Background(),
			env.misAccessToken(t),
			patientRefresh,
		)
		if !errors.Is(err, ErrInvalidAccessToken) {
			t.Fatalf("err = %v，期望 ErrInvalidAccessToken", err)
		}
		if len(env.tokens.revoked) != 0 {
			t.Errorf("跨域登出不得撤销任何 jti，实际 %v", env.tokens.revoked)
		}
		if len(env.tokens.deleted) != 0 {
			t.Errorf("跨域登出不得删除 refresh 会话，实际 %v", env.tokens.deleted)
		}
		if _, ok := env.tokens.sessions[patientHash]; !ok {
			t.Error("患者域 refresh 会话应仍保留在存储中")
		}
		// 跨域令牌连解析都不通过，不得触发按 sid 的会话反查/删除。
		if len(env.tokens.sidDeleted) != 0 || len(env.tokens.sidMissed) != 0 {
			t.Errorf(
				"跨域登出不得调用 DeleteRefreshSessionBySessionID，命中=%v 未命中=%v",
				env.tokens.sidDeleted,
				env.tokens.sidMissed,
			)
		}
	})

	t.Run("签名被篡改的 access token", func(t *testing.T) {
		env := newPatientAuthTestEnv(t)
		env.repo.seedPatient(20, patient.StatusActive)
		login, err := env.service.WeChatLogin(context.Background(), "wx_code_tampered")
		if err != nil {
			t.Fatalf("登录应成功，实际 err=%v", err)
		}

		// 同域 refresh token 也不得因此被撤销：校验失败必须先于任何会话写操作。
		err = env.service.Logout(
			context.Background(),
			tamperJWTSignature(login.Tokens.AccessToken),
			login.Tokens.RefreshToken,
		)
		if !errors.Is(err, ErrInvalidAccessToken) {
			t.Fatalf("err = %v，期望 ErrInvalidAccessToken", err)
		}
		assertNoLogoutSideEffects(t, env)
		tamperedRefreshHash := authsession.HashRefreshToken(login.Tokens.RefreshToken)
		if _, ok := env.tokens.sessions[tamperedRefreshHash]; !ok {
			t.Error("签名无效的登出不得删除 refresh 会话")
		}
	})

	t.Run("已过期的 access token", func(t *testing.T) {
		env := newPatientAuthTestEnv(t)
		env.repo.seedPatient(20, patient.StatusActive)
		const patientRefresh = "patient-refresh-token"
		env.seedPatientSession(domainauth.RealmPatient, 20, patientRefresh)

		err := env.service.Logout(
			context.Background(),
			env.expiredPatientAccessToken(t),
			patientRefresh,
		)
		if !errors.Is(err, ErrInvalidAccessToken) {
			t.Fatalf("err = %v，期望 ErrInvalidAccessToken", err)
		}
		assertNoLogoutSideEffects(t, env)
		expiredRefreshHash := authsession.HashRefreshToken(patientRefresh)
		if _, ok := env.tokens.sessions[expiredRefreshHash]; !ok {
			t.Error("过期 access token 的登出不得删除 refresh 会话")
		}
	})
}

// TestLogoutRevokesAccessTokenAndRefreshSession 覆盖成功登出：
// 撤销 access token 的 jti 并撤销同域 refresh 会话。
func TestLogoutRevokesAccessTokenAndRefreshSession(t *testing.T) {
	env := newPatientAuthTestEnv(t)

	login, err := env.service.WeChatLogin(context.Background(), "wx_code_logout")
	if err != nil {
		t.Fatalf("登录应成功，实际 err=%v", err)
	}
	claims, err := env.service.ParseAccessToken(
		login.Tokens.AccessToken,
		domainauth.RealmPatient,
	)
	if err != nil {
		t.Fatalf("解析患者 access token 失败：%v", err)
	}

	if err := env.service.Logout(
		context.Background(),
		login.Tokens.AccessToken,
		login.Tokens.RefreshToken,
	); err != nil {
		t.Fatalf("登出应成功，实际 err=%v", err)
	}

	if !containsString(env.tokens.revoked, claims.ID) {
		t.Errorf("必须撤销 access token 的 jti %q，实际 revoked=%v", claims.ID, env.tokens.revoked)
	}
	refreshHash := authsession.HashRefreshToken(login.Tokens.RefreshToken)
	if !containsString(env.tokens.deleted, refreshHash) {
		t.Errorf("必须撤销同域 refresh 会话，实际 deleted=%v", env.tokens.deleted)
	}
	// 本用例同时提交了 refresh token，会话已按摘要撤销；随后按 sid 反查应记未命中
	// （只带 access token 的路径由 TestLogoutWithOnlyAccessTokenRevokesSessionByID 覆盖）。
	if !containsString(env.tokens.sidMissed, claims.SessionID) {
		t.Errorf(
			"按 sid 反查应记未命中（会话已按摘要撤销），实际 命中=%v 未命中=%v",
			env.tokens.sidDeleted,
			env.tokens.sidMissed,
		)
	}
}

// TestLogoutIsIdempotent 覆盖同一 access token 重复登出仍成功（幂等）。
func TestLogoutIsIdempotent(t *testing.T) {
	env := newPatientAuthTestEnv(t)

	login, err := env.service.WeChatLogin(context.Background(), "wx_code_repeat")
	if err != nil {
		t.Fatalf("登录应成功，实际 err=%v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if err := env.service.Logout(
			context.Background(),
			login.Tokens.AccessToken,
			login.Tokens.RefreshToken,
		); err != nil {
			t.Fatalf("第 %d 次登出应成功（幂等），实际 err=%v", attempt, err)
		}
	}
}

// --- me ---

// TestMeWithCard 覆盖已实名建卡：cardId 有值、cardCount=1、tel 明文。
func TestMeWithCard(t *testing.T) {
	env := newPatientAuthTestEnv(t)
	env.repo.seedPatient(20, patient.StatusActive)
	env.repo.seedCard(20, "13800138000")

	me, err := env.service.Me(context.Background(), 20)
	if err != nil {
		t.Fatalf("me 应成功，实际 err=%v", err)
	}

	if me.ID != 20 {
		t.Errorf("me.ID = %d，期望 20", me.ID)
	}
	if me.CardID == nil || *me.CardID != 10 {
		t.Errorf("me.CardID = %v，期望 10", me.CardID)
	}
	if me.CardCount != 1 {
		t.Errorf("me.CardCount = %d，期望 1", me.CardCount)
	}
	if me.Tel == nil || *me.Tel != "13800138000" {
		t.Errorf("me.Tel = %v，期望明文 13800138000", me.Tel)
	}
	if me.Status != patient.StatusActive {
		t.Errorf("me.Status = %q，期望 ACTIVE", me.Status)
	}
}

// TestMeWithoutCard 覆盖尚未实名建卡：cardId=null、cardCount=0、tel=null。
func TestMeWithoutCard(t *testing.T) {
	env := newPatientAuthTestEnv(t)
	env.repo.seedPatient(20, patient.StatusActive)

	me, err := env.service.Me(context.Background(), 20)
	if err != nil {
		t.Fatalf("me 应成功，实际 err=%v", err)
	}

	if me.CardID != nil {
		t.Errorf("无卡时 cardId 必须为 null，实际 %v", *me.CardID)
	}
	if me.CardCount != 0 {
		t.Errorf("无卡时 cardCount = %d，期望 0", me.CardCount)
	}
	if me.Tel != nil {
		t.Errorf("无卡时 tel 必须为 null，实际 %v", *me.Tel)
	}
}

// TestMePatientNotFound 覆盖患者不存在：返回 patient.ErrPatientNotFound。
func TestMePatientNotFound(t *testing.T) {
	env := newPatientAuthTestEnv(t)

	if _, err := env.service.Me(context.Background(), 99); !errors.Is(
		err,
		patient.ErrPatientNotFound,
	) {
		t.Fatalf("err = %v，期望 patient.ErrPatientNotFound", err)
	}
}

// TestRefreshDependencyFailureIsReported 覆盖 refresh 会话读取失败（Redis 不可用）：
// 必须归类为 ErrDependencyUnavailable（handler → 502 DEPENDENCY_UNAVAILABLE），
// 不得伪装成 401 AUTH_INVALID_REFRESH_TOKEN（契约 §10）。
func TestRefreshDependencyFailureIsReported(t *testing.T) {
	env := newPatientAuthTestEnv(t)
	env.repo.seedPatient(20, patient.StatusActive)
	cause := errors.New("redis unavailable")
	env.tokens.getErr = cause

	const refresh = "any-refresh-token"
	env.seedPatientSession(domainauth.RealmPatient, 20, refresh)

	_, err := env.service.Refresh(context.Background(), refresh)
	if !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("err = %v，期望 ErrDependencyUnavailable", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("依赖故障必须保留根因，err = %v", err)
	}
	if errors.Is(err, ErrInvalidRefreshToken) {
		t.Error("依赖故障不得被归类为 refresh 令牌无效")
	}
	if errors.Is(err, ErrPatientDisabled) {
		t.Error("依赖故障不得被解释为账号禁用")
	}
	if len(env.tokens.saved) != 0 {
		t.Error("依赖故障不得签发新会话")
	}
}

// TestRefreshPatientLookupDependencyFailure 覆盖轮换成功但账号读取失败：
// 同样归为依赖故障，而不是「refresh 令牌无效」（两者对客户端的意义完全不同）。
func TestRefreshPatientLookupDependencyFailure(t *testing.T) {
	env := newPatientAuthTestEnv(t)
	const refresh = "refresh-lookup-failure"
	env.seedPatientSession(domainauth.RealmPatient, 20, refresh)
	cause := errors.New("postgres unavailable")
	env.repo.findPatientErr = cause

	_, err := env.service.Refresh(context.Background(), refresh)
	if !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("err = %v，期望 ErrDependencyUnavailable", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("依赖故障必须保留根因，err = %v", err)
	}
	if errors.Is(err, ErrInvalidRefreshToken) {
		t.Error("依赖故障不得被归类为 refresh 令牌无效")
	}
}

// TestLogoutWithOnlyAccessTokenRevokesSessionByID 覆盖「只携带合法 patient access token」
// 登出：服务端必须按 claims.sid 撤销本次登录的 refresh 会话（契约 §7.2），
// 且重复登出仍幂等（会话已不存在时按未命中处理）。
func TestLogoutWithOnlyAccessTokenRevokesSessionByID(t *testing.T) {
	env := newPatientAuthTestEnv(t)

	login, err := env.service.WeChatLogin(context.Background(), "wx_code_sid_only")
	if err != nil {
		t.Fatalf("登录应成功，实际 err=%v", err)
	}
	claims, err := env.service.ParseAccessToken(
		login.Tokens.AccessToken,
		domainauth.RealmPatient,
	)
	if err != nil {
		t.Fatalf("解析患者 access token 失败：%v", err)
	}
	if claims.SessionID == "" {
		t.Fatal("access token 必须携带 sid")
	}
	if len(env.tokens.saved) != 1 || env.tokens.saved[0].SessionID != claims.SessionID {
		t.Fatalf("refresh 会话的 sessionId = %+v，期望 %q", env.tokens.saved, claims.SessionID)
	}

	// 不携带 refresh token：仍必须撤销 sid 对应的 refresh 会话。
	if err := env.service.Logout(
		context.Background(),
		login.Tokens.AccessToken,
		"",
	); err != nil {
		t.Fatalf("登出应成功，实际 err=%v", err)
	}
	if !containsString(env.tokens.sidDeleted, claims.SessionID) {
		t.Errorf(
			"必须按 claims.sid 调用 DeleteRefreshSessionBySessionID，实际 %v",
			env.tokens.sidDeleted,
		)
	}
	if !containsString(env.tokens.revoked, claims.ID) {
		t.Errorf("必须撤销 access token 的 jti，实际 %v", env.tokens.revoked)
	}
	refreshHash := authsession.HashRefreshToken(login.Tokens.RefreshToken)
	if _, ok := env.tokens.sessions[refreshHash]; ok {
		t.Error("refresh 会话必须已被撤销")
	}

	// 重复登出：会话已不存在，按未命中处理且不报错。
	if err := env.service.Logout(
		context.Background(),
		login.Tokens.AccessToken,
		"",
	); err != nil {
		t.Fatalf("重复登出应幂等成功，实际 err=%v", err)
	}
	if !containsString(env.tokens.sidMissed, claims.SessionID) {
		t.Errorf("会话已撤销后按 sid 撤销应记为未命中，实际 %v", env.tokens.sidMissed)
	}
}

// TestLogoutSessionRevokeFailureIsDependencyUnavailable 覆盖登出时按 sid 撤销失败：
// 必须归为依赖故障（handler → 502 DEPENDENCY_UNAVAILABLE），
// 不得静默成功，也不得伪装成 access 令牌无效。
func TestLogoutSessionRevokeFailureIsDependencyUnavailable(t *testing.T) {
	env := newPatientAuthTestEnv(t)

	login, err := env.service.WeChatLogin(context.Background(), "wx_code_logout_dep")
	if err != nil {
		t.Fatalf("登录应成功，实际 err=%v", err)
	}
	cause := errors.New("redis unavailable")
	env.tokens.sidErr = cause

	err = env.service.Logout(context.Background(), login.Tokens.AccessToken, "")
	if !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("err = %v，期望 ErrDependencyUnavailable", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("依赖故障必须保留根因，err = %v", err)
	}
	if errors.Is(err, ErrInvalidAccessToken) {
		t.Error("依赖故障不得被归类为 access 令牌无效")
	}
}

// TestWeChatLoginCodeLengthCountsRunes 覆盖 code 长度按 Unicode 字符数校验：
// 128 个中文字符（字节数 384）合法；129 个中文字符非法。
func TestWeChatLoginCodeLengthCountsRunes(t *testing.T) {
	t.Run("128 个多字节字符合法", func(t *testing.T) {
		env := newPatientAuthTestEnv(t)
		code := strings.Repeat("汉", 128)

		result, err := env.service.WeChatLogin(context.Background(), code)
		if err != nil {
			t.Fatalf("128 字符（Unicode）code 应通过校验，实际 err=%v", err)
		}
		if result == nil || !result.IsNewUser {
			t.Fatalf("登录结果 = %+v，期望首次注册成功", result)
		}
		if len(env.wechat.calls) != 1 || env.wechat.calls[0] != code {
			t.Errorf("适配器收到的 code = %q，期望原样透传", env.wechat.calls)
		}
	})

	t.Run("129 个多字节字符非法", func(t *testing.T) {
		env := newPatientAuthTestEnv(t)

		_, err := env.service.WeChatLogin(
			context.Background(),
			strings.Repeat("汉", 129),
		)
		if !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("err = %v，期望 ErrInvalidCode", err)
		}
		if len(env.wechat.calls) != 0 {
			t.Error("本地校验失败不得调用微信适配器")
		}
	})
}
