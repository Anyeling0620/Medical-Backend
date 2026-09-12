package request

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/doctorpatient"
)

// 本文件绑定医生工作台（/api/v1/mis/doctor/*）的查询参数（契约 §6.10、§1.4）。
// 医生编号不在参数中：数据范围由访问令牌主体推导，客户端无法指定他人。

// maxDoctorPatientKeywordLength 是关键词长度上限（按字符计）。
// 就诊卡姓名与联系电话都远短于该值，超长输入只可能是构造出来的模式串。
const maxDoctorPatientKeywordLength = 50

// DoctorPatientListQuery 是 GET /api/v1/mis/doctor/patients 校验后的查询参数。
type DoctorPatientListQuery struct {
	Keyword  string
	Sort     string
	Order    string
	Page     int
	PageSize int
}

// doctorPatientSortWhitelist 是排序字段白名单（契约 §1.4：sort 必须从白名单选择）。
var doctorPatientSortWhitelist = []string{
	doctorpatient.SortLastVisitDate,
	doctorpatient.SortName,
	doctorpatient.SortRegistrationCount,
}

// BindDoctorPatientList 解析「我的患者」列表参数：
// keyword 按姓名/电话模糊匹配（可省略），page/pageSize 与 sort/order 沿用统一约定。
func BindDoctorPatientList(c *gin.Context) (DoctorPatientListQuery, error) {
	q := DoctorPatientListQuery{}

	keyword := strings.TrimSpace(c.Query("keyword"))
	if len([]rune(keyword)) > maxDoctorPatientKeywordLength {
		return q, errors.New("keyword 长度不能超过 50 个字符")
	}
	// PostgreSQL 的 text 不接受 NUL 字节，无效 UTF-8 也会被驱动拒绝；
	// 这两类输入若不拦下会在仓储层变成 500，这里按参数非法返回 422。
	if !utf8.ValidString(keyword) || strings.ContainsRune(keyword, 0) {
		return q, errors.New("keyword 含有非法字符")
	}
	q.Keyword = keyword

	page, err := bindPublicPage(c)
	if err != nil {
		return q, err
	}
	q.Page = page.Page
	q.PageSize = page.PageSize

	sort, order, err := bindPublicOrder(
		c,
		doctorPatientSortWhitelist,
		doctorpatient.SortLastVisitDate,
		"desc",
	)
	if err != nil {
		return q, err
	}
	q.Sort = sort
	q.Order = order
	return q, nil
}
