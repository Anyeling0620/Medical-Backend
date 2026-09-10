package repo

import (
	"reflect"
	"strings"
	"testing"

	"Medical-Web-Backend/internal/domain/catalog"
)

// 本文件是公开医生列表 SQL 片段构造函数的纯函数测试（不连数据库）：
// publicDoctorWhere 固定条件与占位符编号、publicDoctorOrderBy 白名单映射与排序方向。

// TestPublicDoctorWhereFixedConditions 固定条件必须是「在岗（status=1）」+「至少关联一个子科室」，
// 且无条件时参数只有 int16(1)，避免调用方放宽公开域可见性。
func TestPublicDoctorWhereFixedConditions(t *testing.T) {
	where, args := publicDoctorWhere(catalog.PublicDoctorFilter{})

	if !strings.HasPrefix(where, " WHERE ") {
		t.Errorf("where 必须以 WHERE 开头，got %q", where)
	}
	if !strings.Contains(where, "d.status = $1") {
		t.Errorf("where 缺少在岗固定条件 d.status = $1：\n%s", where)
	}
	if !strings.Contains(where, "EXISTS (SELECT 1 FROM hospital.medical_dept_sub_and_doctor sd WHERE sd.doctor_id = d.id)") {
		t.Errorf("where 缺少子科室关联固定条件：\n%s", where)
	}
	wantArgs := []any{int16(1)}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
	if got := strings.Count(where, "$"); got != len(args) {
		t.Errorf("占位符数量 = %d, 参数数量 = %d\n%s", got, len(args), where)
	}
}

// TestPublicDoctorWherePlaceholders 过滤条件按 departmentId、subdepartmentId、name 顺序追加占位符，
// 编号连续且与 args 一一对应（LIMIT/OFFSET 会在其后继续追加，不能错位）。
func TestPublicDoctorWherePlaceholders(t *testing.T) {
	departmentID, subdepartmentID := int64(2), int64(9)
	name := "熊"
	where, args := publicDoctorWhere(catalog.PublicDoctorFilter{
		DepartmentID:    &departmentID,
		SubdepartmentID: &subdepartmentID,
		Name:            &name,
	})

	for _, fragment := range []string{
		"d.status = $1",
		"ds.dept_id = $2",
		"sd.dept_sub_id = $3",
		"d.name ILIKE $4",
	} {
		if !strings.Contains(where, fragment) {
			t.Errorf("where 缺少 %q：\n%s", fragment, where)
		}
	}
	wantArgs := []any{int16(1), int64(2), int64(9), "%熊%"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
	if got := strings.Count(where, "$"); got != len(args) {
		t.Errorf("占位符数量 = %d, 参数数量 = %d\n%s", got, len(args), where)
	}
}

// TestPublicDoctorWhereOnlySubdepartment 只按子科室过滤时占位符应为 $1、$2。
func TestPublicDoctorWhereOnlySubdepartment(t *testing.T) {
	subdepartmentID := int64(9)
	where, args := publicDoctorWhere(catalog.PublicDoctorFilter{SubdepartmentID: &subdepartmentID})

	if !strings.Contains(where, "sd.dept_sub_id = $2") {
		t.Errorf("where 子科室条件占位符错误：\n%s", where)
	}
	wantArgs := []any{int16(1), int64(9)}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
}

// TestPublicDoctorOrderBy 排序白名单映射与方向：未知 sort 回落到 d.id，非 desc 一律 ASC。
func TestPublicDoctorOrderBy(t *testing.T) {
	cases := []struct {
		name    string
		sort    string
		order   string
		wantCol string
		wantDir string
	}{
		{"缺省", "", "", "d.id", "ASC"},
		{"name 升序", "name", "asc", "d.name", "ASC"},
		{"hireDate 升序", "hireDate", "asc", "d.hiredate", "ASC"},
		{"recommended 升序", "recommended", "asc", "d.recommended", "ASC"},
		{"id 降序", "id", "desc", "d.id", "DESC"},
		{"name 降序", "name", "desc", "d.name", "DESC"},
		{"hireDate 降序", "hireDate", "desc", "d.hiredate", "DESC"},
		{"recommended 降序", "recommended", "desc", "d.recommended", "DESC"},
		{"未知 sort 回落 d.id", "password", "asc", "d.id", "ASC"},
		{"未知 sort 且 desc 仍回落 d.id", "password", "desc", "d.id", "DESC"},
		{"order 非法回落 ASC", "name", "drop", "d.name", "ASC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			col, dir := publicDoctorOrderBy(catalog.PublicDoctorFilter{Sort: tc.sort, Order: tc.order})
			if col != tc.wantCol || dir != tc.wantDir {
				t.Errorf("publicDoctorOrderBy(%q,%q) = (%q,%q), want (%q,%q)",
					tc.sort, tc.order, col, dir, tc.wantCol, tc.wantDir)
			}
		})
	}
}

// TestPublicDoctorColumnsTrimmed 公开列清单不得包含管理端敏感列（pid、tel、address、email、uuid 等）。
func TestPublicDoctorColumnsTrimmed(t *testing.T) {
	for _, forbidden := range []string{"pid", "tel", "address", "email", "uuid", "status", "birthday", "school", "remark", "hiredate", "tags", "create_date"} {
		if strings.Contains(strings.ToLower(publicDoctorColumns), forbidden) {
			t.Errorf("公开列清单不应包含 %q：%s", forbidden, publicDoctorColumns)
		}
	}
	want := "d.id,d.name,d.sex,d.photo,d.degree,d.job,d.description,d.recommended"
	if publicDoctorColumns != want {
		t.Errorf("publicDoctorColumns = %q, want %q", publicDoctorColumns, want)
	}
	if publicDoctorActiveStatusCode != int16(1) {
		t.Errorf("publicDoctorActiveStatusCode = %d, want 1", publicDoctorActiveStatusCode)
	}
}
