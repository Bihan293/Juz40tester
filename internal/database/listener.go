package database

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// listenRetryMin / listenRetryMax bound the reconnect pause of Listen.
const (
	listenRetryMin = 2 * time.Second
	listenRetryMax = time.Minute
)

// Listen keeps ONE dedicated connection to databaseURL (must be a DIRECT,
// non-pooler URL: LISTEN does not work behind a transaction-mode pooler),
// subscribes to channels and calls onNotify(channel) for every NOTIFY
// received. On a connection loss it reconnects after a growing pause and,
// once LISTEN is active again, calls onNotify for every channel (a
// notification may have been missed while disconnected). Returns when ctx
// is cancelled.
func Listen(ctx context.Context, databaseURL string, channels []string, onNotify func(channel string)) {
	retry := listenRetryMin
	for ctx.Err() == nil {
		err := listenOnce(ctx, databaseURL, channels, onNotify, func() { retry = listenRetryMin })
		if ctx.Err() != nil {
			return
		}
		log.Printf("db listener: %v — reconnecting in %s", err, retry)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
		if retry *= 2; retry > listenRetryMax {
			retry = listenRetryMax
		}
	}
}

func listenOnce(ctx context.Context, databaseURL string, channels []string, onNotify func(string), connected func()) error {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(cctx)
	}()
	for _, ch := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
			return err
		}
	}
	connected()
	log.Printf("db listener: listening on %v", channels)
	// Catch up on anything enqueued while we were not listening.
	for _, ch := range channels {
		onNotify(ch)
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		onNotify(n.Channel)
	}
}
