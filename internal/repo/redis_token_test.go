package repo

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"

	"Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/port"
)

// fakeRedisClient 是 redis.UniversalClient 的最小测试替身：只覆写 GetRefreshSession
// 真正会用到的 Get，其余方法沿用内嵌接口（未实现，一旦被调用会 panic，便于发现实现
// 越界访问了别的 Redis 命令）。这样可以不引入 miniredis 等新依赖。
type fakeRedisClient struct {
	redis.UniversalClient
	get func(ctx context.Context, key string) *redis.StringCmd
}

// Get 返回预先编排好的结果，模拟 Redis 的各种应答（命中、键不存在、连接故障）。
func (f fakeRedisClient) Get(ctx context.Context, key string) *redis.StringCmd {
	return f.get(ctx, key)
}

// TestGetRefreshSessionKeyNotFound 覆盖「键不存在」分支：Redis 返回 redis.Nil 时必须
// 转换成 port.ErrRefreshSessionNotFound 哨兵错误，上层登出流程据此把已过期/已轮换的
// 会话当成幂等空操作，而不是上报故障。
func TestGetRefreshSessionKeyNotFound(t *testing.T) {
	const tokenHash = "hash-missing"
	var gotKey string
	client := fakeRedisClient{get: func(_ context.Context, key string) *redis.StringCmd {
		gotKey = key
		// NewStringResult 是 go-redis 自带的测试构造器，Bytes() 会原样抛出该错误。
		return redis.NewStringResult("", redis.Nil)
	}}

	session, err := NewRedisTokenRepository(client).GetRefreshSession(context.Background(), tokenHash)

	if session != nil {
		t.Fatalf("键不存在时应返回 nil 会话，got %+v", session)
	}
	if !errors.Is(err, port.ErrRefreshSessionNotFound) {
		t.Fatalf("err = %v, want %v", err, port.ErrRefreshSessionNotFound)
	}
	if gotKey != refreshKey(tokenHash) {
		t.Errorf("查询键 = %q, want %q", gotKey, refreshKey(tokenHash))
	}
}

// TestGetRefreshSessionReadErrorPropagated 覆盖「真正的读取失败」分支：必须原样返回
// 底层错误，不得替换成哨兵错误，否则 Redis 故障会被误判成「会话已过期」，登出接口
// 就会静默返回 204 把故障掩盖掉。
func TestGetRefreshSessionReadErrorPropagated(t *testing.T) {
	customErr := errors.New("redis 连接中断")
	cases := []struct {
		name string
		err  error
	}{
		{"客户端已关闭", redis.ErrClosed},
		{"自定义错误", customErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fakeRedisClient{get: func(context.Context, string) *redis.StringCmd {
				return redis.NewStringResult("", tc.err)
			}}

			session, err := NewRedisTokenRepository(client).GetRefreshSession(context.Background(), "hash-error")

			if session != nil {
				t.Fatalf("读取失败时应返回 nil 会话，got %+v", session)
			}
			if err != tc.err {
				t.Fatalf("应原样返回底层错误，got %#v, want %#v", err, tc.err)
			}
			if errors.Is(err, port.ErrRefreshSessionNotFound) {
				t.Errorf("读取失败不得被替换成哨兵错误")
			}
		})
	}
}

// TestGetRefreshSessionDecodePayload 覆盖正常路径：命中键时按 JSON 反序列化为
// port.RefreshSession。注意 TokenHash 字段带 json:"-" 标签，既不参与写入也不参与
// 读取，调用方必须用入参 tokenHash 来标识会话，因此本用例同时断言载荷里的
// tokenHash 会被忽略（保持空值）。
func TestGetRefreshSessionDecodePayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    port.RefreshSession
	}{
		{
			name:    "患者端会话",
			payload: `{"tokenHash":"should-be-ignored","sessionId":"sess-patient","userId":7,"username":"patient_a","realm":"patient"}`,
			want: port.RefreshSession{
				SessionID: "sess-patient",
				UserID:    7,
				Username:  "patient_a",
				Realm:     auth.RealmPatient,
			},
		},
		{
			name:    "管理端会话",
			payload: `{"sessionId":"sess-mis","userId":1,"username":"admin","realm":"mis"}`,
			want: port.RefreshSession{
				SessionID: "sess-mis",
				UserID:    1,
				Username:  "admin",
				Realm:     auth.RealmMis,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fakeRedisClient{get: func(context.Context, string) *redis.StringCmd {
				return redis.NewStringResult(tc.payload, nil)
			}}

			session, err := NewRedisTokenRepository(client).GetRefreshSession(context.Background(), "hash-hit")
			if err != nil {
				t.Fatalf("正常载荷不应报错，got %v", err)
			}
			if session == nil {
				t.Fatalf("命中键时应返回会话，got nil")
			}
			if *session != tc.want {
				t.Fatalf("会话 = %+v, want %+v", *session, tc.want)
			}
			if session.TokenHash != "" {
				t.Errorf("TokenHash 带 json:\"-\"，反序列化时不应被填充，got %q", session.TokenHash)
			}
			if !session.Realm.Valid() {
				t.Errorf("反序列化出的 realm = %q，不是契约允许的取值", session.Realm)
			}
		})
	}
}

// TestGetRefreshSessionInvalidPayload 覆盖「载荷不是合法 JSON」与「字段类型不匹配」：
// 反序列化错误必须照实返回（非哨兵错误），也不能返回半成品会话，避免上层把损坏的
// 会话当成「会话不存在」而静默跳过。
func TestGetRefreshSessionInvalidPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"非法 JSON", `{"sessionId":`},
		{"字段类型不匹配", `{"sessionId":"sess-1","userId":"not-a-number"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fakeRedisClient{get: func(context.Context, string) *redis.StringCmd {
				return redis.NewStringResult(tc.payload, nil)
			}}

			session, err := NewRedisTokenRepository(client).GetRefreshSession(context.Background(), "hash-broken")

			if session != nil {
				t.Fatalf("反序列化失败时应返回 nil 会话，got %+v", session)
			}
			if err == nil {
				t.Fatalf("非法载荷必须报错")
			}
			if errors.Is(err, port.ErrRefreshSessionNotFound) {
				t.Errorf("反序列化失败不得被替换成哨兵错误")
			}
			if errors.Is(err, redis.Nil) {
				t.Errorf("反序列化失败不得被误判为键不存在：%v", err)
			}
			var syntaxErr *json.SyntaxError
			var typeErr *json.UnmarshalTypeError
			if !errors.As(err, &syntaxErr) && !errors.As(err, &typeErr) {
				t.Errorf("应返回 encoding/json 的解析错误，got %v", err)
			}
		})
	}
}

// TestGetRefreshSessionNilClient 覆盖未注入 Redis 客户端（含 nil 接收者）的场景：
// 必须返回 redis.ErrClosed，既不 panic，也不能退化成「会话不存在」。
func TestGetRefreshSessionNilClient(t *testing.T) {
	t.Run("客户端为 nil", func(t *testing.T) {
		session, err := NewRedisTokenRepository(nil).GetRefreshSession(context.Background(), "hash-nil")

		if session != nil {
			t.Fatalf("客户端为 nil 时应返回 nil 会话，got %+v", session)
		}
		if !errors.Is(err, redis.ErrClosed) {
			t.Fatalf("err = %v, want %v", err, redis.ErrClosed)
		}
		if errors.Is(err, port.ErrRefreshSessionNotFound) {
			t.Errorf("客户端为 nil 不得被当成会话不存在")
		}
	})

	t.Run("接收者为 nil", func(t *testing.T) {
		var r *RedisTokenRepository
		session, err := r.GetRefreshSession(context.Background(), "hash-nil")

		if session != nil {
			t.Fatalf("接收者为 nil 时应返回 nil 会话，got %+v", session)
		}
		if !errors.Is(err, redis.ErrClosed) {
			t.Fatalf("err = %v, want %v", err, redis.ErrClosed)
		}
	})
}
