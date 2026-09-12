package doctorpatient

import "testing"

// 本文件覆盖医生患者领域模型（internal/domain/doctorpatient/entity.go）中的纯函数：
// KeywordPattern 的 ILIKE 通配符转义与 Filter.Offset 的页码换算。
// 两者都是仓储层拼接 SQL 前的最后一道防线：转义缺失会让「按关键词过滤」静默退化成
// 全表匹配（契约 §6.10），页码换算缺失则会让漏校验的调用方产生负数 OFFSET。

// TestKeywordPatternEscapesLikeWildcards 覆盖 KeywordPattern 的通配符转义：
// %、_、\ 都按字面量处理，转义字符固定为反斜杠（与仓储 ILIKE ... ESCAPE '\' 配套）。
func TestKeywordPatternEscapesLikeWildcards(t *testing.T) {
	cases := []struct {
		name    string
		keyword string
		want    string
	}{
		{"普通中文姓名两侧补百分号", "张三", "%张三%"},
		{"电话号码两侧补百分号", "13800000000", "%13800000000%"},
		{"百分号按字面匹配而不是通配符", "10%", `%10\%%`},
		{"只有百分号的关键词", "%", `%\%%`},
		{"下划线按字面匹配而不是单字符通配符", "a_b", `%a\_b%`},
		{"只有下划线的关键词", "_", `%\_%`},
		{"反斜杠本身被转义", `\`, `%\\%`},
		{"反斜杠与百分号同时出现", `\%`, `%\\\%%`},
		{"通配符与中文混排", `_张%三\`, `%\_张\%三\\%`},
		{"空关键词返回空串且不补百分号", "", ""},
		{"只有空格的字符串不是空串（裁剪由请求层负责）", " ", "% %"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := KeywordPattern(tc.keyword); got != tc.want {
				t.Errorf("KeywordPattern(%q) = %q, want %q", tc.keyword, got, tc.want)
			}
		})
	}
}

// TestKeywordPatternEscapesBackslashBeforeWildcards 回归转义顺序：必须先转义反斜杠，
// 再转义通配符。若顺序颠倒，用户输入的 `\%` 会变成 `\\\\%`，其中的 % 仍是通配符。
func TestKeywordPatternEscapesBackslashBeforeWildcards(t *testing.T) {
	const keyword = `\%`
	const want = `%\\\%%`
	if got := KeywordPattern(keyword); got != want {
		t.Fatalf("KeywordPattern(%q) = %q, want %q", keyword, got, want)
	}
}

// TestSortConstantsMatchContract 断言排序白名单常量与契约 §6.10 的字面量一致；
// 请求层与仓储层都按这三个值分支，改动必须同步契约。
func TestSortConstantsMatchContract(t *testing.T) {
	cases := []struct {
		got  string
		want string
	}{
		{SortLastVisitDate, "lastVisitDate"},
		{SortName, "name"},
		{SortRegistrationCount, "registrationCount"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("排序常量 = %q, want %q", tc.got, tc.want)
		}
	}
}

// TestFilterOffset 覆盖页码到 SQL OFFSET 的换算：正常页按 (page-1)*pageSize 计算，
// 页码或页长非法（< 1，例如调用方漏校验）一律兜底为 0，绝不产生负数偏移。
func TestFilterOffset(t *testing.T) {
	cases := []struct {
		name     string
		page     int
		pageSize int
		want     int
	}{
		{"第一页偏移为 0", 1, 20, 0},
		{"第二页偏移等于页长", 2, 20, 20},
		{"第三页自定义页长", 3, 5, 10},
		{"大页码按页长累加", 100, 100, 9900},
		{"页码为 0 兜底为第一页", 0, 20, 0},
		{"页码为负数兜底为第一页", -3, 20, 0},
		{"页长为 0 兜底为第一页", 2, 0, 0},
		{"页长为负数兜底为第一页", 2, -1, 0},
		{"页码与页长都为 0", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter := Filter{Page: tc.page, PageSize: tc.pageSize}
			if got := filter.Offset(); got != tc.want {
				t.Errorf("Filter{Page: %d, PageSize: %d}.Offset() = %d, want %d",
					tc.page, tc.pageSize, got, tc.want)
			}
		})
	}
}
