package database

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// listenRetryMin / listenRetryMax bound the reconnect pause of Listen.
const (
	listenRetryMin = 2 * time.Second
	listenRetryMax = time.Minute
)

// listenKeepalive: an idle LISTEN connection is pinged this often. Without
// it a half-open TCP connection (NAT/LB idle timeout, a Neon compute
// restart without a FIN) blocked WaitForNotification forever: the listener
// looked alive while no NOTIFY ever arrived, and jobs of other instances
// waited for the 3-minute fallback poll. A failed ping reconnects.
var listenKeepalive = 60 * time.Second

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
		wctx, cancel := context.WithTimeout(ctx, listenKeepalive)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) || wctx.Err() != nil {
				// Idle period: prove the connection is still alive.
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				perr := conn.Ping(pctx)
				pcancel()
				if perr != nil {
					return fmt.Errorf("keepalive ping: %w", perr)
				}
				continue
			}
			return err
		}
		onNotify(n.Channel)
	}
}
