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
	domainmedicalrecord "Medical-Web-Backend/internal/domain/medical_record"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/transport/http/request"
	"Medical-Web-Backend/internal/transport/http/response"
	medicalrecordservice "Medical-Web-Backend/internal/usecase/medical_record"
)

// MedicalRecordHandler 提供病历域的五个 HTTP 接口（spec/04-api-contract.md 病历一节）。
//
// 调用者身份完全取自访问令牌：本域只接受 realm=mis 的令牌，路由层已校验
// MEDICAL_RECORD:* 权限码；「只能操作自己负责的挂号记录」这一数据级授权由用例按
// mis_user.ref_id 解析出的医生编号完成，越权与不存在统一返回 404。
// 书写接口（POST）复用 schedule_idem.go 的幂等协调：Claim 原子占位 + 结果重放
// （契约 §1.5：创建接口必须支持 Idempotency-Key）。
type MedicalRecordHandler struct {
	service *medicalrecordservice.Service
	*scheduleIdem
}

// NewMedicalRecordHandler 构造病历处理器；store 为书写接口的幂等存储（Redis）。
func NewMedicalRecordHandler(
	service *medicalrecordservice.Service,
	store port.IdempotencyStore,
) *MedicalRecordHandler {
	return &MedicalRecordHandler{service: service, scheduleIdem: newScheduleIdem(store)}
}

// Create 处理 POST /api/v1/medical-records：医生为指定挂号书写病历。
//
// 必须携带 Idempotency-Key：同一 key 重复提交返回第一次结果，不会重复写入。
// 同一挂号只允许一份病历，重复书写返回 409 MEDICAL_RECORD_DUPLICATE（应改用 PATCH）。
func (h *MedicalRecordHandler) Create(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	key, err := request.BindIdempotencyKey(c)
	if err != nil {
		h.validation(c, err)
		return
	}
	idemKey := medicalRecordIdempotencyKey(
		actor, c.Request.Method+"|"+c.Request.URL.Path, key)

	h.claimAndRun(c, idemKey, func(ctx context.Context) *opResult {
		body, bindErr := request.BindCreateMedicalRecord(c)
		if bindErr != nil {
			return h.bindResult(c, bindErr)
		}
		created, err := h.service.Create(ctx, actor, medicalrecordservice.CreateInput{
			RegistrationID: *body.RegistrationID,
			Diagnosis:      body.Diagnosis,
			Content:        body.Content,
		})
		if err != nil {
			return h.serviceErrorResult(err)
		}
		if created == nil {
			return nil
		}
		return &opResult{
			status:  http.StatusCreated,
			headers: map[string]string{"Location": medicalRecordLocation(created.ID)},
			body:    response.NewMedicalRecordResource(*created),
		}
	})
}

// List 处理 GET /api/v1/medical-records：分页查询病历，只返回当前医生负责的挂号下的记录。
func (h *MedicalRecordHandler) List(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	q, err := request.BindMedicalRecordList(c)
	if err != nil {
		h.validation(c, err)
		return
	}
	page, err := h.service.List(c.Request.Context(), actor, medicalrecordservice.ListQuery{
		RegistrationID: q.RegistrationID,
		PatientCardID:  q.PatientCardID,
		DoctorID:       q.DoctorID,
		Page:           q.Page,
		PageSize:       q.PageSize,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if page == nil {
		h.writeInternal(c)
		return
	}
	c.JSON(http.StatusOK, response.Page[response.MedicalRecordResource]{
		Items:    response.NewMedicalRecordItems(page.Items),
		Page:     page.Page,
		PageSize: page.PageSize,
		Total:    page.Total,
	})
}

// Detail 处理 GET /api/v1/medical-records/{medicalRecordId}：读取单份病历。
func (h *MedicalRecordHandler) Detail(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	recordID, err := request.ParsePositiveID(c.Param("medicalRecordId"))
	if err != nil {
		h.validation(c, err)
		return
	}
	record, err := h.service.Detail(c.Request.Context(), actor, recordID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if record == nil {
		h.writeInternal(c)
		return
	}
	c.JSON(http.StatusOK, response.NewMedicalRecordResource(*record))
}

// Update 处理 PATCH /api/v1/medical-records/{medicalRecordId}：修改病历中提交的字段。
//
// PATCH 只更新请求体里出现的字段：显式传空串会命中长度/必填校验返回 422，
// 不会把已有内容清空（doctor_prescription 的两个内容列都不允许存空值）。
func (h *MedicalRecordHandler) Update(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	recordID, err := request.ParsePositiveID(c.Param("medicalRecordId"))
	if err != nil {
		h.validation(c, err)
		return
	}
	body, err := request.BindUpdateMedicalRecord(c)
	if err != nil {
		h.validation(c, err)
		return
	}
	updated, err := h.service.Update(c.Request.Context(), actor, recordID, domainmedicalrecord.UpdateInput{
		Diagnosis: body.Diagnosis,
		Content:   body.Content,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if updated == nil {
		h.writeInternal(c)
		return
	}
	c.JSON(http.StatusOK, response.NewMedicalRecordResource(*updated))
}

// Delete 处理 DELETE /api/v1/medical-records/{medicalRecordId}：删除病历，成功返回 204。
func (h *MedicalRecordHandler) Delete(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	recordID, err := request.ParsePositiveID(c.Param("medicalRecordId"))
	if err != nil {
		h.validation(c, err)
		return
	}
	if err := h.service.Delete(c.Request.Context(), actor, recordID); err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// actor 从访问令牌载荷构造用例身份；令牌缺失或载荷异常按未认证处理。
func (h *MedicalRecordHandler) actor(c *gin.Context) (medicalrecordservice.Actor, bool) {
	claims, ok := middleware.ClaimsFrom(c)
	if !ok || claims == nil {
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "访问令牌无效或已过期")
		return medicalrecordservice.Actor{}, false
	}
	if claims.Realm != domainauth.RealmMis {
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "访问令牌无效或已过期")
		return medicalrecordservice.Actor{}, false
	}
	return medicalrecordservice.Actor{Realm: claims.Realm, UserID: claims.UserID}, true
}

// validation 输出 422 参数错误。
func (h *MedicalRecordHandler) validation(c *gin.Context, e error) {
	h.writeError(c, http.StatusUnprocessableEntity, medicalrecordservice.CodeValidationFailed, e.Error())
}

// validationResult 生成 422 参数错误的 opResult（供幂等重放保存结果）。
func (h *MedicalRecordHandler) validationResult(_ *gin.Context, e error) *opResult {
	return &opResult{
		status: http.StatusUnprocessableEntity,
		body: map[string]any{
			"code":    medicalrecordservice.CodeValidationFailed,
			"message": e.Error(),
		},
	}
}

// bindResult 区分请求体绑定错误：JSON 语法/类型错误与截断 JSON 返回 400 REQUEST_INVALID_JSON，
// 其余（空请求体、未知字段、语义错误）走 422（与仓库其他写接口一致）。
func (h *MedicalRecordHandler) bindResult(c *gin.Context, e error) *opResult {
	return bindRequestResult(e, func(err error) *opResult { return h.validationResult(c, err) })
}

// serviceErrorResult 把 use case 错误映射为可重放的 HTTP 结果。
func (h *MedicalRecordHandler) serviceErrorResult(err error) *opResult {
	var serviceErr *medicalrecordservice.ServiceError
	if errors.As(err, &serviceErr) {
		return &opResult{
			status: medicalRecordStatus(serviceErr.Code),
			body:   medicalRecordErrorBody(serviceErr.Code, serviceErr.Message, serviceErr.Details),
		}
	}
	if errors.Is(err, medicalrecordservice.ErrDependencyUnavailable) {
		return &opResult{
			status: http.StatusBadGateway,
			body: medicalRecordErrorBody(
				medicalrecordservice.CodeDependencyUnavailable, "服务暂时不可用，请稍后重试", nil),
		}
	}
	return &opResult{
		status: http.StatusInternalServerError,
		body:   medicalRecordErrorBody("INTERNAL_SERVER_ERROR", "操作失败，请稍后重试", nil),
	}
}

// writeServiceError 直接把 use case 错误写回客户端（只读与 PATCH/DELETE 路径）。
func (h *MedicalRecordHandler) writeServiceError(c *gin.Context, err error) {
	var serviceErr *medicalrecordservice.ServiceError
	if errors.As(err, &serviceErr) {
		body := gin.H{"code": serviceErr.Code, "message": serviceErr.Message}
		if len(serviceErr.Details) > 0 {
			body["details"] = serviceErr.Details
		}
		c.JSON(medicalRecordStatus(serviceErr.Code), body)
		return
	}
	if errors.Is(err, medicalrecordservice.ErrDependencyUnavailable) {
		h.writeError(c, http.StatusBadGateway, medicalrecordservice.CodeDependencyUnavailable, "服务暂时不可用，请稍后重试")
		return
	}
	h.writeInternal(c)
}

// medicalRecordErrorBody 组装可序列化的错误响应体（供幂等重放保存字节）。
func medicalRecordErrorBody(code, message string, details medicalrecordservice.Details) map[string]any {
	body := map[string]any{"code": code, "message": message}
	if len(details) > 0 {
		body["details"] = details
	}
	return body
}

// medicalRecordStatus 把业务错误码映射为 HTTP 状态码（契约病历一节的错误码表）。
func medicalRecordStatus(code string) int {
	switch code {
	case medicalrecordservice.CodeValidationFailed:
		return http.StatusUnprocessableEntity
	case medicalrecordservice.CodeNotFound, medicalrecordservice.CodeRegistrationNotFound:
		return http.StatusNotFound
	case medicalrecordservice.CodeForbidden:
		return http.StatusForbidden
	case medicalrecordservice.CodeDuplicate:
		return http.StatusConflict
	case medicalrecordservice.CodeDependencyUnavailable:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// medicalRecordLocation 生成创建成功后的 Location 响应头。
func medicalRecordLocation(recordID int64) string {
	return fmt.Sprintf("/api/v1/medical-records/%d", recordID)
}

// medicalRecordIdempotencyKey 生成书写病历的幂等存储键：
// sha256(realm|userID|scope|key)。前缀与排班域、挂号域不同，三域键空间互不覆盖；
// realm 与主体参与摘要，保证同一 Idempotency-Key 在不同调用者之间不共享结果（契约 §1.5）。
func medicalRecordIdempotencyKey(
	actor medicalrecordservice.Actor,
	scope string,
	key string,
) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%s|%d|%s|%s", actor.Realm, actor.UserID, scope, key)))
	return "medical:idem:medical-records:" + hex.EncodeToString(sum[:])
}
