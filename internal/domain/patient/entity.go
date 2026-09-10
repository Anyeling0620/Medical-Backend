// Package patient 定义患者域（patient_user、patient_user_info_card、
// patient_face_auth）的实体与业务规则。
//
// 患者端（微信小程序）与管理端共用同一套服务端实现，但患者域的主体只能是当前
// 登录的 patient_user 本人：就诊卡一律按 user_id 判定归属，越权访问按“资源不存在”
// 处理（spec/04-api-contract.md §1.2、§7）。本包只做纯领域计算，不依赖数据库、
// HTTP 或第三方 SDK；持久化由 internal/repo 实现 internal/port 中声明的接口。
package patient

// 患者账号状态。hospital.patient_user.status 是 SMALLINT：1 正常、2 禁用
// （见 Medical-Web-Backend/init.sql 建表注释）。
const (
	// StatusCodeActive 是数据库中的“正常”状态值。
	StatusCodeActive int16 = 1
	// StatusCodeDisabled 是数据库中的“禁用”状态值。
	StatusCodeDisabled int16 = 2
)

// 患者账号状态对外字符串语义，与 doctor.status 的 ACTIVE/HIDDEN 风格保持一致
// （spec/04-api-contract.md §7.3 响应中的 status 字段）。
const (
	StatusActive   = "ACTIVE"
	StatusDisabled = "DISABLED"
)

// 性别取值。就诊卡创建时，性别必须与身份证号第 17 位的奇偶性一致。
const (
	SexMale   = "男"
	SexFemale = "女"
)

// Patient 映射 hospital.patient_user。
//
// OpenID 是微信登录凭据，仅服务端内部使用：spec/04-api-contract.md §2.2 把 openId
// 列为禁止返回字段。这里标注 json:"-" 作为兜底，即使有人直接序列化实体也不会泄露。
// Nickname/Photo/Sex 在库中可空，用指针区分“库中为 NULL”和“空字符串”。
type Patient struct {
	ID         int64   `json:"id"`
	OpenID     string  `json:"-"`
	Nickname   *string `json:"nickname"`
	Photo      *string `json:"photo"`
	Sex        *string `json:"sex"`
	Status     string  `json:"status"`
	CreateDate string  `json:"createDate"`
}

// IsActive 报告账号当前是否可用（只有 ACTIVE 才允许签发令牌）。
func (p Patient) IsActive() bool {
	return p.Status == StatusActive
}

// Card 映射 hospital.patient_user_info_card。
//
// 对外响应一律使用 Card.View() 得到 CardView，本类型不用于直接序列化：
// PID 与 Tel 都标注了 json:"-"，其中 PID 是完整身份证号（脱敏只发生在 CardView 上），
// Tel 按契约只允许出现在患者本人接口（spec/04-api-contract.md §2.2）。
// 万一有人直接 c.JSON(200, card)，响应会因为缺少 pid/tel 而在测试或评审中立刻暴露，
// 而不是静默把敏感字段发出去。Birthday 由身份证号推导，不接受客户端提交。
type Card struct {
	ID             int64    `json:"id"`
	UserID         int64    `json:"userId"`
	UUID           string   `json:"uuid"`
	Name           string   `json:"name"`
	Sex            string   `json:"sex"`
	PID            string   `json:"-"`
	Tel            string   `json:"-"`
	Birthday       string   `json:"birthday"`
	MedicalHistory []string `json:"medicalHistory"`
	InsuranceType  string   `json:"insuranceType"`
	ExistFaceModel bool     `json:"existFaceModel"`
}

// BelongsTo 报告就诊卡是否属于指定患者账号。
// 归属判定只依赖 user_id：契约要求“他人就诊卡与不存在统一处理”，
// 因此调用方必须用本方法而不是“查到就返回”来决定是否放行。
func (c Card) BelongsTo(patientID int64) bool {
	return c.UserID == patientID
}

// View 把就诊卡投影成对外响应：pid 脱敏，其余字段一一对应
// （spec/04-api-contract.md §7.4）。
func (c Card) View() CardView {
	return CardView{
		ID:             c.ID,
		UserID:         c.UserID,
		UUID:           c.UUID,
		Name:           c.Name,
		Sex:            c.Sex,
		PID:            MaskPID(c.PID),
		Tel:            c.Tel,
		Birthday:       c.Birthday,
		MedicalHistory: c.MedicalHistory,
		InsuranceType:  c.InsuranceType,
		ExistFaceModel: c.ExistFaceModel,
	}
}

// CardView 是就诊卡对外的响应投影：pid 已脱敏，且不含任何服务端内部字段。
type CardView struct {
	ID             int64    `json:"id"`
	UserID         int64    `json:"userId"`
	UUID           string   `json:"uuid"`
	Name           string   `json:"name"`
	Sex            string   `json:"sex"`
	PID            string   `json:"pid"`
	Tel            string   `json:"tel"`
	Birthday       string   `json:"birthday"`
	MedicalHistory []string `json:"medicalHistory"`
	InsuranceType  string   `json:"insuranceType"`
	ExistFaceModel bool     `json:"existFaceModel"`
}

// PatientMe 是 GET /api/v1/patient/me 的响应（spec/04-api-contract.md §7.3）。
//
// CardID 为 nil 表示尚未实名建卡，前端据此跳转实名流程；CardCount 只会是 0 或 1；
// Tel 来自本人就诊卡并按明文返回，无卡时为 nil。实名状态由“是否存在就诊卡”推导，
// 不新增数据库字段（spec/03-domain-and-state.md §患者与会话域）。
type PatientMe struct {
	ID         int64   `json:"id"`
	Nickname   *string `json:"nickname"`
	Photo      *string `json:"photo"`
	Sex        *string `json:"sex"`
	Status     string  `json:"status"`
	CreateDate string  `json:"createDate"`
	CardID     *int64  `json:"cardId"`
	CardCount  int     `json:"cardCount"`
	Tel        *string `json:"tel"`
}

// FaceAuthRecord 是人脸认证记录（patient_face_auth）中的一条日期记录。
// 本阶段只记录日期，不上传人脸模型、也不做人脸识别。
type FaceAuthRecord struct {
	ID   int64  `json:"id"`
	Date string `json:"date"`
}

// FaceAuthList 是 GET /api/v1/patient/cards/{cardId}/face-auth 的响应。
type FaceAuthList struct {
	PatientCardID int64            `json:"patientCardId"`
	Records       []FaceAuthRecord `json:"records"`
}

// ToFaceAuthList 组装人脸认证记录响应；records 为空时返回空数组而不是 null，
// 以符合“列表为空返回 []”的通用约定。
func ToFaceAuthList(cardID int64, records []FaceAuthRecord) FaceAuthList {
	if records == nil {
		records = make([]FaceAuthRecord, 0)
	}
	return FaceAuthList{PatientCardID: cardID, Records: records}
}
