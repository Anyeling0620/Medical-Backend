package request

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"
)

type DoctorPricesRequest struct {
	DoctorID *int64 `form:"doctorId"`
	Page     *int   `form:"page"`
	PageSize *int   `form:"pageSize"`
}

func BindDoctorPrices(c *gin.Context) (DoctorPricesRequest, error) {
	var r DoctorPricesRequest
	if raw, exists := c.GetQuery("doctorId"); exists {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 1 {
			return r, errors.New("医生编号必须为正整数")
		}
		r.DoctorID = &v
	} else {
		return r, errors.New("医生编号必须为正整数")
	}
	if raw, exists := c.GetQuery("page"); exists {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return r, errors.New("page必须为正整数")
		}
		r.Page = &v
	}
	if r.Page == nil {
		v := 1
		r.Page = &v
	}
	if raw, exists := c.GetQuery("pageSize"); exists {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 100 {
			return r, errors.New("pageSize必须在1到100之间")
		}
		r.PageSize = &v
	}
	if r.PageSize == nil {
		v := 20
		r.PageSize = &v
	}
	return r, nil
}

// CatalogDoctorsRequest is the query model of the doctor catalog list endpoint.
type CatalogDoctorsRequest struct {
	DepartmentID    *int64
	SubdepartmentID *int64
	Name            *string
	Job             *string
	Degree          *string
	Recommended     *bool
	Status          string
	Page            int
	PageSize        int
	Sort            string
	Order           string
}

func BindCatalogDoctors(c *gin.Context) (CatalogDoctorsRequest, error) {
	r := CatalogDoctorsRequest{Status: "ACTIVE", Page: 1, PageSize: 20, Sort: "id", Order: "asc"}
	if raw, exists := c.GetQuery("departmentId"); exists {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 1 {
			return r, errors.New("departmentId必须为正整数")
		}
		r.DepartmentID = &v
	}
	if raw, exists := c.GetQuery("subdepartmentId"); exists {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 1 {
			return r, errors.New("subdepartmentId必须为正整数")
		}
		r.SubdepartmentID = &v
	}
	if raw, exists := c.GetQuery("name"); exists {
		if len([]rune(raw)) > 50 {
			return r, errors.New("name最多50个字符")
		}
		if raw != "" {
			r.Name = &raw
		}
	}
	if raw, exists := c.GetQuery("job"); exists && raw != "" {
		r.Job = &raw
	}
	if raw, exists := c.GetQuery("degree"); exists && raw != "" {
		r.Degree = &raw
	}
	if raw, exists := c.GetQuery("recommended"); exists {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return r, errors.New("recommended必须为布尔值")
		}
		r.Recommended = &v
	}
	if raw, exists := c.GetQuery("status"); exists {
		switch raw {
		case "ACTIVE", "RESIGNED", "RETIRED", "HIDDEN":
			r.Status = raw
		case "":
			r.Status = "ACTIVE"
		default:
			return r, errors.New("status只支持ACTIVE/RESIGNED/RETIRED/HIDDEN")
		}
	}
	if raw, exists := c.GetQuery("page"); exists {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return r, errors.New("page必须为正整数")
		}
		r.Page = v
	}
	if raw, exists := c.GetQuery("pageSize"); exists {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 100 {
			return r, errors.New("pageSize必须在1到100之间")
		}
		r.PageSize = v
	}
	if raw, exists := c.GetQuery("sort"); exists {
		if raw != "name" && raw != "hireDate" && raw != "recommended" && raw != "id" {
			return r, errors.New("排序字段或排序方向不支持")
		}
		r.Sort = raw
	}
	if raw, exists := c.GetQuery("order"); exists {
		if raw != "asc" && raw != "desc" {
			return r, errors.New("排序字段或排序方向不支持")
		}
		r.Order = raw
	}
	return r, nil
}
