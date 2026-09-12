// Package doctorpatient 实现「医生本人患者」用例（契约 §6.10）。
//
// 该用例只做两件事：把登录主体解析成 doctor.id，再把查询收敛到该医生的患者。
// 数据范围完全来自登录主体（mis_user.ref_id），不接受客户端提交的医生编号，
// 这是本视图唯一的越权防线。
package doctorpatient

import (
	"context"
	"errors"
	"fmt"

	domaindoctorpatient "Medical-Web-Backend/internal/domain/doctorpatient"
	"Medical-Web-Backend/internal/port"
)

// ErrDoctorNotBound 表示当前管理端账号没有绑定医生（mis_user.ref_id 为空或非法）。
// handler 按契约 §1.2 映射为 403 AUTH_FORBIDDEN：账号本身合法，
// 但不属于医生视角数据的使用者。
var ErrDoctorNotBound = errors.New("当前账号未关联医生")

// ErrDependencyUnavailable 表示账号仓储或患者仓储不可用（数据库连接失败等），
// handler 按契约 §10 映射为 502 DEPENDENCY_UNAVAILABLE，而不是笼统的 500。
var ErrDependencyUnavailable = errors.New("doctor patient dependency is unavailable")

// defaultPageSize 与请求层的缺省每页条数保持一致：调用方绕过请求层传入非法分页时用它兜底。
const defaultPageSize = 20

// Query 是「我的患者」列表的查询条件；字段合法性由请求层保证。
type Query struct {
	Keyword  string
	Sort     string
	Order    string
	Page     int
	PageSize int
}

// Service 提供医生视角的患者查询。
type Service struct {
	users    port.UserRepository
	patients port.DoctorPatientRepository
}

func NewService(
	users port.UserRepository,
	patients port.DoctorPatientRepository,
) *Service {
	return &Service{users: users, patients: patients}
}

// MyPatients 返回该管理端账号作为医生接诊过的患者分页列表。
//
// misUserID 取自访问令牌主体；未绑定医生的账号返回 ErrDoctorNotBound，
// 而不是返回空列表——空列表会让前端误以为「暂无患者」。
func (s *Service) MyPatients(
	ctx context.Context,
	misUserID int64,
	q Query,
) (*domaindoctorpatient.Page, error) {
	if s == nil || s.users == nil || s.patients == nil {
		return nil, errors.New("医生患者视图依赖未配置")
	}
	if misUserID < 1 {
		return nil, ErrDoctorNotBound
	}

	account, err := s.users.FindByID(ctx, misUserID)
	if err != nil {
		// 归类为依赖不可用（handler → 502），同时用第二个 %w 保留根因错误链。
		return nil, fmt.Errorf("%w: 读取登录账号失败: %w", ErrDependencyUnavailable, err)
	}
	if account == nil || account.RefID == nil || *account.RefID < 1 {
		return nil, ErrDoctorNotBound
	}

	// 请求层已校验分页，这里再兜底一次：调用方绕过请求层传入 0 时，
	// 不能让 SQL 的 LIMIT 0 把「参数错误」静默表现成「无数据」。
	page, pageSize := q.Page, q.PageSize
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPageSize
	}

	filter := domaindoctorpatient.Filter{
		DoctorID: *account.RefID,
		Keyword:  q.Keyword,
		Sort:     q.Sort,
		Order:    q.Order,
		Page:     page,
		PageSize: pageSize,
	}
	items, total, err := s.patients.ListDoctorPatients(
		ctx, filter, filter.Offset(), pageSize)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询医生患者列表失败: %w", ErrDependencyUnavailable, err)
	}
	return &domaindoctorpatient.Page{
		Items:    items,
		Page:     page,
		PageSize: pageSize,
		Total:    total,
	}, nil
}
