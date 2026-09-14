package port

import "context"

// ScheduleCacheVersioner 是「排班写路径 → 公开排班读缓存」之间的失效契约（T4b）。
//
// 为什么用版本号而不是删键：公开时段查询的键由「子科室/医生 + 日期窗口 + 页码」组合而成，
// 一次写操作要失效的键数量不可枚举；用版本号只需一次 INCR，旧键自然失效，
// 避免 SCAN 全库（生产环境 SCAN 会阻塞 Redis）。
//
// 为什么是「子科室 + 医生」双维度而不是只按子科室：
// 公开时段查询允许只按 doctorId 过滤（契约 §8.1），这类缓存键里没有子科室维度，
// 因此写路径必须同时递增其影响到的医生维度，否则「按医生查排班」会读到脏余量。
// 传 0 表示该维度未知或不适用。**任一维度未知都会退化为全局失效**（实现见 repo.ScheduleCache），
// 因为只递增一个维度会漏掉「只按另一维度查询」的缓存键；调用方若能拿到两个维度就应完整传入，
// 拿不到就传 0 让它全量失效——多一次回源，换绝不漏失效。
//
// 失效失败不影响业务：调用方只记录日志，写操作已经提交，缓存是性能依赖而不是正确性依赖。
type ScheduleCacheVersioner interface {
	BumpSchedules(ctx context.Context, subdepartmentID, doctorID int64) error
}
