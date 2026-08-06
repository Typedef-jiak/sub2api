package provider

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

const (
	ltzfDefaultAPIBase  = "https://api.ltzf.cn"
	ltzfHTTPTimeout     = 10 * time.Second
	ltzfMaxResponseSize = 1 << 20
	ltzfNativePath      = "/api/wxpay/native"
	ltzfMobilePath      = "/api/wxpay/jump_h5"
	ltzfQueryPath       = "/api/wxpay/get_pay_order"
	ltzfResponseSuccess = 0
	ltzfPayStatusPaid   = 1
)

var (
	ltzfCreateSignFields = []string{"body", "mch_id", "notify_url", "out_trade_no", "timestamp", "total_fee"}
	ltzfQuerySignFields  = []string{"mch_id", "out_trade_no", "timestamp"}
	ltzfNotifySignFields = []string{"code", "mch_id", "order_no", "out_trade_no", "pay_no", "timestamp", "total_fee"}
)

// LTZF implements 蓝兔支付 WeChat Native and jump H5 payments.
type LTZF struct {
	instanceID string
	config     map[string]string
	apiBase    string
	httpClient *http.Client
	now        func() time.Time
}

// NewLTZF creates a 蓝兔支付 provider.
// Required config keys: mchId, merchantKey, notifyUrl.
func NewLTZF(instanceID string, config map[string]string) (*LTZF, error) {
	for _, key := range []string{"mchId", "merchantKey", "notifyUrl"} {
		if strings.TrimSpace(config[key]) == "" {
			return nil, fmt.Errorf("ltzf config missing required key: %s", key)
		}
	}
	copied := make(map[string]string, len(config))
	for key, value := range config {
		copied[key] = value
	}
	apiBase := strings.TrimRight(strings.TrimSpace(copied["apiBase"]), "/")
	if apiBase == "" {
		apiBase = ltzfDefaultAPIBase
	}
	return &LTZF{
		instanceID: instanceID,
		config:     copied,
		apiBase:    apiBase,
		httpClient: &http.Client{Timeout: ltzfHTTPTimeout},
		now:        time.Now,
	}, nil
}

func (l *LTZF) Name() string        { return "LTZF" }
func (l *LTZF) ProviderKey() string { return payment.TypeLTZF }
func (l *LTZF) SupportedTypes() []payment.PaymentType {
	return []payment.PaymentType{payment.TypeWxpay}
}

func (l *LTZF) MerchantIdentityMetadata() map[string]string {
	if l == nil {
		return nil
	}
	mchID := strings.TrimSpace(l.config["mchId"])
	if mchID == "" {
		return nil
	}
	return map[string]string{"mchid": mchID, "currency": payment.DefaultPaymentCurrency}
}

func (l *LTZF) CreatePayment(ctx context.Context, req payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	notifyURL := strings.TrimSpace(req.NotifyURL)
	if notifyURL == "" {
		notifyURL = strings.TrimSpace(l.config["notifyUrl"])
	}
	if notifyURL == "" {
		return nil, fmt.Errorf("ltzf notifyUrl is required")
	}
	totalFen, err := payment.YuanToFen(req.Amount)
	if err != nil {
		return nil, fmt.Errorf("ltzf create payment amount: %w", err)
	}
	params := map[string]string{
		"mch_id":       strings.TrimSpace(l.config["mchId"]),
		"out_trade_no": strings.TrimSpace(req.OrderID),
		"total_fee":    fmt.Sprintf("%.2f", payment.FenToYuan(totalFen)),
		"body":         strings.TrimSpace(req.Subject),
		"timestamp":    strconv.FormatInt(l.currentTime().Unix(), 10),
		"notify_url":   notifyURL,
	}
	if params["out_trade_no"] == "" || params["body"] == "" {
		return nil, fmt.Errorf("ltzf order id and subject are required")
	}
	for _, key := range []string{"timeExpire", "developerAppId", "attach"} {
		if value := strings.TrimSpace(l.config[key]); value != "" {
			params[configToLTZFField(key)] = value
		}
	}
	params["sign"] = ltzfSign(params, l.config["merchantKey"], ltzfCreateSignFields)

	endpoint := l.apiBase + ltzfNativePath
	if req.IsMobile {
		endpoint = l.apiBase + ltzfMobilePath
		returnURL := strings.TrimSpace(req.ReturnURL)
		if returnURL == "" {
			returnURL = strings.TrimSpace(l.config["returnUrl"])
		}
		if returnURL != "" {
			params["return_url"] = returnURL
		}
	}

	body, status, err := l.post(ctx, endpoint, params)
	if err != nil {
		return nil, fmt.Errorf("ltzf create payment request: %w", err)
	}
	var response ltzfAPIResponse
	if err := decodeLTZFResponse(status, body, &response); err != nil {
		return nil, fmt.Errorf("ltzf create payment: %w", err)
	}
	if req.IsMobile {
		var payURL string
		if err := json.Unmarshal(response.Data, &payURL); err != nil || strings.TrimSpace(payURL) == "" {
			return nil, fmt.Errorf("ltzf mobile response missing payment URL")
		}
		return &payment.CreatePaymentResponse{TradeNo: req.OrderID, PayURL: payURL}, nil
	}
	var data struct {
		CodeURL   string `json:"code_url"`
		QRCodeURL string `json:"QRcode_url"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		return nil, fmt.Errorf("ltzf parse native response: %w", err)
	}
	qrCode := strings.TrimSpace(data.CodeURL)
	if qrCode == "" {
		qrCode = strings.TrimSpace(data.QRCodeURL)
	}
	if qrCode == "" {
		return nil, fmt.Errorf("ltzf native response missing code_url")
	}
	return &payment.CreatePaymentResponse{TradeNo: req.OrderID, QRCode: qrCode}, nil
}

func (l *LTZF) QueryOrder(ctx context.Context, tradeNo string) (*payment.QueryOrderResponse, error) {
	tradeNo = strings.TrimSpace(tradeNo)
	if tradeNo == "" {
		return nil, fmt.Errorf("ltzf query requires out_trade_no")
	}
	params := map[string]string{
		"mch_id":       strings.TrimSpace(l.config["mchId"]),
		"out_trade_no": tradeNo,
		"timestamp":    strconv.FormatInt(l.currentTime().Unix(), 10),
	}
	params["sign"] = ltzfSign(params, l.config["merchantKey"], ltzfQuerySignFields)
	body, status, err := l.post(ctx, l.apiBase+ltzfQueryPath, params)
	if err != nil {
		return nil, fmt.Errorf("ltzf query request: %w", err)
	}
	var response ltzfAPIResponse
	if err := decodeLTZFResponse(status, body, &response); err != nil {
		return nil, fmt.Errorf("ltzf query order: %w", err)
	}
	var data struct {
		MchID       string          `json:"mch_id"`
		OutTradeNo  string          `json:"out_trade_no"`
		PayNo       string          `json:"pay_no"`
		TotalFee    string          `json:"total_fee"`
		PayStatus   json.RawMessage `json:"pay_status"`
		TradeState  string          `json:"trade_state"`
		SuccessTime string          `json:"success_time"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		return nil, fmt.Errorf("ltzf parse query response: %w", err)
	}
	if data.MchID != "" && !strings.EqualFold(strings.TrimSpace(data.MchID), strings.TrimSpace(l.config["mchId"])) {
		return nil, fmt.Errorf("ltzf query merchant mismatch")
	}
	statusValue := payment.ProviderStatusPending
	if ltzfPaidStatus(data.PayStatus, data.TradeState) {
		statusValue = payment.ProviderStatusPaid
	}
	amount, err := strconv.ParseFloat(strings.TrimSpace(data.TotalFee), 64)
	if err != nil {
		amount = 0
	}
	return &payment.QueryOrderResponse{
		TradeNo: strings.TrimSpace(firstNonEmpty(data.PayNo, data.OutTradeNo)),
		Status:  statusValue,
		Amount:  amount,
		PaidAt:  strings.TrimSpace(data.SuccessTime),
		Metadata: map[string]string{
			"mchid":    strings.TrimSpace(firstNonEmpty(data.MchID, l.config["mchId"])),
			"currency": payment.DefaultPaymentCurrency,
		},
	}, nil
}

func (l *LTZF) VerifyNotification(_ context.Context, rawBody string, _ map[string]string) (*payment.PaymentNotification, error) {
	values, err := url.ParseQuery(rawBody)
	if err != nil {
		return nil, fmt.Errorf("ltzf parse notification: %w", err)
	}
	params := make(map[string]string, len(values))
	for key := range values {
		params[key] = values.Get(key)
	}
	for _, key := range append(append([]string{}, ltzfNotifySignFields...), "sign") {
		if strings.TrimSpace(params[key]) == "" {
			return nil, fmt.Errorf("ltzf notification missing %s", key)
		}
	}
	if !strings.EqualFold(strings.TrimSpace(params["mch_id"]), strings.TrimSpace(l.config["mchId"])) {
		return nil, fmt.Errorf("ltzf notification merchant mismatch")
	}
	expected := ltzfSign(params, l.config["merchantKey"], ltzfNotifySignFields)
	if !hmac.Equal([]byte(expected), []byte(strings.ToUpper(strings.TrimSpace(params["sign"])))) {
		return nil, fmt.Errorf("ltzf notification invalid signature")
	}
	amount, err := strconv.ParseFloat(strings.TrimSpace(params["total_fee"]), 64)
	if err != nil || amount <= 0 {
		return nil, fmt.Errorf("ltzf notification invalid total_fee")
	}
	status := payment.ProviderStatusFailed
	tradeState := "FAIL"
	if params["code"] == "0" {
		status = payment.ProviderStatusSuccess
		tradeState = "SUCCESS"
	}
	return &payment.PaymentNotification{
		TradeNo: params["pay_no"], OrderID: params["out_trade_no"], Amount: amount,
		Status: status, RawData: rawBody,
		Metadata: map[string]string{
			"mchid": params["mch_id"], "currency": payment.DefaultPaymentCurrency,
			"trade_state": tradeState,
		},
	}, nil
}

func (l *LTZF) Refund(context.Context, payment.RefundRequest) (*payment.RefundResponse, error) {
	return nil, fmt.Errorf("ltzf refunds are not supported")
}

type ltzfAPIResponse struct {
	Code      int             `json:"code"`
	Data      json.RawMessage `json:"data"`
	Msg       string          `json:"msg"`
	RequestID string          `json:"request_id"`
}

func (l *LTZF) post(ctx context.Context, endpoint string, params map[string]string) ([]byte, int, error) {
	form := url.Values{}
	for key, value := range params {
		form.Set(key, value)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := l.httpClient
	if client == nil {
		client = &http.Client{Timeout: ltzfHTTPTimeout}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, ltzfMaxResponseSize+1))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if len(body) > ltzfMaxResponseSize {
		return nil, response.StatusCode, fmt.Errorf("response exceeds %d bytes", ltzfMaxResponseSize)
	}
	return body, response.StatusCode, nil
}

func decodeLTZFResponse(status int, body []byte, response *ltzfAPIResponse) error {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return fmt.Errorf("upstream HTTP status %d", status)
	}
	if err := json.Unmarshal(body, response); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if response.Code != ltzfResponseSuccess {
		return fmt.Errorf("upstream code %d: %s", response.Code, strings.TrimSpace(response.Msg))
	}
	if len(response.Data) == 0 || string(response.Data) == "null" {
		return fmt.Errorf("upstream response missing data")
	}
	return nil
}

func ltzfSign(params map[string]string, key string, fields []string) string {
	allowed := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		allowed[field] = struct{}{}
	}
	keys := make([]string, 0, len(fields))
	for field := range allowed {
		if strings.TrimSpace(params[field]) != "" {
			keys = append(keys, field)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	for _, field := range keys {
		parts = append(parts, field+"="+params[field])
	}
	parts = append(parts, "key="+key)
	digest := md5.Sum([]byte(strings.Join(parts, "&")))
	return strings.ToUpper(hex.EncodeToString(digest[:]))
}

func (l *LTZF) currentTime() time.Time {
	if l != nil && l.now != nil {
		return l.now()
	}
	return time.Now()
}

func ltzfPaidStatus(raw json.RawMessage, tradeState string) bool {
	if value := strings.Trim(strings.TrimSpace(string(raw)), `"`); value != "" {
		if value == strconv.Itoa(ltzfPayStatusPaid) {
			return true
		}
	}
	return strings.EqualFold(strings.TrimSpace(tradeState), "SUCCESS") || strings.EqualFold(strings.TrimSpace(tradeState), "PAID")
}

func configToLTZFField(key string) string {
	switch key {
	case "timeExpire":
		return "time_expire"
	case "developerAppId":
		return "developer_appid"
	default:
		return key
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

var _ payment.Provider = (*LTZF)(nil)
var _ payment.MerchantIdentityProvider = (*LTZF)(nil)
