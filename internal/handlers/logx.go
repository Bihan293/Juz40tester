package handlers

import (
	"log"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
)

// forbiddenLogEvery: Telegram 403 (the user blocked the bot) is expected
// noise — such errors are counted and summarised at most this often
// instead of one log line per failed call (R-10b).
const forbiddenLogEvery = 10 * time.Minute

var forbiddenLog struct {
	mu   sync.Mutex
	n    int
	last time.Time
}

// logf is log.Printf that throttles Telegram 403 errors.
func logf(format string, args ...any) {
	for _, a := range args {
		if err, ok := a.(error); ok && bot.IsForbidden(err) {
			forbiddenLog.mu.Lock()
			forbiddenLog.n++
			if time.Since(forbiddenLog.last) < forbiddenLogEvery {
				forbiddenLog.mu.Unlock()
				return
			}
			n := forbiddenLog.n
			forbiddenLog.n, forbiddenLog.last = 0, time.Now()
			forbiddenLog.mu.Unlock()
			log.Printf("telegram 403 (bot blocked by user): %d call(s) since last report; latest: "+format, append([]any{n}, args...)...)
			return
		}
	}
	log.Printf(format, args...)
}
