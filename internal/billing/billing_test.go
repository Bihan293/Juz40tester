package billing

import (
	"testing"
	"time"
)

func cat(t *testing.T) *Catalog {
	t.Helper()
	c, err := NewCatalog(DefaultPlans())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaultCatalogMatchesSpec(t *testing.T) {
	c := cat(t)
	want := map[string][2]int{"free": {0, 1}, "plus": {10, 4}, "pro": {40, 10}, "premium": {50, 20}}
	for code, w := range want {
		p, ok := c.Get(code)
		if !ok || p.PriceStars != w[0] || p.DailyLimit != w[1] {
			t.Fatalf("%s: got %+v, want price %d limit %d", code, p, w[0], w[1])
		}
	}
	if got := len(c.Paid()); got != 3 {
		t.Fatalf("paid plans: %d", got)
	}
	if c.Plans()[0].Code != PlanFree {
		t.Fatal("free must be first")
	}
}

func TestCatalogValidation(t *testing.T) {
	bad := [][]Plan{
		{},
		{{Code: "plus", PriceStars: 10, DailyLimit: 4, Rank: 1}},                                         // no free
		{{Code: "free", PriceStars: 5, DailyLimit: 1}},                                                   // paid free
		{{Code: "free", DailyLimit: 0}},                                                                  // zero limit
		{{Code: "free", DailyLimit: 1}, {Code: "plus", PriceStars: 0, DailyLimit: 2, Rank: 1}},           // free paid plan
		{{Code: "free", DailyLimit: 1}, {Code: "plus", PriceStars: 20000, DailyLimit: 2, Rank: 1}},       // > 10000
		{{Code: "free", DailyLimit: 1}, {Code: "plus", PriceStars: 10, DailyLimit: 2, Rank: 0}},          // dup rank
		{{Code: "free", DailyLimit: 1, Rank: 5}, {Code: "plus", PriceStars: 10, DailyLimit: 2, Rank: 1}}, // free not lowest
		{{Code: "free", DailyLimit: 1}, {Code: "free", DailyLimit: 1, Rank: 1}},                          // dup code
	}
	for i, ps := range bad {
		if _, err := NewCatalog(ps); err == nil {
			t.Fatalf("case %d: want error", i)
		}
	}
}

func TestEffectivePlanExpiry(t *testing.T) {
	c := cat(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if p := c.Effective("pro", now.Add(time.Hour), now, 0); p.Code != "pro" {
		t.Fatalf("active: %s", p.Code)
	}
	if p := c.Effective("pro", now.Add(-time.Minute), now, 0); p.Code != "free" {
		t.Fatalf("expired: %s", p.Code)
	}
	if p := c.Effective("pro", now.Add(-time.Minute), now, time.Hour); p.Code != "pro" {
		t.Fatalf("grace: %s", p.Code)
	}
	if p := c.Effective("gold", now.Add(time.Hour), now, 0); p.Code != "free" {
		t.Fatalf("unknown plan: %s", p.Code)
	}
	if p := c.Effective("", time.Time{}, now, time.Hour); p.Code != "free" {
		t.Fatalf("none: %s", p.Code)
	}
}

func TestCalendarAlmatyMidnight(t *testing.T) {
	cal := Calendar{Loc: LoadLocation("Asia/Almaty")}
	// 18:59:59 UTC = 23:59:59 in Almaty (UTC+5) — still Oct 8.
	day, start, next := cal.Day(time.Date(2026, 10, 8, 18, 59, 59, 0, time.UTC))
	if day != "2026-10-08" {
		t.Fatalf("day before midnight: %s", day)
	}
	if !next.Equal(time.Date(2026, 10, 8, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("reset: %s", next.UTC())
	}
	if !start.Equal(time.Date(2026, 10, 7, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("start: %s", start.UTC())
	}
	// One second later it is Oct 9 in Almaty (UTC day is still Oct 8).
	if day, _, _ := cal.Day(time.Date(2026, 10, 8, 19, 0, 0, 0, time.UTC)); day != "2026-10-09" {
		t.Fatalf("after midnight: %s", day)
	}
	if LoadLocation("Nowhere/Nothing") == nil {
		t.Fatal("fallback location")
	}
}

func TestRemainingAndLeft(t *testing.T) {
	cases := []struct{ limit, used, open, rem, left int }{
		{1, 0, 0, 1, 1},
		{1, 1, 0, 0, 0},
		{1, 0, 1, 0, 1},
		{4, 2, 1, 1, 2},
		{1, 3, 0, 0, 0}, // finished tests started yesterday may exceed the limit
	}
	for _, c := range cases {
		if got := Remaining(c.limit, c.used, c.open); got != c.rem {
			t.Fatalf("Remaining(%d,%d,%d)=%d want %d", c.limit, c.used, c.open, got, c.rem)
		}
		if got := Left(c.limit, c.used); got != c.left {
			t.Fatalf("Left(%d,%d)=%d want %d", c.limit, c.used, got, c.left)
		}
	}
}

func TestPayloads(t *testing.T) {
	if p, ok := ParsePayload(SubscriptionPayload("pro")); !ok || p.Kind != KindSubscription || p.Plan != "pro" {
		t.Fatalf("sub payload: %+v %v", p, ok)
	}
	if p, ok := ParsePayload(WeakTestPayload(42)); !ok || p.Kind != KindWeakTest || p.OrderID != 42 {
		t.Fatalf("weak payload: %+v %v", p, ok)
	}
	for _, s := range []string{"", "sub:", "sub:free", "weak:x", "weak:0", "weak:-1", "foo:1", "sub:a b"} {
		if _, ok := ParsePayload(s); ok {
			t.Fatalf("%q must be rejected", s)
		}
	}
}

func TestCanBuy(t *testing.T) {
	c := cat(t)
	free, _ := c.Get("free")
	plus, _ := c.Get("plus")
	pro, _ := c.Get("pro")
	cases := []struct {
		cur    Plan
		target string
		want   BuyVerdict
	}{
		{free, "plus", BuyAllowed},
		{free, "premium", BuyAllowed},
		{plus, "pro", BuyAllowed},
		{pro, "pro", BuyAlreadyActive},
		{pro, "plus", BuyDowngrade},
		{free, "free", BuyUnknownPlan},
		{free, "gold", BuyUnknownPlan},
	}
	for _, cs := range cases {
		if _, got := c.CanBuy(cs.cur, cs.target); got != cs.want {
			t.Fatalf("%s -> %s: %v want %v", cs.cur.Code, cs.target, got, cs.want)
		}
	}
}

func TestApplySubscriptionPayment(t *testing.T) {
	c := cat(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	exp := now.Add(SubscriptionPeriod)

	// New from free.
	d := c.ApplySubscriptionPayment(nil, SubPayment{Plan: "plus", ChargeID: "c1", ExpiresAt: exp}, now, 0)
	if d.Kind != DecisionNew || d.New.Plan != "plus" || !d.New.ExpiresAt.Equal(exp) || d.New.SubChargeID != "c1" || d.Refund || d.CancelChargeID != "" {
		t.Fatalf("new: %+v %+v", d, d.New)
	}
	// No expiration date sent: now + 30 days.
	d = c.ApplySubscriptionPayment(nil, SubPayment{Plan: "plus", ChargeID: "c1"}, now, 0)
	if !d.New.ExpiresAt.Equal(exp) {
		t.Fatalf("default expiry: %s", d.New.ExpiresAt)
	}

	cur := &SubState{Plan: "plus", ExpiresAt: exp, SubChargeID: "c1", ChargeID: "c1", Source: SourceStars}
	// Renewal (recurring charge) of the same plan: expiry moves, sub id kept.
	exp2 := exp.Add(SubscriptionPeriod)
	d = c.ApplySubscriptionPayment(cur, SubPayment{Plan: "plus", ChargeID: "c2", ExpiresAt: exp2, IsRecurring: true}, exp.Add(-time.Hour), 0)
	if d.Kind != DecisionRenewal || !d.New.ExpiresAt.Equal(exp2) || d.New.SubChargeID != "c1" || d.New.ChargeID != "c2" || d.CancelChargeID != "" {
		t.Fatalf("renewal: %+v %+v", d, d.New)
	}
	// A renewal never shortens the period.
	d = c.ApplySubscriptionPayment(cur, SubPayment{Plan: "plus", ChargeID: "c3", ExpiresAt: now.Add(time.Hour), IsRecurring: true}, now, 0)
	if !d.New.ExpiresAt.Equal(exp) {
		t.Fatalf("renewal shortened: %s", d.New.ExpiresAt)
	}
	// The same plan bought twice (two invoices paid before the first
	// payment was applied): the second NEW subscription is refunded and
	// cancelled, the current one keeps its auto-renewal and expiry.
	d = c.ApplySubscriptionPayment(cur, SubPayment{Plan: "plus", ChargeID: "c4", ExpiresAt: exp.Add(time.Minute), IsRecurring: true, IsFirstRecurring: true}, now, 0)
	if d.Kind != DecisionDuplicate || !d.Refund || d.CancelChargeID != "c4" || d.New != nil {
		t.Fatalf("duplicate purchase: %+v", d)
	}
	// Upgrade: applies at once, the old subscription is cancelled.
	d = c.ApplySubscriptionPayment(cur, SubPayment{Plan: "pro", ChargeID: "p1", ExpiresAt: exp.Add(48 * time.Hour)}, now, 0)
	if d.Kind != DecisionUpgrade || d.New.Plan != "pro" || d.CancelChargeID != "c1" || d.New.SubChargeID != "p1" || d.Refund {
		t.Fatalf("upgrade: %+v %+v", d, d.New)
	}
	// A cheaper plan while a better one is active: refund + cancel it.
	pro := &SubState{Plan: "pro", ExpiresAt: exp, SubChargeID: "p1", Source: SourceStars}
	d = c.ApplySubscriptionPayment(pro, SubPayment{Plan: "plus", ChargeID: "c9", IsRecurring: true}, now, 0)
	if d.Kind != DecisionStaleLower || !d.Refund || d.CancelChargeID != "c9" || d.New != nil {
		t.Fatalf("stale lower: %+v", d)
	}
	// Expired plan: any payment starts a new period.
	old := &SubState{Plan: "premium", ExpiresAt: now.Add(-48 * time.Hour), SubChargeID: "x", Source: SourceStars}
	d = c.ApplySubscriptionPayment(old, SubPayment{Plan: "plus", ChargeID: "n1", ExpiresAt: exp}, now, 0)
	if d.Kind != DecisionNew || d.New.Plan != "plus" {
		t.Fatalf("after expiry: %+v", d)
	}
	// Admin-granted plan: a Stars purchase of the same plan does not try to
	// cancel a Telegram subscription that does not exist.
	adm := &SubState{Plan: "pro", ExpiresAt: exp, Source: SourceAdmin}
	d = c.ApplySubscriptionPayment(adm, SubPayment{Plan: "pro", ChargeID: "s1", ExpiresAt: exp.Add(time.Hour)}, now, 0)
	if d.Kind != DecisionRenewal || d.CancelChargeID != "" || d.New.Source != SourceStars || d.New.SubChargeID != "s1" {
		t.Fatalf("admin -> stars: %+v %+v", d, d.New)
	}
	// Admin granted Premium over a Stars Plus subscription; a Plus renewal
	// raced the cancellation: accepted, plan kept, no refund, cancel again.
	admHi := &SubState{Plan: "premium", ExpiresAt: exp, SubChargeID: "c1", Source: SourceAdmin}
	d = c.ApplySubscriptionPayment(admHi, SubPayment{Plan: "plus", ChargeID: "c7", IsRecurring: true}, now, 0)
	if d.Kind != DecisionAdminKept || d.Refund || d.New != nil || d.CancelChargeID != "c1" {
		t.Fatalf("renewal under an admin plan: %+v", d)
	}
	// … and a renewal of a MORE expensive plan does not override it either.
	admLo := &SubState{Plan: "plus", ExpiresAt: exp, SubChargeID: "p1", Source: SourceAdmin}
	d = c.ApplySubscriptionPayment(admLo, SubPayment{Plan: "pro", ChargeID: "p7", IsRecurring: true}, now, 0)
	if d.Kind != DecisionAdminKept || d.Refund || d.New != nil {
		t.Fatalf("higher renewal under an admin plan: %+v", d)
	}
	// A renewal of the SAME plan (the subscription was kept): extends.
	d = c.ApplySubscriptionPayment(admLo, SubPayment{Plan: "plus", ChargeID: "c8", ExpiresAt: exp.Add(SubscriptionPeriod), IsRecurring: true}, now, 0)
	if d.Kind != DecisionRenewal || d.Refund || d.New.SubChargeID != "p1" {
		t.Fatalf("same-plan renewal under an admin plan: %+v %+v", d, d.New)
	}
	// The user subscribes to a cheaper plan himself (grace window): applies.
	d = c.ApplySubscriptionPayment(admHi, SubPayment{Plan: "plus", ChargeID: "n9", ExpiresAt: exp, IsRecurring: true, IsFirstRecurring: true}, now, 0)
	if d.Kind != DecisionNew || d.Refund || d.New.Plan != "plus" || d.CancelChargeID != "c1" {
		t.Fatalf("new cheaper subscription after an admin plan: %+v", d)
	}
	// Admin set Free (revoked row): a late renewal keeps the user on Free.
	rev := &SubState{Plan: "plus", ExpiresAt: exp, SubChargeID: "c1", Source: SourceAdmin, Revoked: true}
	d = c.ApplySubscriptionPayment(rev, SubPayment{Plan: "plus", ChargeID: "c5", IsRecurring: true}, now, 0)
	if d.Kind != DecisionAdminKept || d.Refund || d.New != nil || d.CancelChargeID != "c1" {
		t.Fatalf("renewal after an admin revoke: %+v", d)
	}
	// … but a NEW subscription the user buys himself applies.
	d = c.ApplySubscriptionPayment(rev, SubPayment{Plan: "plus", ChargeID: "c6", ExpiresAt: exp, IsRecurring: true, IsFirstRecurring: true}, now, 0)
	if d.Kind != DecisionNew || d.New.Plan != "plus" {
		t.Fatalf("new subscription after an admin revoke: %+v", d)
	}
	// A refunded (revoked, source stars) row is simply not in force.
	refd := &SubState{Plan: "pro", ExpiresAt: exp, SubChargeID: "p1", Source: SourceStars, Revoked: true}
	d = c.ApplySubscriptionPayment(refd, SubPayment{Plan: "plus", ChargeID: "c10", ExpiresAt: exp}, now, 0)
	if d.Kind != DecisionNew || d.Refund {
		t.Fatalf("payment after a refunded plan: %+v", d)
	}
	// Unknown plan in the payload: refund.
	d = c.ApplySubscriptionPayment(nil, SubPayment{Plan: "gold", ChargeID: "g"}, now, 0)
	if !d.Refund {
		t.Fatalf("unknown plan must be refunded: %+v", d)
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:             "меньше минуты",
		12 * time.Minute:             "12 мин",
		5 * time.Hour:                "5 ч",
		5*time.Hour + 12*time.Minute: "5 ч 12 мин",
	} {
		if got := HumanDuration(d); got != want {
			t.Fatalf("%s: %q want %q", d, got, want)
		}
	}
}
