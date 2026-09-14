package schedule

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
)

// 本文件覆盖排班写路径 → 公开排班读缓存的失效契约（T4b），不连数据库、不连 Redis：
//
//	验收点 i：对 6 个写方法做表驱动断言「写成功后必定发生至少一次 BumpSchedules」。
//	          意图是横切约束——将来有人新增写路径（或改动现有写路径）却忘记失效时，
//	          本文件会立刻失败，而不是等生产环境出现「前端显示可挂、下单拿 409」。
//	间接覆盖 d：CreatePlan 的失效作用域必须是新建计划的「子科室 + 医生」，
//	          这正是「空结果缓存不会挡住新增排班」在用例层的保证（缓存层见
//	          internal/repo/cache_public_schedule_test.go 的空结果用例）。

// versionScope 是一次 BumpSchedules 调用的作用域；0 表示该维度未知或不适用。
type versionScope struct {
	subdepartmentID int64
	doctorID        int64
}

// recordingScheduleVersioner 是 port.ScheduleCacheVersioner 的记录桩：
// 记录每次失效调用的作用域，并可注入错误，用于验证「失效失败只记日志、不让写操作失败」。
type recordingScheduleVersioner struct {
	mu    sync.Mutex
	calls []versionScope
	err   error
}

func (v *recordingScheduleVersioner) BumpSchedules(_ context.Context, subdepartmentID, doctorID int64) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, versionScope{subdepartmentID: subdepartmentID, doctorID: doctorID})
	return v.err
}

func (v *recordingScheduleVersioner) recorded() []versionScope {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]versionScope(nil), v.calls...)
}

// 固定夹具：计划固定落在「子科室 2 + 医生 16」，日期取远期以保证 CanModify 为真
// （Service 注入的 now 为 fixedUTCNow()，见 service_test.go）。
const (
	stubSubdepartmentID = int64(2)
	stubDoctorID        = int64(16)
	stubPlanID          = int64(4)
	stubSlotID          = int64(12)
	stubFuturePlanDate  = "2026-09-20"
)

// stubWriteScheduleRepo 是「写方法全部成功」的 port.ScheduleRepository 桩：
// 事务方法直接作用在自身，便于表驱动断言每个写方法成功提交后都会触发缓存失效。
// 可按需注入失败（findPlanErr / hasPlan），用于验证失败路径不失效。
type stubWriteScheduleRepo struct {
	findPlanErr error
	hasPlan     bool

	findPlanCalls int
	writeCalls    []string
}

var _ port.ScheduleRepository = (*stubWriteScheduleRepo)(nil)

func (s *stubWriteScheduleRepo) plan(planID int64) *schedule.WorkPlan {
	return &schedule.WorkPlan{
		ID:              planID,
		DoctorID:        stubDoctorID,
		SubdepartmentID: stubSubdepartmentID,
		Date:            stubFuturePlanDate,
		Maximum:         10,
	}
}

func (s *stubWriteScheduleRepo) ListPlans(context.Context, schedule.PlanFilter, int, int) ([]schedule.WorkPlan, int64, error) {
	return []schedule.WorkPlan{}, 0, nil
}

func (s *stubWriteScheduleRepo) FindPlan(_ context.Context, planID int64) (*schedule.WorkPlan, error) {
	s.findPlanCalls++
	if s.findPlanErr != nil {
		return nil, s.findPlanErr
	}
	return s.plan(planID), nil
}

func (s *stubWriteScheduleRepo) ExecTx(_ context.Context, fn func(tx port.ScheduleTx) error) error {
	return fn(s)
}

func (s *stubWriteScheduleRepo) TxLockPlanCreate(context.Context, int64, int64, string) error {
	return nil
}

func (s *stubWriteScheduleRepo) TxDoctorAssociation(context.Context, int64, int64) (bool, bool, bool, error) {
	return true, true, true, nil
}

func (s *stubWriteScheduleRepo) TxHasPlan(context.Context, int64, int64, string) (bool, error) {
	return s.hasPlan, nil
}

func (s *stubWriteScheduleRepo) TxInsertPlan(context.Context, schedule.WorkPlan) (int64, error) {
	s.writeCalls = append(s.writeCalls, "TxInsertPlan")
	return 99, nil
}

func (s *stubWriteScheduleRepo) TxFindPlanLocked(_ context.Context, planID int64) (*schedule.WorkPlan, error) {
	if s.findPlanErr != nil {
		return nil, s.findPlanErr
	}
	return s.plan(planID), nil
}

func (s *stubWriteScheduleRepo) TxUpdatePlanMaximum(context.Context, int64, int16) error {
	s.writeCalls = append(s.writeCalls, "TxUpdatePlanMaximum")
	return nil
}

func (s *stubWriteScheduleRepo) TxHasRegistrations(context.Context, int64) (bool, error) {
	return false, nil
}

func (s *stubWriteScheduleRepo) TxDeletePlan(context.Context, int64) error {
	s.writeCalls = append(s.writeCalls, "TxDeletePlan")
	return nil
}

func (s *stubWriteScheduleRepo) ListSlotsByPlan(context.Context, int64) ([]schedule.ScheduleSlot, error) {
	return []schedule.ScheduleSlot{}, nil
}

func (s *stubWriteScheduleRepo) CreateSlot(_ context.Context, slot schedule.ScheduleSlot, _ time.Time) (*schedule.ScheduleSlot, error) {
	s.writeCalls = append(s.writeCalls, "CreateSlot")
	return &schedule.ScheduleSlot{ID: 101, WorkPlanID: slot.WorkPlanID, Slot: slot.Slot, Maximum: slot.Maximum}, nil
}

func (s *stubWriteScheduleRepo) UpdateSlotMaximum(_ context.Context, slotID int64, maximum int16, _ time.Time) (*schedule.ScheduleSlot, error) {
	s.writeCalls = append(s.writeCalls, "UpdateSlotMaximum")
	return &schedule.ScheduleSlot{ID: slotID, WorkPlanID: stubPlanID, Maximum: maximum}, nil
}

func (s *stubWriteScheduleRepo) DeleteSlot(context.Context, int64, time.Time) error {
	s.writeCalls = append(s.writeCalls, "DeleteSlot")
	return nil
}

// TestScheduleWritePathsAlwaysBumpCacheVersion 覆盖验收点 i：
// 用表驱动对 6 个写方法断言「成功提交后必定发生至少一次 BumpSchedules」。
//
// 这是给未来改动设的横切护栏：任何人新增写路径却忘记失效，或把某个写路径改成
// 提前返回，本用例都会失败——比生产环境出现「前端显示可挂、下单拿 409」更早暴露问题。
// DeleteSlot 在删除后已读不到「时段 -> 计划」映射，因此契约要求传 (0,0) 全局失效。
func TestScheduleWritePathsAlwaysBumpCacheVersion(t *testing.T) {
	wantPlanScope := versionScope{subdepartmentID: stubSubdepartmentID, doctorID: stubDoctorID}
	cases := []struct {
		name      string
		write     string
		call      func(t *testing.T, svc *Service) error
		wantScope versionScope
	}{
		{
			name:  "CreatePlan 新增计划",
			write: "CreatePlan",
			call: func(t *testing.T, svc *Service) error {
				t.Helper()
				_, err := svc.CreatePlan(context.Background(), schedule.WorkPlan{
					DoctorID:        stubDoctorID,
					SubdepartmentID: stubSubdepartmentID,
					Date:            stubFuturePlanDate,
					Maximum:         10,
				})
				return err
			},
			wantScope: wantPlanScope,
		},
		{
			name:  "UpdatePlan 修改计划容量",
			write: "UpdatePlan",
			call: func(t *testing.T, svc *Service) error {
				t.Helper()
				_, err := svc.UpdatePlan(context.Background(), stubPlanID, 10)
				return err
			},
			wantScope: wantPlanScope,
		},
		{
			name:  "DeletePlan 删除计划",
			write: "DeletePlan",
			call: func(t *testing.T, svc *Service) error {
				t.Helper()
				return svc.DeletePlan(context.Background(), stubPlanID)
			},
			wantScope: wantPlanScope,
		},
		{
			name:  "CreateSlot 新增时段",
			write: "CreateSlot",
			call: func(t *testing.T, svc *Service) error {
				t.Helper()
				_, err := svc.CreateSlot(context.Background(), schedule.ScheduleSlot{
					WorkPlanID: stubPlanID,
					Slot:       1,
					Maximum:    3,
				})
				return err
			},
			wantScope: wantPlanScope,
		},
		{
			name:  "UpdateSlotMaximum 修改时段容量",
			write: "UpdateSlotMaximum",
			call: func(t *testing.T, svc *Service) error {
				t.Helper()
				_, err := svc.UpdateSlotMaximum(context.Background(), stubSlotID, 5)
				return err
			},
			wantScope: wantPlanScope,
		},
		{
			name:  "DeleteSlot 删除时段（无作用域，全局失效）",
			write: "DeleteSlot",
			call: func(t *testing.T, svc *Service) error {
				t.Helper()
				return svc.DeleteSlot(context.Background(), stubSlotID)
			},
			wantScope: versionScope{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubWriteScheduleRepo{}
			versioner := &recordingScheduleVersioner{}
			svc := NewService(repo, fixedUTCNow, WithScheduleCacheVersioner(versioner))

			if err := tc.call(t, svc); err != nil {
				t.Fatalf("%s 应当成功，实际报错: %v", tc.write, err)
			}
			// 先确认用例确实走到了写路径，避免断言变成「没调用任何东西也算通过」的假阳性。
			if len(repo.writeCalls) == 0 {
				t.Fatalf("%s 没有触达仓库写方法，用例本身失效", tc.write)
			}

			calls := versioner.recorded()
			if len(calls) == 0 {
				t.Fatalf("写路径 %s 成功提交后必须至少调用一次 BumpSchedules 失效公开排班缓存；"+
					"若是新增写路径，请在 usecase 层补上失效调用（T4b）", tc.write)
			}
			for i, call := range calls {
				if call != tc.wantScope {
					t.Errorf("第 %d 次失效作用域 = %+v, want %+v", i+1, call, tc.wantScope)
				}
			}
		})
	}
}

// TestCreatePlanInvalidatesNewPlanScope 覆盖验收点 d 在用例层的保证：
// CreatePlan 的失效作用域必须正好是新建计划的「子科室 + 医生」两个维度，
// 才能让此前缓存的「空结果」立即失效（缓存键里的两个版本号都会前进）。
func TestCreatePlanInvalidatesNewPlanScope(t *testing.T) {
	const (
		newSubdepartmentID = int64(7)
		newDoctorID        = int64(21)
	)
	repo := &stubWriteScheduleRepo{}
	versioner := &recordingScheduleVersioner{}
	svc := NewService(repo, fixedUTCNow, WithScheduleCacheVersioner(versioner))

	if _, err := svc.CreatePlan(context.Background(), schedule.WorkPlan{
		DoctorID:        newDoctorID,
		SubdepartmentID: newSubdepartmentID,
		Date:            stubFuturePlanDate,
		Maximum:         5,
	}); err != nil {
		t.Fatalf("CreatePlan 应当成功: %v", err)
	}

	calls := versioner.recorded()
	if len(calls) != 1 {
		t.Fatalf("CreatePlan 成功后应恰好失效一次，实际 %d 次: %+v", len(calls), calls)
	}
	want := versionScope{subdepartmentID: newSubdepartmentID, doctorID: newDoctorID}
	if calls[0] != want {
		t.Errorf("失效作用域 = %+v, want %+v（两个维度都要递增，否则只按医生查的需求会读到脏余量）", calls[0], want)
	}
}

// TestScheduleWriteFailureDoesNotInvalidateCache 失败路径不得触发失效：
// 既避免无谓回源，也证明上面的表驱动断言不是「任何调用都算通过」的假阳性。
func TestScheduleWriteFailureDoesNotInvalidateCache(t *testing.T) {
	t.Run("CreatePlan 重复计划", func(t *testing.T) {
		repo := &stubWriteScheduleRepo{hasPlan: true}
		versioner := &recordingScheduleVersioner{}
		svc := NewService(repo, fixedUTCNow, WithScheduleCacheVersioner(versioner))

		_, err := svc.CreatePlan(context.Background(), schedule.WorkPlan{
			DoctorID:        stubDoctorID,
			SubdepartmentID: stubSubdepartmentID,
			Date:            stubFuturePlanDate,
			Maximum:         10,
		})
		if err == nil {
			t.Fatalf("重复计划必须返回错误")
		}
		if calls := versioner.recorded(); len(calls) != 0 {
			t.Errorf("写操作失败时不得失效缓存，实际 %d 次: %+v", len(calls), calls)
		}
	})

	t.Run("DeletePlan 计划不存在", func(t *testing.T) {
		repo := &stubWriteScheduleRepo{findPlanErr: sql.ErrNoRows}
		versioner := &recordingScheduleVersioner{}
		svc := NewService(repo, fixedUTCNow, WithScheduleCacheVersioner(versioner))

		if err := svc.DeletePlan(context.Background(), stubPlanID); err == nil {
			t.Fatalf("计划不存在必须返回错误")
		}
		if calls := versioner.recorded(); len(calls) != 0 {
			t.Errorf("写操作失败时不得失效缓存，实际 %d 次: %+v", len(calls), calls)
		}
	})
}

// TestInvalidatePublicSchedulesByPlanFallsBackToGlobalScope 覆盖「只有 planId、拿不到作用域」的兜底：
// 计划读不到时（例如已被并发删除）必须退化为全局失效 (0,0)——宁可多失效一次回源，也不能漏失效。
func TestInvalidatePublicSchedulesByPlanFallsBackToGlobalScope(t *testing.T) {
	repo := &stubWriteScheduleRepo{findPlanErr: sql.ErrNoRows}
	versioner := &recordingScheduleVersioner{}
	svc := NewService(repo, fixedUTCNow, WithScheduleCacheVersioner(versioner))

	// CreateSlot 成功后按计划定位作用域，此时计划已读不到。
	if _, err := svc.CreateSlot(context.Background(), schedule.ScheduleSlot{
		WorkPlanID: stubPlanID,
		Slot:       1,
		Maximum:    3,
	}); err != nil {
		t.Fatalf("CreateSlot 应当成功: %v", err)
	}
	if repo.findPlanCalls != 1 {
		t.Errorf("按计划失效时必须读一次计划，实际 %d 次", repo.findPlanCalls)
	}
	calls := versioner.recorded()
	if len(calls) != 1 || calls[0] != (versionScope{}) {
		t.Fatalf("计划读不到时必须退化为全局失效 (0,0)，实际 %+v", calls)
	}
}

// TestScheduleCacheVersionerFailureDoesNotFailWrite 覆盖验收点 e 的写路径：
// 失效失败只记日志，写操作必须照常成功返回（缓存是性能依赖而不是正确性依赖）。
func TestScheduleCacheVersionerFailureDoesNotFailWrite(t *testing.T) {
	repo := &stubWriteScheduleRepo{}
	versioner := &recordingScheduleVersioner{err: errors.New("redis 不可用")}
	svc := NewService(repo, fixedUTCNow, WithScheduleCacheVersioner(versioner))

	result, err := svc.CreatePlan(context.Background(), schedule.WorkPlan{
		DoctorID:        stubDoctorID,
		SubdepartmentID: stubSubdepartmentID,
		Date:            stubFuturePlanDate,
		Maximum:         10,
	})
	if err != nil {
		t.Fatalf("失效失败不得让写操作失败: %v", err)
	}
	if result == nil || result.NewID != 99 {
		t.Fatalf("写操作必须返回成功结果，实际 %+v", result)
	}
	if len(versioner.recorded()) != 1 {
		t.Errorf("即使失效失败也必须尝试过一次失效")
	}
}

// TestScheduleWriteWithoutCacheVersionerDoesNotPanic 缓存未启用（versioner=nil）时写路径照常工作。
func TestScheduleWriteWithoutCacheVersionerDoesNotPanic(t *testing.T) {
	repo := &stubWriteScheduleRepo{}
	svc := NewService(repo, fixedUTCNow) // 不注入 WithScheduleCacheVersioner

	if _, err := svc.CreatePlan(context.Background(), schedule.WorkPlan{
		DoctorID:        stubDoctorID,
		SubdepartmentID: stubSubdepartmentID,
		Date:            stubFuturePlanDate,
		Maximum:         10,
	}); err != nil {
		t.Fatalf("未启用缓存时 CreatePlan 不应失败: %v", err)
	}
	if err := svc.DeleteSlot(context.Background(), stubSlotID); err != nil {
		t.Fatalf("未启用缓存时 DeleteSlot 不应失败: %v", err)
	}
}
