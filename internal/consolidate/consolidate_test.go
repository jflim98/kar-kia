package consolidate

import (
	"context"
	"errors"
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
	pending      []string
	transcripts  map[string]string // day -> transcript; nil map => "transcript for <day>"
	aged         []string
	dayFiles     map[string]string
	written      map[string]string // day -> body
	gists        map[string]string
	userNote     map[int64]string
	profiles     map[int64]string // existing + reconciled user profiles
	longTerm     []string         // appended facts
	longTermFile string           // whole-file contents, for the compaction path
	appendLTErr  error
	deleted      []string
	rawDeleted   []string
	rawArg       int // retentionDays passed to PruneRawLogs
}

func (f *fakeMem) PendingRawDays() []string { return f.pending }
func (f *fakeMem) RawTranscript(day string) (string, error) {
	if f.transcripts != nil {
		return f.transcripts[day], nil
	}
	return "transcript for " + day, nil
}
func (f *fakeMem) DeleteRawLog(day string) error {
	f.rawDeleted = append(f.rawDeleted, day)
	return nil
}
func (f *fakeMem) WriteDayFile(day, gist, body string) error {
	f.written[day] = body
	f.gists[day] = gist
	return nil
}
func (f *fakeMem) AppendUserNote(uid int64, note string) error { f.userNote[uid] = note; return nil }
func (f *fakeMem) ReadUserProfile(uid int64) (string, error)   { return f.profiles[uid], nil }
func (f *fakeMem) WriteUserProfileIf(uid int64, content, expected string) (bool, error) {
	if f.profiles[uid] != expected {
		return false, nil
	}
	if f.profiles == nil {
		f.profiles = map[int64]string{}
	}
	f.profiles[uid] = content
	return true, nil
}
func (f *fakeMem) ListUserProfiles() ([]int64, error) {
	var uids []int64
	for uid := range f.profiles {
		uids = append(uids, uid)
	}
	return uids, nil
}
func (f *fakeMem) AgedDayFiles(int, time.Time) []string   { return f.aged }
func (f *fakeMem) ReadDayFile(day string) (string, error) { return f.dayFiles[day], nil }
func (f *fakeMem) ReadLongTerm() (string, error)          { return f.longTermFile, nil }
func (f *fakeMem) AppendLongTerm(text string) error {
	if f.appendLTErr != nil {
		return f.appendLTErr
	}
	f.longTerm = append(f.longTerm, text)
	return nil
}
func (f *fakeMem) WriteLongTermIf(content, expected string) (bool, error) {
	if f.longTermFile != expected {
		return false, nil
	}
	f.longTermFile = content
	return true, nil
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

func TestAgeOutKeepsNoteWhenLongTermAppendFails(t *testing.T) {
	mem := &fakeMem{
		aged:     []string{"01-01-26"},
		dayFiles: map[string]string{"01-01-26": "old note"},
		written:  map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
		appendLTErr: errors.New("disk full"),
	}
	sum := fakeSum{reply: func(string) string { return "- a durable fact" }}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(mem.deleted) != 0 {
		t.Fatalf("note must be kept when the long-term append fails (facts would be lost), deleted=%v", mem.deleted)
	}
}

func TestAgeOutTreatsNoneVariantsAsEmpty(t *testing.T) {
	mem := &fakeMem{
		aged:     []string{"01-01-26"},
		dayFiles: map[string]string{"01-01-26": "old note"},
		written:  map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
	}
	sum := fakeSum{reply: func(string) string { return "None." }}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(mem.longTerm) != 0 {
		t.Fatalf(`"None." must not be stored as a fact: %v`, mem.longTerm)
	}
	if len(mem.deleted) != 1 {
		t.Fatalf("the empty note should still age out, deleted=%v", mem.deleted)
	}
}

func TestEmptyTranscriptDropsRawLog(t *testing.T) {
	mem := &fakeMem{
		pending:     []string{"01-06-26"},
		transcripts: map[string]string{"01-06-26": "   "},
		written:     map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
	}
	sum := fakeSum{reply: func(string) string {
		t.Fatal("nothing should be summarized for an empty day")
		return ""
	}}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(mem.rawDeleted) != 1 || mem.rawDeleted[0] != "01-06-26" {
		t.Fatalf("empty raw day should be dropped so it doesn't stay pending forever: %v", mem.rawDeleted)
	}
	if len(mem.written) != 0 {
		t.Fatalf("no day note should be written for an empty day: %v", mem.written)
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

func TestReconcileNeverClobbersConcurrentWrite(t *testing.T) {
	mem := &fakeMem{
		pending:  []string{"01-06-26"},
		written:  map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
		profiles: map[int64]string{7: "- Likes tea"},
	}
	sum := fakeSum{reply: func(instr string) string {
		if strings.Contains(instr, "Consolidate one day") {
			return "GIST: g\nSUMMARY:\ns\nUSER_NOTES:\n7: Got a dog"
		}
		if strings.Contains(instr, "Update a person's memory profile") {
			// A live propose_memory save lands while the summarizer is thinking.
			mem.profiles[7] = "- Likes tea\n- (2026-07-02) Allergic to peanuts"
			return "- Likes tea\n- Has a dog"
		}
		return "none"
	}}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mem.profiles[7], "peanuts") {
		t.Fatalf("concurrent save was clobbered by the reconcile rewrite: %q", mem.profiles[7])
	}
	if mem.userNote[7] != "Got a dog" {
		t.Fatalf("the day's fact must land via append fallback: %v", mem.userNote)
	}
}

func TestReconcileRejectsImplausibleShrink(t *testing.T) {
	big := strings.TrimSpace(strings.Repeat("- A durable fact about this user's life.\n", 20))
	mem := &fakeMem{
		pending:  []string{"01-06-26"},
		written:  map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
		profiles: map[int64]string{7: big},
	}
	sum := fakeSum{reply: func(instr string) string {
		if strings.Contains(instr, "Consolidate one day") {
			return "GIST: g\nSUMMARY:\ns\nUSER_NOTES:\n7: New fact"
		}
		if strings.Contains(instr, "Update a person's memory profile") {
			return "ok" // a refusal-sized reply must not wipe the profile
		}
		return "none"
	}}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if mem.profiles[7] != big {
		t.Fatalf("implausibly short rewrite must not replace the profile: %q", mem.profiles[7])
	}
	if mem.userNote[7] != "New fact" {
		t.Fatalf("fact must land via append fallback: %v", mem.userNote)
	}
}

func TestCompactLongTermWhenOversized(t *testing.T) {
	big := strings.TrimSpace(strings.Repeat("- (2026-06-01) The user prefers tea over coffee.\n", 120))
	mem := &fakeMem{
		longTermFile: big,
		written:      map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
	}
	sum := fakeSum{reply: func(instr string) string {
		if strings.Contains(instr, "Compact this long-term memory") {
			return "- (2026-06-01) Prefers tea over coffee."
		}
		return "none"
	}}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if mem.longTermFile != "- (2026-06-01) Prefers tea over coffee." {
		t.Fatalf("oversized long-term memory should be compacted: %q", mem.longTermFile)
	}
}

func TestCompactLongTermKeepsFileOnBadOutput(t *testing.T) {
	big := strings.TrimSpace(strings.Repeat("- (2026-06-01) A durable fact.\n", 200))
	mem := &fakeMem{
		longTermFile: big,
		written:      map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
	}
	sum := fakeSum{reply: func(string) string { return "I can't help with that." }}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if mem.longTermFile != big {
		t.Fatalf("a non-bullet reply must never replace long-term memory: %q", mem.longTermFile)
	}
}

func TestSweepCompactsProfileWithDatedAppends(t *testing.T) {
	mem := &fakeMem{
		written:  map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
		profiles: map[int64]string{7: "- Likes tea\n- (2026-07-01) Moved to Oslo"},
	}
	sum := fakeSum{reply: func(instr string) string {
		if strings.Contains(instr, "Clean up a person's memory profile") {
			if !strings.Contains(instr, "Oslo") {
				t.Fatalf("compact prompt missing profile content: %q", instr)
			}
			return "- Likes tea\n- Lives in Oslo"
		}
		return "none"
	}}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if mem.profiles[7] != "- Likes tea\n- Lives in Oslo" {
		t.Fatalf("profile with dated appends should be compacted by the sweep: %q", mem.profiles[7])
	}
}

func TestSweepSkipsCleanSmallProfiles(t *testing.T) {
	mem := &fakeMem{
		written:  map[string]string{}, gists: map[string]string{}, userNote: map[int64]string{},
		profiles: map[int64]string{7: "- Likes tea\n- Lives in Oslo"},
	}
	sum := fakeSum{reply: func(instr string) string {
		if strings.Contains(instr, "Clean up a person's memory profile") {
			t.Fatal("a clean small profile must not be re-compacted (nightly LLM churn)")
		}
		return "none"
	}}

	if err := Run(context.Background(), mem, sum, 14, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestTruncateTranscript(t *testing.T) {
	long := strings.Repeat("line one is here\n", 100)
	got := truncateTranscript(long, 200)
	if !strings.HasPrefix(got, "[transcript truncated") {
		t.Fatalf("expected truncation marker: %q", got[:40])
	}
	if len(got) > 200+64 {
		t.Fatalf("truncated transcript too long: %d", len(got))
	}
	if short := truncateTranscript("hello", 200); short != "hello" {
		t.Fatalf("short transcript must pass through: %q", short)
	}
}

func TestIsNone(t *testing.T) {
	for _, s := range []string{"none", "None", "NONE.", " none.\n", "none!"} {
		if !isNone(s) {
			t.Fatalf("isNone(%q) should be true", s)
		}
	}
	for _, s := range []string{"nonetheless", "- none of the above matters", ""} {
		if isNone(s) {
			t.Fatalf("isNone(%q) should be false", s)
		}
	}
}
