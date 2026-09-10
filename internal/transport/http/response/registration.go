package response

import (
	domainregistration "Medical-Web-Backend/internal/domain/registration"
)

// 本文件是挂号域（/api/v1/registrations*）的响应 DTO（契约 §6.1–§6.4、§12.4）。
//
// 显式定义而不直接序列化领域实体：契约对不同接口规定了不同的字段集合
// （列表不含 workPlanId/scheduleId，详情含医生与容量摘要，prepayId 只在受保护的
// 支付详情中返回），字段集合本身是契约的一部分。

// EligibilityResponse 是资格校验响应（契约 §6.1、§12.4）。
//
// 资格不通过仍是 200：eligible=false、reasons 为稳定词表；无法计算金额时输出 null，
// reasons 为空时输出 []。
type EligibilityResponse struct {
	Eligible  bool     `json:"eligible"`
	Remaining int16    `json:"remaining"`
	Amount    *string  `json:"amount"`
	Reasons   []string `json:"reasons"`
}

// NewEligibilityResponse 把资格校验结果投影为响应 DTO。
func NewEligibilityResponse(item domainregistration.Eligibility) EligibilityResponse {
	reasons := item.Reasons
	if reasons == nil {
		reasons = make([]string, 0)
	}
	return EligibilityResponse{
		Eligible:  item.Eligible,
		Remaining: item.Remaining,
		Amount:    item.Amount,
		Reasons:   reasons,
	}
}

// RegistrationResource 是创建挂号的响应体（契约 §6.2、§12.4）：含 workPlanId 与
// scheduleId，便于前端在后续支付步骤中复用关联信息；prepayId 不在本响应中返回。
type RegistrationResource struct {
	ID              int64  `json:"id"`
	PatientCardID   int64  `json:"patientCardId"`
	WorkPlanID      int64  `json:"workPlanId"`
	ScheduleID      int64  `json:"scheduleId"`
	DoctorID        int64  `json:"doctorId"`
	SubdepartmentID int64  `json:"subdepartmentId"`
	Date            string `json:"date"`
	Slot            int16  `json:"slot"`
	Amount          string `json:"amount"`
	OutTradeNo      string `json:"outTradeNo"`
	PaymentStatus   string `json:"paymentStatus"`
	CreateDate      string `json:"createDate"`
}

// NewRegistrationResource 把挂号实体投影为创建成功的响应体。
func NewRegistrationResource(item domainregistration.Registration) RegistrationResource {
	return RegistrationResource{
		ID:              item.ID,
		PatientCardID:   item.PatientCardID,
		WorkPlanID:      item.WorkPlanID,
		ScheduleID:      item.ScheduleID,
		DoctorID:        item.DoctorID,
		SubdepartmentID: item.SubdepartmentID,
		Date:            item.Date,
		Slot:            item.Slot,
		Amount:          item.Amount,
		OutTradeNo:      item.OutTradeNo,
		PaymentStatus:   item.PaymentStatus,
		CreateDate:      item.CreateDate,
	}
}

// RegistrationItem 是挂号列表项（契约 §6.3、§12.4）：只含挂号自身字段，
// 不返回 workPlanId/scheduleId/prepayId/transactionId。
type RegistrationItem struct {
	ID              int64  `json:"id"`
	PatientCardID   int64  `json:"patientCardId"`
	DoctorID        int64  `json:"doctorId"`
	SubdepartmentID int64  `json:"subdepartmentId"`
	Date            string `json:"date"`
	Slot            int16  `json:"slot"`
	Amount          string `json:"amount"`
	OutTradeNo      string `json:"outTradeNo"`
	PaymentStatus   string `json:"paymentStatus"`
	CreateDate      string `json:"createDate"`
}

// NewRegistrationItems 把挂号实体列表投影为列表项；空输入返回空切片，
// 保证响应输出 items: [] 而不是 null（契约 §1.4）。
func NewRegistrationItems(items []domainregistration.Registration) []RegistrationItem {
	result := make([]RegistrationItem, 0, len(items))
	for _, item := range items {
		result = append(result, RegistrationItem{
			ID:              item.ID,
			PatientCardID:   item.PatientCardID,
			DoctorID:        item.DoctorID,
			SubdepartmentID: item.SubdepartmentID,
			Date:            item.Date,
			Slot:            item.Slot,
			Amount:          item.Amount,
			OutTradeNo:      item.OutTradeNo,
			PaymentStatus:   item.PaymentStatus,
			CreateDate:      item.CreateDate,
		})
	}
	return result
}

// RegistrationDoctor 是详情内嵌的医生摘要（id、name、job）。
type RegistrationDoctor struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Job  string `json:"job"`
}

// RegistrationSubdepartment 是详情内嵌的子科室摘要（id、name）。
type RegistrationSubdepartment struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// RegistrationCapacity 是详情内嵌的时段容量（maximum、used、remaining）。
type RegistrationCapacity struct {
	Maximum   int16 `json:"maximum"`
	Used      int16 `json:"used"`
	Remaining int16 `json:"remaining"`
}

// RegistrationDetail 是挂号详情（契约 §6.4、§12.4）。
type RegistrationDetail struct {
	ID            int64                     `json:"id"`
	PatientCardID int64                     `json:"patientCardId"`
	Doctor        RegistrationDoctor        `json:"doctor"`
	Subdepartment RegistrationSubdepartment `json:"subdepartment"`
	Date          string                    `json:"date"`
	Slot          int16                     `json:"slot"`
	Capacity      RegistrationCapacity      `json:"capacity"`
	Amount        string                    `json:"amount"`
	OutTradeNo    string                    `json:"outTradeNo"`
	PaymentStatus string                    `json:"paymentStatus"`
}

// NewRegistrationDetail 把挂号详情投影为响应 DTO。
func NewRegistrationDetail(item domainregistration.Detail) RegistrationDetail {
	return RegistrationDetail{
		ID:            item.ID,
		PatientCardID: item.PatientCardID,
		Doctor: RegistrationDoctor{
			ID:   item.Doctor.ID,
			Name: item.Doctor.Name,
			Job:  item.Doctor.Job,
		},
		Subdepartment: RegistrationSubdepartment{
			ID:   item.Subdepartment.ID,
			Name: item.Subdepartment.Name,
		},
		Date: item.Date,
		Slot: item.Slot,
		Capacity: RegistrationCapacity{
			Maximum:   item.Capacity.Maximum,
			Used:      item.Capacity.Used,
			Remaining: item.Capacity.Remaining,
		},
		Amount:        item.Amount,
		OutTradeNo:    item.OutTradeNo,
		PaymentStatus: item.PaymentStatus,
	}
}
