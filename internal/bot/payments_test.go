package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Telegram Stars: the payment updates decode, the API calls send the
// documented parameters, setWebhook subscribes to pre_checkout_query.
func TestStarsPaymentsAPI(t *testing.T) {
	var mu sync.Mutex
	got := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		m := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		mu.Lock()
		got[m] = p
		mu.Unlock()
		switch m {
		case "createInvoiceLink":
			_, _ = io.WriteString(w, `{"ok":true,"result":"https://t.me/$x"}`)
		case "refundStarPayment":
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: CHARGE_ALREADY_REFUNDED"}`)
		case "sendInvoice":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":7,"chat":{"id":1,"type":"private"}}}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	defer srv.Close()
	c := NewClient("T").WithBaseURL(srv.URL)
	ctx := context.Background()
	inv := Invoice{Title: "JUZ40 Plus", Description: "d", Payload: "sub:plus", Prices: []LabeledPrice{{Label: "Plus", Amount: 10}}, SubscriptionPeriod: 2592000}
	link, err := c.CreateInvoiceLink(ctx, inv)
	if err != nil || link != "https://t.me/$x" {
		t.Fatalf("link: %q %v", link, err)
	}
	if p := got["createInvoiceLink"]; p["currency"] != "XTR" || p["subscription_period"] != float64(2592000) || p["provider_token"] != nil {
		t.Fatalf("createInvoiceLink params: %v", p)
	}
	if _, err := c.SendInvoice(ctx, 1, inv, nil); err != nil {
		t.Fatal(err)
	}
	if p := got["sendInvoice"]; p["subscription_period"] != nil || p["chat_id"] != float64(1) {
		t.Fatalf("sendInvoice params: %v", p)
	}
	if err := c.AnswerPreCheckoutQuery(ctx, "q", false, "нет"); err != nil {
		t.Fatal(err)
	}
	if p := got["answerPreCheckoutQuery"]; p["ok"] != false || p["error_message"] != "нет" {
		t.Fatalf("answerPreCheckoutQuery: %v", p)
	}
	if err := c.EditUserStarSubscription(ctx, 5, "ch", true); err != nil {
		t.Fatal(err)
	}
	if p := got["editUserStarSubscription"]; p["is_canceled"] != true || p["telegram_payment_charge_id"] != "ch" {
		t.Fatalf("editUserStarSubscription: %v", p)
	}
	err = c.RefundStarPayment(ctx, 5, "ch")
	if err == nil || !IsAlreadyRefunded(err) {
		t.Fatalf("already refunded: %v", err)
	}
	if err := c.SetWebhook(ctx, "https://x", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(toStrings(got["setWebhook"]["allowed_updates"]), ","), "pre_checkout_query") {
		t.Fatalf("allowed_updates: %v", got["setWebhook"]["allowed_updates"])
	}

	var u Update
	raw := `{"update_id":1,"message":{"message_id":2,"from":{"id":3},"chat":{"id":3,"type":"private"},
		"successful_payment":{"currency":"XTR","total_amount":10,"invoice_payload":"sub:plus","subscription_expiration_date":1790000000,
		"is_recurring":true,"is_first_recurring":true,"telegram_payment_charge_id":"tch","provider_payment_charge_id":""}}}`
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	sp := u.Message.SuccessfulPayment
	if sp == nil || sp.TotalAmount != 10 || sp.SubscriptionExpirationDate != 1790000000 || !sp.IsRecurring || !sp.IsFirstRecurring || sp.TelegramPaymentChargeID != "tch" {
		t.Fatalf("successful_payment: %+v", sp)
	}
}

func toStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func TestTestEnvironmentPath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer srv.Close()
	c := NewClient("TOK").WithBaseURL(srv.URL).WithTestEnvironment(true)
	if err := c.AnswerPreCheckoutQuery(context.Background(), "q", true, ""); err != nil {
		t.Fatal(err)
	}
	if path != "/botTOK/test/answerPreCheckoutQuery" {
		t.Fatalf("path %q", path)
	}
}
