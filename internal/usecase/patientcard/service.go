// Package patientcard 实现患者端就诊卡的用例：列表、详情、创建与修改
// （spec/04-api-contract.md §7.4、§12.5）。
//
// 就诊卡的主体只能是当前登录患者本人：所有读写都以访问令牌中的患者主键判定归属，
// URL 中的 cardId 只用于定位资源。他人就诊卡与不存在的就诊卡统一返回
// patient.ErrCardNotFound，避免调用方通过状态码枚举他人卡号。
package patientcard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"Medical-Web-Backend/internal/domain/patient"
	"Medical-Web-Backend/internal/port"
)

// ErrDependencyUnavailable 表示就诊卡仓储（PostgreSQL）不可用，映射为
// 502 DEPENDENCY_UNAVAILABLE。依赖故障不能伪装成「就诊卡不存在」，
// 否则客户端会把可重试的故障当成数据问题（spec/04-api-contract.md §10）。
var ErrDependencyUnavailable = errors.New("patient card dependency is unavailable")

// 分页边界，取值来自契约 §1.4：page 从 1 开始、最大 100000，pageSize 最大 100。
// 请求层已按同一口径校验；用例层再兜底一次，避免其它调用方把超大窗口直接打到数据库。
const (
	maxPage     = 100000
	maxPageSize = 100
)

// CardPage 是就诊卡列表用例结果：条目与分页元数据（契约统一列表结构）。
type CardPage struct {
	Items    []patient.Card
	Page     int
	PageSize int
	Total    int64
}

// Service 是就诊卡用例，只服务 realm=patient 的患者本人。
type Service struct {
	cards port.PatientCardRepository
	// now 可注入，用于确定性地验证「出生日期不得晚于今天」的建卡规则。
	now func() time.Time
}

// NewService 构造就诊卡用例；cards 提供就诊卡读写能力。
func NewService(cards port.PatientCardRepository) *Service {
	return &Service{cards: cards, now: time.Now}
}

// ListCards 返回当前患者名下的就诊卡分页列表（契约 §7.4、§12.5）。
//
// page/pageSize 按契约 §1.4 校验（page 从 1 开始、最大 100000，pageSize 最大 100）
// 后再换算 offset/limit：越界时不触达仓储，直接返回 patient.ErrInvalidPagination，
// 由 handler 映射为 422——绝不把 PostgreSQL 的 2201W/2201X 原始错误变成 500。
func (s *Service) ListCards(
	ctx context.Context,
	patientID int64,
	page, pageSize int,
) (*CardPage, error) {
	if s == nil || s.cards == nil || patientID <= 0 {
		// 主体缺失只可能来自伪造或被绕过的令牌载荷，按「账号不存在」收敛。
		return nil, patient.ErrPatientNotFound
	}
	if page < 1 || page > maxPage || pageSize < 1 || pageSize > maxPageSize {
		// 非法分页窗口不触达仓储：返回领域错误，由 handler 映射为 422，
		// 而不是让 PostgreSQL 的 2201W/2201X 原始错误冒泡成 500。
		return nil, patient.ErrInvalidPagination
	}

	items, total, err := s.cards.ListCardsByPatient(
		ctx,
		patientID,
		(page-1)*pageSize,
		pageSize,
	)
	if err != nil {
		return nil, cardDataError(err)
	}
	if items == nil {
		// 空列表仍返回 200 与 items: []，不能输出 null（契约 §1.4）。
		items = make([]patient.Card, 0)
	}

	return &CardPage{Items: items, Page: page, PageSize: pageSize, Total: total}, nil
}

// GetCard 返回当前患者名下指定就诊卡的详情，pid 脱敏、tel 明文（契约 §7.4）。
// 他人就诊卡与不存在的卡统一返回 patient.ErrCardNotFound（404）。
func (s *Service) GetCard(ctx context.Context, patientID, cardID int64) (*patient.Card, error) {
	if s == nil || s.cards == nil || patientID <= 0 {
		return nil, patient.ErrPatientNotFound
	}
	return s.findOwnedCard(ctx, patientID, cardID)
}

// CreateCard 创建当前患者的就诊卡（契约 §7.4：一次性提交全部必填字段）。
//
// 字段级校验全部交给 domain 的 patient.NewCard：它一次性收集所有问题字段，
// 输出 details.fields 需要的字段名，出生日期由身份证号推导、不接受客户端提交。
// 每个账号只允许一张卡，唯一性由 repository 在事务内串行化保证，已有卡时
// 返回 patient.ErrCardExists（409 PATIENT_CARD_EXISTS）。
func (s *Service) CreateCard(
	ctx context.Context,
	patientID int64,
	input patient.CardInput,
) (*patient.Card, error) {
	if s == nil || s.cards == nil || patientID <= 0 {
		return nil, patient.ErrPatientNotFound
	}

	card, err := patient.NewCard(patientID, "", input, s.now())
	if err != nil {
		// 字段级校验错误原样上抛：handler 用 patient.FieldsOf 组装 details.fields。
		return nil, err
	}

	created, err := s.cards.CreateCard(ctx, card)
	if err != nil {
		return nil, cardDataError(err)
	}
	return created, nil
}

// UpdateCard 修改当前患者名下指定就诊卡的允许字段（契约 §7.4、§12.5）。
//
// 顺序固定为「先归属校验，再写入」：repository 只按主键操作，不替上层判断
// 归属，因此他人卡必须在进入写入前就被拦成 404。就诊卡 PATCH 不使用 If-Match，
// 防重复写入依靠 repository 在事务内按主键串行化。
func (s *Service) UpdateCard(
	ctx context.Context,
	patientID, cardID int64,
	update patient.CardUpdate,
) (*patient.Card, error) {
	if s == nil || s.cards == nil || patientID <= 0 {
		return nil, patient.ErrPatientNotFound
	}

	if _, err := s.findOwnedCard(ctx, patientID, cardID); err != nil {
		return nil, err
	}

	updated, err := s.cards.UpdateCard(ctx, cardID, update)
	if err != nil {
		return nil, cardDataError(err)
	}
	return updated, nil
}

// findOwnedCard 读取就诊卡并判定归属；不存在与他人卡统一返回 ErrCardNotFound。
func (s *Service) findOwnedCard(
	ctx context.Context,
	patientID, cardID int64,
) (*patient.Card, error) {
	card, err := s.cards.FindCardByID(ctx, cardID)
	if err != nil {
		return nil, cardDataError(err)
	}
	if card == nil || !card.BelongsTo(patientID) {
		// 契约要求他人就诊卡与不存在统一处理，不能暴露卡是否存在。
		return nil, patient.ErrCardNotFound
	}
	return card, nil
}

// cardDataError 把仓储错误翻译为上层可直接映射的语义：领域错误（卡不存在、
// 卡已存在、字段校验、分页窗口非法）原样上抛，其余归类为依赖不可用（502）。
func cardDataError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, patient.ErrCardNotFound),
		errors.Is(err, patient.ErrCardExists),
		errors.Is(err, patient.ErrInvalidPagination),
		isFieldValidation(err):
		return err
	}
	// 第二个 %w 保留原因错误链：调用方既能识别 ErrDependencyUnavailable（→502），
	// 日志里也不会丢掉原始根因。
	return fmt.Errorf("%w: %w", ErrDependencyUnavailable, err)
}

// isFieldValidation 判断错误链上是否存在就诊卡的字段级校验错误。
// 这类错误由 domain 的 *patient.FieldError 承载，handler 据此返回 422 与 details.fields。
func isFieldValidation(err error) bool {
	var fieldErr *patient.FieldError
	return errors.As(err, &fieldErr)
}
