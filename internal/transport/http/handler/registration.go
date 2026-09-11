package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/transport/http/request"
	"Medical-Web-Backend/internal/transport/http/response"
	registrationservice "Medical-Web-Backend/internal/usecase/registration"
)

// RegistrationHandler 提供挂号域的四个 HTTP 接口（spec/04-api-contract.md §6.1–§6.4）。
//
// 调用者身份完全取自访问令牌：realm=patient 时主体固定为当前患者，realm=mis 时
// 已由中间件完成权限编码校验。幂等协调（Claim 原子占位 + 结果重放）复用
// schedule_idem.go 的共享实现。
type RegistrationHandler struct {
	service *registrationservice.Service
	*scheduleIdem
}

// NewRegistrationHandler 构造挂号处理器；store 为创建接口的幂等存储（Redis）。
func NewRegistrationHandler(
	service *registrationservice.Service,
	store port.IdempotencyStore,
) *RegistrationHandler {
	return &RegistrationHandler{service: service, scheduleIdem: newScheduleIdem(store)}
}

// Eligibility 处理 POST /api/v1/registrations/eligibility：挂号前资格校验。
//
// 本接口只读：不占用号源、不产生幂等记录；资格不通过仍是 200，
// 由 eligible=false 与 reasons 词表表达（契约 §6.1）。
func (h *RegistrationHandler) Eligibility(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	body, err := request.BindEligibility(c)
	if err != nil {
		h.validation(c, err)
		return
	}

	result, err := h.service.Eligibility(
		c.Request.Context(), actor, *body.PatientCardID, *body.ScheduleID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if result == nil {
		h.internal(c)
		return
	}
	c.JSON(http.StatusOK, response.NewEligibilityResponse(*result))
}

// Create 处理 POST /api/v1/registrations：创建挂单与待支付信息（契约 §6.2）。
// 必须携带 Idempotency-Key：重复 key 返回第一次结果，且不重复扣号/建单。
//
// 建单事务提交后会在同一个请求内完成支付宝预下单，201 响应因此携带 qrCode 与
// payableUntil/validUntil；预下单或二维码回写失败时订单已整单补偿，返回 502
// PAYMENT_PROVIDER_UNAVAILABLE 且不含二维码。5xx 不入幂等存储，客户端可用同一 key 重试，
// 重试会以新的 out_trade_no 重新建单。
func (h *RegistrationHandler) Create(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	key, err := request.BindIdempotencyKey(c)
	if err != nil {
		h.validation(c, err)
		return
	}
	// 幂等键摘要包含 realm 与主体：管理端用户与患者的主键可能同号，
	// 必须隔离键空间，避免把一方的结果重放给另一方（契约 §1.5）。
	idemKey := registrationIdempotencyKey(
		actor, c.Request.Method+"|"+c.Request.URL.Path, key)

	h.claimAndRun(c, idemKey, func(ctx context.Context) *opResult {
		body, bindErr := request.BindCreateRegistration(c)
		if bindErr != nil {
			return h.bindResult(c, bindErr)
		}
		created, err := h.service.Create(ctx, actor, registrationservice.CreateInput{
			PatientCardID: body.PatientCardID,
			ScheduleID:    *body.ScheduleID,
			PaymentMethod: body.PaymentMethod,
		})
		if err != nil {
			return h.serviceErrorResult(c, err)
		}
		if created == nil {
			return nil
		}
		return &opResult{
			status:  http.StatusCreated,
			headers: map[string]string{"Location": registrationLocation(created.Registration.ID)},
			body:    response.NewRegistrationResource(created.Registration, created.Payment),
		}
	})
}

// List 处理 GET /api/v1/registrations：管理端按条件查询，患者端只返回本人记录（契约 §6.3）。
func (h *RegistrationHandler) List(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	q, err := request.BindRegistrationList(c)
	if err != nil {
		h.validation(c, err)
		return
	}

	page, err := h.service.List(c.Request.Context(), actor, registrationservice.ListQuery{
		PatientCardID:   q.PatientCardID,
		DoctorID:        q.DoctorID,
		SubdepartmentID: q.SubdepartmentID,
		FromDate:        q.FromDate,
		ToDate:          q.ToDate,
		PaymentStatus:   q.PaymentStatus,
		Page:            q.Page,
		PageSize:        q.PageSize,
		Sort:            q.Sort,
		Order:           q.Order,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if page == nil {
		h.internal(c)
		return
	}
	c.JSON(http.StatusOK, response.Page[response.RegistrationItem]{
		Items:    response.NewRegistrationItems(page.Items),
		Page:     page.Page,
		PageSize: page.PageSize,
		Total:    page.Total,
	})
}

// Detail 处理 GET /api/v1/registrations/{registrationId}：详情含医生/科室摘要与时段容量。
// 越权访问与不存在统一返回 404 REGISTRATION_NOT_FOUND（契约 §6.4）。
func (h *RegistrationHandler) Detail(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	registrationID, err := request.ParsePositiveID(c.Param("registrationId"))
	if err != nil {
		h.validation(c, err)
		return
	}

	detail, err := h.service.Detail(c.Request.Context(), actor, registrationID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if detail == nil {
		h.internal(c)
		return
	}
	c.JSON(http.StatusOK, response.NewRegistrationDetail(*detail))
}

// actor 从访问令牌载荷构造用例身份；令牌缺失或载荷异常按未认证处理。
func (h *RegistrationHandler) actor(c *gin.Context) (registrationservice.Actor, bool) {
	claims, ok := middleware.ClaimsFrom(c)
	if !ok || claims == nil {
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "访问令牌无效或已过期")
		return registrationservice.Actor{}, false
	}
	actor := registrationservice.Actor{Realm: claims.Realm, UserID: claims.UserID}
	if claims.Realm == domainauth.RealmPatient {
		// 患者域主体固定为令牌中的当前患者，不接受 body/query 传入的患者标识。
		actor.PatientID = claims.UserID
	}
	return actor, true
}

// validation 输出 422 参数错误。
func (h *RegistrationHandler) validation(c *gin.Context, e error) {
	h.writeError(c, http.StatusUnprocessableEntity, registrationservice.CodeValidationFailed, e.Error())
}

// validationResult 生成 422 参数错误的 opResult（绑定失败发生在幂等占位之后，需一并重放）。
func (h *RegistrationHandler) validationResult(_ *gin.Context, e error) *opResult {
	return &opResult{
		status: http.StatusUnprocessableEntity,
		body: map[string]any{
			"code":    registrationservice.CodeValidationFailed,
			"message": e.Error(),
		},
	}
}

// bindResult 按仓库 auth 约定区分请求体绑定错误：JSON 语法/类型错误与截断的 JSON 返回
// 400 REQUEST_INVALID_JSON，其余（空请求体、未知字段、语义错误）走 422。
func (h *RegistrationHandler) bindResult(c *gin.Context, e error) *opResult {
	return bindRequestResult(e, func(err error) *opResult { return h.validationResult(c, err) })
}

// serviceErrorResult 把 use case 错误映射为可重放的 HTTP 结果。
func (h *RegistrationHandler) serviceErrorResult(c *gin.Context, err error) *opResult {
	var serviceErr *registrationservice.ServiceError
	if errors.As(err, &serviceErr) {
		return &opResult{
			status: registrationStatus(serviceErr.Code),
			body:   registrationErrorBody(serviceErr.Code, serviceErr.Message, serviceErr.Details),
		}
	}
	if errors.Is(err, registrationservice.ErrDependencyUnavailable) {
		return &opResult{
			status: http.StatusBadGateway,
			body: registrationErrorBody(
				registrationservice.CodeDependencyUnavailable, "服务暂时不可用，请稍后重试", nil),
		}
	}
	return &opResult{
		status: http.StatusInternalServerError,
		body:   registrationErrorBody("INTERNAL_SERVER_ERROR", "操作失败，请稍后重试", nil),
	}
}

// writeServiceError 直接把 use case 错误写回客户端（只读接口路径）。
func (h *RegistrationHandler) writeServiceError(c *gin.Context, err error) {
	var serviceErr *registrationservice.ServiceError
	if errors.As(err, &serviceErr) {
		h.writeErrorWithDetails(
			c, registrationStatus(serviceErr.Code), serviceErr.Code, serviceErr.Message, serviceErr.Details)
		return
	}
	if errors.Is(err, registrationservice.ErrDependencyUnavailable) {
		h.writeError(c, http.StatusBadGateway, registrationservice.CodeDependencyUnavailable, "服务暂时不可用，请稍后重试")
		return
	}
	h.internal(c)
}

// internal 输出 500，避免把内部错误原文透给客户端。
func (h *RegistrationHandler) internal(c *gin.Context) {
	h.writeError(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "操作失败，请稍后重试")
}

// writeErrorWithDetails 输出带 details 的契约错误体；details 为空时不输出该字段。
func (h *RegistrationHandler) writeErrorWithDetails(
	c *gin.Context,
	status int,
	code string,
	message string,
	details registrationservice.Details,
) {
	body := gin.H{"code": code, "message": message}
	if len(details) > 0 {
		body["details"] = details
	}
	c.JSON(status, body)
}

// registrationErrorBody 组装可序列化的错误响应体（供幂等重放保存字节）。
func registrationErrorBody(code, message string, details registrationservice.Details) map[string]any {
	body := map[string]any{"code": code, "message": message}
	if len(details) > 0 {
		body["details"] = details
	}
	return body
}

// registrationStatus 把业务错误码映射为 HTTP 状态码（契约 §10 错误码目录）。
func registrationStatus(code string) int {
	switch code {
	case registrationservice.CodeValidationFailed:
		return http.StatusUnprocessableEntity
	case registrationservice.CodeCardNotFound,
		registrationservice.CodeSlotNotFound,
		registrationservice.CodeRegistrationNotFound:
		return http.StatusNotFound
	case registrationservice.CodeSlotSoldOut, registrationservice.CodeDuplicate:
		return http.StatusConflict
	case registrationservice.CodeDependencyUnavailable,
		registrationservice.CodePaymentProviderUnavailable:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// registrationLocation 生成创建成功后的 Location 响应头。
func registrationLocation(registrationID int64) string {
	return fmt.Sprintf("/api/v1/registrations/%d", registrationID)
}

// registrationIdempotencyKey 生成建单的幂等存储键：sha256(realm|userID|patientID|scope|key)。
// 前缀与排班域不同，两域的键空间互不覆盖；realm 与主体参与摘要，
// 保证同一 Idempotency-Key 在不同调用者之间不共享结果（契约 §1.5）。
func registrationIdempotencyKey(
	actor registrationservice.Actor,
	scope string,
	key string,
) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%s|%d|%d|%s|%s", actor.Realm, actor.UserID, actor.PatientID, scope, key)))
	return "medical:idem:registrations:" + hex.EncodeToString(sum[:])
}
