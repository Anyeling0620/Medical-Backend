// 微信 code2Session 适配器单测：把 http.RoundTripper 替换为函数，
// 在不发起真实网络请求的前提下覆盖错误码映射、参数拼装与失败分支
// （spec/04-api-contract.md §7.1、§10，port.ErrWeChatCodeInvalid / port.ErrWeChatUnavailable）。
package repo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"Medical-Web-Backend/internal/port"
)

// weChatRoundTripFunc 让用例用函数替换 http.RoundTripper 驱动适配器各分支。
type weChatRoundTripFunc func(req *http.Request) (*http.Response, error)

func (f weChatRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// newWeChatTestClient 构造注入了假 Transport 的适配器。
//
// 注意：WeChatCode2SessionClient.client 是未导出字段，且没有任何导出的注入点，
// 因此只能由同包测试直接替换（见测试报告「可注入点」一节）。
func newWeChatTestClient(
	t *testing.T,
	roundTrip weChatRoundTripFunc,
) *WeChatCode2SessionClient {
	t.Helper()
	client := NewWeChatCode2SessionClient("wx-appid", "wx-secret")
	client.client = &http.Client{Transport: roundTrip}
	return client
}

// weChatJSONResponse 构造微信 JSON 响应。
func weChatJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestWeChatCode2SessionRejectsEmptyCode 断言空 code 在本地就被拒绝，
// 不会发起任何 HTTP 请求。
func TestWeChatCode2SessionRejectsEmptyCode(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{name: "空字符串", code: ""},
		{name: "仅空白字符", code: "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewWeChatCode2SessionClient("wx-appid", "wx-secret")

			_, err := client.Code2Session(context.Background(), tc.code)
			if !errors.Is(err, port.ErrWeChatCodeInvalid) {
				t.Fatalf("err = %v，期望 port.ErrWeChatCodeInvalid", err)
			}
		})
	}
}

// TestWeChatCode2SessionRequiresCredentials 断言未配置 appid/secret（或接收者为 nil）
// 时返回 port.ErrWeChatUnavailable，而不是静默发出错误请求。
func TestWeChatCode2SessionRequiresCredentials(t *testing.T) {
	cases := []struct {
		name   string
		appID  string
		secret string
	}{
		{name: "appid 与 secret 均缺失"},
		{name: "仅缺少 secret", appID: "wx-appid"},
		{name: "仅缺少 appid", secret: "wx-secret"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewWeChatCode2SessionClient(tc.appID, tc.secret)

			_, err := client.Code2Session(context.Background(), "wx_code_abc123")
			if !errors.Is(err, port.ErrWeChatUnavailable) {
				t.Fatalf("err = %v，期望 port.ErrWeChatUnavailable", err)
			}
			if errors.Is(err, port.ErrWeChatCodeInvalid) {
				t.Error("缺少凭据不得归类为 code 非法")
			}
		})
	}

	t.Run("接收者为 nil", func(t *testing.T) {
		var client *WeChatCode2SessionClient

		_, err := client.Code2Session(context.Background(), "wx_code_abc123")
		if !errors.Is(err, port.ErrWeChatUnavailable) {
			t.Fatalf("err = %v，期望 port.ErrWeChatUnavailable", err)
		}
	})
}

// TestWeChatCode2SessionRequestParameters 断言请求参数按微信契约拼装：
// appid/secret/js_code/grant_type=authorization_code，且 code 两端的空白被裁剪。
func TestWeChatCode2SessionRequestParameters(t *testing.T) {
	var captured *http.Request

	client := newWeChatTestClient(t, func(req *http.Request) (*http.Response, error) {
		captured = req
		return weChatJSONResponse(200, `{"openid":"openid-from-wechat","session_key":"sk"}`), nil
	})

	openID, err := client.Code2Session(context.Background(), "  wx_code_abc123  ")
	if err != nil {
		t.Fatalf("正常响应应成功，实际 err=%v", err)
	}
	if openID != "openid-from-wechat" {
		t.Fatalf("openid = %q，期望 openid-from-wechat", openID)
	}
	if captured == nil {
		t.Fatal("未捕获到 HTTP 请求")
	}
	if captured.Method != http.MethodGet {
		t.Errorf("method = %s，期望 GET", captured.Method)
	}
	if captured.URL.Host != "api.weixin.qq.com" {
		t.Errorf("host = %s，期望 api.weixin.qq.com", captured.URL.Host)
	}
	if captured.URL.Path != "/sns/jscode2session" {
		t.Errorf("path = %s，期望 /sns/jscode2session", captured.URL.Path)
	}

	query := captured.URL.Query()
	want := map[string]string{
		"appid":      "wx-appid",
		"secret":     "wx-secret",
		"js_code":    "wx_code_abc123",
		"grant_type": "authorization_code",
	}
	for key, value := range want {
		if got := query.Get(key); got != value {
			t.Errorf("query %s = %q，期望 %q", key, got, value)
		}
	}
}

// TestWeChatCode2SessionErrorCodeMapping 覆盖微信错误码与异常响应的归类：
// code 不可用（40029/40163）→ ErrWeChatCodeInvalid；其余错误与系统级故障 → ErrWeChatUnavailable。
func TestWeChatCode2SessionErrorCodeMapping(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantErr  error
		wantNoOp error
	}{
		{
			name:    "errcode 40029 invalid code",
			status:  200,
			body:    `{"errcode":40029,"errmsg":"invalid code"}`,
			wantErr: port.ErrWeChatCodeInvalid,
		},
		{
			name:    "errcode 40163 code been used",
			status:  200,
			body:    `{"errcode":40163,"errmsg":"code been used"}`,
			wantErr: port.ErrWeChatCodeInvalid,
		},
		{
			name:    "errcode -1 系统繁忙",
			status:  200,
			body:    `{"errcode":-1,"errmsg":"system error"}`,
			wantErr: port.ErrWeChatUnavailable,
		},
		{
			name:    "HTTP 500",
			status:  500,
			body:    `{"errcode":-1}`,
			wantErr: port.ErrWeChatUnavailable,
		},
		{
			name:    "响应体不是 JSON",
			status:  200,
			body:    `<html>bad gateway</html>`,
			wantErr: port.ErrWeChatUnavailable,
		},
		{
			name:    "成功但未返回 openid",
			status:  200,
			body:    `{"session_key":"sk"}`,
			wantErr: port.ErrWeChatUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newWeChatTestClient(t, func(*http.Request) (*http.Response, error) {
				return weChatJSONResponse(tc.status, tc.body), nil
			})

			_, err := client.Code2Session(context.Background(), "wx_code_abc123")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if tc.wantErr == port.ErrWeChatCodeInvalid &&
				errors.Is(err, port.ErrWeChatUnavailable) {
				t.Error("code 不可用不得同时归类为服务不可用")
			}
		})
	}
}

// TestWeChatCode2SessionNetworkFailure 断言网络层失败归类为 ErrWeChatUnavailable。
func TestWeChatCode2SessionNetworkFailure(t *testing.T) {
	client := newWeChatTestClient(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})

	_, err := client.Code2Session(context.Background(), "wx_code_abc123")
	if !errors.Is(err, port.ErrWeChatUnavailable) {
		t.Fatalf("err = %v，期望 port.ErrWeChatUnavailable", err)
	}
}
