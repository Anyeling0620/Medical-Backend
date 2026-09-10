package port

import (
	"context"
	"errors"
)

var (
	// ErrWeChatCodeInvalid 表示微信判定 code 无效、已被使用或已过期。
	// 上层映射为 422 REQUEST_VALIDATION_FAILED，提示用户重新发起微信登录。
	ErrWeChatCodeInvalid = errors.New("wechat login code is invalid or expired")
	// ErrWeChatUnavailable 表示微信接口不可达、返回系统级错误或本服务未配置凭据。
	// 上层映射为 502 DEPENDENCY_UNAVAILABLE（可重试）。
	ErrWeChatUnavailable = errors.New("wechat service is unavailable")
)

// WeChatAuthenticator 描述「小程序临时 code 换取 openid」的外部服务能力。
//
// openid 只能由微信签发，服务端不得凭空构造，本接口也不提供开发期直通实现：
// 本地与自动化测试通过替换本接口的实现（测试桩）来隔离外部依赖
// （spec/流程.md 患者身份批次、spec/02-architecture.md「外部服务适配」）。
//
// 测试阶段（APP_ENV=development）另有一条不经本接口的 openid 直通登录，
// 由 patientauth.LoginByOpenID 提供、transport 层按 APP_ENV 拦截（契约 §7.1）。
type WeChatAuthenticator interface {
	// Code2Session 用 wx.login 得到的临时 code 换取 openid。
	// code 为空返回 ErrWeChatCodeInvalid；微信判定 code 不可用同样返回该错误；
	// 网络失败、未配置凭据或微信返回系统级错误返回 ErrWeChatUnavailable。
	Code2Session(ctx context.Context, code string) (string, error)
}
