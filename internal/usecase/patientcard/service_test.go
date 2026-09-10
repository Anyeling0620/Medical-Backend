// 就诊卡用例层单测：列表、详情、创建与修改的领域规则与错误归类
// （spec/04-api-contract.md §1.4、§7.4、§10、§12.5）。
//
// 全部用例使用内存桩替换 port.PatientCardRepository，不依赖 PostgreSQL/Redis。
package patientcard

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"Medical-Web-Backend/internal/domain/patient"
	"Medical-Web-Backend/internal/port"
)

const (
	// cardTestPIDMale 是 1990-01-01 出生、第 17 位为 3（奇，男）的合法号码。
	cardTestPIDMale = "110101199001011237"
	// cardTestPIDFemale 是 1990-01-01 出生、第 17 位为 0（偶，女）的合法号码。
	cardTestPIDFemale = "110101199001011202"
	// cardTestPIDFuture 是 2099-01-01 出生、校验位正确的号码，
	// 用于验证「出生日期不得晚于业务当前日期」。
	cardTestPIDFuture = "11010120990101013X"
)

// cardTestNow 是注入 Service 的固定业务时刻，保证出生日期推导用例可重复。
var cardTestNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// cardFakeRepo 是内存版 port.PatientCardRepository 桩。
//
// 只实现用例真正调用的四个方法，其余方法由内嵌接口提供（被调用会 panic，
// 正是「用例不该触达它们」的哨兵）。
type cardFakeRepo struct {
	port.PatientCardRepository

	cards  map[int64]patient.Card
	nextID int64

	// 调用计数：用于断言越权分支不得触达仓储的写路径。
	listCalls   int
	findCalls   int
	createCalls int
	updateCalls int

	// lastOffset/lastLimit 记录最近一次列表调用的分页窗口，
	// 用于断言用例把 page/pageSize 正确换算为 offset/limit。
	lastOffset int
	lastLimit  int

	// 故障注入：非 nil 时对应方法恒定失败。
	listErr   error
	findErr   error
	createErr error
	updateErr error

	// listNil 为 true 时列表返回 nil 切片，用于验证用例把 null 归一为 []。
	listNil bool
}

func newCardFakeRepo() *cardFakeRepo {
	return &cardFakeRepo{cards: map[int64]patient.Card{}}
}

// seed 预置一张就诊卡并返回落库后的副本（主键由桩自增分配）。
func (r *cardFakeRepo) seed(card patient.Card) patient.Card {
	r.nextID++
	card.ID = r.nextID
	r.cards[card.ID] = card
	return card
}

func (r *cardFakeRepo) ListCardsByPatient(
	_ context.Context,
	patientID int64,
	offset, limit int,
) ([]patient.Card, int64, error) {
	r.listCalls++
	r.lastOffset = offset
	r.lastLimit = limit
	if r.listErr != nil {
		return nil, 0, r.listErr
	}
	if r.listNil {
		return nil, 0, nil
	}

	owned := make([]patient.Card, 0)
	for _, card := range r.cards {
		if card.UserID == patientID {
			owned = append(owned, card)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].ID < owned[j].ID })

	total := int64(len(owned))
	if offset >= len(owned) {
		return []patient.Card{}, total, nil
	}
	end := offset + limit
	if end > len(owned) {
		end = len(owned)
	}
	return owned[offset:end], total, nil
}

func (r *cardFakeRepo) FindCardByID(_ context.Context, cardID int64) (*patient.Card, error) {
	r.findCalls++
	if r.findErr != nil {
		return nil, r.findErr
	}
	card, ok := r.cards[cardID]
	if !ok {
		// 与 repo.PostgresPatientRepository 一致：不存在返回领域哨兵错误。
		return nil, patient.ErrCardNotFound
	}
	copied := card
	return &copied, nil
}

func (r *cardFakeRepo) CreateCard(_ context.Context, card patient.Card) (*patient.Card, error) {
	r.createCalls++
	if r.createErr != nil {
		return nil, r.createErr
	}
	for _, existing := range r.cards {
		if existing.UserID == card.UserID {
			// 每个账号只允许一张卡，唯一性由仓储在事务内串行化保证。
			return nil, patient.ErrCardExists
		}
	}
	r.nextID++
	card.ID = r.nextID
	r.cards[card.ID] = card
	copied := card
	return &copied, nil
}

func (r *cardFakeRepo) UpdateCard(
	_ context.Context,
	cardID int64,
	update patient.CardUpdate,
) (*patient.Card, error) {
	r.updateCalls++
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	base, ok := r.cards[cardID]
	if !ok {
		return nil, patient.ErrCardNotFound
	}
	// 与真实仓储一致：不可修改字段与字段级校验都在 ApplyUpdate 内判定，
	// 出错时不写入任何列。
	updated, err := base.ApplyUpdate(update)
	if err != nil {
		return nil, err
	}
	r.cards[cardID] = updated
	copied := updated
	return &copied, nil
}

// newCardService 构造注入固定时钟的用例，使出生日期推导可重复。
func newCardService(repo *cardFakeRepo) *Service {
	service := NewService(repo)
	service.now = func() time.Time { return cardTestNow }
	return service
}

// cardTestInput 返回一份全字段合法的建卡入参，用例只覆盖需要验证的字段。
func cardTestInput() patient.CardInput {
	return patient.CardInput{
		Name:           "张三",
		Sex:            patient.SexMale,
		PID:            cardTestPIDMale,
		Tel:            "13800138000",
		MedicalHistory: []string{"高血压", "糖尿病"},
		InsuranceType:  "社会基本医疗保险",
	}
}

// cardTestPtr 返回 value 的指针，用于构造「本次提交了该字段」的 CardUpdate。
func cardTestPtr[T any](value T) *T {
	return &value
}

// --- 列表 ---

// TestListCardsReturnsItemsAndPagination 覆盖正常列表：条目、分页元数据与
// page/pageSize 到 offset/limit 的换算都必须正确。
func TestListCardsReturnsItemsAndPagination(t *testing.T) {
	repo := newCardFakeRepo()
	repo.seed(patient.Card{UserID: 20, Name: "张三"})
	repo.seed(patient.Card{UserID: 20, Name: "李四"})
	repo.seed(patient.Card{UserID: 99, Name: "他人"})
	service := newCardService(repo)

	page, err := service.ListCards(context.Background(), 20, 2, 1)
	if err != nil {
		t.Fatalf("ListCards 意外错误：%v", err)
	}
	if page.Page != 2 || page.PageSize != 1 {
		t.Fatalf("分页元数据 = page %d / pageSize %d，期望 2 / 1", page.Page, page.PageSize)
	}
	if page.Total != 2 {
		t.Errorf("total = %d，期望 2（只统计本人就诊卡）", page.Total)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "李四" {
		t.Fatalf("第二页条目 = %+v，期望只剩李四", page.Items)
	}
	if repo.lastOffset != 1 || repo.lastLimit != 1 {
		t.Errorf("仓储收到 offset/limit = %d/%d，期望 1/1", repo.lastOffset, repo.lastLimit)
	}
}

// TestListCardsEmptyIsNonNilSlice 覆盖空列表：无论仓储返回空切片还是 nil，
// 用例都必须输出非 nil 的空切片（契约 §1.4：空列表返回 items: []）。
func TestListCardsEmptyIsNonNilSlice(t *testing.T) {
	cases := []struct {
		name    string
		listNil bool
	}{
		{name: "仓储无数据", listNil: false},
		{name: "仓储返回 nil 切片", listNil: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newCardFakeRepo()
			repo.listNil = tc.listNil
			service := newCardService(repo)

			page, err := service.ListCards(context.Background(), 20, 1, 20)
			if err != nil {
				t.Fatalf("ListCards 意外错误：%v", err)
			}
			if page.Items == nil {
				t.Fatal("items 为 nil，期望非 nil 空切片（JSON 中必须是 []，不能是 null）")
			}
			if len(page.Items) != 0 {
				t.Fatalf("items = %+v，期望空切片", page.Items)
			}
			if page.Total != 0 {
				t.Errorf("total = %d，期望 0", page.Total)
			}
		})
	}
}

// TestListCardsPropagatesInvalidPagination 覆盖仓储判定分页窗口非法：
// 必须原样上抛 patient.ErrInvalidPagination（handler → 422），
// 绝不能归类为依赖故障（handler → 502）。
func TestListCardsPropagatesInvalidPagination(t *testing.T) {
	repo := newCardFakeRepo()
	repo.listErr = patient.ErrInvalidPagination
	service := newCardService(repo)

	_, err := service.ListCards(context.Background(), 20, 1, 20)
	if !errors.Is(err, patient.ErrInvalidPagination) {
		t.Fatalf("err = %v，期望 patient.ErrInvalidPagination", err)
	}
	if errors.Is(err, ErrDependencyUnavailable) {
		t.Error("分页错误不得被包装为 ErrDependencyUnavailable（否则会返回 502 而不是 422）")
	}
}

// --- 详情 ---

// TestGetCardReturnsOwnCard 覆盖本人就诊卡：用例返回实体原值，
// pid 脱敏只发生在 Card.View()，不在用例层。
func TestGetCardReturnsOwnCard(t *testing.T) {
	repo := newCardFakeRepo()
	seeded := repo.seed(patient.Card{
		UserID:   20,
		Name:     "张三",
		PID:      cardTestPIDMale,
		Tel:      "13800138000",
		Birthday: "1990-01-01",
	})
	service := newCardService(repo)

	card, err := service.GetCard(context.Background(), 20, seeded.ID)
	if err != nil {
		t.Fatalf("GetCard 意外错误：%v", err)
	}
	if card.ID != seeded.ID || card.Name != "张三" {
		t.Fatalf("GetCard = %+v，期望与预置卡一致", card)
	}
	if card.PID != cardTestPIDMale {
		t.Errorf("PID = %q，期望用例层保留原值（脱敏由 Card.View 负责）", card.PID)
	}
}

// TestGetCardHidesForeignAndMissing 覆盖越权与不存在：他人就诊卡与不存在的卡
// 必须返回同一个 patient.ErrCardNotFound（handler → 404），不得暴露卡是否存在。
func TestGetCardHidesForeignAndMissing(t *testing.T) {
	repo := newCardFakeRepo()
	others := repo.seed(patient.Card{UserID: 99, Name: "他人"})
	service := newCardService(repo)

	cases := []struct {
		name   string
		cardID int64
	}{
		{name: "他人就诊卡", cardID: others.ID},
		{name: "就诊卡不存在", cardID: 99999},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := service.GetCard(context.Background(), 20, tc.cardID)
			if !errors.Is(err, patient.ErrCardNotFound) {
				t.Fatalf("err = %v，期望 patient.ErrCardNotFound", err)
			}
			if errors.Is(err, ErrDependencyUnavailable) {
				t.Error("越权/不存在不得被归类为依赖故障（否则会返回 502 而不是 404）")
			}
		})
	}
}

// --- 创建 ---

// TestCreateCardCollectsAllMissingFields 覆盖必填字段全缺失：
// 字段清单必须一次性给出，且顺序与请求体字段顺序一致。
func TestCreateCardCollectsAllMissingFields(t *testing.T) {
	repo := newCardFakeRepo()
	service := newCardService(repo)

	_, err := service.CreateCard(context.Background(), 20, patient.CardInput{})
	if err == nil {
		t.Fatal("缺全部必填字段时应报错")
	}
	want := []string{"name", "sex", "pid", "tel", "medicalHistory", "insuranceType"}
	if got := patient.FieldsOf(err); !reflect.DeepEqual(got, want) {
		t.Fatalf("details.fields = %v，期望 %v", got, want)
	}
	if repo.createCalls != 0 {
		t.Error("字段校验失败不得触达仓储")
	}
}

// TestCreateCardCollectsPIDAndTelTogether 覆盖多字段同时非法：
// pid 与 tel 必须同时出现在 details.fields（契约 §12.5 示例）。
func TestCreateCardCollectsPIDAndTelTogether(t *testing.T) {
	repo := newCardFakeRepo()
	service := newCardService(repo)

	input := cardTestInput()
	input.PID = "invalid"
	input.Tel = "123"

	_, err := service.CreateCard(context.Background(), 20, input)
	if err == nil {
		t.Fatal("非法 pid 与 tel 应报错")
	}
	if got := patient.FieldsOf(err); !reflect.DeepEqual(got, []string{"pid", "tel"}) {
		t.Fatalf("details.fields = %v，期望 [pid tel]", got)
	}
	if !errors.Is(err, patient.ErrPIDInvalid) || !errors.Is(err, patient.ErrTelInvalid) {
		t.Errorf("错误链应同时保留 pid 与 tel 的领域原因，err = %v", err)
	}
}

// TestCreateCardRejectsSexMismatch 覆盖性别与身份证号不一致：
// 返回 patient.ErrSexMismatch，字段清单只有 sex。
func TestCreateCardRejectsSexMismatch(t *testing.T) {
	repo := newCardFakeRepo()
	service := newCardService(repo)

	input := cardTestInput()
	// 第 17 位为奇数（男），提交女必然不一致。
	input.Sex = patient.SexFemale

	_, err := service.CreateCard(context.Background(), 20, input)
	if !errors.Is(err, patient.ErrSexMismatch) {
		t.Fatalf("err = %v，期望 patient.ErrSexMismatch", err)
	}
	if got := patient.FieldsOf(err); !reflect.DeepEqual(got, []string{"sex"}) {
		t.Fatalf("details.fields = %v，期望 [sex]", got)
	}
	if repo.createCalls != 0 {
		t.Error("性别不一致不得触达仓储")
	}
}

// TestCreateCardRejectsFutureBirthday 覆盖身份证号推导出的出生日期晚于今天：
// 该规则依赖注入的时钟，用例固定 now 后必须稳定命中。
func TestCreateCardRejectsFutureBirthday(t *testing.T) {
	repo := newCardFakeRepo()
	service := newCardService(repo)

	input := cardTestInput()
	input.PID = cardTestPIDFuture

	_, err := service.CreateCard(context.Background(), 20, input)
	if !errors.Is(err, patient.ErrBirthdayInFuture) {
		t.Fatalf("err = %v，期望 patient.ErrBirthdayInFuture", err)
	}
	if repo.createCalls != 0 {
		t.Error("出生日期非法不得触达仓储")
	}
}

// TestCreateCardExistingCard 覆盖每个账号只允许一张卡：
// 仓储返回 patient.ErrCardExists 时必须原样上抛（handler → 409），不得归为依赖故障。
func TestCreateCardExistingCard(t *testing.T) {
	repo := newCardFakeRepo()
	repo.seed(patient.Card{UserID: 20, Name: "旧卡"})
	service := newCardService(repo)

	_, err := service.CreateCard(context.Background(), 20, cardTestInput())
	if !errors.Is(err, patient.ErrCardExists) {
		t.Fatalf("err = %v，期望 patient.ErrCardExists", err)
	}
	if errors.Is(err, ErrDependencyUnavailable) {
		t.Error("已有卡不得被归类为依赖故障（否则会返回 502 而不是 409）")
	}
}

// TestCreateCardDerivesBirthdayAndUUID 覆盖创建成功：出生日期由身份证号推导、
// UUID 由服务端生成（32 位无横线），归属取自入参患者主键。
func TestCreateCardDerivesBirthdayAndUUID(t *testing.T) {
	repo := newCardFakeRepo()
	service := newCardService(repo)

	card, err := service.CreateCard(context.Background(), 20, cardTestInput())
	if err != nil {
		t.Fatalf("CreateCard 意外错误：%v", err)
	}
	if card.UserID != 20 {
		t.Errorf("userId = %d，期望 20（归属只能取自访问令牌）", card.UserID)
	}
	if card.Birthday != "1990-01-01" {
		t.Errorf("birthday = %q，期望由身份证号推导的 1990-01-01", card.Birthday)
	}
	if card.Sex != patient.SexMale {
		t.Errorf("sex = %q，期望 %q", card.Sex, patient.SexMale)
	}
	if len(card.UUID) != 32 || strings.Contains(card.UUID, "-") {
		t.Errorf("uuid = %q，期望 32 位无横线字符串", card.UUID)
	}
	if !reflect.DeepEqual(card.MedicalHistory, []string{"高血压", "糖尿病"}) {
		t.Errorf("medicalHistory = %v，期望原样保存", card.MedicalHistory)
	}
	if card.ExistFaceModel {
		t.Error("新建就诊卡不得带人脸模型")
	}
	if repo.createCalls != 1 || len(repo.cards) != 1 {
		t.Errorf("仓储创建次数 = %d、卡数 = %d，期望各 1", repo.createCalls, len(repo.cards))
	}
}

// --- 修改 ---

// TestUpdateCardHidesForeignCard 覆盖越权修改：他人就诊卡必须返回
// patient.ErrCardNotFound，且绝不能进入写入路径。
func TestUpdateCardHidesForeignCard(t *testing.T) {
	repo := newCardFakeRepo()
	others := repo.seed(patient.Card{UserID: 99, Name: "他人", Tel: "13800138000"})
	service := newCardService(repo)

	_, err := service.UpdateCard(
		context.Background(),
		20,
		others.ID,
		patient.CardUpdate{Tel: cardTestPtr("13900139000")},
	)
	if !errors.Is(err, patient.ErrCardNotFound) {
		t.Fatalf("err = %v，期望 patient.ErrCardNotFound", err)
	}
	if repo.updateCalls != 0 {
		t.Fatalf("越权修改不得调用仓储 UpdateCard，实际调用 %d 次", repo.updateCalls)
	}
	if repo.cards[others.ID].Tel != "13800138000" {
		t.Error("越权修改不得写入任何字段")
	}
}

// TestUpdateCardRejectsImmutableFields 覆盖不可修改字段：
// pid/userId/birthday 提交即 422，且字段清单用于响应 details.fields。
func TestUpdateCardRejectsImmutableFields(t *testing.T) {
	cases := []struct {
		name       string
		update     patient.CardUpdate
		wantErr    error
		wantFields []string
	}{
		{
			name:       "提交 pid",
			update:     patient.CardUpdate{PID: cardTestPtr(cardTestPIDFemale)},
			wantErr:    patient.ErrPIDImmutable,
			wantFields: []string{"pid"},
		},
		{
			name:       "提交 userId",
			update:     patient.CardUpdate{UserID: cardTestPtr(int64(99))},
			wantErr:    patient.ErrUserIDImmutable,
			wantFields: []string{"userId"},
		},
		{
			name:       "提交 birthday",
			update:     patient.CardUpdate{Birthday: cardTestPtr("1991-01-01")},
			wantErr:    patient.ErrBirthdayImmutable,
			wantFields: []string{"birthday"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newCardFakeRepo()
			seeded := repo.seed(patient.Card{
				UserID:   20,
				Name:     "张三",
				PID:      cardTestPIDMale,
				Birthday: "1990-01-01",
			})
			service := newCardService(repo)

			_, err := service.UpdateCard(context.Background(), 20, seeded.ID, tc.update)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if got := patient.FieldsOf(err); !reflect.DeepEqual(got, tc.wantFields) {
				t.Fatalf("details.fields = %v，期望 %v", got, tc.wantFields)
			}
			stored := repo.cards[seeded.ID]
			if stored.PID != cardTestPIDMale ||
				stored.UserID != 20 ||
				stored.Birthday != "1990-01-01" {
				t.Errorf("不可修改字段出错时不得写入，实际卡 = %+v", stored)
			}
		})
	}
}

// TestUpdateCardReturnsRepositoryResult 覆盖修改成功：返回仓储落库后的卡，
// 未提交的字段保持原值。
func TestUpdateCardReturnsRepositoryResult(t *testing.T) {
	repo := newCardFakeRepo()
	seeded := repo.seed(patient.Card{
		UserID:         20,
		UUID:           "CARD0000000000000000000000000010",
		Name:           "张三",
		Sex:            patient.SexMale,
		PID:            cardTestPIDMale,
		Tel:            "13800138000",
		Birthday:       "1990-01-01",
		MedicalHistory: []string{"高血压", "糖尿病"},
		InsuranceType:  "社会基本医疗保险",
	})
	service := newCardService(repo)

	updated, err := service.UpdateCard(context.Background(), 20, seeded.ID, patient.CardUpdate{
		Tel:            cardTestPtr("13900139000"),
		MedicalHistory: cardTestPtr([]string{"高血压", "其他"}),
	})
	if err != nil {
		t.Fatalf("UpdateCard 意外错误：%v", err)
	}
	if updated.Tel != "13900139000" {
		t.Errorf("tel = %q，期望 13900139000", updated.Tel)
	}
	if !reflect.DeepEqual(updated.MedicalHistory, []string{"高血压", "其他"}) {
		t.Errorf("medicalHistory = %v，期望 [高血压 其他]", updated.MedicalHistory)
	}
	if updated.Name != "张三" || updated.PID != cardTestPIDMale || updated.Birthday != "1990-01-01" {
		t.Errorf("未提交的字段必须保持原值，实际 %+v", updated)
	}
	if repo.cards[seeded.ID].Tel != "13900139000" {
		t.Error("修改结果必须落库")
	}
}

// --- 仓储故障 ---

// TestCardRepositoryFailuresAreDependencyUnavailable 覆盖四个用例方法的仓储故障：
// 普通错误必须包装为 ErrDependencyUnavailable（handler → 502），
// 且 errors.Is 仍能定位到根因，日志不丢原始错误。
func TestCardRepositoryFailuresAreDependencyUnavailable(t *testing.T) {
	cause := errors.New("boom")

	cases := []struct {
		name   string
		inject func(*cardFakeRepo)
		call   func(*Service) error
	}{
		{
			name:   "列表",
			inject: func(repo *cardFakeRepo) { repo.listErr = cause },
			call: func(service *Service) error {
				_, err := service.ListCards(context.Background(), 20, 1, 20)
				return err
			},
		},
		{
			name:   "详情",
			inject: func(repo *cardFakeRepo) { repo.findErr = cause },
			call: func(service *Service) error {
				_, err := service.GetCard(context.Background(), 20, 1)
				return err
			},
		},
		{
			name:   "创建",
			inject: func(repo *cardFakeRepo) { repo.createErr = cause },
			call: func(service *Service) error {
				_, err := service.CreateCard(context.Background(), 20, cardTestInput())
				return err
			},
		},
		{
			name:   "修改",
			inject: func(repo *cardFakeRepo) { repo.updateErr = cause },
			call: func(service *Service) error {
				_, err := service.UpdateCard(
					context.Background(),
					20,
					1,
					patient.CardUpdate{Tel: cardTestPtr("13900139000")},
				)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newCardFakeRepo()
			// 预置本人卡：确保「修改」用例的故障来自 UpdateCard 而不是归属校验。
			repo.seed(patient.Card{UserID: 20, Name: "张三"})
			tc.inject(repo)
			service := newCardService(repo)

			err := tc.call(service)
			if !errors.Is(err, ErrDependencyUnavailable) {
				t.Fatalf("err = %v，期望包装为 ErrDependencyUnavailable", err)
			}
			if !errors.Is(err, cause) {
				t.Errorf("依赖故障必须保留根因，err = %v", err)
			}
		})
	}
}

// TestCardServiceRejectsAnonymousSubject 覆盖主体缺失：
// 患者主键非法（<= 0）时一律按账号不存在收敛，绝不按请求参数降级放行。
func TestCardServiceRejectsAnonymousSubject(t *testing.T) {
	repo := newCardFakeRepo()
	repo.seed(patient.Card{UserID: 20, Name: "张三"})
	service := newCardService(repo)

	calls := []struct {
		name string
		call func() error
	}{
		{
			name: "列表",
			call: func() error {
				_, err := service.ListCards(context.Background(), 0, 1, 20)
				return err
			},
		},
		{
			name: "详情",
			call: func() error {
				_, err := service.GetCard(context.Background(), 0, 1)
				return err
			},
		},
		{
			name: "创建",
			call: func() error {
				_, err := service.CreateCard(context.Background(), 0, cardTestInput())
				return err
			},
		},
		{
			name: "修改",
			call: func() error {
				_, err := service.UpdateCard(context.Background(), 0, 1, patient.CardUpdate{})
				return err
			},
		},
	}

	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, patient.ErrPatientNotFound) {
				t.Fatalf("err = %v，期望 patient.ErrPatientNotFound", err)
			}
		})
	}
	if repo.listCalls != 0 || repo.findCalls != 0 || repo.createCalls != 0 || repo.updateCalls != 0 {
		t.Errorf(
			"主体缺失不得触达仓储，实际 list=%d find=%d create=%d update=%d",
			repo.listCalls, repo.findCalls, repo.createCalls, repo.updateCalls,
		)
	}
}

// TestListCardsRejectsOutOfRangeWindowWithoutRepository 覆盖用例层的分页上界兜底：
// page/pageSize 越界时不触达仓储，直接返回 patient.ErrInvalidPagination（handler → 422），
// 既不把 PostgreSQL 的 2201W/2201X 原始错误变成 500，也不伪装成依赖故障（→502）。
func TestListCardsRejectsOutOfRangeWindowWithoutRepository(t *testing.T) {
	cases := []struct {
		name     string
		page     int
		pageSize int
	}{
		{name: "page=0", page: 0, pageSize: 20},
		{name: "page 为负", page: -1, pageSize: 20},
		{name: "page=100001", page: 100001, pageSize: 20},
		{name: "pageSize=0", page: 1, pageSize: 0},
		{name: "pageSize 为负", page: 1, pageSize: -1},
		{name: "pageSize=101", page: 1, pageSize: 101},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newCardFakeRepo()
			service := newCardService(repo)

			_, err := service.ListCards(context.Background(), 20, tc.page, tc.pageSize)
			if !errors.Is(err, patient.ErrInvalidPagination) {
				t.Fatalf(
					"ListCards(page=%d, pageSize=%d) 错误 = %v，期望 patient.ErrInvalidPagination",
					tc.page, tc.pageSize, err,
				)
			}
			if errors.Is(err, ErrDependencyUnavailable) {
				t.Error("分页越界不得被归类为依赖故障（否则会返回 502 而不是 422）")
			}
			if repo.listCalls != 0 {
				t.Fatalf("分页越界不得触达仓储，实际调用 %d 次", repo.listCalls)
			}
		})
	}
}

// TestListCardsAcceptsBoundaryWindow 覆盖契约 §1.4 的合法上界：
// page=100000、pageSize=100 必须照常触达仓储，并按 (page-1)*pageSize 换算 offset。
func TestListCardsAcceptsBoundaryWindow(t *testing.T) {
	repo := newCardFakeRepo()
	repo.seed(patient.Card{UserID: 20, Name: "张三"})
	service := newCardService(repo)

	page, err := service.ListCards(context.Background(), 20, 100000, 100)
	if err != nil {
		t.Fatalf("边界合法分页不应报错，实际 %v", err)
	}
	if repo.listCalls != 1 {
		t.Fatalf("边界合法分页必须触达仓储，实际调用 %d 次", repo.listCalls)
	}
	wantOffset := (100000 - 1) * 100
	if repo.lastOffset != wantOffset || repo.lastLimit != 100 {
		t.Errorf(
			"仓储收到 offset/limit = %d/%d，期望 %d/100",
			repo.lastOffset, repo.lastLimit, wantOffset,
		)
	}
	if page.Page != 100000 || page.PageSize != 100 {
		t.Errorf("分页元数据 = %d/%d，期望 100000/100", page.Page, page.PageSize)
	}
	if page.Total != 1 {
		t.Errorf("total = %d，期望 1（总数不受分页窗口影响）", page.Total)
	}
}
