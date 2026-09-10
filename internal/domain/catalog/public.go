package catalog

// 本文件是匿名公开查询域（/api/v1/public/*）的目录资源模型。
// 契约 §2.3「公开查询资源」列出了该域的公开字段，且明确「公开字段集合本身是契约的一部分」，
// 因此公开域单独建模，不复用管理端的 DoctorCatalog：管理端字段日后新增时不会静默泄漏到匿名接口。

// PublicDoctor 是公开医生列表项，只包含契约允许的字段；
// pid、tel、address、email、原始 uuid 等敏感列既不返回也不查询。
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

// PublicSubdepartmentRef 是医生详情内嵌的子科室引用（只暴露 id 与 name）。
type PublicSubdepartmentRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// PublicDoctorPrice 是随医生详情返回的价目（doctor_price），
// 按契约只暴露 id、level、price1、price2，不返回 doctor_id。
type PublicDoctorPrice struct {
	ID     int64  `json:"id"`
	Level  string `json:"level"`
	Price1 string `json:"price1"`
	Price2 string `json:"price2"`
}

// PublicDoctorDetail 是公开医生详情：列表字段 + 所属子科室 + 价目。
type PublicDoctorDetail struct {
	PublicDoctor
	Subdepartments []PublicSubdepartmentRef `json:"subdepartments"`
	Prices         []PublicDoctorPrice      `json:"prices"`
}

// PublicDoctorFilter 是公开医生列表的过滤与排序条件。
// 与管理端 DoctorFilter 不同：公开域没有 status、job、degree、recommended 过滤，
// 且状态固定为「在岗且非隐藏」，由 repository 强制写死，调用方不得放宽。
type PublicDoctorFilter struct {
	DepartmentID    *int64
	SubdepartmentID *int64
	Name            *string
	Sort            string
	Order           string
}
