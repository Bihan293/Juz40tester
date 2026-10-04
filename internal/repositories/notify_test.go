package repositories

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type fakeExec struct {
	sql  string
	args []any
	err  error
}

func (f *fakeExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sql, f.args = sql, args
	return pgconn.CommandTag{}, f.err
}

func TestNotifyQueueSendsPgNotify(t *testing.T) {
	f := &fakeExec{}
	notifyQueue(context.Background(), f, ChannelGenJobs)
	if f.sql != `SELECT pg_notify($1, '')` || len(f.args) != 1 || f.args[0] != "gen_jobs" {
		t.Fatalf("unexpected exec: %q %v", f.sql, f.args)
	}
}

func TestNotifyQueueErrorIsNotFatal(t *testing.T) {
	f := &fakeExec{err: errors.New("boom")}
	notifyQueue(context.Background(), f, ChannelTrJobs) // must not panic
	if f.args[0] != "tr_jobs" {
		t.Fatalf("channel = %v", f.args[0])
	}
}
