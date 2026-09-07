package doctor

import (
	"context"
	"database/sql"
	"errors"

	domaindoctor "Medical-Web-Backend/internal/domain/doctor"
	"Medical-Web-Backend/internal/port"
)

var ErrInvalidInput = errors.New("医生查询参数不正确")
var ErrNotFound = errors.New("医生不存在")

// Service handles doctor search use cases.
type Service struct {
	repository       port.DoctorRepository
	detailRepository port.DoctorDetailRepository
}

func NewService(repository port.DoctorRepository) *Service {
	detailRepository, _ := repository.(port.DoctorDetailRepository)

	return &Service{
		repository:       repository,
		detailRepository: detailRepository,
	}
}

func (s *Service) Search(
	ctx context.Context,
	filters domaindoctor.SearchFilters,
	page int,
	length int,
) ([]domaindoctor.Doctor, error) {
	if s == nil || s.repository == nil {
		return nil, errors.New("doctor repository is not configured")
	}

	if page < 1 || length < 10 || length > 50 || page-1 > maxInt()/length {
		return nil, ErrInvalidInput
	}

	return s.repository.Search(ctx, filters, (page-1)*length, length)
}

func (s *Service) Count(
	ctx context.Context,
	filters domaindoctor.SearchFilters,
) (int64, error) {
	if s == nil || s.repository == nil {
		return 0, errors.New("doctor repository is not configured")
	}

	return s.repository.Count(ctx, filters)
}

func (s *Service) ListDepts(ctx context.Context) ([]string, error) {
	if s == nil || s.repository == nil {
		return nil, errors.New("doctor repository is not configured")
	}
	return s.repository.ListDepts(ctx)
}

func (s *Service) ListDegrees(ctx context.Context) ([]string, error) {
	if s == nil || s.repository == nil {
		return nil, errors.New("doctor repository is not configured")
	}
	return s.repository.ListDegrees(ctx)
}

func (s *Service) ListJobs(ctx context.Context) ([]string, error) {
	if s == nil || s.repository == nil {
		return nil, errors.New("doctor repository is not configured")
	}
	return s.repository.ListJobs(ctx)
}

func (s *Service) FindByID(
	ctx context.Context,
	id int64,
) (*domaindoctor.DoctorDetail, error) {
	if s == nil || s.detailRepository == nil {
		return nil, errors.New("doctor detail repository is not configured")
	}

	if id < 1 {
		return nil, ErrInvalidInput
	}

	detail, err := s.detailRepository.FindByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || detail == nil {
		return nil, ErrNotFound
	}

	return detail, err
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
