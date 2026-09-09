package schedule

import (
	"errors"
	"testing"
	"time"
)

func TestWorkPlanValidateNew(t *testing.T) {
	now := time.Date(2026, 9, 9, 15, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	valid := WorkPlan{DoctorID: 1, SubdepartmentID: 2, Date: "2026-09-09", Maximum: 45}
	if err := valid.ValidateNew(now); err != nil {
		t.Fatalf("same-day plan should be accepted: %v", err)
	}
	for _, tc := range []struct {
		name string
		plan WorkPlan
		want error
	}{
		{"past date", WorkPlan{DoctorID: 1, SubdepartmentID: 2, Date: "2026-09-08", Maximum: 1}, ErrDateInPast},
		{"invalid date", WorkPlan{DoctorID: 1, SubdepartmentID: 2, Date: "09/10/2026", Maximum: 1}, ErrInvalidDate},
		{"zero maximum", WorkPlan{DoctorID: 1, SubdepartmentID: 2, Date: "2026-09-09", Maximum: 0}, ErrInvalidMaximum},
		{"maximum below used", WorkPlan{DoctorID: 1, SubdepartmentID: 2, Date: "2026-09-09", Maximum: 2, Used: 3}, ErrMaximumBelowUsed},
		{"invalid doctor", WorkPlan{SubdepartmentID: 2, Date: "2026-09-09", Maximum: 1}, ErrInvalidDoctor},
		{"invalid subdepartment", WorkPlan{DoctorID: 1, Date: "2026-09-09", Maximum: 1}, ErrInvalidSubdept},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.plan.ValidateNew(now); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestScheduleSlotValidateAndRemaining(t *testing.T) {
	slot := ScheduleSlot{WorkPlanID: 1, Slot: 1, Maximum: 3, Used: 1}
	if err := slot.ValidateNew(); err != nil {
		t.Fatal(err)
	}
	slot.Recalculate()
	if slot.Remaining != 2 {
		t.Fatalf("remaining = %d, want 2", slot.Remaining)
	}
	if err := (ScheduleSlot{WorkPlanID: 1, Slot: 0, Maximum: 1}).ValidateNew(); !errors.Is(err, ErrInvalidSlot) {
		t.Fatalf("got %v, want invalid slot", err)
	}
	if err := (ScheduleSlot{Slot: 1, Maximum: 1}).ValidateNew(); !errors.Is(err, ErrInvalidWorkPlan) {
		t.Fatalf("got %v, want invalid work plan", err)
	}
}

func TestCanModifyLocksStartedPlanButAllowsUsedFutureSlot(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	plan := WorkPlan{Date: "2026-09-10"}
	if !plan.CanModify(now) {
		t.Fatal("future plan should be editable")
	}
	plan.Date = "2026-09-09"
	if plan.CanModify(now) {
		t.Fatal("same-day plan should be locked")
	}
	plan.Date = "2026-09-10"
	if !(ScheduleSlot{Used: 1}).CanModify(plan, now) {
		t.Fatal("used slot on a future plan should be editable when capacity stays valid")
	}
}

func TestUpdateMaximumValidatesCapacityAndDate(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	plan := WorkPlan{Date: "2026-09-10", Maximum: 45, Used: 3}
	if err := plan.UpdateMaximum(60, now); err != nil {
		t.Fatal(err)
	}
	if plan.Maximum != 60 || plan.Remaining != 57 {
		t.Fatalf("plan = %#v, want maximum 60 and remaining 57", plan)
	}
	if err := plan.UpdateMaximum(2, now); !errors.Is(err, ErrMaximumBelowUsed) {
		t.Fatalf("got %v, want maximum below used", err)
	}
	plan.Date = "2026-09-09"
	if err := plan.UpdateMaximum(70, now); !errors.Is(err, ErrPlanLocked) {
		t.Fatalf("got %v, want plan locked", err)
	}

	slotPlan := WorkPlan{Date: "2026-09-10"}
	slot := ScheduleSlot{Slot: 1, Maximum: 3, Used: 1}
	if err := slot.UpdateMaximum(5, slotPlan, now); err != nil {
		t.Fatal(err)
	}
	if slot.Maximum != 5 || slot.Remaining != 4 {
		t.Fatalf("slot = %#v, want maximum 5 and remaining 4", slot)
	}
	if err := slot.UpdateMaximum(0, slotPlan, now); !errors.Is(err, ErrInvalidMaximum) {
		t.Fatalf("got %v, want invalid maximum", err)
	}
	slotPlan.Date = "2026-09-09"
	if err := slot.UpdateMaximum(6, slotPlan, now); !errors.Is(err, ErrSlotLocked) {
		t.Fatalf("got %v, want slot locked", err)
	}
}

func TestValidateDeleteProtectsRegistrationsAndStartedPlans(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	if err := (WorkPlan{Date: "2026-09-10"}).ValidateDelete(now); err != nil {
		t.Fatalf("unused future plan should be deletable: %v", err)
	}
	if err := (WorkPlan{Date: "2026-09-10", Used: 1}).ValidateDelete(now); !errors.Is(err, ErrHasRegistrations) {
		t.Fatalf("got %v, want has registrations", err)
	}
	if err := (WorkPlan{Date: "2026-09-09"}).ValidateDelete(now); !errors.Is(err, ErrPlanLocked) {
		t.Fatalf("got %v, want plan locked", err)
	}

	plan := WorkPlan{Date: "2026-09-10"}
	if err := (ScheduleSlot{}).ValidateDelete(plan, now); err != nil {
		t.Fatalf("unused future slot should be deletable: %v", err)
	}
	if err := (ScheduleSlot{Used: 1}).ValidateDelete(plan, now); !errors.Is(err, ErrHasRegistrations) {
		t.Fatalf("got %v, want has registrations", err)
	}
	plan.Date = "2026-09-09"
	if err := (ScheduleSlot{}).ValidateDelete(plan, now); !errors.Is(err, ErrSlotLocked) {
		t.Fatalf("got %v, want slot locked", err)
	}
}
