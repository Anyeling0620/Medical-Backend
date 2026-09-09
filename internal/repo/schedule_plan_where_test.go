package repo

import (
	"reflect"
	"strings"
	"testing"

	"Medical-Web-Backend/internal/domain/schedule"
)

// TestSchedulePlanWherePlaceholders 验证列表过滤条件生成的 SQL 片段与参数占位符一一对应，
// 避免 LIMIT/OFFSET 追加时发生占位符错位。
func TestSchedulePlanWherePlaceholders(t *testing.T) {
	doctor, dept, subdept := int64(16), int64(2), int64(9)
	f := schedule.PlanFilter{
		DoctorID:        &doctor,
		DepartmentID:    &dept,
		SubdepartmentID: &subdept,
		FromDate:        "2026-09-20",
		ToDate:          "2026-09-30",
	}
	where, args := planWhere(f)
	for _, fragment := range []string{
		"p.doctor_id=$1",
		"sd.doctor_id=p.doctor_id AND sd.dept_sub_id=p.dept_sub_id AND ds.dept_id=$2",
		"p.dept_sub_id=$3",
		"p.date>=$4::date",
		"p.date<=$5::date",
	} {
		if !strings.Contains(where, fragment) {
			t.Errorf("where 缺少 %q:\n%s", fragment, where)
		}
	}
	if !strings.HasPrefix(where, " WHERE ") {
		t.Errorf("where 必须以 WHERE 开头，got %q", where)
	}
	if got := strings.Count(where, "$"); got != len(args) {
		t.Errorf("占位符数量 = %d, 参数数量 = %d\n%s", got, len(args), where)
	}
	wantArgs := []any{int64(16), int64(2), int64(9), "2026-09-20", "2026-09-30"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
}

// TestSchedulePlanWhereNoFilters 无条件时 where 为空、无参数，SQL 保持简洁。
func TestSchedulePlanWhereNoFilters(t *testing.T) {
	where, args := planWhere(schedule.PlanFilter{})
	if where != "" {
		t.Errorf("无条件 where 应为空，got %q", where)
	}
	if len(args) != 0 {
		t.Errorf("无条件 args 应为空，got %v", args)
	}
}

// TestSchedulePlanOrderBy 排序字段白名单映射与默认排序方向。
func TestSchedulePlanOrderBy(t *testing.T) {
	cases := []struct {
		name    string
		sort    string
		order   string
		wantCol string
		wantDir string
	}{
		{"默认", "", "", "p.id", "ASC"},
		{"doctorId 降序", "doctorId", "desc", "p.doctor_id", "DESC"},
		{"date 升序", "date", "asc", "p.date", "ASC"},
		{"id 降序", "id", "desc", "p.id", "DESC"},
		{"未知 sort 回退 id", "unknown", "asc", "p.id", "ASC"},
		{"未知 order 回退 asc", "id", "drop", "p.id", "ASC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			col, dir := planOrderBy(schedule.PlanFilter{Sort: tc.sort, Order: tc.order})
			if col != tc.wantCol || dir != tc.wantDir {
				t.Errorf("planOrderBy(%q,%q) = (%q,%q), want (%q,%q)", tc.sort, tc.order, col, dir, tc.wantCol, tc.wantDir)
			}
		})
	}
}

// TestPlanCreateLockKeyDeterministic 验证 planCreateLockKey 对同一创建键（医生/子科室/日期）
// 多次调用始终返回同一 int64，保证 pg_advisory_xact_lock 的排队语义确定（“同创建键等同一把
// 咨询锁”）。FNV-1a 对不同输入理论上存在碰撞，但不同创建键碰撞只会让两个键多排一次队
// （仅多串行化、不影响正确性），因此本测试只断言“同输入同键”，不要求哈希无碰撞。
func TestPlanCreateLockKeyDeterministic(t *testing.T) {
	groups := []struct {
		doctorID int64
		subID    int64
		date     string
	}{
		{16, 2, "2026-09-20"},
		{16, 2, "2026-09-21"},
		{1, 99, "2026-10-01"},
	}
	for _, g := range groups {
		want := planCreateLockKey(g.doctorID, g.subID, g.date)
		for i := 0; i < 5; i++ {
			if got := planCreateLockKey(g.doctorID, g.subID, g.date); got != want {
				t.Fatalf("同输入应产生同键：%+v 第 %d 次 = %d, 首次 = %d", g, i, got, want)
			}
		}
	}
}
