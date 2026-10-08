package models

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// WeakTopicsFingerprint is the identity of a weak-topics profile: the
// SORTED, normalised, de-duplicated set of topic keys of one subject.
//
// Two users of the same subject whose weak topics are the same set (in any
// order, any spelling that normalises equally) get the same fingerprint —
// and therefore the same test: the bank assembly orders candidates by the
// fingerprint (identical selection for identical histories) and an
// AI-generated test is stored once as a template and CLONED for every
// further user with that fingerprint (see repositories.TestTemplate).
//
// The fingerprint is a 32-hex-char prefix of SHA-256 (128 bits — collisions
// are not a practical concern) and never contains user data.
func WeakTopicsFingerprint(subjectID int64, topicKeys []string) string {
	return fingerprint("personal", strconv.FormatInt(subjectID, 10), normKeys(topicKeys))
}

// ChainFingerprint identifies the INPUT of a chain-test generation: the
// subject, the chain position, the previous test it builds on and the weak
// topics it must train. A regeneration of the same chain slot with the same
// inputs can reuse the stored template instead of paying for a new
// generation.
func ChainFingerprint(subjectID int64, testNumber int, prevTestID int64, weakKeys []string) string {
	return fingerprint("chain",
		strconv.FormatInt(subjectID, 10)+"/"+strconv.Itoa(testNumber)+"/"+strconv.FormatInt(prevTestID, 10),
		normKeys(weakKeys))
}

func normKeys(keys []string) []string {
	seen := make(map[string]bool, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		n := NormalizeTopic(k)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func fingerprint(kind, scope string, keys []string) string {
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(scope))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// SameTopicSet reports whether two topic lists are the same set after
// normalisation (order and duplicates ignored).
func SameTopicSet(a, b []string) bool {
	na, nb := normKeys(a), normKeys(b)
	return strings.Join(na, "\x00") == strings.Join(nb, "\x00")
}
