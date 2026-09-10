package repo

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"Medical-Web-Backend/internal/domain/registration"
)

// 本文件是挂号（medical_registration）SQL 片段构造与文本语义的纯函数测试（不连数据库）：
// 列表 WHERE 的占位符编号/顺序/参数、排序白名单、字段投影范围，以及占用判定与时段快照
// 查询的固定语义（spec/04-api-contract.md §6.1–§6.4、§1.4）。

// TestRegistrationWhereNoFilters 无过滤条件时不生成 WHERE，也不产生参数。
func TestRegistrationWhereNoFilters(t *testing.T) {
	where, args := registrationWhere(registration.Filter{})

	if where != "" {
		t.Errorf("无条件 where 应为空，got %q", where)
	}
	if len(args) != 0 {
		t.Errorf("无条件 args 应为空，got %#v", args)
	}
}

// TestRegistrationWhereOwnerCondition 患者端归属条件必须是就诊卡 EXISTS 子查询
// （越权记录不可见），且不能退化为对 r.patient_card_id 的等值过滤。
func TestRegistrationWhereOwnerCondition(t *testing.T) {
	owner := int64(20)
	where, args := registrationWhere(registration.Filter{OwnerPatientID: &owner})

	if !strings.HasPrefix(where, " WHERE ") {
		t.Errorf("where 必须以 WHERE 开头，got %q", where)
	}
	if !strings.Contains(where,
		"EXISTS (SELECT 1 FROM hospital.patient_user_info_card c WHERE c.id = r.patient_card_id AND c.user_id = $1)") {
		t.Errorf("owner 条件应为就诊卡归属 EXISTS 子查询：\n%s", where)
	}
	wantArgs := []any{int64(20)}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
	if got := strings.Count(where, "$"); got != len(args) {
		t.Errorf("占位符数量 = %d, 参数数量 = %d\n%s", got, len(args), where)
	}
}

// TestRegistrationWherePlaceholders 七个过滤条件按 owner、patientCardId、doctorId、
// subdepartmentId、fromDate、toDate、paymentStatus 顺序追加占位符，编号连续且与 args 顺序一致；
// 日期条件使用 $n::date 显式转换，避免与 date 列比较时依赖隐式转换。
func TestRegistrationWherePlaceholders(t *testing.T) {
	owner, patientCard, doctor, subdepartment := int64(20), int64(10), int64(16), int64(2)
	paymentStatus := registration.PaymentCodeUnpaid
	where, args := registrationWhere(registration.Filter{
		OwnerPatientID:  &owner,
		PatientCardID:   &patientCard,
		DoctorID:        &doctor,
		SubdepartmentID: &subdepartment,
		FromDate:        "2026-09-01",
		ToDate:          "2026-09-30",
		PaymentStatus:   &paymentStatus,
	})

	for _, fragment := range []string{
		"c.user_id = $1",
		"r.patient_card_id = $2",
		"r.doctor_id = $3",
		"r.dept_sub_id = $4",
		"r.date >= $5::date",
		"r.date <= $6::date",
		"r.payment_status = $7",
	} {
		if !strings.Contains(where, fragment) {
			t.Errorf("where 缺少 %q：\n%s", fragment, where)
		}
	}
	wantArgs := []any{
		int64(20), int64(10), int64(16), int64(2),
		"2026-09-01", "2026-09-30", registration.PaymentCodeUnpaid,
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
	if got := strings.Count(where, "$"); got != len(args) {
		t.Errorf("占位符数量 = %d, 参数数量 = %d\n%s", got, len(args), where)
	}
}

// TestRegistrationWherePlaceholdersFollowProvidedFilters 占位符编号按「已提供的条件」
// 连续编号，跳过未提供的条件后不会留下空洞（否则参数会与占位符错位）。
func TestRegistrationWherePlaceholdersFollowProvidedFilters(t *testing.T) {
	subdepartment := int64(9)
	paid := registration.PaymentCodePaid
	cases := []struct {
		name        string
		filter      registration.Filter
		wantArgs    []any
		wantContain []string
	}{
		{
			name:        "仅 fromDate 时从 $1 开始",
			filter:      registration.Filter{FromDate: "2026-09-01"},
			wantArgs:    []any{"2026-09-01"},
			wantContain: []string{"r.date >= $1::date"},
		},
		{
			name:        "仅 paymentStatus 时为 $1",
			filter:      registration.Filter{PaymentStatus: &paid},
			wantArgs:    []any{registration.PaymentCodePaid},
			wantContain: []string{"r.payment_status = $1"},
		},
		{
			name: "跳过 patientCardId 与 doctorId 后占位符仍连续",
			filter: registration.Filter{
				SubdepartmentID: &subdepartment,
				ToDate:          "2026-09-30",
			},
			wantArgs:    []any{int64(9), "2026-09-30"},
			wantContain: []string{"r.dept_sub_id = $1", "r.date <= $2::date"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			where, args := registrationWhere(tc.filter)
			for _, fragment := range tc.wantContain {
				if !strings.Contains(where, fragment) {
					t.Errorf("where 缺少 %q：\n%s", fragment, where)
				}
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("args = %#v, want %#v", args, tc.wantArgs)
			}
			if got := strings.Count(where, "$"); got != len(args) {
				t.Errorf("占位符数量 = %d, 参数数量 = %d\n%s", got, len(args), where)
			}
		})
	}
}

// TestRegistrationOrderBy 排序字段只来自白名单（契约 §1.4：禁止拼接 SQL），
// 默认按 createDate 倒序（最近挂号在前），未知取值回退默认而不是报错。
func TestRegistrationOrderBy(t *testing.T) {
	cases := []struct {
		name    string
		sort    string
		order   string
		wantCol string
		wantDir string
	}{
		{"默认 createDate 倒序", "", "", "r.create_time", "DESC"},
		{"createDate 升序", registration.SortCreateDate, "asc", "r.create_time", "ASC"},
		{"date 降序", registration.SortDate, "desc", "r.date", "DESC"},
		{"id 升序", registration.SortID, "asc", "r.id", "ASC"},
		{"order 大小写不敏感", registration.SortID, "ASC", "r.id", "ASC"},
		{"未知 sort 回退 createDate", "dropTable", "asc", "r.create_time", "ASC"},
		{"未知 order 回退 desc", registration.SortDate, "random", "r.date", "DESC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			col, dir := registrationOrderBy(registration.Filter{Sort: tc.sort, Order: tc.order})
			if col != tc.wantCol || dir != tc.wantDir {
				t.Errorf("registrationOrderBy(%q,%q) = (%q,%q), want (%q,%q)",
					tc.sort, tc.order, col, dir, tc.wantCol, tc.wantDir)
			}
		})
	}
}

// TestRegistrationColumnsExcludeSensitivePaymentFields 列表/详情投影不得包含
// prepay_id 与 transaction_id：契约 §6.3 明确支付二维码内容只在受保护的支付响应中返回。
func TestRegistrationColumnsExcludeSensitivePaymentFields(t *testing.T) {
	for _, forbidden := range []string{"prepay_id", "transaction_id"} {
		if strings.Contains(registrationColumns, forbidden) {
			t.Errorf("registrationColumns 不应包含 %q：\n%s", forbidden, registrationColumns)
		}
	}
	for _, required := range []string{"r.id", "btrim(r.out_trade_no)", "r.payment_status", "r.create_time"} {
		if !strings.Contains(registrationColumns, required) {
			t.Errorf("registrationColumns 缺少 %q：\n%s", required, registrationColumns)
		}
	}
}

// TestOccupyingRegistrationSQLSemantics 占用判定必须按身份证号跨账号判重：
// 通过就诊卡表关联后比较 btrim(pid)，并使用 EXISTS 语义（同一 pid 可能对应多张卡，
// JOIN 结果行数不代表挂号数）；占用状态只有 PAID 与未超期的 UNPAID，
// REFUNDED/EXPIRED 不占用（契约 §6.1、§6.2）。
func TestOccupyingRegistrationSQLSemantics(t *testing.T) {
	query := occupyingRegistrationExistsQuery

	for _, fragment := range []string{
		"SELECT EXISTS(",
		"JOIN hospital.patient_user_info_card c ON c.id = r.patient_card_id",
		"btrim(c.pid) = $1",
		"r.doctor_schedule_id = $2",
		"r.payment_status = $3 OR (r.payment_status = $4 AND r.create_time >= $5::date)",
	} {
		if !strings.Contains(query, fragment) {
			t.Errorf("占用判定 SQL 缺少 %q：\n%s", fragment, query)
		}
	}

	// hasOccupyingRegistration 的参数顺序是 (pid, scheduleId, PAID, UNPAID, 业务日)，
	// 因此 $3/$4 分别是已付款与未付款编码；两个编码的取值来自 domain 常量。
	if registration.PaymentCodePaid != 2 {
		t.Errorf("PaymentCodePaid = %d, want 2", registration.PaymentCodePaid)
	}
	if registration.PaymentCodeUnpaid != 1 {
		t.Errorf("PaymentCodeUnpaid = %d, want 1", registration.PaymentCodeUnpaid)
	}
	if got := strings.Count(query, "payment_status"); got != 2 {
		t.Errorf("占用判定只应比较两种占用状态，payment_status 出现 %d 次：\n%s", got, query)
	}
	if got := strings.Count(query, "$"); got != 5 {
		t.Errorf("占位符数量 = %d, want 5：\n%s", got, query)
	}
	// REFUNDED(3) 与 EXPIRED(4) 既不匹配 $3 也不匹配 $4，因此不会被判为占用；
	// 这里额外确认 SQL 没有把它们写成显式排除（显式排除会掩盖编码映射错误）。
	for _, forbidden := range []string{"REFUNDED", "EXPIRED", "payment_status = 3", "payment_status = 4"} {
		if strings.Contains(query, forbidden) {
			t.Errorf("占用判定 SQL 不应出现 %q：\n%s", forbidden, query)
		}
	}
}

// TestScheduleSnapshotQueryJoinsRequiredTables 时段快照必须一次读出时段->计划->医生->子科室，
// 内连接保证关联缺失（脏数据）的时段按「时段不存在」收敛，不会以空医生对象进入资格校验。
func TestScheduleSnapshotQueryJoinsRequiredTables(t *testing.T) {
	query := scheduleSnapshotQuery

	for _, fragment := range []string{
		"FROM hospital.doctor_work_plan_schedule s",
		"JOIN hospital.doctor_work_plan p ON p.id = s.work_plan_id",
		"JOIN hospital.doctor d ON d.id = p.doctor_id",
		"JOIN hospital.medical_dept_sub ds ON ds.id = p.dept_sub_id",
		"d.status = 1) AS doctor_active",
		"WHERE s.id = $1",
	} {
		if !strings.Contains(query, fragment) {
			t.Errorf("时段快照 SQL 缺少 %q：\n%s", fragment, query)
		}
	}
	if got := strings.Count(query, "$"); got != 1 {
		t.Errorf("占位符数量 = %d, want 1：\n%s", got, query)
	}
}

// TestRemainingCapacity 详情内嵌容量按时段行计算剩余量，且不允许出现负数。
func TestRemainingCapacity(t *testing.T) {
	cases := []struct {
		name          string
		maximum, used int16
		want          int16
	}{
		{"正常余量", 3, 1, 2},
		{"刚好占满", 3, 3, 0},
		{"超卖归零", 3, 5, 0},
		{"脏数据全为零", 0, 0, 0},
		{"最大值为零但已用不为零", 0, 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := remainingCapacity(tc.maximum, tc.used); got != tc.want {
				t.Errorf("remainingCapacity(%d,%d) = %d, want %d", tc.maximum, tc.used, got, tc.want)
			}
		})
	}
}

// TestRegistrationAmount 金额取 numeric 文本并统一为两位小数；无价目（NULL 或空串）
// 时按 "0.00" 返回，保证响应字段始终是可解析的十进制字符串。
func TestRegistrationAmount(t *testing.T) {
	cases := []struct {
		name string
		raw  sql.NullString
		want string
	}{
		{"NULL 按 0.00", sql.NullString{}, "0.00"},
		{"空白按 0.00", sql.NullString{String: "   ", Valid: true}, "0.00"},
		{"整数补两位小数", sql.NullString{String: "80", Valid: true}, "80.00"},
		{"一位小数补齐", sql.NullString{String: "80.5", Valid: true}, "80.50"},
		{"两位小数原样", sql.NullString{String: "80.00", Valid: true}, "80.00"},
		{"带空格的数值先 trim", sql.NullString{String: " 128.8 ", Valid: true}, "128.80"},
		{"非数值原样返回（不伪造金额）", sql.NullString{String: "abc", Valid: true}, "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := registrationAmount(tc.raw); got != tc.want {
				t.Errorf("registrationAmount(%+v) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
