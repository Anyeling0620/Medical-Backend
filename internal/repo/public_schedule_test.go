package repo

import (
	"reflect"
	"strings"
	"testing"

	"Medical-Web-Backend/internal/domain/schedule"
)

// 本文件是公开排班 SQL 片段构造函数的纯函数测试（不连数据库）：
// publicScheduleWhere 固定条件「医生在岗」、过滤条件占位符编号与顺序、日期参数的类型转换。

// TestPublicScheduleWhereFixedCondition 固定条件必须是 d.status = $1（在岗医生才可挂号）。
func TestPublicScheduleWhereFixedCondition(t *testing.T) {
	where, args := publicScheduleWhere(schedule.PublicScheduleFilter{})

	if !strings.HasPrefix(where, " WHERE ") {
		t.Errorf("where 必须以 WHERE 开头，got %q", where)
	}
	if !strings.Contains(where, "d.status = $1") {
		t.Errorf("where 缺少在岗固定条件 d.status = $1：\n%s", where)
	}
	wantArgs := []any{int16(1)}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
	if got := strings.Count(where, "$"); got != len(args) {
		t.Errorf("占位符数量 = %d, 参数数量 = %d\n%s", got, len(args), where)
	}
}

// TestPublicScheduleWherePlaceholders 四个过滤条件按 subdepartmentId、doctorId、fromDate、toDate
// 顺序追加占位符，编号连续且与 args 顺序一致。
func TestPublicScheduleWherePlaceholders(t *testing.T) {
	subdepartmentID, doctorID := int64(2), int64(16)
	where, args := publicScheduleWhere(schedule.PublicScheduleFilter{
		SubdepartmentID: &subdepartmentID,
		DoctorID:        &doctorID,
		FromDate:        "2026-09-20",
		ToDate:          "2026-09-26",
	})

	for _, fragment := range []string{
		"d.status = $1",
		"p.dept_sub_id = $2",
		"p.doctor_id = $3",
		"p.date >= $4::date",
		"p.date <= $5::date",
	} {
		if !strings.Contains(where, fragment) {
			t.Errorf("where 缺少 %q：\n%s", fragment, where)
		}
	}
	wantArgs := []any{int16(1), int64(2), int64(16), "2026-09-20", "2026-09-26"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
	if got := strings.Count(where, "$"); got != len(args) {
		t.Errorf("占位符数量 = %d, 参数数量 = %d\n%s", got, len(args), where)
	}
}

// TestPublicScheduleWhereSubdepartmentOnly 只按子科室过滤时占位符应为 $1、$2，且不带日期条件。
func TestPublicScheduleWhereSubdepartmentOnly(t *testing.T) {
	subdepartmentID := int64(2)
	where, args := publicScheduleWhere(schedule.PublicScheduleFilter{SubdepartmentID: &subdepartmentID})

	if !strings.Contains(where, "p.dept_sub_id = $2") {
		t.Errorf("where 子科室条件占位符错误：\n%s", where)
	}
	if strings.Contains(where, "p.date") {
		t.Errorf("未传日期时不应生成日期条件：\n%s", where)
	}
	wantArgs := []any{int16(1), int64(2)}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
}

// TestPublicScheduleWhereOnlyFromDate fromDate 使用 $n::date 显式转换，避免字符串与 date 列比较时依赖隐式转换。
func TestPublicScheduleWhereOnlyFromDate(t *testing.T) {
	where, args := publicScheduleWhere(schedule.PublicScheduleFilter{FromDate: "2026-09-20"})

	if !strings.Contains(where, "p.date >= $2::date") {
		t.Errorf("fromDate 应使用 $2::date：\n%s", where)
	}
	if strings.Contains(where, "p.date <=") {
		t.Errorf("未传 toDate 时不应生成上界条件：\n%s", where)
	}
	wantArgs := []any{int16(1), "2026-09-20"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
}

// TestPublicScheduleWhereOnlyToDate 只传 toDate 时占位符为 $2::date。
func TestPublicScheduleWhereOnlyToDate(t *testing.T) {
	where, args := publicScheduleWhere(schedule.PublicScheduleFilter{ToDate: "2026-09-26"})

	if !strings.Contains(where, "p.date <= $2::date") {
		t.Errorf("toDate 应使用 $2::date：\n%s", where)
	}
	wantArgs := []any{int16(1), "2026-09-26"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
}

// TestPublicScheduleFromJoinsRequiredTables 固定关联必须覆盖时段、计划、医生、子科室四张表，
// 且不得出现挂号表（公开查询只读，不读写号源）。
func TestPublicScheduleFromJoinsRequiredTables(t *testing.T) {
	for _, fragment := range []string{
		"hospital.doctor_work_plan_schedule s",
		"hospital.doctor_work_plan p ON p.id = s.work_plan_id",
		"hospital.doctor d ON d.id = p.doctor_id",
		"hospital.medical_dept_sub ds ON ds.id = p.dept_sub_id",
	} {
		if !strings.Contains(publicScheduleFrom, fragment) {
			t.Errorf("publicScheduleFrom 缺少 %q：\n%s", fragment, publicScheduleFrom)
		}
	}
}
