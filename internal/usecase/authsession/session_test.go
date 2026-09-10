// 共用会话层单测：realm 隔离、跨域不互相破坏会话、refresh 轮换与登出撤销。
// 用内存桩替换 port.TokenRepository，不依赖 Redis
// （spec/04-api-contract.md §1.2 认证域隔离、§7 患者域共用会话层）。
package authsession

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/port"
)

// sessionTestSecret 是本文件所有用例共用的 JWT 签名密钥。
const sessionTestSecret = "authsession-unit-test-secret"

// sessionTestTokenRepo 是内存版 port.TokenRepository 桩，并记录写操作。
type sessionTestTokenRepo struct {
	port.TokenRepository

	sessions   map[string]port.RefreshSession
	saved      []port.RefreshSession
	deleted    []string
	revoked    []string
	sidDeleted []string
	sidMissed  []string
	// deletedSIDs 与 deleted 一一对应，记录 DeleteRefreshSession 收到的 sessionID：
	// 生产实现靠它一并删除 sessionID -> tokenHash 反查索引，桩若丢弃该参数，
	// 「忘记传 sessionID」的回归就无法被测出。
	deletedSIDs []string

	// calls 按调用顺序记录关键写操作（撤销 access token、按 sid 清理会话），
	// 用于断言登出的执行顺序：先撤销 access token，再清理 refresh 会话。
	calls []string

	// sidErr 非 nil 时 DeleteRefreshSessionBySessionID 固定失败，
	// 用于验证 refresh 会话清理失败不会阻止 access token 进入撤销名单。
	sidErr error
}

func newSessionTestTokenRepo() *sessionTestTokenRepo {
	return &sessionTestTokenRepo{sessions: map[string]port.RefreshSession{}}
}

func (r *sessionTestTokenRepo) SaveRefreshSession(
	_ context.Context,
	session port.RefreshSession,
	ttlSeconds int64,
) error {
	if ttlSeconds < 1 {
		return errors.New("ttl must be positive")
	}
	r.sessions[session.TokenHash] = session
	r.saved = append(r.saved, session)
	return nil
}

func (r *sessionTestTokenRepo) GetRefreshSession(
	_ context.Context,
	tokenHash string,
) (*port.RefreshSession, error) {
	session, ok := r.sessions[tokenHash]
	if !ok {
		return nil, port.ErrRefreshSessionNotFound
	}
	copied := session
	return &copied, nil
}

func (r *sessionTestTokenRepo) DeleteRefreshSession(
	_ context.Context,
	tokenHash string,
	sessionID string,
) error {
	r.deleted = append(r.deleted, tokenHash)
	r.deletedSIDs = append(r.deletedSIDs, sessionID)
	delete(r.sessions, tokenHash)
	return nil
}

// DeleteRefreshSessionBySessionID 按 sessionID 在内存里反查会话并删除，
// 对应生产实现中 Redis 的 sessionID -> tokenHash 反查索引；
// 命中返回 (true, nil)，未命中返回 (false, nil)（登出保持幂等）。
func (r *sessionTestTokenRepo) DeleteRefreshSessionBySessionID(
	_ context.Context,
	sessionID string,
) (bool, error) {
	r.calls = append(r.calls, "sid:"+sessionID)
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

func (r *sessionTestTokenRepo) RevokeAccessToken(
	_ context.Context,
	jti string,
	_ int64,
) error {
	r.calls = append(r.calls, "revoke:"+jti)
	r.revoked = append(r.revoked, jti)
	return nil
}

func (r *sessionTestTokenRepo) IsAccessTokenRevoked(
	_ context.Context,
	_ string,
) (bool, error) {
	return false, nil
}

// newSessionTestManager 构造带有效配置的共用会话层。
func newSessionTestManager(t *testing.T) (*Manager, *sessionTestTokenRepo) {
	t.Helper()
	repo := newSessionTestTokenRepo()
	manager := NewManager(repo, Config{
		JWTSecret:  sessionTestSecret,
		AccessTTL:  15 * time.Minute,
		RefreshTTL: time.Hour,
	})
	return manager, repo
}

// seedSession 预置一条属于指定 realm 的 refresh 会话。
func seedSession(repo *sessionTestTokenRepo, realm domainauth.Realm, refreshToken string) string {
	hash := HashRefreshToken(refreshToken)
	repo.sessions[hash] = port.RefreshSession{
		TokenHash: hash,
		SessionID: "session-" + string(realm),
		UserID:    20,
		Username:  "小明",
		Realm:     realm,
	}
	return hash
}

// containsSessionValue 判断切片中是否包含目标值。
func containsSessionValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// TestIssueAndParseAccessTokenEnforcesRealm 覆盖签发与解析的 realm 隔离：
// 患者令牌只能在患者域被解释；缺少/非法 realm 一律按无效令牌处理。
func TestIssueAndParseAccessTokenEnforcesRealm(t *testing.T) {
	manager, _ := newSessionTestManager(t)

	pair, sessionID, err := manager.Issue(
		domainauth.RealmPatient,
		Subject{ID: 20, Name: "小明"},
	)
	if err != nil {
		t.Fatalf("签发患者令牌失败：%v", err)
	}
	if sessionID == "" {
		t.Error("会话 ID 不得为空")
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("必须同时签发 access/refresh 令牌")
	}

	claims, err := manager.ParseAccessToken(pair.AccessToken, domainauth.RealmPatient)
	if err != nil {
		t.Fatalf("患者令牌应在患者域被接受，实际 err=%v", err)
	}
	if claims.Realm != domainauth.RealmPatient {
		t.Errorf("claims.realm = %q，期望 patient", claims.Realm)
	}
	if claims.UserID != 20 {
		t.Errorf("claims.uid = %d，期望 20", claims.UserID)
	}
	if claims.TokenType != TokenTypeAccess {
		t.Errorf("claims.token_type = %q，期望 %q", claims.TokenType, TokenTypeAccess)
	}
	if claims.SessionID != sessionID {
		t.Errorf("claims.sid = %q，期望 %q", claims.SessionID, sessionID)
	}
	if claims.ID == "" {
		t.Error("access token 必须带 jti")
	}

	cases := []struct {
		name  string
		realm domainauth.Realm
	}{
		{name: "在管理域解析", realm: domainauth.RealmMis},
		{name: "realm 缺失", realm: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := manager.ParseAccessToken(
				pair.AccessToken,
				tc.realm,
			); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v，期望 ErrInvalidToken", err)
			}
		})
	}

	if _, _, err := manager.Issue(
		"unknown-realm",
		Subject{ID: 20, Name: "小明"},
	); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("非法 realm 签发应返回 ErrInvalidToken，实际 err=%v", err)
	}
}

// TestSaveRefreshSessionStoresHashNotRawToken 覆盖刷新会话落库：
// 只保存摘要，且必须带 realm。
func TestSaveRefreshSessionStoresHashNotRawToken(t *testing.T) {
	manager, repo := newSessionTestManager(t)

	pair, sessionID, err := manager.Issue(
		domainauth.RealmPatient,
		Subject{ID: 20, Name: "小明"},
	)
	if err != nil {
		t.Fatalf("签发患者令牌失败：%v", err)
	}

	if err := manager.SaveRefreshSession(
		context.Background(),
		pair,
		sessionID,
		Subject{ID: 20, Name: "小明"},
		domainauth.RealmPatient,
	); err != nil {
		t.Fatalf("写入 refresh 会话失败：%v", err)
	}

	if len(repo.saved) != 1 {
		t.Fatalf("写入次数 = %d，期望 1", len(repo.saved))
	}
	saved := repo.saved[0]
	if saved.Realm != domainauth.RealmPatient {
		t.Errorf("会话 realm = %q，期望 patient", saved.Realm)
	}
	if saved.TokenHash != HashRefreshToken(pair.RefreshToken) {
		t.Error("会话必须以 refresh token 摘要为键")
	}
	if saved.TokenHash == pair.RefreshToken {
		t.Error("Redis 中不得出现 refresh token 原文")
	}
	if _, ok := repo.sessions[pair.RefreshToken]; ok {
		t.Error("不得以 refresh token 原文作为存储键")
	}
}

// TestRotateRefreshSessionEnforcesRealm 覆盖轮换的 realm 隔离：
// 其他域的 refresh 会话既不被解释也不被删除。
func TestRotateRefreshSessionEnforcesRealm(t *testing.T) {
	manager, repo := newSessionTestManager(t)

	const misRefresh = "mis-refresh-token"
	misHash := seedSession(repo, domainauth.RealmMis, misRefresh)

	if _, err := manager.RotateRefreshSession(
		context.Background(),
		misRefresh,
		domainauth.RealmPatient,
	); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("跨域轮换应返回 ErrInvalidToken，实际 err=%v", err)
	}
	if containsSessionValue(repo.deleted, misHash) {
		t.Error("跨域轮换不得删除其他域的会话")
	}
	if _, ok := repo.sessions[misHash]; !ok {
		t.Error("其他域的会话必须保留")
	}

	// realm 匹配时正常轮换，且旧令牌立刻失效。
	if _, err := manager.RotateRefreshSession(
		context.Background(),
		misRefresh,
		domainauth.RealmMis,
	); err != nil {
		t.Fatalf("同域轮换应成功，实际 err=%v", err)
	}
	if !containsSessionValue(repo.deleted, misHash) {
		t.Error("同域轮换必须删除旧会话")
	}
	if !containsSessionValue(repo.deletedSIDs, "session-mis") {
		t.Errorf("轮换必须把会话 sessionID 传给仓储，实际 %v", repo.deletedSIDs)
	}
	if _, err := manager.RotateRefreshSession(
		context.Background(),
		misRefresh,
		domainauth.RealmMis,
	); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("旧 refresh token 已轮换，应返回 ErrInvalidToken，实际 err=%v", err)
	}
}

// TestLogoutDoesNotTouchOtherRealmState 覆盖登出的 realm 边界：
// 用错误的 realm 登出时既不删除对方会话，也不撤销 access token。
func TestLogoutDoesNotTouchOtherRealmState(t *testing.T) {
	manager, repo := newSessionTestManager(t)

	pair, sessionID, err := manager.Issue(
		domainauth.RealmPatient,
		Subject{ID: 20, Name: "小明"},
	)
	if err != nil {
		t.Fatalf("签发患者令牌失败：%v", err)
	}
	if err := manager.SaveRefreshSession(
		context.Background(),
		pair,
		sessionID,
		Subject{ID: 20, Name: "小明"},
		domainauth.RealmPatient,
	); err != nil {
		t.Fatalf("写入 refresh 会话失败：%v", err)
	}

	claims, err := manager.ParseAccessToken(pair.AccessToken, domainauth.RealmPatient)
	if err != nil {
		t.Fatalf("解析患者令牌失败：%v", err)
	}
	refreshHash := HashRefreshToken(pair.RefreshToken)

	if err := manager.Logout(
		context.Background(),
		pair.AccessToken,
		pair.RefreshToken,
		domainauth.RealmMis,
	); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("跨域登出应返回 ErrInvalidToken，实际 err=%v", err)
	}
	if len(repo.revoked) != 0 {
		t.Errorf("跨域登出不得撤销 access token，实际 %v", repo.revoked)
	}
	if len(repo.deleted) != 0 {
		t.Errorf("跨域登出不得删除 refresh 会话，实际 %v", repo.deleted)
	}
	if len(repo.sidDeleted) != 0 {
		t.Errorf("跨域登出不得按 sid 撤销任何会话，实际 %v", repo.sidDeleted)
	}

	// realm 匹配且只携带 access token 时正常登出：必须按 claims.sid 找到并撤销
	// refresh 会话，同时撤销 access token 的 jti（契约 §7.2：登出要撤销当前会话）。
	if err := manager.Logout(
		context.Background(),
		pair.AccessToken,
		"",
		domainauth.RealmPatient,
	); err != nil {
		t.Fatalf("同域登出应成功，实际 err=%v", err)
	}
	if !containsSessionValue(repo.deleted, refreshHash) {
		t.Error("同域登出必须删除 refresh 会话")
	}
	if !containsSessionValue(repo.revoked, claims.ID) {
		t.Error("同域登出必须撤销 access token")
	}
	if !containsSessionValue(repo.sidDeleted, sessionID) {
		t.Errorf("同域登出必须按 claims.sid 撤销 refresh 会话，实际 %v", repo.sidDeleted)
	}

	// 重复登出仍幂等：会话已不存在时按未命中处理（返回 false 且不报错）。
	if err := manager.Logout(
		context.Background(),
		pair.AccessToken,
		"",
		domainauth.RealmPatient,
	); err != nil {
		t.Fatalf("重复登出应幂等成功，实际 err=%v", err)
	}
	if !containsSessionValue(repo.sidMissed, sessionID) {
		t.Errorf("会话已撤销后按 sid 撤销应记为未命中，实际 %v", repo.sidMissed)
	}
}

// TestLogoutRevokesAccessTokenBeforeRefreshCleanup 覆盖登出的执行顺序与失败隔离：
// refresh 会话的读-删窗口由 access token 的可靠性兜底，因此必须先撤销 access token
// 的 jti（安全关键动作），再按 claims.sid 清理 refresh 会话；即使 sid 清理失败，
// access token 也必须已经进入撤销名单，错误按依赖故障原样上报（契约 §7.2）。
func TestLogoutRevokesAccessTokenBeforeRefreshCleanup(t *testing.T) {
	manager, repo := newSessionTestManager(t)

	pair, sessionID, err := manager.Issue(
		domainauth.RealmPatient,
		Subject{ID: 20, Name: "小明"},
	)
	if err != nil {
		t.Fatalf("签发患者令牌失败：%v", err)
	}
	if err := manager.SaveRefreshSession(
		context.Background(),
		pair,
		sessionID,
		Subject{ID: 20, Name: "小明"},
		domainauth.RealmPatient,
	); err != nil {
		t.Fatalf("写入 refresh 会话失败：%v", err)
	}
	claims, err := manager.ParseAccessToken(pair.AccessToken, domainauth.RealmPatient)
	if err != nil {
		t.Fatalf("解析患者令牌失败：%v", err)
	}

	repo.sidErr = errors.New("redis unavailable")
	err = manager.Logout(
		context.Background(),
		pair.AccessToken,
		"",
		domainauth.RealmPatient,
	)
	if err == nil {
		t.Fatal("按 sid 清理会话失败时登出必须上报错误，不得静默成功")
	}
	if !errors.Is(err, repo.sidErr) {
		t.Errorf("err = %v，期望保留 sid 清理失败的根因", err)
	}

	if !containsSessionValue(repo.revoked, claims.ID) {
		t.Errorf("sid 清理失败也必须先撤销 access token，实际 revoked=%v", repo.revoked)
	}
	wantOrder := []string{"revoke:" + claims.ID, "sid:" + sessionID}
	if len(repo.calls) != len(wantOrder) {
		t.Fatalf("写操作调用序列 = %v，期望 %v", repo.calls, wantOrder)
	}
	for i := range wantOrder {
		if repo.calls[i] != wantOrder[i] {
			t.Errorf(
				"第 %d 个写操作 = %q，期望 %q（必须先撤销 access token，再清理 refresh 会话）",
				i+1,
				repo.calls[i],
				wantOrder[i],
			)
		}
	}
}

// TestRevokeRefreshSessionDeletesByHashWithSessionID 覆盖按 refresh token 撤销会话：
// 与轮换共用同一删除契约，必须把会话的 sessionID 一并传给仓储，
// 否则 Redis 中的 sessionID -> tokenHash 反查索引会悬挂（契约 §7.2）。
func TestRevokeRefreshSessionDeletesByHashWithSessionID(t *testing.T) {
	manager, repo := newSessionTestManager(t)
	const refreshToken = "mis-refresh-token"
	hash := seedSession(repo, domainauth.RealmMis, refreshToken)

	if err := manager.RevokeRefreshSession(
		context.Background(),
		refreshToken,
		domainauth.RealmMis,
	); err != nil {
		t.Fatalf("同域撤销会话应成功，实际 err=%v", err)
	}
	if !containsSessionValue(repo.deleted, hash) {
		t.Error("撤销必须删除 refresh 会话主键")
	}
	if !containsSessionValue(repo.deletedSIDs, "session-mis") {
		t.Errorf("撤销必须把会话 sessionID 传给仓储，实际 %v", repo.deletedSIDs)
	}
}

// sessionTestExpiredAccessToken 用测试密钥签发一枚「签名正确但已过期」的 patient
// access token。Manager.Issue 拒绝非正 TTL，因此这里直接构造 JWT，
// 用于覆盖登出严格语义里「已过期令牌」这一类输入。
func sessionTestExpiredAccessToken(t *testing.T) string {
	t.Helper()
	now := time.Now()
	claims := AccessClaims{
		UserID:    20,
		Username:  "小明",
		TokenType: TokenTypeAccess,
		Realm:     domainauth.RealmPatient,
		SessionID: "session-expired",
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "expired-patient-jti",
			IssuedAt:  jwt.NewNumericDate(now.Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(now.Add(-time.Hour)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(sessionTestSecret))
	if err != nil {
		t.Fatalf("签发过期测试令牌失败：%v", err)
	}
	return token
}

// sessionTestTamperSignature 改动 JWT 签名段的第一个字符，模拟被篡改的令牌。
// 不能改最后一个字符：base64url 的末位可能只承载未使用的填充位，
// 改动后解码出的签名字节不变，令牌仍会通过校验。
func sessionTestTamperSignature(token string) string {
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

// assertLogoutHasNoSideEffects 断言「无效 access token」分支零副作用：
// 既不撤销任何 jti（不写撤销名单），也不按摘要或按 sid 删除 refresh 会话，
// 且整条路径上没有任何写操作调用记录。
func assertLogoutHasNoSideEffects(t *testing.T, repo *sessionTestTokenRepo) {
	t.Helper()
	if len(repo.revoked) != 0 {
		t.Errorf("不得撤销任何 jti，实际 %v", repo.revoked)
	}
	if len(repo.deleted) != 0 {
		t.Errorf("不得按摘要删除任何 refresh 会话，实际 %v", repo.deleted)
	}
	if len(repo.sidDeleted) != 0 {
		t.Errorf("不得按 sid 删除任何 refresh 会话，实际 %v", repo.sidDeleted)
	}
	if len(repo.calls) != 0 {
		t.Errorf("无效令牌分支不得产生副作用，实际调用序列 %v", repo.calls)
	}
}

// TestLogoutWithInvalidAccessTokenHasNoSideEffects 覆盖登出顺序调整后的关键不变量：
// 调用方提交了 access token 时必须先解析校验，再做任何撤销动作。
// 调整前是「先撤 refresh 会话、后解析 access token」，无效的 access token
// 会先把仍然有效的 refresh 会话删掉；现在签名被篡改 / 已过期 / realm 不匹配
// 三种输入都必须零副作用，且 refresh 会话原样保留（契约 §7.2 严格语义）。
func TestLogoutWithInvalidAccessTokenHasNoSideEffects(t *testing.T) {
	cases := []struct {
		name string
		// invalidToken 基于本次会话构造要提交的无效 access token。
		invalidToken func(t *testing.T, manager *Manager, pair TokenPair) string
	}{
		{
			name: "签名被篡改",
			invalidToken: func(_ *testing.T, _ *Manager, pair TokenPair) string {
				return sessionTestTamperSignature(pair.AccessToken)
			},
		},
		{
			name: "已过期",
			invalidToken: func(t *testing.T, _ *Manager, _ TokenPair) string {
				return sessionTestExpiredAccessToken(t)
			},
		},
		{
			name: "realm 不匹配",
			invalidToken: func(t *testing.T, manager *Manager, _ TokenPair) string {
				misPair, _, err := manager.Issue(
					domainauth.RealmMis,
					Subject{ID: 7, Name: "管理员"},
				)
				if err != nil {
					t.Fatalf("签发管理端测试令牌失败：%v", err)
				}
				return misPair.AccessToken
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, repo := newSessionTestManager(t)

			pair, sessionID, err := manager.Issue(
				domainauth.RealmPatient,
				Subject{ID: 20, Name: "小明"},
			)
			if err != nil {
				t.Fatalf("签发患者令牌失败：%v", err)
			}
			if err := manager.SaveRefreshSession(
				context.Background(),
				pair,
				sessionID,
				Subject{ID: 20, Name: "小明"},
				domainauth.RealmPatient,
			); err != nil {
				t.Fatalf("写入 refresh 会话失败：%v", err)
			}
			refreshHash := HashRefreshToken(pair.RefreshToken)

			invalid := tc.invalidToken(t, manager, pair)
			if invalid == "" || invalid == pair.AccessToken {
				t.Fatalf("用例必须提交与有效令牌不同的无效令牌，实际 %q", invalid)
			}

			err = manager.Logout(
				context.Background(),
				invalid,
				pair.RefreshToken,
				domainauth.RealmPatient,
			)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("无效 access token 登出应返回 ErrInvalidToken，实际 err=%v", err)
			}

			// 零副作用：没有任何写操作，也没有按 sid 的反查删除。
			assertLogoutHasNoSideEffects(t, repo)

			// refresh 会话必须原样保留，客户端可继续用它换取新令牌。
			if _, ok := repo.sessions[refreshHash]; !ok {
				t.Error("无效 access token 登出不得破坏仍然有效的 refresh 会话")
			}
			if len(repo.sidMissed) != 0 {
				t.Errorf("无效 access token 登出不得触发按 sid 的反查，实际 %v", repo.sidMissed)
			}
		})
	}
}

// TestLogoutWithoutAccessTokenStillRevokesRefreshSession 覆盖管理端「只带 refresh
// Cookie 登出」的容忍路径：空 access token 是合法用法，必须跳过解析（不得因此
// 返回 ErrInvalidToken），同时仍按 realm 撤销 refresh 会话。
// 该路径没有 claims，不得撤销任何 jti。
func TestLogoutWithoutAccessTokenStillRevokesRefreshSession(t *testing.T) {
	manager, repo := newSessionTestManager(t)

	pair, sessionID, err := manager.Issue(
		domainauth.RealmPatient,
		Subject{ID: 20, Name: "小明"},
	)
	if err != nil {
		t.Fatalf("签发患者令牌失败：%v", err)
	}
	if err := manager.SaveRefreshSession(
		context.Background(),
		pair,
		sessionID,
		Subject{ID: 20, Name: "小明"},
		domainauth.RealmPatient,
	); err != nil {
		t.Fatalf("写入 refresh 会话失败：%v", err)
	}
	refreshHash := HashRefreshToken(pair.RefreshToken)

	if err := manager.Logout(
		context.Background(),
		"",
		pair.RefreshToken,
		domainauth.RealmPatient,
	); err != nil {
		t.Fatalf("只带 refresh token 的登出应成功，实际 err=%v", err)
	}

	if !containsSessionValue(repo.deleted, refreshHash) {
		t.Error("只带 refresh token 的登出也必须删除 refresh 会话")
	}
	if !containsSessionValue(repo.deletedSIDs, sessionID) {
		t.Errorf("按摘要删除必须把会话 sessionID 传给仓储，实际 %v", repo.deletedSIDs)
	}
	if _, ok := repo.sessions[refreshHash]; ok {
		t.Error("refresh 会话必须已从存储中移除")
	}
	if len(repo.revoked) != 0 {
		t.Errorf("没有 access token 时不得撤销任何 jti，实际 %v", repo.revoked)
	}
	for _, call := range repo.calls {
		if strings.HasPrefix(call, "revoke:") {
			t.Errorf("没有 access token 时不得调用 RevokeAccessToken，实际调用序列 %v", repo.calls)
		}
	}
}

// TestLogoutWithAccessAndRefreshTokensRevokesBoth 覆盖两种令牌同时提交：
// access token 的 jti 与 refresh 会话都必须失效，且写操作顺序保持
// 「先撤销 access token、再按 sid 清理会话」；按摘要删除时收到的 sessionID
// 必须等于本次会话的 sid（漏传会让 Redis 反查索引悬挂，契约 §7.2）。
func TestLogoutWithAccessAndRefreshTokensRevokesBoth(t *testing.T) {
	manager, repo := newSessionTestManager(t)

	pair, sessionID, err := manager.Issue(
		domainauth.RealmPatient,
		Subject{ID: 20, Name: "小明"},
	)
	if err != nil {
		t.Fatalf("签发患者令牌失败：%v", err)
	}
	if err := manager.SaveRefreshSession(
		context.Background(),
		pair,
		sessionID,
		Subject{ID: 20, Name: "小明"},
		domainauth.RealmPatient,
	); err != nil {
		t.Fatalf("写入 refresh 会话失败：%v", err)
	}
	claims, err := manager.ParseAccessToken(pair.AccessToken, domainauth.RealmPatient)
	if err != nil {
		t.Fatalf("解析患者令牌失败：%v", err)
	}
	refreshHash := HashRefreshToken(pair.RefreshToken)

	if err := manager.Logout(
		context.Background(),
		pair.AccessToken,
		pair.RefreshToken,
		domainauth.RealmPatient,
	); err != nil {
		t.Fatalf("同时携带两种令牌的登出应成功，实际 err=%v", err)
	}

	if !containsSessionValue(repo.revoked, claims.ID) {
		t.Errorf("必须撤销 access token 的 jti，实际 revoked=%v", repo.revoked)
	}
	if !containsSessionValue(repo.deleted, refreshHash) {
		t.Error("必须按摘要删除 refresh 会话")
	}
	if !containsSessionValue(repo.deletedSIDs, sessionID) {
		t.Errorf("按摘要删除必须把会话 sessionID（%q）传给仓储，实际 %v", sessionID, repo.deletedSIDs)
	}
	if _, ok := repo.sessions[refreshHash]; ok {
		t.Error("refresh 会话必须已从存储中移除")
	}

	// refresh 分支已经删掉会话，随后按 sid 的反查应记为未命中并保持幂等成功。
	if !containsSessionValue(repo.sidMissed, sessionID) {
		t.Errorf("会话已由 refresh 分支删除，按 sid 反查应记为未命中，实际 %v", repo.sidMissed)
	}

	wantOrder := []string{"revoke:" + claims.ID, "sid:" + sessionID}
	if len(repo.calls) != len(wantOrder) {
		t.Fatalf("写操作调用序列 = %v，期望 %v", repo.calls, wantOrder)
	}
	for i := range wantOrder {
		if repo.calls[i] != wantOrder[i] {
			t.Errorf("第 %d 个写操作 = %q，期望 %q", i+1, repo.calls[i], wantOrder[i])
		}
	}
}
