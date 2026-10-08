package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// AdminRepository backs the admin panel (docs/ADMIN.md): statistics, user
// search and card, the admin action log, the admin dialog state and the
// broadcasts. Every state lives in PostgreSQL, so it works the same with
// ROLE=all and with web + N workers.
type AdminRepository struct {
	pool *pgxpool.Pool
}

// NewAdminRepository creates the repository.
func NewAdminRepository(pool *pgxpool.Pool) *AdminRepository {
	return &AdminRepository{pool: pool}
}

// --- Statistics ------------------------------------------------------------------

// StatsWindow is the calendar the statistics are computed in.
type StatsWindow struct {
	DayStart   time.Time // start of "today" (quota time zone)
	WeekStart  time.Time // DayStart - 6 days
	MonthStart time.Time // DayStart - 29 days
	// ActiveToday is the streak calendar date of today (users.last_active_date).
	ActiveToday time.Time
	// Grace extends paid plans (SUB_GRACE_MIN).
	Grace time.Duration
}

// AdminStats is the «📊 Статистика» screen.
type AdminStats struct {
	UsersTotal, NewToday, New7d, New30d int
	ActiveToday, Active7d, Active30d    int
	Blocked                             int

	StartedToday, CompletedToday, Completed7d, CompletedTotal, InProgress int

	PaidPlans map[string]int // active paid plans by code

	Payments, Stars, Payments30d, Stars30d, PaymentsToday, StarsToday int
	Refunded, RefundPending, Bypass                                   int
	WeakOrdersInFlight                                                int

	GenJobs        map[string]int // "kind:status" → count (pending / running)
	GenFailed24h   int
	TrPending      int
	TrRunning      int
	UpdatesPending int
	UpdatesDead    int

	BroadcastsActive int
}

// Stats computes the statistics screen. A handful of indexed aggregate
// queries — an admin-only screen, opened by hand (no cache needed).
func (r *AdminRepository) Stats(ctx context.Context, w StatsWindow) (*AdminStats, error) {
	s := &AdminStats{PaidPlans: map[string]int{}, GenJobs: map[string]int{}}
	today := w.ActiveToday.Format("2006-01-02")
	if err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)::int,
		       COUNT(*) FILTER (WHERE created_at >= $1)::int,
		       COUNT(*) FILTER (WHERE created_at >= $2)::int,
		       COUNT(*) FILTER (WHERE created_at >= $3)::int,
		       COUNT(*) FILTER (WHERE last_active_date >= $4::date)::int,
		       COUNT(*) FILTER (WHERE last_active_date >= $4::date - 6)::int,
		       COUNT(*) FILTER (WHERE last_active_date >= $4::date - 29)::int,
		       COUNT(*) FILTER (WHERE blocked_at IS NOT NULL)::int
		FROM users`, w.DayStart, w.WeekStart, w.MonthStart, today).
		Scan(&s.UsersTotal, &s.NewToday, &s.New7d, &s.New30d, &s.ActiveToday, &s.Active7d, &s.Active30d, &s.Blocked); err != nil {
		return nil, err
	}
	if err := r.pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*)::int FROM test_attempts WHERE started_at >= $1),
		       (SELECT COUNT(*)::int FROM test_attempts WHERE status = 'completed' AND completed_at >= $1),
		       (SELECT COUNT(*)::int FROM test_attempts WHERE status = 'completed' AND completed_at >= $2),
		       (SELECT COUNT(*)::int FROM test_attempts WHERE status = 'completed'),
		       (SELECT COUNT(*)::int FROM test_attempts WHERE status = 'in_progress')`, w.DayStart, w.WeekStart).
		Scan(&s.StartedToday, &s.CompletedToday, &s.Completed7d, &s.CompletedTotal, &s.InProgress); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT plan, COUNT(*)::int FROM user_subscriptions
		WHERE status = 'active' AND expires_at > now() - make_interval(secs => $1)
		GROUP BY plan`, w.Grace.Seconds())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var plan string
		var n int
		if err := rows.Scan(&plan, &n); err != nil {
			rows.Close()
			return nil, err
		}
		s.PaidPlans[plan] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Revenue: real payments only (admin-bypass purchases had no Stars),
	// refunded ones excluded.
	if err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE real AND status <> 'refunded')::int,
		       COALESCE(SUM(amount) FILTER (WHERE real AND status <> 'refunded'), 0)::int,
		       COUNT(*) FILTER (WHERE real AND status <> 'refunded' AND created_at >= $2)::int,
		       COALESCE(SUM(amount) FILTER (WHERE real AND status <> 'refunded' AND created_at >= $2), 0)::int,
		       COUNT(*) FILTER (WHERE real AND status <> 'refunded' AND created_at >= $1)::int,
		       COALESCE(SUM(amount) FILTER (WHERE real AND status <> 'refunded' AND created_at >= $1), 0)::int,
		       COUNT(*) FILTER (WHERE real AND status = 'refunded')::int,
		       COUNT(*) FILTER (WHERE real AND status = 'refund_pending')::int,
		       COUNT(*) FILTER (WHERE NOT real)::int
		FROM (SELECT amount, status, created_at, NOT admin_bypass AS real FROM payments) p`, w.DayStart, w.MonthStart).
		Scan(&s.Payments, &s.Stars, &s.Payments30d, &s.Stars30d, &s.PaymentsToday, &s.StarsToday,
			&s.Refunded, &s.RefundPending, &s.Bypass); err != nil {
		return nil, err
	}
	if err := r.pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*)::int FROM weak_test_orders WHERE status = 'paid'),
		       (SELECT COUNT(*)::int FROM generation_jobs WHERE status = 'failed' AND updated_at > now() - interval '1 day'),
		       (SELECT COUNT(*)::int FROM translation_jobs WHERE status = 'pending'),
		       (SELECT COUNT(*)::int FROM translation_jobs WHERE status = 'running'),
		       (SELECT COUNT(*)::int FROM tg_update_queue WHERE status IN ('pending','processing')),
		       (SELECT COUNT(*)::int FROM tg_update_queue WHERE status = 'dead'),
		       (SELECT COUNT(*)::int FROM broadcasts WHERE status IN ('queued','sending'))`).
		Scan(&s.WeakOrdersInFlight, &s.GenFailed24h, &s.TrPending, &s.TrRunning, &s.UpdatesPending, &s.UpdatesDead, &s.BroadcastsActive); err != nil {
		return nil, err
	}
	rows, err = r.pool.Query(ctx, `
		SELECT kind, status, COUNT(*)::int FROM generation_jobs
		WHERE status IN ('pending','running') GROUP BY kind, status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, status string
		var n int
		if err := rows.Scan(&kind, &status, &n); err != nil {
			return nil, err
		}
		s.GenJobs[kind+":"+status] = n
	}
	return s, rows.Err()
}

// --- Users -------------------------------------------------------------------------

// UserBrief is one user in the admin search results / card.
type UserBrief struct {
	ID             int64
	TelegramID     int64
	Username       string
	FirstName      string
	LastName       string
	CreatedAt      time.Time
	LastActiveDate *time.Time
	StreakDays     int
	TestLang       string
	BlockedAt      *time.Time
}

// DisplayName is «Имя Фамилия» (or @username / id when empty).
func (u UserBrief) DisplayName() string {
	n := strings.TrimSpace(u.FirstName + " " + u.LastName)
	switch {
	case n != "":
		return n
	case u.Username != "":
		return "@" + u.Username
	}
	return strconv.FormatInt(u.TelegramID, 10)
}

const userBriefCols = `id, telegram_id, username, first_name, last_name, created_at, last_active_date, streak_days, test_lang, blocked_at`

func scanUserBriefs(rows pgx.Rows) ([]UserBrief, error) {
	defer rows.Close()
	var out []UserBrief
	for rows.Next() {
		var u UserBrief
		if err := rows.Scan(&u.ID, &u.TelegramID, &u.Username, &u.FirstName, &u.LastName, &u.CreatedAt,
			&u.LastActiveDate, &u.StreakDays, &u.TestLang, &u.BlockedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SearchQuery is a parsed admin search input.
type SearchQuery struct {
	Username string // «@name» / t.me/name (without @)
	ID       int64  // all digits: Telegram id or internal id
	Name     string // anything else: part of the name / username
}

// ParseSearchQuery recognises «@username», «https://t.me/username», a
// numeric id or a name.
func ParseSearchQuery(q string) SearchQuery {
	q = strings.TrimSpace(q)
	for _, p := range []string{"https://t.me/", "http://t.me/", "t.me/"} {
		if strings.HasPrefix(strings.ToLower(q), p) {
			return SearchQuery{Username: strings.Trim(q[len(p):], "/@ ")}
		}
	}
	if strings.HasPrefix(q, "@") {
		return SearchQuery{Username: strings.TrimSpace(q[1:])}
	}
	if id, err := strconv.ParseInt(q, 10, 64); err == nil && id > 0 {
		return SearchQuery{ID: id}
	}
	return SearchQuery{Name: q}
}

// likeEscape escapes LIKE wildcards of user input.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// SearchUsers finds users by @username (exact, then prefix), by Telegram
// id / internal id, or by a part of the name. At most limit results, the
// most recently active first.
func (r *AdminRepository) SearchUsers(ctx context.Context, q SearchQuery, limit int) ([]UserBrief, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	switch {
	case q.Username != "":
		rows, err := r.pool.Query(ctx, `SELECT `+userBriefCols+` FROM users
			WHERE username <> '' AND lower(username) = lower($1) LIMIT $2`, q.Username, limit)
		if err != nil {
			return nil, err
		}
		out, err := scanUserBriefs(rows)
		if err != nil || len(out) > 0 {
			return out, err
		}
		rows, err = r.pool.Query(ctx, `SELECT `+userBriefCols+` FROM users
			WHERE username <> '' AND lower(username) LIKE lower($1) || '%' ESCAPE '\'
			ORDER BY last_active_date DESC NULLS LAST, id LIMIT $2`, likeEscape(q.Username), limit)
		if err != nil {
			return nil, err
		}
		return scanUserBriefs(rows)
	case q.ID > 0:
		rows, err := r.pool.Query(ctx, `SELECT `+userBriefCols+` FROM users
			WHERE telegram_id = $1 OR id = $1 ORDER BY (telegram_id = $1) DESC, id LIMIT $2`, q.ID, limit)
		if err != nil {
			return nil, err
		}
		return scanUserBriefs(rows)
	case strings.TrimSpace(q.Name) != "":
		rows, err := r.pool.Query(ctx, `SELECT `+userBriefCols+` FROM users
			WHERE (first_name || ' ' || last_name) ILIKE '%' || $1 || '%' ESCAPE '\'
			   OR username ILIKE '%' || $1 || '%' ESCAPE '\'
			ORDER BY last_active_date DESC NULLS LAST, id LIMIT $2`, likeEscape(strings.TrimSpace(q.Name)), limit)
		if err != nil {
			return nil, err
		}
		return scanUserBriefs(rows)
	}
	return nil, nil
}

// UserBriefByID loads one user (ErrNotFound when missing).
func (r *AdminRepository) UserBriefByID(ctx context.Context, userID int64) (*UserBrief, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+userBriefCols+` FROM users WHERE id = $1`, userID)
	if err != nil {
		return nil, err
	}
	out, err := scanUserBriefs(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return &out[0], nil
}

// UserActivity is the activity part of the user card.
type UserActivity struct {
	AttemptsTotal  int
	CompletedTotal int
	CompletedToday int
	InProgress     int
	LastAttemptAt  *time.Time
	ChargedToday   int // daily_usage.used today (quota day)
	PaymentsReal   int
	StarsPaid      int
	PaymentsBypass int
}

// UserActivity computes the activity counters of one user (index scans by
// user_id).
func (r *AdminRepository) UserActivity(ctx context.Context, userID int64, dayStart time.Time, quotaDay string) (*UserActivity, error) {
	a := &UserActivity{}
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)::int,
		       COUNT(*) FILTER (WHERE status = 'completed')::int,
		       COUNT(*) FILTER (WHERE status = 'completed' AND completed_at >= $2)::int,
		       COUNT(*) FILTER (WHERE status = 'in_progress')::int,
		       MAX(started_at),
		       COALESCE((SELECT used FROM daily_usage WHERE user_id = $1 AND day = $3::date), 0),
		       (SELECT COUNT(*)::int FROM payments WHERE user_id = $1 AND NOT admin_bypass AND status <> 'refunded'),
		       (SELECT COALESCE(SUM(amount), 0)::int FROM payments WHERE user_id = $1 AND NOT admin_bypass AND status <> 'refunded'),
		       (SELECT COUNT(*)::int FROM payments WHERE user_id = $1 AND admin_bypass)
		FROM test_attempts WHERE user_id = $1`, userID, dayStart, quotaDay).
		Scan(&a.AttemptsTotal, &a.CompletedTotal, &a.CompletedToday, &a.InProgress, &a.LastAttemptAt,
			&a.ChargedToday, &a.PaymentsReal, &a.StarsPaid, &a.PaymentsBypass)
	return a, err
}

// SubjectWeakTopics is the weak-topics part of the user card.
type SubjectWeakTopics struct {
	Subject string
	Topics  []models.TopicStat
}

// UserWeakTopics returns the user's weak topics (worst first, at most
// perSubject per subject) of every subject with any.
func (r *AdminRepository) UserWeakTopics(ctx context.Context, userID int64, perSubject int) ([]SubjectWeakTopics, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT t.subject_id, t.topic_key, t.topic, t.correct_count, t.wrong_count, t.recent, s.name, s.position
		FROM user_topic_stats t JOIN subjects s ON s.id = t.subject_id
		WHERE t.user_id = $1
		ORDER BY s.position, s.id, t.topic_key`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stats []models.TopicStat
	names := map[int64]string{}
	var order []int64
	for rows.Next() {
		var st models.TopicStat
		var name string
		var pos int
		if err := rows.Scan(&st.SubjectID, &st.Key, &st.Topic, &st.Correct, &st.Wrong, &st.Recent, &name, &pos); err != nil {
			return nil, err
		}
		if _, ok := names[st.SubjectID]; !ok {
			names[st.SubjectID] = name
			order = append(order, st.SubjectID)
		}
		stats = append(stats, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	by := models.WeakTopicStatsBySubject(stats, perSubject)
	var out []SubjectWeakTopics
	for _, sid := range order {
		if ts := by[sid]; len(ts) > 0 {
			out = append(out, SubjectWeakTopics{Subject: names[sid], Topics: ts})
		}
	}
	return out, nil
}

// MarkUserBlocked records that the user blocked the bot (Telegram 403).
func (r *AdminRepository) MarkUserBlocked(ctx context.Context, userID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET blocked_at = now() WHERE id = $1 AND blocked_at IS NULL`, userID)
	return err
}

// --- Admin action log ---------------------------------------------------------------

// AdminAction is one entry of the admin audit log.
type AdminAction struct {
	ID           int64
	AdminTgID    int64
	Action       string
	TargetUserID int64
	Details      string
	CreatedAt    time.Time
}

// LogAction appends to the admin audit log (target 0 = none).
func (r *AdminRepository) LogAction(ctx context.Context, adminTgID int64, action string, targetUserID int64, details string) error {
	var target *int64
	if targetUserID > 0 {
		target = &targetUserID
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO admin_actions (admin_tg_id, action, target_user_id, details) VALUES ($1, $2, $3, left($4, 2000))`,
		adminTgID, action, target, details)
	return err
}

// RecentActions returns the latest admin actions (about one user when
// targetUserID > 0).
func (r *AdminRepository) RecentActions(ctx context.Context, targetUserID int64, limit int) ([]AdminAction, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, admin_tg_id, action, COALESCE(target_user_id, 0), details, created_at
		FROM admin_actions WHERE ($1 = 0 OR target_user_id = $1)
		ORDER BY created_at DESC, id DESC LIMIT $2`, targetUserID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminAction
	for rows.Next() {
		var a AdminAction
		if err := rows.Scan(&a.ID, &a.AdminTgID, &a.Action, &a.TargetUserID, &a.Details, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- Admin dialog state ------------------------------------------------------------

// AdminSession is the step of a multi-step admin dialog.
type AdminSession struct {
	State     string
	Data      map[string]any
	UpdatedAt time.Time
}

// GetSession returns the admin's dialog state (empty State when none).
func (r *AdminRepository) GetSession(ctx context.Context, adminTgID int64) (*AdminSession, error) {
	s := &AdminSession{Data: map[string]any{}}
	var raw []byte
	err := r.pool.QueryRow(ctx, `SELECT state, data, updated_at FROM admin_sessions WHERE admin_tg_id = $1`, adminTgID).
		Scan(&s.State, &raw, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.Data); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// SetSession stores the admin's dialog state.
func (r *AdminRepository) SetSession(ctx context.Context, adminTgID int64, state string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO admin_sessions (admin_tg_id, state, data, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (admin_tg_id) DO UPDATE SET state = EXCLUDED.state, data = EXCLUDED.data, updated_at = now()`,
		adminTgID, state, raw)
	return err
}

// ClearSession ends the admin's dialog.
func (r *AdminRepository) ClearSession(ctx context.Context, adminTgID int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE admin_tg_id = $1`, adminTgID)
	return err
}
