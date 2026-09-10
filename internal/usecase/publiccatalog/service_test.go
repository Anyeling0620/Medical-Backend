package publiccatalog

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/domain/schedule"
)

// 本文件覆盖匿名公开查询域用例层（internal/usecase/publiccatalog）：
// 必填过滤校验、缺省/显式日期窗口、不存在语义（404）、分页透传与空结果切片。
// 全部使用内存 fake 实现 port 接口，不连接真实数据库。

// publicCatalogRepoFake 是 port.PublicCatalogRepository 的内存 fake，
// 记录最近一次调用的过滤条件与分页参数，便于断言 use case 的透传行为。
type publicCatalogRepoFake struct {
	departments         []catalog.Department
	departmentsTotal    int64
	departmentsErr      error
	departmentDetail    *catalog.Department
	findDepartmentErr   error
	subdepartments      []catalog.Subdepartment
	subdepartmentsTotal int64
	subdepartmentsErr   error
	doctors             []catalog.PublicDoctor
	doctorsTotal        int64
	doctorsErr          error
	doctorDetail        *catalog.PublicDoctorDetail
	findDoctorErr       error

	lastDepartmentFilter catalog.DepartmentFilter
	lastDoctorFilter     catalog.PublicDoctorFilter
	lastDepartmentID     int64
	lastOffset           int
	lastLimit            int
}

// ListPublicDepartments 返回预置科室并记录过滤/分页参数。
func (f *publicCatalogRepoFake) ListPublicDepartments(_ context.Context, filter catalog.DepartmentFilter, offset, limit int) ([]catalog.Department, int64, error) {
	f.lastDepartmentFilter, f.lastOffset, f.lastLimit = filter, offset, limit
	return f.departments, f.departmentsTotal, f.departmentsErr
}

// FindPublicDepartment 返回预置科室详情。
func (f *publicCatalogRepoFake) FindPublicDepartment(_ context.Context, id int64) (*catalog.Department, error) {
	f.lastDepartmentID = id
	return f.departmentDetail, f.findDepartmentErr
}

// ListPublicSubdepartments 返回预置子科室并记录科室编号与分页参数。
func (f *publicCatalogRepoFake) ListPublicSubdepartments(_ context.Context, departmentID int64, offset, limit int) ([]catalog.Subdepartment, int64, error) {
	f.lastDepartmentID, f.lastOffset, f.lastLimit = departmentID, offset, limit
	return f.subdepartments, f.subdepartmentsTotal, f.subdepartmentsErr
}

// ListPublicDoctors 返回预置公开医生并记录过滤/分页参数。
func (f *publicCatalogRepoFake) ListPublicDoctors(_ context.Context, filter catalog.PublicDoctorFilter, offset, limit int) ([]catalog.PublicDoctor, int64, error) {
	f.lastDoctorFilter, f.lastOffset, f.lastLimit = filter, offset, limit
	return f.doctors, f.doctorsTotal, f.doctorsErr
}

// FindPublicDoctor 返回预置医生详情。
func (f *publicCatalogRepoFake) FindPublicDoctor(_ context.Context, id int64) (*catalog.PublicDoctorDetail, error) {
	f.lastDepartmentID = id
	return f.doctorDetail, f.findDoctorErr
}

// publicScheduleRepoFake 是 port.PublicScheduleRepository 的内存 fake。
type publicScheduleRepoFake struct {
	items      []schedule.PublicSchedule
	total      int64
	err        error
	lastFilter schedule.PublicScheduleFilter
	lastOffset int
	lastLimit  int
	callCount  int
}

// ListPublicSchedules 返回预置时段并记录过滤/分页参数与调用次数。
func (f *publicScheduleRepoFake) ListPublicSchedules(_ context.Context, filter schedule.PublicScheduleFilter, offset, limit int) ([]schedule.PublicSchedule, int64, error) {
	f.callCount++
	f.lastFilter, f.lastOffset, f.lastLimit = filter, offset, limit
	return f.items, f.total, f.err
}

// publicTestShanghai 是业务时区 Asia/Shanghai 的固定时区（不依赖运行环境的 tzdata）。
var publicTestShanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

// requireServiceError 断言错误是带指定 code 与 message 的业务错误（handler 据此映射 4xx）。
func requireServiceError(t *testing.T, err error, code, message string) {
	t.Helper()
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) {
		t.Fatalf("err = %v, want *ServiceError{code: %s}", err, code)
	}
	if serviceErr.Code != code {
		t.Errorf("code = %s, want %s", serviceErr.Code, code)
	}
	if serviceErr.Message != message {
		t.Errorf("message = %q, want %q", serviceErr.Message, message)
	}
}

// requirePlainError 断言错误是普通错误而非业务错误（handler 会按 500 处理）。
func requirePlainError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望返回 error，实际为 nil")
	}
	var serviceErr *ServiceError
	if errors.As(err, &serviceErr) {
		t.Fatalf("期望普通错误（走 500），实际为业务错误 %s: %s", serviceErr.Code, serviceErr.Message)
	}
}

// TestSchedulesRequiresSubdepartmentOrDoctor subdepartmentId 与 doctorId 都缺失时返回 422 业务错误，
// 且不得访问 repository（避免用默认查询掩盖参数错误）。
func TestSchedulesRequiresSubdepartmentOrDoctor(t *testing.T) {
	scheduleRepo := &publicScheduleRepoFake{}
	service := NewService(&publicCatalogRepoFake{}, scheduleRepo, func() time.Time {
		return time.Date(2026, 1, 1, 0, 30, 0, 0, publicTestShanghai)
	})

	_, _, err := service.Schedules(context.Background(), ScheduleQuery{
		FromDate: "2026-01-01",
		Page:     1,
		PageSize: 20,
	})
	requireServiceError(t, err, CodeValidationFailed, "必须提供 subdepartmentId 或 doctorId")
	if scheduleRepo.callCount != 0 {
		t.Errorf("校验失败时不应访问 repository，实际调用 %d 次", scheduleRepo.callCount)
	}
}

// TestSchedulesDefaultDateWindow 缺省日期窗口 = 业务当天 ~ 业务当天+6（共 7 天），
// 且业务当天按 Asia/Shanghai 归一：UTC 跨日时刻不能算成前一天。
func TestSchedulesDefaultDateWindow(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
	}{
		{"上海零点后", time.Date(2026, 1, 1, 0, 30, 0, 0, publicTestShanghai)},
		{"UTC 跨日（UTC 仍是 2025-12-31）", time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &publicScheduleRepoFake{}
			service := NewService(&publicCatalogRepoFake{}, repo, func() time.Time { return tc.now })
			subdepartmentID := int64(2)

			if _, _, err := service.Schedules(context.Background(), ScheduleQuery{
				SubdepartmentID: &subdepartmentID,
				Page:            1,
				PageSize:        20,
			}); err != nil {
				t.Fatalf("Schedules 返回错误：%v", err)
			}
			if repo.lastFilter.FromDate != "2026-01-01" || repo.lastFilter.ToDate != "2026-01-07" {
				t.Errorf("缺省窗口 = %s..%s, want 2026-01-01..2026-01-07（业务当天起 7 天）",
					repo.lastFilter.FromDate, repo.lastFilter.ToDate)
			}
		})
	}
}

// TestSchedulesOnlyFromDateKeepsSevenDayWindow 只给 fromDate 时窗口按同一长度（7 天）顺延。
func TestSchedulesOnlyFromDateKeepsSevenDayWindow(t *testing.T) {
	repo := &publicScheduleRepoFake{}
	service := NewService(&publicCatalogRepoFake{}, repo, func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, publicTestShanghai)
	})
	doctorID := int64(16)

	if _, _, err := service.Schedules(context.Background(), ScheduleQuery{
		DoctorID: &doctorID,
		FromDate: "2026-03-10",
		Page:     1,
		PageSize: 20,
	}); err != nil {
		t.Fatalf("Schedules 返回错误：%v", err)
	}
	if repo.lastFilter.FromDate != "2026-03-10" || repo.lastFilter.ToDate != "2026-03-16" {
		t.Errorf("窗口 = %s..%s, want 2026-03-10..2026-03-16", repo.lastFilter.FromDate, repo.lastFilter.ToDate)
	}
}

// TestSchedulesRangeValidation 覆盖日期跨度上限（含首尾 31 天）、顺序颠倒与非法日期格式。
func TestSchedulesRangeValidation(t *testing.T) {
	cases := []struct {
		name        string
		fromDate    string
		toDate      string
		wantMessage string
	}{
		{"跨度正好 31 天通过", "2026-09-01", "2026-10-01", ""},
		{"跨度 32 天返回 422", "2026-09-01", "2026-10-02", "日期范围无效"},
		{"toDate 早于 fromDate", "2026-09-05", "2026-09-01", "日期范围无效"},
		{"fromDate 非 YYYY-MM-DD", "2026-9-1", "", "日期范围无效"},
		{"toDate 非法日期", "", "2026-02-30", "日期范围无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &publicScheduleRepoFake{}
			service := NewService(&publicCatalogRepoFake{}, repo, func() time.Time {
				// 业务当天固定为 2026-01-01，保证缺省起点稳定。
				return time.Date(2026, 1, 1, 0, 0, 0, 0, publicTestShanghai)
			})
			doctorID := int64(16)

			_, _, err := service.Schedules(context.Background(), ScheduleQuery{
				DoctorID: &doctorID,
				FromDate: tc.fromDate,
				ToDate:   tc.toDate,
				Page:     1,
				PageSize: 20,
			})
			if tc.wantMessage == "" {
				if err != nil {
					t.Fatalf("期望通过，实际返回错误：%v", err)
				}
				if repo.lastFilter.FromDate != tc.fromDate || repo.lastFilter.ToDate != tc.toDate {
					t.Errorf("窗口 = %s..%s, want %s..%s",
						repo.lastFilter.FromDate, repo.lastFilter.ToDate, tc.fromDate, tc.toDate)
				}
				return
			}
			requireServiceError(t, err, CodeValidationFailed, tc.wantMessage)
		})
	}
}

// TestDepartmentNotFoundSemantics repository 用 sql.ErrNoRows 或 (nil, nil) 表示不存在时，
// 都转换为 404 CATALOG_DEPARTMENT_NOT_FOUND，不能泄漏成 500。
func TestDepartmentNotFoundSemantics(t *testing.T) {
	cases := []struct {
		name string
		item *catalog.Department
		err  error
	}{
		{"sql.ErrNoRows", nil, sql.ErrNoRows},
		{"nil,nil 视为不存在", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService(&publicCatalogRepoFake{departmentDetail: tc.item, findDepartmentErr: tc.err}, nil, nil)
			item, err := service.Department(context.Background(), 99999)
			if item != nil {
				t.Errorf("item = %+v, want nil", item)
			}
			requireServiceError(t, err, CodeDepartmentNotFound, "科室不存在")
		})
	}
}

// TestDoctorNotFoundSemantics 医生不存在（含 (nil, nil)）统一按 404 CATALOG_DOCTOR_NOT_FOUND 处理。
func TestDoctorNotFoundSemantics(t *testing.T) {
	cases := []struct {
		name string
		item *catalog.PublicDoctorDetail
		err  error
	}{
		{"sql.ErrNoRows", nil, sql.ErrNoRows},
		{"nil,nil 视为不存在", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService(&publicCatalogRepoFake{doctorDetail: tc.item, findDoctorErr: tc.err}, nil, nil)
			item, err := service.Doctor(context.Background(), 99999)
			if item != nil {
				t.Errorf("item = %+v, want nil", item)
			}
			requireServiceError(t, err, CodeDoctorNotFound, "医生不存在")
		})
	}
}

// TestSubdepartmentsDepartmentNotFound 目标科室不存在时返回 404 CATALOG_DEPARTMENT_NOT_FOUND，
// 不能用 200 + 空列表掩盖错误路径。
func TestSubdepartmentsDepartmentNotFound(t *testing.T) {
	service := NewService(&publicCatalogRepoFake{subdepartmentsErr: sql.ErrNoRows}, nil, nil)
	items, total, err := service.Subdepartments(context.Background(), 99999, 1, 20)
	if items != nil || total != 0 {
		t.Errorf("items = %v, total = %d, want nil, 0", items, total)
	}
	requireServiceError(t, err, CodeDepartmentNotFound, "科室不存在")
}

// TestSubdepartmentsPassesDepartmentID 子科室列表把科室编号与分页参数原样透传给 repository。
func TestSubdepartmentsPassesDepartmentID(t *testing.T) {
	repo := &publicCatalogRepoFake{}
	service := NewService(repo, nil, nil)

	items, total, err := service.Subdepartments(context.Background(), 7, 2, 20)
	if err != nil || total != 0 || len(items) != 0 {
		t.Fatalf("items=%v total=%d err=%v", items, total, err)
	}
	if repo.lastDepartmentID != 7 || repo.lastOffset != 20 || repo.lastLimit != 20 {
		t.Errorf("透传参数 = departmentID %d, offset %d, limit %d, want 7, 20, 20",
			repo.lastDepartmentID, repo.lastOffset, repo.lastLimit)
	}
}

// TestDoctorsPaginationOffset 页码换算：page=2、pageSize=20 → offset=20、limit=20。
func TestDoctorsPaginationOffset(t *testing.T) {
	cases := []struct {
		page     int
		pageSize int
		wantSkip int
	}{
		{1, 20, 0},
		{2, 20, 20},
		{5, 10, 40},
		{1, 100, 0},
	}
	for _, tc := range cases {
		repo := &publicCatalogRepoFake{}
		service := NewService(repo, nil, nil)

		if _, _, err := service.Doctors(context.Background(), catalog.PublicDoctorFilter{}, tc.page, tc.pageSize); err != nil {
			t.Fatalf("Doctors 返回错误：%v", err)
		}
		if repo.lastOffset != tc.wantSkip || repo.lastLimit != tc.pageSize {
			t.Errorf("page=%d pageSize=%d → offset=%d limit=%d, want offset=%d limit=%d",
				tc.page, tc.pageSize, repo.lastOffset, repo.lastLimit, tc.wantSkip, tc.pageSize)
		}
	}
}

// TestDepartmentsPaginationOffset 科室列表的分页与过滤条件同样原样透传。
func TestDepartmentsPaginationOffset(t *testing.T) {
	repo := &publicCatalogRepoFake{}
	service := NewService(repo, nil, nil)
	outpatient := true

	filter := catalog.DepartmentFilter{Outpatient: &outpatient, Sort: "id", Order: "desc"}
	if _, _, err := service.ListDepartments(context.Background(), filter, 3, 15); err != nil {
		t.Fatalf("ListDepartments 返回错误：%v", err)
	}
	if repo.lastOffset != 30 || repo.lastLimit != 15 {
		t.Errorf("offset=%d limit=%d, want 30, 15", repo.lastOffset, repo.lastLimit)
	}
	if repo.lastDepartmentFilter.Outpatient == nil || !*repo.lastDepartmentFilter.Outpatient ||
		repo.lastDepartmentFilter.Sort != "id" || repo.lastDepartmentFilter.Order != "desc" {
		t.Errorf("过滤条件透传错误：%+v", repo.lastDepartmentFilter)
	}
}

// TestSchedulesPaginationOffset 排班列表的分页与过滤条件原样透传（含必填过滤）。
func TestSchedulesPaginationOffset(t *testing.T) {
	repo := &publicScheduleRepoFake{}
	service := NewService(&publicCatalogRepoFake{}, repo, func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, publicTestShanghai)
	})
	doctorID := int64(16)

	if _, _, err := service.Schedules(context.Background(), ScheduleQuery{
		DoctorID: &doctorID,
		Page:     4,
		PageSize: 5,
	}); err != nil {
		t.Fatalf("Schedules 返回错误：%v", err)
	}
	if repo.lastOffset != 15 || repo.lastLimit != 5 {
		t.Errorf("offset=%d limit=%d, want 15, 5", repo.lastOffset, repo.lastLimit)
	}
	if repo.lastFilter.DoctorID == nil || *repo.lastFilter.DoctorID != 16 {
		t.Errorf("doctorId 透传错误：%v", repo.lastFilter.DoctorID)
	}
}

// TestEmptyResultsAreNonNil 空结果必须返回非 nil 空切片，保证响应输出 items:[] 而不是 null。
func TestEmptyResultsAreNonNil(t *testing.T) {
	catalogRepo := &publicCatalogRepoFake{}
	scheduleRepo := &publicScheduleRepoFake{}
	service := NewService(catalogRepo, scheduleRepo, func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, publicTestShanghai)
	})
	subdepartmentID := int64(2)

	departments, _, err := service.ListDepartments(context.Background(), catalog.DepartmentFilter{}, 1, 20)
	if err != nil {
		t.Fatalf("ListDepartments 返回错误：%v", err)
	}
	if departments == nil || len(departments) != 0 {
		t.Errorf("departments = %v, want 非 nil 空切片", departments)
	}

	subdepartments, _, err := service.Subdepartments(context.Background(), 1, 1, 20)
	if err != nil {
		t.Fatalf("Subdepartments 返回错误：%v", err)
	}
	if subdepartments == nil || len(subdepartments) != 0 {
		t.Errorf("subdepartments = %v, want 非 nil 空切片", subdepartments)
	}

	doctors, _, err := service.Doctors(context.Background(), catalog.PublicDoctorFilter{}, 1, 20)
	if err != nil {
		t.Fatalf("Doctors 返回错误：%v", err)
	}
	if doctors == nil || len(doctors) != 0 {
		t.Errorf("doctors = %v, want 非 nil 空切片", doctors)
	}

	schedules, _, err := service.Schedules(context.Background(), ScheduleQuery{
		SubdepartmentID: &subdepartmentID,
		Page:            1,
		PageSize:        20,
	})
	if err != nil {
		t.Fatalf("Schedules 返回错误：%v", err)
	}
	if schedules == nil || len(schedules) != 0 {
		t.Errorf("schedules = %v, want 非 nil 空切片", schedules)
	}
}

// TestDoctorDetailFillsEmptySubdepartmentsAndPrices 详情内嵌集合为空时输出空切片而不是 nil。
func TestDoctorDetailFillsEmptySubdepartmentsAndPrices(t *testing.T) {
	repo := &publicCatalogRepoFake{
		departmentDetail: nil,
		doctorDetail: &catalog.PublicDoctorDetail{
			PublicDoctor: catalog.PublicDoctor{ID: 16, Name: "熊佳钰"},
		},
	}
	service := NewService(repo, nil, nil)

	detail, err := service.Doctor(context.Background(), 16)
	if err != nil {
		t.Fatalf("Doctor 返回错误：%v", err)
	}
	if repo.lastDepartmentID != 16 {
		t.Errorf("医生编号透传 = %d, want 16", repo.lastDepartmentID)
	}
	if detail.Subdepartments == nil || detail.Prices == nil {
		t.Errorf("Subdepartments=%v Prices=%v, want 非 nil 空切片", detail.Subdepartments, detail.Prices)
	}
}

// TestNilRepositoriesReturnPlainError 仓库未配置时返回普通 error（handler 走 500），
// 而不是业务错误码（不能被误映射成 4xx）。
func TestNilRepositoriesReturnPlainError(t *testing.T) {
	service := NewService(nil, nil, nil)
	ctx := context.Background()

	if _, _, err := service.ListDepartments(ctx, catalog.DepartmentFilter{}, 1, 20); err == nil {
		t.Errorf("ListDepartments 应返回 error")
	} else {
		requirePlainError(t, err)
	}
	if _, err := service.Department(ctx, 1); err == nil {
		t.Errorf("Department 应返回 error")
	} else {
		requirePlainError(t, err)
	}
	if _, _, err := service.Subdepartments(ctx, 1, 1, 20); err == nil {
		t.Errorf("Subdepartments 应返回 error")
	} else {
		requirePlainError(t, err)
	}
	if _, _, err := service.Doctors(ctx, catalog.PublicDoctorFilter{}, 1, 20); err == nil {
		t.Errorf("Doctors 应返回 error")
	} else {
		requirePlainError(t, err)
	}
	if _, err := service.Doctor(ctx, 1); err == nil {
		t.Errorf("Doctor 应返回 error")
	} else {
		requirePlainError(t, err)
	}

	subdepartmentID := int64(2)
	if _, _, err := service.Schedules(ctx, ScheduleQuery{SubdepartmentID: &subdepartmentID, Page: 1, PageSize: 20}); err == nil {
		t.Errorf("Schedules 应返回 error")
	} else {
		requirePlainError(t, err)
	}
}

// TestRepositoryErrorsPropagate 普通仓储错误原样向上传播（handler 统一 500，不泄漏细节）。
func TestRepositoryErrorsPropagate(t *testing.T) {
	boom := errors.New("db down")
	service := NewService(
		&publicCatalogRepoFake{departmentsErr: boom, doctorsErr: boom, subdepartmentsErr: boom},
		&publicScheduleRepoFake{err: boom},
		func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, publicTestShanghai) },
	)
	ctx := context.Background()

	if _, _, err := service.ListDepartments(ctx, catalog.DepartmentFilter{}, 1, 20); !errors.Is(err, boom) {
		t.Errorf("ListDepartments err = %v, want %v", err, boom)
	}
	if _, _, err := service.Doctors(ctx, catalog.PublicDoctorFilter{}, 1, 20); !errors.Is(err, boom) {
		t.Errorf("Doctors err = %v, want %v", err, boom)
	}
	if _, _, err := service.Subdepartments(ctx, 1, 1, 20); !errors.Is(err, boom) {
		t.Errorf("Subdepartments err = %v, want %v", err, boom)
	}
	subdepartmentID := int64(2)
	if _, _, err := service.Schedules(ctx, ScheduleQuery{SubdepartmentID: &subdepartmentID, Page: 1, PageSize: 20}); !errors.Is(err, boom) {
		t.Errorf("Schedules err = %v, want %v", err, boom)
	}
}
