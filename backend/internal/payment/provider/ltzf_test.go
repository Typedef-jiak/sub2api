package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

func TestLTZFSignUsesOnlyRequiredFields(t *testing.T) {
	params := map[string]string{
		"mch_id":       "1001",
		"out_trade_no": "order-1",
		"total_fee":    "12.34",
		"body":         "Sub2API recharge",
		"timestamp":    "1700000000",
		"notify_url":   "https://example.com/notify",
		"return_url":   "https://example.com/result",
	}
	got := ltzfSign(params, "merchant-secret", ltzfCreateSignFields)
	require.Equal(t, "787A7AB40601790BCC923210D98CFE1F", got)
}

func TestLTZFCreatePaymentRoutesByDevice(t *testing.T) {
	tests := []struct {
		name       string
		isMobile   bool
		wantPath   string
		response   string
		wantPayURL string
		wantQRCode string
	}{
		{
			name:       "desktop native",
			wantPath:   ltzfNativePath,
			response:   `{"code":0,"data":{"code_url":"weixin://wxpay/bizpayurl?pr=test","QRcode_url":"https://api.ltzf.cn/test.png"},"msg":"ok"}`,
			wantQRCode: "weixin://wxpay/bizpayurl?pr=test",
		},
		{
			name:       "mobile jump h5",
			isMobile:   true,
			wantPath:   ltzfMobilePath,
			response:   `{"code":0,"data":"https://api.ltzf.cn/template/html/jump_h5?state=test","msg":"ok"}`,
			wantPayURL: "https://api.ltzf.cn/template/html/jump_h5?state=test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, tt.wantPath, r.URL.Path)
				require.NoError(t, r.ParseForm())
				require.Equal(t, "1001", r.Form.Get("mch_id"))
				require.Equal(t, "order-1", r.Form.Get("out_trade_no"))
				require.Equal(t, "12.34", r.Form.Get("total_fee"))
				require.Equal(t, "https://example.com/notify", r.Form.Get("notify_url"))
				require.Empty(t, r.Form.Get("developer_appid"))
				require.Empty(t, r.Form.Get("time_expire"))
				require.Empty(t, r.Form.Get("title"))
				if tt.isMobile {
					require.Equal(t, "https://example.com/result", r.Form.Get("return_url"))
				}
				require.NotEmpty(t, r.Form.Get("sign"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.response))
			}))
			defer server.Close()

			provider := newTestLTZF(server.URL, server.Client())
			provider.now = func() time.Time { return time.Unix(1700000000, 0) }
			result, err := provider.CreatePayment(context.Background(), payment.CreatePaymentRequest{
				OrderID: "order-1", Amount: "12.34", Subject: "Sub2API recharge",
				NotifyURL: "https://example.com/notify", ReturnURL: "https://example.com/result", IsMobile: tt.isMobile,
			})

			require.NoError(t, err)
			require.Equal(t, "order-1", result.TradeNo)
			require.Equal(t, tt.wantPayURL, result.PayURL)
			require.Equal(t, tt.wantQRCode, result.QRCode)
		})
	}
}

func TestLTZFQueryOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, ltzfQueryPath, r.URL.Path)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "1001", r.Form.Get("mch_id"))
		require.Equal(t, "order-1", r.Form.Get("out_trade_no"))
		_, _ = w.Write([]byte(`{"code":0,"data":{"mch_id":"1001","out_trade_no":"order-1","pay_no":"420001","total_fee":"12.34","pay_status":1,"success_time":"2026-08-06 12:00:00"},"msg":"ok"}`))
	}))
	defer server.Close()

	result, err := newTestLTZF(server.URL, server.Client()).QueryOrder(context.Background(), "order-1")
	require.NoError(t, err)
	require.Equal(t, payment.ProviderStatusPaid, result.Status)
	require.Equal(t, "420001", result.TradeNo)
	require.Equal(t, 12.34, result.Amount)
	require.Equal(t, "1001", result.Metadata["mchid"])
}

func TestLTZFVerifyNotification(t *testing.T) {
	provider := newTestLTZF("https://api.ltzf.cn", http.DefaultClient)
	values := url.Values{
		"code": {"0"}, "timestamp": {"1700000000"}, "mch_id": {"1001"},
		"order_no": {"WX123"}, "out_trade_no": {"order-1"}, "pay_no": {"420001"}, "total_fee": {"12.34"},
	}
	signed := make(map[string]string, len(ltzfNotifySignFields))
	for _, key := range ltzfNotifySignFields {
		signed[key] = values.Get(key)
	}
	values.Set("sign", ltzfSign(signed, "merchant-secret", ltzfNotifySignFields))

	notification, err := provider.VerifyNotification(context.Background(), values.Encode(), nil)
	require.NoError(t, err)
	require.Equal(t, payment.ProviderStatusSuccess, notification.Status)
	require.Equal(t, "order-1", notification.OrderID)
	require.Equal(t, "420001", notification.TradeNo)
	require.Equal(t, 12.34, notification.Amount)
	require.Equal(t, "1001", notification.Metadata["mchid"])

	t.Run("rejects another merchant", func(t *testing.T) {
		foreign := cloneLTZFValues(values)
		foreign.Set("mch_id", "other")
		foreignSigned := make(map[string]string, len(ltzfNotifySignFields))
		for _, key := range ltzfNotifySignFields {
			foreignSigned[key] = foreign.Get(key)
		}
		foreign.Set("sign", ltzfSign(foreignSigned, "merchant-secret", ltzfNotifySignFields))
		_, verifyErr := provider.VerifyNotification(context.Background(), foreign.Encode(), nil)
		require.ErrorContains(t, verifyErr, "merchant mismatch")
	})

	t.Run("rejects invalid signature", func(t *testing.T) {
		invalid := cloneLTZFValues(values)
		invalid.Set("sign", strings.Repeat("0", 32))
		_, verifyErr := provider.VerifyNotification(context.Background(), invalid.Encode(), nil)
		require.ErrorContains(t, verifyErr, "invalid signature")
	})
}

func newTestLTZF(apiBase string, client *http.Client) *LTZF {
	return &LTZF{
		instanceID: "test",
		config:     map[string]string{"mchId": "1001", "merchantKey": "merchant-secret", "notifyUrl": "https://example.com/notify"},
		apiBase:    apiBase, httpClient: client, now: time.Now,
	}
}

func cloneLTZFValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for key, items := range values {
		cloned[key] = append([]string(nil), items...)
	}
	return cloned
}
