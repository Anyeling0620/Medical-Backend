package repo

import (
	"reflect"
	"strings"
	"testing"

	"Medical-Web-Backend/internal/domain/doctorpatient"
)

// 本文件覆盖「医生本人患者」列表的 SQL 片段纯函数
// （internal/repo/postgres_doctor_patient.go，契约 §6.10），不连接数据库：
// 列表与计数两条查询共用 doctorPatientWhere，$1 必须固定为医生编号，
// 关键词从 $2 起并做 ILIKE 转义，排序只允许白名单表达式，绝不拼接用户输入。

// TestDoctorPatientWhereWithoutKeyword 无关键词时 where 为空、参数只有医生编号。
func TestDoctorPatientWhereWithoutKeyword(t *testing.T) {
	where, args := doctorPatientWhere(doctorpatient.Filter{DoctorID: 16})
	if where != "" {
		t.Errorf("无关键词 where 应为空，got %q", where)
	}
	if len(args) != 1 || args[0] != int64(16) {
		t.Errorf("args = %#v, want [int64(16)]", args)
	}
	// 计数查询的固定来源只引用医生编号这一个占位符；无关键词时不得多出占位符。
	if got := strings.Count(doctorPatientCountFrom+where, "$"); got != 1 {
		t.Errorf("计数查询占位符数量 = %d, want 1\n%s", got, doctorPatientCountFrom+where)
	}
	if !strings.Contains(doctorPatientCountFrom, "$1") {
		t.Error("计数查询来源必须把医生编号固定为 $1（与列表查询参数顺序一致）")
	}
}

// TestDoctorPatientWhereWithKeyword 有关键词时从 $2 起，姓名与电话共用同一个参数，
// 并显式声明 ESCAPE 字符，避免反斜杠语义随数据库会话设置变化。
func TestDoctorPatientWhereWithKeyword(t *testing.T) {
	where, args := doctorPatientWhere(doctorpatient.Filter{DoctorID: 16, Keyword: "张"})
	if !strings.HasPrefix(where, " WHERE ") {
		t.Fatalf("where 必须以 WHERE 开头，got %q", where)
	}
	wantArgs := []any{int64(16), "%张%"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
	if got := strings.Count(where, "$"); got != len(args) {
		t.Errorf("占位符数量 = %d, 参数数量 = %d\n%s", got, len(args), where)
	}
	for _, fragment := range []string{"c.name ILIKE $2", "c.tel ILIKE $2"} {
		if !strings.Contains(where, fragment) {
			t.Errorf("where 缺少 %q:\n%s", fragment, where)
		}
	}
	// 姓名与电话必须共用 $2：多出占位符会让 LIMIT/OFFSET 与参数错位。
	if strings.Contains(where, "$3") {
		t.Errorf("姓名与电话必须共用一个参数，不应出现 $3:\n%s", where)
	}
	if !strings.Contains(where, `ESCAPE '\'`) {
		t.Errorf(`where 必须显式声明 ESCAPE '\':`+"\n%s", where)
	}
	// 计数查询与列表查询共用同一段 where，$1 必须仍是医生编号。
	if !strings.Contains(doctorPatientCountFrom+where, "$1") {
		t.Error("计数查询与列表查询必须共用医生编号 $1")
	}
}

// TestDoctorPatientWhereEscapesLikeWildcards 用户输入的通配符必须被转义：
// 否则「按姓名查找」会被 % 或 _ 静默变成全表匹配。
func TestDoctorPatientWhereEscapesLikeWildcards(t *testing.T) {
	cases := []struct {
		name    string
		keyword string
		want    string
	}{
		{"百分号被转义", "100%", `%100\%%`},
		{"下划线被转义", "a_b", `%a\_b%`},
		{"反斜杠被转义", `a\b`, `%a\\b%`},
		{"混合通配符", `%_\`, `%\%\_\\%`},
		{"纯通配符不再匹配全部", "%", `%\%%`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, args := doctorPatientWhere(doctorpatient.Filter{DoctorID: 16, Keyword: tc.keyword})
			if len(args) != 2 {
				t.Fatalf("args = %#v, want 两个参数（医生编号 + 模式）", args)
			}
			pattern, ok := args[1].(string)
			if !ok {
				t.Fatalf("args[1] = %#v, want string 模式", args[1])
			}
			if pattern != tc.want {
				t.Errorf("模式 = %q, want %q", pattern, tc.want)
			}
			if !strings.HasPrefix(pattern, "%") || !strings.HasSuffix(pattern, "%") {
				t.Errorf("模式必须以 %% 首尾包裹，got %q", pattern)
			}
			// 除首尾包裹用的 % 外，模式内不允许出现未转义的通配符。
			inner := strings.TrimSuffix(strings.TrimPrefix(pattern, "%"), "%")
			for i := 0; i < len(inner); i++ {
				if inner[i] != '%' && inner[i] != '_' {
					continue
				}
				if i == 0 || inner[i-1] != '\\' {
					t.Errorf("模式中存在未转义的通配符 %q: %q", string(inner[i]), pattern)
				}
			}
		})
	}
}

// TestDoctorPatientOrderBy 排序表达式只来自白名单，方向只认 asc（大小写不敏感），
// 其余一律回落默认排序，保证分页顺序稳定。
func TestDoctorPatientOrderBy(t *testing.T) {
	cases := []struct {
		name  string
		sort  string
		order string
		want  string
	}{
		{"缺省按最近就诊日期降序", "", "", "agg.last_visit_date DESC NULLS LAST, c.id ASC"},
		{"最近就诊日期升序", doctorpatient.SortLastVisitDate, "asc", "agg.last_visit_date ASC NULLS LAST, c.id ASC"},
		{"姓名降序", doctorpatient.SortName, "desc", "COALESCE(c.name, '') DESC NULLS LAST, c.id ASC"},
		{"挂号次数升序", doctorpatient.SortRegistrationCount, "asc", "agg.registration_count ASC NULLS LAST, c.id ASC"},
		{"未知 sort 回落最近就诊日期", "pid", "asc", "agg.last_visit_date ASC NULLS LAST, c.id ASC"},
		{"order 非 asc 回落 DESC", doctorpatient.SortName, "drop", "COALESCE(c.name, '') DESC NULLS LAST, c.id ASC"},
		{"order 大小写不敏感", doctorpatient.SortName, "ASC", "COALESCE(c.name, '') ASC NULLS LAST, c.id ASC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := doctorPatientOrderBy(doctorpatient.Filter{Sort: tc.sort, Order: tc.order})
			if got != tc.want {
				t.Errorf("doctorPatientOrderBy(%q,%q) = %q, want %q", tc.sort, tc.order, got, tc.want)
			}
		})
	}
}

// TestDoctorPatientOrderByIgnoresUserInput 排序表达式不得包含用户输入原文，
// 未知 sort/order 必须整体回落到默认排序（契约 §1.4：禁止拼接用户输入）。
func TestDoctorPatientOrderByIgnoresUserInput(t *testing.T) {
	const fallback = "agg.last_visit_date DESC NULLS LAST, c.id ASC"
	payloads := []string{
		"c.name; DROP TABLE hospital.patient_user_info_card--",
		"name) DESC, (SELECT pg_sleep(10))--",
		"name ASC, c.tel",
		"1=1",
	}
	for _, payload := range payloads {
		t.Run(payload, func(t *testing.T) {
			got := doctorPatientOrderBy(doctorpatient.Filter{Sort: payload, Order: payload})
			if got != fallback {
				t.Errorf("未知 sort/order 必须回落到默认排序，got %q", got)
			}
			if strings.Contains(got, payload) {
				t.Errorf("排序表达式包含用户输入原文：%q", got)
			}
		})
	}
}
