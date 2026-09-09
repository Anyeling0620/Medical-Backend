package schedule

import (
	"testing"
	"time"
)

// TestCanModifyShanghaiBusinessDayBoundary 验证“计划日期等于业务当日即锁定”按
// Asia/Shanghai（+08:00）日历日归一：UTC 前一日 16:30（上海次日 00:30）时，
// 上海当日计划应锁定，次日计划仍可写。
func TestCanModifyShanghaiBusinessDayBoundary(t *testing.T) {
	// 2026-09-08 16:30 UTC == 上海时间 2026-09-09 00:30（业务日已是 09-09）。
	now := time.Date(2026, 9, 8, 16, 30, 0, 0, time.UTC)
	if now.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Day() != 9 {
		t.Fatalf("前置条件错误：UTC 时间未落在上海 09-09 业务日")
	}

	// 上海 09-09 当天的计划：已开始，不可写。
	if (WorkPlan{Date: "2026-09-09"}).CanModify(now) {
		t.Errorf("业务当日（上海 09-09）的计划应已开始，CanModify 应为 false")
	}
	// 上海 09-10 的计划：仍在未来，可写。
	if !(WorkPlan{Date: "2026-09-10"}).CanModify(now) {
		t.Errorf("业务次日（上海 09-10）的计划应可写，CanModify 应为 true")
	}
	// 上海 09-08（UTC 日期上仍是 09-08）的计划：早于业务当日，视为已结束，不可写。
	if (WorkPlan{Date: "2026-09-08"}).CanModify(now) {
		t.Errorf("业务前一日（上海 09-08）的计划应不可写")
	}

	// 时段借用计划日期判定，行为应一致。
	plan := WorkPlan{Date: "2026-09-09"}
	if (ScheduleSlot{Used: 0}).CanModify(plan, now) {
		t.Errorf("业务当日计划下的时段不应可写")
	}
}

// TestCanModifyUtcLateNightStillUsesShanghaiDate UTC 23:30 在上海已是次日 07:30，
// 不应按 UTC 日期误判当天计划仍可写。
func TestCanModifyUtcLateNightStillUsesShanghaiDate(t *testing.T) {
	// 2026-09-09 23:30 UTC == 上海时间 2026-09-10 07:30。
	now := time.Date(2026, 9, 9, 23, 30, 0, 0, time.UTC)
	if (WorkPlan{Date: "2026-09-10"}).CanModify(now) {
		t.Errorf("按上海日历 09-10 的计划已开始，CanModify 应为 false")
	}
	if !(WorkPlan{Date: "2026-09-11"}).CanModify(now) {
		t.Errorf("按上海日历 09-11 的计划应可写")
	}
}

// TestValidateNewAcceptsTodayAtShanghaiMidnight 创建校验沿用同一业务日界：
// 上海 09-09 00:30 提交 09-09 当天计划不被判为过去（契约仅禁止早于业务当日）。
func TestValidateNewAcceptsTodayAtShanghaiMidnight(t *testing.T) {
	now := time.Date(2026, 9, 8, 16, 30, 0, 0, time.UTC) // 上海 09-09 00:30
	plan := WorkPlan{DoctorID: 1, SubdepartmentID: 2, Date: "2026-09-09", Maximum: 45}
	if err := plan.ValidateNew(now); err != nil {
		t.Fatalf("业务当日计划应可创建：%v", err)
	}
	if err := (WorkPlan{DoctorID: 1, SubdepartmentID: 2, Date: "2026-09-08", Maximum: 45}).ValidateNew(now); err != ErrDateInPast {
		t.Fatalf("业务前一日计划应报 ErrDateInPast，got %v", err)
	}
}
