package handler

import (
	"net/http"
	"strings"
	"testing"

	"Medical-Web-Backend/internal/port"
)

// TestScheduleSlotCreateSlotMissingIdempotencyKey 缺少幂等键返回 422，且不触碰 store 与 repo。
func TestScheduleSlotCreateSlotMissingIdempotencyKey(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, stub, store, 7)

	w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, nil)
	body := assertErrorStatus(t, w, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED")
	if body["message"] != "Idempotency-Key 请求头必填" {
		t.Errorf("message = %v, want Idempotency-Key 请求头必填", body["message"])
	}
	if store.claimCalls != 0 || stub.createCalls != 0 {
		t.Errorf("缺少幂等键不应触达 store/repo：claim=%d create=%d",
			store.claimCalls, stub.createCalls)
	}
}

// TestScheduleSlotCreateSlotInvalidIdempotencyKey 非法幂等键（超长/含不可打印字符）返回 422。
func TestScheduleSlotCreateSlotInvalidIdempotencyKey(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, stub, store, 7)

	cases := map[string]struct {
		key     string
		message string
	}{
		"too long":        {key: strings.Repeat("a", 129), message: "Idempotency-Key 长度必须在 1 到 128 之间"},
		"tab control":     {key: "ab\tcd", message: "Idempotency-Key 只能包含可打印 ASCII 字符"},
		"non-ascii bytes": {key: "abc\u00e9", message: "Idempotency-Key 只能包含可打印 ASCII 字符"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
				`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": tc.key})
			body := assertErrorStatus(t, w, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED")
			if body["message"] != tc.message {
				t.Errorf("message = %v, want %s", body["message"], tc.message)
			}
		})
	}
	if store.claimCalls != 0 || stub.createCalls != 0 {
		t.Errorf("非法幂等键不应触达 store/repo：claim=%d create=%d",
			store.claimCalls, stub.createCalls)
	}
}

// TestScheduleSlotCreateSlotMissingClaims 上下文缺少 access claims 时返回 401 AUTH_INVALID_TOKEN。
func TestScheduleSlotCreateSlotMissingClaims(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, stub, store, 0) // 未预置 claims

	w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": "no-claims"})
	body := assertErrorStatus(t, w, http.StatusUnauthorized, "AUTH_INVALID_TOKEN")
	if body["message"] != "访问令牌无效或已过期" {
		t.Errorf("message = %v, want 访问令牌无效或已过期", body["message"])
	}
	if store.claimCalls != 0 || stub.createCalls != 0 {
		t.Errorf("缺少 claims 不应触达 store/repo：claim=%d create=%d",
			store.claimCalls, stub.createCalls)
	}
}

// TestScheduleSlotCreateSlotStoreUnavailable 幂等存储不可用时返回 503，
// 且不得静默降级为无保护创建（repo 不被调用）。
func TestScheduleSlotCreateSlotStoreUnavailable(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	store := newMemIdempotencyStore()
	store.claimErr = port.ErrIdempotencyStoreUnavailable
	e := newScheduleSlotEngine(t, stub, store, 7)

	w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": "store-down"})
	body := assertErrorStatus(t, w, http.StatusServiceUnavailable, "IDEMPOTENCY_STORE_UNAVAILABLE")
	if body["message"] != "幂等存储不可用，请稍后重试" {
		t.Errorf("message = %v", body["message"])
	}
	if stub.createCalls != 0 {
		t.Errorf("store 不可用时不得创建时段，createCalls = %d", stub.createCalls)
	}
}

// TestScheduleSlotCreateSlotBodyValidation 创建请求体字段非法返回固定中文文案的 422。
func TestScheduleSlotCreateSlotBodyValidation(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, stub, store, 7)

	cases := map[string]struct {
		body    string
		message string
	}{
		"empty body":       {body: "", message: "请求体不能为空"},
		"slot zero":        {body: `{"slot":0,"maximum":3}`, message: "时段编号必须为正整数"},
		"slot too big":     {body: `{"slot":70000,"maximum":3}`, message: "时段编号不能超过 32767"},
		"maximum zero":     {body: `{"slot":1,"maximum":0}`, message: "时段最大号源必须大于 0"},
		"maximum too big":  {body: `{"slot":1,"maximum":32768}`, message: "时段最大号源不能超过 32767"},
		"unknown field":    {body: `{"slot":1,"maximum":3,"extra":1}`, message: "请求体格式不正确（包含未知字段）"},
		"trailing content": {body: `{"slot":1,"maximum":3}{"slot":2}`, message: "请求体包含多余内容"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// 每次使用独立幂等键，避免上一次保存的 422 结果被重放。
			w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
				tc.body, map[string]string{"Idempotency-Key": "validation-" + name})
			body := assertErrorStatus(t, w, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED")
			if body["message"] != tc.message {
				t.Errorf("message = %v, want %s", body["message"], tc.message)
			}
		})
	}
	if stub.createCalls != 0 {
		t.Errorf("校验失败不应创建时段，createCalls = %d", stub.createCalls)
	}
}

// TestScheduleSlotCreateSlotInvalidJSON JSON 语法错误、字段类型错误以及截断的
// JSON（io.ErrUnexpectedEOF，如 {"slot":3,）按 auth 约定统一返回 400
// REQUEST_INVALID_JSON 与固定文案“请求体不是合法的 JSON”。
func TestScheduleSlotCreateSlotInvalidJSON(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, stub, store, 7)

	cases := map[string]string{
		"syntax error":   `{"slot":1,"maximum":3,}`,
		"truncated json": `{"slot":3,`,
		"field type err": `{"slot":"x","maximum":3}`,
		"non-object":     `[1,2,3]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
				body, map[string]string{"Idempotency-Key": "bad-json-" + name})
			resp := assertErrorStatus(t, w, http.StatusBadRequest, "REQUEST_INVALID_JSON")
			if resp["message"] != "请求体不是合法的 JSON" {
				t.Errorf("message = %v, want 请求体不是合法的 JSON", resp["message"])
			}
		})
	}
	if stub.createCalls != 0 {
		t.Errorf("非法 JSON 不应创建时段，createCalls = %d", stub.createCalls)
	}
}

// TestScheduleSlotCreateSlotSlotNumberTooLarge slot 超过 int16 上限（32767）时返回
// 422 并给出“时段编号不能超过 32767”，防止截断成错误的时段号落库。
func TestScheduleSlotCreateSlotSlotNumberTooLarge(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, stub, store, 7)

	w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":70000,"maximum":3}`, map[string]string{"Idempotency-Key": "slot-too-big"})
	body := assertErrorStatus(t, w, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED")
	if body["message"] != "时段编号不能超过 32767" {
		t.Errorf("message = %v, want 时段编号不能超过 32767", body["message"])
	}
	if stub.createCalls != 0 {
		t.Errorf("超界 slot 不应触达 repo.CreateSlot，createCalls = %d", stub.createCalls)
	}
}
