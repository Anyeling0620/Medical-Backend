package catalog

type Department struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Outpatient  bool   `json:"outpatient"`
	Description string `json:"description"`
	Recommended bool   `json:"recommended"`
}

type Subdepartment struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DepartmentID int64  `json:"departmentId"`
	Location     string `json:"location"`
}

type SubdepartmentDetail struct {
	Subdepartment
	Department DepartmentRef `json:"department"`
}

type DepartmentRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type DepartmentFilter struct {
	Outpatient  *bool
	Recommended *bool
	Sort        string
	Order       string
}

type Page[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
	Total    int64 `json:"total"`
}
