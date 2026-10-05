package handlers

import "unicode/utf8"

// weakMenuMaxUTF16 is the budget of the «🎯 Слабые темы» text in UTF-16
// code units (how Telegram counts the 4096 message limit), leaving room for
// the ellipsis and a safety margin.
const weakMenuMaxUTF16 = 4000

// truncateUTF16 cuts s so that it is at most max UTF-16 code units long
// (an emoji outside the BMP counts as 2), appending «…» when cut. It never
// splits a rune.
func truncateUTF16(s string, max int) string {
	n := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if n+w > max {
			return s[:i] + "…"
		}
		n += w
	}
	return s
}

// utf16Len returns the length of s in UTF-16 code units.
func utf16Len(s string) int {
	n := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}
