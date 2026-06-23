package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// This file holds the memory file operations used by the daemon-orchestrated nightly
// consolidation (package consolidate). All file I/O lives here in the daemon; the LLM
// only ever receives/returns text.

// PendingRawDays returns day stamps (DD-MM-YY) that have a raw log but no compacted
// daily file yet, excluding today (still in progress). Oldest first.
func (m *Manager) PendingRawDays() []string {
	today := dayStamp(m.Now())
	rawGlob, _ := filepath.Glob(m.path("daily_memory", "_raw", "*.jsonl"))

	var days []string
	for _, p := range rawGlob {
		day := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		if day == today {
			continue
		}
		if _, err := os.Stat(m.path("daily_memory", day+".md")); os.IsNotExist(err) {
			days = append(days, day)
		}
	}
	sortDayStamps(days)
	return days
}

// RawTranscript renders a day's raw log as a readable transcript for summarization.
func (m *Manager) RawTranscript(day string) (string, error) {
	b, err := os.ReadFile(m.path("daily_memory", "_raw", day+".jsonl"))
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for line := range strings.SplitSeq(string(b), "\n") {
		if line == "" {
			continue
		}
		var e rawEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch e.Role {
		case "assistant":
			fmt.Fprintf(&sb, "Assistant: %s\n", e.Text)
		default:
			who := e.User
			if who == "" {
				who = "user"
			}
			scope := "DM"
			if e.IsGroup {
				scope = fmt.Sprintf("group %d", e.ChatID)
			}
			fmt.Fprintf(&sb, "%s (uid %d, %s): %s\n", who, e.UserID, scope, e.Text)
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// WriteDayFile writes the compacted daily note and refreshes its gist in the index.
func (m *Manager) WriteDayFile(day, gist, body string) error {
	if err := os.MkdirAll(m.path("daily_memory"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.path("daily_memory", day+".md"), []byte(strings.TrimSpace(body)+"\n"), 0o644); err != nil {
		return err
	}
	return m.upsertIndexGist(day, gist)
}

// AppendUserNote appends a durable fact about a user to their profile.
func (m *Manager) AppendUserNote(userID int64, note string) error {
	return m.AppendKnowledge(ScopeUser, userID, note)
}

// ReadUserProfile returns a user's stored profile, or "" if they have none yet.
func (m *Manager) ReadUserProfile(userID int64) (string, error) {
	return readFileTrim(m.path("users", userFile(userID))), nil
}

// WriteUserProfile overwrites a user's profile with reconciled content (used by the nightly
// consolidation to fold new facts in coherently instead of appending).
func (m *Manager) WriteUserProfile(userID int64, content string) error {
	if err := os.MkdirAll(m.path("users"), 0o755); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return os.WriteFile(m.path("users", userFile(userID)), []byte(strings.TrimSpace(content)+"\n"), 0o644)
}

// AppendLongTerm appends durable facts to long-term memory.
func (m *Manager) AppendLongTerm(text string) error {
	return m.AppendKnowledge(ScopeLongTerm, 0, text)
}

// AgedDayFiles returns day stamps whose daily file is older than retentionDays.
func (m *Manager) AgedDayFiles(retentionDays int, now time.Time) []string {
	files, _ := filepath.Glob(m.path("daily_memory", "*.md"))
	cutoff := now.AddDate(0, 0, -retentionDays)

	var days []string
	for _, p := range files {
		base := filepath.Base(p)
		if base == "index.md" {
			continue
		}
		day := strings.TrimSuffix(base, ".md")
		t, err := time.ParseInLocation("02-01-06", day, now.Location())
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			days = append(days, day)
		}
	}
	sortDayStamps(days)
	return days
}

// PruneRawLogs deletes raw daily logs older than retentionDays, never touching today's
// (still being written). Consolidation compacts every pending raw day into a dated note
// before this runs (see consolidate.Run), so an aged-out raw log has already been summarized.
// Returns the count removed.
func (m *Manager) PruneRawLogs(retentionDays int, now time.Time) (int, error) {
	files, _ := filepath.Glob(m.path("daily_memory", "_raw", "*.jsonl"))
	cutoff := now.AddDate(0, 0, -retentionDays)
	today := dayStamp(now)

	var removed int
	for _, p := range files {
		day := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		if day == today {
			continue
		}
		t, err := time.ParseInLocation("02-01-06", day, now.Location())
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// ReadDayFile returns a compacted daily note's contents.
func (m *Manager) ReadDayFile(day string) (string, error) {
	b, err := os.ReadFile(m.path("daily_memory", day+".md"))
	return string(b), err
}

// DeleteDayFile removes a daily note and its index line (used after age-out).
func (m *Manager) DeleteDayFile(day string) error {
	if err := os.Remove(m.path("daily_memory", day+".md")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return m.removeIndexGist(day)
}

// --- index.md maintenance ---

func (m *Manager) upsertIndexGist(day, gist string) error {
	row := fmt.Sprintf("| %s | %s |", day, oneLine(gist))
	lines := m.indexLines()
	lines = dropDayRows(lines, day)

	// Insert right after the table separator (| --- | --- |), newest first.
	sep := indexSeparatorIdx(lines)
	if sep >= 0 {
		lines = append(lines[:sep+1], append([]string{row}, lines[sep+1:]...)...)
	} else {
		lines = append(lines, row)
	}
	return m.writeIndex(lines)
}

func (m *Manager) removeIndexGist(day string) error {
	return m.writeIndex(dropDayRows(m.indexLines(), day))
}

func (m *Manager) indexLines() []string {
	b, err := os.ReadFile(m.path("daily_memory", "index.md"))
	if err != nil {
		return []string{indexHeader}
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func (m *Manager) writeIndex(lines []string) error {
	return os.WriteFile(m.path("daily_memory", "index.md"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

const indexHeader = "# Daily memory index"

func indexSeparatorIdx(lines []string) int {
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "| --") {
			return i
		}
	}
	return -1
}

func dropDayRows(lines []string, day string) []string {
	out := lines[:0:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "| "+day+" ") {
			continue
		}
		out = append(out, l)
	}
	return out
}

func sortDayStamps(days []string) {
	sort.Slice(days, func(i, j int) bool {
		ti, _ := time.Parse("02-01-06", days[i])
		tj, _ := time.Parse("02-01-06", days[j])
		return ti.Before(tj)
	})
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	return strings.ReplaceAll(s, "|", "/")
}
