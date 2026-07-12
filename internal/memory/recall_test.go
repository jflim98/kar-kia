package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMem(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newRecallManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	m := New(dir, filepath.Join(dir, "persona.md"), "UTC")
	writeMem(t, dir, filepath.Join("daily_memory", "04-06-26.md"),
		"# 04-06-26\n\nPlanned the Kyoto trip: booked flights and a ryokan.\n\nFixed the billing bug in checkout.")
	writeMem(t, dir, filepath.Join("daily_memory", "05-06-26.md"),
		"# 05-06-26\n\nDiscussed the quarterly budget and hiring plans.")
	writeMem(t, dir, "long_term_memory.md",
		"- (2026-01-02) The user prefers terse replies.\n- (2026-02-03) The user is allergic to peanuts.")
	writeMem(t, dir, filepath.Join("users", "7.md"), "- (2026-03-03) Works as a pediatric nurse.")
	return m, dir
}

func TestRecallQueryRanksRelevantChunk(t *testing.T) {
	m, _ := newRecallManager(t)

	out, err := m.Recall("kyoto trip", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Kyoto trip") {
		t.Fatalf("expected the Kyoto chunk, got:\n%s", out)
	}
	if !strings.Contains(out, "[04-06-26]") {
		t.Fatalf("hit should be tagged with its source day:\n%s", out)
	}
	// An unrelated chunk should not be the top hit.
	if strings.HasPrefix(out, "[05-06-26]") {
		t.Fatalf("budget hijacked by an unrelated chunk:\n%s", out)
	}
}

func TestRecallStemmingAndTypoTolerance(t *testing.T) {
	m, _ := newRecallManager(t)

	// "flight" should match the stored plural "flights" (stemming).
	if out, _ := m.Recall("flight booking", "", 3); !strings.Contains(out, "booked flights") {
		t.Fatalf("stemming miss (flight->flights):\n%s", out)
	}
	// "budgte" (typo) should still reach "budget" (edit distance 1).
	if out, _ := m.Recall("budgte", "", 3); !strings.Contains(out, "quarterly budget") {
		t.Fatalf("typo tolerance miss (budgte->budget):\n%s", out)
	}
}

func TestRecallSynonymMissesWithoutExpansion(t *testing.T) {
	m, _ := newRecallManager(t)

	// Pure lexical: "vacation" does not match "trip". This documents the known limitation
	// the tool description mitigates by asking the model to supply synonyms.
	if out, _ := m.Recall("vacation", "", 3); out != "No matching memory." {
		t.Fatalf("expected a synonym miss for 'vacation', got:\n%s", out)
	}
	// With the synonym included (as the model is told to do), it hits.
	if out, _ := m.Recall("vacation trip", "", 3); !strings.Contains(out, "Kyoto trip") {
		t.Fatalf("expanded query should hit:\n%s", out)
	}
}

func TestRecallSearchesLongTermAndProfiles(t *testing.T) {
	m, _ := newRecallManager(t)

	if out, _ := m.Recall("peanuts allergy", "", 3); !strings.Contains(out, "[long-term]") {
		t.Fatalf("should reach long-term memory:\n%s", out)
	}
	if out, _ := m.Recall("nurse", "", 3); !strings.Contains(out, "[about user 7]") {
		t.Fatalf("should reach user profiles:\n%s", out)
	}
}

func TestRecallDateMode(t *testing.T) {
	m, _ := newRecallManager(t)

	out, err := m.Recall("", "04-06-26", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ryokan") || !strings.HasPrefix(out, "[04-06-26]") {
		t.Fatalf("date mode should return the full day note:\n%s", out)
	}

	if out, _ := m.Recall("", "01-01-26", 0); !strings.Contains(out, "No stored note") {
		t.Fatalf("missing day should report no note:\n%s", out)
	}
	if _, err := m.Recall("", "not-a-date", 0); err == nil {
		t.Fatal("malformed date should error")
	}
}

func TestRecallEmptyArgs(t *testing.T) {
	m, _ := newRecallManager(t)
	if _, err := m.Recall("", "", 0); err == nil {
		t.Fatal("empty query and date should error")
	}
}
