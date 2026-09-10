// Package publiccatalog 实现匿名公开查询域（/api/v1/public/*）的读取用例：
// 科室、子科室、医生与可挂号时段。
//
// 该域是匿名只读域（契约 §1.2）：不读取也不要求令牌，不写库，不产生幂等记录；
// 参数校验（分页、日期范围、必填过滤）与「资源是否存在」的语义都收敛在本层，
// repository 只负责按条件取数。
package publiccatalog

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
)

// 业务错误码：与 spec/04-api-contract.md 第 10 节错误码目录一致，handler 依据 code 映射 HTTP 状态码。
const (
	CodeValidationFailed   = "REQUEST_VALIDATION_FAILED"
	CodeDepartmentNotFound = "CATALOG_DEPARTMENT_NOT_FOUND"
	// CodeSubdepartmentNotFound 属公开域错误码目录，但子科室详情路由（§8 的第 7 条）
	// 不在本次交付范围内，因此当前没有用例返回它；补齐该路由时直接复用本常量。
	CodeSubdepartmentNotFound = "CATALOG_SUBDEPARTMENT_NOT_FOUND"
	CodeDoctorNotFound        = "CATALOG_DOCTOR_NOT_FOUND"
)

// MaxScheduleRangeDays 是公开排班查询允许的日期跨度上限（含首尾，契约 §8.1）。
const MaxScheduleRangeDays = 31

// defaultScheduleRangeDays 是缺省日期窗口长度：业务当天起 7 天（含当天，契约 §8.1）。
const defaultScheduleRangeDays = 6

// Details 是可选的错误细节（仅非敏感字段），保留给后续需要补充上下文的错误。
type Details map[string]any

// ServiceError 是 use case 返回给 handler 的业务错误：稳定 code + 面向用户的 message。
type ServiceError struct {
	Code    string
	Message string
	Details Details
}

func (e *ServiceError) Error() string { return e.Message }

// Service 编排公开域的读取用例；now 可注入以便测试缺省日期窗口。
type Service struct {
	catalog  port.PublicCatalogRepository
	schedule port.PublicScheduleRepository
	now      func() time.Time
}

// NewService 构造公开域服务；now 为空时回退到当前时间。
func NewService(catalogRepository port.PublicCatalogRepository, scheduleRepository port.PublicScheduleRepository, now func() time.Time) *Service {
	if now == nil {
		now = func() time.Time { return time.Now() }
	}
	return &Service{catalog: catalogRepository, schedule: scheduleRepository, now: now}
}

// ListDepartments 分页返回科室列表；空结果返回空切片以保证输出 items:[]。
func (s *Service) ListDepartments(ctx context.Context, f catalog.DepartmentFilter, page, pageSize int) ([]catalog.Department, int64, error) {
	if s.catalog == nil {
		return nil, 0, errors.New("目录仓库未配置")
	}
	items, total, err := s.catalog.ListPublicDepartments(ctx, f, offset(page, pageSize), pageSize)
	if items == nil {
		items = make([]catalog.Department, 0)
	}
	return items, total, err
}

// Department 返回科室详情；不存在返回 404 CATALOG_DEPARTMENT_NOT_FOUND（契约 §12.7）。
func (s *Service) Department(ctx context.Context, id int64) (*catalog.Department, error) {
	if s.catalog == nil {
		return nil, errors.New("目录仓库未配置")
	}
	item, err := s.catalog.FindPublicDepartment(ctx, id)
	// repository 以 sql.ErrNoRows 表示「不存在」，这里转换成语义错误码，
	// 避免把数据库层错误直接泄漏成 500（契约 §10 错误码目录）。
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &ServiceError{Code: CodeDepartmentNotFound, Message: "科室不存在"}
	}
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, &ServiceError{Code: CodeDepartmentNotFound, Message: "科室不存在"}
	}
	return item, nil
}

// Subdepartments 分页返回某科室下的子科室；科室不存在同样返回 404，避免用空列表掩盖错误路径。
func (s *Service) Subdepartments(ctx context.Context, departmentID int64, page, pageSize int) ([]catalog.Subdepartment, int64, error) {
	if s.catalog == nil {
		return nil, 0, errors.New("目录仓库未配置")
	}
	items, total, err := s.catalog.ListPublicSubdepartments(ctx, departmentID, offset(page, pageSize), pageSize)
	// 科室不存在同样按 404 处理：不能返回 200 + 空列表把错误路径藏起来（契约 §12.7）。
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, &ServiceError{Code: CodeDepartmentNotFound, Message: "科室不存在"}
	}
	if items == nil {
		items = make([]catalog.Subdepartment, 0)
	}
	return items, total, err
}

// Doctors 分页返回公开医生列表（固定只含在岗且非隐藏医生）。
func (s *Service) Doctors(ctx context.Context, f catalog.PublicDoctorFilter, page, pageSize int) ([]catalog.PublicDoctor, int64, error) {
	if s.catalog == nil {
		return nil, 0, errors.New("目录仓库未配置")
	}
	items, total, err := s.catalog.ListPublicDoctors(ctx, f, offset(page, pageSize), pageSize)
	if items == nil {
		items = make([]catalog.PublicDoctor, 0)
	}
	return items, total, err
}

// Doctor 返回医生详情；不存在、非在岗或隐藏统一按 404 CATALOG_DOCTOR_NOT_FOUND 处理。
func (s *Service) Doctor(ctx context.Context, id int64) (*catalog.PublicDoctorDetail, error) {
	if s.catalog == nil {
		return nil, errors.New("目录仓库未配置")
	}
	item, err := s.catalog.FindPublicDoctor(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &ServiceError{Code: CodeDoctorNotFound, Message: "医生不存在"}
	}
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, &ServiceError{Code: CodeDoctorNotFound, Message: "医生不存在"}
	}
	if item.Subdepartments == nil {
		item.Subdepartments = make([]catalog.PublicSubdepartmentRef, 0)
	}
	if item.Prices == nil {
		item.Prices = make([]catalog.PublicDoctorPrice, 0)
	}
	return item, nil
}

// ScheduleQuery 是公开排班查询的原始输入。
// SubdepartmentID 与 DoctorID 至少提供一个（同时提供取交集）；
// FromDate/ToDate 为空时由服务端按业务当天推导；Page/PageSize 已由绑定层校验过范围。
type ScheduleQuery struct {
	SubdepartmentID *int64
	DoctorID        *int64
	FromDate        string
	ToDate          string
	Page            int
	PageSize        int
}

// Schedules 返回可挂号时段（含满号时段）：前端需要展示 remaining=0 并置灰，不能因满号而隐藏。
func (s *Service) Schedules(ctx context.Context, q ScheduleQuery) ([]schedule.PublicSchedule, int64, error) {
	if s.schedule == nil {
		return nil, 0, errors.New("排班仓库未配置")
	}
	filter, err := q.resolve(s.now())
	if err != nil {
		return nil, 0, err
	}
	items, total, err := s.schedule.ListPublicSchedules(ctx, filter, offset(q.Page, q.PageSize), q.PageSize)
	if items == nil {
		items = make([]schedule.PublicSchedule, 0)
	}
	return items, total, err
}

// resolve 校验必填过滤并使日期窗口落地为闭区间 [fromDate, toDate]。
//
// 缺省规则（契约 §8.1）：fromDate 缺省为业务当前日期，toDate 缺省为当前日期 +6 天。
// 只给 fromDate 时窗口按同一长度（7 天）顺延，避免出现「只指定起始日期就必然 422」的死角。
func (q ScheduleQuery) resolve(now time.Time) (schedule.PublicScheduleFilter, error) {
	if q.SubdepartmentID == nil && q.DoctorID == nil {
		return schedule.PublicScheduleFilter{}, &ServiceError{
			Code:    CodeValidationFailed,
			Message: "必须提供 subdepartmentId 或 doctorId",
		}
	}
	today := schedule.BusinessToday(now)
	from := today
	if q.FromDate != "" {
		parsed, err := time.ParseInLocation(schedule.DateLayout, q.FromDate, time.UTC)
		if err != nil {
			return schedule.PublicScheduleFilter{}, invalidRangeError()
		}
		from = parsed
	}
	to := from.AddDate(0, 0, defaultScheduleRangeDays)
	if q.ToDate != "" {
		parsed, err := time.ParseInLocation(schedule.DateLayout, q.ToDate, time.UTC)
		if err != nil {
			return schedule.PublicScheduleFilter{}, invalidRangeError()
		}
		to = parsed
	}
	if to.Before(from) || int(to.Sub(from).Hours()/24) > MaxScheduleRangeDays-1 {
		return schedule.PublicScheduleFilter{}, invalidRangeError()
	}
	return schedule.PublicScheduleFilter{
		SubdepartmentID: q.SubdepartmentID,
		DoctorID:        q.DoctorID,
		FromDate:        from.Format(schedule.DateLayout),
		ToDate:          to.Format(schedule.DateLayout),
	}, nil
}

// invalidRangeError 统一日期窗口非法（顺序颠倒或超过 31 天）的响应文案（契约 §12.7）。
func invalidRangeError() *ServiceError {
	return &ServiceError{Code: CodeValidationFailed, Message: "日期范围无效"}
}

// offset 把 1 起始的页码换算为 SQL OFFSET；页码由绑定层保证 >=1。
func offset(page, pageSize int) int {
	if page < 1 {
		page = 1
	}
	return (page - 1) * pageSize
}
