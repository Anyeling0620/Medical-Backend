package repo

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"Medical-Web-Backend/internal/domain/payment"
	"Medical-Web-Backend/internal/port"
)

// 支付宝网关的固定公共参数与业务码。
const (
	// alipaySignTypeRSA2 是本阶段唯一使用的签名算法（RSA2 = SHA256withRSA）。
	alipaySignTypeRSA2 = "RSA2"
	alipayCharset      = "utf-8"
	alipayFormat       = "JSON"
	alipayVersion      = "1.0"
	// alipaySuccessCode 是支付宝网关的业务成功码。
	alipaySuccessCode = "10000"
	// alipayTradeNotExistSubCode 表示交易不存在：主动查询的正常结果，不是故障。
	alipayTradeNotExistSubCode = "ACQ.TRADE_NOT_EXIST"
	// alipayTradeHasCloseSubCode/alipayTradeHasSuccessSubCode 表示该 out_trade_no 的交易
	// 已关闭/已成功，属于不可重试的冲突（契约 §6.2）。
	alipayTradeHasCloseSubCode   = "ACQ.TRADE_HAS_CLOSE"
	alipayTradeHasSuccessSubCode = "ACQ.TRADE_HAS_SUCCESS"
	// alipayDefaultTimeout 是单次网关调用的默认超时：外部调用必须有上限，
	// 否则会把挂号/取码请求的尾延迟绑到支付宝的响应时间上。
	alipayDefaultTimeout = 5 * time.Second
)

// alipayLocation 是支付宝网关要求的时间戳时区（GMT+8，无夏令时）。
var alipayLocation = time.FixedZone("GMT+8", 8*60*60)

// AlipayOptions 是支付宝当面付适配器的构造参数。
//
// 凭据缺失不阻止服务启动（与微信适配器同一取舍），但调用时一律返回
// port.ErrAlipayUnavailable，由上层映射为 502 PAYMENT_PROVIDER_UNAVAILABLE。
type AlipayOptions struct {
	// AppID 是支付宝开放平台的应用 ID。
	AppID string
	// PrivateKey 是应用私钥（PKCS#1 或 PKCS#8 的 base64，允许带 PEM 头），用于请求签名。
	PrivateKey string
	// PublicKey 是支付宝公钥，用于异步通知验签与响应验签。
	PublicKey string
	// GatewayURL 是网关地址：生产 https://openapi.alipay.com/gateway.do，
	// 沙箱 https://openapi-sandbox.dl.alipaydev.com/gateway.do。
	GatewayURL string
	// SellerIDs 是允许的 seller_id 集合；为空表示跳过该项校验（配置缺失时降级并告警）。
	SellerIDs []string
	// Timeout 是单次网关调用超时，缺省 5 秒。
	Timeout time.Duration
	// HTTPClient 允许注入自定义客户端（测试用）。
	HTTPClient *http.Client
}

// AlipayGateway 是 port.AlipayGateway 的真实实现：用标准库完成
// 请求签名（RSA2）、表单 POST、响应验签与异步通知验签，不引入第三方 SDK。
//
// 支付宝统一下单/查询接口都是「公共参数 + biz_content + sign」的表单 POST，
// 签名串是「剔除 sign/sign_type 与空值后按参数名升序用 & 连接的 k=v」，
// 请求与通知验签共用同一套拼接口径（契约 §6.7）。
type AlipayGateway struct {
	appID      string
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	gatewayURL string
	sellerIDs  map[string]bool
	client     *http.Client
}

// NewAlipayGateway 构造支付宝适配器；密钥解析失败只告警不阻断启动，
// 具体调用会返回 port.ErrAlipayUnavailable（与微信适配器一致的降级方式）。
func NewAlipayGateway(opts AlipayOptions) *AlipayGateway {
	gateway := &AlipayGateway{
		appID:      strings.TrimSpace(opts.AppID),
		gatewayURL: strings.TrimSpace(opts.GatewayURL),
		sellerIDs:  make(map[string]bool, len(opts.SellerIDs)),
	}
	for _, sellerID := range opts.SellerIDs {
		if trimmed := strings.TrimSpace(sellerID); trimmed != "" {
			gateway.sellerIDs[trimmed] = true
		}
	}
	if strings.TrimSpace(opts.PrivateKey) != "" {
		privateKey, err := parseAlipayPrivateKey(opts.PrivateKey)
		if err != nil {
			log.Printf("警告：支付宝应用私钥解析失败，支付接口将返回 PAYMENT_PROVIDER_UNAVAILABLE：%v", err)
		} else {
			gateway.privateKey = privateKey
		}
	}
	if strings.TrimSpace(opts.PublicKey) != "" {
		publicKey, err := parseAlipayPublicKey(opts.PublicKey)
		if err != nil {
			log.Printf("警告：支付宝公钥解析失败，异步通知验签与响应验签会失败：%v", err)
		} else {
			gateway.publicKey = publicKey
		}
	}
	gateway.client = opts.HTTPClient
	if gateway.client == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = alipayDefaultTimeout
		}
		gateway.client = &http.Client{Timeout: timeout}
	}
	return gateway
}

// Ready 报告网关是否具备发起主动查询/关单所需的最小有效配置。
// 后台收口任务在启动前调用它，避免无效私钥被误判为运行期网络抖动并释放号源。
func (g *AlipayGateway) Ready() error {
	return g.readyToCall()
}

// Precreate 调用 alipay.trade.precreate 生成付款二维码（契约 §6.2 第 5 步）。
func (g *AlipayGateway) Precreate(
	ctx context.Context,
	req payment.PrecreateRequest,
) (*payment.PrecreateResult, error) {
	if err := g.readyToCall(); err != nil {
		return nil, err
	}
	bizContent := map[string]string{
		"out_trade_no": req.OutTradeNo,
		"total_amount": req.Amount,
		"subject":      req.Subject,
	}
	if strings.TrimSpace(req.TimeoutExpress) != "" {
		bizContent["timeout_express"] = req.TimeoutExpress
	}
	payload, err := g.call(ctx, "alipay.trade.precreate", bizContent, req.NotifyURL)
	if err != nil {
		return nil, err
	}
	var result struct {
		Code    string `json:"code"`
		Msg     string `json:"msg"`
		SubCode string `json:"sub_code"`
		SubMsg  string `json:"sub_msg"`
		QRCode  string `json:"qr_code"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, fmt.Errorf("%w: 解析预下单响应失败: %v", port.ErrAlipayUnavailable, err)
	}
	if result.Code != alipaySuccessCode {
		return nil, alipayBizError("alipay.trade.precreate", result.Code, result.SubCode, result.Msg, result.SubMsg)
	}
	if strings.TrimSpace(result.QRCode) == "" {
		return nil, fmt.Errorf("%w: 预下单未返回 qr_code", port.ErrAlipayUnavailable)
	}
	return &payment.PrecreateResult{QRCode: result.QRCode}, nil
}

// QueryTrade 调用 alipay.trade.query 查询交易状态（契约 §6.6 的兜底主动查询）。
func (g *AlipayGateway) QueryTrade(
	ctx context.Context,
	outTradeNo string,
) (*payment.TradeQueryResult, error) {
	if err := g.readyToCall(); err != nil {
		return nil, err
	}
	payload, err := g.call(ctx, "alipay.trade.query", map[string]string{
		"out_trade_no": outTradeNo,
	}, "")
	if err != nil {
		return nil, err
	}
	var result struct {
		Code        string `json:"code"`
		Msg         string `json:"msg"`
		SubCode     string `json:"sub_code"`
		SubMsg      string `json:"sub_msg"`
		OutTradeNo  string `json:"out_trade_no"`
		TradeNo     string `json:"trade_no"`
		TradeStatus string `json:"trade_status"`
		TotalAmount string `json:"total_amount"`
		GmtPayment  string `json:"gmt_payment"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, fmt.Errorf("%w: 解析交易查询响应失败: %v", port.ErrAlipayUnavailable, err)
	}
	if result.Code != alipaySuccessCode {
		if result.SubCode == alipayTradeNotExistSubCode {
			// 交易不存在是查询的正常结果，不构成任何状态迁移证据。
			return &payment.TradeQueryResult{OutTradeNo: outTradeNo}, nil
		}
		return nil, alipayBizError("alipay.trade.query", result.Code, result.SubCode, result.Msg, result.SubMsg)
	}
	return &payment.TradeQueryResult{
		OutTradeNo:  result.OutTradeNo,
		TradeNo:     result.TradeNo,
		TradeStatus: result.TradeStatus,
		TotalAmount: result.TotalAmount,
		GmtPayment:  result.GmtPayment,
	}, nil
}

// CancelTrade 调用 alipay.trade.cancel 关闭支付宝侧交易（契约 §6.8 的关单步骤）。
//
// 调用口径与 precreate/query 完全一致：公共参数 + biz_content + RSA2 签名，复用 call 与
// 响应验签，不引入第三套实现。关单失败不阻塞收口，因此这里刻意不做重试，只分类返回错误：
// ACQ.TRADE_HAS_CLOSE / ACQ.TRADE_HAS_SUCCESS 由 alipayBizError 归为 ErrAlipayTradeClosed，
// 其余业务码、网络失败与系统级错误归为 ErrAlipayUnavailable（创建订单与支付业务说明.md 第 7 节）。
func (g *AlipayGateway) CancelTrade(ctx context.Context, outTradeNo string) error {
	if err := g.readyToCall(); err != nil {
		return err
	}
	payload, err := g.call(ctx, "alipay.trade.cancel", map[string]string{
		"out_trade_no": outTradeNo,
	}, "")
	if err != nil {
		return err
	}
	var result struct {
		Code    string `json:"code"`
		Msg     string `json:"msg"`
		SubCode string `json:"sub_code"`
		SubMsg  string `json:"sub_msg"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return fmt.Errorf("%w: 解析关单响应失败: %v", port.ErrAlipayUnavailable, err)
	}
	if result.Code != alipaySuccessCode {
		return alipayBizError("alipay.trade.cancel", result.Code, result.SubCode, result.Msg, result.SubMsg)
	}
	return nil
}

// VerifyNotify 校验异步通知的签名与身份（契约 §6.7 第 1、2 步）。
//
// 步骤固定为：剔除 sign、sign_type 与空值参数 -> 按参数名升序拼接待签串（值取 URL 解码后的
// 原始值，不做二次编码）-> RSA2 验签 -> 校验 app_id 与 seller_id。
// 验签与身份校验失败都是不可恢复异常，返回稳定错误供上层映射为 400 并告警。
func (g *AlipayGateway) VerifyNotify(
	ctx context.Context,
	form url.Values,
) (*payment.NotifyPayload, error) {
	if g == nil || g.publicKey == nil {
		return nil, fmt.Errorf("%w: 未配置支付宝公钥", port.ErrAlipayUnavailable)
	}
	params := make(map[string]string, len(form))
	for key, values := range form {
		if len(values) == 0 {
			continue
		}
		params[key] = values[0]
	}
	if signType := params["sign_type"]; signType != "" && !strings.EqualFold(signType, alipaySignTypeRSA2) {
		return nil, fmt.Errorf("%w: sign_type 不是 RSA2", port.ErrNotifySignatureInvalid)
	}
	signature := strings.TrimSpace(params["sign"])
	if signature == "" {
		return nil, fmt.Errorf("%w: 通知缺少 sign", port.ErrNotifySignatureInvalid)
	}
	if err := verifyRSA2(g.publicKey, buildNotifySignContent(params), signature); err != nil {
		return nil, fmt.Errorf("%w: %v", port.ErrNotifySignatureInvalid, err)
	}
	if g.appID != "" && params["app_id"] != g.appID {
		return nil, fmt.Errorf("%w: app_id 与服务端配置不一致", port.ErrNotifyIdentityMismatch)
	}
	if len(g.sellerIDs) > 0 && !g.sellerIDs[strings.TrimSpace(params["seller_id"])] {
		return nil, fmt.Errorf("%w: seller_id 不在允许集合内", port.ErrNotifyIdentityMismatch)
	}
	return &payment.NotifyPayload{
		AppID:       params["app_id"],
		SellerID:    params["seller_id"],
		OutTradeNo:  strings.TrimSpace(params["out_trade_no"]),
		TradeNo:     strings.TrimSpace(params["trade_no"]),
		TradeStatus: strings.TrimSpace(params["trade_status"]),
		TotalAmount: strings.TrimSpace(params["total_amount"]),
		GmtPayment:  strings.TrimSpace(params["gmt_payment"]),
		NotifyTime:  strings.TrimSpace(params["notify_time"]),
	}, nil
}

// readyToCall 断言调用网关所需的最小配置齐备。
func (g *AlipayGateway) readyToCall() error {
	if g == nil || g.appID == "" || g.privateKey == nil {
		return fmt.Errorf("%w: 未配置 ALIPAY_APP_ID/ALIPAY_APP_PRIVATE_SECRET", port.ErrAlipayUnavailable)
	}
	if g.gatewayURL == "" {
		return fmt.Errorf("%w: 未配置支付宝网关地址", port.ErrAlipayUnavailable)
	}
	return nil
}

// call 组装公共参数、签名、发起表单 POST，并返回验签后的业务响应节点。
func (g *AlipayGateway) call(
	ctx context.Context,
	method string,
	bizContent map[string]string,
	notifyURL string,
) (json.RawMessage, error) {
	content, err := json.Marshal(bizContent)
	if err != nil {
		return nil, fmt.Errorf("%w: 序列化 biz_content 失败: %v", port.ErrAlipayUnavailable, err)
	}
	params := map[string]string{
		"app_id":      g.appID,
		"method":      method,
		"format":      alipayFormat,
		"charset":     alipayCharset,
		"sign_type":   alipaySignTypeRSA2,
		"timestamp":   time.Now().In(alipayLocation).Format("2006-01-02 15:04:05"),
		"version":     alipayVersion,
		"biz_content": string(content),
	}
	// notify_url 未配置时不传该参数，系统降级为主动查询模式（契约 §6.7）。
	if strings.TrimSpace(notifyURL) != "" {
		params["notify_url"] = notifyURL
	}
	signature, err := signRSA2(g.privateKey, buildRequestSignContent(params))
	if err != nil {
		return nil, fmt.Errorf("%w: 请求签名失败: %v", port.ErrAlipayUnavailable, err)
	}
	params["sign"] = signature

	form := url.Values{}
	for key, value := range params {
		form.Set(key, value)
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, g.gatewayURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", port.ErrAlipayUnavailable, err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")

	response, err := g.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: 调用支付宝网关失败: %v", port.ErrAlipayUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()

	// 限制读取长度，避免异常响应体占用内存。
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: 读取支付宝响应失败: %v", port.ErrAlipayUnavailable, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: 支付宝网关 HTTP %d", port.ErrAlipayUnavailable, response.StatusCode)
	}

	payload, sign, err := decodeAlipayNode(body, method)
	if err != nil {
		return nil, err
	}
	// 配置了支付宝公钥时必须验签同步响应，防止响应被篡改；
	// 未返回 sign 时只告警放行（部分网关/沙箱渠道不返回响应签名）。
	if sign == "" {
		log.Printf("警告：支付宝 %s 响应未携带 sign，跳过响应验签", method)
	} else if g.publicKey != nil {
		if err := verifyRSA2(g.publicKey, string(payload), sign); err != nil {
			return nil, fmt.Errorf("%w: 支付宝响应验签失败: %v", port.ErrAlipayUnavailable, err)
		}
	} else {
		// 网关返回了签名但本地没有公钥：无法验证响应是否被篡改，属于配置缺失，
		// 与「响应不带 sign」区分开告警，便于定位 ALIPAY_PUBLIC_SECRET 漏配。
		log.Printf("警告：支付宝 %s 响应携带 sign 但未配置支付宝公钥，跳过响应验签", method)
	}
	return payload, nil
}

// decodeAlipayNode 从网关响应中取出业务节点原文与 sign。
// 业务节点原文必须保持逐字节原样，才能用于响应验签。
func decodeAlipayNode(body []byte, method string) (json.RawMessage, string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, "", fmt.Errorf("%w: 解析支付宝响应失败: %v", port.ErrAlipayUnavailable, err)
	}
	node := strings.ReplaceAll(method, ".", "_") + "_response"
	payload, ok := envelope[node]
	if !ok {
		// 网关异常时返回 {"error_response":{...}}，统一按服务不可用处理。
		return nil, "", fmt.Errorf("%w: 支付宝响应缺少 %s 节点", port.ErrAlipayUnavailable, node)
	}
	var sign string
	if raw, ok := envelope["sign"]; ok {
		_ = json.Unmarshal(raw, &sign)
	}
	return payload, sign, nil
}

// alipayBizError 把业务失败映射为稳定错误：交易已关闭/已成功不可重试，
// 其余（含 code=20000 系统级错误）按可重试的服务不可用处理（契约 §6.2）。
func alipayBizError(method, code, subCode, msg, subMsg string) error {
	if subCode == alipayTradeHasCloseSubCode || subCode == alipayTradeHasSuccessSubCode {
		return fmt.Errorf("%w: %s code=%s sub_code=%s", port.ErrAlipayTradeClosed, method, code, subCode)
	}
	return fmt.Errorf(
		"%w: %s code=%s sub_code=%s msg=%s sub_msg=%s",
		port.ErrAlipayUnavailable, method, code, subCode, msg, subMsg)
}

// buildRequestSignContent 拼装请求（gateway.do）的待签串：剔除 sign 与空值参数后，
// 按参数名升序用 & 连接 k=v，值取原始值（不做二次编码）。
//
// 注意：请求签名必须**保留 sign_type**。这是支付宝 OpenAPI 1.0 gateway.do 的口径，
// 网关在 isv.invalid-signature 报错里回显的待签串也包含 sign_type=RSA2。
// 它与异步通知验签（剔除 sign、sign_type）不同，因此两者不能共用同一个函数。
func buildRequestSignContent(params map[string]string) string {
	return buildSignContent(params, false)
}

// buildNotifySignContent 拼装异步通知的待签串：剔除 sign、sign_type 与空值参数
// （契约 §6.7 第 1 步），其余口径与请求签名一致。
func buildNotifySignContent(params map[string]string) string {
	return buildSignContent(params, true)
}

// buildSignContent 按支付宝口径拼接待签串：剔除 sign 与空值参数，
// skipSignType 为 true 时同时剔除 sign_type（仅异步通知验签使用）。
func buildSignContent(params map[string]string, skipSignType bool) string {
	keys := make([]string, 0, len(params))
	for key, value := range params {
		if key == "sign" || strings.TrimSpace(value) == "" {
			continue
		}
		if skipSignType && key == "sign_type" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+params[key])
	}
	return strings.Join(parts, "&")
}

// signRSA2 用应用私钥做 SHA256withRSA 签名并 base64 编码。
func signRSA2(privateKey *rsa.PrivateKey, content string) (string, error) {
	hashed := sha256.Sum256([]byte(content))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hashed[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

// verifyRSA2 用 RSA2（SHA256withRSA）校验签名。
func verifyRSA2(publicKey *rsa.PublicKey, content, signature string) error {
	decoded, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return errors.New("签名不是合法的 base64")
	}
	hashed := sha256.Sum256([]byte(content))
	return rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, hashed[:], decoded)
}

// parseAlipayPrivateKey 解析应用私钥，兼容 PKCS#8 与 PKCS#1 两种编码。
func parseAlipayPrivateKey(raw string) (*rsa.PrivateKey, error) {
	block, err := decodePEM(raw, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block); err == nil {
		privateKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("应用私钥不是 RSA 私钥")
		}
		return privateKey, nil
	}
	return x509.ParsePKCS1PrivateKey(block)
}

// parseAlipayPublicKey 解析支付宝公钥，兼容 X.509 SubjectPublicKeyInfo 与 PKCS#1 两种编码。
func parseAlipayPublicKey(raw string) (*rsa.PublicKey, error) {
	block, err := decodePEM(raw, "PUBLIC KEY")
	if err != nil {
		return nil, err
	}
	if parsed, err := x509.ParsePKIXPublicKey(block); err == nil {
		publicKey, ok := parsed.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("支付宝公钥不是 RSA 公钥")
		}
		return publicKey, nil
	}
	return x509.ParsePKCS1PublicKey(block)
}

// decodePEM 取出 PEM 块中的 DER 字节；密钥在配置里可能是不带头尾的一行 base64
// （支付宝开放平台复制的格式），这里补上 PEM 头尾并按 64 字符换行后再解析。
func decodePEM(raw, blockType string) ([]byte, error) {
	content := strings.TrimSpace(raw)
	if content == "" {
		return nil, errors.New("密钥为空")
	}
	if !strings.Contains(content, "-----BEGIN") {
		content = wrapPEM(strings.ReplaceAll(strings.ReplaceAll(content, "\r", ""), "\n", ""), blockType)
	}
	block, _ := pem.Decode([]byte(content))
	if block == nil {
		return nil, errors.New("密钥不是合法的 PEM 格式")
	}
	return block.Bytes, nil
}

// wrapPEM 把裸 base64 密钥补成标准 PEM 文本。
func wrapPEM(body, blockType string) string {
	var builder strings.Builder
	builder.WriteString("-----BEGIN " + blockType + "-----\n")
	for index := 0; index < len(body); index += 64 {
		end := index + 64
		if end > len(body) {
			end = len(body)
		}
		builder.WriteString(body[index:end])
		builder.WriteString("\n")
	}
	builder.WriteString("-----END " + blockType + "-----\n")
	return builder.String()
}
