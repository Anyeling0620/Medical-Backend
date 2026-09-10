// Redis 令牌仓储的 sessionID 反查索引测试：保存时同时写主键与索引，
// 删除时两把键一起清理，按 sessionID 撤销在未命中时返回 (false, nil)
// （spec/04-api-contract.md §7.2：只带 access token 的登出也要撤销 refresh 会话）。
//
// 用内存 map 模拟 Redis 键空间，不依赖真实 Redis，也不引入新依赖；
// 与 redis_token_test.go 的 fakeRedisClient 互不影响（那个替身只覆写 Get）。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/port"
)

// sessionIndexRedisStore 用内存 map 模拟 Redis 的键空间与 TTL。
type sessionIndexRedisStore struct {
	values map[string]string
	ttls   map[string]time.Duration
}

func newSessionIndexRedisStore() *sessionIndexRedisStore {
	return &sessionIndexRedisStore{
		values: map[string]string{},
		ttls:   map[string]time.Duration{},
	}
}

func (s *sessionIndexRedisStore) set(key string, value string, ttl time.Duration) {
	s.values[key] = value
	if ttl > 0 {
		s.ttls[key] = ttl
	}
}

func (s *sessionIndexRedisStore) del(keys ...string) int {
	removed := 0
	for _, key := range keys {
		if _, ok := s.values[key]; ok {
			removed++
			delete(s.values, key)
		}
		delete(s.ttls, key)
	}
	return removed
}

// sessionIndexRedisClient 是 redis.UniversalClient 的最小替身：
// 只覆写本文件用到的 Get/Set/Del/TxPipeline，其余方法沿用内嵌接口
// （一旦被调用会 panic，便于发现实现越界使用了别的 Redis 命令）。
type sessionIndexRedisClient struct {
	redis.UniversalClient

	store  *sessionIndexRedisStore
	getErr error
	// getCalls 统计 Get 调用次数，用于断言删除路径不会为反查索引多做一次读。
	getCalls int
	// pipeExecErr 非 nil 时事务管道的 Exec 固定失败，用于覆盖写入/删除的故障分支。
	pipeExecErr error
}

func newSessionIndexRedisClient() *sessionIndexRedisClient {
	return &sessionIndexRedisClient{store: newSessionIndexRedisStore()}
}

func (c *sessionIndexRedisClient) Get(_ context.Context, key string) *redis.StringCmd {
	c.getCalls++
	if c.getErr != nil {
		return redis.NewStringResult("", c.getErr)
	}
	value, ok := c.store.values[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(value, nil)
}

func (c *sessionIndexRedisClient) Set(
	_ context.Context,
	key string,
	value any,
	expiration time.Duration,
) *redis.StatusCmd {
	switch typed := value.(type) {
	case []byte:
		c.store.set(key, string(typed), expiration)
	case string:
		c.store.set(key, typed, expiration)
	default:
		c.store.set(key, fmt.Sprint(value), expiration)
	}
	return redis.NewStatusResult("OK", nil)
}

func (c *sessionIndexRedisClient) Del(_ context.Context, keys ...string) *redis.IntCmd {
	return redis.NewIntResult(int64(c.store.del(keys...)), nil)
}

func (c *sessionIndexRedisClient) TxPipeline() redis.Pipeliner {
	return &sessionIndexPipeliner{client: c, execErr: c.pipeExecErr}
}

// sessionIndexPipeliner 模拟事务管道：命令入队，Exec 时按序作用到内存键空间。
type sessionIndexPipeliner struct {
	redis.Pipeliner

	client  *sessionIndexRedisClient
	ops     []func()
	execErr error
}

func (p *sessionIndexPipeliner) Set(
	ctx context.Context,
	key string,
	value any,
	expiration time.Duration,
) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx)
	p.ops = append(p.ops, func() {
		p.client.Set(ctx, key, value, expiration)
		cmd.SetVal("OK")
	})
	return cmd
}

func (p *sessionIndexPipeliner) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	cmd := redis.NewIntCmd(ctx)
	queued := append([]string(nil), keys...)
	p.ops = append(p.ops, func() {
		// 与真实 go-redis 一致：Del 的返回值在 Exec 之后才可用，
		// 且反映实际删除的键数量（生产实现据此判断会话主键是否真的被删除）。
		cmd.SetVal(int64(p.client.store.del(queued...)))
	})
	return cmd
}

func (p *sessionIndexPipeliner) Exec(ctx context.Context) ([]redis.Cmder, error) {
	if p.execErr != nil {
		return nil, p.execErr
	}
	queued := p.ops
	p.ops = nil
	for _, op := range queued {
		op()
	}
	return nil, nil
}

// sessionIndexSeedSession 构造一条患者域 refresh 会话。
func sessionIndexSeedSession() port.RefreshSession {
	return port.RefreshSession{
		TokenHash: "hash-abc",
		SessionID: "session-abc",
		UserID:    20,
		Username:  "小明",
		Realm:     auth.RealmPatient,
	}
}

// TestSaveRefreshSessionWritesSessionIndex 断言保存会话时同时写入
// 「令牌摘要主键」与「sessionID -> tokenHash 反查索引」，且 TTL 一致。
func TestSaveRefreshSessionWritesSessionIndex(t *testing.T) {
	client := newSessionIndexRedisClient()
	repo := NewRedisTokenRepository(client)

	session := sessionIndexSeedSession()
	if err := repo.SaveRefreshSession(context.Background(), session, 60); err != nil {
		t.Fatalf("保存 refresh 会话失败：%v", err)
	}

	payload, ok := client.store.values[refreshKey(session.TokenHash)]
	if !ok {
		t.Fatalf("缺少令牌摘要主键 %q，实际键=%v", refreshKey(session.TokenHash), keysOf(client.store))
	}
	var decoded port.RefreshSession
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("主键载荷不是合法 JSON：%v", err)
	}
	if decoded.SessionID != session.SessionID || decoded.Realm != auth.RealmPatient {
		t.Errorf("主键载荷 = %+v，期望 sessionId=%q realm=patient", decoded, session.SessionID)
	}

	indexed, ok := client.store.values[refreshSessionKey(session.SessionID)]
	if !ok {
		t.Fatalf("缺少 sid 反查索引 %q，实际键=%v", refreshSessionKey(session.SessionID), keysOf(client.store))
	}
	if indexed != session.TokenHash {
		t.Errorf("反查索引值 = %q，期望 %q", indexed, session.TokenHash)
	}

	wantTTL := 60 * time.Second
	for _, key := range []string{refreshKey(session.TokenHash), refreshSessionKey(session.SessionID)} {
		if got := client.store.ttls[key]; got != wantTTL {
			t.Errorf("键 %q 的 TTL = %v，期望 %v", key, got, wantTTL)
		}
	}
}

// TestDeleteRefreshSessionRemovesIndex 断言按摘要删除会话（携带 sessionID）时，
// 主键与 sid 反查索引一起被清理（会话已不存在时退化为只删主键且不报错）。
func TestDeleteRefreshSessionRemovesIndex(t *testing.T) {
	client := newSessionIndexRedisClient()
	repo := NewRedisTokenRepository(client)
	session := sessionIndexSeedSession()

	if err := repo.SaveRefreshSession(context.Background(), session, 60); err != nil {
		t.Fatalf("保存 refresh 会话失败：%v", err)
	}
	if err := repo.DeleteRefreshSession(
		context.Background(),
		session.TokenHash,
		session.SessionID,
	); err != nil {
		t.Fatalf("删除 refresh 会话失败：%v", err)
	}
	if _, ok := client.store.values[refreshKey(session.TokenHash)]; ok {
		t.Error("删除后令牌摘要主键应不存在")
	}
	if _, ok := client.store.values[refreshSessionKey(session.SessionID)]; ok {
		t.Error("删除后 sid 反查索引应被清理")
	}

	// 会话不存在时删除仍是幂等成功（不能因为读不到会话就报错）。
	if err := repo.DeleteRefreshSession(
		context.Background(),
		"missing-hash",
		"missing-session",
	); err != nil {
		t.Fatalf("删除不存在的会话不应报错，实际 err=%v", err)
	}
}

// TestDeleteRefreshSessionWithoutSessionIDSkipsIndexAndRead 断言不传 sessionID 时
// 只删令牌摘要主键：不触碰反查索引，也不产生任何额外读取。
// 这样轮换路径才能把「读-删窗口」压到最小，避免并发使用同一 refresh token
// 时把窗口放大（port.TokenRepository.DeleteRefreshSession 的注释约束）。
func TestDeleteRefreshSessionWithoutSessionIDSkipsIndexAndRead(t *testing.T) {
	client := newSessionIndexRedisClient()
	repo := NewRedisTokenRepository(client)
	session := sessionIndexSeedSession()

	if err := repo.SaveRefreshSession(context.Background(), session, 60); err != nil {
		t.Fatalf("保存 refresh 会话失败：%v", err)
	}
	// 保存路径本身不应触发读取，从这里开始单独统计删除路径的读取次数。
	client.getCalls = 0

	if err := repo.DeleteRefreshSession(
		context.Background(),
		session.TokenHash,
		"",
	); err != nil {
		t.Fatalf("删除 refresh 会话失败：%v", err)
	}
	if _, ok := client.store.values[refreshKey(session.TokenHash)]; ok {
		t.Error("不传 sessionID 时仍应删除令牌摘要主键")
	}
	if _, ok := client.store.values[refreshSessionKey(session.SessionID)]; !ok {
		t.Error("不传 sessionID 时不得删除 sid 反查索引")
	}
	if client.getCalls != 0 {
		t.Errorf("删除路径不得产生额外读取，实际 Get 调用次数 = %d", client.getCalls)
	}
}

// TestDeleteRefreshSessionBySessionID 覆盖按 sessionID 撤销的四个分支：
// 命中（删除主键与索引并返回 true）、未命中（返回 false 且不报错）、
// 空 sessionID，以及「索引残留但主键不存在」（返回 false 并清理残留索引）。
func TestDeleteRefreshSessionBySessionID(t *testing.T) {
	t.Run("命中", func(t *testing.T) {
		client := newSessionIndexRedisClient()
		repo := NewRedisTokenRepository(client)
		session := sessionIndexSeedSession()

		if err := repo.SaveRefreshSession(context.Background(), session, 60); err != nil {
			t.Fatalf("保存 refresh 会话失败：%v", err)
		}
		removed, err := repo.DeleteRefreshSessionBySessionID(
			context.Background(),
			session.SessionID,
		)
		if err != nil {
			t.Fatalf("按 sid 撤销应成功，实际 err=%v", err)
		}
		if !removed {
			t.Error("命中索引时应返回 true")
		}
		if _, ok := client.store.values[refreshKey(session.TokenHash)]; ok {
			t.Error("命中后令牌摘要主键应被删除")
		}
		if _, ok := client.store.values[refreshSessionKey(session.SessionID)]; ok {
			t.Error("命中后 sid 反查索引应被删除")
		}
	})

	t.Run("未命中", func(t *testing.T) {
		client := newSessionIndexRedisClient()
		repo := NewRedisTokenRepository(client)
		session := sessionIndexSeedSession()
		if err := repo.SaveRefreshSession(context.Background(), session, 60); err != nil {
			t.Fatalf("保存 refresh 会话失败：%v", err)
		}

		removed, err := repo.DeleteRefreshSessionBySessionID(
			context.Background(),
			"unknown-session",
		)
		if err != nil {
			t.Fatalf("未命中不应报错，实际 err=%v", err)
		}
		if removed {
			t.Error("未命中索引时应返回 false")
		}
		// 未命中不得误删仍然有效的会话。
		if _, ok := client.store.values[refreshKey(session.TokenHash)]; !ok {
			t.Error("未命中不得删除其他会话的主键")
		}
		if _, ok := client.store.values[refreshSessionKey(session.SessionID)]; !ok {
			t.Error("未命中不得删除其他会话的索引")
		}
	})

	t.Run("空 sessionID 直接返回 false", func(t *testing.T) {
		client := newSessionIndexRedisClient()
		repo := NewRedisTokenRepository(client)

		removed, err := repo.DeleteRefreshSessionBySessionID(context.Background(), "")
		if err != nil {
			t.Fatalf("空 sessionID 不应报错，实际 err=%v", err)
		}
		if removed {
			t.Error("空 sessionID 应返回 false")
		}
	})

	t.Run("索引残留但主键不存在", func(t *testing.T) {
		client := newSessionIndexRedisClient()
		repo := NewRedisTokenRepository(client)
		session := sessionIndexSeedSession()
		// 只写反查索引，主键已过期或被轮换：索引命中但主键不存在。
		client.store.set(refreshSessionKey(session.SessionID), session.TokenHash, 0)

		removed, err := repo.DeleteRefreshSessionBySessionID(
			context.Background(),
			session.SessionID,
		)
		if err != nil {
			t.Fatalf("索引残留时不应报错，实际 err=%v", err)
		}
		if removed {
			t.Error("主键不存在时应返回 false，不得报告已撤销会话")
		}
		if _, ok := client.store.values[refreshSessionKey(session.SessionID)]; ok {
			t.Error("残留的反查索引应被清理，避免长期悬挂")
		}
	})
}

// TestDeleteRefreshSessionBySessionIDReportsRedisFailure 断言索引读取失败
// （例如 Redis 不可用）必须如实上报，不能静默当作「会话不存在」。
func TestDeleteRefreshSessionBySessionIDReportsRedisFailure(t *testing.T) {
	client := newSessionIndexRedisClient()
	client.getErr = errors.New("redis unavailable")
	repo := NewRedisTokenRepository(client)

	removed, err := repo.DeleteRefreshSessionBySessionID(context.Background(), "session-abc")
	if !errors.Is(err, client.getErr) {
		t.Fatalf("err = %v，期望原样上报 Redis 故障", err)
	}
	if removed {
		t.Error("读取失败时应返回 false")
	}
}

// TestSessionIndexPipelineErrorsAreReported 断言事务管道 Exec 失败时如实上报：
// 保存、按摘要删除与按 sid 撤销都必须原样返回错误，不得伪装成成功
// （按 sid 撤销还要额外保证返回 false，避免调用方误判会话已撤销）。
func TestSessionIndexPipelineErrorsAreReported(t *testing.T) {
	cause := errors.New("redis pipeline failed")
	client := newSessionIndexRedisClient()
	client.pipeExecErr = cause
	repo := NewRedisTokenRepository(client)
	session := sessionIndexSeedSession()

	if err := repo.SaveRefreshSession(
		context.Background(),
		session,
		60,
	); !errors.Is(err, cause) {
		t.Errorf("保存时管道失败应原样上报，实际 err=%v", err)
	}
	if err := repo.DeleteRefreshSession(
		context.Background(),
		session.TokenHash,
		session.SessionID,
	); !errors.Is(err, cause) {
		t.Errorf("按摘要删除时管道失败应原样上报，实际 err=%v", err)
	}

	// 按 sid 撤销需要先读到反查索引，因此先用正常管道写入会话，再让 Exec 失败。
	client.pipeExecErr = nil
	if err := repo.SaveRefreshSession(context.Background(), session, 60); err != nil {
		t.Fatalf("保存 refresh 会话失败：%v", err)
	}
	client.pipeExecErr = cause
	removed, err := repo.DeleteRefreshSessionBySessionID(
		context.Background(),
		session.SessionID,
	)
	if !errors.Is(err, cause) {
		t.Errorf("按 sid 撤销时管道失败应原样上报，实际 err=%v", err)
	}
	if removed {
		t.Error("管道失败时应返回 false，不得报告会话已撤销")
	}
}

// TestRedisTokenRepositoryWithoutClientReturnsClosed 断言未配置客户端时
// 新旧方法都返回 redis.ErrClosed，而不是 panic。
func TestRedisTokenRepositoryWithoutClientReturnsClosed(t *testing.T) {
	repo := NewRedisTokenRepository(nil)

	if _, err := repo.DeleteRefreshSessionBySessionID(
		context.Background(),
		"session-abc",
	); !errors.Is(err, redis.ErrClosed) {
		t.Errorf("err = %v，期望 redis.ErrClosed", err)
	}
	if err := repo.DeleteRefreshSession(
		context.Background(),
		"hash-abc",
		"session-abc",
	); !errors.Is(err, redis.ErrClosed) {
		t.Errorf("err = %v，期望 redis.ErrClosed", err)
	}
}

// keysOf 返回存储中现有键的快照，仅用于失败信息。
func keysOf(store *sessionIndexRedisStore) []string {
	keys := make([]string, 0, len(store.values))
	for key := range store.values {
		keys = append(keys, key)
	}
	return keys
}
