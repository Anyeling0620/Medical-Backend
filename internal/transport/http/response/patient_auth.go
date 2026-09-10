package response

import "Medical-Web-Backend/internal/domain/patient"

// PatientLoginResponse 是 POST /api/v1/patient/auth/wechat-login 的成功响应
// （spec/04-api-contract.md §7.1 与 §12.5）。
//
// refresh token 同时经两条通道下发：HttpOnly Cookie（浏览器）与响应体
// （微信小程序：wx.request 不携带 Cookie）。响应体字段只服务于小程序通道，
// 浏览器端应忽略它并继续使用 Cookie。
type PatientLoginResponse struct {
	IsNewUser       bool           `json:"isNewUser"`
	Patient         PatientSummary `json:"patient"`
	CardID          *int64         `json:"cardId"`
	AccessToken     string         `json:"accessToken"`
	AccessExpiresAt string         `json:"accessExpiresAt"`
	// RefreshToken 是本次登录的 refresh 令牌原文，供小程序本地保存并续期；
	// 必须存放在 App 沙箱存储，不得写入浏览器可读的 localStorage（契约 §1.2）。
	RefreshToken string `json:"refreshToken"`
	// RefreshExpiresAt 是该 refresh 会话的过期时刻（RFC3339），
	// 供客户端在过期前主动重新登录，避免一次注定失败的刷新。
	RefreshExpiresAt string `json:"refreshExpiresAt"`
}

// PatientSummary 是 patient_user 的对外投影：openId 仅服务端保存，
// 因此本 DTO 里根本没有该字段（spec §2.2 禁止返回字段）。
type PatientSummary struct {
	ID         int64   `json:"id"`
	Nickname   *string `json:"nickname"`
	Photo      *string `json:"photo"`
	Sex        *string `json:"sex"`
	Status     string  `json:"status"`
	CreateDate string  `json:"createDate"`
}

// NewPatientSummary 把患者实体投影为响应 DTO；profile 为 nil 时返回零值。
func NewPatientSummary(profile *patient.Patient) PatientSummary {
	if profile == nil {
		return PatientSummary{}
	}
	return PatientSummary{
		ID:         profile.ID,
		Nickname:   profile.Nickname,
		Photo:      profile.Photo,
		Sex:        profile.Sex,
		Status:     profile.Status,
		CreateDate: profile.CreateDate,
	}
}

// PatientRefreshResponse 是 POST /api/v1/patient/auth/refresh 的成功响应。
//
// 轮换后的 refresh token 同时经 HttpOnly Cookie（浏览器）与响应体（小程序）下发：
// 小程序读不到 Cookie，若响应体不回传新令牌，第一次刷新后便无凭据可继续续期。
type PatientRefreshResponse struct {
	AccessToken      string `json:"accessToken"`
	AccessExpiresAt  string `json:"accessExpiresAt"`
	RefreshToken     string `json:"refreshToken"`
	RefreshExpiresAt string `json:"refreshExpiresAt"`
}

// PatientMeResponse 是 GET /api/v1/patient/me 的成功响应（spec/04-api-contract.md §7.3）。
//
// 显式定义 DTO 而不是直接序列化领域实体：领域字段的调整不应静默改变 HTTP 契约，
// 也让「cardId/cardCount/tel 的空值语义」在一处集中说明。
type PatientMeResponse struct {
	ID         int64   `json:"id"`
	Nickname   *string `json:"nickname"`
	Photo      *string `json:"photo"`
	Sex        *string `json:"sex"`
	Status     string  `json:"status"`
	CreateDate string  `json:"createDate"`
	// CardID 为 null 表示尚未实名建卡，前端据此跳转实名流程。
	CardID *int64 `json:"cardId"`
	// CardCount 只会是 0 或 1。
	CardCount int `json:"cardCount"`
	// Tel 来自本人就诊卡，明文返回；无卡时为 null。
	Tel *string `json:"tel"`
}

// NewPatientMeResponse 把领域投影转换为响应 DTO；me 为 nil 时返回零值。
func NewPatientMeResponse(me *patient.PatientMe) PatientMeResponse {
	if me == nil {
		return PatientMeResponse{}
	}
	return PatientMeResponse{
		ID:         me.ID,
		Nickname:   me.Nickname,
		Photo:      me.Photo,
		Sex:        me.Sex,
		Status:     me.Status,
		CreateDate: me.CreateDate,
		CardID:     me.CardID,
		CardCount:  me.CardCount,
		Tel:        me.Tel,
	}
}
