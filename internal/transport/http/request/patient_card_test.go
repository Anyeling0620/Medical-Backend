// 就诊卡列表分页参数校验单测：缺省值、上下界与错误文案
// （spec/04-api-contract.md §1.4、§7.4、§12.5）。
//
// 分页窗口必须在请求层收敛，否则负 offset 或 limit=0 会拖到 PostgreSQL
// 才以 2201W/2201X 之类的存储错误暴露出来。
package request

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// bindPatientCardListInTest 在最小 gin 路由上执行 BindPatientCardList 并返回结果。
// 辅助端点固定返回 200，用例只关心绑定结果本身。
func bindPatientCardListInTest(t *testing.T, query string) (PatientCardListQuery, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var got PatientCardListQuery
	var bindErr error
	engine := gin.New()
	engine.GET("/api/v1/patient/cards", func(c *gin.Context) {
		got, bindErr = BindPatientCardList(c)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/patient/cards"+query, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("辅助端点 status = %d，期望 200", w.Code)
	}
	return got, bindErr
}

// TestBindPatientCardListDefaults 覆盖缺省分页：page=1、pageSize=20（契约 §1.4）。
func TestBindPatientCardListDefaults(t *testing.T) {
	query, err := bindPatientCardListInTest(t, "")
	if err != nil {
		t.Fatalf("缺省分页不应报错，实际 %v", err)
	}
	if query.Page != 1 || query.PageSize != 20 {
		t.Fatalf("缺省分页 = page %d / pageSize %d，期望 1 / 20", query.Page, query.PageSize)
	}
}

// TestBindPatientCardListBoundaries 覆盖分页上下界与错误文案：
// page 最大 100000、pageSize 最大 100，越界与非整数一律返回可直接作为
// 422 message 的错误文案。
func TestBindPatientCardListBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantPage   int
		wantSize   int
		wantErrMsg string
	}{
		{name: "page=100000 合法", query: "?page=100000", wantPage: 100000, wantSize: 20},
		{name: "page=100001 非法", query: "?page=100001", wantErrMsg: "page 不能超过 100000"},
		{name: "pageSize=100 合法", query: "?pageSize=100", wantPage: 1, wantSize: 100},
		{name: "pageSize=0 非法", query: "?pageSize=0", wantErrMsg: "pageSize 必须在 1 到 100 之间"},
		{name: "pageSize=101 非法", query: "?pageSize=101", wantErrMsg: "pageSize 必须在 1 到 100 之间"},
		{name: "page=0 非法", query: "?page=0", wantErrMsg: "page 必须从 1 开始"},
		{name: "page 非整数", query: "?page=abc", wantErrMsg: "page 必须为正整数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query, err := bindPatientCardListInTest(t, tc.query)

			if tc.wantErrMsg != "" {
				if err == nil {
					t.Fatalf("期望错误 %q，实际 nil", tc.wantErrMsg)
				}
				if err.Error() != tc.wantErrMsg {
					t.Fatalf("错误文案 = %q，期望 %q", err.Error(), tc.wantErrMsg)
				}
				return
			}

			if err != nil {
				t.Fatalf("合法分页不应报错，实际 %v", err)
			}
			if query.Page != tc.wantPage || query.PageSize != tc.wantSize {
				t.Fatalf(
					"分页 = page %d / pageSize %d，期望 %d / %d",
					query.Page, query.PageSize, tc.wantPage, tc.wantSize,
				)
			}
		})
	}
}

// --- PATCH /api/v1/patient/cards/{cardId} 请求体 ---

// decodeUpdatePatientCardRequest 用与 gin ShouldBindJSON 相同的标准 JSON 解码器
// 解析 PATCH 请求体，保证用例走的是真实解析路径而不是手工构造的结构体。
func decodeUpdatePatientCardRequest(t *testing.T, body string) UpdatePatientCardRequest {
	t.Helper()
	var req UpdatePatientCardRequest
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("解析 PATCH 请求体 %s 失败：%v", body, err)
	}
	return req
}

// TestUpdatePatientCardRequestDetectsImmutableFields 覆盖不可修改字段的「是否提交过」判定：
// 键存在（无论是显式 null 还是真实取值）必须映射为非 nil 指针，
// 键不存在必须保持 nil，否则显式提交的不可修改字段会被静默忽略（契约 §7.4）。
func TestUpdatePatientCardRequestDetectsImmutableFields(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantPID   bool
		wantUser  bool
		wantBirth bool
	}{
		{name: "显式提交 null 的 pid", body: `{"pid":null}`, wantPID: true},
		{name: "显式提交 null 的 userId", body: `{"userId":null}`, wantUser: true},
		{name: "显式提交 null 的 birthday", body: `{"birthday":null}`, wantBirth: true},
		{
			name:      "三个不可修改字段同时显式提交 null",
			body:      `{"pid":null,"userId":null,"birthday":null}`,
			wantPID:   true,
			wantUser:  true,
			wantBirth: true,
		},
		{name: "提交 pid 真实取值", body: `{"pid":"110101199001019999"}`, wantPID: true},
		{name: "显式 null 的 pid 与可修改字段混用", body: `{"pid":null,"tel":"13900139000"}`, wantPID: true},
		// 键存在但取值为空串：判定依据是「原始 JSON 长度」而不是「解出的值是否为空」，
		// 因此空串同样属于已提交（写成 string(r.PID) != "" 之类会漏掉这一情形）。
		{name: "显式提交空串的 pid", body: `{"pid":""}`, wantPID: true},
		{name: "提交 userId 真实取值", body: `{"userId":99}`, wantUser: true},
		{name: "提交 birthday 真实取值", body: `{"birthday":"1991-01-01"}`, wantBirth: true},
		{
			name:      "显式 null 的 userId/birthday 与可修改字段混用",
			body:      `{"userId":null,"birthday":null,"name":"李四"}`,
			wantUser:  true,
			wantBirth: true,
		},
		{name: "空对象", body: `{}`},
		{name: "只提交可修改字段", body: `{"name":"李四","tel":"13900139000"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			update := decodeUpdatePatientCardRequest(t, tc.body).Update()

			if got := update.PID != nil; got != tc.wantPID {
				t.Errorf("pid 判定为已提交 = %t，期望 %t；请求体=%s", got, tc.wantPID, tc.body)
			}
			if got := update.UserID != nil; got != tc.wantUser {
				t.Errorf("userId 判定为已提交 = %t，期望 %t；请求体=%s", got, tc.wantUser, tc.body)
			}
			if got := update.Birthday != nil; got != tc.wantBirth {
				t.Errorf("birthday 判定为已提交 = %t，期望 %t；请求体=%s", got, tc.wantBirth, tc.body)
			}
		})
	}
}

// TestUpdatePatientCardRequestMapsMutableFieldsAndEmptyPlaceholders 覆盖字段映射：
// 可修改字段原样透传；不可修改字段只留空值占位，不把客户端原文带进领域层。
func TestUpdatePatientCardRequestMapsMutableFieldsAndEmptyPlaceholders(t *testing.T) {
	const body = `{"name":"李四","sex":"女","tel":"13900139000",` +
		`"medicalHistory":["高血压","其他"],"insuranceType":"商业医疗保险",` +
		`"pid":"110101199001019999","userId":99,"birthday":"1991-01-01"}`

	update := decodeUpdatePatientCardRequest(t, body).Update()

	if update.Name == nil || *update.Name != "李四" {
		t.Errorf("name = %v，期望 李四", update.Name)
	}
	if update.Sex == nil || *update.Sex != "女" {
		t.Errorf("sex = %v，期望 女", update.Sex)
	}
	if update.Tel == nil || *update.Tel != "13900139000" {
		t.Errorf("tel = %v，期望 13900139000", update.Tel)
	}
	if update.MedicalHistory == nil ||
		!reflect.DeepEqual(*update.MedicalHistory, []string{"高血压", "其他"}) {
		t.Errorf("medicalHistory = %v，期望 [高血压 其他]", update.MedicalHistory)
	}
	if update.InsuranceType == nil || *update.InsuranceType != "商业医疗保险" {
		t.Errorf("insuranceType = %v，期望 商业医疗保险", update.InsuranceType)
	}

	// 不可修改字段只需「非 nil」表达已提交，占位取值必须是空/零，
	// 避免客户端原文进入领域层与日志。
	if update.PID == nil {
		t.Fatal("提交 pid 后 PID 必须非 nil")
	}
	if *update.PID != "" {
		t.Errorf("PID 占位值 = %q，期望空字符串（不携带客户端原文）", *update.PID)
	}
	if update.UserID == nil {
		t.Fatal("提交 userId 后 UserID 必须非 nil")
	}
	if *update.UserID != 0 {
		t.Errorf("UserID 占位值 = %d，期望 0（不携带客户端取值）", *update.UserID)
	}
	if update.Birthday == nil {
		t.Fatal("提交 birthday 后 Birthday 必须非 nil")
	}
	if *update.Birthday != "" {
		t.Errorf("Birthday 占位值 = %q，期望空字符串（不携带客户端原文）", *update.Birthday)
	}
}

// TestUpdatePatientCardRequestUnsubmittedFieldsStayNil 覆盖未提交字段：
// 除显式提交的字段外，其余指针必须为 nil，domain 的 ApplyUpdate 据此跳过修改。
func TestUpdatePatientCardRequestUnsubmittedFieldsStayNil(t *testing.T) {
	update := decodeUpdatePatientCardRequest(t, `{"name":"李四"}`).Update()

	if update.Name == nil || *update.Name != "李四" {
		t.Fatalf("name = %v，期望 李四", update.Name)
	}
	checks := []struct {
		name   string
		nonNil bool
	}{
		{name: "sex", nonNil: update.Sex != nil},
		{name: "tel", nonNil: update.Tel != nil},
		{name: "medicalHistory", nonNil: update.MedicalHistory != nil},
		{name: "insuranceType", nonNil: update.InsuranceType != nil},
		{name: "pid", nonNil: update.PID != nil},
		{name: "userId", nonNil: update.UserID != nil},
		{name: "birthday", nonNil: update.Birthday != nil},
	}
	for _, check := range checks {
		if check.nonNil {
			t.Errorf("未提交的字段 %s 必须保持 nil（表示本次不修改）", check.name)
		}
	}
}
