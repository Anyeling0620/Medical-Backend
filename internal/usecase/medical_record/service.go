// Package medical_record 实现病历域用例：医生为自己负责的挂号记录书写、修改、删除、查询病历
// （spec/04-api-contract.md 病历一节）。
//
// 授权分两层：
//  1. 功能级：路由中间件校验管理端令牌必须带 MEDICAL_RECORD:* 权限码；
//  2. 数据级：本层把调用者解析成「医生编号」，所有读写都限定在
//     medical_registration.doctor_id = 该医生编号 的挂号下，越权与不存在统一按资源不存在处理。
//
// 账号与医生的绑定关系取自 mis_user.ref_id（医生账号的业务编号指向 doctor.id，见契约 §1.2）。
// 未绑定医生身份的账号（含 ROOT 管理员）一律返回 403 AUTH_FORBIDDEN：
// 与医生工作台 §6.10 的口径一致，数据范围只能来自令牌主体，不得退化为全量数据。
package medical_record

import (
	"context"
	"errors"
	"fmt"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainmedicalrecord "Medical-Web-Backend/internal/domain/medical_record"
	"Medical-Web-Backend/internal/port"
)

// 业务错误码：handler 依据这些稳定编码映射 HTTP 响应（契约 §10 错误码目录与病历一节）。
const (
	CodeValidationFailed = "REQUEST_VALIDATION_FAILED"
	// CodeNotFound 表示病历不存在，或存在但不属于当前医生负责的挂号。
	CodeNotFound = "MEDICAL_RECORD_NOT_FOUND"
	// CodeRegistrationNotFound 表示挂号不存在，或不由当前医生负责。
	CodeRegistrationNotFound = "REGISTRATION_NOT_FOUND"
	// CodeDuplicate 表示同一挂号已经有病历（同一挂号只允许一份病历，改用修改接口）。
	CodeDuplicate = "MEDICAL_RECORD_DUPLICATE"
	// CodeForbidden 表示调用者账号未绑定医生身份（mis_user.ref_id 为空或非正数），
	// 无法确定可访问的病历范围，本域所有读写都返回该错误。
	// 复用契约 §1.2、§6.10 的既有错误码，不另造编码。
	CodeForbidden = "AUTH_FORBIDDEN"
	// CodeDependencyUnavailable 表示仓储不可用，映射为 502 DEPENDENCY_UNAVAILABLE。
	CodeDependencyUnavailable = "DEPENDENCY_UNAVAILABLE"
)

// forbiddenMessage 是未绑定医生身份时的统一文案（与医生工作台 §6.10 的措辞保持同一风格）。
const forbiddenMessage = "当前账号未关联医生，无法访问病历数据"

// 分页边界（契约 §1.4）：请求层已校验，这里再兜底一次，避免其它调用方把超大窗口打到数据库。
const (
	maxPage     = 100000
	maxPageSize = 100
)

// ErrDependencyUnavailable 表示病历仓储不可用（连接失败、SQL 执行失败等），映射为
// 502 DEPENDENCY_UNAVAILABLE。依赖故障不能伪装成「病历不存在」，否则客户端会把可重试的
// 故障当成业务结论（契约 §10）。
var ErrDependencyUnavailable = errors.New("medical record dependency is unavailable")

// ErrInvalidActor 表示调用者身份不可用于授权判定（realm 不是管理端）。
// 这是内部调用错误（映射为 500）：本域只服务管理端令牌，路由中间件已按 realm=mis 拦截。
var ErrInvalidActor = errors.New("medical record actor is not usable for authorization")

// Details 是可选的错误细节（仅非敏感字段）。
type Details map[string]any

// ServiceError 是 use case 返回给 handler 的业务错误，携带稳定 code 与面向用户的 message。
type ServiceError struct {
	Code    string
	Message string
	Details Details
}

func (e *ServiceError) Error() string { return e.Message }

// Actor 是本次调用的调用者身份（管理端）。
// UserID 用于解析 mis_user.ref_id 得到医生编号，客户端提交的 doctorId 一律不被采信。
type Actor struct {
	Realm  domainauth.Realm
	UserID int64
}

// CreateInput 是书写病历的用例入参：归属字段（医生、就诊卡、子科室）一律由服务端从挂号推导，
// 客户端只能提交挂号编号、诊断与正文。
type CreateInput struct {
	RegistrationID int64
	Diagnosis      string
	Content        string
}

// ListQuery 是病历列表用例入参（取值与格式已由请求层校验）。
type ListQuery struct {
	RegistrationID *int64
	PatientCardID  *int64
	DoctorID       *int64
	Page           int
	PageSize       int
}

// ListPage 是病历列表用例结果：条目与分页元数据（契约统一列表结构）。
type ListPage struct {
	Items    []domainmedicalrecord.MedicalRecord
	Page     int
	PageSize int
	Total    int64
}

// Service 编排病历域用例：repo 提供病历读写、挂号归属判定与账号-医生绑定解析。
type Service struct {
	repo port.MedicalRecordRepository
}

// NewService 构造病历用例服务。
func NewService(repo port.MedicalRecordRepository) *Service {
	return &Service{repo: repo}
}

// Create 为指定挂号书写病历：先解析当前医生并确认挂号归属，再落库并返回病历资源。
func (s *Service) Create(
	ctx context.Context,
	actor Actor,
	input CreateInput,
) (*domainmedicalrecord.MedicalRecord, error) {
	doctorID, err := s.currentDoctorID(ctx, actor)
	if err != nil {
		return nil, err
	}
	diagnosis, err := domainmedicalrecord.NormalizeDiagnosis(input.Diagnosis)
	if err != nil {
		return nil, validationError(err)
	}
	content, err := domainmedicalrecord.NormalizeContent(input.Content)
	if err != nil {
		return nil, validationError(err)
	}
	owner, err := s.repo.FindRegistrationOwner(ctx, input.RegistrationID, doctorID)
	if err != nil {
		return nil, s.translate(err)
	}
	record, err := s.repo.CreateMedicalRecord(
		ctx, *owner, domainmedicalrecord.NewUUID(), diagnosis, content)
	if err != nil {
		return nil, s.translate(err)
	}
	return record, nil
}

// List 按条件分页返回当前医生负责的挂号下的病历。
func (s *Service) List(
	ctx context.Context,
	actor Actor,
	query ListQuery,
) (*ListPage, error) {
	doctorID, err := s.currentDoctorID(ctx, actor)
	if err != nil {
		return nil, err
	}
	page, pageSize := normalizePaging(query.Page, query.PageSize)
	items, total, err := s.repo.ListMedicalRecords(
		ctx,
		domainmedicalrecord.Filter{
			OwnerDoctorID:  doctorID,
			RegistrationID: query.RegistrationID,
			PatientCardID:  query.PatientCardID,
			DoctorID:       query.DoctorID,
		},
		(page-1)*pageSize,
		pageSize,
	)
	if err != nil {
		return nil, s.translate(err)
	}
	return &ListPage{Items: items, Page: page, PageSize: pageSize, Total: total}, nil
}

// Detail 读取病历详情；不属于当前医生负责的挂号时按不存在返回。
func (s *Service) Detail(
	ctx context.Context,
	actor Actor,
	recordID int64,
) (*domainmedicalrecord.MedicalRecord, error) {
	doctorID, err := s.currentDoctorID(ctx, actor)
	if err != nil {
		return nil, err
	}
	record, err := s.repo.FindMedicalRecord(ctx, recordID, doctorID)
	if err != nil {
		return nil, s.translate(err)
	}
	return record, nil
}

// Update 修改病历中提交的字段（PATCH 语义）；未提交任何字段属于参数错误。
func (s *Service) Update(
	ctx context.Context,
	actor Actor,
	recordID int64,
	input domainmedicalrecord.UpdateInput,
) (*domainmedicalrecord.MedicalRecord, error) {
	if input.Diagnosis == nil && input.Content == nil {
		return nil, &ServiceError{
			Code:    CodeValidationFailed,
			Message: "至少提交 diagnosis 或 content 中的一个字段",
		}
	}
	doctorID, err := s.currentDoctorID(ctx, actor)
	if err != nil {
		return nil, err
	}
	normalized := domainmedicalrecord.UpdateInput{}
	if input.Diagnosis != nil {
		diagnosis, err := domainmedicalrecord.NormalizeDiagnosis(*input.Diagnosis)
		if err != nil {
			return nil, validationError(err)
		}
		normalized.Diagnosis = &diagnosis
	}
	if input.Content != nil {
		content, err := domainmedicalrecord.NormalizeContent(*input.Content)
		if err != nil {
			return nil, validationError(err)
		}
		normalized.Content = &content
	}
	record, err := s.repo.UpdateMedicalRecord(ctx, recordID, doctorID, normalized)
	if err != nil {
		return nil, s.translate(err)
	}
	return record, nil
}

// Delete 删除病历；不存在或不属于当前医生负责的挂号时按不存在返回。
func (s *Service) Delete(ctx context.Context, actor Actor, recordID int64) error {
	doctorID, err := s.currentDoctorID(ctx, actor)
	if err != nil {
		return err
	}
	if err := s.repo.DeleteMedicalRecord(ctx, recordID, doctorID); err != nil {
		return s.translate(err)
	}
	return nil
}

// currentDoctorID 解析本次调用可操作的医生编号。
//
// 只接受 realm=mis 的令牌，并只用令牌主体查 mis_user.ref_id：客户端提交的 doctorId 一律不采信。
// 未绑定医生身份（ref_id 为空或非正数）时返回 403 AUTH_FORBIDDEN，
// 绝不退化为「不限定归属」的全量数据（契约 §1.2、§6.10）。
func (s *Service) currentDoctorID(ctx context.Context, actor Actor) (int64, error) {
	if actor.Realm != domainauth.RealmMis {
		return 0, ErrInvalidActor
	}
	if s.repo == nil {
		return 0, ErrDependencyUnavailable
	}
	doctorID, err := s.repo.FindDoctorIDByUserID(ctx, actor.UserID)
	if err != nil {
		return 0, fmt.Errorf("%w: 读取医生绑定失败", ErrDependencyUnavailable)
	}
	if doctorID <= 0 {
		return 0, &ServiceError{Code: CodeForbidden, Message: forbiddenMessage}
	}
	return doctorID, nil
}

// translate 把仓储/领域错误翻译成稳定业务错误码。
// 未识别的错误按依赖不可用处理（502），避免把数据库故障伪装成业务结论。
func (s *Service) translate(err error) error {
	switch {
	case errors.Is(err, domainmedicalrecord.ErrNotFound):
		return &ServiceError{Code: CodeNotFound, Message: "病历不存在"}
	case errors.Is(err, domainmedicalrecord.ErrRegistrationNotFound):
		return &ServiceError{Code: CodeRegistrationNotFound, Message: "挂号记录不存在"}
	case errors.Is(err, domainmedicalrecord.ErrDuplicate):
		return &ServiceError{Code: CodeDuplicate, Message: "该挂号已有病历，请改用修改接口"}
	default:
		return fmt.Errorf("%w: %v", ErrDependencyUnavailable, err)
	}
}

// validationError 把领域校验错误翻译成 422 的稳定文案。
func validationError(err error) error {
	switch {
	case errors.Is(err, domainmedicalrecord.ErrDiagnosisRequired):
		return &ServiceError{Code: CodeValidationFailed, Message: "diagnosis 为必传字段"}
	case errors.Is(err, domainmedicalrecord.ErrDiagnosisTooLong):
		return &ServiceError{
			Code:    CodeValidationFailed,
			Message: fmt.Sprintf("diagnosis 不能超过 %d 个字符", domainmedicalrecord.MaxDiagnosisRunes),
		}
	case errors.Is(err, domainmedicalrecord.ErrContentRequired):
		return &ServiceError{Code: CodeValidationFailed, Message: "content 为必传字段"}
	case errors.Is(err, domainmedicalrecord.ErrContentTooLong):
		return &ServiceError{
			Code:    CodeValidationFailed,
			Message: fmt.Sprintf("content 不能超过 %d 个字符", domainmedicalrecord.MaxContentRunes),
		}
	default:
		return &ServiceError{Code: CodeValidationFailed, Message: err.Error()}
	}
}

// normalizePaging 兜底分页参数：越界值收敛到合法区间，保证不会出现负 offset。
func normalizePaging(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if page > maxPage {
		page = maxPage
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	return page, pageSize
}
