package repositories

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bankFixture creates a subject with catalog topics and n audited bank
// questions per topic. Returns the subject id.
func bankFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string, keys []string, n int) int64 {
	t.Helper()
	sid, err := testutil.CreateSubject(ctx, pool, name+" "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if _, err := pool.Exec(ctx, `
			INSERT INTO subject_topics (subject_id, topic_key, title) VALUES ($1, $2, $2);
		`, sid, k); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO topic_aliases (subject_id, alias_norm, topic_key) VALUES ($1, $2, $2)`, sid, k); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if _, err := pool.Exec(ctx, `
				INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d,
				                       correct_answer, topic, difficulty, quality_checked_at, topic_key)
				VALUES ($1, $2, 'a','b','c','d','A', $3, 2, now(), $3)`,
				sid, fmt.Sprintf("%s q%d", k, i), k); err != nil {
				t.Fatal(err)
			}
		}
	}
	return sid
}

func bankUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, off int64) *models.User {
	t.Helper()
	u, err := NewUserRepository(pool).upsertAt(ctx, &models.User{TelegramID: time.Now().UnixNano()%1_000_000_000 + 6_000_000_000 + off}, d("2026-10-01"))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func countQuestions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sid int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM questions WHERE subject_id = $1`, sid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestBankAssemblyB3: with enough audited bank questions the personal test
// is one tests row + 20 links (no new questions); solved questions are not
// handed out again (🔴 ones are); a short bank reports the missing topics;
// finishing a bank test keeps the shared questions and the user's progress.
func TestBankAssemblyB3(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	keys := []string{"т1", "т2", "т3", "т4", "т5"}
	sid := bankFixture(t, ctx, pool, "Банк B3", keys, 5)
	u := bankUser(t, ctx, pool, 1)
	before := countQuestions(t, ctx, pool, sid)

	// Solve one question of т1 (🟢) and fail one of т2 (🔴).
	var solved, red int64
	if err := pool.QueryRow(ctx, `SELECT id FROM questions WHERE subject_id = $1 AND topic_key = 'т1' ORDER BY id LIMIT 1`, sid).Scan(&solved); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM questions WHERE subject_id = $1 AND topic_key = 'т2' ORDER BY id LIMIT 1`, sid).Scan(&red); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_question_progress (user_id, question_id, status) VALUES ($1, $2, 2), ($1, $3, 0)`, u.ID, solved, red); err != nil {
		t.Fatal(err)
	}
	// An unaudited question of т1 must never be picked.
	if _, err := pool.Exec(ctx, `
		INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d, correct_answer, topic, difficulty, topic_key)
		VALUES ($1, 'unchecked', 'a','b','c','d','A', 'т1', 2, 'т1')`, sid); err != nil {
		t.Fatal(err)
	}
	before++

	test, missing, err := gen.AssembleBankPersonalTest(ctx, sid, u.ID, keys, keys, 20, "🎯 Слабые темы")
	if err != nil || test == nil || len(missing) != 0 {
		t.Fatalf("assemble: test=%v missing=%v err=%v", test, missing, err)
	}
	if !test.FromBank || test.OwnerUserID != u.ID || test.Kind != models.TestKindPersonal {
		t.Fatalf("bad test row: %+v", test)
	}
	if got := countQuestions(t, ctx, pool, sid); got != before {
		t.Fatalf("questions rows %d -> %d: bank assembly must not create questions", before, got)
	}
	var links, perTopicMax, hasSolved, unchecked int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       (SELECT MAX(c) FROM (SELECT COUNT(*) c FROM test_questions tq2 JOIN questions q2 ON q2.id = tq2.question_id WHERE tq2.test_id = $1 GROUP BY q2.topic_key) x),
		       COUNT(*) FILTER (WHERE tq.question_id = $2),
		       COUNT(*) FILTER (WHERE q.quality_checked_at IS NULL)
		FROM test_questions tq JOIN questions q ON q.id = tq.question_id WHERE tq.test_id = $1`,
		test.ID, solved).Scan(&links, &perTopicMax, &hasSolved, &unchecked); err != nil {
		t.Fatal(err)
	}
	if links != 20 || perTopicMax != 4 || hasSolved != 0 || unchecked != 0 {
		t.Fatalf("links=%d perTopicMax=%d solved=%d unchecked=%d", links, perTopicMax, hasSolved, unchecked)
	}

	// Second call: the user's existing personal test is returned (unique index).
	again, err := gen.CreateBankPersonalTest(ctx, sid, u.ID, "x", keys, []int64{red})
	if err != nil || again.ID != test.ID {
		t.Fatalf("second create: %+v %v", again, err)
	}

	// Finish: the test row goes away, the bank and the progress stay.
	if err := gen.DeletePersonalTest(ctx, u.ID, test.ID, sid); err != nil {
		t.Fatal(err)
	}
	if got := countQuestions(t, ctx, pool, sid); got != before {
		t.Fatalf("finish deleted bank questions: %d -> %d", before, got)
	}
	var progress int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_question_progress WHERE user_id = $1`, u.ID).Scan(&progress); err != nil || progress != 2 {
		t.Fatalf("progress rows = %d (%v), want 2", progress, err)
	}

	// Short bank: 6 per topic needed, т1 has only 4 left for this user.
	_, missing, err = gen.AssembleBankPersonalTest(ctx, sid, u.ID, keys[:1], keys[:1], 6, "x")
	if err != nil || len(missing) != 1 || missing[0].TopicKey != "т1" || missing[0].Missing != 2 {
		t.Fatalf("short bank: missing=%v err=%v", missing, err)
	}
	if p, _ := gen.FindPersonalTest(ctx, sid, u.ID); p != nil {
		t.Fatalf("short bank created a test: %+v", p)
	}
}

func TestBankQuota(t *testing.T) {
	q := BankQuota([]string{"a", "b", "c"}, 20)
	if q["a"] != 7 || q["b"] != 7 || q["c"] != 6 {
		t.Fatalf("quota = %v", q)
	}
	q = BankQuota([]string{"a", "b", "c", "d", "e"}, 20)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		if q[k] != 4 {
			t.Fatalf("quota = %v", q)
		}
	}
}

// TestBankAssemblyDedupesClonedContent: template clones store identical
// questions as separate rows. The bank must never put the same question
// into one test twice, nor hand out (on a clone row) a question the user
// already solved on another row.
func TestBankAssemblyDedupesClonedContent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	keys := []string{"д1"}
	// 4 distinct questions, each stored 3 times (source test + 2 clones).
	sid := bankFixture(t, ctx, pool, "Банк дубли", keys, 4)
	for c := 0; c < 2; c++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d,
			                       correct_answer, topic, difficulty, quality_checked_at, topic_key)
			SELECT subject_id, question_text, option_a, option_b, option_c, option_d,
			       correct_answer, topic, difficulty, now(), topic_key
			FROM questions WHERE subject_id = $1 AND id IN (
				SELECT MIN(id) FROM questions WHERE subject_id = $1 GROUP BY question_text)`, sid); err != nil {
			t.Fatal(err)
		}
	}
	u := bankUser(t, ctx, pool, 7)
	// The user solved «д1 q0» on its ORIGINAL row.
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_question_progress (user_id, question_id, status)
		SELECT $1, MIN(id), 2 FROM questions WHERE subject_id = $2 AND question_text = 'д1 q0'`, u.ID, sid); err != nil {
		t.Fatal(err)
	}

	// Only 3 distinct unseen questions exist — a quota of 4 is short by 1
	// (by id there would be 11 "unseen" rows).
	_, missing, err := gen.AssembleBankPersonalTest(ctx, sid, u.ID, keys, keys, 4, "x")
	if err != nil || len(missing) != 1 || missing[0].Missing != 1 {
		t.Fatalf("missing=%v err=%v (cloned rows must not count as distinct questions)", missing, err)
	}
	test, missing, err := gen.AssembleBankPersonalTest(ctx, sid, u.ID, keys, keys, 3, "x")
	if err != nil || test == nil || len(missing) != 0 {
		t.Fatalf("assemble 3: test=%v missing=%v err=%v", test, missing, err)
	}
	var links, distinct, solved int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT q.question_text),
		       COUNT(*) FILTER (WHERE q.question_text = 'д1 q0')
		FROM test_questions tq JOIN questions q ON q.id = tq.question_id WHERE tq.test_id = $1`,
		test.ID).Scan(&links, &distinct, &solved); err != nil {
		t.Fatal(err)
	}
	if links != 3 || distinct != 3 || solved != 0 {
		t.Fatalf("links=%d distinct=%d solved=%d", links, distinct, solved)
	}
}

// TestBankSkipsGivenUpQuestions: a flagged question the quality sweep gave
// up on is stamped as checked only to stop paying for repairs — it still
// fails the audit and must never be handed out by the bank.
func TestBankSkipsGivenUpQuestions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	keys := []string{"г1"}
	sid := bankFixture(t, ctx, pool, "Банк брак", keys, 3)
	var bad int64
	if err := pool.QueryRow(ctx, `SELECT MIN(id) FROM questions WHERE subject_id = $1`, sid).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if err := gen.GiveUpQualityCheck(ctx, bad); err != nil {
		t.Fatal(err)
	}
	u := bankUser(t, ctx, pool, 9)
	_, missing, err := gen.AssembleBankPersonalTest(ctx, sid, u.ID, keys, keys, 3, "x")
	if err != nil || len(missing) != 1 || missing[0].Missing != 1 {
		t.Fatalf("given-up question counted as bank material: missing=%v err=%v", missing, err)
	}
	test, _, err := gen.AssembleBankPersonalTest(ctx, sid, u.ID, keys, keys, 2, "x")
	if err != nil || test == nil {
		t.Fatalf("assemble 2: %v %v", test, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM test_questions WHERE test_id = $1 AND question_id = $2`, test.ID, bad).Scan(&n); err != nil || n != 0 {
		t.Fatalf("given-up question linked: %d %v", n, err)
	}
}
