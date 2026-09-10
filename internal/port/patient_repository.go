package port

import (
	"context"
	"time"

	"Medical-Web-Backend/internal/domain/patient"
)

// PatientUserRepository 描述患者账号（patient_user）的持久化能力。
//
// openId 只在服务端内部流转，因此本接口的读写都以 openId 为登录标识、
// 以患者主键为业务标识，不提供任何“按昵称/手机号查询患者”的能力。
type PatientUserRepository interface {
	// FindPatientByID 按主键读取患者账号；不存在时返回 patient.ErrPatientNotFound。
	FindPatientByID(ctx context.Context, patientID int64) (*patient.Patient, error)
	// FindOrCreatePatientByOpenID 实现“登录与注册合一”的持久化部分：
	// 按 openId 查找 patient_user，不存在则以 status=1 注册后再返回。
	//
	// 返回值的第二个结果表示本次调用是否真正创建了新账号，用于响应中的
	// isNewUser 字段。openId 为空时返回 patient.ErrOpenIDRequired。
	//
	// patient_user.open_id 在库中只有普通索引、没有唯一约束，无法依赖数据库
	// 兜底防重，因此实现必须在事务内对 openId 串行化：同一 openId 的并发调用
	// 只能有一个创建，其余调用复用同一个账号。
	FindOrCreatePatientByOpenID(ctx context.Context, openID string, now time.Time) (*patient.Patient, bool, error)
}

// PatientCardRepository 描述就诊卡（patient_user_info_card）与人脸认证记录
// （patient_face_auth）的持久化能力。
//
// 所有“按账号取卡”的读取都返回 patient.ErrCardNotFound，让上层统一映射为
// 404 PATIENT_CARD_NOT_FOUND；归属判定由上层用 patient.Card.BelongsTo 完成，
// repository 不替上层决定“是否允许访问他人就诊卡”。
type PatientCardRepository interface {
	// ListCardsByPatient 按 user_id 分页返回就诊卡，按 id 升序，返回总数。
	//
	// 调用方必须先按契约 §1.4 校验 page >= 1、pageSize <= 100 再换算 offset/limit；
	// offset < 0 或 limit < 1 时返回 patient.ErrInvalidPagination，由上层映射为
	// 422 REQUEST_VALIDATION_FAILED，不能让 PostgreSQL 的 2201W/2201X 原始错误
	// 冒泡成 500。
	ListCardsByPatient(ctx context.Context, patientID int64, offset, limit int) ([]patient.Card, int64, error)
	// FindCardByID 按主键读取就诊卡；不存在返回 patient.ErrCardNotFound。
	FindCardByID(ctx context.Context, cardID int64) (*patient.Card, error)
	// FindCardByPatientID 读取账号名下唯一的就诊卡；没有卡时返回
	// patient.ErrCardNotFound。
	//
	// /api/v1/patient/me 的 cardCount 必须用它推导（只可能是 0 或 1），
	// 不能用 ListCardsByPatient 的 total：库中 user_id 上没有唯一约束，
	// total > 1 属于脏数据，应记录数据质量告警而不是透出给客户端。
	FindCardByPatientID(ctx context.Context, patientID int64) (*patient.Card, error)
	// CreateCard 创建就诊卡：以 user_id 为互斥对象在事务内串行化，
	// 该账号已有卡时返回 patient.ErrCardExists（409 PATIENT_CARD_EXISTS）。
	// card 必须已经过 patient.NewCard 校验，repository 只负责落库与唯一性。
	CreateCard(ctx context.Context, card patient.Card) (*patient.Card, error)
	// UpdateCard 在事务内按主键 FOR UPDATE 重读后应用 update，返回更新后的卡。
	//
	// 调用方必须先 FindCardByID 并用 patient.Card.BelongsTo 完成归属校验，
	// 本方法只按主键操作，不替上层判断“这张卡是否属于当前患者”。
	//
	// 就诊卡 PATCH 不使用 If-Match（契约 §1.5），防重复写入依靠事务内串行化：
	// 只有允许修改的列会出现在 UPDATE 语句里，身份证号、出生日期、user_id
	// 与 uuid 在 SQL 层面就不参与更新。卡不存在返回 patient.ErrCardNotFound，
	// 提交了不可修改字段或非法取值时返回对应的领域错误且不写入。
	UpdateCard(ctx context.Context, cardID int64, update patient.CardUpdate) (*patient.Card, error)
	// ListFaceAuthByCard 返回就诊卡的人脸认证日期记录，按日期倒序（最近的在前）；
	// 卡不存在同样返回 patient.ErrCardNotFound，便于上层区分 404 与空列表。
	//
	// 与 UpdateCard 一样，调用方必须先完成就诊卡归属校验。
	//
	// 本阶段只读取日期，不上传也不识别人脸模型。
	ListFaceAuthByCard(ctx context.Context, cardID int64) ([]patient.FaceAuthRecord, error)
}

// PatientRepository 组合患者账号与就诊卡的全部持久化读写能力，
// 供患者域 use case 依赖：同一实现需同时满足 Slice 4 的会话与就诊卡接口。
type PatientRepository interface {
	PatientUserRepository
	PatientCardRepository
}
