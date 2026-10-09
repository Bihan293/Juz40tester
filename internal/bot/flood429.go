package bot

import (
	"sync"
	"time"
)

// Telegram answers 429 for two different reasons and does not say which:
//
//   - per-chat flood control (≈1 message per second in one chat, a user
//     tapping fast): only THAT chat must wait;
//   - the bot-wide limit (≈30 messages per second): everybody must wait.
//
// Before, every 429 paused the global limiter for min(retry_after, 5 s), so
// a single fast-tapping user froze the bot for all other users. Under load
// (1000 users answering at once, 1 % of the chats with one per-chat 429)
// that stretched the answer round from 36 s to 59 s. Now a 429 pauses
// everybody only when it looks bot-wide: the request has no chat, or
// several DIFFERENT chats got a 429 within globalFloodWindow. A lone
// per-chat 429 only blocks its chat (floodControl.block) and is waited out
// by that chat alone.

// globalFloodWindow / globalFloodChats: this many distinct chats with a 429
// within the window mean the bot-wide limit was hit.
const (
	globalFloodWindow = 2 * time.Second
	globalFloodChats  = 3
)

// floodDetector classifies 429 answers as per-chat or bot-wide.
type floodDetector struct {
	mu     sync.Mutex
	recent []flood429 // newest last, at most globalFloodChats*4 entries
	now    func() time.Time
}

type flood429 struct {
	chat int64
	at   time.Time
}

func newFloodDetector() *floodDetector { return &floodDetector{now: time.Now} }

// global records a 429 for chatID (0 = a request without a chat) and
// reports whether the whole bot must pause.
func (d *floodDetector) global(chatID int64) bool {
	if d == nil || chatID == 0 {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	keep := d.recent[:0]
	for _, r := range d.recent {
		if now.Sub(r.at) < globalFloodWindow {
			keep = append(keep, r)
		}
	}
	d.recent = append(keep, flood429{chat: chatID, at: now})
	if n := len(d.recent); n > globalFloodChats*4 {
		d.recent = append(d.recent[:0], d.recent[n-globalFloodChats*4:]...)
	}
	distinct := map[int64]struct{}{}
	for _, r := range d.recent {
		distinct[r.chat] = struct{}{}
	}
	return len(distinct) >= globalFloodChats
}
