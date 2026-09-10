package request

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"

	domainregistration "Medical-Web-Backend/internal/domain/registration"
)

// 本文件是挂号域（/api/v1/registrations*）的请求绑定（spec/04-api-contract.md §6.1–§6.3）。
// 字段级校验在这里收敛成面向用户的中文文案，handler 统一映射为 422 REQUEST_VALIDATION_FAILED；
// 取值语义（归属、资格、状态）由 use case 判定。

// EligibilityRequest 是 POST /api/v1/registrations/eligibility 的请求体（契约 §6.1、§12.4）。
//
// 两个字段都用指针承载：nil 表示请求未提交该字段，可据此与「提交了 0」区分并给出
// 「必传字段」而不是「取值为 0」的提示。
type EligibilityRequest struct {
	PatientCardID *int64 `json:"patientCardId"`
	ScheduleID    *int64 `json:"scheduleId"`
}

// BindEligibility 严格解析资格校验请求体：拒绝未知字段与多余内容，
// patientCardId 与 scheduleId 均为必传且必须为正整数。
func BindEligibility(c *gin.Context) (EligibilityRequest, error) {
	var body EligibilityRequest
	if err := decodeStrict(c, &body); err != nil {
		return body, err
	}
	if body.PatientCardID == nil {
		return body, errors.New("patientCardId 为必传字段")
	}
	if *body.PatientCardID < 1 {
		return body, errors.New("patientCardId 必须为正整数")
	}
	if body.ScheduleID == nil {
		return body, errors.New("scheduleId 为必传字段")
	}
	if *body.ScheduleID < 1 {
		return body, errors.New("scheduleId 必须为正整数")
	}
	return body, nil
}

// CreateRegistrationRequest 是 POST /api/v1/registrations 的请求体（契约 §6.2、§12.4）。
//
// patientCardId 为 nil 时表示请求未提交：患者端由服务端从当前患者推导，
// 管理端代建则返回 422（字段级必填判定放在 use case，避免请求层依赖 realm）。
type CreateRegistrationRequest struct {
	PatientCardID *int64 `json:"patientCardId"`
	ScheduleID    *int64 `json:"scheduleId"`
	PaymentMethod string `json:"paymentMethod"`
}

// BindCreateRegistration 严格解析建单请求体：scheduleId 为必传且必须为正整数，
// paymentMethod 必传；patientCardId 的必填与否取决于调用者 realm（见 use case）。
func BindCreateRegistration(c *gin.Context) (CreateRegistrationRequest, error) {
	var body CreateRegistrationRequest
	if err := decodeStrict(c, &body); err != nil {
		return body, err
	}
	if body.ScheduleID == nil {
		return body, errors.New("scheduleId 为必传字段")
	}
	if *body.ScheduleID < 1 {
		return body, errors.New("scheduleId 必须为正整数")
	}
	if body.PatientCardID != nil && *body.PatientCardID < 1 {
		return body, errors.New("patientCardId 必须为正整数")
	}
	return body, nil
}

// RegistrationListQuery 是 GET /api/v1/registrations 校验后的查询参数（契约 §6.3、§1.4）。
type RegistrationListQuery struct {
	PatientCardID   *int64
	DoctorID        *int64
	SubdepartmentID *int64
	FromDate        string
	ToDate          string
	PaymentStatus   *int16
	Page            int
	PageSize        int
	Sort            string
	Order           string
}

// BindRegistrationList 解析并校验挂号列表查询参数。
//
// 默认值：page=1、pageSize=20、sort=createDate、order=desc（最近挂号在前）；
// paymentStatus 只接受 UNPAID|PAID|REFUNDED|EXPIRED，其余取值按契约 §12.4
// 返回「支付状态不支持」；sort 只来自白名单，避免拼接 SQL（契约 §1.4）。
func BindRegistrationList(c *gin.Context) (RegistrationListQuery, error) {
	q := RegistrationListQuery{
		Page:     1,
		PageSize: 20,
		Sort:     domainregistration.SortCreateDate,
		Order:    "desc",
	}

	var err error
	if q.PatientCardID, err = optionalPositiveID(c, "patientCardId"); err != nil {
		return q, err
	}
	if q.DoctorID, err = optionalPositiveID(c, "doctorId"); err != nil {
		return q, err
	}
	if q.SubdepartmentID, err = optionalPositiveID(c, "subdepartmentId"); err != nil {
		return q, err
	}
	if q.FromDate, err = optionalDateParam(c, "fromDate"); err != nil {
		return q, err
	}
	if q.ToDate, err = optionalDateParam(c, "toDate"); err != nil {
		return q, err
	}
	// fromDate 不得晚于 toDate（闭区间，契约 §1.4）。
	if q.FromDate != "" && q.ToDate != "" && q.FromDate > q.ToDate {
		return q, errors.New("日期范围无效")
	}

	if raw, exists := c.GetQuery("paymentStatus"); exists {
		code, ok := domainregistration.PaymentStatusCode(raw)
		if !ok {
			return q, errors.New("支付状态不支持")
		}
		q.PaymentStatus = &code
	}
	if raw, exists := c.GetQuery("page"); exists {
		page, convErr := strconv.Atoi(raw)
		if convErr != nil || page < 1 {
			return q, errors.New("page 必须从 1 开始")
		}
		if page > 100000 {
			return q, errors.New("page 不能超过 100000")
		}
		q.Page = page
	}
	if raw, exists := c.GetQuery("pageSize"); exists {
		pageSize, convErr := strconv.Atoi(raw)
		if convErr != nil || pageSize < 1 || pageSize > 100 {
			return q, errors.New("pageSize 必须在 1 到 100 之间")
		}
		q.PageSize = pageSize
	}
	if raw, exists := c.GetQuery("sort"); exists {
		switch raw {
		case domainregistration.SortCreateDate, domainregistration.SortDate, domainregistration.SortID:
			q.Sort = raw
		default:
			return q, errors.New("sort 只支持 createDate/date/id")
		}
	}
	if raw, exists := c.GetQuery("order"); exists {
		switch raw {
		case "asc", "desc":
			q.Order = raw
		default:
			return q, errors.New("order 只支持 asc/desc")
		}
	}
	return q, nil
}

// optionalPositiveID 读取可选的正整数编号参数，缺省返回 nil。
func optionalPositiveID(c *gin.Context, name string) (*int64, error) {
	raw, exists := c.GetQuery(name)
	if !exists {
		return nil, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1 {
		return nil, errors.New(name + " 必须为正整数")
	}
	return &value, nil
}
