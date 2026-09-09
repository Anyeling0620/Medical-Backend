package request

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
)

// CreateSlotRequest 对应 POST /api/v1/schedule/plans/{planId}/slots 的请求体：{slot, maximum}。
// 使用 int 承载，便于在绑定层输出契约要求的中文范围文案（时段编号与容量均为小整数）。
type CreateSlotRequest struct {
	Slot    int `json:"slot"`
	Maximum int `json:"maximum"`
}

// UpdateSlotRequest 对应 PATCH /api/v1/schedule/slots/{slotId} 的请求体，只允许修改 maximum。
type UpdateSlotRequest struct {
	Maximum int `json:"maximum"`
}

// BindCreateSlotRequest 严格解析创建时段请求体：拒绝未知字段与格式错误；
// 时段编号与最大号源的范围校验输出契约样例的中文文案。
func BindCreateSlotRequest(c *gin.Context) (CreateSlotRequest, error) {
	var req CreateSlotRequest
	if err := decodeStrict(c, &req); err != nil {
		return req, err
	}
	if req.Slot < 1 {
		return req, errors.New("时段编号必须为正整数")
	}
	if req.Slot > 32767 {
		return req, errors.New("时段编号不能超过 32767")
	}
	if req.Maximum < 1 {
		return req, errors.New("时段最大号源必须大于 0")
	}
	if req.Maximum > 32767 {
		return req, errors.New("时段最大号源不能超过 32767")
	}
	return req, nil
}

// BindUpdateSlotRequest 严格解析更新时段请求体：只允许修改 maximum（1-32767）。
func BindUpdateSlotRequest(c *gin.Context) (UpdateSlotRequest, error) {
	var req UpdateSlotRequest
	if err := decodeStrict(c, &req); err != nil {
		return req, err
	}
	if req.Maximum < 1 {
		return req, errors.New("时段最大号源必须大于 0")
	}
	if req.Maximum > 32767 {
		return req, errors.New("时段最大号源不能超过 32767")
	}
	return req, nil
}

// BindIdempotencyKey 校验 Idempotency-Key：1-128 个可打印 ASCII 字符，缺失即报错。
// 幂等协调（原子占位与结果重放）由 handler 负责，此处只做格式校验。
func BindIdempotencyKey(c *gin.Context) (string, error) {
	raw := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if raw == "" {
		return "", errors.New("Idempotency-Key 请求头必填")
	}
	if len(raw) > 128 {
		return "", errors.New("Idempotency-Key 长度必须在 1 到 128 之间")
	}
	for _, b := range []byte(raw) {
		if b < 0x20 || b > 0x7E {
			return "", errors.New("Idempotency-Key 只能包含可打印 ASCII 字符")
		}
	}
	return raw, nil
}

// decodeStrict 使用 DisallowUnknownFields 解析 JSON，并在空 body / 多余 JSON 时报错；
// 底层解码细节被归一为面向用户的中文文案，避免把内部错误直接返回给调用方。
func decodeStrict(c *gin.Context, target any) error {
	if c.Request.Body == nil {
		return errors.New("请求体不能为空")
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("请求体不能为空")
		}
		// 保留 json.SyntaxError/json.UnmarshalTypeError/io.ErrUnexpectedEOF
		// 的原始类型，供 handler 映射 400 REQUEST_INVALID_JSON（与 auth 处理约定一致）；
		// 其余（未知字段等）在这里归一为面向用户的稳定文案（422）。
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) || errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		return errors.New("请求体格式不正确（包含未知字段）")
	}
	// 防止“合法 JSON 后跟多余内容或非 JSON 垃圾”被静默忽略。
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("请求体包含多余内容")
	} else if !errors.Is(err, io.EOF) {
		return errors.New("请求体包含多余内容")
	}
	return nil
}
