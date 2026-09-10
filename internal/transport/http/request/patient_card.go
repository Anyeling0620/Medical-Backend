package request

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/patient"
)

// CreatePatientCardRequest 是 POST /api/v1/patient/cards 的请求体
// （spec/04-api-contract.md §7.4）。
//
// 六个字段全部必填，缺任一项都不创建；出生日期由身份证号推导、userId 取自访问
// 令牌，因此二者都不在请求体中。字段校验不在这里做绑定断言，而是交给 domain
// 的 patient.NewCard 一次性收集，才能按契约输出 details.fields 的完整字段清单。
type CreatePatientCardRequest struct {
	Name           string   `json:"name"`
	Sex            string   `json:"sex"`
	PID            string   `json:"pid"`
	Tel            string   `json:"tel"`
	MedicalHistory []string `json:"medicalHistory"`
	InsuranceType  string   `json:"insuranceType"`
}

// UpdatePatientCardRequest 是 PATCH /api/v1/patient/cards/{cardId} 的请求体。
//
// 可修改字段用指针：nil 表示「本次未提交该字段」。JSON 的显式 null 同样会解成 nil，
// 因此 {"tel":null} 等价于「不提交 tel」，不会把已有取值清空——本项目没有「清空字段」
// 的语义，就诊卡的各字段也不允许为空。
//
// 不可修改字段（pid、userId、birthday）保留为值类型的 json.RawMessage，而不是
// *string 或 *json.RawMessage：encoding/json 遇到 null 时会把「指针字段」整体置零，
// 于是「键不存在」与「显式提交 null」在指针上完全同形；值类型则分别得到 nil 与
// []byte("null")，可以区分。契约 §7.4 要求提交 pid 返回 422 PATIENT_CARD_PID_IMMUTABLE，
// 显式的 null 同样属于「提交了不可修改字段」，不能被静默忽略
// （静默忽略会让客户端误以为修改已经生效）。
type UpdatePatientCardRequest struct {
	Name           *string   `json:"name"`
	Sex            *string   `json:"sex"`
	Tel            *string   `json:"tel"`
	MedicalHistory *[]string `json:"medicalHistory"`
	InsuranceType  *string   `json:"insuranceType"`

	// 以下字段不可修改，只在检测到「键存在」（len > 0）时用于报 422。
	// 保留原始 JSON 的副作用是取值类型不再产生 400 REQUEST_INVALID_JSON：
	// {"pid":0}、{"userId":"abc"} 这类请求会按契约 §7.4「提交即 422」处理。
	// 这三列永远不会被写入（SQL 层不参与 UPDATE），因此忽略取值类型是安全的。
	PID      json.RawMessage `json:"pid"`
	UserID   json.RawMessage `json:"userId"`
	Birthday json.RawMessage `json:"birthday"`
}

// Update 把请求体转换为领域入参 patient.CardUpdate。
//
// 不可修改字段只传递「客户端提交过该字段」这一事实：domain 的 ApplyUpdate 只判定
// 指针是否为 nil，取值本身不参与写入（repository 的 UPDATE 语句也不覆盖这些列），
// 因此这里用空值占位即可，不把客户端原文带进领域层。
func (r UpdatePatientCardRequest) Update() patient.CardUpdate {
	update := patient.CardUpdate{
		Name:           r.Name,
		Sex:            r.Sex,
		Tel:            r.Tel,
		MedicalHistory: r.MedicalHistory,
		InsuranceType:  r.InsuranceType,
	}
	if len(r.PID) > 0 {
		update.PID = submittedString()
	}
	if len(r.UserID) > 0 {
		update.UserID = submittedInt64()
	}
	if len(r.Birthday) > 0 {
		update.Birthday = submittedString()
	}
	return update
}

// submittedString / submittedInt64 返回非 nil 的占位指针，语义是「客户端提交了该字段」。
func submittedString() *string {
	placeholder := ""
	return &placeholder
}

func submittedInt64() *int64 {
	placeholder := int64(0)
	return &placeholder
}

// PatientCardListQuery 是 GET /api/v1/patient/cards 校验后的分页参数。
type PatientCardListQuery struct {
	Page     int
	PageSize int
}

// 分页默认值与边界来自契约 §1.4：page 从 1 开始、最大 100000，pageSize 最大 100。
// 与 usecase/patientcard 的 maxPage/maxPageSize 必须保持同一口径（用例层还有一次兜底校验）。
const (
	patientCardDefaultPage     = 1
	patientCardDefaultPageSize = 20
	patientCardMaxPage         = 100000
	patientCardMaxPageSize     = 100
)

// BindPatientCardList 解析并校验就诊卡列表的分页参数。
//
// 非法入参的返回文案直接作为 422 响应的 message（契约 §1.4、§12.5）：
// page=0 对应「page 必须从 1 开始」。分页窗口在这里收敛，
// 可以保证 repository 不会收到负 offset 或 limit=0。
func BindPatientCardList(c *gin.Context) (PatientCardListQuery, error) {
	result := PatientCardListQuery{
		Page:     patientCardDefaultPage,
		PageSize: patientCardDefaultPageSize,
	}

	if raw, exists := c.GetQuery("page"); exists {
		page, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			return result, errors.New("page 必须为正整数")
		case page < 1:
			return result, errors.New("page 必须从 1 开始")
		case page > patientCardMaxPage:
			return result, errors.New("page 不能超过 100000")
		}
		result.Page = page
	}

	if raw, exists := c.GetQuery("pageSize"); exists {
		pageSize, err := strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > patientCardMaxPageSize {
			return result, errors.New("pageSize 必须在 1 到 100 之间")
		}
		result.PageSize = pageSize
	}

	return result, nil
}
