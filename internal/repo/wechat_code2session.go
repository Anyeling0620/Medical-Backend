package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"Medical-Web-Backend/internal/port"
)

// WeChatCode2SessionClient 是 port.WeChatAuthenticator 的真实实现，
// 调用微信小程序 code2Session 接口把临时 code 换成 openid。
//
// 只做一次请求、不自动重试：微信的 code 一次性有效，重试只会拿到
// errcode=40163（code been used），重试语义交给上层与客户端决定。
type WeChatCode2SessionClient struct {
	appID  string
	secret string
	client *http.Client
}

// weChatCode2SessionURL 是微信官方的小程序登录凭据校验接口。
const weChatCode2SessionURL = "https://api.weixin.qq.com/sns/jscode2session"

// weChatCodeInvalidErrorCodes 是微信判定「code 本身不可用」的错误码集合，
// 其余错误码按服务不可用处理，避免把系统级故障当成用户操作错误。
var weChatCodeInvalidErrorCodes = map[int]bool{
	40029: true, // invalid code：code 无效
	40163: true, // code been used：code 已被使用
}

// NewWeChatCode2SessionClient 构造微信登录适配器。
// appID/secret 为空时不报错，而是在调用时返回 ErrWeChatUnavailable，
// 便于开发环境在没有微信凭据时仍能启动服务（生产环境由 bootstrap 拒绝启动）。
func NewWeChatCode2SessionClient(appID, secret string) *WeChatCode2SessionClient {
	return &WeChatCode2SessionClient{
		appID:  appID,
		secret: secret,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// redactCredential 从文案中抹去 appid 与 secret，
// 防止微信凭据随错误信息流入日志、监控或响应体。
func (c *WeChatCode2SessionClient) redactCredential(message string) string {
	if c == nil {
		return message
	}
	if c.secret != "" {
		message = strings.ReplaceAll(message, c.secret, "***")
	}
	if c.appID != "" {
		message = strings.ReplaceAll(message, c.appID, "***")
	}
	return message
}

// Code2Session 用临时 code 换取 openid。
//
// 微信成功时返回 200 + JSON（可能不带 errcode 字段），失败时返回 errcode/errmsg。
// 本方法只读取 openid：session_key 属于敏感凭据，本阶段不使用加密数据解密，
// 因此不落地、也不写日志。
func (c *WeChatCode2SessionClient) Code2Session(
	ctx context.Context,
	code string,
) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", port.ErrWeChatCodeInvalid
	}
	if c == nil || c.appID == "" || c.secret == "" {
		return "", fmt.Errorf(
			"%w: 未配置 WECHAT_APPID/WECHAT_SECRET",
			port.ErrWeChatUnavailable,
		)
	}

	endpoint, err := url.Parse(weChatCode2SessionURL)
	if err != nil {
		return "", fmt.Errorf("%w: %v", port.ErrWeChatUnavailable, err)
	}
	query := endpoint.Query()
	query.Set("appid", c.appID)
	query.Set("secret", c.secret)
	query.Set("js_code", code)
	query.Set("grant_type", "authorization_code")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		endpoint.String(),
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("%w: %v", port.ErrWeChatUnavailable, err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		// net/http 失败时返回的 *url.Error 会原样携带请求 URL，其中含 secret 与 js_code，
		// 因此必须脱敏后再包装，避免该 error 被记录时泄漏微信凭据。
		return "", fmt.Errorf(
			"%w: %s",
			port.ErrWeChatUnavailable,
			c.redactCredential(err.Error()),
		)
	}
	defer func() { _ = resp.Body.Close() }()

	// 限制读取长度，避免异常响应体占用内存。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf(
			"%w: 读取微信响应失败: %v",
			port.ErrWeChatUnavailable,
			err,
		)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf(
			"%w: 微信接口 HTTP %d",
			port.ErrWeChatUnavailable,
			resp.StatusCode,
		)
	}

	var payload struct {
		OpenID  string `json:"openid"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf(
			"%w: 解析微信响应失败: %v",
			port.ErrWeChatUnavailable,
			err,
		)
	}

	if payload.ErrCode != 0 {
		if weChatCodeInvalidErrorCodes[payload.ErrCode] {
			return "", fmt.Errorf(
				"%w: errcode=%d errmsg=%s",
				port.ErrWeChatCodeInvalid,
				payload.ErrCode,
				payload.ErrMsg,
			)
		}
		return "", fmt.Errorf(
			"%w: errcode=%d errmsg=%s",
			port.ErrWeChatUnavailable,
			payload.ErrCode,
			payload.ErrMsg,
		)
	}

	if payload.OpenID == "" {
		return "", fmt.Errorf(
			"%w: 微信未返回 openid",
			port.ErrWeChatUnavailable,
		)
	}

	return payload.OpenID, nil
}
