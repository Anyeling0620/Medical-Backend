package doctor

import (
	"context"
	"errors"

	domaindoctor "Medical-Web-Backend/internal/domain/doctor"
	"Medical-Web-Backend/internal/port"
)

var ErrInvalidInput = errors.New("医生查询参数不正确")

// Service handles doctor search use cases.
type Service struct {
	repository port.DoctorRepository
}

func NewService(repository port.DoctorRepository) *Service {
	return &Service{repository: repository}
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

func maxInt() int { return int(^uint(0) >> 1) }
