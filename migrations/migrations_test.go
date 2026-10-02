package migrations

import (
	"strconv"
	"strings"
	"testing"
)

// #41: migration numbers must be unique and contiguous (000001..N), so a
// second «000012_*» (or a gap) is caught before deploy.
func TestMigrationVersionsUniqueAndContiguous(t *testing.T) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]string{}
	max := 0
	for _, e := range entries {
		name := e.Name()
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			t.Fatalf("bad migration name %q", name)
		}
		if prev, ok := seen[v]; ok {
			t.Fatalf("duplicate migration number %d: %s and %s", v, prev, name)
		}
		seen[v] = name
		if v > max {
			max = v
		}
	}
	for v := 1; v <= max; v++ {
		if _, ok := seen[v]; !ok {
			t.Fatalf("gap in migration numbers: %06d is missing", v)
		}
	}
}
