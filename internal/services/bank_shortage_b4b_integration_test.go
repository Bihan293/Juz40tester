package services

import (
	"context"
	"fmt"
	"testing"
)

// TestBankShortageQueuesTopicBatchesB4b: a short bank queues topic_batch
// jobs ONLY for the short topics (no personal job, the user waits); once the
// batches are done the test is assembled from the bank without new
// per-user questions rows.
func TestBankShortageQueuesTopicBatchesB4b(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u := e.user(t, "BankShort")
	t1 := e.firstChainTest(t, 1)
	e.play(t, u, t1, notIn("Тема 0", "Тема 1"))
	keys, err := e.gen.WeakTopicKeys(ctx, u, e.sid, 5)
	if err != nil || len(keys) != 2 {
		t.Fatalf("weak keys = %v, %v", keys, err)
	}
	full, short := keys[0], keys[1]
	for _, k := range keys {
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO subject_topics (subject_id, topic_key, title) VALUES ($1, $2, $2)
			ON CONFLICT DO NOTHING`, e.sid, k); err != nil {
			t.Fatal(err)
		}
	}
	// Only the first weak topic has enough bank questions.
	for i := 0; i < 30; i++ {
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d,
			                       correct_answer, topic, difficulty, quality_checked_at, topic_key)
			VALUES ($1, $2, 'a','b','c','d','A', $3, 2, now(), $3)`,
			e.sid, fmt.Sprintf("bank %s %d", full, i), full); err != nil {
			t.Fatal(err)
		}
	}

	p, pending, _, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid)
	if err != nil || p != nil || !pending {
		t.Fatalf("short bank: test=%+v pending=%v err=%v", p, pending, err)
	}
	// Twice in parallel-ish: still one job per short topic.
	if _, _, _, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid); err != nil {
		t.Fatal(err)
	}
	rows, err := e.pool.Query(ctx, `
		SELECT kind, COALESCE(topic_key, '') FROM generation_jobs
		WHERE subject_id = $1 AND status IN ('pending','running') ORDER BY id`, e.sid)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var kind, key string
		if err := rows.Scan(&kind, &key); err != nil {
			t.Fatal(err)
		}
		got = append(got, kind+":"+key)
	}
	rows.Close()
	if len(got) != 1 || got[0] != "topic_batch:"+short {
		t.Fatalf("active jobs = %v, want exactly [topic_batch:%s]", got, short)
	}

	e.drain(t)
	var before int
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM questions WHERE subject_id = $1`, e.sid).Scan(&before)
	p, pending, _, err = e.quiz.EnsurePersonalTest(ctx, u, e.sid)
	if err != nil || p == nil || pending || !p.FromBank {
		t.Fatalf("after batch: test=%+v pending=%v err=%v", p, pending, err)
	}
	var after, personal int
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM questions WHERE subject_id = $1`, e.sid).Scan(&after)
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM generation_jobs WHERE subject_id = $1 AND kind = 'personal'`, e.sid).Scan(&personal)
	if after != before || personal != 0 {
		t.Fatalf("assembly after batch: questions %d -> %d, personal jobs %d", before, after, personal)
	}
}
