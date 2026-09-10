package patient

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// dateLayout 是 API 与数据库 DATE 列共用的日期格式。
	dateLayout = "2006-01-02"
	// pidDateLayout 是身份证号第 7-14 位的紧凑日期格式。
	pidDateLayout = "20060102"

	// 以下长度上限来自 init.sql 的列定义。领域校验必须与数据库一致，
	// 否则越界值会拖到写库时才以 22001 之类的存储错误暴露出来。
	maxNameLength = 20 // patient_user_info_card.name VARCHAR(20)
	pidLength     = 18 // patient_user_info_card.pid CHAR(18)

	// 字段名用于组装 422 响应中的 details.fields，必须与 API 的 JSON 字段名一致。
	fieldName           = "name"
	fieldSex            = "sex"
	fieldPID            = "pid"
	fieldTel            = "tel"
	fieldBirthday       = "birthday"
	fieldMedicalHistory = "medicalHistory"
	fieldInsuranceType  = "insuranceType"
	fieldUserID         = "userId"
)

// optionNone 是疾病史与医保类型枚举共用的“无”取值，同时是疾病史里的互斥哨兵值：
// 一旦出现“无”，就不允许再出现其他取值。
const optionNone = "无"

// medicalHistoryOptions 是疾病史的合法取值全集
// （spec/03-domain-and-state.md §患者与会话域）。数据库以 VARCHAR 存 JSON 文本。
var medicalHistoryOptions = []string{
	"高血压", "糖尿病", "心脏病", "脑梗", "脑出血", "脑中风",
	"白血病", "癫痫", "肾病", "其他", optionNone,
}

// insuranceTypeOptions 是医保类型的合法取值全集，单值。
var insuranceTypeOptions = []string{
	"社会基本医疗保险", "商业医疗保险", "新型农村合作医疗", "大病统筹",
	"公费医疗", "城镇居民医疗保险", "其他", optionNone,
}

// medicalHistorySet / insuranceTypeSet 由上面的取值列表派生，用于 O(1) 命中判断。
var (
	medicalHistorySet = newStringSet(medicalHistoryOptions)
	insuranceTypeSet  = newStringSet(insuranceTypeOptions)
)

// telPattern 是中国大陆手机号规则：1 开头、第二位 3-9、共 11 位。
// 列类型是 CHAR(11)，因此长度必须恰好为 11。
var telPattern = regexp.MustCompile(`^1[3-9][0-9]{9}$`)

// pidPattern 是 18 位居民身份证号的字符集约束：前 17 位数字，末位数字或 X。
var pidPattern = regexp.MustCompile(`^[0-9]{17}[0-9X]$`)

// pidWeights 与 pidCheckCodes 实现 GB 11643 身份证号校验位的
// ISO 7064:1983 MOD 11-2 算法。
var pidWeights = [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}

const pidCheckCodes = "10X98765432"

// MedicalHistoryOptions 返回疾病史合法取值的副本，供请求层枚举校验或前端下发；
// 调用方修改返回值不会影响包内状态。
func MedicalHistoryOptions() []string {
	return append([]string(nil), medicalHistoryOptions...)
}

// InsuranceTypeOptions 返回医保类型合法取值的副本。
func InsuranceTypeOptions() []string {
	return append([]string(nil), insuranceTypeOptions...)
}

// PIDInfo 是从身份证号推导出的可信信息。客户端提交的出生日期和性别一律不采用，
// 只以身份证号推导结果为准（spec/03-domain-and-state.md §患者与会话域）。
type PIDInfo struct {
	// Birthday 为身份证号第 7-14 位，格式 YYYY-MM-DD。
	Birthday string
	// Sex 由第 17 位的奇偶性推导：奇数为男、偶数为女。
	Sex string
}

// ParsePID 校验 18 位居民身份证号并推导出生日期与性别。
//
// 校验项：长度与字符集、地址码非全零、校验位正确、出生日期真实存在。
// 出生日期是否“晚于今天”属于业务判断，需要可注入的当前时间，
// 因此放在 NewCard 中用调用方传入的 now 判定，保证本函数是纯函数、便于测试。
func ParsePID(pid string) (PIDInfo, error) {
	normalized := strings.ToUpper(strings.TrimSpace(pid))
	if !pidPattern.MatchString(normalized) {
		return PIDInfo{}, ErrPIDInvalid
	}
	// 前 6 位是地址码，全零在现实中不存在，按非法处理。
	if normalized[:6] == "000000" {
		return PIDInfo{}, ErrPIDInvalid
	}
	sum := 0
	for i := 0; i < 17; i++ {
		sum += int(normalized[i]-'0') * pidWeights[i]
	}
	if pidCheckCodes[sum%11] != normalized[pidLength-1] {
		return PIDInfo{}, ErrPIDInvalid
	}
	birthday, err := time.Parse(pidDateLayout, normalized[6:14])
	if err != nil {
		// 例如 20260230 这类“校验位正确但日期不存在”的号码。
		return PIDInfo{}, ErrPIDInvalid
	}
	sex := SexMale
	if (normalized[16]-'0')%2 == 0 {
		sex = SexFemale
	}
	return PIDInfo{
		Birthday: birthday.Format(dateLayout),
		Sex:      sex,
	}, nil
}

// MaskPID 按契约把身份证号脱敏为“前 6 位 + 8 个星号 + 后 4 位”，
// 例如 110101199001011237 -> 110101********1237（spec/04-api-contract.md §7.4）。
//
// 长度不是 18 位时不猜测、不截断，而是整串替换为等长星号：
// 宁可看不出内容，也不能把半个身份证号当成脱敏结果输出。
func MaskPID(pid string) string {
	if pid == "" {
		return ""
	}
	if len(pid) != pidLength {
		return strings.Repeat("*", len(pid))
	}
	return pid[:6] + strings.Repeat("*", 8) + pid[14:]
}

// ParseMedicalHistory 把数据库中的 medical_history（JSON 文本）解析为字符串数组。
//
// 与目录域的 ParseTags 同一策略：解析失败不报错、返回空数组，
// 避免一条脏数据让整个列表接口 500；数据质量由告警另行处理。
func ParseMedicalHistory(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil || values == nil {
		return []string{}
	}
	return values
}

// EncodeMedicalHistory 把疾病史序列化为数据库 VARCHAR 中的 JSON 文本。
// 空集合固定编码为 `[]`：json.Marshal 对 nil 切片会输出 `null`，
// 而库里存 `null` 会让后续解析与人工排查都更难判断“真的没有”还是“写坏了”。
func EncodeMedicalHistory(values []string) (string, error) {
	if len(values) == 0 {
		return "[]", nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode medicalHistory: %w", err)
	}
	return string(encoded), nil
}

// BusinessDate 把时刻归一到患者业务的日历日，格式 YYYY-MM-DD。
//
// patient_user.create_time 是 DATE 列，写入的是医院所在地（Asia/Shanghai）的
// 日历日。使用固定 +08:00 偏移，避免依赖部署环境是否安装时区数据库
// （与排班域 businessLocation 保持同一口径）。
func BusinessDate(t time.Time) string {
	return t.In(businessLocation).Format(dateLayout)
}

// businessLocation 是患者业务的日历时区，见 BusinessDate 说明。
var businessLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// NewCardUUID 生成就诊卡的业务唯一标识：32 位无横线十六进制字符串
// （spec/03-domain-and-state.md §就诊卡：“uuid 由服务端生成，固定 32 位无横线字符串”）。
func NewCardUUID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// ValidateName 校验并规范化姓名（去首尾空白，长度不超过列宽）。
func ValidateName(value string) (string, error) {
	name := strings.TrimSpace(value)
	if name == "" {
		return "", &FieldError{Field: fieldName, Reason: ErrNameRequired}
	}
	// 列宽按字符数（不是字节数）约束，避免中文姓名被按字节误判超长。
	if len([]rune(name)) > maxNameLength {
		return "", &FieldError{Field: fieldName, Reason: ErrNameTooLong}
	}
	return name, nil
}

// ValidateSex 校验性别取值。这里只做枚举校验：
// “与身份证号一致”是建卡时的规则，在 NewCard 中单独判定。
func ValidateSex(value string) (string, error) {
	sex := strings.TrimSpace(value)
	if sex != SexMale && sex != SexFemale {
		return "", &FieldError{Field: fieldSex, Reason: ErrSexInvalid}
	}
	return sex, nil
}

// ValidateTel 校验并规范化手机号。
func ValidateTel(value string) (string, error) {
	tel := strings.TrimSpace(value)
	if !telPattern.MatchString(tel) {
		return "", &FieldError{Field: fieldTel, Reason: ErrTelInvalid}
	}
	return tel, nil
}

// ValidateMedicalHistory 校验并规范化疾病史。
//
// 规则：必填且非空；每项都必须命中约定取值；“无”与其他取值互斥。
// 重复项按首次出现顺序去重，避免客户端重复提交导致存储冗余。
func ValidateMedicalHistory(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, &FieldError{Field: fieldMedicalHistory, Reason: ErrMedicalHistoryRequired}
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	hasNone := false
	for _, raw := range values {
		item := strings.TrimSpace(raw)
		if item == "" || !medicalHistorySet[item] {
			// 空字符串同样按“不支持的取值”处理：静默丢弃客户端提交的内容
			// 会让调用方误以为已经保存。
			return nil, &FieldError{Field: fieldMedicalHistory, Reason: ErrMedicalHistoryInvalid}
		}
		if item == optionNone {
			hasNone = true
		}
		if seen[item] {
			continue
		}
		seen[item] = true
		normalized = append(normalized, item)
	}
	if hasNone && len(normalized) > 1 {
		return nil, &FieldError{Field: fieldMedicalHistory, Reason: ErrMedicalHistoryNoneConflict}
	}
	if len(normalized) == 0 {
		return nil, &FieldError{Field: fieldMedicalHistory, Reason: ErrMedicalHistoryRequired}
	}
	return normalized, nil
}

// ValidateInsuranceType 校验并规范化医保类型（单值）。
func ValidateInsuranceType(value string) (string, error) {
	insurance := strings.TrimSpace(value)
	if insurance == "" {
		return "", &FieldError{Field: fieldInsuranceType, Reason: ErrInsuranceTypeRequired}
	}
	if !insuranceTypeSet[insurance] {
		return "", &FieldError{Field: fieldInsuranceType, Reason: ErrInsuranceTypeInvalid}
	}
	return insurance, nil
}

// CardInput 是创建就诊卡时客户端一次性提交的全部必填字段。
//
// 出生日期与 uuid 不在其中：前者由身份证号推导，后者由服务端生成。
// 缺任一项都不予创建（spec/04-api-contract.md §7.4）。
type CardInput struct {
	Name           string
	Sex            string
	PID            string
	Tel            string
	MedicalHistory []string
	InsuranceType  string
}

// NewCard 校验建卡入参并构造就诊卡实体。
//
// 校验策略是“一次性收集全部字段错误”而不是首个错误即返回：契约要求 422 的
// details.fields 能同时列出多个问题字段（例如 pid 与 tel 同时格式错误）。
// 只要存在任一错误就不构造实体，即“字段缺失不予创建”。
// cardUUID 为空时自动生成，便于调用方在需要确定性 uuid 的场景（测试、数据迁移）
// 显式传入。
func NewCard(userID int64, cardUUID string, input CardInput, now time.Time) (Card, error) {
	errs := make([]error, 0, 6)

	// 校验顺序与契约 §7.4 的字段顺序一致（name、sex、pid、tel、medicalHistory、
	// insuranceType），使 details.fields 的顺序与契约示例相同：
	// 例如 pid 与 tel 同时非法时返回 ["pid","tel"]。
	name, err := ValidateName(input.Name)
	if err != nil {
		errs = append(errs, err)
	}
	sex, sexErr := ValidateSex(input.Sex)
	if sexErr != nil {
		errs = append(errs, sexErr)
	}

	normalizedPID := strings.ToUpper(strings.TrimSpace(input.PID))
	info, pidErr := ParsePID(normalizedPID)
	if pidErr != nil {
		errs = append(errs, &FieldError{Field: fieldPID, Reason: ErrPIDInvalid})
	} else {
		// 出生日期必须在今天之前：身份证号校验位正确但仍可能编码未来日期。
		if info.Birthday > BusinessDate(now) {
			errs = append(errs, &FieldError{Field: fieldPID, Reason: ErrBirthdayInFuture})
		}
		// 性别一致性只在性别本身合法时才判定，否则会用一个字段的错误
		// 去污染另一个字段的错误集合。
		if sexErr == nil && info.Sex != sex {
			errs = append(errs, &FieldError{Field: fieldSex, Reason: ErrSexMismatch})
		}
	}

	tel, err := ValidateTel(input.Tel)
	if err != nil {
		errs = append(errs, err)
	}
	history, err := ValidateMedicalHistory(input.MedicalHistory)
	if err != nil {
		errs = append(errs, err)
	}
	insurance, err := ValidateInsuranceType(input.InsuranceType)
	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return Card{}, errors.Join(errs...)
	}
	if strings.TrimSpace(cardUUID) == "" {
		cardUUID = NewCardUUID()
	}

	return Card{
		UserID:         userID,
		UUID:           cardUUID,
		Name:           name,
		Sex:            sex,
		PID:            normalizedPID,
		Tel:            tel,
		Birthday:       info.Birthday,
		MedicalHistory: history,
		InsuranceType:  insurance,
		// 新建卡时尚未录入人脸信息；本阶段不上传也不识别人脸模型。
		ExistFaceModel: false,
	}, nil
}

// CardUpdate 描述 PATCH 就诊卡的入参。指针为 nil 一律表示本次不修改该字段：
// 本类型不区分“键不存在”与“显式提交 null”，后者的区分由请求层用 json.RawMessage
// 保留原始键信息完成（见 internal/transport/http/request 的 UpdatePatientCardRequest.Update）。
//
// PID、UserID、Birthday 不可修改，但仍在此声明，用于把“客户端提交了不可修改字段”
// 和“客户端没提交”区分开：契约要求提交 pid 返回 422 PATIENT_CARD_PID_IMMUTABLE，
// 提交 birthday/userId 返回 422 REQUEST_VALIDATION_FAILED，
// 静默忽略会让客户端误以为修改已经生效。
type CardUpdate struct {
	Name           *string
	Sex            *string
	Tel            *string
	MedicalHistory *[]string
	InsuranceType  *string

	// 以下字段不可修改，仅用于检测客户端是否提交。
	PID      *string
	UserID   *int64
	Birthday *string
}

// ApplyUpdate 校验改卡入参并返回修改后的就诊卡副本，不修改原实体。
//
// 只更新契约允许的字段（姓名、性别、手机号、疾病史、医保类型）。
// 性别在这里只做枚举校验而不与身份证号比对：契约把“性别与身份证号一致”列为
// 建卡规则，同时把性别列为允许修改的字段；若改卡时也强制比对，性别实际上
// 将永远无法修改，与“允许修改”自相矛盾。该口径需要在规格评审时确认。
func (c Card) ApplyUpdate(update CardUpdate) (Card, error) {
	errs := make([]error, 0, 5)

	if update.PID != nil {
		errs = append(errs, &FieldError{Field: fieldPID, Reason: ErrPIDImmutable})
	}
	if update.UserID != nil {
		errs = append(errs, &FieldError{Field: fieldUserID, Reason: ErrUserIDImmutable})
	}
	if update.Birthday != nil {
		errs = append(errs, &FieldError{Field: fieldBirthday, Reason: ErrBirthdayImmutable})
	}

	updated := c
	if update.Name != nil {
		value, err := ValidateName(*update.Name)
		if err != nil {
			errs = append(errs, err)
		} else {
			updated.Name = value
		}
	}
	if update.Sex != nil {
		value, err := ValidateSex(*update.Sex)
		if err != nil {
			errs = append(errs, err)
		} else {
			updated.Sex = value
		}
	}
	if update.Tel != nil {
		value, err := ValidateTel(*update.Tel)
		if err != nil {
			errs = append(errs, err)
		} else {
			updated.Tel = value
		}
	}
	if update.MedicalHistory != nil {
		value, err := ValidateMedicalHistory(*update.MedicalHistory)
		if err != nil {
			errs = append(errs, err)
		} else {
			updated.MedicalHistory = value
		}
	}
	if update.InsuranceType != nil {
		value, err := ValidateInsuranceType(*update.InsuranceType)
		if err != nil {
			errs = append(errs, err)
		} else {
			updated.InsuranceType = value
		}
	}

	if len(errs) > 0 {
		return Card{}, errors.Join(errs...)
	}
	return updated, nil
}

// newStringSet 把取值列表转为集合，便于 O(1) 命中判断。
func newStringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}
