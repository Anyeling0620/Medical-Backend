package port

import (
	"context"

	"Medical-Web-Backend/internal/domain/doctor"
)

// DoctorRepository contains persistence operations for doctor search.
type DoctorRepository interface {
	Search(ctx context.Context, filters doctor.SearchFilters, offset, limit int) ([]doctor.Doctor, error)
	Count(ctx context.Context, filters doctor.SearchFilters) (int64, error)
}
