package handlers

import (
	"strings"
	"testing"
)

func TestTruncateUTF16(t *testing.T) {
	if got := truncateUTF16("abc", 10); got != "abc" {
		t.Fatalf("short text changed: %q", got)
	}
	// 🔴 is outside the BMP: 2 UTF-16 units each.
	s := strings.Repeat("🔴", 3000) // 6000 units, 3000 runes
	got := truncateUTF16(s, weakMenuMaxUTF16)
	if n := utf16Len(got); n > 4096 {
		t.Fatalf("truncated text is %d UTF-16 units, over the Telegram limit", n)
	}
	if !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "🔴") {
		t.Fatalf("bad truncation: %q…", got[:8])
	}
	// Mixed text: never split a surrogate pair.
	mixed := "a🔴b"
	if got := truncateUTF16(mixed, 2); got != "a…" {
		t.Fatalf("got %q, want %q", got, "a…")
	}
}
