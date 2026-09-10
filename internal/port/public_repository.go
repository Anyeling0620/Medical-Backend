package port

import (
	"context"

	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/domain/schedule"
)

// PublicCatalogRepository 描述匿名公开域对目录数据（科室、子科室、医生）的只读访问能力。
// 与管理端 CatalogRepository 分开定义的原因：
//  1. 公开域只允许契约 §2.3 的字段集合，实现只能查询相应列；
//  2. 公开域医生固定「在岗且非隐藏」，不存在 status 等管理端过滤参数；
//  3. 公开域不需要 options、doctor-prices 等管理端接口，避免实现被迫补空方法。
type PublicCatalogRepository interface {
	// ListPublicDepartments 分页返回科室；过滤与排序同管理端目录（字段集合本就一致）。
	ListPublicDepartments(ctx context.Context, f catalog.DepartmentFilter, offset, limit int) ([]catalog.Department, int64, error)
	// FindPublicDepartment 返回单个科室；不存在返回 sql.ErrNoRows。
	FindPublicDepartment(ctx context.Context, id int64) (*catalog.Department, error)
	// ListPublicSubdepartments 分页返回某科室下的子科室；科室不存在返回 sql.ErrNoRows。
	ListPublicSubdepartments(ctx context.Context, departmentID int64, offset, limit int) ([]catalog.Subdepartment, int64, error)
	// ListPublicDoctors 分页返回公开医生列表，只包含在岗（ACTIVE）且已关联子科室的医生。
	ListPublicDoctors(ctx context.Context, f catalog.PublicDoctorFilter, offset, limit int) ([]catalog.PublicDoctor, int64, error)
	// FindPublicDoctor 返回医生详情（含子科室与价目）；医生不存在、非在岗或隐藏时返回 sql.ErrNoRows，
	// 由 handler 统一按「医生不存在」隐藏，不泄漏管理端状态。
	FindPublicDoctor(ctx context.Context, id int64) (*catalog.PublicDoctorDetail, error)
}

// PublicScheduleRepository 描述匿名公开域对可挂号时段的只读查询能力。
// 该接口只读：不占用号源、不产生幂等记录（契约 §8.1）。
type PublicScheduleRepository interface {
	// ListPublicSchedules 分页返回可挂号时段（含满号时段），并计算 remaining。
	// 只返回在岗医生名下的时段，避免展示无法挂号的号源。
	ListPublicSchedules(ctx context.Context, f schedule.PublicScheduleFilter, offset, limit int) ([]schedule.PublicSchedule, int64, error)
}
