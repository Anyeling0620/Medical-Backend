package request

import (
	"errors"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/doctor"
)

// DoctorSearchRequest is the query-string model for doctor search.
type DoctorSearchRequest struct {
	Name        *string `form:"name"`
	DeptID      *int64  `form:"deptId"`
	Degree      *string `form:"degree"`
	Job         *string `form:"job"`
	Recommended *bool   `form:"recommended"`
	Status      *int16  `form:"status"`
	Order       *string `form:"order"`
	Page        *int    `form:"page"`
	Length      *int    `form:"length"`
}

func BindDoctorSearch(c *gin.Context) (DoctorSearchRequest, error) {
	var req DoctorSearchRequest
	if value, present := c.GetQueryMap("recommended"); present && len(value) > 0 {
		return req, errors.New("recommended must be a boolean")
	}
	if values, present := c.Request.URL.Query()["recommended"]; present {
		if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
			return req, errors.New("recommended must be true or false")
		}
	}
	if err := c.ShouldBindQuery(&req); err != nil {
		return req, err
	}
	return req, nil
}

func (r DoctorSearchRequest) Filters() doctor.SearchFilters {
	return doctor.SearchFilters{
		Name:        r.Name,
		DeptID:      r.DeptID,
		Degree:      r.Degree,
		Job:         r.Job,
		Recommended: r.Recommended,
		Status:      r.Status,
		Order:       r.Order,
	}
}

func (r DoctorSearchRequest) ValidateList() error {
	if err := r.validateFilters(); err != nil {
		return err
	}
	if r.Page == nil {
		return invalidDoctorParam("page不能为空")
	}
	if *r.Page < 1 {
		return invalidDoctorParam("page不能小于1")
	}
	if r.Length == nil {
		return invalidDoctorParam("length不能为空")
	}
	if *r.Length < 10 || *r.Length > 50 {
		return invalidDoctorParam("length内容不正确")
	}
	return nil
}

func (r DoctorSearchRequest) ValidateCount() error {
	return r.validateFilters()
}

func (r DoctorSearchRequest) validateFilters() error {
	if r.Name != nil {
		name := []rune(*r.Name)
		if len(name) < 1 || len(name) > 20 {
			return invalidDoctorParam("name内容不正确")
		}
		for _, char := range name {
			if char < '\u4e00' || char > '\u9fa5' {
				return invalidDoctorParam("name内容不正确")
			}
		}
	}
	if r.DeptID != nil && *r.DeptID < 1 {
		return invalidDoctorParam("deptId不能小于1")
	}
	if r.Degree != nil && !oneOf(*r.Degree, "本科", "研究生", "博士") {
		return invalidDoctorParam("degree内容不正确")
	}
	if r.Job != nil && !oneOf(*r.Job, "主治医师", "副主治医师", "主任医师", "副主任医师") {
		return invalidDoctorParam("job内容不正确")
	}
	if r.Status == nil {
		return invalidDoctorParam("status不能为空")
	}
	if *r.Status < 1 || *r.Status > 3 {
		return invalidDoctorParam("status内容不正确")
	}
	if r.Order != nil && !oneOf(*r.Order, "ASC", "DESC") {
		return invalidDoctorParam("order内容不正确")
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func invalidDoctorParam(message string) error { return errors.New(message) }
