// Package billing holds the pure (database-free) rules of the Telegram Stars
// monetisation: the plan catalog, the daily-quota calendar, invoice
// payloads, the purchase rules (upgrade / downgrade) and the way a
// subscription payment changes the user's subscription. Everything here is
// deterministic and unit-tested; the repositories and services only store
// and apply the decisions.
package billing

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Plan codes. The free plan is always present; the paid ones are
// configured (price / daily limit) through the environment.
const (
	PlanFree    = "free"
	PlanPlus    = "plus"
	PlanPro     = "pro"
	PlanPremium = "premium"
)

// CurrencyStars is the Telegram Stars currency code (XTR).
const CurrencyStars = "XTR"

// SubscriptionPeriod is the only subscription period Telegram supports
// (createInvoiceLink.subscription_period = 30 days).
const SubscriptionPeriod = 30 * 24 * time.Hour

// SubscriptionPeriodSeconds is SubscriptionPeriod in seconds (2592000).
const SubscriptionPeriodSeconds = 2592000

// Plan is one tariff.
type Plan struct {
	Code       string // free | plus | pro | premium
	Title      string // shown to the user
	PriceStars int    // monthly price in Stars (0 = free)
	DailyLimit int    // completed tests per day
	Rank       int    // ordering: a higher rank is a better plan
}

// IsFree reports whether p is the free plan.
func (p Plan) IsFree() bool { return p.Code == PlanFree }

// DefaultPlans is the catalog of the owner's specification:
// Free 1/day, Plus 10⭐ 4/day, Pro 40⭐ 10/day, Premium 50⭐ 20/day.
func DefaultPlans() []Plan {
	return []Plan{
		{Code: PlanFree, Title: "Free", PriceStars: 0, DailyLimit: 1, Rank: 0},
		{Code: PlanPlus, Title: "Plus", PriceStars: 10, DailyLimit: 4, Rank: 1},
		{Code: PlanPro, Title: "Pro", PriceStars: 40, DailyLimit: 10, Rank: 2},
		{Code: PlanPremium, Title: "Premium", PriceStars: 50, DailyLimit: 20, Rank: 3},
	}
}

// Catalog is the validated, immutable set of plans.
type Catalog struct {
	plans  []Plan // sorted by Rank
	byCode map[string]Plan
}

// NewCatalog validates the plans: exactly one free plan (price 0), unique
// codes and ranks, paid plans priced 1..10000 Stars (the Telegram limit of
// a subscription), daily limits >= 1.
func NewCatalog(plans []Plan) (*Catalog, error) {
	if len(plans) == 0 {
		return nil, errors.New("billing: empty plan catalog")
	}
	c := &Catalog{byCode: make(map[string]Plan, len(plans))}
	ranks := map[int]bool{}
	for _, p := range plans {
		p.Code = strings.ToLower(strings.TrimSpace(p.Code))
		if p.Code == "" {
			return nil, errors.New("billing: plan without a code")
		}
		if _, dup := c.byCode[p.Code]; dup {
			return nil, fmt.Errorf("billing: duplicate plan %q", p.Code)
		}
		if ranks[p.Rank] {
			return nil, fmt.Errorf("billing: duplicate rank %d", p.Rank)
		}
		ranks[p.Rank] = true
		if p.DailyLimit < 1 {
			return nil, fmt.Errorf("billing: plan %q: daily limit must be >= 1", p.Code)
		}
		if p.Code == PlanFree {
			if p.PriceStars != 0 {
				return nil, errors.New("billing: the free plan must cost 0")
			}
		} else if p.PriceStars < 1 || p.PriceStars > 10000 {
			return nil, fmt.Errorf("billing: plan %q: price must be 1..10000 Stars", p.Code)
		}
		if p.Title == "" {
			p.Title = strings.ToUpper(p.Code[:1]) + p.Code[1:]
		}
		c.byCode[p.Code] = p
		c.plans = append(c.plans, p)
	}
	free, ok := c.byCode[PlanFree]
	if !ok {
		return nil, errors.New("billing: the free plan is missing")
	}
	sort.Slice(c.plans, func(i, j int) bool { return c.plans[i].Rank < c.plans[j].Rank })
	if c.plans[0].Code != free.Code {
		return nil, errors.New("billing: the free plan must have the lowest rank")
	}
	return c, nil
}

// MustCatalog is NewCatalog for known-good plans (tests, defaults).
func MustCatalog(plans []Plan) *Catalog {
	c, err := NewCatalog(plans)
	if err != nil {
		panic(err)
	}
	return c
}

// Plans returns every plan, cheapest first.
func (c *Catalog) Plans() []Plan { return append([]Plan(nil), c.plans...) }

// Paid returns the paid plans, cheapest first.
func (c *Catalog) Paid() []Plan {
	out := make([]Plan, 0, len(c.plans))
	for _, p := range c.plans {
		if !p.IsFree() {
			out = append(out, p)
		}
	}
	return out
}

// Free returns the free plan.
func (c *Catalog) Free() Plan { return c.byCode[PlanFree] }

// Get returns the plan by code.
func (c *Catalog) Get(code string) (Plan, bool) {
	p, ok := c.byCode[strings.ToLower(strings.TrimSpace(code))]
	return p, ok
}

// Effective returns the plan that is in force at now: the stored plan while
// it has not expired (expiresAt + grace > now), the free plan otherwise.
// An unknown stored code (a plan removed from the configuration) also
// falls back to free.
func (c *Catalog) Effective(code string, expiresAt time.Time, now time.Time, grace time.Duration) Plan {
	if code == "" || expiresAt.IsZero() || !now.Before(expiresAt.Add(grace)) {
		return c.Free()
	}
	p, ok := c.Get(code)
	if !ok {
		return c.Free()
	}
	return p
}

// --- Daily quota calendar --------------------------------------------------

// Calendar computes the quota day ("today" switches at local midnight of
// QUOTA_TZ, Asia/Almaty by default).
type Calendar struct {
	Loc *time.Location
}

// Day returns the calendar day of t as "YYYY-MM-DD" (a PostgreSQL DATE),
// the moment it started and the moment the next one starts (the reset).
func (c Calendar) Day(t time.Time) (day string, start, next time.Time) {
	loc := c.Loc
	if loc == nil {
		loc = time.UTC
	}
	lt := t.In(loc)
	y, m, d := lt.Date()
	start = time.Date(y, m, d, 0, 0, 0, 0, loc)
	next = time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	return start.Format("2006-01-02"), start, next
}

// LoadLocation loads a time zone, falling back to UTC+5 (Kazakhstan since
// 2024) when the name is unknown or the system has no tzdata.
func LoadLocation(name string) *time.Location {
	if name == "" {
		name = "Asia/Almaty"
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.FixedZone("UTC+5", 5*60*60)
}

// Remaining is the number of tests the user may still START today: the
// daily limit minus the completions already charged today minus the
// non-personal attempts started today that are still in progress (each of
// them will be charged when finished — counting them prevents opening ten
// tests in parallel with one completion left). Never negative.
func Remaining(limit, used, openToday int) int {
	r := limit - used - openToday
	if r < 0 {
		return 0
	}
	return r
}

// Left is limit - used (what the user still gets charged for today;
// open attempts not included), never negative — the «Осталось сегодня»
// figure of the subscription screen.
func Left(limit, used int) int {
	if used >= limit {
		return 0
	}
	return limit - used
}

// --- Invoice payloads -------------------------------------------------------

// Payload kinds.
const (
	KindSubscription = "subscription"
	KindWeakTest     = "weak_test"
)

const (
	subPrefix  = "sub:"
	weakPrefix = "weak:"
)

// SubscriptionPayload is the invoice payload of a plan subscription.
func SubscriptionPayload(plan string) string { return subPrefix + plan }

// WeakTestPayload is the invoice payload of a weak-topics test order.
func WeakTestPayload(orderID int64) string { return weakPrefix + strconv.FormatInt(orderID, 10) }

// Payload is a parsed invoice payload.
type Payload struct {
	Kind    string // KindSubscription | KindWeakTest
	Plan    string // subscription plan code
	OrderID int64  // weak-test order id
}

// ParsePayload parses an invoice payload; ok is false for anything the bot
// did not issue.
func ParsePayload(s string) (Payload, bool) {
	switch {
	case strings.HasPrefix(s, subPrefix):
		plan := strings.TrimPrefix(s, subPrefix)
		if plan == "" || plan == PlanFree || strings.ContainsAny(plan, ": ") {
			return Payload{}, false
		}
		return Payload{Kind: KindSubscription, Plan: plan}, true
	case strings.HasPrefix(s, weakPrefix):
		id, err := strconv.ParseInt(strings.TrimPrefix(s, weakPrefix), 10, 64)
		if err != nil || id <= 0 {
			return Payload{}, false
		}
		return Payload{Kind: KindWeakTest, OrderID: id}, true
	}
	return Payload{}, false
}

// --- Purchase rules -----------------------------------------------------------

// BuyVerdict is the answer to "may the user buy plan X now?".
type BuyVerdict int

const (
	// BuyAllowed: a new subscription (from free / expired) or an upgrade.
	BuyAllowed BuyVerdict = iota
	// BuyAlreadyActive: the same plan is active (it renews by itself).
	BuyAlreadyActive
	// BuyDowngrade: a cheaper plan while a better one is active — allowed
	// only once the current one has ended (cancel its auto-renewal in
	// Telegram → it expires at the end of the paid period).
	BuyDowngrade
	// BuyUnknownPlan: no such paid plan.
	BuyUnknownPlan
)

// CanBuy applies the purchase rules: upgrade at once, the same plan or a
// downgrade is refused while the current plan is active.
func (c *Catalog) CanBuy(current Plan, target string) (Plan, BuyVerdict) {
	p, ok := c.Get(target)
	if !ok || p.IsFree() {
		return Plan{}, BuyUnknownPlan
	}
	switch {
	case current.IsFree() || p.Rank > current.Rank:
		return p, BuyAllowed
	case p.Rank == current.Rank:
		return p, BuyAlreadyActive
	default:
		return p, BuyDowngrade
	}
}

// --- Applying a subscription payment ---------------------------------------

// Subscription sources.
const (
	SourceStars = "stars"
	SourceAdmin = "admin"
)

// SubState is the stored subscription of one user.
type SubState struct {
	Plan      string
	ExpiresAt time.Time
	// SubChargeID identifies the Telegram Stars subscription (the
	// telegram_payment_charge_id of its first payment); used to cancel the
	// auto-renewal of a replaced subscription.
	SubChargeID string
	ChargeID    string // last payment
	Source      string // stars | admin
}

// SubPayment is a successful_payment of a subscription invoice.
type SubPayment struct {
	Plan             string
	ChargeID         string
	ExpiresAt        time.Time // subscription_expiration_date (zero when absent)
	IsRecurring      bool
	IsFirstRecurring bool
}

// Decision kinds.
const (
	DecisionNew        = "new"         // from free / expired
	DecisionRenewal    = "renewal"     // same plan, extended
	DecisionUpgrade    = "upgrade"     // better plan, applies at once
	DecisionStaleLower = "stale_lower" // cheaper plan while a better one is active
)

// SubDecision is how a payment changes the subscription.
type SubDecision struct {
	Kind string
	// New is the subscription after the payment (nil = unchanged).
	New *SubState
	// CancelChargeID: the Telegram subscription whose auto-renewal must be
	// cancelled (editUserStarSubscription), "" = none.
	CancelChargeID string
	// Refund: the payment bought nothing (a renewal of a cheaper,
	// replaced subscription) and is refunded automatically.
	Refund bool
}

// expiryOf returns the end of the paid period: the expiration date sent by
// Telegram, otherwise base + 30 days.
func expiryOf(p SubPayment, base time.Time) time.Time {
	if !p.ExpiresAt.IsZero() {
		return p.ExpiresAt
	}
	return base.Add(SubscriptionPeriod)
}

// isNewSubscription: the first payment of a Telegram subscription (not a
// renewal charge).
func (p SubPayment) isNewSubscription() bool { return !p.IsRecurring || p.IsFirstRecurring }

// ApplySubscriptionPayment decides what a successful subscription payment
// does with the current subscription (cur may be nil):
//   - nothing active (free / expired / unknown plan): the paid plan starts
//     now, until the Telegram expiration date;
//   - same plan: renewal — the expiry moves to the later of the two dates;
//     a NEW subscription of the same plan replaces the old Telegram
//     subscription (its auto-renewal is cancelled);
//   - better plan: upgrade at once; the old Telegram subscription is
//     cancelled (its remaining days are not refunded — see docs);
//   - cheaper plan while a better one is active: the payment is refunded
//     and that (cheaper) Telegram subscription is cancelled — it can only
//     be a renewal of a subscription that was replaced by an upgrade.
func (c *Catalog) ApplySubscriptionPayment(cur *SubState, p SubPayment, now time.Time, grace time.Duration) SubDecision {
	paid, ok := c.Get(p.Plan)
	if !ok || paid.IsFree() {
		// A payload for a plan that no longer exists: refund.
		return SubDecision{Kind: DecisionStaleLower, Refund: true, CancelChargeID: p.ChargeID}
	}
	active := c.Free()
	if cur != nil {
		active = c.Effective(cur.Plan, cur.ExpiresAt, now, grace)
	}
	subID := p.ChargeID
	switch {
	case active.IsFree():
		ns := &SubState{Plan: paid.Code, ExpiresAt: expiryOf(p, now), SubChargeID: subID, ChargeID: p.ChargeID, Source: SourceStars}
		d := SubDecision{Kind: DecisionNew, New: ns}
		if !p.isNewSubscription() && cur != nil && cur.SubChargeID != "" {
			// A late renewal of the same Telegram subscription after the
			// grace window — keep its identifier.
			ns.SubChargeID = cur.SubChargeID
		}
		return d
	case paid.Rank == active.Rank:
		ns := &SubState{Plan: paid.Code, ChargeID: p.ChargeID, Source: SourceStars, SubChargeID: cur.SubChargeID}
		exp := expiryOf(p, cur.ExpiresAt)
		if cur.ExpiresAt.After(exp) {
			exp = cur.ExpiresAt
		}
		ns.ExpiresAt = exp
		d := SubDecision{Kind: DecisionRenewal, New: ns}
		if p.isNewSubscription() {
			if cur.SubChargeID != "" && cur.SubChargeID != subID && cur.Source == SourceStars {
				d.CancelChargeID = cur.SubChargeID
			}
			ns.SubChargeID = subID
		} else if ns.SubChargeID == "" {
			ns.SubChargeID = subID
		}
		return d
	case paid.Rank > active.Rank:
		ns := &SubState{Plan: paid.Code, ExpiresAt: expiryOf(p, now), SubChargeID: subID, ChargeID: p.ChargeID, Source: SourceStars}
		d := SubDecision{Kind: DecisionUpgrade, New: ns}
		if cur.SubChargeID != "" && cur.SubChargeID != subID && cur.Source == SourceStars {
			d.CancelChargeID = cur.SubChargeID
		}
		return d
	default:
		return SubDecision{Kind: DecisionStaleLower, Refund: true, CancelChargeID: p.ChargeID}
	}
}

// --- Formatting helpers --------------------------------------------------------

// HumanDuration renders d as «5 ч 12 мин» / «12 мин» / «меньше минуты».
func HumanDuration(d time.Duration) string {
	if d < time.Minute {
		return "меньше минуты"
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%d ч %d мин", h, m)
	case h > 0:
		return fmt.Sprintf("%d ч", h)
	default:
		return fmt.Sprintf("%d мин", m)
	}
}
