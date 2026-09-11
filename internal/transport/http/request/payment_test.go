// 支付域请求绑定单测：POST /api/v1/payments 的请求体严格解析
// （spec/04-api-contract.md §6.5、§12.4）。
//
// 覆盖空体、未知字段、registrationId 缺失/非正/类型错误、多余内容与合法值；
// method 的取值校验属于业务语义（用例层判定），本层只做必传与类型校验。
package request

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// bindPaymentReadInTest 在最小 gin 路由上执行 BindPaymentRead 并返回绑定结果。
// 辅助端点不写响应体，用例只关心绑定结果本身。
func bindPaymentReadInTest(t *testing.T, body string) (PaymentReadRequest, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var got PaymentReadRequest
	var bindErr error
	engine := gin.New()
	engine.POST("/api/v1/payments", func(c *gin.Context) {
		got, bindErr = BindPaymentRead(c)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("辅助端点 status = %d，期望 200", w.Code)
	}
	return got, bindErr
}

// TestBindPaymentReadRejectsMissingOrInvalidFields 覆盖可归一为面向用户文案的错误分支：
// 空体、未知字段、registrationId 缺失/为 0/为负与多余内容。
func TestBindPaymentReadRejectsMissingOrInvalidFields(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "空体", body: "", wantErr: "请求体不能为空"},
		{name: "仅空白字符", body: "   ", wantErr: "请求体不能为空"},
		{
			name:    "未知字段",
			body:    `{"registrationId":1001,"method":"ALIPAY","extra":true}`,
			wantErr: "请求体格式不正确（包含未知字段）",
		},
		{name: "registrationId 缺失", body: `{"method":"ALIPAY"}`, wantErr: "registrationId 为必传字段"},
		{name: "registrationId 为 0", body: `{"registrationId":0,"method":"ALIPAY"}`, wantErr: "registrationId 必须为正整数"},
		{name: "registrationId 为负数", body: `{"registrationId":-1,"method":"ALIPAY"}`, wantErr: "registrationId 必须为正整数"},
		{name: "合法 JSON 后跟多余内容", body: `{"registrationId":1001}{"registrationId":1002}`, wantErr: "请求体包含多余内容"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindPaymentReadInTest(t, tc.body)
			if err == nil {
				t.Fatalf("期望绑定失败（%s），实际 err = nil", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("错误文案 = %q，期望 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestBindPaymentReadAcceptsValidBody 合法请求体必须绑定成功并原样保留字段。
func TestBindPaymentReadAcceptsValidBody(t *testing.T) {
	body, err := bindPaymentReadInTest(t, `{"registrationId":1001,"method":"ALIPAY"}`)
	if err != nil {
		t.Fatalf("合法请求体不应报错，实际 %v", err)
	}
	if body.RegistrationID == nil {
		t.Fatal("registrationId 指针不应为 nil")
	}
	if *body.RegistrationID != 1001 {
		t.Fatalf("registrationId = %d，期望 1001", *body.RegistrationID)
	}
	if body.Method != "ALIPAY" {
		t.Fatalf("method = %q，期望 %q", body.Method, "ALIPAY")
	}
}

// TestBindPaymentReadLeavesMethodToUseCase 只提交 registrationId 也能绑定成功：
// method 的「只接受 ALIPAY」是业务语义，由用例层返回 422（契约 §6.5）。
func TestBindPaymentReadLeavesMethodToUseCase(t *testing.T) {
	body, err := bindPaymentReadInTest(t, `{"registrationId":1001}`)
	if err != nil {
		t.Fatalf("缺少 method 时绑定不应报错（由用例层校验），实际 %v", err)
	}
	if body.RegistrationID == nil || *body.RegistrationID != 1001 {
		t.Fatalf("registrationId 绑定结果 = %v，期望 1001", body.RegistrationID)
	}
	if body.Method != "" {
		t.Fatalf("method = %q，期望空串（未提交）", body.Method)
	}
}

// TestBindPaymentReadKeepsJSONSyntaxAndTypeErrors 覆盖 JSON 语法/类型错误：
// 这两类错误保留原始类型，由 handler 映射为 400 REQUEST_INVALID_JSON，
// 不得被归一为「未知字段」这类 422 文案。
func TestBindPaymentReadKeepsJSONSyntaxAndTypeErrors(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantSyntax bool
	}{
		{name: "registrationId 为字符串", body: `{"registrationId":"1001"}`},
		{name: "registrationId 为小数", body: `{"registrationId":1001.5}`},
		{name: "JSON 尾随逗号", body: `{"registrationId":1001,}`, wantSyntax: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindPaymentReadInTest(t, tc.body)
			if err == nil {
				t.Fatal("期望绑定失败，实际 err = nil")
			}
			if tc.wantSyntax {
				var syntaxErr *json.SyntaxError
				if !errors.As(err, &syntaxErr) {
					t.Fatalf("期望 *json.SyntaxError，实际 %T：%v", err, err)
				}
				return
			}
			var typeErr *json.UnmarshalTypeError
			if !errors.As(err, &typeErr) {
				t.Fatalf("期望 *json.UnmarshalTypeError，实际 %T：%v", err, err)
			}
		})
	}
}

// TestBindPaymentReadKeepsTruncatedJSONError 请求体被截断时返回 io.ErrUnexpectedEOF，
// 由 handler 映射为 400 REQUEST_INVALID_JSON。
func TestBindPaymentReadKeepsTruncatedJSONError(t *testing.T) {
	_, err := bindPaymentReadInTest(t, `{"registrationId":`)
	if err == nil {
		t.Fatal("截断的 JSON 期望绑定失败，实际 err = nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("期望 io.ErrUnexpectedEOF，实际 %T：%v", err, err)
	}
}
