// 就诊卡路由的认证域保护单测：患者端就诊卡的四个接口都必须挂在
// realm=patient 的访问令牌校验之后（spec/04-api-contract.md §7.4、§10、§12.5）。
package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"Medical-Web-Backend/internal/config"
	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/domain/patient"
	"Medical-Web-Backend/internal/port"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

// patientCardGuardRoutes 是本次新增的就诊卡路由（路径参数用占位值）。
var patientCardGuardRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/v1/patient/cards"},
	{http.MethodPost, "/api/v1/patient/cards"},
	{http.MethodGet, "/api/v1/patient/cards/10"},
	{http.MethodPatch, "/api/v1/patient/cards/10"},
}

// TestRouterPatientCardRoutesAreGuardedByPatientRealm 断言就诊卡接口只接受
// realm=patient 的令牌：匿名请求与 realm=mis 的合法令牌都必须返回
// 401 AUTH_INVALID_TOKEN 与患者域文案「患者访问令牌无效」，
// 不得按管理端文案返回，也不得被权限中间件拦成 403。
func TestRouterPatientCardRoutesAreGuardedByPatientRealm(t *testing.T) {
	router, misToken := newRealmTestRouter(t, domainauth.RealmMis)

	for _, route := range patientCardGuardRoutes {
		for _, token := range []string{"", misToken} {
			w := performRealmRequest(router, route.method, route.path, token)

			if w.Code != http.StatusUnauthorized {
				t.Errorf(
					"%s %s（携带令牌=%t）status = %d, want 401; body=%s",
					route.method,
					route.path,
					token != "",
					w.Code,
					w.Body.String(),
				)
				continue
			}

			body := decodeRealmRouterBody(t, w)
			if body["code"] != "AUTH_INVALID_TOKEN" {
				t.Errorf(
					"%s %s（携带令牌=%t）code = %v, want AUTH_INVALID_TOKEN",
					route.method,
					route.path,
					token != "",
					body["code"],
				)
			}
			if body["message"] != "患者访问令牌无效" {
				t.Errorf(
					"%s %s（携带令牌=%t）message = %v, want 患者访问令牌无效",
					route.method,
					route.path,
					token != "",
					body["message"],
				)
			}
		}
	}
}

// patientCardRouterPatientID 是正向用例中令牌与就诊卡共同的属主主键。
const patientCardRouterPatientID int64 = 20

// patientCardListRepoStub 是内存版 port.PatientRepository 桩：只为就诊卡列表提供
// ListCardsByPatient 的真实实现，其余方法返回零值或领域错误，
// 保证路由层用例不依赖 PostgreSQL（调用未实现的方法时会得到明确错误而不是 panic）。
type patientCardListRepoStub struct {
	cards []patient.Card
}

func (s patientCardListRepoStub) ListCardsByPatient(
	_ context.Context,
	patientID int64,
	offset, limit int,
) ([]patient.Card, int64, error) {
	owned := make([]patient.Card, 0)
	for _, card := range s.cards {
		if card.UserID == patientID {
			owned = append(owned, card)
		}
	}
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

func (s patientCardListRepoStub) FindPatientByID(_ context.Context, _ int64) (*patient.Patient, error) {
	return nil, patient.ErrPatientNotFound
}

func (s patientCardListRepoStub) FindOrCreatePatientByOpenID(
	_ context.Context,
	_ string,
	_ time.Time,
) (*patient.Patient, bool, error) {
	return nil, false, errors.New("patient card list stub: FindOrCreatePatientByOpenID 未实现")
}

func (s patientCardListRepoStub) FindCardByID(_ context.Context, _ int64) (*patient.Card, error) {
	return nil, patient.ErrCardNotFound
}

func (s patientCardListRepoStub) FindCardByPatientID(_ context.Context, _ int64) (*patient.Card, error) {
	return nil, patient.ErrCardNotFound
}

func (s patientCardListRepoStub) CreateCard(_ context.Context, _ patient.Card) (*patient.Card, error) {
	return nil, errors.New("patient card list stub: CreateCard 未实现")
}

func (s patientCardListRepoStub) UpdateCard(
	_ context.Context,
	_ int64,
	_ patient.CardUpdate,
) (*patient.Card, error) {
	return nil, errors.New("patient card list stub: UpdateCard 未实现")
}

func (s patientCardListRepoStub) ListFaceAuthByCard(
	_ context.Context,
	_ int64,
) ([]patient.FaceAuthRecord, error) {
	return nil, errors.New("patient card list stub: ListFaceAuthByCard 未实现")
}

// newPatientCardRealmTestRouter 构造完整路由树并签发 realm=patient 的合法 access token，
// 同时注入内存版就诊卡仓储，用于验证「患者域令牌能真正走到 handler」。
func newPatientCardRealmTestRouter(
	t *testing.T,
	repo port.PatientRepository,
) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := NewRouter(
		func() map[string]any { return map[string]any{"status": "ok"} },
		config.Config{Auth: config.AuthConfig{JWTSecret: contractTestJWTSecret}},
		realmRouterUserRepo{},
		contractTokenRepo{},
		nil, // doctorRepository
		nil, // scheduleRepository
		nil, // idempotencyStore
		repo,
		nil, // wechatAuthenticator
		nil, // registrationRepository
	)

	claims := &userservice.AccessClaims{
		UserID:    patientCardRouterPatientID,
		Username:  "patient-20",
		TokenType: userservice.TokenTypeAccess,
		Realm:     domainauth.RealmPatient,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "patient-card-realm-jti",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(contractTestJWTSecret))
	if err != nil {
		t.Fatalf("签发患者域测试 access token 失败：%v", err)
	}
	return router, signed
}

// TestRouterPatientCardListReachesHandlerWithPatientRealmToken 是 realm 保护的正向断言：
// 只断言匿名/跨域令牌返回 401 无法发现「患者域令牌被误拦」的缺陷（假阴性），
// 因此这里用 realm=patient 的合法令牌走完整路由树，断言请求真正到达 handler：
// 200 + items[0] 的字段形状（pid 脱敏、tel 明文）。
func TestRouterPatientCardListReachesHandlerWithPatientRealmToken(t *testing.T) {
	repo := patientCardListRepoStub{cards: []patient.Card{{
		ID:             10,
		UserID:         patientCardRouterPatientID,
		UUID:           "CARD0000000000000000000000000010",
		Name:           "张三",
		Sex:            patient.SexMale,
		PID:            "110101199001011237",
		Tel:            "13800138000",
		Birthday:       "1990-01-01",
		MedicalHistory: []string{"无"},
		InsuranceType:  "社会基本医疗保险",
	}}}
	router, token := newPatientCardRealmTestRouter(t, repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/patient/cards", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf(
			"status = %d, want 200（患者域令牌不得被 realm 校验误拦）; body=%s",
			w.Code,
			w.Body.String(),
		)
	}

	body := decodeRealmRouterBody(t, w)
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %v，期望长度为 1 的数组", body["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] = %v，期望对象", items[0])
	}
	if item["id"] != float64(10) {
		t.Errorf("items[0].id = %v，期望 10", item["id"])
	}
	if item["pid"] != "110101********1237" {
		t.Errorf("items[0].pid = %v，期望脱敏后的 110101********1237", item["pid"])
	}
	if item["tel"] != "13800138000" {
		t.Errorf("items[0].tel = %v，期望患者本人接口明文 13800138000", item["tel"])
	}
}
