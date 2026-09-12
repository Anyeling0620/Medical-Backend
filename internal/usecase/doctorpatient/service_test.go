package doctorpatient

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	domaindoctorpatient "Medical-Web-Backend/internal/domain/doctorpatient"
	domainuser "Medical-Web-Backend/internal/domain/user"
	"Medical-Web-Backend/internal/port"
)

// 本文件覆盖「我的患者」用例（internal/usecase/doctorpatient/service.go）。
// 重点断言两件事：数据范围必须来自登录主体（mis_user.ref_id 推导出的 doctor.id），
// 以及未绑定医生的账号必须返回 ErrDoctorNotBound，而不是退化成空列表或全量数据。

// doctorPatientTestUserRepo 是 port.UserRepository 桩，只实现 FindByID：
// MyPatients 只用到该方法，其余方法保留内嵌 nil 接口，
// 一旦被意外调用就会 panic，从而暴露「用例越界读取了无关数据」的问题。
type doctorPatientTestUserRepo struct {
	port.UserRepository

	account *domainuser.User
	err     error
	lastID  int64
	calls   int
}

// FindByID 记录入参并返回预置账号或预置错误。
func (r *doctorPatientTestUserRepo) FindByID(
	_ context.Context,
	userID int64,
) (*domainuser.User, error) {
	r.calls++
	r.lastID = userID
	return r.account, r.err
}

// doctorPatientTestRepo 是 port.DoctorPatientRepository 桩：
// 返回预置患者并记录收到的过滤条件与分页参数，便于断言 doctorId 来自 ref_id。
type doctorPatientTestRepo struct {
	items      []domaindoctorpatient.Patient
	total      int64
	err        error
	calls      int
	lastFilter domaindoctorpatient.Filter
	lastOffset int
	lastLimit  int
}

// ListDoctorPatients 记录过滤条件与 offset/limit 后返回预置结果。
func (r *doctorPatientTestRepo) ListDoctorPatients(
	_ context.Context,
	filter domaindoctorpatient.Filter,
	offset, limit int,
) ([]domaindoctorpatient.Patient, int64, error) {
	r.calls++
	r.lastFilter, r.lastOffset, r.lastLimit = filter, offset, limit
	return r.items, r.total, r.err
}

// TestMyPatientsRejectsUnboundDoctor 覆盖 ref_id 缺失的各种形态：
// 主体编号非法、账号不存在、ref_id 为 nil/0/负数都必须返回 ErrDoctorNotBound，
// 且不得触达患者仓储——数据范围未确定时不允许发起查询。
func TestMyPatientsRejectsUnboundDoctor(t *testing.T) {
	zero := int64(0)
	negative := int64(-1)
	bound := int64(16)

	cases := []struct {
		name      string
		misUserID int64
		account   *domainuser.User
	}{
		{"主体编号为 0", 0, &domainuser.User{ID: 7, RefID: &bound}},
		{"主体编号为负数", -1, &domainuser.User{ID: 7, RefID: &bound}},
		{"账号不存在", 7, nil},
		{"ref_id 为 nil", 7, &domainuser.User{ID: 7, Username: "mis-admin"}},
		{"ref_id 为 0", 7, &domainuser.User{ID: 7, RefID: &zero}},
		{"ref_id 为负数", 7, &domainuser.User{ID: 7, RefID: &negative}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patientRepo := &doctorPatientTestRepo{}
			service := NewService(
				&doctorPatientTestUserRepo{account: tc.account},
				patientRepo,
			)

			page, err := service.MyPatients(
				context.Background(),
				tc.misUserID,
				Query{Page: 1, PageSize: 20},
			)
			if !errors.Is(err, ErrDoctorNotBound) {
				t.Fatalf("err = %v, want ErrDoctorNotBound", err)
			}
			if page != nil {
				t.Errorf("page = %+v, want nil（未绑定医生不得返回列表）", page)
			}
			if patientRepo.calls != 0 {
				t.Errorf("未绑定医生时不得查询患者仓储，实际调用 %d 次", patientRepo.calls)
			}
		})
	}
}

// TestMyPatientsUsesBoundDoctorAndOffset 覆盖绑定成功路径：
// doctorId 必须取自 mis_user.ref_id（而不是客户端入参），keyword/sort/order 原样透传，
// 分页按 (page-1)*pageSize 换算成 offset，响应回填 page/pageSize/total 与患者列表。
func TestMyPatientsUsesBoundDoctorAndOffset(t *testing.T) {
	doctorID := int64(16)
	userRepo := &doctorPatientTestUserRepo{
		account: &domainuser.User{ID: 7, Username: "mis-doctor", RefID: &doctorID},
	}
	patientRepo := &doctorPatientTestRepo{
		items: []domaindoctorpatient.Patient{
			{PatientCardID: 10, Name: "张三", RegistrationCount: 3},
			{PatientCardID: 11, Name: "李四", RegistrationCount: 1},
		},
		total: 42,
	}
	service := NewService(userRepo, patientRepo)

	page, err := service.MyPatients(context.Background(), 7, Query{
		Keyword:  "张",
		Sort:     domaindoctorpatient.SortName,
		Order:    "asc",
		Page:     3,
		PageSize: 15,
	})
	if err != nil {
		t.Fatalf("MyPatients 返回错误：%v", err)
	}
	if userRepo.lastID != 7 {
		t.Errorf("FindByID 入参 = %d, want 7（主体编号来自令牌）", userRepo.lastID)
	}
	if patientRepo.lastFilter.DoctorID != doctorID {
		t.Errorf("Filter.DoctorID = %d, want %d（必须取自 ref_id）",
			patientRepo.lastFilter.DoctorID, doctorID)
	}
	if patientRepo.lastFilter.Keyword != "张" ||
		patientRepo.lastFilter.Sort != domaindoctorpatient.SortName ||
		patientRepo.lastFilter.Order != "asc" {
		t.Errorf("查询条件透传错误：%+v", patientRepo.lastFilter)
	}
	if patientRepo.lastFilter.Page != 3 || patientRepo.lastFilter.PageSize != 15 {
		t.Errorf("Filter 分页 = %d/%d, want 3/15",
			patientRepo.lastFilter.Page, patientRepo.lastFilter.PageSize)
	}
	if patientRepo.lastOffset != 30 || patientRepo.lastLimit != 15 {
		t.Errorf("仓储 offset/limit = %d/%d, want 30/15",
			patientRepo.lastOffset, patientRepo.lastLimit)
	}
	if page == nil {
		t.Fatal("page = nil, want 分页结果")
	}
	if page.Page != 3 || page.PageSize != 15 || page.Total != 42 {
		t.Errorf("分页响应 = page:%d pageSize:%d total:%d, want 3/15/42",
			page.Page, page.PageSize, page.Total)
	}
	if len(page.Items) != 2 ||
		page.Items[0].PatientCardID != 10 || page.Items[1].PatientCardID != 11 {
		t.Errorf("患者列表 = %+v, want 两条预置数据", page.Items)
	}
}

// TestMyPatientsPropagatesErrors 覆盖错误传播分支：
// 账号读取失败与患者仓储失败都必须保留原始错误（errors.Is 可判别），
// 且都不得被误映射成 ErrDoctorNotBound（否则 handler 会把 500 错报成 403）。
func TestMyPatientsPropagatesErrors(t *testing.T) {
	boom := errors.New("db down")
	doctorID := int64(16)

	t.Run("账号读取失败", func(t *testing.T) {
		patientRepo := &doctorPatientTestRepo{}
		service := NewService(&doctorPatientTestUserRepo{err: boom}, patientRepo)

		_, err := service.MyPatients(context.Background(), 7, Query{Page: 1, PageSize: 20})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
		if errors.Is(err, ErrDoctorNotBound) {
			t.Error("账号读取失败不得被当成未绑定医生（会把 500 错报成 403）")
		}
		if patientRepo.calls != 0 {
			t.Errorf("账号读取失败时不得查询患者仓储，实际调用 %d 次", patientRepo.calls)
		}
	})

	t.Run("患者仓储失败", func(t *testing.T) {
		service := NewService(
			&doctorPatientTestUserRepo{
				account: &domainuser.User{ID: 7, RefID: &doctorID},
			},
			&doctorPatientTestRepo{err: boom},
		)

		_, err := service.MyPatients(context.Background(), 7, Query{Page: 1, PageSize: 20})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
		if errors.Is(err, ErrDoctorNotBound) {
			t.Error("仓储失败不得被当成未绑定医生")
		}
	})

	t.Run("依赖未配置", func(t *testing.T) {
		service := NewService(nil, nil)
		if _, err := service.MyPatients(context.Background(), 7, Query{Page: 1, PageSize: 20}); err == nil {
			t.Fatal("依赖未配置时应返回错误")
		} else if errors.Is(err, ErrDoctorNotBound) {
			t.Error("依赖未配置不得映射成 ErrDoctorNotBound（应为普通错误走 500）")
		}

		var nilService *Service
		if _, err := nilService.MyPatients(context.Background(), 7, Query{Page: 1, PageSize: 20}); err == nil {
			t.Error("nil Service 应返回错误而不是 panic")
		}
	})
}

// TestMyPatientsClassifiesDependencyErrors 覆盖依赖失败的错误归类：
// 账号读取失败与患者仓储失败都必须能被 errors.Is(err, ErrDependencyUnavailable) 判别
// （handler → 502 DEPENDENCY_UNAVAILABLE），同时用第二个 %w 保留根因链，
// 且绝不能被误判为 ErrDoctorNotBound（否则 handler 会把 502 错报成 403）。
func TestMyPatientsClassifiesDependencyErrors(t *testing.T) {
	boom := errors.New("db down")
	doctorID := int64(16)
	boundAccount := &domainuser.User{ID: 7, Username: "mis-doctor", RefID: &doctorID}

	cases := []struct {
		name        string
		userRepo    *doctorPatientTestUserRepo
		patientRepo *doctorPatientTestRepo
		cause       error
	}{
		{
			name:        "账号读取失败",
			userRepo:    &doctorPatientTestUserRepo{err: boom},
			patientRepo: &doctorPatientTestRepo{},
			cause:       boom,
		},
		{
			name: "账号读取失败且根因已被包装",
			userRepo: &doctorPatientTestUserRepo{
				err: fmt.Errorf("查询 mis_user 失败: %w", boom),
			},
			patientRepo: &doctorPatientTestRepo{},
			cause:       boom,
		},
		{
			name:        "患者仓储查询失败",
			userRepo:    &doctorPatientTestUserRepo{account: boundAccount},
			patientRepo: &doctorPatientTestRepo{err: boom},
			cause:       boom,
		},
		{
			name:        "患者仓储连接已断开",
			userRepo:    &doctorPatientTestUserRepo{account: boundAccount},
			patientRepo: &doctorPatientTestRepo{err: sql.ErrConnDone},
			cause:       sql.ErrConnDone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService(tc.userRepo, tc.patientRepo)
			page, err := service.MyPatients(
				context.Background(), 7, Query{Page: 1, PageSize: 20})
			if err == nil {
				t.Fatal("依赖失败时必须返回错误")
			}
			if !errors.Is(err, ErrDependencyUnavailable) {
				t.Errorf("err = %v, want errors.Is(err, ErrDependencyUnavailable)（应映射 502）", err)
			}
			if !errors.Is(err, tc.cause) {
				t.Errorf("err = %v, want 保留根因 %v（错误链不得断裂）", err, tc.cause)
			}
			if errors.Is(err, ErrDoctorNotBound) {
				t.Error("依赖失败不得被误判为 ErrDoctorNotBound（会把 502 错报成 403）")
			}
			if page != nil {
				t.Errorf("page = %+v, want nil", page)
			}
		})
	}
}

// TestMyPatientsFallsBackInvalidPagination 覆盖分页兜底：
// 调用方绕过请求层传入 0 或负数时必须按 page=1、pageSize=20 处理，
// 响应字段、Filter 与传给仓储的 offset/limit 都要用兜底后的值，
// 不能让 SQL 的 LIMIT 0 把「参数非法」静默表现成「无数据」。
func TestMyPatientsFallsBackInvalidPagination(t *testing.T) {
	doctorID := int64(16)

	cases := []struct {
		name     string
		page     int
		pageSize int
	}{
		{"page 为 0", 0, 10},
		{"page 为负数", -5, 10},
		{"pageSize 为 0", 1, 0},
		{"pageSize 为负数", 1, -3},
		{"page 合法但 pageSize 为 0（offset 必须按兜底页长换算）", 3, 0},
		{"page 与 pageSize 都为 0", 0, 0},
		{"page 与 pageSize 都为负数", -2, -2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantPage, wantPageSize := tc.page, tc.pageSize
			if wantPage < 1 {
				wantPage = 1
			}
			if wantPageSize < 1 {
				wantPageSize = 20
			}

			patientRepo := &doctorPatientTestRepo{total: 3}
			service := NewService(
				&doctorPatientTestUserRepo{
					account: &domainuser.User{ID: 7, RefID: &doctorID},
				},
				patientRepo,
			)

			page, err := service.MyPatients(context.Background(), 7, Query{
				Page:     tc.page,
				PageSize: tc.pageSize,
			})
			if err != nil {
				t.Fatalf("分页非法应兜底而不是报错：%v", err)
			}
			if patientRepo.lastOffset != (wantPage-1)*wantPageSize ||
				patientRepo.lastLimit != wantPageSize {
				t.Errorf("仓储 offset/limit = %d/%d, want %d/%d",
					patientRepo.lastOffset, patientRepo.lastLimit,
					(wantPage-1)*wantPageSize, wantPageSize)
			}
			if patientRepo.lastFilter.Page != wantPage ||
				patientRepo.lastFilter.PageSize != wantPageSize {
				t.Errorf("Filter 分页 = %d/%d, want %d/%d",
					patientRepo.lastFilter.Page, patientRepo.lastFilter.PageSize,
					wantPage, wantPageSize)
			}
			if page == nil {
				t.Fatal("page = nil, want 分页结果")
			}
			if page.Page != wantPage || page.PageSize != wantPageSize {
				t.Errorf("响应分页 = %d/%d, want %d/%d",
					page.Page, page.PageSize, wantPage, wantPageSize)
			}
		})
	}
}
