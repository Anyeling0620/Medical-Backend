package request

// WeChatLoginRequest 是 POST /api/v1/patient/auth/wechat-login 的请求体。
// code 为 wx.login 换取的一次性临时授权字符串，长度校验在 use case 完成（契约 §7.1）。
//
// OpenID 只服务于测试阶段的直通登录：openid 只能由微信签发，正式环境必须走 code，
// 是否放行由 transport 层按 APP_ENV 判定（契约 §7.1 的 development 例外说明）。
// code 与 openid 至少提交一个；同时提交时以 code 为准。
type WeChatLoginRequest struct {
	Code   string `json:"code"`
	OpenID string `json:"openid"`
}

// PatientRefreshRequest 是患者 refresh/logout 的可选请求体。
// refresh token 允许通过 Cookie 或请求体提交（契约 §7.2 明文允许两种来源）：
// 小程序侧不能依赖 Cookie 通道，因此请求体是必需的第二来源；
// 但 refresh token 不得写入 localStorage 等可被脚本读取的位置（契约 §1.2）。
type PatientRefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}
