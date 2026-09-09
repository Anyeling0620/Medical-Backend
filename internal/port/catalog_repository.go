package port

import (
	"Medical-Web-Backend/internal/domain/catalog"
	"context"
)

type CatalogRepository interface {
	ListDepartments(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error)
	FindDepartment(context.Context, int64) (*catalog.Department, error)
	ListSubdepartments(context.Context, int64, int, int) ([]catalog.Subdepartment, int64, error)
	FindSubdepartment(context.Context, int64) (*catalog.SubdepartmentDetail, error)
	ListDoctors(context.Context, catalog.DoctorFilter, int, int) ([]catalog.DoctorCatalog, int64, error)
	FindDoctor(context.Context, int64) (*catalog.DoctorCatalogDetail, error)
	ListDoctorOptions(context.Context) (catalog.DoctorOptions, error)
	ListDoctorPrices(context.Context, int64, int, int) ([]catalog.DoctorPrice, int64, error)
}
