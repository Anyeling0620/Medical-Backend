// Package doctorpatient 定义「医生本人患者」只读视图的领域模型。
//
// 该视图不是独立业务表，而是把「挂号为当前医生」的挂号记录
// （medical_registration.doctor_id）与就诊卡（patient_user_info_card）聚合出的结果。
// 医生登录管理端后只能看到自己接诊过的患者：数据范围由 usecase 依据登录主体
// 的绑定关系（mis_user.ref_id -> doctor.id）推导，绝不采用客户端提交的 doctorId
// （契约 §1.2、§6.10）。
package doctorpatient

import "strings"

// 列表排序白名单（契约 §1.4：sort 必须从白名单选择，禁止拼接 SQL）。
const (
	// SortLastVisitDate 按最近一次就诊日期排序，也是默认值。
	SortLastVisitDate = "lastVisitDate"
	// SortName 按患者姓名排序。
	SortName = "name"
	// SortRegistrationCount 按该患者在本医生处的挂号次数排序。
	SortRegistrationCount = "registrationCount"
)

// Patient 是医生视角下的患者列表项：就诊卡基础信息 + 该患者在本医生处的就诊统计。
//
// 不返回身份证号（patient_user_info_card.pid）：列表只需要可辨识与可联系的信息，
// 身份证号属于更高敏感度的字段，等出现确实需要的业务场景（如开方、实名核对）
// 再单独评估返回范围。
type Patient struct {
	PatientCardID int64
	Name          string
	Sex           string
	Tel           string
	// Birthday 为空串表示就诊卡未登记出生日期。
	Birthday string
	// MedicalHistory 是就诊卡的疾病史：按契约 §2 统一以字符串数组暴露，
	// 库中该列是存 JSON 文本的 VARCHAR，空切片表示就诊卡未登记病史。
	MedicalHistory []string
	InsuranceType  string
	// RegistrationCount 是该患者在本医生处的挂号总次数（含未支付与已过期记录）。
	RegistrationCount int64
	// LastVisitDate 为空串表示本医生名下的挂号记录缺少有效就诊日期（脏数据）。
	LastVisitDate string
	// LastPaymentStatus 是该患者在本医生处最近一次挂号的支付状态（UNPAID/PAID/REFUNDED/EXPIRED）。
	LastPaymentStatus string
}

// Filter 是医生患者列表的查询条件。
//
// DoctorID 由 usecase 从登录主体推导后写入，调用方（handler/request 层）不得用
// 客户端提交的值覆盖，否则医生视角会退化为全量数据。
type Filter struct {
	DoctorID int64
	// Keyword 按患者姓名或联系电话模糊匹配；空串表示不过滤。
	Keyword  string
	Sort     string
	Order    string
	Page     int
	PageSize int
}

// Page 是医生患者列表的分页结果（契约 §1.1 的统一分页结构）。
type Page struct {
	Items    []Patient
	Page     int
	PageSize int
	Total    int64
}

// Offset 把 1 起的页码换算为 SQL OFFSET；非法页码按第一页处理，
// 保证仓储层不会因调用方漏校验而产生负数偏移。
func (f Filter) Offset() int {
	if f.Page < 1 || f.PageSize < 1 {
		return 0
	}
	return (f.Page - 1) * f.PageSize
}

// KeywordPattern 把用户输入的关键词转成 ILIKE 模式：先转义通配符，再两侧补 %。
//
// 不转义会让用户输入的 % 或 _ 变成通配符，把「按姓名查找」静默变成全表匹配；
// 转义字符固定为反斜杠，与仓储层 ILIKE ... ESCAPE '\' 的写法配套。
func KeywordPattern(keyword string) string {
	if keyword == "" {
		return ""
	}
	return "%" + escapeLikePattern(keyword) + "%"
}

// escapeLikePattern 转义 ILIKE 模式中的反斜杠、% 与 _。
func escapeLikePattern(keyword string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(keyword)
}
