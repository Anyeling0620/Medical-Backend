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
