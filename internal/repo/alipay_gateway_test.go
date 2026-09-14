// 支付宝适配器的离线单测（审查 P2-8）：不依赖网络、真实凭据、数据库与系统时间。
//
// 覆盖范围：待签串拼接口径（请求保留 sign_type、通知剔除 sign_type）、响应信封解析、
// 业务错误码映射、响应验签失败、预下单成功取 qr_code、主动查询「交易不存在」。
// RSA 密钥对在本文件内现场生成，假网关用 httptest 起在本机回环地址上。
package repo

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"Medical-Web-Backend/internal/domain/payment"
	"Medical-Web-Backend/internal/port"
)

// ---- 离线测试装置：现场生成密钥 + 本机假网关 ----

// alipayTestKeyPair 现场生成 RSA 密钥对（2048 位），避免依赖任何真实凭据或固定测试密钥。
func alipayTestKeyPair(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 密钥对失败：%v", err)
	}
	return key, &key.PublicKey
}

// TestAlipayGatewayReadyRejectsInvalidPrivateKey 覆盖后台任务启动前的就绪检查：
// 非空但无法解析的私钥仍属于无效配置，不能让收口任务启动后按运行期故障继续释放号源。
func TestAlipayGatewayReadyRejectsInvalidPrivateKey(t *testing.T) {
	gateway := NewAlipayGateway(AlipayOptions{
		AppID:      "test-app",
		PrivateKey: "not-a-private-key",
		GatewayURL: "https://example.invalid/gateway.do",
	})
	if err := gateway.Ready(); !errors.Is(err, port.ErrAlipayUnavailable) {
		t.Fatalf("无效私钥的 Ready 错误 = %v，期望 ErrAlipayUnavailable", err)
	}
}

// alipayTestPrivateKeyBase64 把私钥编码为裸 base64（PKCS#8），与支付宝开放平台复制出来的格式一致。
func alipayTestPrivateKeyBase64(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("编码应用私钥失败：%v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// alipayTestPublicKeyBase64 把公钥编码为裸 base64（PKIX DER）。
func alipayTestPublicKeyBase64(t *testing.T, key *rsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatalf("编码支付宝公钥失败：%v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// alipayTestSignRSA2 用给定私钥做 SHA256withRSA 签名并 base64 编码（独立实现，不复用被测代码）。
func alipayTestSignRSA2(t *testing.T, key *rsa.PrivateKey, content string) string {
	t.Helper()
	hashed := sha256.Sum256([]byte(content))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatalf("RSA2 签名失败：%v", err)
	}
	return base64.StdEncoding.EncodeToString(signature)
}

// alipayTestFakeGateway 是只监听本机的假支付宝网关句柄，同时记录收到的请求表单。
type alipayTestFakeGateway struct {
	*httptest.Server
	mu       sync.Mutex
	requests []url.Values
}

// Requests 返回收到的请求表单副本（加锁读取，兼容 -race）。
func (g *alipayTestFakeGateway) Requests() []url.Values {
	g.mu.Lock()
	defer g.mu.Unlock()
	copied := make([]url.Values, 0, len(g.requests))
	for _, form := range g.requests {
		copied = append(copied, form)
	}
	return copied
}

// alipayTestServerOptions 描述假网关的响应行为。
type alipayTestServerOptions struct {
	// signKey 是服务端签名私钥；它的公钥会通过 AlipayOptions.PublicKey 配置给被测适配器。
	signKey *rsa.PrivateKey
	// tamperKey 非空时改用该私钥签名响应，模拟「响应被第三方篡改」。
	tamperKey *rsa.PrivateKey
	// payloads 是 method -> 业务节点原文（必须逐字节原样返回，响应验签依赖这一点）。
	payloads map[string]string
}

// alipayTestServer 启动假网关：按请求里的 method 返回固定业务节点，并用指定私钥对节点原文签名。
// 签名在启动前算好，避免在 HTTP handler 的 goroutine 里调用 t.Fatalf。
func alipayTestServer(t *testing.T, options alipayTestServerOptions) *alipayTestFakeGateway {
	t.Helper()
	signatures := make(map[string]string, len(options.payloads))
	for method, payload := range options.payloads {
		key := options.signKey
		if options.tamperKey != nil {
			key = options.tamperKey
		}
		signatures[method] = alipayTestSignRSA2(t, key, payload)
	}

	fake := &alipayTestFakeGateway{}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "解析表单失败", http.StatusBadRequest)
			return
		}
		form := r.PostForm
		fake.mu.Lock()
		fake.requests = append(fake.requests, form)
		fake.mu.Unlock()

		method := form.Get("method")
		payload, ok := options.payloads[method]
		if !ok {
			http.Error(w, "未知 method", http.StatusBadRequest)
			return
		}
		node := strings.ReplaceAll(method, ".", "_") + "_response"
		_, _ = fmt.Fprintf(w, `{"%s":%s,"sign":"%s"}`, node, payload, signatures[method])
	}))
	return fake
}

// alipayTestGateway 用现场生成的密钥对构造被测适配器：
// appKey 用于请求签名，alipayPublicKey 用于响应验签。
func alipayTestGateway(
	t *testing.T,
	gatewayURL string,
	appKey *rsa.PrivateKey,
	alipayPublicKey *rsa.PublicKey,
) *AlipayGateway {
	t.Helper()
	return NewAlipayGateway(AlipayOptions{
		AppID:      "2021000000000000",
		PrivateKey: alipayTestPrivateKeyBase64(t, appKey),
		PublicKey:  alipayTestPublicKeyBase64(t, alipayPublicKey),
		GatewayURL: gatewayURL,
		Timeout:    5 * time.Second,
	})
}

// alipayTestFormToParams 把请求表单转成待签串所需的 map（取每个键的首个值）。
func alipayTestFormToParams(form url.Values) map[string]string {
	params := make(map[string]string, len(form))
	for key, values := range form {
		if len(values) == 0 {
			continue
		}
		params[key] = values[0]
	}
	return params
}

// ---- 待签串拼接口径 ----

// TestAlipayBuildSignContent 覆盖待签串拼接口径（契约 §6.7）：
// 请求签名必须保留 sign_type 并剔除 sign 与空值；异步通知验签必须额外剔除 sign_type。
// 两者口径不同，混用会导致「请求被网关以 invalid-signature 拒绝」或「通知验签误判」。
func TestAlipayBuildSignContent(t *testing.T) {
	// 参数故意乱序，断言排序结果稳定；空值覆盖三种形态：空串、空白串、只出现在通知里的缺省值。
	params := map[string]string{
		"method":    "alipay.trade.precreate",
		"app_id":    "2021000000000000",
		"sign_type": "RSA2",
		"sign":      "should-be-dropped",
		"empty":     "",
		"blank":     "   ",
	}

	wantRequest := "app_id=2021000000000000&method=alipay.trade.precreate&sign_type=RSA2"
	if got := buildRequestSignContent(params); got != wantRequest {
		t.Fatalf("buildRequestSignContent = %q，期望 %q（必须保留 sign_type、剔除 sign 与空值）", got, wantRequest)
	}

	wantNotify := "app_id=2021000000000000&method=alipay.trade.precreate"
	if got := buildNotifySignContent(params); got != wantNotify {
		t.Fatalf("buildNotifySignContent = %q，期望 %q（必须额外剔除 sign_type）", got, wantNotify)
	}
}

// ---- 响应信封解析与业务错误码映射 ----

// TestAlipayDecodeNode 覆盖响应信封解析：正常响应取出业务节点原文与 sign；
// 缺少业务节点（含网关异常时的 error_response 形态）与非法 JSON 一律报 ErrAlipayUnavailable。
func TestAlipayDecodeNode(t *testing.T) {
	t.Run("正常取出业务节点原文与 sign", func(t *testing.T) {
		body := []byte(`{"alipay_trade_query_response":{"code":"10000","trade_status":"TRADE_SUCCESS"},"sign":"c2ln"}`)
		payload, sign, err := decodeAlipayNode(body, "alipay.trade.query")
		if err != nil {
			t.Fatalf("解析正常响应不应报错，实际 %v", err)
		}
		if string(payload) != `{"code":"10000","trade_status":"TRADE_SUCCESS"}` {
			t.Fatalf("业务节点原文 = %s，期望逐字节原样（响应验签依赖这一点）", payload)
		}
		if sign != "c2ln" {
			t.Fatalf("sign = %q，期望 c2ln", sign)
		}
	})

	t.Run("响应缺少业务节点", func(t *testing.T) {
		body := []byte(`{"alipay_trade_query_response_typo":{"code":"10000"},"sign":"c2ln"}`)
		if _, _, err := decodeAlipayNode(body, "alipay.trade.query"); !errors.Is(err, port.ErrAlipayUnavailable) {
			t.Fatalf("缺少业务节点期望 ErrAlipayUnavailable，实际 %v", err)
		}
	})

	t.Run("网关返回 error_response", func(t *testing.T) {
		body := []byte(`{"error_response":{"code":"20000","msg":"Service Currently Unavailable"}}`)
		if _, _, err := decodeAlipayNode(body, "alipay.trade.precreate"); !errors.Is(err, port.ErrAlipayUnavailable) {
			t.Fatalf("error_response 期望 ErrAlipayUnavailable，实际 %v", err)
		}
	})

	t.Run("响应不是合法 JSON", func(t *testing.T) {
		body := []byte("<html>502 Bad Gateway</html>")
		if _, _, err := decodeAlipayNode(body, "alipay.trade.query"); !errors.Is(err, port.ErrAlipayUnavailable) {
			t.Fatalf("非法 JSON 期望 ErrAlipayUnavailable，实际 %v", err)
		}
	})

	t.Run("缺少 sign 节点时不报错但 sign 为空", func(t *testing.T) {
		body := []byte(`{"alipay_trade_query_response":{"code":"10000"}}`)
		payload, sign, err := decodeAlipayNode(body, "alipay.trade.query")
		if err != nil {
			t.Fatalf("缺少 sign 不应报错（上层按无签名放行并告警），实际 %v", err)
		}
		if sign != "" {
			t.Fatalf("sign = %q，期望空字符串", sign)
		}
		if string(payload) != `{"code":"10000"}` {
			t.Fatalf("业务节点原文 = %s", payload)
		}
	})
}

// TestAlipayBizErrorMapping 覆盖业务错误码映射（契约 §6.2）：
// ACQ.TRADE_HAS_CLOSE / ACQ.TRADE_HAS_SUCCESS 是不可重试的冲突（ErrAlipayTradeClosed），
// 其余（含 code=20000 系统级错误）按可重试的服务不可用处理（ErrAlipayUnavailable）。
func TestAlipayBizErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		subCode string
		want    error
	}{
		{name: "ACQ.TRADE_HAS_CLOSE", subCode: "ACQ.TRADE_HAS_CLOSE", want: port.ErrAlipayTradeClosed},
		{name: "ACQ.TRADE_HAS_SUCCESS", subCode: "ACQ.TRADE_HAS_SUCCESS", want: port.ErrAlipayTradeClosed},
		{name: "ACQ.TRADE_NOT_EXIST", subCode: "ACQ.TRADE_NOT_EXIST", want: port.ErrAlipayUnavailable},
		{name: "ACQ.SYSTEM_ERROR", subCode: "ACQ.SYSTEM_ERROR", want: port.ErrAlipayUnavailable},
		{name: "未知 sub_code", subCode: "ACQ.UNKNOWN", want: port.ErrAlipayUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := alipayBizError("alipay.trade.precreate", "40004", tc.subCode, "Business Failed", "业务失败")
			if !errors.Is(err, tc.want) {
				t.Fatalf("sub_code=%s 期望错误链包含 %v，实际 %T：%v", tc.subCode, tc.want, err, err)
			}
			if tc.want == port.ErrAlipayUnavailable && errors.Is(err, port.ErrAlipayTradeClosed) {
				t.Fatalf("sub_code=%s 不得被判定为交易已关闭", tc.subCode)
			}
		})
	}
}

// ---- 未配置凭据 ----

// TestAlipayGatewayWithoutCredentialsIsUnavailable 未配置凭据时调用一律返回
// ErrAlipayUnavailable（上层映射 502），不得发起网络请求。
func TestAlipayGatewayWithoutCredentialsIsUnavailable(t *testing.T) {
	gateway := NewAlipayGateway(AlipayOptions{GatewayURL: "http://127.0.0.1:1/gateway.do"})

	_, err := gateway.Precreate(context.Background(), payment.PrecreateRequest{
		OutTradeNo: "202609080001",
		Amount:     "80.00",
		Subject:    "医院挂号费",
	})
	if !errors.Is(err, port.ErrAlipayUnavailable) {
		t.Fatalf("未配置凭据时预下单期望 ErrAlipayUnavailable，实际 %T：%v", err, err)
	}
	if _, err := gateway.QueryTrade(context.Background(), "202609080001"); !errors.Is(err, port.ErrAlipayUnavailable) {
		t.Fatalf("未配置凭据时主动查询期望 ErrAlipayUnavailable，实际 %T：%v", err, err)
	}
}

// ---- 响应验签 ----

// TestAlipayGatewayRejectsTamperedResponseSignature 覆盖响应验签：服务器返回的 sign 与
// 业务节点原文不匹配（用第三方私钥签名）时必须报 ErrAlipayUnavailable，
// 防止被篡改的响应（例如伪造的 qr_code）被交付给客户端。
func TestAlipayGatewayRejectsTamperedResponseSignature(t *testing.T) {
	appKey, _ := alipayTestKeyPair(t)
	alipayKey, alipayPublicKey := alipayTestKeyPair(t)
	tamperKey, _ := alipayTestKeyPair(t)

	server := alipayTestServer(t, alipayTestServerOptions{
		signKey:   alipayKey,
		tamperKey: tamperKey,
		payloads: map[string]string{
			"alipay.trade.precreate": `{"code":"10000","msg":"Success","qr_code":"https://qr.alipay.com/bax-tampered"}`,
		},
	})
	defer server.Close()

	gateway := alipayTestGateway(t, server.URL, appKey, alipayPublicKey)
	_, err := gateway.Precreate(context.Background(), payment.PrecreateRequest{
		OutTradeNo: "202609080001",
		Amount:     "80.00",
		Subject:    "医院挂号费",
	})
	if !errors.Is(err, port.ErrAlipayUnavailable) {
		t.Fatalf("响应验签失败期望 ErrAlipayUnavailable，实际 %T：%v", err, err)
	}
}

// ---- 预下单成功 ----

// TestAlipayGatewayPrecreateSuccess 覆盖预下单成功路径：请求签名、表单 POST 与响应验签全部通过后
// 返回 qr_code；同时断言发出去的请求确实带了公共参数、sign 与 sign_type（不访问外网）。
func TestAlipayGatewayPrecreateSuccess(t *testing.T) {
	const wantQRCode = "https://qr.alipay.com/bax-unit-test"
	const wantNotifyURL = "https://api.example.test/api/v1/payments/alipay/notify"

	appKey, appPublicKey := alipayTestKeyPair(t)
	alipayKey, alipayPublicKey := alipayTestKeyPair(t)
	server := alipayTestServer(t, alipayTestServerOptions{
		signKey: alipayKey,
		payloads: map[string]string{
			"alipay.trade.precreate": `{"code":"10000","msg":"Success","qr_code":"` + wantQRCode + `"}`,
		},
	})
	defer server.Close()

	gateway := alipayTestGateway(t, server.URL, appKey, alipayPublicKey)
	result, err := gateway.Precreate(context.Background(), payment.PrecreateRequest{
		OutTradeNo:     "202609080001",
		Amount:         "80.00",
		Subject:        "医院挂号费",
		TimeoutExpress: "30m",
		NotifyURL:      wantNotifyURL,
	})
	if err != nil {
		t.Fatalf("预下单期望成功，实际 %v", err)
	}
	if result == nil || result.QRCode != wantQRCode {
		t.Fatalf("预下单结果 = %+v，期望 qr_code=%s", result, wantQRCode)
	}

	requests := server.Requests()
	if len(requests) != 1 {
		t.Fatalf("假网关收到的请求数 = %d，期望 1", len(requests))
	}
	form := requests[0]
	if form.Get("method") != "alipay.trade.precreate" {
		t.Fatalf("method = %q，期望 alipay.trade.precreate", form.Get("method"))
	}
	if form.Get("sign_type") != "RSA2" || form.Get("sign") == "" {
		t.Fatalf("请求必须带 sign_type=RSA2 与非空 sign，实际 sign_type=%q sign=%q",
			form.Get("sign_type"), form.Get("sign"))
	}
	if form.Get("notify_url") != wantNotifyURL {
		t.Fatalf("notify_url = %q，期望 %q", form.Get("notify_url"), wantNotifyURL)
	}
	if !strings.Contains(form.Get("biz_content"), `"out_trade_no":"202609080001"`) {
		t.Fatalf("biz_content = %q，期望包含 out_trade_no", form.Get("biz_content"))
	}
	// 用自己的公钥验签请求，证明「待签串 + sign」自洽（sign_type 确实参与了请求签名）。
	if err := verifyRSA2(appPublicKey, buildRequestSignContent(alipayTestFormToParams(form)), form.Get("sign")); err != nil {
		t.Fatalf("请求签名自验失败（sign_type 可能未参与待签串）：%v", err)
	}
}

// ---- 主动查询：交易不存在 ----

// TestAlipayGatewayQueryTradeNotExist 覆盖主动查询的「交易不存在」：ACQ.TRADE_NOT_EXIST
// 是查询的正常结果，不得当成错误；返回空 TradeStatus 表示没有任何状态迁移证据（契约 §6.6）。
func TestAlipayGatewayQueryTradeNotExist(t *testing.T) {
	appKey, _ := alipayTestKeyPair(t)
	alipayKey, alipayPublicKey := alipayTestKeyPair(t)
	server := alipayTestServer(t, alipayTestServerOptions{
		signKey: alipayKey,
		payloads: map[string]string{
			"alipay.trade.query": `{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_NOT_EXIST","sub_msg":"交易不存在"}`,
		},
	})
	defer server.Close()

	gateway := alipayTestGateway(t, server.URL, appKey, alipayPublicKey)
	result, err := gateway.QueryTrade(context.Background(), "202609080001")
	if err != nil {
		t.Fatalf("交易不存在不应报错，实际 %v", err)
	}
	if result == nil {
		t.Fatal("交易不存在应返回非 nil 结果（TradeStatus 为空）")
	}
	if result.TradeStatus != "" {
		t.Fatalf("trade_status = %q，期望空字符串", result.TradeStatus)
	}
	if result.OutTradeNo != "202609080001" {
		t.Fatalf("out_trade_no = %q，期望回显请求值", result.OutTradeNo)
	}
}

// ---- 关单（alipay.trade.cancel） ----

// TestAlipayGatewayCancelTradeSuccess 覆盖关单成功：code=10000 时返回 nil；同时断言发出去的
// 请求 method 是 alipay.trade.cancel、biz_content 带 out_trade_no，并用请求方公钥自验签名，
// 证明关单复用了预下单/主动查询同一套「公共参数 + biz_content + RSA2 签名」口径（契约 §6.8）。
func TestAlipayGatewayCancelTradeSuccess(t *testing.T) {
	appKey, appPublicKey := alipayTestKeyPair(t)
	alipayKey, alipayPublicKey := alipayTestKeyPair(t)
	server := alipayTestServer(t, alipayTestServerOptions{
		signKey: alipayKey,
		payloads: map[string]string{
			"alipay.trade.cancel": `{"code":"10000","msg":"Success","out_trade_no":"202609080001","trade_no":"2026090822001456789012"}`,
		},
	})
	defer server.Close()

	gateway := alipayTestGateway(t, server.URL, appKey, alipayPublicKey)
	if err := gateway.CancelTrade(context.Background(), "202609080001"); err != nil {
		t.Fatalf("关单成功期望返回 nil，实际 %T：%v", err, err)
	}

	requests := server.Requests()
	if len(requests) != 1 {
		t.Fatalf("假网关收到的请求数 = %d，期望 1", len(requests))
	}
	form := requests[0]
	if form.Get("method") != "alipay.trade.cancel" {
		t.Fatalf("method = %q，期望 alipay.trade.cancel", form.Get("method"))
	}
	if !strings.Contains(form.Get("biz_content"), `"out_trade_no":"202609080001"`) {
		t.Fatalf("biz_content = %q，期望包含 out_trade_no", form.Get("biz_content"))
	}
	if form.Get("sign_type") != "RSA2" || form.Get("sign") == "" {
		t.Fatalf("关单请求必须带 sign_type=RSA2 与非空 sign，实际 sign_type=%q sign=%q",
			form.Get("sign_type"), form.Get("sign"))
	}
	if err := verifyRSA2(appPublicKey, buildRequestSignContent(alipayTestFormToParams(form)), form.Get("sign")); err != nil {
		t.Fatalf("关单请求签名自验失败：%v", err)
	}
}

// TestAlipayGatewayCancelTradeErrorMapping 覆盖关单业务错误码映射（契约 §6.8）：
// ACQ.TRADE_HAS_CLOSE / ACQ.TRADE_HAS_SUCCESS 是不可重试的冲突（ErrAlipayTradeClosed，
// 收口任务据此判定「支付宝侧已不可支付」），其余 sub_code 与 code != 10000 一律按可重试的
// 服务不可用处理（ErrAlipayUnavailable），两类都必须能被 errors.Is 判定。
func TestAlipayGatewayCancelTradeErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    error
	}{
		{
			name:    "ACQ.TRADE_HAS_CLOSE",
			payload: `{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_HAS_CLOSE","sub_msg":"交易已经关闭"}`,
			want:    port.ErrAlipayTradeClosed,
		},
		{
			name:    "ACQ.TRADE_HAS_SUCCESS",
			payload: `{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_HAS_SUCCESS","sub_msg":"交易已支付成功"}`,
			want:    port.ErrAlipayTradeClosed,
		},
		{
			name:    "未知 sub_code",
			payload: `{"code":"40004","msg":"Business Failed","sub_code":"ACQ.UNKNOWN","sub_msg":"未知业务失败"}`,
			want:    port.ErrAlipayUnavailable,
		},
		{
			name:    "系统级错误 code=20000",
			payload: `{"code":"20000","msg":"Service Currently Unavailable","sub_code":"aop.unknownerror"}`,
			want:    port.ErrAlipayUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			appKey, _ := alipayTestKeyPair(t)
			alipayKey, alipayPublicKey := alipayTestKeyPair(t)
			server := alipayTestServer(t, alipayTestServerOptions{
				signKey:  alipayKey,
				payloads: map[string]string{"alipay.trade.cancel": tc.payload},
			})
			defer server.Close()

			gateway := alipayTestGateway(t, server.URL, appKey, alipayPublicKey)
			err := gateway.CancelTrade(context.Background(), "202609080001")
			if !errors.Is(err, tc.want) {
				t.Fatalf("关单期望错误链包含 %v，实际 %T：%v", tc.want, err, err)
			}
			// 反向保护：服务不可用不得被误判为「交易已关闭」，否则收口会漏掉关单动作。
			if tc.want == port.ErrAlipayUnavailable && errors.Is(err, port.ErrAlipayTradeClosed) {
				t.Fatalf("关单错误不得被判定为交易已关闭：%v", err)
			}
		})
	}
}

// TestAlipayGatewayCancelTradeWithoutCredentialsIsUnavailable 未配置凭据时不发起网络请求，
// 直接返回 ErrAlipayUnavailable（与预下单/主动查询同一降级口径）。
func TestAlipayGatewayCancelTradeWithoutCredentialsIsUnavailable(t *testing.T) {
	gateway := NewAlipayGateway(AlipayOptions{GatewayURL: "http://127.0.0.1:1/gateway.do"})

	if err := gateway.CancelTrade(context.Background(), "202609080001"); !errors.Is(err, port.ErrAlipayUnavailable) {
		t.Fatalf("未配置凭据时关单期望 ErrAlipayUnavailable，实际 %T：%v", err, err)
	}
}
