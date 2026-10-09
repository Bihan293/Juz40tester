package main

// Load test: ~1000 users answering at once through the PRODUCTION wiring
// (config.Load → startWorkerSide → the in-process webhook dispatcher), a
// real PostgreSQL and a fake Bot API.
//
// Skipped unless LOADTEST_DATABASE_URL is set (use a scratch database: the
// test seeds background volume into it). Knobs (all optional):
//
//	LOADTEST_USERS=1000     concurrent users
//	LOADTEST_ROUNDS=5       answers per user (each round: everybody taps at once)
//	LOADTEST_RTT_MS=2       simulated network round trip per SQL statement (Neon)
//	LOADTEST_TG_MS=40       fake Bot API latency
//	LOADTEST_TG_RPS=28      TG_MAX_RPS (outgoing limiter); 10000 = measure the DB side only
//	LOADTEST_BG_USERS=20000 background users with history (seeded once)
//	LOADTEST_TG_429_EVERY=0 every n-th chat gets one per-chat 429 per phase
//	LOADTEST_SCREENS=1      also measure the other hot screens
//	LOADTEST_BROADCAST=1    one answer round while an admin broadcast is sending
//	DB_MAX_CONNS=20         pool size, as in production
//
// Run:
//
//	LOADTEST_DATABASE_URL=postgres://… go test ./cmd/bot -run TestLoadAnswers -v -timeout 30m

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v >= 0 {
		return v
	}
	return def
}

// loadTracer counts SQL statements, simulates the network round trip and
// records per-statement time.
type loadTracer struct {
	rtt     time.Duration
	queries atomic.Int64
	mu      sync.Mutex
	stats   map[string]*stmtStat
}

type stmtStat struct {
	n     int64
	total time.Duration
	max   time.Duration
}

type traceKey struct{}

type traceVal struct {
	sql   string
	start time.Time
}

func (t *loadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	t.queries.Add(1)
	if t.rtt > 0 {
		time.Sleep(t.rtt)
	}
	return context.WithValue(ctx, traceKey{}, traceVal{sql: d.SQL, start: time.Now()})
}

func (t *loadTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	v, ok := ctx.Value(traceKey{}).(traceVal)
	if !ok {
		return
	}
	el := time.Since(v.start) + t.rtt
	key := strings.Join(strings.Fields(v.sql), " ")
	if len(key) > 110 {
		key = key[:110]
	}
	t.mu.Lock()
	s := t.stats[key]
	if s == nil {
		s = &stmtStat{}
		t.stats[key] = s
	}
	s.n++
	s.total += el
	if el > s.max {
		s.max = el
	}
	t.mu.Unlock()
}

func (t *loadTracer) reset() {
	t.queries.Store(0)
	t.mu.Lock()
	t.stats = map[string]*stmtStat{}
	t.mu.Unlock()
}

func (t *loadTracer) top(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	type row struct {
		k string
		s *stmtStat
	}
	var rows []row
	for k, s := range t.stats {
		rows = append(rows, row{k, s})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].s.total > rows[j].s.total })
	var b strings.Builder
	for i, r := range rows {
		if i >= n {
			break
		}
		fmt.Fprintf(&b, "  %6d× avg %-8s max %-8s %s\n", r.s.n, (r.s.total / time.Duration(r.s.n)).Round(10*time.Microsecond),
			r.s.max.Round(100*time.Microsecond), r.k)
	}
	return b.String()
}

// fakeTG is a Bot API answering every call after a fixed latency; it
// reports the first message-changing call per chat on a channel.
type fakeTG struct {
	latency time.Duration
	// flood429: every n-th chat (0 = off) gets ONE per-chat 429
	// (retry_after 3 s) on its first edit of each phase — Telegram's
	// «1 message per second per chat» flood control.
	flood429 int64
	flooded  sync.Map // chat -> phase already 429ed
	phase    atomic.Int64
	n429     atomic.Int64
	mu       sync.Mutex
	waiters  map[int64]chan string
	calls    atomic.Int64
}

func (f *fakeTG) expect(chatID int64) chan string {
	ch := make(chan string, 4)
	f.mu.Lock()
	f.waiters[chatID] = ch
	f.mu.Unlock()
	return ch
}

func (f *fakeTG) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.calls.Add(1)
	time.Sleep(f.latency)
	m := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if m == "editMessageText" || m == "sendMessage" {
		var p struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}
		_ = json.Unmarshal(body, &p)
		if f.flood429 > 0 && p.ChatID%f.flood429 == 0 {
			key := fmt.Sprintf("%d:%d", p.ChatID, f.phase.Load())
			if _, done := f.flooded.LoadOrStore(key, true); !done {
				f.n429.Add(1)
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`)
				return
			}
		}
		f.mu.Lock()
		ch := f.waiters[p.ChatID]
		f.mu.Unlock()
		if ch != nil {
			select {
			case ch <- p.Text:
			default:
			}
		}
	}
	switch m {
	case "sendMessage":
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":10,"chat":{"id":1,"type":"private"}}}`)
	default:
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}
}

// seedBackground adds realistic history volume (once per database).
func seedBackground(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subjectID int64, testIDs []int64, bgUsers int) {
	var have int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE telegram_id >= 7000000000 AND telegram_id < 7100000000`).Scan(&have)
	if have >= bgUsers {
		return
	}
	start := time.Now()
	stmts := []string{
		fmt.Sprintf(`INSERT INTO users (telegram_id, first_name, last_active_date, streak_days)
			SELECT 7000000000 + g, 'u' || g, CURRENT_DATE - (g %% 30), g %% 20 FROM generate_series(%d, %d) g
			ON CONFLICT (telegram_id) DO NOTHING`, have+1, bgUsers),
		// 10 completed attempts per background user, spread over the tests.
		fmt.Sprintf(`INSERT INTO test_attempts (user_id, test_id, status, current_position, correct_count, wrong_count, started_at, completed_at, updated_at)
			SELECT u.id, (ARRAY[%s]::bigint[])[1 + (k %% %d)], 'completed', 21, 15, 5,
			       now() - make_interval(days => k), now() - make_interval(days => k), now() - make_interval(days => k)
			FROM users u, generate_series(1, 10) k
			WHERE u.telegram_id > 7000000000 + %d AND u.telegram_id < 7100000000`, joinIDs(testIDs), len(testIDs), have),
		`INSERT INTO attempt_questions (attempt_id, question_id, position, answered, selected_answer, is_correct, option_order)
			SELECT a.id, tq.question_id, tq.position, true, 'A', (tq.position % 4) <> 0, '["A","B","C","D"]'
			FROM test_attempts a JOIN test_questions tq ON tq.test_id = a.test_id
			WHERE a.status = 'completed' AND NOT EXISTS (SELECT 1 FROM attempt_questions x WHERE x.attempt_id = a.id)`,
		`INSERT INTO user_question_progress (user_id, question_id, status, correct_count, wrong_count)
			SELECT a.user_id, aq.question_id, (aq.question_id % 3)::int, 1, 0
			FROM attempt_questions aq JOIN test_attempts a ON a.id = aq.attempt_id
			ON CONFLICT DO NOTHING`,
		`INSERT INTO test_completions (attempt_id, user_id, test_id, day, charged, completed_at)
			SELECT id, user_id, test_id, completed_at::date, false, completed_at FROM test_attempts WHERE status = 'completed'
			ON CONFLICT DO NOTHING`,
		fmt.Sprintf(`INSERT INTO user_topic_stats (user_id, subject_id, topic_key, topic, correct_count, wrong_count)
			SELECT u.id, %d, 'тема ' || k, 'Тема ' || k, 3, 1 FROM users u, generate_series(1, 5) k
			WHERE u.telegram_id >= 7000000000 AND u.telegram_id < 7100000000
			ON CONFLICT DO NOTHING`, subjectID),
		`ANALYZE`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	t.Logf("background volume seeded in %s", time.Since(start).Round(time.Second))
}

func joinIDs(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(s, ",")
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	i := int(float64(len(d)-1) * p)
	return d[i]
}

func TestLoadAnswers(t *testing.T) {
	dbURL := os.Getenv("LOADTEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("LOADTEST_DATABASE_URL not set")
	}
	users := envInt("LOADTEST_USERS", 1000)
	rounds := envInt("LOADTEST_ROUNDS", 5)
	rtt := time.Duration(envInt("LOADTEST_RTT_MS", 2)) * time.Millisecond
	tgLatency := time.Duration(envInt("LOADTEST_TG_MS", 40)) * time.Millisecond
	tgRPS := envInt("LOADTEST_TG_RPS", 28)
	bgUsers := envInt("LOADTEST_BG_USERS", 20000)

	ctx := context.Background()
	if err := database.MigrateURL(ctx, dbURL); err != nil {
		t.Fatal(err)
	}

	tgSrv := &fakeTG{latency: tgLatency, waiters: map[int64]chan string{}, flood429: int64(envInt("LOADTEST_TG_429_EVERY", 0))}
	tgHTTP := httptest.NewServer(tgSrv)
	defer tgHTTP.Close()

	t.Setenv("BOT_TOKEN", "LOAD")
	t.Setenv("DATABASE_URL", dbURL)
	t.Setenv("WEBHOOK_URL", "http://127.0.0.1:1")
	t.Setenv("APP_ENV", "development")
	t.Setenv("WEBHOOK_SECRET", "")
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	if os.Getenv("DB_MAX_CONNS") == "" {
		t.Setenv("DB_MAX_CONNS", "20")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.TGMaxRPS = tgRPS

	tracer := &loadTracer{rtt: rtt, stats: map[string]*stmtStat{}}
	pcfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	applyLoadPool(pcfg, cfg)
	pcfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Seed: one subject, a chain of 10 tests × 20 questions.
	sid, err := testutil.CreateSubject(ctx, pool, "Нагрузка")
	if err != nil {
		t.Fatal(err)
	}
	gen := repositories.NewGenerationRepository(pool)
	var testIDs []int64
	for n := 1; n <= 10; n++ {
		var id int64
		if err := pool.QueryRow(ctx, `SELECT id FROM tests WHERE subject_id = $1 AND test_number = $2`, sid, n).Scan(&id); err == nil {
			testIDs = append(testIDs, id)
			continue
		}
		seeds := make([]models.SeedQuestion, 20)
		for i := range seeds {
			seeds[i] = models.SeedQuestion{Text: fmt.Sprintf("Нагрузка %d-%d: вопрос?", n, i), Options: [4]string{"верно", "нет1", "нет2", "нет3"},
				Correct: 0, Topic: fmt.Sprintf("Тема %d", i%5+1), Difficulty: 1 + i%3}
		}
		test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: n, Title: fmt.Sprintf("Тест %d", n), Kind: models.TestKindChain}, seeds)
		if err != nil {
			t.Fatal(err)
		}
		testIDs = append(testIDs, test.ID)
	}
	seedBackground(t, ctx, pool, sid, testIDs, bgUsers)
	// The measured users start from scratch on every run.
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE telegram_id >= 7100000000 AND telegram_id < 7200000000`); err != nil {
		t.Fatal(err)
	}

	tg := bot.NewClient("LOAD").WithBaseURL(tgHTTP.URL).WithMaxRPS(cfg.TGMaxRPS)
	workerCtx, stopWorker := context.WithCancel(context.Background())
	var bgWG sync.WaitGroup
	w := startWorkerSide(ctx, workerCtx, &bgWG, cfg, pool, dbURL, tg, nil, nil)
	disp := newUpdateDispatcher("", w.updateWorkers, defaultUpdateQueueSize, cfg.UpdateTimeout, w.h.HandleUpdate)
	hook := httptest.NewServer(disp)
	defer hook.Close()
	defer func() {
		stopWorker()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = disp.Shutdown(sctx)
		_ = tg.Close(sctx)
		waitTimeout(&bgWG, 5*time.Second)
	}()
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: users, MaxConnsPerHost: 0}}
	time.Sleep(500 * time.Millisecond) // let the startup loops settle
	goroutines0 := runtime.NumGoroutine()
	defer func() {
		// Leak check: after the load everything transient must be gone.
		time.Sleep(3 * time.Second)
		client.CloseIdleConnections()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		t.Logf("goroutines: %d before the load, %d after; heap in use %.1f MB", goroutines0, runtime.NumGoroutine(), float64(ms.HeapInuse)/(1<<20))
		if os.Getenv("LOADTEST_GOROUTINES") != "" {
			buf := make([]byte, 1<<20)
			t.Logf("%s", buf[:runtime.Stack(buf, true)])
		}
	}()
	t.Logf("config: users=%d rounds=%d update workers=%d DB_MAX_CONNS=%d RTT=%s TG latency=%s TG_MAX_RPS=%d",
		users, rounds, w.updateWorkers, cfg.DBMaxConns, rtt, tgLatency, cfg.TGMaxRPS)

	var updateID atomic.Int64
	updateID.Store(time.Now().UnixNano() % 1_000_000_000)
	attempt := make([]int64, users)
	tgID := func(i int) int64 { return 7100000000 + int64(i) }

	// phase runs one tap per user concurrently and returns the latencies
	// from the webhook POST to the bot's reply reaching Telegram.
	phase := func(name string, data func(i int) string) {
		tracer.reset()
		tgSrv.phase.Add(1)
		n4290 := tgSrv.n429.Load()
		calls0 := tgSrv.calls.Load()
		st0 := pool.Stat()
		lat := make([]time.Duration, users)
		var rejected, lost atomic.Int64
		var wg sync.WaitGroup
		begin := time.Now()
		for i := 0; i < users; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ch := tgSrv.expect(tgID(i))
				upd := bot.Update{UpdateID: updateID.Add(1), CallbackQuery: &bot.CallbackQuery{
					ID: fmt.Sprintf("cb-%s-%d", name, i), From: &bot.TgUser{ID: tgID(i), FirstName: "Нагрузка"},
					Data:    data(i),
					Message: &bot.Message{MessageID: 10, Chat: bot.Chat{ID: tgID(i), Type: "private"}},
				}}
				body, _ := json.Marshal(upd)
				t0 := time.Now()
				for {
					resp, err := client.Post(hook.URL, "application/json", bytes.NewReader(body))
					if err != nil {
						lost.Add(1)
						return
					}
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						break
					}
					// 503: Telegram re-delivers later (≈1 s+).
					rejected.Add(1)
					time.Sleep(time.Second)
				}
				select {
				case <-ch:
					lat[i] = time.Since(t0)
				case <-time.After(2 * time.Minute):
					lost.Add(1)
				}
			}(i)
		}
		wg.Wait()
		wall := time.Since(begin)
		var ok []time.Duration
		for _, d := range lat {
			if d > 0 {
				ok = append(ok, d)
			}
		}
		sort.Slice(ok, func(i, j int) bool { return ok[i] < ok[j] })
		st := pool.Stat()
		q := tracer.queries.Load()
		t.Logf("%-10s wall %-8s p50 %-8s p95 %-8s max %-8s done %d/%d, 503s %d, lost %d, SQL %d (%.1f/update), TG calls %d (429s %d), pool waits %d (%.0f ms total)",
			name, wall.Round(time.Millisecond), pct(ok, .5).Round(time.Millisecond), pct(ok, .95).Round(time.Millisecond),
			pct(ok, 1).Round(time.Millisecond), len(ok), users, rejected.Load(), lost.Load(), q, float64(q)/float64(users),
			tgSrv.calls.Load()-calls0, tgSrv.n429.Load()-n4290, st.EmptyAcquireCount()-st0.EmptyAcquireCount(),
			float64((st.AcquireDuration() - st0.AcquireDuration()).Milliseconds()))
		if testing.Verbose() {
			t.Logf("top statements of %s:\n%s", name, tracer.top(12))
		}
		if lost.Load() > 0 {
			t.Errorf("%s: %d user(s) never got an answer", name, lost.Load())
		}
		time.Sleep(cfg.UserActionInterval + 100*time.Millisecond) // per-user tap throttle
	}

	phase("open", func(i int) string { return cbOpenTestData(testIDs[0]) })
	rows, err := pool.Query(ctx, `SELECT u.telegram_id, a.id FROM test_attempts a JOIN users u ON u.id = a.user_id
		WHERE u.telegram_id >= 7100000000 AND u.telegram_id < 7200000000 AND a.status = 'in_progress'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tid, aid int64
		if err := rows.Scan(&tid, &aid); err != nil {
			t.Fatal(err)
		}
		attempt[tid-7100000000] = aid
	}
	rows.Close()
	for i, a := range attempt {
		if a == 0 {
			t.Fatalf("user %d has no attempt after opening the test", i)
		}
	}
	answer := func(name string, pos int) {
		phase(name, func(i int) string { return fmt.Sprintf("ans:%d:%d:0", attempt[i], pos) })
	}
	pos := 0
	for r := 1; r <= rounds; r++ {
		pos++
		answer(fmt.Sprintf("answer#%d", pos), pos)
	}
	if os.Getenv("LOADTEST_SCREENS") != "0" {
		// The other hot screens, everybody at once.
		phase("subject", func(int) string { return "subj:open:" + strconv.FormatInt(sid, 10) })
		phase("progress", func(int) string { return "nav:progress" })
		phase("lb-subj", func(int) string { return "lb:subj:" + strconv.FormatInt(sid, 10) })
		phase("lb-streak", func(int) string { return "lb:streak" })
		phase("weak", func(int) string { return "weak:menu" })
		phase("resume", func(int) string { return cbOpenTestData(testIDs[0]) })
	}
	if os.Getenv("LOADTEST_BROADCAST") != "0" {
		// An admin broadcast to every user is being sent while everybody answers.
		admin := repositories.NewAdminRepository(pool)
		b, err := admin.CreateBroadcastDraft(ctx, &repositories.Broadcast{AdminTgID: 1, Text: "Нагрузочная рассылка"})
		if err != nil {
			t.Fatal(err)
		}
		if _, total, err := admin.ConfirmBroadcast(ctx, b.ID); err != nil {
			t.Fatal(err)
		} else {
			t.Logf("broadcast %d queued for %d recipient(s)", b.ID, total)
		}
		w.wakeBroadcasts()
		deadline := time.Now().Add(45 * time.Second)
		for {
			st, err := admin.BroadcastStatus(ctx, b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if st == "sending" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("broadcast never started (status %s)", st)
			}
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(2 * time.Second)
		pos++
		answer(fmt.Sprintf("answer#%d+bc", pos), pos)
		_, _ = admin.CancelBroadcast(ctx, b.ID)
	}
}

func cbOpenTestData(testID int64) string { return "test:open:" + strconv.FormatInt(testID, 10) }

// applyLoadPool sizes the pool like main does.
func applyLoadPool(pc *pgxpool.Config, cfg *config.Config) {
	pc.MaxConns = int32(cfg.DBMaxConns)
	pc.MinConns = int32(min(cfg.DBMinConns, cfg.DBMaxConns))
	pc.MaxConnLifetime = cfg.DBConnMaxLifetime
	pc.MaxConnIdleTime = cfg.DBConnMaxIdleTime
}
