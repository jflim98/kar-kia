// Package consolidate runs the nightly memory consolidation with the daemon doing all
// file I/O and the LLM only summarizing text. This keeps the model free of filesystem
// tools: it never reads or writes files — it just turns a transcript into a summary.
package consolidate

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Summarizer turns an instruction (with embedded text) into a text result, using no
// tools. brain.Brain.Summarize satisfies this.
type Summarizer interface {
	Summarize(ctx context.Context, instruction string) (string, error)
}

// MemoryOps is the file-side of consolidation, implemented by memory.Manager.
type MemoryOps interface {
	PendingRawDays() []string
	RawTranscript(day string) (string, error)
	DeleteRawLog(day string) error
	WriteDayFile(day, gist, body string) error
	AppendUserNote(userID int64, note string) error
	ReadUserProfile(userID int64) (string, error)
	WriteUserProfileIf(userID int64, content, expected string) (bool, error)
	ListUserProfiles() ([]int64, error)
	AgedDayFiles(retentionDays int, now time.Time) []string
	ReadDayFile(day string) (string, error)
	ReadLongTerm() (string, error)
	AppendLongTerm(text string) error
	WriteLongTermIf(content, expected string) (bool, error)
	DeleteDayFile(day string) error
	PruneRawLogs(retentionDays int, now time.Time) (int, error)
}

const (
	// maxTranscriptBytes bounds a day transcript fed to the summarizer, so one very busy
	// day can't blow the consolidation model's context window or budget (a failure that
	// would repeat every night and leave the day pending forever).
	maxTranscriptBytes = 200_000
	// profileCompactBytes / longTermCompactBytes are the size thresholds past which the
	// nightly compaction rewrites a user profile / long-term memory. Both files are
	// injected into prompts (long-term on every turn), so size is token cost.
	profileCompactBytes  = 2048
	longTermCompactBytes = 4096
)

// Run performs the nightly consolidation: compact each pending raw day into a dated
// note (+ index gist + per-user notes), age out notes older than retentionDays into
// long-term memory, drop raw logs past rawRetentionDays (only if summarized), then
// compact user profiles carrying un-reconciled appends and an oversized long-term memory.
func Run(ctx context.Context, mem MemoryOps, sum Summarizer, retentionDays, rawRetentionDays int, now time.Time) error {
	// 1. Episodic save: raw day logs -> dated notes.
	for _, day := range mem.PendingRawDays() {
		transcript, err := mem.RawTranscript(day)
		if err != nil {
			log.Printf("consolidate: read raw %s: %v", day, err)
			continue
		}
		if strings.TrimSpace(transcript) == "" {
			// Nothing to summarize; drop the raw file so the day doesn't stay pending forever.
			if err := mem.DeleteRawLog(day); err != nil {
				log.Printf("consolidate: delete empty raw %s: %v", day, err)
			}
			continue
		}
		transcript = truncateTranscript(transcript, maxTranscriptBytes)
		out, err := sum.Summarize(ctx, dayPrompt(day, transcript))
		if err != nil {
			log.Printf("consolidate: summarize %s: %v", day, err)
			continue
		}
		gist, body, userNotes := parseDayOutput(out)
		if err := mem.WriteDayFile(day, gist, body); err != nil {
			log.Printf("consolidate: write %s: %v", day, err)
			continue
		}
		for uid, note := range userNotes {
			reconcileUser(ctx, mem, sum, uid, note)
		}
		log.Printf("consolidate: saved daily note %s", day)
	}

	// 2. Age-out: notes older than the retention window -> long-term memory.
	for _, day := range mem.AgedDayFiles(retentionDays, now) {
		content, err := mem.ReadDayFile(day)
		if err != nil {
			log.Printf("consolidate: read note %s: %v", day, err)
			continue
		}
		facts, err := sum.Summarize(ctx, ageOutPrompt(day, content))
		if err != nil {
			log.Printf("consolidate: age-out %s: %v", day, err)
			continue
		}
		if f := strings.TrimSpace(facts); f != "" && !isNone(f) {
			if err := mem.AppendLongTerm(f); err != nil {
				// The extracted facts aren't saved yet — keep the note and retry next night.
				log.Printf("consolidate: append long-term for %s: %v; keeping note", day, err)
				continue
			}
		}
		if err := mem.DeleteDayFile(day); err != nil {
			log.Printf("consolidate: delete note %s: %v", day, err)
		}
		log.Printf("consolidate: aged out %s into long-term", day)
	}

	// 3. Drop raw logs past their retention window (only ones compacted into a dated note
	// in step 1 — PruneRawLogs keeps a never-summarized day rather than destroy its only copy).
	if n, err := mem.PruneRawLogs(rawRetentionDays, now); err != nil {
		log.Printf("consolidate: prune raw logs: %v", err)
	} else if n > 0 {
		log.Printf("consolidate: pruned %d raw logs", n)
	}

	// 4. Compact user profiles that reconcileUser didn't cover: mid-day propose_memory saves
	// land as raw dated appends, and are only folded in when that user appears in the day's
	// USER_NOTES — this sweep catches the rest (plus any profile past the size threshold).
	compactProfiles(ctx, mem, sum)

	// 5. Compact long-term memory once it outgrows its threshold. It is otherwise append-only
	// (propose_memory + the age-out above) and rides in every turn's system prompt.
	compactLongTerm(ctx, mem, sum)
	return nil
}

// compactProfiles rewrites profiles that carry un-reconciled dated appends or exceed the
// size threshold. Guarded like reconcileUser: implausible LLM output or a concurrent write
// leaves the file alone (retried next night).
func compactProfiles(ctx context.Context, mem MemoryOps, sum Summarizer) {
	uids, err := mem.ListUserProfiles()
	if err != nil {
		log.Printf("consolidate: list profiles: %v", err)
		return
	}
	for _, uid := range uids {
		content, err := mem.ReadUserProfile(uid)
		if err != nil || strings.TrimSpace(content) == "" {
			continue
		}
		if !hasDatedAppends(content) && len(content) <= profileCompactBytes {
			continue
		}
		compacted, err := sum.Summarize(ctx, compactProfilePrompt(content))
		if err != nil {
			log.Printf("consolidate: compact profile %d: %v", uid, err)
			continue
		}
		compacted = strings.TrimSpace(compacted)
		if !plausibleCompaction(compacted) {
			log.Printf("consolidate: compact profile %d: implausible rewrite (%d -> %d bytes); keeping file", uid, len(content), len(compacted))
			continue
		}
		if ok, err := mem.WriteUserProfileIf(uid, compacted, content); err != nil {
			log.Printf("consolidate: write compacted profile %d: %v", uid, err)
		} else if !ok {
			log.Printf("consolidate: profile %d changed mid-compaction; skipping", uid)
		} else {
			log.Printf("consolidate: compacted profile %d (%d -> %d bytes)", uid, len(content), len(compacted))
		}
	}
}

// compactLongTerm dedupes and rewrites long-term memory when it exceeds the threshold. The
// write is conditional on the file being unchanged since the read (a live propose_memory
// append during the summarize step must not be clobbered), and the memory layer keeps the
// prior content as a .bak — by compaction time the raw sources are pruned, so the rewrite
// is the only copy.
func compactLongTerm(ctx context.Context, mem MemoryOps, sum Summarizer) {
	existing, err := mem.ReadLongTerm()
	if err != nil || len(existing) <= longTermCompactBytes {
		return
	}
	compacted, err := sum.Summarize(ctx, compactLongTermPrompt(existing))
	if err != nil {
		log.Printf("consolidate: compact long-term: %v", err)
		return
	}
	compacted = strings.TrimSpace(compacted)
	if !plausibleCompaction(compacted) || len(compacted) >= len(existing) {
		log.Printf("consolidate: compact long-term: unusable rewrite (%d -> %d bytes); keeping file", len(existing), len(compacted))
		return
	}
	if ok, err := mem.WriteLongTermIf(compacted, existing); err != nil {
		log.Printf("consolidate: write compacted long-term: %v", err)
	} else if !ok {
		log.Printf("consolidate: long-term memory changed mid-compaction; skipping")
	} else {
		log.Printf("consolidate: compacted long-term memory (%d -> %d bytes)", len(existing), len(compacted))
	}
}

// reconcileUser merges the day's new fact(s) about a user into their profile, rewriting it
// into a coherent, deduped whole rather than appending. If reading the existing profile or the
// summarize step fails, if the LLM output looks implausible, or if the profile changed during
// the slow summarize step (e.g. a live propose_memory save), it falls back to a plain append
// so a fact is never lost or clobbered.
func reconcileUser(ctx context.Context, mem MemoryOps, sum Summarizer, uid int64, note string) {
	existing, err := mem.ReadUserProfile(uid)
	if err != nil {
		log.Printf("consolidate: read profile %d: %v", uid, err)
		appendUserNote(mem, uid, note)
		return
	}
	if strings.TrimSpace(existing) == "" {
		// Nothing to reconcile against yet — just record the new fact(s).
		appendUserNote(mem, uid, note)
		return
	}
	merged, err := sum.Summarize(ctx, reconcilePrompt(existing, note))
	if err != nil {
		log.Printf("consolidate: reconcile profile %d: %v", uid, err)
		appendUserNote(mem, uid, note)
		return
	}
	merged = strings.TrimSpace(merged)
	if !plausibleRewrite(existing, merged) {
		log.Printf("consolidate: reconcile profile %d: implausible rewrite (%d -> %d bytes); appending instead", uid, len(existing), len(merged))
		appendUserNote(mem, uid, note)
		return
	}
	ok, err := mem.WriteUserProfileIf(uid, merged, existing)
	if err != nil {
		log.Printf("consolidate: write profile %d: %v", uid, err)
		appendUserNote(mem, uid, note)
		return
	}
	if !ok {
		log.Printf("consolidate: profile %d changed mid-reconcile; appending instead", uid)
		appendUserNote(mem, uid, note)
		return
	}
	log.Printf("consolidate: reconciled profile %d", uid)
}

// plausibleRewrite sanity-checks a RECONCILE result (new facts merged into an existing
// profile): non-empty, not a "none" reply, and — when the existing file is substantial —
// not so short that it smells like a refusal or truncated reply that would wipe accumulated
// facts. Merging facts IN never legitimately shrinks a profile fourfold.
func plausibleRewrite(existing, rewritten string) bool {
	if rewritten == "" || isNone(rewritten) {
		return false
	}
	if len(existing) >= 400 && len(rewritten) < len(existing)/4 {
		return false
	}
	return true
}

// plausibleCompaction sanity-checks a COMPACTION result. Deduping genuinely redundant
// content can legitimately shrink it many-fold, so no length floor — instead the output
// must have the bullet-list shape the prompt demands, which a refusal or preamble doesn't.
func plausibleCompaction(rewritten string) bool {
	if rewritten == "" || isNone(rewritten) {
		return false
	}
	return strings.HasPrefix(rewritten, "- ") || strings.HasPrefix(rewritten, "* ")
}

// isNone matches the "nothing durable" replies the prompts ask for, tolerating case and
// trailing punctuation ("None.") so they never get written into memory as facts.
func isNone(s string) bool {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(s)), ".!") == "none"
}

// truncateTranscript keeps the most recent max bytes of a transcript, cut at a line boundary.
func truncateTranscript(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[len(s)-max:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:]
	}
	return "[transcript truncated: earliest messages omitted]\n" + cut
}

// datedAppendRe matches the raw "- (YYYY-MM-DD) fact" lines AppendKnowledge writes; their
// presence in a profile means saves that no reconcile pass has folded in yet.
var datedAppendRe = regexp.MustCompile(`(?m)^- \(\d{4}-\d{2}-\d{2}\)`)

func hasDatedAppends(s string) bool { return datedAppendRe.MatchString(s) }

func appendUserNote(mem MemoryOps, uid int64, note string) {
	if err := mem.AppendUserNote(uid, note); err != nil {
		log.Printf("consolidate: user note %d: %v", uid, err)
	}
}

func dayPrompt(day, transcript string) string {
	return fmt.Sprintf(`Consolidate one day of chat history into a memory note. Date: %s.
Be concise and factual; never invent anything. Respond EXACTLY in this format:

GIST: <one sentence describing the day, for an index>
SUMMARY:
<a short markdown summary of what mattered: decisions, facts, tasks, notable exchanges>
USER_NOTES:
<durable facts worth remembering about specific people. Capture preferences, identity details,
ongoing projects, relationships, and commitments — include facts that are merely IMPLIED by how
someone spoke or what they did, not only ones they explicitly asked you to remember. Skip
one-off trivia and anything fleeting. One line per fact, "<user_id>: <fact>" (a user may have
several lines). Write "none" only if nothing durable came up.>

Transcript:
%s`, day, transcript)
}

func ageOutPrompt(day, content string) string {
	return fmt.Sprintf(`This is an old daily memory note (%s) being archived. Extract only the
durable, long-term-worthy facts (preferences, identities, ongoing projects, commitments)
as a short markdown bullet list. If nothing is durable, reply exactly "none".

Note:
%s`, day, content)
}

func reconcilePrompt(existing, newFacts string) string {
	return fmt.Sprintf(`Update a person's memory profile by folding in newly learned facts.
Return ONLY the updated profile as a concise markdown bullet list — no preamble, no headings.
Rules: merge the new facts into the existing ones; remove duplicates and near-duplicates; when a
new fact contradicts or updates an old one, keep the newer version and drop the stale one; drop
trivia that isn't durable; keep it coherent and compact. Preserve everything still true.

Existing profile:
%s

Newly learned:
%s`, existing, newFacts)
}

func compactProfilePrompt(profile string) string {
	return fmt.Sprintf(`Clean up a person's memory profile. Return ONLY the rewritten profile as a
concise markdown bullet list — no preamble, no headings. Rules: merge duplicates and
near-duplicates; when entries contradict, keep the later-dated one (entries may carry a
"(YYYY-MM-DD)" prefix — drop those prefixes in the output, keeping a date only when the date
itself matters to the fact); drop trivia that isn't durable. Preserve every fact still true.
Never invent anything.

Profile:
%s`, profile)
}

func compactLongTermPrompt(content string) string {
	return fmt.Sprintf(`Compact this long-term memory file. Return ONLY the rewritten memory as a
concise markdown bullet list — no preamble, no headings. Rules: merge duplicates and
near-duplicates; when entries contradict, keep the later-dated one (entries carry a
"(YYYY-MM-DD)" prefix — preserve the newest date prefix on each merged fact); drop fleeting
trivia. Preserve every durable fact still true. Never invent anything.

Memory:
%s`, content)
}

// parseDayOutput splits the model's structured reply into a gist, a body, and per-user
// notes. It degrades gracefully if the markers are missing.
func parseDayOutput(out string) (gist, body string, userNotes map[int64]string) {
	userNotes = map[int64]string{}
	out = strings.TrimSpace(out)

	gist = firstLine(out)
	body = out

	if i := strings.Index(out, "GIST:"); i >= 0 {
		rest := out[i+len("GIST:"):]
		gist = strings.TrimSpace(firstLine(rest))
	}
	if i := strings.Index(out, "SUMMARY:"); i >= 0 {
		seg := out[i+len("SUMMARY:"):]
		if j := strings.Index(seg, "USER_NOTES:"); j >= 0 {
			body = strings.TrimSpace(seg[:j])
			parseUserNotes(seg[j+len("USER_NOTES:"):], userNotes)
		} else {
			body = strings.TrimSpace(seg)
		}
	}
	if strings.TrimSpace(body) == "" {
		body = out
	}
	return gist, body, userNotes
}

func parseUserNotes(s string, into map[int64]string) {
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		if line == "" || isNone(line) {
			continue
		}
		id, note, ok := strings.Cut(line, ":")
		if !ok {
			log.Printf("consolidate: dropping malformed user note line %q", line)
			continue
		}
		uid, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
		if err != nil {
			log.Printf("consolidate: dropping user note with non-numeric id %q", line)
			continue
		}
		if n := strings.TrimSpace(note); n != "" {
			if prev := into[uid]; prev != "" {
				into[uid] = prev + "\n" + n
			} else {
				into[uid] = n
			}
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
