// Package registration 实现挂号域用例：资格校验、创建挂号、列表与详情
// （spec/04-api-contract.md §6.1–§6.4、§8.2、§12.4）。
//
// 挂号是双 realm 共享业务接口：管理端令牌必须带 REGISTRATION:SELECT/WRITE 权限，
// 患者端令牌只能操作本人就诊卡名下的资源，越权与不存在统一按资源不存在处理
// （契约 §1.2）。本层负责就诊卡归属、患者状态、资料完整性、时段与占用判重等
// 业务规则；号源扣减与判重的原子性由 repository 在事务内保证。
package registration

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/domain/patient"
	domainpayment "Medical-Web-Backend/internal/domain/payment"
	domainregistration "Medical-Web-Backend/internal/domain/registration"
	"Medical-Web-Backend/internal/port"
)

// 业务错误码：handler 依据这些稳定编码映射 HTTP 响应（契约 §10 错误码目录）。
const (
	CodeValidationFailed     = "REQUEST_VALIDATION_FAILED"
	CodeCardNotFound         = "PATIENT_CARD_NOT_FOUND"
	CodeSlotNotFound         = "SCHEDULE_SLOT_NOT_FOUND"
	CodeSlotSoldOut          = "REGISTRATION_SLOT_SOLD_OUT"
	CodeDuplicate            = "REGISTRATION_DUPLICATE"
	CodeRegistrationNotFound = "REGISTRATION_NOT_FOUND"
	// CodeDependencyUnavailable 表示仓储（PostgreSQL）不可用，映射为 502 DEPENDENCY_UNAVAILABLE。
	CodeDependencyUnavailable = "DEPENDENCY_UNAVAILABLE"
	// CodePaymentProviderUnavailable 表示建单事务提交后的支付宝预下单或二维码回写失败：
	// 订单已按业务说明第 3.3 节执行整单补偿（删除订单 + 两级号源回退），映射为
	// 502 PAYMENT_PROVIDER_UNAVAILABLE，可重试且不返回二维码（契约 §6.2）。
	CodePaymentProviderUnavailable = "PAYMENT_PROVIDER_UNAVAILABLE"
)

// PaymentMethodAlipay 是本阶段唯一支持的支付方式（契约 §6.2、§6.5、§12.4）。
// 契约明确第一阶段只接受 ALIPAY，历史示例中的 WECHAT 等其它取值一律 422。
const PaymentMethodAlipay = "ALIPAY"

// 分页边界，取值来自契约 §1.4：page 从 1 开始、最大 100000，pageSize 最大 100。
// 请求层已按同一口径校验；用例层再兜底一次，避免其它调用方把超大窗口打到数据库。
const (
	maxPage     = 100000
	maxPageSize = 100
)

// ErrDependencyUnavailable 表示挂号仓储不可用（连接失败等），映射为
// 502 DEPENDENCY_UNAVAILABLE。依赖故障不能伪装成「挂号不存在」或「号源已满」，
// 否则客户端会把可重试的故障当成业务结论（契约 §10）。
var ErrDependencyUnavailable = errors.New("registration dependency is unavailable")

// ErrInvalidActor 表示调用者身份自相矛盾：声明为患者域却缺少当前患者标识。
//
// 这是内部调用错误（映射为 500），用于守住一条跨层隐式契约：仓储用
// ownerPatientID>0 作为「是否限定归属」的开关，属性一旦缺失就会静默关闭归属条件、
// 读到全量数据。与其依赖中间件始终填充 PatientID，这里显式拒绝（契约 §1.2：
// 患者端只能查看自己的记录）。
var ErrInvalidActor = errors.New("registration actor is not usable for authorization")

// Details 是可选的错误细节（仅非敏感字段），例如号源不足时的 scheduleId 与 remaining。
type Details map[string]any

// ServiceError 是 use case 返回给 handler 的业务错误，携带稳定 code 与面向用户的 message。
type ServiceError struct {
	Code    string
	Message string
	Details Details
}

func (e *ServiceError) Error() string { return e.Message }

// Actor 是本次调用的调用者身份。
//
// Realm 决定授权路径：patient 域的主体固定为令牌中的当前患者（PatientID），
// 只能访问本人就诊卡名下的资源；mis 域已由中间件完成权限编码校验，
// 这里只按资源存在性处理。UserID 仅用于排障，不参与授权判断。
type Actor struct {
	Realm     domainauth.Realm
	PatientID int64
	UserID    int64
}

// IsPatient 报告调用者是否来自患者域。
func (a Actor) IsPatient() bool { return a.Realm == domainauth.RealmPatient }

// ListQuery 是挂号列表用例入参（取值与格式已由请求层校验）。
type ListQuery struct {
	PatientCardID   *int64
	DoctorID        *int64
	SubdepartmentID *int64
	FromDate        string
	ToDate          string
	PaymentStatus   *int16
	Page            int
	PageSize        int
	Sort            string
	Order           string
}

// ListPage 是挂号列表用例结果：条目与分页元数据（契约统一列表结构）。
type ListPage struct {
	Items    []domainregistration.Registration
	Page     int
	PageSize int
	Total    int64
}

// CreateInput 是创建挂号的用例入参。
//
// PatientCardID 为 nil 表示请求未提交该字段：患者端可由服务端从当前患者推导
// （每个账号最多一张就诊卡，契约 §6.2、§8.2 第 6 步），管理端代建必须显式提交。
type CreateInput struct {
	PatientCardID *int64
	ScheduleID    int64
	PaymentMethod string
}

// CreateResult 是建单结果：挂号资源 + 同一次 HTTP 请求内完成的预下单支付参数（契约 §6.2）。
//
// 订单与预下单不再分离：建单事务提交后立即预下单，Payment 携带的 precreate_at、
// pay_deadline（+30 分钟）与 expire_at（+35 分钟）就是建单 INSERT 写定的一组时间点，
// 二维码（PrepayID）与订单有效期因此同起点（创建订单与支付业务说明.md 第 2、3 节）。
type CreateResult struct {
	Registration domainregistration.Registration
	Payment      domainpayment.Payment
}

// Service 编排挂号域用例。cards 提供就诊卡归属判定，patients 提供持卡账号状态，
// repo 提供挂号读写与事务内的号源扣减，precreator 提供建单后的支付宝预下单能力。
type Service struct {
	repo       port.RegistrationRepository
	cards      port.PatientCardRepository
	patients   port.PatientUserRepository
	precreator port.PaymentPrecreator
	// now 可注入，用于确定性地验证时段是否已开始与占用判定。
	now func() time.Time
	// newTradeNo 生成外部交易号，可注入以便测试固化格式。
	newTradeNo func(time.Time) string
}

// NewService 构造挂号用例；now 为空时回退到当前 UTC 时间。
//
// precreator 由 usecase/payment 实现（port.PaymentPrecreator），建单成功提交后立即调用；
// 缺失时建单直接失败，不允许降级为「先建单、稍后再取码」（业务说明第 3.3 节）。
func NewService(
	repo port.RegistrationRepository,
	cards port.PatientCardRepository,
	patients port.PatientUserRepository,
	precreator port.PaymentPrecreator,
	now func() time.Time,
) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		repo:       repo,
		cards:      cards,
		patients:   patients,
		precreator: precreator,
		now:        now,
		newTradeNo: domainregistration.NewOutTradeNo,
	}
}

// Eligibility 校验当前患者（或管理端代查的就诊卡）能否挂指定时段（契约 §6.1）。
//
// 资格不通过不是错误：返回领域结果并把原因写进 reasons 词表，由 handler 输出 200。
// reasons 按契约 §6.1 词表顺序追加，保证同一输入下结果稳定可比。
func (s *Service) Eligibility(
	ctx context.Context,
	actor Actor,
	cardID int64,
	scheduleID int64,
) (*domainregistration.Eligibility, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := s.requireUsableActor(actor); err != nil {
		return nil, err
	}
	if cardID < 1 {
		return nil, validationError("patientCardId 必须为正整数")
	}
	if scheduleID < 1 {
		return nil, validationError("scheduleId 必须为正整数")
	}

	now := s.now()
	card, err := s.cards.FindCardByID(ctx, cardID)
	if errors.Is(err, patient.ErrCardNotFound) || (err == nil && card == nil) {
		return domainregistration.NewEligibility(0, nil, []string{domainregistration.ReasonCardInvalid}), nil
	}
	if err != nil {
		return nil, dataError(err)
	}
	// 患者端只能校验自己名下的就诊卡：他人卡与不存在的卡在资格校验里同样是
	// PATIENT_CARD_INVALID（词表原文「就诊卡不存在或不属于当前患者」），不按 404 处理。
	if actor.IsPatient() && !card.BelongsTo(actor.PatientID) {
		return domainregistration.NewEligibility(0, nil, []string{domainregistration.ReasonCardInvalid}), nil
	}

	reasons := make([]string, 0, 4)
	if !domainregistration.ProfileComplete(*card) {
		reasons = append(reasons, domainregistration.ReasonProfileIncomplete)
	}
	owner, err := s.patients.FindPatientByID(ctx, card.UserID)
	switch {
	case errors.Is(err, patient.ErrPatientNotFound), err == nil && owner == nil:
		// 持卡账号不存在属于脏数据：就诊卡不可用，按词表首项收敛。
		return domainregistration.NewEligibility(0, nil, []string{domainregistration.ReasonCardInvalid}), nil
	case err != nil:
		return nil, dataError(err)
	case !owner.IsActive():
		reasons = append(reasons, domainregistration.ReasonPatientDisabled)
	}

	// 时段有效性：时段/关联不存在与医生不在诊都按 SCHEDULE_NOT_FOUND 收敛——
	// 词表没有「医生停诊」原因，而非在岗医生的号源在公开域本就不可见，
	// 对前端而言与「时段不存在」等价。
	snapshot, err := s.repo.FindScheduleSnapshot(ctx, scheduleID)
	if errors.Is(err, domainregistration.ErrScheduleNotFound) {
		return domainregistration.NewEligibility(0, nil, append(reasons, domainregistration.ReasonScheduleNotFound)), nil
	}
	if err != nil {
		return nil, dataError(err)
	}
	amount := snapshot.Amount
	remaining := snapshot.Remaining()
	if !snapshot.DoctorActive {
		return domainregistration.NewEligibility(
			remaining, &amount,
			append(reasons, domainregistration.ReasonScheduleNotFound)), nil
	}
	if domainregistration.Started(snapshot.Date, now) {
		reasons = append(reasons, domainregistration.ReasonScheduleStarted)
	}
	if remaining <= 0 {
		reasons = append(reasons, domainregistration.ReasonSoldOut)
	}

	// 同一身份证号在同一时段已有占用中的挂号时不可再挂；EXPIRED/REFUNDED 不占用
	// （契约 §6.1、§6.3）。
	occupied, err := s.repo.HasOccupyingRegistration(ctx, card.PID, scheduleID, now)
	if err != nil {
		return nil, dataError(err)
	}
	if occupied {
		reasons = append(reasons, domainregistration.ReasonDuplicate)
	}

	return domainregistration.NewEligibility(remaining, &amount, reasons), nil
}

// Create 创建挂号并即时完成支付预下单（契约 §6.2）。幂等由 handler 的幂等键协调完成，
// 本层负责业务规则：归属、患者状态、时段资格、占用判重、号源扣减，以及在**建单事务提交后**
// 于同一个 HTTP 请求内调用支付宝预下单并回写二维码（创建订单与支付业务说明.md 第 3.1 节）。
//
// 事务边界：预下单是外部网络调用，必须发生在数据库事务之外，否则占号行的并发挂号会被串行化、
// 事务时长也会绑到支付宝的尾延迟上；预下单或二维码回写失败时执行整单补偿（删除订单 +
// 两级号源回退）并返回 502 PAYMENT_PROVIDER_UNAVAILABLE，不向客户端返回二维码
// （业务说明第 3.2、3.3 节）。
func (s *Service) Create(
	ctx context.Context,
	actor Actor,
	input CreateInput,
) (*CreateResult, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := s.requireUsableActor(actor); err != nil {
		return nil, err
	}
	if input.ScheduleID < 1 {
		return nil, validationError("scheduleId 必须为正整数")
	}
	if err := validatePaymentMethod(input.PaymentMethod); err != nil {
		return nil, err
	}
	// 支付能力缺失属于启动期装配错误：预下单不可能成功，直接返回 502，不建单也不占号，
	// 避免留下「占用号源但没有二维码」的订单（业务说明第 3.3 节）。
	// 放在入参校验之后，保证非法入参仍优先返回 422。
	if s.precreator == nil {
		return nil, paymentUnavailableError()
	}

	now := s.now()
	card, err := s.resolveCard(ctx, actor, input.PatientCardID)
	if err != nil {
		return nil, err
	}
	if err := s.ensureBookablePatient(ctx, card); err != nil {
		return nil, err
	}
	if _, err := s.requireBookableSchedule(ctx, card, input.ScheduleID, now); err != nil {
		return nil, err
	}

	created, err := s.repo.CreateRegistration(ctx, domainregistration.CreateInput{
		PatientCardID: card.ID,
		PID:           card.PID,
		ScheduleID:    input.ScheduleID,
		OutTradeNo:    s.newTradeNo(now),
		Now:           now,
	})
	if err != nil {
		return nil, s.createError(err, input.ScheduleID)
	}
	// 建单事务已提交，随后在同一 HTTP 请求内完成预下单（业务说明第 3.1 节第 5~6 步）。
	pay, err := s.precreateForOrder(ctx, created)
	if err != nil {
		return nil, err
	}
	return &CreateResult{Registration: *created, Payment: *pay}, nil
}

// precreateForOrder 在建单事务提交后执行预下单；任何失败都按业务说明第 3.3 节执行整单补偿，
// 并统一映射为 502 PAYMENT_PROVIDER_UNAVAILABLE（契约 §6.2）。
//
// 允许删除订单记录的前提是二维码从未交付给客户端：本函数只在二维码确实落库时才返回成功，
// 因此失败路径上不存在「用户可能已付款、记录却已被删除」的情况。补偿本身失败必须告警，
// 不允许静默留下「占用号源但没有订单」的状态。
func (s *Service) precreateForOrder(
	ctx context.Context,
	created *domainregistration.Registration,
) (*domainpayment.Payment, error) {
	pay, err := s.precreator.PrecreateForOrder(ctx, created.ID)
	switch {
	case err != nil:
		log.Printf("告警：建单预下单失败，执行整单补偿 registration_id=%d out_trade_no=%s: %v",
			created.ID, created.OutTradeNo, err)
	case pay == nil || strings.TrimSpace(pay.PrepayID) == "":
		// 拿不到二维码就不能交付 201：没有支付凭证的建单必须整体撤销。
		log.Printf("告警：建单预下单未取得二维码，执行整单补偿 registration_id=%d out_trade_no=%s",
			created.ID, created.OutTradeNo)
	default:
		return pay, nil
	}
	if compErr := s.repo.CompensateRegistration(ctx, created.ID); compErr != nil {
		log.Printf("告警：建单整单补偿失败，可能残留占用号源的订单 registration_id=%d out_trade_no=%s: %v",
			created.ID, created.OutTradeNo, compErr)
	}
	return nil, paymentUnavailableError()
}

// List 返回挂号分页列表（契约 §6.3）：管理端按过滤条件查询，患者端只返回本人记录。
func (s *Service) List(ctx context.Context, actor Actor, q ListQuery) (*ListPage, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := s.requireUsableActor(actor); err != nil {
		return nil, err
	}
	if q.Page < 1 || q.Page > maxPage || q.PageSize < 1 || q.PageSize > maxPageSize {
		return nil, validationError("分页参数不正确")
	}
	if q.FromDate != "" && q.ToDate != "" && q.FromDate > q.ToDate {
		return nil, validationError("日期范围无效")
	}
	switch q.Sort {
	case "", domainregistration.SortCreateDate, domainregistration.SortDate, domainregistration.SortID:
	default:
		return nil, validationError("sort 只支持 createDate/date/id")
	}
	switch q.Order {
	case "", "asc", "desc":
	default:
		return nil, validationError("order 只支持 asc/desc")
	}

	filter := domainregistration.Filter{
		PatientCardID:   q.PatientCardID,
		DoctorID:        q.DoctorID,
		SubdepartmentID: q.SubdepartmentID,
		FromDate:        q.FromDate,
		ToDate:          q.ToDate,
		PaymentStatus:   q.PaymentStatus,
		Sort:            q.Sort,
		Order:           q.Order,
	}
	if actor.IsPatient() {
		// 患者端即使提交了 patientCardId 也必须是本人名下的卡：他人卡按资源不存在处理，
		// 不返回 403，避免用状态码枚举他人卡号（契约 §1.2）。
		if q.PatientCardID != nil {
			if _, err := s.ensureOwnedCard(ctx, actor, *q.PatientCardID); err != nil {
				return nil, err
			}
		}
		owner := actor.PatientID
		filter.OwnerPatientID = &owner
	}

	items, total, err := s.repo.ListRegistrations(
		ctx, filter, (q.Page-1)*q.PageSize, q.PageSize)
	if err != nil {
		return nil, dataError(err)
	}
	if items == nil {
		// 空列表仍返回 200 与 items: []，不能输出 null（契约 §1.4）。
		items = make([]domainregistration.Registration, 0)
	}
	return &ListPage{Items: items, Page: q.Page, PageSize: q.PageSize, Total: total}, nil
}

// Detail 返回挂号详情（契约 §6.4）：患者端只允许本人记录，越权与不存在统一 404。
func (s *Service) Detail(
	ctx context.Context,
	actor Actor,
	registrationID int64,
) (*domainregistration.Detail, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := s.requireUsableActor(actor); err != nil {
		return nil, err
	}
	if registrationID < 1 {
		return nil, validationError("挂号记录编号必须为正整数")
	}
	// ownerPatientID=0 表示管理端不限定归属；患者端由 repository 在同一查询内加归属条件，
	// 使「他人记录」与「记录不存在」返回同一个错误，无法据此枚举患者数据。
	ownerPatientID := int64(0)
	if actor.IsPatient() {
		ownerPatientID = actor.PatientID
	}
	detail, err := s.repo.FindRegistrationDetail(ctx, registrationID, ownerPatientID)
	if errors.Is(err, domainregistration.ErrRegistrationNotFound) {
		return nil, registrationNotFoundError()
	}
	if err != nil {
		return nil, dataError(err)
	}
	return detail, nil
}

// ready 校验用例依赖是否齐备；缺失属于启动期配置错误。
func (s *Service) ready() error {
	if s == nil || s.repo == nil || s.cards == nil || s.patients == nil {
		return errors.New("挂号用例依赖未配置")
	}
	return nil
}

// requireUsableActor 校验调用者身份自洽，防止归属条件被静默跳过（见 ErrInvalidActor）。
func (s *Service) requireUsableActor(actor Actor) error {
	if actor.IsPatient() && actor.PatientID < 1 {
		return ErrInvalidActor
	}
	return nil
}

// resolveCard 定位本次挂号使用的就诊卡，并按 realm 完成归属校验。
func (s *Service) resolveCard(
	ctx context.Context,
	actor Actor,
	requested *int64,
) (*patient.Card, error) {
	if requested != nil && *requested < 1 {
		return nil, validationError("patientCardId 必须为正整数")
	}
	if actor.IsPatient() {
		if requested == nil {
			// 患者端可省略 patientCardId：每个账号最多一张卡，由服务端推导。
			card, err := s.cards.FindCardByPatientID(ctx, actor.PatientID)
			if errors.Is(err, patient.ErrCardNotFound) || (err == nil && card == nil) {
				return nil, cardNotFoundError("当前账号尚未建立就诊卡")
			}
			if err != nil {
				return nil, dataError(err)
			}
			return card, nil
		}
		return s.ensureOwnedCard(ctx, actor, *requested)
	}

	// 管理端代建必须显式提交 patientCardId（契约 §6.2：缺失返回 422）。
	if requested == nil {
		return nil, validationError("patientCardId 必填")
	}
	card, err := s.cards.FindCardByID(ctx, *requested)
	if errors.Is(err, patient.ErrCardNotFound) || (err == nil && card == nil) {
		return nil, cardNotFoundError("就诊卡不存在")
	}
	if err != nil {
		return nil, dataError(err)
	}
	return card, nil
}

// ensureOwnedCard 读取就诊卡并判定归属；他人卡与不存在的卡统一 404（契约 §1.2、§6.2）。
func (s *Service) ensureOwnedCard(
	ctx context.Context,
	actor Actor,
	cardID int64,
) (*patient.Card, error) {
	card, err := s.cards.FindCardByID(ctx, cardID)
	if errors.Is(err, patient.ErrCardNotFound) || (err == nil && card == nil) {
		return nil, cardNotFoundError("就诊卡不存在")
	}
	if err != nil {
		return nil, dataError(err)
	}
	if !card.BelongsTo(actor.PatientID) {
		return nil, cardNotFoundError("就诊卡不存在")
	}
	return card, nil
}

// ensureBookablePatient 校验持卡账号可用于挂号：账号存在、未被禁用、资料完整。
// 词表里的 PATIENT_DISABLED / PATIENT_PROFILE_INCOMPLETE 只用于资格校验的 reasons，
// 建单时按契约 §10「字段校验或资格失败」返回 422 REQUEST_VALIDATION_FAILED。
func (s *Service) ensureBookablePatient(ctx context.Context, card *patient.Card) error {
	owner, err := s.patients.FindPatientByID(ctx, card.UserID)
	switch {
	case errors.Is(err, patient.ErrPatientNotFound), err == nil && owner == nil:
		return cardNotFoundError("就诊卡不存在")
	case err != nil:
		return dataError(err)
	case !owner.IsActive():
		return validationError("患者账号已被禁用，无法挂号")
	}
	if !domainregistration.ProfileComplete(*card) {
		return validationError("就诊卡资料不完整，请先补全实名信息")
	}
	return nil
}

// requireBookableSchedule 校验时段当前可挂，并做占用判重。
// 返回的快照仅用于日志与后续扩展，建单金额与关联仍由 repository 在事务内重读。
func (s *Service) requireBookableSchedule(
	ctx context.Context,
	card *patient.Card,
	scheduleID int64,
	now time.Time,
) (*domainregistration.ScheduleSnapshot, error) {
	snapshot, err := s.repo.FindScheduleSnapshot(ctx, scheduleID)
	if errors.Is(err, domainregistration.ErrScheduleNotFound) {
		return nil, slotNotFoundError()
	}
	if err != nil {
		return nil, dataError(err)
	}
	// 医生不在诊或时段已开始：时段不可挂号，按「不存在」收敛（契约 §6.1 词表口径）。
	if !snapshot.DoctorActive || domainregistration.Started(snapshot.Date, now) {
		return nil, slotNotFoundError()
	}
	if snapshot.Remaining() <= 0 {
		return nil, soldOutError(scheduleID, 0)
	}
	occupied, err := s.repo.HasOccupyingRegistration(ctx, card.PID, scheduleID, now)
	if err != nil {
		return nil, dataError(err)
	}
	if occupied {
		return nil, duplicateError()
	}
	return snapshot, nil
}

// createError 把建单事务返回的领域错误映射为契约错误码。
// 事务内的判定是最终判定：并发下先通过前置检查、后被其它请求抢占号源时，
// 仍由这里给出 409 而不是 500（契约 §11 挂号域必测「最后一个号源并发竞争」）。
func (s *Service) createError(err error, scheduleID int64) error {
	var soldOut *domainregistration.SlotSoldOutError
	if errors.As(err, &soldOut) {
		id := soldOut.ScheduleID
		if id < 1 {
			id = scheduleID
		}
		return soldOutError(id, soldOut.Remaining)
	}
	switch {
	case errors.Is(err, domainregistration.ErrDuplicate):
		return duplicateError()
	case errors.Is(err, domainregistration.ErrScheduleNotFound),
		errors.Is(err, domainregistration.ErrScheduleStarted),
		errors.Is(err, domainregistration.ErrDoctorInactive):
		return slotNotFoundError()
	case errors.Is(err, domainregistration.ErrCardNotFound):
		return cardNotFoundError("就诊卡不存在")
	default:
		return dataError(err)
	}
}

// validatePaymentMethod 校验支付方式：本阶段只支持 ALIPAY（契约 §6.2、§12.4）。
//
// 这里是精确匹配，不做大小写归一化：契约写的是「其他取值（含历史示例中的 WECHAT）
// 一律返回 422」，与本仓库其它枚举入参（paymentStatus/sort/order 均为精确匹配）
// 保持同一口径，避免把 "alipay" 之类的非契约取值静默放行。
func validatePaymentMethod(method string) error {
	if method == "" {
		return validationError("paymentMethod 必填")
	}
	if method != PaymentMethodAlipay {
		return validationError("暂不支持该支付方式")
	}
	return nil
}

// dataError 把仓储错误归类为依赖不可用（502）；领域错误由调用方先行处理，
// 走到这里说明是未预期的存储层故障。
func dataError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrDependencyUnavailable, err)
}

func validationError(message string) error {
	return &ServiceError{Code: CodeValidationFailed, Message: message}
}

func cardNotFoundError(message string) error {
	return &ServiceError{Code: CodeCardNotFound, Message: message}
}

func slotNotFoundError() error {
	return &ServiceError{Code: CodeSlotNotFound, Message: "时段不存在或已不可挂号"}
}

func soldOutError(scheduleID int64, remaining int16) error {
	return &ServiceError{
		Code:    CodeSlotSoldOut,
		Message: "号源已满",
		Details: Details{"scheduleId": scheduleID, "remaining": remaining},
	}
}

func duplicateError() error {
	return &ServiceError{Code: CodeDuplicate, Message: "同一身份证号已挂该时段"}
}

func registrationNotFoundError() error {
	return &ServiceError{Code: CodeRegistrationNotFound, Message: "挂号记录不存在"}
}

// paymentUnavailableError 是建单预下单失败后的统一对外错误（契约 §6.2）：订单已完成整单补偿，
// 用户重新发起挂号会生成新的 out_trade_no，因此文案引导重新挂号；支付宝 sub_code 与 provider
// 原文只进日志，绝不出现在响应里。
func paymentUnavailableError() error {
	return &ServiceError{
		Code:    CodePaymentProviderUnavailable,
		Message: "支付渠道暂时不可用，请稍后重新发起挂号",
	}
}
