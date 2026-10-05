package repositories

import (
	"context"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// TestTopicStatsMergeByKey (B2): rows of alias spellings are summed into
// the catalog topic row (totals unchanged), live answers are recorded by
// topic_key, weak topics are returned as keys.
func TestTopicStatsMergeByKey(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	users := NewUserRepository(pool)

	sid, err := testutil.CreateSubject(ctx, pool, "Статистика B2 "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := users.upsertAt(ctx, &models.User{TelegramID: time.Now().UnixNano()%1_000_000_000 + 5_000_000_000}, d("2026-10-01"))
	if err != nil {
		t.Fatal(err)
	}
	// Catalog: «Генетика» with alias «наследственность».
	if _, err := pool.Exec(ctx, `
		INSERT INTO subject_topics (subject_id, topic_key, title) VALUES ($1, 'генетика', 'Генетика'), ($1, 'клетка', 'Клетка');
	`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO topic_aliases (subject_id, alias_norm, topic_key)
		VALUES ($1, 'генетика', 'генетика'), ($1, 'наследственность', 'генетика'), ($1, 'клетка', 'клетка')`, sid); err != nil {
		t.Fatal(err)
	}
	// Old statistics: two rows of the same topic under two spellings.
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_topic_stats (user_id, subject_id, topic_key, topic, correct_count, wrong_count, recent, updated_at) VALUES
		($1, $2, 'генетика',         'Генетика',         3, 2, '11100', now() - interval '2 hours'),
		($1, $2, 'наследственность', 'Наследственность', 1, 4, '0001',  now() - interval '1 hour'),
		($1, $2, 'клетка',           'Клетка',           9, 0, '111111111', now())`, u.ID, sid); err != nil {
		t.Fatal(err)
	}
	sums := func() (rows, c, w int) {
		if err := pool.QueryRow(ctx, `
			SELECT COUNT(*), COALESCE(SUM(correct_count),0), COALESCE(SUM(wrong_count),0)
			FROM user_topic_stats WHERE user_id = $1 AND subject_id = $2`, u.ID, sid).Scan(&rows, &c, &w); err != nil {
			t.Fatal(err)
		}
		return
	}
	_, c0, w0 := sums()
	if _, err := gen.MergeTopicStatsByKey(ctx); err != nil {
		t.Fatal(err)
	}
	rows, c1, w1 := sums()
	if rows != 2 || c1 != c0 || w1 != w0 {
		t.Fatalf("after merge rows=%d c=%d/%d w=%d/%d", rows, c1, c0, w1, w0)
	}
	var title, recent string
	var c, w int
	if err := pool.QueryRow(ctx, `
		SELECT topic, correct_count, wrong_count, recent FROM user_topic_stats
		WHERE user_id = $1 AND subject_id = $2 AND topic_key = 'генетика'`, u.ID, sid).Scan(&title, &c, &w, &recent); err != nil {
		t.Fatal(err)
	}
	if title != "Генетика" || c != 4 || w != 6 || recent != "111000001" {
		t.Fatalf("merged row: %q %d %d %q", title, c, w, recent)
	}
	// Idempotent.
	if n, err := gen.MergeTopicStatsByKey(ctx); err != nil || n != 0 {
		t.Fatalf("second merge: n=%d err=%v", n, err)
	}

	// Live answer to a question WITHOUT topic_key whose topic is an alias:
	// counted into the catalog row.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordTopicAnswer(ctx, tx, u.ID, sid, "Наследственность", "", false); err != nil {
		t.Fatal(err)
	}
	// Live answer with topic_key.
	if err := recordTopicAnswer(ctx, tx, u.ID, sid, "что угодно", "клетка", true); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows, c2, w2 := sums()
	if rows != 2 || c2 != c0+1 || w2 != w0+1 {
		t.Fatalf("live answers: rows=%d c=%d w=%d", rows, c2, w2)
	}

	keys, err := gen.WeakTopicKeys(ctx, u.ID, sid, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "генетика" {
		t.Fatalf("weak keys = %v, want [генетика]", keys)
	}
	titles, err := gen.WeakTopics(ctx, u.ID, sid, 5)
	if err != nil || len(titles) != 1 || titles[0] != "Генетика" {
		t.Fatalf("weak titles = %v %v", titles, err)
	}
}
