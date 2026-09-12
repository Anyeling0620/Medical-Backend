package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/transport/http/request"
	"Medical-Web-Backend/internal/transport/http/response"
	doctorpatientservice "Medical-Web-Backend/internal/usecase/doctorpatient"
)

// DoctorPatientHandler 提供医生工作台的「我的患者」接口（契约 §6.10）。
//
// 数据范围只由访问令牌主体决定：中间件已完成 realm=mis 与权限编码校验，
// 这里把用户编号交给用例去解析 mis_user.ref_id，客户端无法指定医生编号。
type DoctorPatientHandler struct {
	service *doctorpatientservice.Service
}

func NewDoctorPatientHandler(
	service *doctorpatientservice.Service,
) *DoctorPatientHandler {
	return &DoctorPatientHandler{service: service}
}

// List 处理 GET /api/v1/mis/doctor/patients。
//
// 账号未绑定医生时返回 403 AUTH_FORBIDDEN（契约 §1.2）：这是「已登录但无该数据权限」，
// 不是资源不存在，也不泄露任何患者信息。
func (h *DoctorPatientHandler) List(c *gin.Context) {
	claims, ok := middleware.ClaimsFrom(c)
	if !ok || claims == nil {
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "访问令牌无效或已过期")
		return
	}

	q, err := request.BindDoctorPatientList(c)
	if err != nil {
		h.writeError(c, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", err.Error())
		return
	}

	page, err := h.service.MyPatients(
		c.Request.Context(),
		claims.UserID,
		doctorpatientservice.Query{
			Keyword:  q.Keyword,
			Sort:     q.Sort,
			Order:    q.Order,
			Page:     q.Page,
			PageSize: q.PageSize,
		},
	)
	if err != nil {
		if errors.Is(err, doctorpatientservice.ErrDoctorNotBound) {
			h.writeError(
				c,
				http.StatusForbidden,
				"AUTH_FORBIDDEN",
				"当前账号未关联医生，无法查看医生工作台数据",
			)
			return
		}
		// 契约 §10：仓储/数据库不可用统一映射为 502，而不是笼统的 500。
		if errors.Is(err, doctorpatientservice.ErrDependencyUnavailable) {
			h.writeError(
				c,
				http.StatusBadGateway,
				"DEPENDENCY_UNAVAILABLE",
				"服务暂时不可用，请稍后重试",
			)
			return
		}
		h.writeError(
			c,
			http.StatusInternalServerError,
			"INTERNAL_SERVER_ERROR",
			"查询患者列表失败，请稍后重试",
		)
		return
	}
	if page == nil {
		h.writeError(
			c,
			http.StatusInternalServerError,
			"INTERNAL_SERVER_ERROR",
			"查询患者列表失败，请稍后重试",
		)
		return
	}

	c.JSON(http.StatusOK, response.Page[response.DoctorPatientItem]{
		Items:    response.NewDoctorPatientItems(page.Items),
		Page:     page.Page,
		PageSize: page.PageSize,
		Total:    page.Total,
	})
}

// writeError 输出契约 §1.3 的错误体；handler 各自持有该辅助方法，
// 与其它域（auth/registration/payment）保持一致，便于后续按域追加 details。
func (h *DoctorPatientHandler) writeError(
	c *gin.Context,
	status int,
	code string,
	message string,
) {
	c.JSON(status, gin.H{
		"code":    code,
		"message": message,
	})
}
