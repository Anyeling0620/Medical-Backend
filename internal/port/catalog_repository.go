package port

import (
	"Medical-Web-Backend/internal/domain/catalog"
	"context"
)

type CatalogRepository interface {
	ListDepartments(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error)
	FindDepartment(context.Context, int64) (*catalog.Department, error)
	ListSubdepartments(context.Context, int64, int, int) ([]catalog.Subdepartment, int64, error)
}
