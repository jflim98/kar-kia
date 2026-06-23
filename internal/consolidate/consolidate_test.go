package consolidate

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseDayOutput(t *testing.T) {
	out := `GIST: Alice planned a trip and set a reminder.
SUMMARY:
- Alice is going to Tokyo in July.
- Set a reminder to book flights.
USER_NOTES:
5: Prefers window seats.
9: none-of-this-parses
none`
	gist, body, notes := parseDayOutput(out)
	if gist != "Alice planned a trip and set a reminder." {
		t.Fatalf("gist = %q", gist)
	}
	if !strings.Contains(body, "Tokyo") || strings.Contains(body, "USER_NOTES") {
		t.Fatalf("body wrong: %q", body)
	}
	if notes[5] != "Prefers window seats." {
		t.Fatalf("user note 5 = %q", notes[5])
	}
	if _, ok := notes[9]; !ok {
		t.Fatalf("expected a note for uid 9")
	}
}

func TestParseDayOutputDegraded(t *testing.T) {
	// No markers at all — gist is the first line, body is the whole thing.
	gist, body, notes := parseDayOutput("just a blob\nof text")
	if gist != "just a blob" || !strings.Contains(body, "blob") {
		t.Fatalf("degraded parse wrong: gist=%q body=%q", gist, body)
	}
	if len(notes) != 0 {
		t.Fatalf("expected no notes, got %v", notes)
	}
}

// --- orchestrator with fakes ---

type fakeMem struct {
	pending  []string
	aged     []string
	dayFiles map[string]string
	written  map[string]string // day -> body
	gists    map[string]string
	userNote map[int64]string
	profiles map[int64]string // existing + reconciled user profiles
	longTerm []string
	deleted  []string
	rawArg   int // retentionDays passed to PruneRawLogs
}

func (f *fakeMem) PendingRawDays() []string                 { return f.pending }
func (f *fakeMem) RawTranscript(day string) (string, error) { return "transcript for " + day, nil }
func (f *fakeMem) WriteDayFile(day, gist, body string) error {
	f.written[day] = body
	f.gists[day] = gist
	return nil
}
func (f *fakeMem) AppendUserNote(uid int64, note string) error { f.userNote[uid] = note; return nil }
func (f *fakeMem) ReadUserProfile(uid int64) (string, error)   { return f.profiles[uid], nil }
func (f *fakeMem) WriteUserProfile(uid int64, content string) error {
	if f.profiles == nil {
		f.profiles = map[int64]string{}
	}
	f.profiles[uid] = content
	return nil
}
func (f *fakeMem) AgedDayFiles(int, time.Time) []string        { return f.aged }
func (f *fakeMem) ReadDayFile(day string) (string, error)      { return f.dayFiles[day], nil }
func (f *fakeMem) AppendLongTerm(text string) error {
	f.longTerm = append(f.longTerm, text)
	return nil
}
func (f *fakeMem) DeleteDayFile(day string) error { f.deleted = append(f.deleted, day); return nil }
func (f *fakeMem) PruneRawLogs(retentionDays int, _ time.Time) (int, error) {
	f.rawArg = retentionDays
	return 0, nil
}

type fakeSum struct{ reply func(string) string }

func (f fakeSum) Summarize(_ context.Context, instr string) (string, error) {
	return f.reply(instr), nil
}

func TestRunEpisodicAndAgeOut(t *testing.T) {
	mem := &fakeMem{
		pending:  []string{"01-06-26"},
		aged:     []string{"01-01-26", "02-01-26"},
		dayFiles: map[string]string{"01-01-26": "old note A", "02-01-26": "old note B"},
		written:  map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
	}
	sum := fakeSum{reply: func(instr string) string {
		if strings.Contains(instr, "Consolidate one day") {
			return "GIST: a day\nSUMMARY:\nstuff happened\nUSER_NOTES:\n7: likes tea"
		}
		// age-out: durable for A, "none" for B
		if strings.Contains(instr, "old note A") {
			return "- A is durable"
		}
		return "none"
	}}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if mem.rawArg != 30 {
		t.Fatalf("PruneRawLogs should get rawRetentionDays=30, got %d", mem.rawArg)
	}

	if mem.written["01-06-26"] != "stuff happened" || mem.gists["01-06-26"] != "a day" {
		t.Fatalf("episodic write wrong: %+v / %+v", mem.written, mem.gists)
	}
	if mem.userNote[7] != "likes tea" {
		t.Fatalf("user note wrong: %v", mem.userNote)
	}
	// Only A produced a durable fact; both day files are deleted after age-out.
	if len(mem.longTerm) != 1 || !strings.Contains(mem.longTerm[0], "A is durable") {
		t.Fatalf("long-term wrong: %v", mem.longTerm)
	}
	if len(mem.deleted) != 2 {
		t.Fatalf("both aged notes should be deleted, got %v", mem.deleted)
	}
}

func TestRunReconcilesExistingProfile(t *testing.T) {
	mem := &fakeMem{
		pending:  []string{"01-06-26"},
		written:  map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
		profiles: map[int64]string{7: "- Lives in Berlin\n- Likes tea"},
	}
	sum := fakeSum{reply: func(instr string) string {
		if strings.Contains(instr, "Consolidate one day") {
			return "GIST: a day\nSUMMARY:\nstuff\nUSER_NOTES:\n7: Moved to Munich"
		}
		if strings.Contains(instr, "Update a person's memory profile") {
			// The reconcile call sees both the old profile and the new fact.
			if !strings.Contains(instr, "Berlin") || !strings.Contains(instr, "Munich") {
				t.Fatalf("reconcile prompt missing context: %q", instr)
			}
			return "- Lives in Munich\n- Likes tea"
		}
		return "none"
	}}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if mem.profiles[7] != "- Lives in Munich\n- Likes tea" {
		t.Fatalf("profile not reconciled: %q", mem.profiles[7])
	}
	if _, appended := mem.userNote[7]; appended {
		t.Fatalf("should reconcile, not append, when a profile exists: %v", mem.userNote)
	}
}

func TestReconcileFallsBackToAppendOnError(t *testing.T) {
	mem := &fakeMem{
		pending:  []string{"01-06-26"},
		written:  map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
		profiles: map[int64]string{7: "- Likes tea"},
	}
	sum := fakeSum{reply: func(instr string) string {
		if strings.Contains(instr, "Consolidate one day") {
			return "GIST: a day\nSUMMARY:\nstuff\nUSER_NOTES:\n7: Likes coffee too"
		}
		return "" // reconcile returns empty -> fall back to append, never lose the fact
	}}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if mem.userNote[7] != "Likes coffee too" {
		t.Fatalf("fact lost on reconcile failure: %v", mem.userNote)
	}
}
