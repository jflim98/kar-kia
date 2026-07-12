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

		// Shape: "<minute> 3 * * *" with minute in [0,10] (the 03:00 hour + jitter).
		fields := strings.Fields(spec)
		if len(fields) != 5 || fields[1] != "3" || fields[2] != "*" || fields[3] != "*" || fields[4] != "*" {
			t.Fatalf("id %d: unexpected spec %q", id, spec)
		}
		min, err := strconv.Atoi(fields[0])
		if err != nil || min < 0 || min > 10 {
			t.Fatalf("id %d: minute out of range in %q", id, spec)
		}
	}
}
