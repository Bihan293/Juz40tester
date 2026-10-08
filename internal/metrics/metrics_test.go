package metrics

import (
	"strings"
	"testing"
)

func TestCountersAndGauges(t *testing.T) {
	Reset()
	Inc("c_total", "result", "ok")
	Add("c_total", 2, "result", "ok")
	SetGauge("q_pending", 7)
	SetGauge("q_pending", 3)
	if Get("c_total", "result", "ok") != 3 || Gauge("q_pending") != 3 {
		t.Fatalf("counter=%v gauge=%v", Get("c_total", "result", "ok"), Gauge("q_pending"))
	}
	var b strings.Builder
	WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{"# TYPE c_total counter", `c_total{result="ok"} 3`, "# TYPE q_pending gauge", "q_pending 3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	Reset()
	if Gauge("q_pending") != 0 {
		t.Fatal("Reset must clear gauges")
	}
}
