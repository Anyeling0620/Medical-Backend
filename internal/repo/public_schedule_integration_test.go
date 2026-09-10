package repo

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"

	"Medical-Web-Backend/internal/config"
	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/domain/schedule"
)

// 本文件是匿名公开查询域仓储的真实 PostgreSQL 集成测试：严格只读。
//
// 只调用公开域的 SELECT 语义方法（ListPublicDepartments / FindPublicDepartment /
// ListPublicSubdepartments / ListPublicDoctors / FindPublicDoctor / ListPublicSchedules），
// 断言「能成功执行且不报错」；不写入、不更新、不删除任何数据，
// 也不对行数据是否存在做强断言（空库同样应通过），因此可安全地在共享库上执行。
//
// 未设置 PGSQL_INTEGRATION_TEST=1 时一律 t.Skip；连不上数据库也跳过而不是失败。

// publicCatalogITSkipMsg 是未开启集成测试开关时的跳过说明。
const publicCatalogITSkipMsg = "设置 PGSQL_INTEGRATION_TEST=1 后才会执行 PostgreSQL 集成测试"

// newPublicCatalogIT 组装只读集成测试所需的仓库实例；未开启开关或连不上数据库时跳过。
func newPublicCatalogIT(t *testing.T) (*PostgresDoctorRepository, *PostgresScheduleRepository, context.Context) {
	t.Helper()
	if os.Getenv("PGSQL_INTEGRATION_TEST") != "1" {
		t.Skip(publicCatalogITSkipMsg)
	}
	if err := godotenv.Load("../../.env"); err != nil {
		t.Skipf("加载 .env 失败，跳过集成测试：%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("加载配置失败，跳过集成测试：%v", err)
	}
	dsn := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.Postgres.Username, cfg.Postgres.Password),
		Host:   cfg.Postgres.Addr,
		Path:   cfg.Postgres.Database,
	}
	query := dsn.Query()
	query.Set("sslmode", cfg.Postgres.SSLMode)
	dsn.RawQuery = query.Encode()

	db, err := sql.Open("pgx", dsn.String())
	if err != nil {
		t.Skipf("打开 PostgreSQL 连接失败，跳过集成测试：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("PostgreSQL 不可用，跳过集成测试：%v", err)
	}
	return NewPostgresDoctorRepository(db), NewPostgresScheduleRepository(db), ctx
}

// TestPublicCatalogRepositoryIntegrationReadOnly 覆盖公开域目录查询：科室、子科室、医生列表与详情。
func TestPublicCatalogRepositoryIntegrationReadOnly(t *testing.T) {
	doctors, _, ctx := newPublicCatalogIT(t)

	departments, total, err := doctors.ListPublicDepartments(ctx, catalog.DepartmentFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("ListPublicDepartments 执行失败：%v", err)
	}
	t.Logf("公开科室列表执行成功：返回 %d 条，total=%d", len(departments), total)
	if departments == nil {
		t.Errorf("ListPublicDepartments 应返回非 nil 切片（空库时为空数组）")
	}

	// 子科室：目标科室存在时返回列表，不存在时应为 sql.ErrNoRows，两者都算「执行成功」。
	if len(departments) > 0 {
		subdepartments, subTotal, err := doctors.ListPublicSubdepartments(ctx, departments[0].ID, 0, 20)
		if err != nil {
			t.Fatalf("ListPublicSubdepartments 执行失败：%v", err)
		}
		t.Logf("公开子科室列表执行成功：科室 %d 返回 %d 条，total=%d", departments[0].ID, len(subdepartments), subTotal)

		if detail, err := doctors.FindPublicDepartment(ctx, departments[0].ID); err != nil {
			t.Fatalf("FindPublicDepartment 执行失败：%v", err)
		} else if detail == nil || detail.ID != departments[0].ID {
			t.Errorf("FindPublicDepartment 返回错误数据：%+v", detail)
		}
	} else {
		t.Logf("当前库无科室数据，跳过子科室/科室详情调用")
	}

	// 不存在的科室：允许 sql.ErrNoRows 或 nil，但不能是数据库执行错误。
	if _, err := doctors.FindPublicDepartment(ctx, 999999999); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("FindPublicDepartment 对不存在的科室返回了非预期错误：%v", err)
	}
	if _, _, err := doctors.ListPublicSubdepartments(ctx, 999999999, 0, 20); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("ListPublicSubdepartments 对不存在的科室返回了非预期错误：%v", err)
	}

	items, doctorTotal, err := doctors.ListPublicDoctors(ctx, catalog.PublicDoctorFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("ListPublicDoctors 执行失败：%v", err)
	}
	t.Logf("公开医生列表执行成功：返回 %d 条，total=%d", len(items), doctorTotal)
	if items == nil {
		t.Errorf("ListPublicDoctors 应返回非 nil 切片（空库时为空数组）")
	}

	// 只对列表里真实存在的医生做详情校验；不存在时允许 sql.ErrNoRows。
	if len(items) > 0 {
		if detail, err := doctors.FindPublicDoctor(ctx, items[0].ID); err != nil {
			t.Fatalf("FindPublicDoctor 执行失败：%v", err)
		} else if detail == nil {
			t.Errorf("FindPublicDoctor 命中列表中的医生 %d 却返回 nil", items[0].ID)
		} else {
			if detail.Subdepartments == nil || detail.Prices == nil {
				t.Errorf("FindPublicDoctor 的子科室/价目应为非 nil 切片：%+v", detail)
			}
			// 公开域不得返回管理端字段：这里以 ID/名称等公开字段为主做存在性断言。
			if detail.ID != items[0].ID {
				t.Errorf("详情 ID = %d, want %d", detail.ID, items[0].ID)
			}
		}
	} else {
		t.Logf("当前库无在岗医生数据，跳过医生详情调用")
	}
	if _, err := doctors.FindPublicDoctor(ctx, 999999999); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("FindPublicDoctor 对不存在的医生返回了非预期错误：%v", err)
	}
}

// TestPublicScheduleRepositoryIntegrationReadOnly 覆盖公开可挂号时段查询（含 remaining 计算）。
func TestPublicScheduleRepositoryIntegrationReadOnly(t *testing.T) {
	_, schedules, ctx := newPublicCatalogIT(t)

	// 业务当天（Asia/Shanghai）起 7 天，与默认窗口一致。
	today := schedule.BusinessToday(time.Now())
	from := today.Format(schedule.DateLayout)
	to := today.AddDate(0, 0, 6).Format(schedule.DateLayout)

	items, total, err := schedules.ListPublicSchedules(ctx, schedule.PublicScheduleFilter{
		FromDate: from,
		ToDate:   to,
	}, 0, 20)
	if err != nil {
		t.Fatalf("ListPublicSchedules 执行失败：%v", err)
	}
	t.Logf("公开时段查询执行成功：窗口 %s..%s 返回 %d 条，total=%d", from, to, len(items), total)
	if items == nil {
		t.Errorf("ListPublicSchedules 应返回非 nil 切片（空库时为空数组）")
	}

	for _, item := range items {
		if item.ScheduleID <= 0 || item.Date == "" {
			t.Errorf("时段缺少必要字段：%+v", item)
		}
		// 满号时段（remaining=0）必须保留，且剩余号源不得为负数。
		if item.Remaining < 0 {
			t.Errorf("remaining 不应为负数：%+v", item)
		}
		if item.Amount == "" {
			t.Errorf("amount 不应为空串：%+v", item)
		}
	}

	// 更大的窗口与分页参数也应能执行（只读，不写库）。
	if _, _, err := schedules.ListPublicSchedules(ctx, schedule.PublicScheduleFilter{
		FromDate: from,
		ToDate:   today.AddDate(0, 0, 30).Format(schedule.DateLayout),
	}, 20, 50); err != nil {
		t.Fatalf("带分页的 ListPublicSchedules 执行失败：%v", err)
	}
}
