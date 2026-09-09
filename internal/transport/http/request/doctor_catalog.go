package request

import (
	"errors"
	"github.com/gin-gonic/gin"
)

type DoctorPricesRequest struct {
	DoctorID *int64 `form:"doctorId"`
	Page     *int   `form:"page"`
	PageSize *int   `form:"pageSize"`
}

func BindDoctorPrices(c *gin.Context) (DoctorPricesRequest, error) {
	var r DoctorPricesRequest
	if err := c.ShouldBindQuery(&r); err != nil {
		return r, err
	}
	if r.DoctorID == nil || *r.DoctorID < 1 {
		return r, errors.New("医生编号必须为正整数")
	}
	if r.Page == nil {
		v := 1
		r.Page = &v
	}
	if *r.Page < 1 {
		return r, errors.New("page必须为正整数")
	}
	if r.PageSize == nil {
		v := 20
		r.PageSize = &v
	}
	if *r.PageSize < 1 || *r.PageSize > 100 {
		return r, errors.New("pageSize必须在1到100之间")
	}
	return r, nil
}
