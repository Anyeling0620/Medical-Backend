package patient

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestCardViewMasksPID 验证对外投影使用脱敏身份证号，且不泄露出生日期段。
func TestCardViewMasksPID(t *testing.T) {
	card := mustCard(t)
	view := card.View()

	if view.PID != "110101********1237" {
		t.Fatalf("CardView.PID = %q，期望 110101********1237", view.PID)
	}
	if strings.Contains(view.PID, testPIDMale) {
		t.Fatalf("CardView.PID 泄露完整身份证号：%q", view.PID)
	}
	if strings.Contains(view.PID, testPIDMale[6:14]) {
		t.Fatalf("CardView.PID 泄露出生日期段：%q", view.PID)
	}
	if view.ID != card.ID || view.UserID != card.UserID || view.UUID != card.UUID ||
		view.Name != card.Name || view.Sex != card.Sex || view.Tel != card.Tel ||
		view.Birthday != card.Birthday || view.InsuranceType != card.InsuranceType ||
		view.ExistFaceModel != card.ExistFaceModel {
		t.Fatalf("投影与实体不一致：%+v vs %+v", view, card)
	}
	if !reflect.DeepEqual(view.MedicalHistory, card.MedicalHistory) {
		t.Fatalf("疾病史投影不一致：%v vs %v", view.MedicalHistory, card.MedicalHistory)
	}
}

// TestCardJSONDoesNotLeakPID 验证直接序列化实体也不会泄露完整身份证号。
func TestCardJSONDoesNotLeakPID(t *testing.T) {
	card := mustCard(t)
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("序列化就诊卡失败：%v", err)
	}
	text := string(raw)
	if strings.Contains(text, testPIDMale) {
		t.Fatalf("就诊卡 JSON 泄露完整 pid：%s", text)
	}
	if strings.Contains(text, testPIDMale[6:14]) {
		t.Fatalf("就诊卡 JSON 泄露出生日期段：%s", text)
	}
	if !strings.Contains(text, `"medicalHistory"`) {
		t.Fatalf("就诊卡 JSON 缺少 medicalHistory 字段：%s", text)
	}
}

// TestPatientJSONDoesNotLeakOpenID 验证患者账号序列化时 openId 被屏蔽。
func TestPatientJSONDoesNotLeakOpenID(t *testing.T) {
	const openID = "wx-openid-secret-123"
	nickname := "张三"
	profile := Patient{
		ID:         1,
		OpenID:     openID,
		Nickname:   &nickname,
		Status:     StatusActive,
		CreateDate: "2026-09-10",
	}
	raw, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("序列化患者账号失败：%v", err)
	}
	if strings.Contains(string(raw), openID) {
		t.Fatalf("患者账号 JSON 泄露 openId：%s", raw)
	}
	if !profile.IsActive() {
		t.Error("ACTIVE 账号 IsActive() 应为 true")
	}
	if (Patient{Status: StatusDisabled}).IsActive() {
		t.Error("DISABLED 账号 IsActive() 应为 false")
	}
	if (Patient{}).IsActive() {
		t.Error("状态为空时 IsActive() 必须为 false（向拒绝一侧收敛）")
	}
}

// TestToFaceAuthListEmptyRecords 验证记录为空时返回非 nil 空切片，JSON 输出 []。
func TestToFaceAuthListEmptyRecords(t *testing.T) {
	list := ToFaceAuthList(42, nil)
	if list.PatientCardID != 42 {
		t.Fatalf("PatientCardID = %d，期望 42", list.PatientCardID)
	}
	if list.Records == nil {
		t.Fatal("Records 不应为 nil")
	}
	if len(list.Records) != 0 {
		t.Fatalf("Records 长度 = %d，期望 0", len(list.Records))
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("序列化人脸认证列表失败：%v", err)
	}
	if !strings.Contains(string(raw), `"records":[]`) {
		t.Fatalf("空记录应序列化为 []，实际：%s", raw)
	}
}

// TestToFaceAuthListKeepsRecords 验证有记录时原样保留。
func TestToFaceAuthListKeepsRecords(t *testing.T) {
	records := []FaceAuthRecord{{ID: 1, Date: "2026-09-10"}}
	list := ToFaceAuthList(7, records)
	if !reflect.DeepEqual(list.Records, records) {
		t.Fatalf("Records = %v，期望 %v", list.Records, records)
	}
}

// TestCardBelongsTo 验证归属判定只依赖 user_id，并覆盖 userID=0 的边界。
func TestCardBelongsTo(t *testing.T) {
	cases := []struct {
		name      string
		userID    int64
		patientID int64
		want      bool
	}{
		{"本人", 5, 5, true},
		{"他人", 5, 6, false},
		{"占位卡归零号账号", 0, 0, true},
		{"占位卡不属于真实患者", 0, 5, false},
		{"真实卡不属于零号账号", 5, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := Card{UserID: tc.userID}
			if got := card.BelongsTo(tc.patientID); got != tc.want {
				t.Fatalf("Card{UserID:%d}.BelongsTo(%d) = %v，期望 %v",
					tc.userID, tc.patientID, got, tc.want)
			}
		})
	}
}
