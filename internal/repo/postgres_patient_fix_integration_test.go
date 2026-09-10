package repo

import (
	"errors"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"Medical-Web-Backend/internal/domain/patient"
)

// 本文件锁定 Slice 4 复审后的 repository 修复项：分页窗口校验与患者域
// 咨询锁的双键隔离。仅新增测试，不修改任何生产代码。

// TestPatientRepositoryListCardsByPatientRejectsInvalidPagination 验证 repository
// 主动拦截非法分页窗口并返回 ErrInvalidPagination，而不是让 PostgreSQL 的
// 2201W/2201X 原始错误冒泡成 500（契约 §1.4 要求映射为 422）。
func TestPatientRepositoryListCardsByPatientRejectsInvalidPagination(t *testing.T) {
	it, ctx := newPatientIT(t)
	owner := it.newPatient(ctx)

	cases := []struct {
		name   string
		offset int
		limit  int
	}{
		{"负 offset", -1, 20},
		{"limit 为 0", 0, 0},
		{"负 limit", 0, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := it.repo.ListCardsByPatient(ctx, owner.ID, tc.offset, tc.limit)
			if !errors.Is(err, patient.ErrInvalidPagination) {
				t.Fatalf("ListCardsByPatient(offset=%d, limit=%d) 错误 = %v，期望 %v",
					tc.offset, tc.limit, err, patient.ErrInvalidPagination)
			}
		})
	}

	// 合法窗口不应报错：owner 尚未建卡，应返回空列表与 total=0。
	cards, total, err := it.repo.ListCardsByPatient(ctx, owner.ID, 0, 20)
	if err != nil {
		t.Fatalf("合法分页窗口意外错误：%v", err)
	}
	if len(cards) != 0 || total != 0 {
		t.Fatalf("尚未建卡时 len=%d total=%d，期望 0/0", len(cards), total)
	}
}

// TestPatientAdvisoryLockUsesTwoKeyForm 验证患者域的咨询锁以 (classid, objid)
// 双键形式登记，并与排班域使用的单 bigint 键空间相互隔离。
func TestPatientAdvisoryLockUsesTwoKeyForm(t *testing.T) {
	it, ctx := newPatientIT(t)

	objID := patientRegisterLockKey("itest-lock-" + uuid.NewString())

	tx, err := it.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务失败：%v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock($1, $2)", patientAdvisoryClass, objID); err != nil {
		t.Fatalf("获取双键咨询锁失败：%v", err)
	}

	// 只读核对：本次持锁必须登记为 classid=patientAdvisoryClass、objsubid=2。
	classText := strconv.FormatInt(int64(patientAdvisoryClass), 10)
	objText := strconv.FormatUint(uint64(uint32(objID)), 10)
	var count int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()"+
			" AND objsubid = 2 AND classid::text = $1 AND objid::text = $2",
		classText, objText).Scan(&count); err != nil {
		t.Fatalf("查询 pg_locks 失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("pg_locks 中双键咨询锁行数 = %d，期望 1（classid=%s objid=%s objsubid=2）",
			count, classText, objText)
	}

	// 另一会话尝试同一 (classid, objid)：必须返回 false。
	var peerAcquired bool
	if err := it.db.QueryRowContext(ctx,
		"SELECT pg_try_advisory_xact_lock($1, $2)", patientAdvisoryClass, objID).Scan(&peerAcquired); err != nil {
		t.Fatalf("另一会话获取双键咨询锁失败：%v", err)
	}
	if peerAcquired {
		t.Fatal("另一会话不应取得已被占用的双键咨询锁")
	}

	// 跨键空间隔离：把同样两个数值包装成单 bigint 键，必须可立即获得。
	bigintKey := int64(uint32(patientAdvisoryClass))<<32 | int64(uint32(objID))
	var singleAcquired bool
	if err := it.db.QueryRowContext(ctx,
		"SELECT pg_try_advisory_xact_lock($1)", bigintKey).Scan(&singleAcquired); err != nil {
		t.Fatalf("另一会话获取单键咨询锁失败：%v", err)
	}
	if !singleAcquired {
		t.Fatalf("单 bigint 键空间应与 (classid, objid) 双键空间隔离：key=%d 不应被阻塞", bigintKey)
	}
}
