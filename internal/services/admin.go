package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/metrics"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// Admin action kinds (admin_actions.action).
const (
	ActionSetPlan           = "set_plan"
	ActionBypassPurchase    = "bypass_purchase"
	ActionBroadcastConfirm  = "broadcast_confirm"
	ActionBroadcastCancel   = "broadcast_cancel"
	ActionCommandGrant      = "cmd_grant"
	ActionCommandRevoke     = "cmd_revoke"
	ActionCommandRefund     = "cmd_refund"
	adminPlanMaxDays        = 3660
	adminWeakTopicsPerSubj  = 3
	adminRecentActionsCount = 3
)

// AdminService implements the admin panel (docs/ADMIN.md). Every method
// that changes something re-checks the administrator on the server
// (isAdmin, ADMIN_IDS) — the UI hiding the buttons is not the protection.
type AdminService struct {
	repo    *repositories.AdminRepository
	billing *BillingService // nil = subscriptions off (no plan changes)
	isAdmin func(tgUserID int64) bool
	loc     *time.Location
	now     func() time.Time
}

// NewAdminService wires the service. loc is the calendar of «today» in the
// statistics (QUOTA_TZ).
func NewAdminService(repo *repositories.AdminRepository, bill *BillingService, isAdmin func(int64) bool, loc *time.Location) *AdminService {
	if loc == nil {
		loc = billing.LoadLocation("Asia/Almaty")
	}
	return &AdminService{repo: repo, billing: bill, isAdmin: isAdmin, loc: loc, now: time.Now}
}

// IsAdmin reports whether the Telegram user is an administrator.
func (s *AdminService) IsAdmin(tgUserID int64) bool {
	return s != nil && s.isAdmin != nil && s.isAdmin(tgUserID)
}

// Billing returns the billing service (nil when subscriptions are off).
func (s *AdminService) Billing() *BillingService { return s.billing }

// Repo exposes the repository (sessions, broadcasts).
func (s *AdminService) Repo() *repositories.AdminRepository { return s.repo }

// Location is the statistics calendar.
func (s *AdminService) Location() *time.Location { return s.loc }

func (s *AdminService) dayStart() time.Time {
	now := s.now().In(s.loc)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.loc)
}

// Stats computes the statistics screen.
func (s *AdminService) Stats(ctx context.Context) (*repositories.AdminStats, error) {
	day := s.dayStart()
	var grace time.Duration
	if s.billing != nil {
		grace = s.billing.repo.Grace()
	}
	return s.repo.Stats(ctx, repositories.StatsWindow{
		DayStart:    day,
		WeekStart:   day.AddDate(0, 0, -6),
		MonthStart:  day.AddDate(0, 0, -29),
		ActiveToday: models.StreakToday(s.now()),
		Grace:       grace,
	})
}

// Search finds users by @username, id or name.
func (s *AdminService) Search(ctx context.Context, q string) ([]repositories.UserBrief, error) {
	return s.repo.SearchUsers(ctx, repositories.ParseSearchQuery(q), 10)
}

// UserCard is everything the admin sees about one user.
type UserCard struct {
	User       repositories.UserBrief
	Activity   *repositories.UserActivity
	Usage      *repositories.Usage // nil when subscriptions are off
	WeakTopics []repositories.SubjectWeakTopics
	Actions    []repositories.AdminAction
}

// Card loads the user card.
func (s *AdminService) Card(ctx context.Context, userID int64) (*UserCard, error) {
	u, err := s.repo.UserBriefByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	c := &UserCard{User: *u}
	day := s.dayStart()
	if c.Activity, err = s.repo.UserActivity(ctx, userID, day, day.Format("2006-01-02")); err != nil {
		return nil, err
	}
	if s.billing != nil {
		if c.Usage, err = s.billing.Usage(ctx, userID); err != nil {
			return nil, err
		}
	}
	if c.WeakTopics, err = s.repo.UserWeakTopics(ctx, userID, adminWeakTopicsPerSubj); err != nil {
		return nil, err
	}
	if c.Actions, err = s.repo.RecentActions(ctx, userID, adminRecentActionsCount); err != nil {
		return nil, err
	}
	return c, nil
}

// ErrSubscriptionsOff: plan changes need SUBSCRIPTIONS_ENABLED=1.
var ErrSubscriptionsOff = errors.New("subscriptions are off")

// SetPlan changes the user's plan at once (admin): a paid plan for days
// days from now, "free" = the current plan is revoked. The previous state
// and the new one go to the admin action log.
func (s *AdminService) SetPlan(ctx context.Context, adminTgID, userID int64, plan string, days int) (time.Time, error) {
	if !s.IsAdmin(adminTgID) {
		return time.Time{}, ErrNotAdmin
	}
	if s.billing == nil {
		return time.Time{}, ErrSubscriptionsOff
	}
	p, ok := s.billing.Catalog().Get(plan)
	if !ok {
		return time.Time{}, ErrUnknownPlan
	}
	if p.IsFree() {
		days = 0
	} else if days <= 0 || days > adminPlanMaxDays {
		return time.Time{}, fmt.Errorf("days must be 1…%d", adminPlanMaxDays)
	}
	u, err := s.repo.UserBriefByID(ctx, userID)
	if err != nil {
		return time.Time{}, err
	}
	before := "?"
	if us, err := s.billing.Usage(ctx, userID); err == nil {
		before = us.Plan.Code
		if !us.Plan.IsFree() {
			before += " до " + us.ExpiresAt.In(s.loc).Format("02.01.2006 15:04")
		}
	}
	_, until, err := s.billing.Grant(ctx, u.TelegramID, p.Code, days)
	if err != nil {
		return time.Time{}, err
	}
	details := fmt.Sprintf("plan=%s days=%d (было: %s)", p.Code, days, before)
	if !p.IsFree() {
		details = fmt.Sprintf("plan=%s days=%d until=%s (было: %s)", p.Code, days, until.In(s.loc).Format("02.01.2006 15:04"), before)
	}
	s.LogAction(ctx, adminTgID, ActionSetPlan, userID, details)
	return until, nil
}

// LogAction appends to the admin audit log (errors are only logged — the
// action itself has already happened).
func (s *AdminService) LogAction(ctx context.Context, adminTgID int64, action string, targetUserID int64, details string) {
	metrics.Inc(metrics.AdminActions, "action", action)
	log.Printf("admin: tg %d %s user=%d %s", adminTgID, action, targetUserID, details)
	if err := s.repo.LogAction(ctx, adminTgID, action, targetUserID, details); err != nil {
		log.Printf("admin: log action: %v", err)
	}
}

// --- Broadcasts (UI side) -----------------------------------------------------------

// CreateBroadcastDraft validates and stores a composed broadcast.
func (s *AdminService) CreateBroadcastDraft(ctx context.Context, adminTgID int64, b *repositories.Broadcast) (*repositories.Broadcast, error) {
	if !s.IsAdmin(adminTgID) {
		return nil, ErrNotAdmin
	}
	b.AdminTgID = adminTgID
	if err := ValidateBroadcast(b); err != nil {
		return nil, err
	}
	return s.repo.CreateBroadcastDraft(ctx, b)
}

// ConfirmBroadcast queues a draft for sending (idempotent).
func (s *AdminService) ConfirmBroadcast(ctx context.Context, adminTgID, id int64) (bool, int, error) {
	if !s.IsAdmin(adminTgID) {
		return false, 0, ErrNotAdmin
	}
	changed, total, err := s.repo.ConfirmBroadcast(ctx, id)
	if err == nil && changed {
		s.LogAction(ctx, adminTgID, ActionBroadcastConfirm, 0, fmt.Sprintf("broadcast=%d recipients=%d", id, total))
	}
	return changed, total, err
}

// CancelBroadcast stops a broadcast.
func (s *AdminService) CancelBroadcast(ctx context.Context, adminTgID, id int64) (bool, error) {
	if !s.IsAdmin(adminTgID) {
		return false, ErrNotAdmin
	}
	changed, err := s.repo.CancelBroadcast(ctx, id)
	if err == nil && changed {
		s.LogAction(ctx, adminTgID, ActionBroadcastCancel, 0, fmt.Sprintf("broadcast=%d", id))
	}
	return changed, err
}

// PlanTitle is the display title of a plan code ("" unknown).
func (s *AdminService) PlanTitle(code string) string {
	if s.billing == nil {
		return code
	}
	if p, ok := s.billing.Catalog().Get(code); ok {
		return p.Title
	}
	if code == "" {
		return "?"
	}
	return strings.ToUpper(code[:1]) + code[1:]
}
