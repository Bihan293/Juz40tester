package repositories

import (
	"reflect"
	"testing"
)

func TestOptionOrderCompactRoundTrip(t *testing.T) {
	enc, err := encodeOptionOrder([]string{"C", "A", "D", "B"})
	if err != nil || enc != `"CADB"` {
		t.Fatalf("encode = %q, %v; want \"CADB\"", enc, err)
	}
	got, err := decodeOptionOrder([]byte(enc))
	if err != nil || !reflect.DeepEqual(got, []string{"C", "A", "D", "B"}) {
		t.Fatalf("decode compact = %v, %v", got, err)
	}
}

func TestOptionOrderReadsLegacyArray(t *testing.T) {
	got, err := decodeOptionOrder([]byte(`["B", "D", "A", "C"]`))
	if err != nil || !reflect.DeepEqual(got, []string{"B", "D", "A", "C"}) {
		t.Fatalf("decode legacy = %v, %v", got, err)
	}
	if _, err := decodeOptionOrder([]byte(`{}`)); err == nil {
		t.Fatal("want error for unknown format")
	}
}
