// 病历域请求绑定单测：POST /api/v1/medical-records、PATCH /api/v1/medical-records/{medicalRecordId}、
// GET /api/v1/medical-records 的严格解析与错误文案（spec/04-api-contract.md 病历一节、§1.4、§12.4）。
//
// 与 payment_test.go 一致：在最小 gin 路由上用真实 HTTP 请求驱动绑定函数，
// 只断言绑定层行为（用例层语义由 usecase/medical_record 的单测覆盖）。
package request

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	domainmedicalrecord "Medical-Web-Backend/internal/domain/medical_record"
)

// bindCreateMedicalRecordInTest 在最小 gin 路由上执行 BindCreateMedicalRecord 并返回绑定结果。
func bindCreateMedicalRecordInTest(t *testing.T, body string) (CreateMedicalRecordRequest, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var got CreateMedicalRecordRequest
	var bindErr error
	engine := gin.New()
	engine.POST("/api/v1/medical-records", func(c *gin.Context) {
		got, bindErr = BindCreateMedicalRecord(c)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/medical-records", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("辅助端点 status = %d，期望 200", w.Code)
	}
	return got, bindErr
}

// bindUpdateMedicalRecordInTest 在最小 gin 路由上执行 BindUpdateMedicalRecord 并返回绑定结果。
func bindUpdateMedicalRecordInTest(t *testing.T, body string) (UpdateMedicalRecordRequest, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var got UpdateMedicalRecordRequest
	var bindErr error
	engine := gin.New()
	engine.PATCH("/api/v1/medical-records/:medicalRecordId", func(c *gin.Context) {
		got, bindErr = BindUpdateMedicalRecord(c)
	})

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/medical-records/88", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("辅助端点 status = %d，期望 200", w.Code)
	}
	return got, bindErr
}

// bindMedicalRecordListInTest 在最小 gin 路由上执行 BindMedicalRecordList 并返回绑定结果。
func bindMedicalRecordListInTest(t *testing.T, query string) (MedicalRecordListQuery, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var got MedicalRecordListQuery
	var bindErr error
	engine := gin.New()
	engine.GET("/api/v1/medical-records", func(c *gin.Context) {
		got, bindErr = BindMedicalRecordList(c)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/medical-records"+query, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("辅助端点 status = %d，期望 200", w.Code)
	}
	return got, bindErr
}

// TestBindCreateMedicalRecordRejectsMissingOrInvalidFields 覆盖书写请求体的全部字段错误分支：
// 空体、未知字段、registrationId 缺失/非正数、diagnosis 与 content 的空值/纯空白/超长。
func TestBindCreateMedicalRecordRejectsMissingOrInvalidFields(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "空体", body: "", wantErr: "请求体不能为空"},
		{name: "仅空白字符", body: "   ", wantErr: "请求体不能为空"},
		{
			name:    "未知字段",
			body:    `{"registrationId":1001,"diagnosis":"牙髓炎","content":"正文","extra":true}`,
			wantErr: "请求体格式不正确（包含未知字段）",
		},
		{name: "registrationId 缺失", body: `{"diagnosis":"牙髓炎","content":"正文"}`, wantErr: "registrationId 为必传字段"},
		{
			name:    "registrationId 为 0",
			body:    `{"registrationId":0,"diagnosis":"牙髓炎","content":"正文"}`,
			wantErr: "registrationId 必须为正整数",
		},
		{
			name:    "registrationId 为负数",
			body:    `{"registrationId":-1,"diagnosis":"牙髓炎","content":"正文"}`,
			wantErr: "registrationId 必须为正整数",
		},
		{
			name:    "diagnosis 缺失",
			body:    `{"registrationId":1001,"content":"正文"}`,
			wantErr: "diagnosis 为必传字段",
		},
		{
			name:    "diagnosis 为纯空白",
			body:    `{"registrationId":1001,"diagnosis":"   ","content":"正文"}`,
			wantErr: "diagnosis 为必传字段",
		},
		{
			name:    "content 缺失",
			body:    `{"registrationId":1001,"diagnosis":"牙髓炎"}`,
			wantErr: "content 为必传字段",
		},
		{
			name:    "content 为纯空白",
			body:    `{"registrationId":1001,"diagnosis":"牙髓炎","content":"\n\t "}`,
			wantErr: "content 为必传字段",
		},
		{
			name: "diagnosis 超长",
			body: `{"registrationId":1001,"diagnosis":"` +
				strings.Repeat("诊", domainmedicalrecord.MaxDiagnosisRunes+1) + `","content":"正文"}`,
			wantErr: "diagnosis 不能超过 200 个字符",
		},
		{
			name: "content 超长",
			body: `{"registrationId":1001,"diagnosis":"牙髓炎","content":"` +
				strings.Repeat("疗", domainmedicalrecord.MaxContentRunes+1) + `"}`,
			wantErr: "content 不能超过 20000 个字符",
		},
		{
			name:    "合法 JSON 后跟多余内容",
			body:    `{"registrationId":1001,"diagnosis":"牙髓炎","content":"正文"}{"registrationId":1002}`,
			wantErr: "请求体包含多余内容",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindCreateMedicalRecordInTest(t, tc.body)
			if err == nil {
				t.Fatalf("期望绑定失败（%s），实际 err = nil", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("错误文案 = %q，期望 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestBindCreateMedicalRecordAcceptsValidBody 合法请求体必须绑定成功；
// 绑定层不做归一化（首尾空白由用例层裁剪），因此原始串原样保留。
func TestBindCreateMedicalRecordAcceptsValidBody(t *testing.T) {
	body, err := bindCreateMedicalRecordInTest(t,
		`{"registrationId":1001,"diagnosis":"  牙髓炎  ","content":" 主诉：牙痛 "}`)
	if err != nil {
		t.Fatalf("合法请求体不应报错，实际 %v", err)
	}
	if body.RegistrationID == nil || *body.RegistrationID != 1001 {
		t.Fatalf("registrationId = %v，期望 1001", body.RegistrationID)
	}
	if body.Diagnosis != "  牙髓炎  " {
		t.Errorf("diagnosis = %q，期望原样保留 首尾空白（归一化由用例层完成）", body.Diagnosis)
	}
	if body.Content != " 主诉：牙痛 " {
		t.Errorf("content = %q，期望原样保留 首尾空白（归一化由用例层完成）", body.Content)
	}
}

// TestBindCreateMedicalRecordRuneLengthBoundary 长度上限按 rune 计算：
// 恰好等于上限可通过，多 1 个字符被拒（避免用字节数误判中文与 emoji）。
func TestBindCreateMedicalRecordRuneLengthBoundary(t *testing.T) {
	atLimit := `{"registrationId":1001,"diagnosis":"` +
		strings.Repeat("诊", domainmedicalrecord.MaxDiagnosisRunes) +
		`","content":"` + strings.Repeat("😀", domainmedicalrecord.MaxContentRunes) + `"}`
	if _, err := bindCreateMedicalRecordInTest(t, atLimit); err != nil {
		t.Fatalf("恰好等于上限不应报错，实际 %v", err)
	}

	overLimit := `{"registrationId":1001,"diagnosis":"牙髓炎","content":"` +
		strings.Repeat("😀", domainmedicalrecord.MaxContentRunes+1) + `"}`
	_, err := bindCreateMedicalRecordInTest(t, overLimit)
	if err == nil || err.Error() != "content 不能超过 20000 个字符" {
		t.Fatalf("多字节字符超长应被拒，实际 %v", err)
	}
}

// TestBindCreateMedicalRecordKeepsJSONSyntaxAndTypeErrors JSON 语法/类型错误必须保留原始类型，
// 由 handler 映射为 400 REQUEST_INVALID_JSON（不得伪造成 422 参数错误）。
func TestBindCreateMedicalRecordKeepsJSONSyntaxAndTypeErrors(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantSyntax bool
	}{
		{name: "registrationId 为字符串", body: `{"registrationId":"1001","diagnosis":"牙髓炎","content":"正文"}`},
		{name: "registrationId 为小数", body: `{"registrationId":1001.5,"diagnosis":"牙髓炎","content":"正文"}`},
		{name: "JSON 尾随逗号", body: `{"registrationId":1001,}`, wantSyntax: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindCreateMedicalRecordInTest(t, tc.body)
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

// TestBindCreateMedicalRecordKeepsTruncatedJSONError 截断的请求体返回 io.ErrUnexpectedEOF，
// 由 handler 映射为 400 REQUEST_INVALID_JSON。
func TestBindCreateMedicalRecordKeepsTruncatedJSONError(t *testing.T) {
	_, err := bindCreateMedicalRecordInTest(t, `{"registrationId":`)
	if err == nil {
		t.Fatal("截断的 JSON 期望绑定失败，实际 err = nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("期望 io.ErrUnexpectedEOF，实际 %T：%v", err, err)
	}
}

// TestBindUpdateMedicalRecord 覆盖 PATCH 请求体：两字段都为 nil（含显式 null）返回 422；
// 提交空串命中长度下限校验；只提交一个字段时另一个保持 nil（PATCH 语义）。
func TestBindUpdateMedicalRecord(t *testing.T) {
	t.Run("两字段都为 nil", func(t *testing.T) {
		cases := []struct {
			name string
			body string
		}{
			{name: "空对象", body: `{}`},
			{name: "显式 null", body: `{"diagnosis":null,"content":null}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := bindUpdateMedicalRecordInTest(t, tc.body)
				if err == nil {
					t.Fatal("期望绑定失败，实际 err = nil")
				}
				if err.Error() != "至少提交 diagnosis 或 content 中的一个字段" {
					t.Fatalf("错误文案 = %q", err.Error())
				}
			})
		}
	})

	t.Run("提交空串命中校验", func(t *testing.T) {
		cases := []struct {
			name    string
			body    string
			wantErr string
		}{
			{name: "diagnosis 为空串", body: `{"diagnosis":""}`, wantErr: "diagnosis 为必传字段"},
			{name: "diagnosis 为纯空白", body: `{"diagnosis":"  "}`, wantErr: "diagnosis 为必传字段"},
			{name: "content 为空串", body: `{"content":""}`, wantErr: "content 为必传字段"},
			{name: "content 为纯空白", body: `{"content":"\t\n"}`, wantErr: "content 为必传字段"},
			{
				name:    "diagnosis 超长",
				body:    `{"diagnosis":"` + strings.Repeat("诊", domainmedicalrecord.MaxDiagnosisRunes+1) + `"}`,
				wantErr: "diagnosis 不能超过 200 个字符",
			},
			{
				name:    "content 超长",
				body:    `{"content":"` + strings.Repeat("疗", domainmedicalrecord.MaxContentRunes+1) + `"}`,
				wantErr: "content 不能超过 20000 个字符",
			},
			{
				name:    "未知字段",
				body:    `{"diagnosis":"牙髓炎","extra":1}`,
				wantErr: "请求体格式不正确（包含未知字段）",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := bindUpdateMedicalRecordInTest(t, tc.body)
				if err == nil {
					t.Fatalf("期望绑定失败（%s），实际 err = nil", tc.wantErr)
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("错误文案 = %q，期望 %q", err.Error(), tc.wantErr)
				}
			})
		}
	})

	t.Run("部分更新只保留已提交字段", func(t *testing.T) {
		body, err := bindUpdateMedicalRecordInTest(t, `{"diagnosis":"  新诊断  "}`)
		if err != nil {
			t.Fatalf("合法请求体不应报错，实际 %v", err)
		}
		if body.Diagnosis == nil || *body.Diagnosis != "  新诊断  " {
			t.Errorf("diagnosis = %v，期望原样保留（归一化由用例层完成）", body.Diagnosis)
		}
		if body.Content != nil {
			t.Errorf("未提交的 content 必须保持 nil，实际 %v", *body.Content)
		}
	})

	t.Run("空体与截断 JSON 保留原始错误类型", func(t *testing.T) {
		if _, err := bindUpdateMedicalRecordInTest(t, ""); err == nil || err.Error() != "请求体不能为空" {
			t.Fatalf("空体应返回 请求体不能为空，实际 %v", err)
		}
		_, err := bindUpdateMedicalRecordInTest(t, `{"diagnosis":`)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("期望 io.ErrUnexpectedEOF，实际 %T：%v", err, err)
		}
	})
}

// TestBindMedicalRecordListDefaults 列表查询缺省参数：page=1、pageSize=20，全部过滤条件为空。
func TestBindMedicalRecordListDefaults(t *testing.T) {
	q, err := bindMedicalRecordListInTest(t, "")
	if err != nil {
		t.Fatalf("缺省参数不应报错，实际 %v", err)
	}
	if q.Page != 1 || q.PageSize != 20 {
		t.Errorf("page/pageSize = %d/%d, want 1/20", q.Page, q.PageSize)
	}
	if q.RegistrationID != nil || q.PatientCardID != nil || q.DoctorID != nil {
		t.Errorf("过滤条件默认应为空：%+v", q)
	}
}

// TestBindMedicalRecordListParsesQuery 合法查询串必须原样解析出全部可选过滤条件与分页。
func TestBindMedicalRecordListParsesQuery(t *testing.T) {
	values := url.Values{}
	values.Set("registrationId", "1001")
	values.Set("patientCardId", "501")
	values.Set("doctorId", "16")
	values.Set("page", "3")
	values.Set("pageSize", "25")

	q, err := bindMedicalRecordListInTest(t, "?"+values.Encode())
	if err != nil {
		t.Fatalf("合法查询串不应报错，实际 %v", err)
	}
	if q.RegistrationID == nil || *q.RegistrationID != 1001 {
		t.Errorf("registrationId = %v, want 1001", q.RegistrationID)
	}
	if q.PatientCardID == nil || *q.PatientCardID != 501 {
		t.Errorf("patientCardId = %v, want 501", q.PatientCardID)
	}
	if q.DoctorID == nil || *q.DoctorID != 16 {
		t.Errorf("doctorId = %v, want 16", q.DoctorID)
	}
	if q.Page != 3 || q.PageSize != 25 {
		t.Errorf("page/pageSize = %d/%d, want 3/25", q.Page, q.PageSize)
	}
}

// TestBindMedicalRecordListValidation 列表查询边界（契约 §1.4）：
// page 从 1 开始且不超过 100000；pageSize 在 1 到 100 之间；可选编号必须为正整数。
func TestBindMedicalRecordListValidation(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{name: "page 为 0", query: "?page=0", wantErr: "page 必须从 1 开始"},
		{name: "page 为负数", query: "?page=-1", wantErr: "page 必须从 1 开始"},
		{name: "page 非数字", query: "?page=abc", wantErr: "page 必须从 1 开始"},
		{name: "page 超过上限", query: "?page=100001", wantErr: "page 不能超过 100000"},
		{name: "pageSize 为 0", query: "?pageSize=0", wantErr: "pageSize 必须在 1 到 100 之间"},
		{name: "pageSize 为负数", query: "?pageSize=-5", wantErr: "pageSize 必须在 1 到 100 之间"},
		{name: "pageSize 非数字", query: "?pageSize=abc", wantErr: "pageSize 必须在 1 到 100 之间"},
		{name: "pageSize 超过上限", query: "?pageSize=101", wantErr: "pageSize 必须在 1 到 100 之间"},
		{name: "registrationId 为 0", query: "?registrationId=0", wantErr: "registrationId 必须为正整数"},
		{name: "registrationId 非数字", query: "?registrationId=abc", wantErr: "registrationId 必须为正整数"},
		{name: "patientCardId 为 0", query: "?patientCardId=0", wantErr: "patientCardId 必须为正整数"},
		{name: "patientCardId 为负数", query: "?patientCardId=-2", wantErr: "patientCardId 必须为正整数"},
		{name: "doctorId 为 0", query: "?doctorId=0", wantErr: "doctorId 必须为正整数"},
		{name: "doctorId 非数字", query: "?doctorId=x", wantErr: "doctorId 必须为正整数"},
		{
			name:    "sort 参数被拒",
			query:   "?sort=id",
			wantErr: "本接口不支持 sort 参数（固定按 id 倒序）",
		},
		{
			name:    "sort 空值也被拒",
			query:   "?sort=",
			wantErr: "本接口不支持 sort 参数（固定按 id 倒序）",
		},
		{
			name:    "order 参数被拒",
			query:   "?order=asc",
			wantErr: "本接口不支持 order 参数（固定按 id 倒序）",
		},
		{
			name:    "order 空值也被拒",
			query:   "?order=",
			wantErr: "本接口不支持 order 参数（固定按 id 倒序）",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindMedicalRecordListInTest(t, tc.query)
			if err == nil {
				t.Fatalf("期望绑定失败（%s），实际 err = nil", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("错误文案 = %q，期望 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestBindMedicalRecordListAcceptsBoundaries 恰好落在边界上的取值必须被接受。
func TestBindMedicalRecordListAcceptsBoundaries(t *testing.T) {
	q, err := bindMedicalRecordListInTest(t, "?page=100000&pageSize=1")
	if err != nil {
		t.Fatalf("边界值不应报错，实际 %v", err)
	}
	if q.Page != 100000 || q.PageSize != 1 {
		t.Errorf("page/pageSize = %d/%d, want 100000/1", q.Page, q.PageSize)
	}

	q, err = bindMedicalRecordListInTest(t, "?pageSize=100&registrationId=1")
	if err != nil {
		t.Fatalf("边界值不应报错，实际 %v", err)
	}
	if q.PageSize != 100 {
		t.Errorf("pageSize = %d, want 100", q.PageSize)
	}
	if q.RegistrationID == nil || *q.RegistrationID != 1 {
		t.Errorf("registrationId = %v, want 1", q.RegistrationID)
	}
}
