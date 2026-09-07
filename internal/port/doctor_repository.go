package port

import (
	"context"

	"Medical-Web-Backend/internal/domain/doctor"
)

// DoctorRepository contains persistence operations for doctor search.
type DoctorRepository interface {
	Search(ctx context.Context, filters doctor.SearchFilters, offset, limit int) ([]doctor.Doctor, error)
	Count(ctx context.Context, filters doctor.SearchFilters) (int64, error)
	ListDepts(ctx context.Context) ([]string, error)
	ListDegrees(ctx context.Context) ([]string, error)
	ListJobs(ctx context.Context) ([]string, error)
}

// DoctorDetailRepository contains the optional doctor detail operation.
type DoctorDetailRepository interface {
	FindByID(ctx context.Context, id int64) (*doctor.DoctorDetail, error)
}
