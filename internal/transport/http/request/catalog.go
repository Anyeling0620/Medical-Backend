package request

import (
	"errors"
	"github.com/gin-gonic/gin"
	"strconv"
)

type DepartmentListRequest struct {
	Outpatient  *bool   `form:"outpatient"`
	Recommended *bool   `form:"recommended"`
	Page        *int    `form:"page"`
	PageSize    *int    `form:"pageSize"`
	Sort        *string `form:"sort"`
	Order       *string `form:"order"`
}

func BindDepartmentList(c *gin.Context) (DepartmentListRequest, error) {
	var r DepartmentListRequest
	if err := c.ShouldBindQuery(&r); err != nil {
		return r, err
	}
	return r, nil
}
func (r DepartmentListRequest) Validate() error {
	if r.Page == nil {
		v := 1
		r.Page = &v
	}
	if r.Page == nil || *r.Page < 1 {
		return errors.New("page必须为正整数")
	}
	if r.PageSize == nil {
		v := 20
		r.PageSize = &v
	}
	if *r.PageSize < 1 || *r.PageSize > 100 {
		return errors.New("pageSize必须在1到100之间")
	}
	if r.Sort != nil && *r.Sort != "name" && *r.Sort != "id" {
		return errors.New("排序字段不支持")
	}
	if r.Order != nil && *r.Order != "asc" && *r.Order != "desc" {
		return errors.New("排序方向不支持")
	}
	return nil
}
func (r DepartmentListRequest) Values() (int, int, string, string) {
	p, ps := 1, 20
	if r.Page != nil {
		p = *r.Page
	}
	if r.PageSize != nil {
		ps = *r.PageSize
	}
	sort, order := "name", "asc"
	if r.Sort != nil {
		sort = *r.Sort
	}
	if r.Order != nil {
		order = *r.Order
	}
	return p, ps, sort, order
}
func ParsePositiveID(raw string) (int64, error) {
	id, e := strconv.ParseInt(raw, 10, 64)
	if e != nil || id < 1 {
		return 0, errors.New("编号必须为正整数")
	}
	return id, nil
}
