package misuser

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainuser "Medical-Web-Backend/internal/domain/user"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/usecase/authsession"
)

// realmTestJWTSecret 是本文件所有用例共用的签名密钥。
const realmTestJWTSecret = "misuser-realm-test-secret"

// realmTestTokenRepository 是内存版 TokenRepository 桩：refresh 会话保存在 map 中，
// 同时记录保存/删除/撤销动作，便于在 realm 隔离用例中断言
// “其他认证域的会话既不被解释也不被撤销”。
type realmTestTokenRepository struct {
	port.TokenRepository

	sessions   map[string]port.RefreshSession // tokenHash -> refresh 会话
	saved      []port.RefreshSession          // SaveRefreshSession 入参记录
	deleted    []string                       // DeleteRefreshSession 的 tokenHash 记录
	revoked    []string                       // RevokeAccessToken 的 jti 记录
	sidDeleted []string                       // 按 sessionID 命中删除的记录
	sidMissed  []string                       // 按 sessionID 未命中的记录
	// deletedSIDs 与 deleted 一一对应，记录 DeleteRefreshSession 收到的 sessionID：
	// 生产实现靠它一并删除反查索引，桩若丢弃该参数就无法测出相关回归。
	deletedSIDs []string

	// getHook 非 nil 时取代默认读取逻辑，用于精确构造读取 (会话, 错误) 组合，
	// 覆盖“会话不存在”“Redis 读取失败”等分支。
	getHook func(tokenHash string) (*port.RefreshSession, error)
}

func newRealmTestTokenRepository() *realmTestTokenRepository {
	return &realmTestTokenRepository{sessions: map[string]port.RefreshSession{}}
}

func (r *realmTestTokenRepository) SaveRefreshSession(
	_ context.Context,
	session port.RefreshSession,
	_ int64,
) error {
	r.sessions[session.TokenHash] = session
	r.saved = append(r.saved, session)
	return nil
}

func (r *realmTestTokenRepository) GetRefreshSession(
	_ context.Context,
	tokenHash string,
) (*port.RefreshSession, error) {
	session, ok := r.sessions[tokenHash]
	if r.getHook != nil {
		return r.getHook(tokenHash)
	}
	if !ok {
		// 与 repo.RedisTokenRepository 保持一致：键不存在时返回哨兵错误，
		// 便于调用方区分“会话不存在”与“读取失败”。
		return nil, port.ErrRefreshSessionNotFound
	}
	copied := session
	return &copied, nil
}

func (r *realmTestTokenRepository) DeleteRefreshSession(
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
// 对应生产实现里 Redis 的 sessionID -> tokenHash 反查索引：
// 命中返回 (true, nil)，未命中返回 (false, nil)（登出保持幂等）。
func (r *realmTestTokenRepository) DeleteRefreshSessionBySessionID(
	_ context.Context,
	sessionID string,
) (bool, error) {
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

func (r *realmTestTokenRepository) RevokeAccessToken(
	_ context.Context,
	jti string,
	_ int64,
) error {
	r.revoked = append(r.revoked, jti)
	return nil
}

func (r *realmTestTokenRepository) IsAccessTokenRevoked(
	_ context.Context,
	_ string,
) (bool, error) {
	return false, nil
}

// realmTestUserRepository 是内存版 UserRepository 桩，固定返回一个启用中的管理端用户。
type realmTestUserRepository struct {
	user        *domainuser.User
	permissions []string
}

func (r realmTestUserRepository) FindByUsername(
	_ context.Context,
	_ string,
) (*domainuser.User, error) {
	return r.user, nil
}

func (r realmTestUserRepository) FindByID(
	_ context.Context,
	userID int64,
) (*domainuser.User, error) {
	if r.user == nil || r.user.ID != userID {
		return nil, nil
	}
	return r.user, nil
}

func (r realmTestUserRepository) Permissions(
	_ context.Context,
	_ int64,
) ([]string, error) {
	return r.permissions, nil
}

// newRealmTestService 构造带有效 JWT 配置的 Service，用户桩固定为 ID=7 的启用用户。
func newRealmTestService(t *testing.T, tokens port.TokenRepository) *Service {
	t.Helper()
	return NewService(
		realmTestUserRepository{
			user: &domainuser.User{ID: 7, Username: "mis-admin", Status: 1},
		},
		tokens,
		Config{
			JWTSecret:  realmTestJWTSecret,
			AccessTTL:  15 * time.Minute,
			RefreshTTL: 24 * time.Hour,
		},
	)
}

// signRealmAccessToken 按给定 realm 签发 access token；
// realm 传空字符串可模拟改造前未携带 realm 字段的旧令牌。
func signRealmAccessToken(t *testing.T, realm domainauth.Realm) string {
	t.Helper()
	claims := &AccessClaims{
		UserID:    7,
		Username:  "mis-admin",
		TokenType: TokenTypeAccess,
		Realm:     realm,
		SessionID: "realm-test-session",
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "realm-test-jti-" + string(realm),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(realmTestJWTSecret))
	if err != nil {
		t.Fatalf("签发测试 access token 失败：%v", err)
	}
	return signed
}

// otherRealm 返回另一个认证域，便于断言同一令牌无法在另一域被解释。
func otherRealm(realm domainauth.Realm) domainauth.Realm {
	if realm == domainauth.RealmMis {
		return domainauth.RealmPatient
	}
	return domainauth.RealmMis
}

// realmTestContains 判断字符串切片中是否包含目标值。
func realmTestContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// TestParseAccessTokenRealmIsolation 覆盖 ParseAccessToken 的 realm 契约：
// 只有令牌 realm 与期望 realm 一致才解析成功；
// realm 不匹配、令牌缺少 realm、期望 realm 非法时统一返回 ErrInvalidToken。
func TestParseAccessTokenRealmIsolation(t *testing.T) {
	service := newRealmTestService(t, newRealmTestTokenRepository())

	cases := []struct {
		name          string
		tokenRealm    domainauth.Realm
		expectedRealm domainauth.Realm
		wantOK        bool
	}{
		{"管理域令牌在管理域解析成功", domainauth.RealmMis, domainauth.RealmMis, true},
		{"患者域令牌在患者域解析成功", domainauth.RealmPatient, domainauth.RealmPatient, true},
		{"患者域令牌在管理域被拒", domainauth.RealmPatient, domainauth.RealmMis, false},
		{"管理域令牌在患者域被拒", domainauth.RealmMis, domainauth.RealmPatient, false},
		{"缺少 realm 的旧令牌在管理域被拒", "", domainauth.RealmMis, false},
		{"缺少 realm 的旧令牌在患者域被拒", "", domainauth.RealmPatient, false},
		{"期望 realm 非法时被拒", domainauth.RealmMis, "unknown-realm", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := signRealmAccessToken(t, tc.tokenRealm)

			claims, err := service.ParseAccessToken(raw, tc.expectedRealm)

			if tc.wantOK {
				if err != nil {
					t.Fatalf("期望解析成功，实际 err=%v", err)
				}
				if claims == nil {
					t.Fatal("解析成功时 claims 不应为 nil")
				}
				if claims.Realm != tc.expectedRealm {
					t.Errorf("claims.Realm = %q, want %q", claims.Realm, tc.expectedRealm)
				}
				return
			}

			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("期望 ErrInvalidToken，实际 err=%v", err)
			}
			if claims != nil {
				t.Errorf("解析失败时 claims 应为 nil，实际 %+v", claims)
			}
		})
	}
}

// TestIssueTokensTagsRealm 断言签发的 access token 携带传入的 realm，
// 且该令牌无法在另一个认证域被解释（同一签名密钥下的 realm 隔离基线）。
func TestIssueTokensTagsRealm(t *testing.T) {
	service := newRealmTestService(t, newRealmTestTokenRepository())
	u := &domainuser.User{ID: 7, Username: "mis-admin", Status: 1}

	for _, realm := range []domainauth.Realm{domainauth.RealmMis, domainauth.RealmPatient} {
		pair, sessionID, err := service.issueTokens(u, realm)
		if err != nil {
			t.Fatalf("realm=%q 签发令牌失败：%v", realm, err)
		}
		if sessionID == "" {
			t.Fatalf("realm=%q 签发令牌必须返回会话 ID", realm)
		}

		claims, err := service.ParseAccessToken(pair.AccessToken, realm)
		if err != nil {
			t.Fatalf("realm=%q 解析自己签发的令牌失败：%v", realm, err)
		}
		if claims.Realm != realm {
			t.Errorf("realm=%q 的令牌 claims.Realm = %q", realm, claims.Realm)
		}

		if _, err := service.ParseAccessToken(
			pair.AccessToken,
			otherRealm(realm),
		); !errors.Is(err, ErrInvalidToken) {
			t.Errorf(
				"realm=%q 的令牌在 %q 域解析应返回 ErrInvalidToken，实际 err=%v",
				realm,
				otherRealm(realm),
				err,
			)
		}
	}
}

// TestRefreshRejectsCrossRealmSession 覆盖 Refresh 的 realm 隔离：
// refresh 会话 realm 与请求域不一致时必须返回 ErrInvalidToken，
// 且不得轮换（删除）对方的会话，也不得签发新令牌。
func TestRefreshRejectsCrossRealmSession(t *testing.T) {
	const (
		patientRefresh = "patient-refresh-token"
		misRefresh     = "mis-refresh-token"
		legacyRefresh  = "legacy-refresh-token"
	)

	tokens := newRealmTestTokenRepository()
	tokens.sessions[authsession.HashRefreshToken(patientRefresh)] = port.RefreshSession{
		TokenHash: authsession.HashRefreshToken(patientRefresh),
		SessionID: "session-patient",
		UserID:    7,
		Username:  "mis-admin",
		Realm:     domainauth.RealmPatient,
	}
	tokens.sessions[authsession.HashRefreshToken(misRefresh)] = port.RefreshSession{
		TokenHash: authsession.HashRefreshToken(misRefresh),
		SessionID: "session-mis",
		UserID:    7,
		Username:  "mis-admin",
		Realm:     domainauth.RealmMis,
	}
	// 改造前写入 Redis 的旧会话没有 realm 字段，反序列化后为零值。
	tokens.sessions[authsession.HashRefreshToken(legacyRefresh)] = port.RefreshSession{
		TokenHash: authsession.HashRefreshToken(legacyRefresh),
		SessionID: "session-legacy",
		UserID:    7,
		Username:  "mis-admin",
	}

	service := newRealmTestService(t, tokens)
	ctx := context.Background()

	// 管理域用患者 refresh token 刷新：拒绝，且不得删除患者会话或签发新令牌。
	if _, err := service.Refresh(
		ctx,
		patientRefresh,
		domainauth.RealmMis,
	); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("跨域刷新应返回 ErrInvalidToken，实际 err=%v", err)
	}
	if len(tokens.deleted) != 0 {
		t.Errorf("跨域刷新不得删除 refresh 会话，实际删除 %v", tokens.deleted)
	}
	if len(tokens.saved) != 0 {
		t.Errorf("跨域刷新不得保存新会话，实际保存 %d 条", len(tokens.saved))
	}
	if _, ok := tokens.sessions[authsession.HashRefreshToken(patientRefresh)]; !ok {
		t.Error("患者域 refresh 会话不应被撤销")
	}

	// 患者域用自己的 refresh token 刷新：成功，新令牌与新会话都带 patient realm。
	result, err := service.Refresh(ctx, patientRefresh, domainauth.RealmPatient)
	if err != nil {
		t.Fatalf("同域刷新应成功，实际 err=%v", err)
	}
	claims, err := service.ParseAccessToken(result.Tokens.AccessToken, domainauth.RealmPatient)
	if err != nil {
		t.Fatalf("同域刷新签发的令牌应能按 patient 解析，实际 err=%v", err)
	}
	if claims.Realm != domainauth.RealmPatient || claims.UserID != 7 {
		t.Errorf("刷新后 claims = %+v，期望 realm=patient 且 uid=7", claims)
	}
	if len(tokens.saved) != 1 {
		t.Fatalf("刷新应保存 1 条新会话，实际 %d 条", len(tokens.saved))
	}
	if got := tokens.saved[0].Realm; got != domainauth.RealmPatient {
		t.Errorf("轮换后的 refresh 会话 realm = %q，期望 %q", got, domainauth.RealmPatient)
	}
	if !realmTestContains(tokens.deleted, authsession.HashRefreshToken(patientRefresh)) {
		t.Error("refresh token rotation 应删除旧会话")
	}
	if !realmTestContains(tokens.deletedSIDs, "session-patient") {
		t.Errorf("轮换必须把会话 sessionID 传给仓储，实际 %v", tokens.deletedSIDs)
	}

	// 管理域仍能用自己的 refresh token 刷新（隔离不影响本域）。
	if _, err := service.Refresh(ctx, misRefresh, domainauth.RealmMis); err != nil {
		t.Errorf("管理域刷新自己的会话应成功，实际 err=%v", err)
	}

	// 旧会话（realm 为空）不得被任何域解释。
	if _, err := service.Refresh(
		ctx,
		legacyRefresh,
		domainauth.RealmMis,
	); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("缺少 realm 的旧会话应返回 ErrInvalidToken，实际 err=%v", err)
	}

	// realm 非法时直接拒绝。
	if _, err := service.Refresh(
		ctx,
		misRefresh,
		"unknown-realm",
	); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("非法 realm 应返回 ErrInvalidToken，实际 err=%v", err)
	}

	// 会话不存在（仓储返回 port.ErrRefreshSessionNotFound）同样按无效令牌处理。
	if _, err := service.Refresh(
		ctx,
		"rotated-or-unknown-refresh-token",
		domainauth.RealmMis,
	); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("会话不存在应返回 ErrInvalidToken，实际 err=%v", err)
	}
}

// TestLogoutIsolatesRealmSessions 覆盖 Logout 的 realm 隔离：
// 请求域携带其他域的 refresh token 时不得撤销对方会话；
// realm 匹配时仍要正常撤销；非法 realm 直接拒绝。
func TestLogoutIsolatesRealmSessions(t *testing.T) {
	const patientRefresh = "patient-refresh-token"

	tokens := newRealmTestTokenRepository()
	tokens.sessions[authsession.HashRefreshToken(patientRefresh)] = port.RefreshSession{
		TokenHash: authsession.HashRefreshToken(patientRefresh),
		SessionID: "session-patient",
		UserID:    7,
		Username:  "mis-admin",
		Realm:     domainauth.RealmPatient,
	}
	service := newRealmTestService(t, tokens)
	ctx := context.Background()

	// 场景 1：管理端登出，携带患者 refresh token 与自己合法的 access token。
	// 患者会话必须保留，管理端自己的 access token 仍要被撤销。
	misAccess := signRealmAccessToken(t, domainauth.RealmMis)
	if err := service.Logout(ctx, misAccess, patientRefresh, domainauth.RealmMis); err != nil {
		t.Fatalf("管理端登出应幂等成功，实际 err=%v", err)
	}
	if realmTestContains(tokens.deleted, authsession.HashRefreshToken(patientRefresh)) {
		t.Error("管理端登出不得删除患者域 refresh 会话")
	}
	if _, ok := tokens.sessions[authsession.HashRefreshToken(patientRefresh)]; !ok {
		t.Error("患者域 refresh 会话应仍在存储中")
	}
	if len(tokens.revoked) != 1 || tokens.revoked[0] != "realm-test-jti-mis" {
		t.Errorf("管理端 access token 应被撤销，实际 revoked=%v", tokens.revoked)
	}

	// 场景 2：非法 realm 直接拒绝，且不触碰任何会话。
	if err := service.Logout(
		ctx,
		"",
		patientRefresh,
		"unknown-realm",
	); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("非法 realm 应返回 ErrInvalidToken，实际 err=%v", err)
	}
	if len(tokens.deleted) != 0 {
		t.Errorf("非法 realm 不得删除会话，实际 %v", tokens.deleted)
	}

	// 场景 3（回归）：realm 匹配时 refresh 会话必须被撤销。
	const misRefresh = "mis-refresh-token"
	tokens.sessions[authsession.HashRefreshToken(misRefresh)] = port.RefreshSession{
		TokenHash: authsession.HashRefreshToken(misRefresh),
		SessionID: "session-mis",
		UserID:    7,
		Username:  "mis-admin",
		Realm:     domainauth.RealmMis,
	}
	if err := service.Logout(ctx, "", misRefresh, domainauth.RealmMis); err != nil {
		t.Fatalf("管理端登出应成功，实际 err=%v", err)
	}
	if !realmTestContains(tokens.deleted, authsession.HashRefreshToken(misRefresh)) {
		t.Error("realm 匹配时必须撤销 refresh 会话")
	}

	// 场景 4：患者域 access token 无法在管理域被解释，整体返回 ErrInvalidToken，
	// 且不得删除患者域仍在使用的 refresh 会话、不得撤销任何 access token。
	deletedBefore := len(tokens.deleted)
	revokedBefore := len(tokens.revoked)
	patientAccess := signRealmAccessToken(t, domainauth.RealmPatient)
	if err := service.Logout(
		ctx,
		patientAccess,
		patientRefresh,
		domainauth.RealmMis,
	); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("跨域 access token 应返回 ErrInvalidToken，实际 err=%v", err)
	}
	if len(tokens.deleted) != deletedBefore {
		t.Errorf("跨域登出不得删除 refresh 会话，实际 %v", tokens.deleted)
	}
	if len(tokens.revoked) != revokedBefore {
		t.Errorf("跨域登出不得撤销 access token，实际 %v", tokens.revoked)
	}
	if _, ok := tokens.sessions[authsession.HashRefreshToken(patientRefresh)]; !ok {
		t.Error("患者域 refresh 会话应仍在存储中")
	}
}

// TestLogoutHandlesRefreshSessionLookupErrors 覆盖 Logout 读取 refresh 会话时的错误分支：
// 会话不存在（哨兵错误）按幂等空操作处理；其他读取错误必须原样上报，
// 且两种情况都不得调用 DeleteRefreshSession（读取失败时无法判断会话归属）。
func TestLogoutHandlesRefreshSessionLookupErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("仓储返回哨兵错误时幂等且不删除", func(t *testing.T) {
		tokens := newRealmTestTokenRepository()
		tokens.getHook = func(string) (*port.RefreshSession, error) {
			return nil, port.ErrRefreshSessionNotFound
		}
		service := newRealmTestService(t, tokens)

		if err := service.Logout(
			ctx,
			"",
			"already-rotated-refresh-token",
			domainauth.RealmMis,
		); err != nil {
			t.Fatalf("会话不存在时登出应幂等成功，实际 err=%v", err)
		}
		if len(tokens.deleted) != 0 {
			t.Errorf("会话不存在时不得调用 DeleteRefreshSession，实际 %v", tokens.deleted)
		}
	})

	t.Run("会话为 nil 且无错误时幂等且不删除", func(t *testing.T) {
		tokens := newRealmTestTokenRepository()
		tokens.getHook = func(string) (*port.RefreshSession, error) {
			return nil, nil
		}
		service := newRealmTestService(t, tokens)

		if err := service.Logout(
			ctx,
			"",
			"nil-session-refresh-token",
			domainauth.RealmMis,
		); err != nil {
			t.Fatalf("会话为 nil 时登出应幂等成功，实际 err=%v", err)
		}
		if len(tokens.deleted) != 0 {
			t.Errorf("会话为 nil 时不得调用 DeleteRefreshSession，实际 %v", tokens.deleted)
		}
	})

	t.Run("其他读取错误原样上报且不删除", func(t *testing.T) {
		redisDown := errors.New("redis unavailable")
		tokens := newRealmTestTokenRepository()
		tokens.getHook = func(string) (*port.RefreshSession, error) {
			return nil, redisDown
		}
		service := newRealmTestService(t, tokens)

		err := service.Logout(
			ctx,
			"",
			"any-refresh-token",
			domainauth.RealmMis,
		)
		if !errors.Is(err, redisDown) {
			t.Fatalf("读取失败应原样上报 %v，实际 err=%v", redisDown, err)
		}
		if len(tokens.deleted) != 0 {
			t.Errorf("读取失败时不得删除会话，实际 %v", tokens.deleted)
		}
	})

	t.Run("读取错误时也不得撤销 access token", func(t *testing.T) {
		tokens := newRealmTestTokenRepository()
		tokens.getHook = func(string) (*port.RefreshSession, error) {
			return nil, errors.New("redis timeout")
		}
		service := newRealmTestService(t, tokens)

		misAccess := signRealmAccessToken(t, domainauth.RealmMis)
		if err := service.Logout(
			ctx,
			misAccess,
			"any-refresh-token",
			domainauth.RealmMis,
		); err == nil {
			t.Fatal("读取失败时登出应返回错误，实际 nil")
		}
		if len(tokens.revoked) != 0 {
			t.Errorf("refresh 会话读取失败时不得继续撤销 access token，实际 %v", tokens.revoked)
		}
	})
}
