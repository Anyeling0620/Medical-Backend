package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/transport/http/request"
	"Medical-Web-Backend/internal/transport/http/response"
	paymentservice "Medical-Web-Backend/internal/usecase/payment"
)

// maxNotifyBodyBytes 限制支付宝异步通知的请求体大小：通知是表单 POST，
// 正常体量在 1KB 量级，限制上限可避免异常大表单占用内存。
const maxNotifyBodyBytes = 1 << 20

// PaymentHandler 提供支付域的三个 HTTP 接口（spec/04-api-contract.md §6.5–§6.7）。
//
// 调用者身份完全取自访问令牌：realm=patient 时主体固定为当前患者，realm=mis 时
// 已由中间件完成权限编码校验；异步通知入口不读取任何用户令牌，只信任 RSA2 验签。
type PaymentHandler struct {
	service *paymentservice.Service
}

// NewPaymentHandler 构造支付处理器。
func NewPaymentHandler(service *paymentservice.Service) *PaymentHandler {
	return &PaymentHandler{service: service}
}

// Read 处理 POST /api/v1/payments：幂等读取支付参数（契约 §6.5）。
//
// 本接口是读取语义（只需 PAYMENT:SELECT），患者端只能读取本人挂号关联的订单；
// 已支付、已过期与 30~35 分钟窗口都返回 200，用 payable=false 表达而不是 409。
func (h *PaymentHandler) Read(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	body, err := request.BindPaymentRead(c)
	if err != nil {
		// JSON 语法/类型错误与截断的请求体统一返回 400 REQUEST_INVALID_JSON，
		// 避免把 Go 标准库的英文原文透给客户端（与 schedule_idem.go 的 bindRequestResult 同一口径）；
		// 其余（空体、未知字段、语义）仍按 422 REQUEST_VALIDATION_FAILED 处理。
		if isJSONBindError(err) {
			h.writeError(c, http.StatusBadRequest, codeInvalidJSON, "请求体不是合法的 JSON")
			return
		}
		h.validation(c, err)
		return
	}
	result, err := h.service.ReadPayable(c.Request.Context(), actor, paymentservice.ReadInput{
		RegistrationID: *body.RegistrationID,
		Method:         body.Method,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if result == nil {
		h.internal(c)
		return
	}
	c.JSON(http.StatusOK, response.NewPaymentReadResponse(result.Payment, result.Payable))
}

// Detail 处理 GET /api/v1/payments/{outTradeNo}：查询支付状态（契约 §6.6）。
// 患者端只能查询本人挂号关联的订单，越权与不存在统一返回 404 PAYMENT_NOT_FOUND。
func (h *PaymentHandler) Detail(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	item, err := h.service.Detail(c.Request.Context(), actor, c.Param("outTradeNo"))
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if item == nil {
		h.internal(c)
		return
	}
	c.JSON(http.StatusOK, response.NewPaymentDetailResponse(*item))
}

// Notify 处理 POST /api/v1/payments/alipay/notify：支付宝当面付异步通知入口（契约 §6.7）。
//
// 本接口不使用用户令牌，也不进入权限矩阵：信任来源只有 RSA2 验签。成功响应必须是纯文本
// success（小写、无引号、无 JSON 包裹），这是统一 envelope 的唯一例外；失败响应仍使用
// 统一 envelope，且不得为了「让支付宝停止重试」把验签/身份/金额/订单异常伪装成 success。
func (h *PaymentHandler) Notify(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxNotifyBodyBytes)
	if err := c.Request.ParseForm(); err != nil {
		// 解析失败意味着拿不到签名字段，按验签失败（不可恢复异常）处理。
		h.writeError(c, http.StatusBadRequest,
			paymentservice.CodeNotifySignatureInvalid, "支付通知验签失败")
		return
	}
	form := c.Request.PostForm
	if len(form) == 0 {
		// 通知按契约是表单 POST，这里兼容 query 通道仅为健壮性兜底。
		form = c.Request.Form
	}
	if _, err := h.service.HandleNotify(c.Request.Context(), form); err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte("success"))
}

// actor 从访问令牌载荷构造用例身份；令牌缺失或载荷异常按未认证处理。
func (h *PaymentHandler) actor(c *gin.Context) (paymentservice.Actor, bool) {
	claims, ok := middleware.ClaimsFrom(c)
	if !ok || claims == nil {
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "访问令牌无效或已过期")
		return paymentservice.Actor{}, false
	}
	actor := paymentservice.Actor{Realm: claims.Realm, UserID: claims.UserID}
	if claims.Realm == domainauth.RealmPatient {
		// 患者域主体固定为令牌中的当前患者，不接受 body/query 传入的患者标识。
		actor.PatientID = claims.UserID
	}
	return actor, true
}

// validation 输出 422 参数错误。
func (h *PaymentHandler) validation(c *gin.Context, e error) {
	h.writeError(c, http.StatusUnprocessableEntity, paymentservice.CodeValidationFailed, e.Error())
}

// codeInvalidJSON 是请求体不是合法 JSON 时的契约错误码（契约 §10）。
const codeInvalidJSON = "REQUEST_INVALID_JSON"

// isJSONBindError 判定绑定错误是否属于「请求体不是合法 JSON」：
// 只有 JSON 语法错误、字段类型错误与截断的请求体算这一类，
// 空体/未知字段/语义错误仍归入 422。
func isJSONBindError(err error) bool {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntaxErr) || errors.As(err, &typeErr) || errors.Is(err, io.ErrUnexpectedEOF)
}

// writeServiceError 把 use case 错误映射为契约错误体。
func (h *PaymentHandler) writeServiceError(c *gin.Context, err error) {
	var serviceErr *paymentservice.ServiceError
	if errors.As(err, &serviceErr) {
		h.writeError(c, paymentStatus(serviceErr.Code), serviceErr.Code, serviceErr.Message)
		return
	}
	if errors.Is(err, paymentservice.ErrDependencyUnavailable) {
		h.writeError(c, http.StatusBadGateway,
			paymentservice.CodeDependencyUnavailable, "服务暂时不可用，请稍后重试")
		return
	}
	h.internal(c)
}

// internal 输出 500，避免把内部错误原文透给客户端。
func (h *PaymentHandler) internal(c *gin.Context) {
	h.writeError(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "操作失败，请稍后重试")
}

// writeError 输出统一契约错误体 {code,message}；错误体不含 requestId（与现有实现一致）。
func (h *PaymentHandler) writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"code": code, "message": message})
}

// paymentStatus 把业务错误码映射为 HTTP 状态码（契约 §10 错误码目录）。
func paymentStatus(code string) int {
	switch code {
	case paymentservice.CodeValidationFailed:
		return http.StatusUnprocessableEntity
	case paymentservice.CodeNotifySignatureInvalid,
		paymentservice.CodeNotifyIdentityMismatch,
		paymentservice.CodeAmountMismatch:
		return http.StatusBadRequest
	case paymentservice.CodePaymentNotFound:
		return http.StatusNotFound
	case paymentservice.CodeProviderUnavailable, paymentservice.CodeDependencyUnavailable:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}
