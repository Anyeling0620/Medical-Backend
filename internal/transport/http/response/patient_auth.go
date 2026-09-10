package response

import "Medical-Web-Backend/internal/domain/patient"

// PatientLoginResponse 是 POST /api/v1/patient/auth/wechat-login 的成功响应
// （spec/04-api-contract.md §7.1 与 §12.5）。
// refresh token 只通过 HttpOnly Cookie 返回，绝不进入响应体。
type PatientLoginResponse struct {
	IsNewUser       bool           `json:"isNewUser"`
	Patient         PatientSummary `json:"patient"`
	CardID          *int64         `json:"cardId"`
	AccessToken     string         `json:"accessToken"`
	AccessExpiresAt string         `json:"accessExpiresAt"`
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

// PatientRefreshResponse 是 POST /api/v1/patient/auth/refresh 的成功响应：
// 只返回新的 access token，轮换后的 refresh token 仍通过 HttpOnly Cookie 下发。
type PatientRefreshResponse struct {
	AccessToken     string `json:"accessToken"`
	AccessExpiresAt string `json:"accessExpiresAt"`
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
