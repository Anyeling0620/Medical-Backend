package request

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"

	domainmedicalrecord "Medical-Web-Backend/internal/domain/medical_record"
)

// 本文件是病历域（/api/v1/medical-records*）的请求绑定（spec/04-api-contract.md 病历一节）。
//
// 字段级校验在这里收敛成面向用户的中文文案，handler 统一映射为 422 REQUEST_VALIDATION_FAILED；
// 归属、授权与占位规则由 use case 判定。归属字段（doctorId/patientCardId/subDepartmentId）
// 不接受客户端提交：它们一律由服务端从挂号记录推导，避免把病历写到他人名下。

// CreateMedicalRecordRequest 是 POST /api/v1/medical-records 的请求体。
//
// registrationId 用指针承载：nil 表示未提交，可据此给出「必传字段」而不是「取值为 0」的提示。
// content 是医生自行整理的病历正文，允许换行与自定义排版。
type CreateMedicalRecordRequest struct {
	RegistrationID *int64 `json:"registrationId"`
	Diagnosis      string `json:"diagnosis"`
	Content        string `json:"content"`
}

// BindCreateMedicalRecord 严格解析书写病历请求体：拒绝未知字段，
// registrationId 必传且为正整数，diagnosis 与 content 必传且不超过长度上限。
func BindCreateMedicalRecord(c *gin.Context) (CreateMedicalRecordRequest, error) {
	var body CreateMedicalRecordRequest
	if err := decodeStrict(c, &body); err != nil {
		return body, err
	}
	if body.RegistrationID == nil {
		return body, errors.New("registrationId 为必传字段")
	}
	if *body.RegistrationID < 1 {
		return body, errors.New("registrationId 必须为正整数")
	}
	if _, err := medicalRecordDiagnosis(body.Diagnosis); err != nil {
		return body, err
	}
	if _, err := medicalRecordContent(body.Content); err != nil {
		return body, err
	}
	return body, nil
}

// UpdateMedicalRecordRequest 是 PATCH /api/v1/medical-records/{medicalRecordId} 的请求体。
//
// 两个字段都用指针承载：nil 表示本次不修改该字段（PATCH 语义），
// 因此显式提交空串会得到 422（诊断/正文不允许为空），而不是被当成「不修改」。
type UpdateMedicalRecordRequest struct {
	Diagnosis *string `json:"diagnosis"`
	Content   *string `json:"content"`
}

// BindUpdateMedicalRecord 严格解析修改请求体：至少提交一个字段，且提交的字段必须合法。
func BindUpdateMedicalRecord(c *gin.Context) (UpdateMedicalRecordRequest, error) {
	var body UpdateMedicalRecordRequest
	if err := decodeStrict(c, &body); err != nil {
		return body, err
	}
	if body.Diagnosis == nil && body.Content == nil {
		return body, errors.New("至少提交 diagnosis 或 content 中的一个字段")
	}
	if body.Diagnosis != nil {
		if _, err := medicalRecordDiagnosis(*body.Diagnosis); err != nil {
			return body, err
		}
	}
	if body.Content != nil {
		if _, err := medicalRecordContent(*body.Content); err != nil {
			return body, err
		}
	}
	return body, nil
}

// MedicalRecordListQuery 是 GET /api/v1/medical-records 校验后的查询参数（契约 §1.4）。
//
// 这里没有 sort/order：doctor_prescription 没有时间列，排序固定为 id 倒序（最近书写的在前），
// 不接受客户端指定排序字段，避免暴露无意义的排序口径。
type MedicalRecordListQuery struct {
	RegistrationID *int64
	PatientCardID  *int64
	DoctorID       *int64
	Page           int
	PageSize       int
}

// BindMedicalRecordList 解析并校验病历列表查询参数。
// 默认值：page=1、pageSize=20；三个编号参数都是可选的正整数过滤条件。
func BindMedicalRecordList(c *gin.Context) (MedicalRecordListQuery, error) {
	q := MedicalRecordListQuery{Page: 1, PageSize: 20}

	var err error
	if q.RegistrationID, err = optionalPositiveID(c, "registrationId"); err != nil {
		return q, err
	}
	if q.PatientCardID, err = optionalPositiveID(c, "patientCardId"); err != nil {
		return q, err
	}
	if q.DoctorID, err = optionalPositiveID(c, "doctorId"); err != nil {
		return q, err
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
	// doctor_prescription 没有时间列，排序固定为 id 倒序：显式拒绝 sort/order，
	// 避免客户端以为自定义排序生效（契约 §1.4 的统一查询参数在本域不适用）。
	if _, exists := c.GetQuery("sort"); exists {
		return q, errors.New("本接口不支持 sort 参数（固定按 id 倒序）")
	}
	if _, exists := c.GetQuery("order"); exists {
		return q, errors.New("本接口不支持 order 参数（固定按 id 倒序）")
	}
	return q, nil
}

// medicalRecordDiagnosis 校验诊断并返回归一化结果（去除首尾空白）。
func medicalRecordDiagnosis(raw string) (string, error) {
	value, err := domainmedicalrecord.NormalizeDiagnosis(raw)
	if err == nil {
		return value, nil
	}
	if errors.Is(err, domainmedicalrecord.ErrDiagnosisRequired) {
		return "", errors.New("diagnosis 为必传字段")
	}
	return "", fmt.Errorf("diagnosis 不能超过 %d 个字符", domainmedicalrecord.MaxDiagnosisRunes)
}

// medicalRecordContent 校验病历正文并返回归一化结果（仅裁剪首尾空白，保留内部排版）。
func medicalRecordContent(raw string) (string, error) {
	value, err := domainmedicalrecord.NormalizeContent(raw)
	if err == nil {
		return value, nil
	}
	if errors.Is(err, domainmedicalrecord.ErrContentRequired) {
		return "", errors.New("content 为必传字段")
	}
	return "", fmt.Errorf("content 不能超过 %d 个字符", domainmedicalrecord.MaxContentRunes)
}
