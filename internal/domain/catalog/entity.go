package catalog

import "encoding/json"

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

// DoctorFilter contains the optional filters and ordering for the doctor catalog
// list endpoint. Status uses the canonical ACTIVE/RESIGNED/RETIRED/HIDDEN value.
type DoctorFilter struct {
	DepartmentID    *int64
	SubdepartmentID *int64
	Name            *string
	Job             *string
	Degree          *string
	Recommended     *bool
	Status          string
	Sort            string
	Order           string
}

// Doctor status values reported by the catalog API. doctor.status stores them
// as 1=ACTIVE, 2=RESIGNED, 3=RETIRED and 4=HIDDEN.
const (
	DoctorStatusActive   = "ACTIVE"
	DoctorStatusResigned = "RESIGNED"
	DoctorStatusRetired  = "RETIRED"
	DoctorStatusHidden   = "HIDDEN"
)

type Page[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
	Total    int64 `json:"total"`
}

type DepartmentOption struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
type SubdepartmentOption struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DepartmentID int64  `json:"departmentId"`
}
type DoctorPrice struct {
	ID       int64  `json:"id"`
	DoctorID int64  `json:"doctorId"`
	Level    string `json:"level"`
	Price1   string `json:"price1"`
	Price2   string `json:"price2"`
}
type DoctorCatalog struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Sex         string   `json:"sex"`
	PhotoURL    string   `json:"photoUrl"`
	Birthday    string   `json:"birthday"`
	School      string   `json:"school"`
	Degree      string   `json:"degree"`
	Job         string   `json:"job"`
	Remark      string   `json:"remark"`
	Description string   `json:"description"`
	HireDate    string   `json:"hireDate"`
	Tags        []string `json:"tags"`
	Recommended bool     `json:"recommended"`
	Status      string   `json:"status"`
	CreateDate  string   `json:"createDate"`
}
type DoctorCatalogDetail struct {
	DoctorCatalog
	Subdepartments []SubdepartmentOption `json:"subdepartments"`
	Prices         []DoctorPrice         `json:"prices"`
}
type DoctorOptions struct {
	Departments    []DepartmentOption    `json:"departments"`
	Subdepartments []SubdepartmentOption `json:"subdepartments"`
	Jobs           []string              `json:"jobs"`
	Degrees        []string              `json:"degrees"`
}

func ParseTags(raw string) []string {
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil || tags == nil {
		return []string{}
	}
	return tags
}
