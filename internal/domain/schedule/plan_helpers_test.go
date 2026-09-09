package schedule

import (
	"testing"
)

// TestPlanFilterDefaults 计划过滤条件默认值只存在于上层绑定层；
// 领域层 PlanFilter 以零值存在，不隐含业务默认。
func TestPlanFilterZeroValue(t *testing.T) {
	var f PlanFilter
	if f.Sort != "" || f.Order != "" || f.IncludeSlots {
		t.Errorf("PlanFilter 零值应全空，got %+v", f)
	}
}
