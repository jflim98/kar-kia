// Package consolidate runs the nightly memory consolidation with the daemon doing all
// file I/O and the LLM only summarizing text. This keeps the model free of filesystem
// tools: it never reads or writes files — it just turns a transcript into a summary.
package consolidate

import (
	"context"
	"fmt"
	"log"
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
	WriteDayFile(day, gist, body string) error
	AppendUserNote(userID int64, note string) error
	AgedDayFiles(retentionDays int, now time.Time) []string
	ReadDayFile(day string) (string, error)
	AppendLongTerm(text string) error
	DeleteDayFile(day string) error
	PruneRawLogs(retentionDays int, now time.Time) (int, error)
}

// Run performs the nightly consolidation: compact each pending raw day into a dated
// note (+ index gist + per-user notes), age out notes older than retentionDays into
// long-term memory, then drop raw logs past rawRetentionDays (already summarized in step 1).
func Run(ctx context.Context, mem MemoryOps, sum Summarizer, retentionDays, rawRetentionDays int, now time.Time) error {
	// 1. Episodic save: raw day logs -> dated notes.
	for _, day := range mem.PendingRawDays() {
		transcript, err := mem.RawTranscript(day)
		if err != nil {
			log.Printf("consolidate: read raw %s: %v", day, err)
			continue
		}
		if strings.TrimSpace(transcript) == "" {
			continue
		}
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
			if err := mem.AppendUserNote(uid, note); err != nil {
				log.Printf("consolidate: user note %d: %v", uid, err)
			}
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
		if f := strings.TrimSpace(facts); f != "" && !strings.EqualFold(f, "none") {
			if err := mem.AppendLongTerm(f); err != nil {
				log.Printf("consolidate: append long-term: %v", err)
			}
		}
		if err := mem.DeleteDayFile(day); err != nil {
			log.Printf("consolidate: delete note %s: %v", day, err)
		}
		log.Printf("consolidate: aged out %s into long-term", day)
	}

	// 3. Drop raw logs past their retention window. They were compacted into dated notes
	// in step 1, so this only reclaims the redundant verbatim transcripts.
	if n, err := mem.PruneRawLogs(rawRetentionDays, now); err != nil {
		log.Printf("consolidate: prune raw logs: %v", err)
	} else if n > 0 {
		log.Printf("consolidate: pruned %d raw logs", n)
	}
	return nil
}

func dayPrompt(day, transcript string) string {
	return fmt.Sprintf(`Consolidate one day of chat history into a memory note. Date: %s.
Be concise and factual; never invent anything. Respond EXACTLY in this format:

GIST: <one sentence describing the day, for an index>
SUMMARY:
<a short markdown summary of what mattered: decisions, facts, tasks, notable exchanges>
USER_NOTES:
<for each user with a durable fact worth remembering long-term, one line "<user_id>: <fact>"; write "none" if there are none>

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
		if line == "" || strings.EqualFold(line, "none") {
			continue
		}
		id, note, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		uid, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
		if err != nil {
			continue
		}
		if n := strings.TrimSpace(note); n != "" {
			into[uid] = n
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
