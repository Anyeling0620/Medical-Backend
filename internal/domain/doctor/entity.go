package doctor

// Doctor contains the fields exposed by the doctor search endpoint.
type Doctor struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Sex         string `json:"sex"`
	Tel         string `json:"tel"`
	School      string `json:"school"`
	Degree      string `json:"degree"`
	Job         string `json:"job"`
	DeptName    string `json:"deptName"`
	SubName     string `json:"subName"`
	Recommended bool   `json:"recommended"`
	Status      int16  `json:"status"`
}

// SearchFilters contains the optional doctor filters and required status.
type SearchFilters struct {
	Name        *string
	DeptID      *int64
	Degree      *string
	Job         *string
	Recommended *bool
	Status      *int16
	Order       *string
}
