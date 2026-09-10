package response

import "Medical-Web-Backend/internal/domain/patient"

// PatientCardListResponse 是 GET /api/v1/patient/cards 的成功响应
// （spec/04-api-contract.md §1.1、§1.4 的统一列表结构）。
type PatientCardListResponse struct {
	Items    []patient.CardView `json:"items"`
	Page     int                `json:"page"`
	PageSize int                `json:"pageSize"`
	Total    int64              `json:"total"`
}

// NewPatientCardListResponse 把就诊卡实体投影为响应 DTO：
// pid 在 CardView 中已脱敏，tel 按患者本人接口明文返回（契约 §7.4）。
// 空列表固定输出 items: []，不输出 null。
func NewPatientCardListResponse(
	cards []patient.Card,
	page, pageSize int,
	total int64,
) PatientCardListResponse {
	items := make([]patient.CardView, 0, len(cards))
	for _, card := range cards {
		items = append(items, card.View())
	}
	return PatientCardListResponse{
		Items:    items,
		Page:     page,
		PageSize: pageSize,
		Total:    total,
	}
}
