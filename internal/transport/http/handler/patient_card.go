package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/patient"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/transport/http/request"
	"Medical-Web-Backend/internal/transport/http/response"
	patientcardservice "Medical-Web-Backend/internal/usecase/patientcard"
)

// PatientCardHandler 处理 /api/v1/patient/cards* 就诊卡接口
// （spec/04-api-contract.md §7.4、§12.5）。
//
// 患者主体固定取自访问令牌（realm=patient），不接受 body/query 传入的患者标识；
// 就诊卡归属由 use case 判定，他人卡与不存在的卡统一按 404 PATIENT_CARD_NOT_FOUND 返回。
type PatientCardHandler struct {
	service *patientcardservice.Service
}

// NewPatientCardHandler 构造就诊卡 handler。
func NewPatientCardHandler(service *patientcardservice.Service) *PatientCardHandler {
	return &PatientCardHandler{service: service}
}

// List 处理 GET /api/v1/patient/cards：当前患者的就诊卡分页列表。
// 每个账号最多一张卡，但接口仍按统一列表结构返回 { items, page, pageSize, total }。
func (h *PatientCardHandler) List(c *gin.Context) {
	patientID, ok := h.currentPatientID(c)
	if !ok {
		return
	}

	query, err := request.BindPatientCardList(c)
	if err != nil {
		h.writeError(c, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", err.Error())
		return
	}

	page, err := h.service.ListCards(c.Request.Context(), patientID, query.Page, query.PageSize)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	c.JSON(http.StatusOK, response.NewPatientCardListResponse(
		page.Items,
		page.Page,
		page.PageSize,
		page.Total,
	))
}

// Detail 处理 GET /api/v1/patient/cards/{cardId}：就诊卡详情，pid 脱敏、tel 明文。
func (h *PatientCardHandler) Detail(c *gin.Context) {
	patientID, ok := h.currentPatientID(c)
	if !ok {
		return
	}
	cardID, ok := h.pathCardID(c)
	if !ok {
		return
	}

	card, err := h.service.GetCard(c.Request.Context(), patientID, cardID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if card == nil {
		// 用例成功时必返回实体；这里兜底，避免 repository 异常返回 (nil, nil) 时 panic。
		h.internalError(c)
		return
	}

	c.JSON(http.StatusOK, card.View())
}

// Create 处理 POST /api/v1/patient/cards：一次性提交全部必填字段建卡。
// 成功返回 201 与脱敏后的就诊卡（契约 §12.5）。
func (h *PatientCardHandler) Create(c *gin.Context) {
	patientID, ok := h.currentPatientID(c)
	if !ok {
		return
	}

	var body request.CreatePatientCardRequest
	if !h.bindJSON(c, &body) {
		return
	}

	card, err := h.service.CreateCard(c.Request.Context(), patientID, patient.CardInput{
		Name:           body.Name,
		Sex:            body.Sex,
		PID:            body.PID,
		Tel:            body.Tel,
		MedicalHistory: body.MedicalHistory,
		InsuranceType:  body.InsuranceType,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if card == nil {
		h.internalError(c)
		return
	}

	c.JSON(http.StatusCreated, card.View())
}

// Update 处理 PATCH /api/v1/patient/cards/{cardId}：只允许修改
// name、sex、tel、medicalHistory、insuranceType（契约 §7.4、§12.5）。
func (h *PatientCardHandler) Update(c *gin.Context) {
	patientID, ok := h.currentPatientID(c)
	if !ok {
		return
	}
	cardID, ok := h.pathCardID(c)
	if !ok {
		return
	}

	var body request.UpdatePatientCardRequest
	if !h.bindJSON(c, &body) {
		return
	}

	// body.Update() 已经把「客户端提交了不可修改字段」翻译成领域入参里的非 nil 指针：
	// 提交 pid 返回 422 PATIENT_CARD_PID_IMMUTABLE，提交 userId/birthday 返回 422。
	updated, err := h.service.UpdateCard(c.Request.Context(), patientID, cardID, body.Update())
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if updated == nil {
		h.internalError(c)
		return
	}

	// 响应与 GET 详情复用同一个投影：契约 §12.5 的 PATCH 示例漏写了 uuid，
	// 但 §2.2 明确把 uuid 列为 patientCard 的返回字段，这里保持一致，避免同一资源
	// 在不同接口下字段形状不同。
	c.JSON(http.StatusOK, updated.View())
}

// currentPatientID 取出访问令牌中的患者主键。
// 令牌缺失/载荷异常时按未认证处理，绝不按请求参数降级放行。
func (h *PatientCardHandler) currentPatientID(c *gin.Context) (int64, bool) {
	claims, ok := middleware.ClaimsFrom(c)
	if !ok || claims.UserID <= 0 {
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "患者访问令牌无效")
		return 0, false
	}
	return claims.UserID, true
}

// pathCardID 解析路径中的就诊卡编号；非正整数按参数错误返回 422，
// 与契约 §12.5 中「编号必须为正整数」的其他资源口径一致。
func (h *PatientCardHandler) pathCardID(c *gin.Context) (int64, bool) {
	cardID, err := request.ParsePositiveID(c.Param("cardId"))
	if err != nil {
		h.writeError(
			c,
			http.StatusUnprocessableEntity,
			"REQUEST_VALIDATION_FAILED",
			"就诊卡编号必须为正整数",
		)
		return 0, false
	}
	return cardID, true
}

// bindJSON 解析请求体：JSON 语法错误、类型不匹配与截断返回 400 REQUEST_INVALID_JSON
// （契约 §1.3）。空请求体按「所有字段都未提交」处理，交给领域校验给出 422 与字段清单，
// 与排班写接口的空请求体口径保持一致。
func (h *PatientCardHandler) bindJSON(c *gin.Context, target any) bool {
	err := c.ShouldBindJSON(target)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}

	var syntaxError *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &syntaxError) ||
		errors.As(err, &typeError) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		h.writeError(c, http.StatusBadRequest, "REQUEST_INVALID_JSON", "请求体不是合法的 JSON")
		return false
	}

	// 其余绑定失败同样按请求体不合法处理：不透出内部错误原文。
	h.writeError(c, http.StatusBadRequest, "REQUEST_INVALID_JSON", "请求体不是合法的 JSON")
	return false
}

// writeServiceError 把 use case 与领域错误映射为契约错误码（§10 错误码目录）。
// 判定顺序有意义：*patient.FieldError 承载的不可修改字段错误必须先于通用字段
// 校验分支匹配，否则会被归入 REQUEST_VALIDATION_FAILED 的 details.fields 分支。
func (h *PatientCardHandler) writeServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, patient.ErrPatientNotFound):
		// 令牌有效但账号已不存在：不枚举账号状态，统一按令牌无效处理（与 /patient/me 一致）。
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "患者访问令牌无效")
	case errors.Is(err, patient.ErrCardNotFound):
		h.writeError(c, http.StatusNotFound, "PATIENT_CARD_NOT_FOUND", "就诊卡不存在")
	case errors.Is(err, patient.ErrCardExists):
		h.writeError(c, http.StatusConflict, "PATIENT_CARD_EXISTS", "该账号已存在就诊卡")
	case errors.Is(err, patient.ErrPIDImmutable):
		h.writeError(c, http.StatusUnprocessableEntity, "PATIENT_CARD_PID_IMMUTABLE", "身份证号不可修改")
	case errors.Is(err, patient.ErrUserIDImmutable):
		h.writeError(c, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", "不允许修改 userId")
	case errors.Is(err, patient.ErrBirthdayImmutable):
		h.writeError(c, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", "不允许修改 birthday")
	case errors.Is(err, patient.ErrInvalidPagination):
		// 分页窗口由请求层收敛；走到这里说明有调用方绕过校验，仍按 422 处理而不是 500。
		h.writeError(c, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", "分页参数不正确")
	case isPatientCardFieldValidation(err):
		h.writeCardValidationError(c, err)
	case errors.Is(err, patientcardservice.ErrDependencyUnavailable):
		h.writeError(c, http.StatusBadGateway, "DEPENDENCY_UNAVAILABLE", "服务暂时不可用，请稍后重试")
	default:
		h.internalError(c)
	}
}

// writeCardValidationError 输出 422 与 details.fields（契约 §12.5 建卡示例：
// 缺失 medicalHistory、insuranceType 时同时列出两个字段）。
func (h *PatientCardHandler) writeCardValidationError(c *gin.Context, err error) {
	body := gin.H{
		"code":    "REQUEST_VALIDATION_FAILED",
		"message": patientCardValidationMessage(err),
	}
	// 字段名只包含契约字段名（name、sex、pid、tel、medicalHistory、insuranceType），
	// 不包含用户提交的取值，可安全回显。
	if fields := patient.FieldsOf(err); len(fields) > 0 {
		body["details"] = gin.H{"fields": fields}
	}
	c.JSON(http.StatusUnprocessableEntity, body)
}

// isPatientCardFieldValidation 判断错误链上是否存在就诊卡字段级校验错误。
func isPatientCardFieldValidation(err error) bool {
	var fieldErr *patient.FieldError
	return errors.As(err, &fieldErr)
}

// patientCardValidationMessage 按出错字段选择用户可见文案，
// 与契约 §12.5 的三个 422 示例一一对应（性别不一致、身份证号/手机号格式、必填字段缺失）；
// 其余取值非法的情况各自给出专用文案，避免全部落到笼统的兜底句（契约 §1.3：message 面向用户）。
func patientCardValidationMessage(err error) string {
	fields := patient.FieldsOf(err)
	switch {
	case errors.Is(err, patient.ErrSexMismatch):
		return "性别与身份证号不一致"
	case errors.Is(err, patient.ErrBirthdayInFuture):
		// 身份证号校验位正确但仍编码了未来日期，字段名也是 pid，需先于 pid 格式分支判定。
		return "身份证号推导的出生日期不合法"
	case hasCardField(fields, "pid") && hasCardField(fields, "tel"):
		return "身份证号或联系电话格式不正确"
	case hasCardField(fields, "pid"):
		return "身份证号格式不正确"
	case hasCardField(fields, "tel"):
		return "联系电话格式不正确"
	case errors.Is(err, patient.ErrNameRequired),
		errors.Is(err, patient.ErrMedicalHistoryRequired),
		errors.Is(err, patient.ErrInsuranceTypeRequired):
		return "就诊卡必填字段不完整"
	case errors.Is(err, patient.ErrNameTooLong):
		return "姓名长度超出限制"
	case errors.Is(err, patient.ErrSexInvalid):
		return "性别取值不正确"
	case errors.Is(err, patient.ErrMedicalHistoryInvalid):
		return "疾病史含不支持的取值"
	case errors.Is(err, patient.ErrMedicalHistoryNoneConflict):
		return "疾病史「无」不能与其他选项同时提交"
	case errors.Is(err, patient.ErrInsuranceTypeInvalid):
		return "医保类型取值不正确"
	default:
		return "就诊卡信息不正确"
	}
}

// hasCardField 判断字段清单中是否包含指定字段名。
func hasCardField(fields []string, name string) bool {
	for _, field := range fields {
		if field == name {
			return true
		}
	}
	return false
}

// internalError 统一输出 500，避免把内部错误原文透给客户端。
func (h *PatientCardHandler) internalError(c *gin.Context) {
	h.writeError(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "操作失败，请稍后重试")
}

// writeError 输出契约错误体：code 为稳定的大写下划线编码，message 面向用户且不含敏感信息。
// 与仓库现状一致，响应体暂不含 requestId——它是既有基线待统一项
// （见 handler/schedule.go、handler/schedule_slots.go 的同类说明），不随本切片单独引入。
func (h *PatientCardHandler) writeError(c *gin.Context, status int, code string, message string) {
	c.JSON(status, gin.H{
		"code":    code,
		"message": message,
	})
}
