package response

import (
	"strings"

	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/domain/schedule"
)

// 本文件是匿名公开查询域（/api/v1/public/*）的响应 DTO（契约 §2.3、§12.7）。
// 显式定义而不直接序列化领域实体：公开字段集合本身是契约的一部分，
// 领域结构体的调整不应静默改变匿名接口的响应形状。

// Page 是公开域列表接口的统一分页 envelope：{ items, page, pageSize, total }。
// 空列表输出 items:[] 而不是 null。
type Page[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
	Total    int64 `json:"total"`
}

// PublicPhotoURL 把数据库中的对象名补全为可访问的 MinIO 地址。
// base 或对象名为空时原样返回：未配置对象存储的本地环境不应被伪造出无效地址。
func PublicPhotoURL(base, object string) string {
	if base == "" || object == "" {
		return object
	}
	if strings.HasPrefix(object, "http://") || strings.HasPrefix(object, "https://") {
		return object
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(object, "/")
}

// PublicDepartment 是科室列表项与详情（公开字段：id、name、outpatient、description、recommended）。
type PublicDepartment struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Outpatient  bool   `json:"outpatient"`
	Description string `json:"description"`
	Recommended bool   `json:"recommended"`
}

// NewPublicDepartment 把科室实体投影为响应 DTO。
func NewPublicDepartment(item catalog.Department) PublicDepartment {
	return PublicDepartment{
		ID:          item.ID,
		Name:        item.Name,
		Outpatient:  item.Outpatient,
		Description: item.Description,
		Recommended: item.Recommended,
	}
}

// PublicSubdepartment 是子科室列表项（公开字段：id、name、departmentId、location）。
type PublicSubdepartment struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DepartmentID int64  `json:"departmentId"`
	Location     string `json:"location"`
}

// NewPublicSubdepartment 把子科室实体投影为响应 DTO。
func NewPublicSubdepartment(item catalog.Subdepartment) PublicSubdepartment {
	return PublicSubdepartment{
		ID:           item.ID,
		Name:         item.Name,
		DepartmentID: item.DepartmentID,
		Location:     item.Location,
	}
}

// PublicDoctor 是医生列表项，字段与契约 §2.3 的公开集合一致；
// pid、tel、address、email、原始 uuid 等敏感字段在这里根本不存在。
type PublicDoctor struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Sex         string `json:"sex"`
	PhotoURL    string `json:"photoUrl"`
	Degree      string `json:"degree"`
	Job         string `json:"job"`
	Description string `json:"description"`
	Recommended bool   `json:"recommended"`
}

// NewPublicDoctor 投影医生列表项，并把 photoUrl 补全为可访问地址。
func NewPublicDoctor(item catalog.PublicDoctor, photoBaseURL string) PublicDoctor {
	return PublicDoctor{
		ID:          item.ID,
		Name:        item.Name,
		Sex:         item.Sex,
		PhotoURL:    PublicPhotoURL(photoBaseURL, item.PhotoURL),
		Degree:      item.Degree,
		Job:         item.Job,
		Description: item.Description,
		Recommended: item.Recommended,
	}
}

// PublicDoctorSubdepartment 是医生详情内嵌的子科室引用。
type PublicDoctorSubdepartment struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// PublicDoctorPrice 是医生详情内嵌的价目（不含 doctorId）。
// price1/price2 在库中可空：为空时按空字符串返回，与管理端目录投影（catalog.DoctorPrice）保持一致；
// 可挂号时段的 amount 是金额字段，缺价目时按 "0.00" 返回，两者语义不同，不做统一。
type PublicDoctorPrice struct {
	ID     int64  `json:"id"`
	Level  string `json:"level"`
	Price1 string `json:"price1"`
	Price2 string `json:"price2"`
}

// PublicDoctorDetail 是医生详情：列表字段 + subdepartments + prices。
type PublicDoctorDetail struct {
	PublicDoctor
	Subdepartments []PublicDoctorSubdepartment `json:"subdepartments"`
	Prices         []PublicDoctorPrice         `json:"prices"`
}

// NewPublicDoctorDetail 投影医生详情；子科室与价目为空时输出空数组而不是 null。
func NewPublicDoctorDetail(item catalog.PublicDoctorDetail, photoBaseURL string) PublicDoctorDetail {
	detail := PublicDoctorDetail{
		PublicDoctor:   NewPublicDoctor(item.PublicDoctor, photoBaseURL),
		Subdepartments: make([]PublicDoctorSubdepartment, 0, len(item.Subdepartments)),
		Prices:         make([]PublicDoctorPrice, 0, len(item.Prices)),
	}
	for _, ref := range item.Subdepartments {
		detail.Subdepartments = append(detail.Subdepartments, PublicDoctorSubdepartment{ID: ref.ID, Name: ref.Name})
	}
	for _, price := range item.Prices {
		detail.Prices = append(detail.Prices, PublicDoctorPrice{
			ID:     price.ID,
			Level:  price.Level,
			Price1: price.Price1,
			Price2: price.Price2,
		})
	}
	return detail
}

// PublicScheduleDoctor 是可挂号时段内嵌的医生摘要。
type PublicScheduleDoctor struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Job      string `json:"job"`
	Degree   string `json:"degree"`
	PhotoURL string `json:"photoUrl"`
}

// PublicScheduleSubdepartment 是可挂号时段内嵌的子科室摘要。
type PublicScheduleSubdepartment struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// PublicSchedule 是可挂号时段项；不含 planId、workPlanId 等排班内部管理字段。
type PublicSchedule struct {
	ScheduleID    int64                       `json:"scheduleId"`
	Date          string                      `json:"date"`
	Slot          int16                       `json:"slot"`
	Maximum       int16                       `json:"maximum"`
	Remaining     int16                       `json:"remaining"`
	Amount        string                      `json:"amount"`
	Doctor        PublicScheduleDoctor        `json:"doctor"`
	Subdepartment PublicScheduleSubdepartment `json:"subdepartment"`
}

// NewPublicSchedule 投影可挂号时段，并把医生照片补全为可访问地址。
func NewPublicSchedule(item schedule.PublicSchedule, photoBaseURL string) PublicSchedule {
	return PublicSchedule{
		ScheduleID: item.ScheduleID,
		Date:       item.Date,
		Slot:       item.Slot,
		Maximum:    item.Maximum,
		Remaining:  item.Remaining,
		Amount:     item.Amount,
		Doctor: PublicScheduleDoctor{
			ID:       item.Doctor.ID,
			Name:     item.Doctor.Name,
			Job:      item.Doctor.Job,
			Degree:   item.Doctor.Degree,
			PhotoURL: PublicPhotoURL(photoBaseURL, item.Doctor.PhotoURL),
		},
		Subdepartment: PublicScheduleSubdepartment{
			ID:   item.Subdepartment.ID,
			Name: item.Subdepartment.Name,
		},
	}
}
