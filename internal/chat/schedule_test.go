package chat

import (
	"strconv"
	"strings"
	"testing"
)

func TestConsolidationSpec(t *testing.T) {
	ids := []int64{1, 42, -1001234567890, 0, -7}
	for _, id := range ids {
		spec := consolidationSpec(id)

		// Stable: same ID always yields the same spec.
		if again := consolidationSpec(id); again != spec {
			t.Fatalf("id %d: not deterministic: %q vs %q", id, spec, again)
		}

		// Shape: "<minute> 7 * * *" with minute in [2,12] (the 07:00 hour + jitter, starting
		// at :02 so the 07:01 session prune stays ahead of every chat).
		fields := strings.Fields(spec)
		if len(fields) != 5 || fields[1] != "7" || fields[2] != "*" || fields[3] != "*" || fields[4] != "*" {
			t.Fatalf("id %d: unexpected spec %q", id, spec)
		}
		min, err := strconv.Atoi(fields[0])
		if err != nil || min < 2 || min > 12 {
			t.Fatalf("id %d: minute out of range in %q", id, spec)
		}
	}
}
