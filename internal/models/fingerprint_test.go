package models

import "testing"

func TestWeakTopicsFingerprint(t *testing.T) {
	a := WeakTopicsFingerprint(1, []string{"Генетика", "клетка", "Экология"})
	b := WeakTopicsFingerprint(1, []string{" экология", "КЛЕТКА", "генетика", "генетика"})
	if a != b || len(a) != 32 {
		t.Fatalf("same set must give the same fingerprint: %s vs %s", a, b)
	}
	if a == WeakTopicsFingerprint(2, []string{"Генетика", "клетка", "Экология"}) {
		t.Fatal("subjects must not share fingerprints")
	}
	if a == WeakTopicsFingerprint(1, []string{"Генетика", "клетка"}) {
		t.Fatal("different sets must differ")
	}
	c1 := ChainFingerprint(1, 5, 40, []string{"x", "y"})
	if c1 != ChainFingerprint(1, 5, 40, []string{"Y", "x"}) || c1 == ChainFingerprint(1, 5, 41, []string{"x", "y"}) {
		t.Fatal("chain fingerprint: order-insensitive, previous-test sensitive")
	}
	if !SameTopicSet([]string{"A", "b"}, []string{"B", "a", "a"}) || SameTopicSet([]string{"a"}, []string{"b"}) {
		t.Fatal("SameTopicSet")
	}
}
