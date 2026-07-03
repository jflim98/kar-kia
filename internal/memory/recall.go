package memory

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Recall is the daemon-side memory retrieval behind the recall_memory MCP tool. The model
// has no filesystem access, so it asks via the tool and the daemon does the lookup, confined
// to THIS chat's memory. Two modes:
//
//   - date (DD-MM-YY): return that day's full compacted note.
//   - query: a lexical search (BM25-style, with light stemming and typo tolerance) across the
//     day notes, long-term memory, and user profiles; returns the top source-tagged snippets.
//
// limit defaults to 6 (clamped to 1..20). Read-only; never escapes this chat's dir.
func (m *Manager) Recall(query, date string, limit int) (string, error) {
	if date = strings.TrimSpace(date); date != "" {
		return m.recallDay(date)
	}
	if query = strings.TrimSpace(query); query == "" {
		return "", fmt.Errorf("provide a query or a date (DD-MM-YY)")
	}
	switch {
	case limit <= 0:
		limit = 6
	case limit > 20:
		limit = 20
	}
	return m.recallQuery(query, limit), nil
}

func (m *Manager) recallDay(date string) (string, error) {
	if _, err := time.ParseInLocation("02-01-06", date, m.tz); err != nil {
		return "", fmt.Errorf("date must be DD-MM-YY, got %q", date)
	}
	b, err := os.ReadFile(m.path("daily_memory", date+".md"))
	if err != nil {
		if os.IsNotExist(err) {
			return "No stored note for " + date + ".", nil
		}
		return "", err
	}
	return fmt.Sprintf("[%s]\n%s", date, strings.TrimSpace(string(b))), nil
}

// chunk is one searchable unit (a paragraph or bullet) with its source tag and stemmed tokens.
type chunk struct {
	source string
	text   string
	tokens []string
}

func (m *Manager) recallQuery(query string, limit int) string {
	chunks := m.gatherChunks()
	qterms := dedupe(tokenize(query))
	if len(chunks) == 0 || len(qterms) == 0 {
		return "No matching memory."
	}

	// Corpus stats for BM25 (tiny corpus, so a full scan per query is fine).
	n := len(chunks)
	totalLen := 0
	for _, c := range chunks {
		totalLen += len(c.tokens)
	}
	avg := float64(totalLen) / float64(n)

	df := make(map[string]int, len(qterms))
	for _, q := range qterms {
		for i := range chunks {
			if termFreq(q, chunks[i].tokens) > 0 {
				df[q]++
			}
		}
	}

	const k1, b = 1.5, 0.75
	type scored struct {
		c     chunk
		score float64
	}
	var ranked []scored
	for _, c := range chunks {
		var score float64
		dl := float64(len(c.tokens))
		for _, q := range qterms {
			tf := float64(termFreq(q, c.tokens))
			if tf == 0 {
				continue
			}
			nq := float64(df[q])
			idf := math.Log(1 + (float64(n)-nq+0.5)/(nq+0.5))
			score += idf * (tf * (k1 + 1)) / (tf + k1*(1-b+b*dl/avg))
		}
		if score > 0 {
			ranked = append(ranked, scored{c, score})
		}
	}
	if len(ranked) == 0 {
		return "No matching memory."
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	// Emit top hits under a rough token budget so a recall can't blow up the context.
	const budget = 6000 // ~1500 tokens
	var sb strings.Builder
	used, shown := 0, 0
	for _, r := range ranked {
		if shown >= limit {
			break
		}
		entry := r.c.source + " " + clip(r.c.text, 600)
		if shown > 0 && used+len(entry) > budget {
			break
		}
		if shown > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(entry)
		used += len(entry)
		shown++
	}
	return sb.String()
}

// gatherChunks reads every searchable memory file for this chat and splits it into chunks.
func (m *Manager) gatherChunks() []chunk {
	var chunks []chunk
	add := func(source, text string) {
		if text = strings.TrimSpace(text); text != "" {
			chunks = append(chunks, chunk{source: source, text: text, tokens: tokenize(text)})
		}
	}

	// Daily notes (skip the index — it's already always loaded).
	files, _ := filepath.Glob(m.path("daily_memory", "*.md"))
	sort.Strings(files)
	for _, p := range files {
		base := filepath.Base(p)
		if base == "index.md" {
			continue
		}
		day := strings.TrimSuffix(base, ".md")
		if b, err := os.ReadFile(p); err == nil {
			for _, blk := range splitBlocks(string(b)) {
				add("["+day+"]", blk)
			}
		}
	}
	// Long-term memory.
	if b, err := os.ReadFile(m.path("long_term_memory.md")); err == nil {
		for _, blk := range splitBlocks(string(b)) {
			add("[long-term]", blk)
		}
	}
	// Per-user profiles.
	profs, _ := filepath.Glob(m.path("users", "*.md"))
	sort.Strings(profs)
	for _, p := range profs {
		uid := strings.TrimSuffix(filepath.Base(p), ".md")
		if b, err := os.ReadFile(p); err == nil {
			for _, blk := range splitBlocks(string(b)) {
				add("[about user "+uid+"]", blk)
			}
		}
	}
	return chunks
}

// splitBlocks splits text into searchable chunks: paragraphs by blank lines, but a bullet
// list is split per bullet so a long list doesn't collapse into one coarse chunk.
func splitBlocks(s string) []string {
	var out []string
	for para := range strings.SplitSeq(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n") {
		if para = strings.TrimSpace(para); para == "" {
			continue
		}
		lines := strings.Split(para, "\n")
		if isBulletList(lines) {
			for _, ln := range lines {
				if ln = strings.TrimSpace(ln); ln != "" {
					out = append(out, ln)
				}
			}
			continue
		}
		out = append(out, para)
	}
	return out
}

func isBulletList(lines []string) bool {
	n := 0
	for _, ln := range lines {
		if ln = strings.TrimSpace(ln); ln == "" {
			continue
		}
		if !strings.HasPrefix(ln, "- ") && !strings.HasPrefix(ln, "* ") {
			return false
		}
		n++
	}
	return n >= 2
}

// tokenize lowercases, drops punctuation and stopwords, and stems each token.
func tokenize(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if len(f) < 2 || stopwords[f] {
			continue
		}
		out = append(out, stem(f))
	}
	return out
}

// stem does conservative English suffix stripping (plurals, -ing/-ed/-ly). It is intentionally
// light — enough to fold common inflections, not a full Porter stemmer.
func stem(w string) string {
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		w = w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "sses"):
		w = w[:len(w)-2]
	case strings.HasSuffix(w, "es") && len(w) > 3:
		w = w[:len(w)-2]
	case strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && len(w) > 3:
		w = w[:len(w)-1]
	}
	switch {
	case strings.HasSuffix(w, "ing") && len(w) > 5:
		return w[:len(w)-3]
	case strings.HasSuffix(w, "edly") && len(w) > 6:
		return w[:len(w)-4]
	case strings.HasSuffix(w, "ed") && len(w) > 4:
		return w[:len(w)-2]
	case strings.HasSuffix(w, "ly") && len(w) > 4:
		return w[:len(w)-2]
	}
	return w
}

// termFreq counts tokens matching the query term (exact stem, or within edit distance 1 for
// longer words to tolerate typos).
func termFreq(qterm string, tokens []string) int {
	n := 0
	for _, t := range tokens {
		if termMatch(qterm, t) {
			n++
		}
	}
	return n
}

func termMatch(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) >= 4 && len(b) >= 4 && abs(len(a)-len(b)) <= 1 {
		return osaDistance(a, b) <= 1
	}
	return false
}

// osaDistance is the optimal string alignment distance: Levenshtein plus adjacent
// transpositions counted as a single edit (so "budgte"->"budget" is distance 1), which
// catches the most common typo class. Words are short, so a full matrix is cheap.
func osaDistance(a, b string) int {
	la, lb := len(a), len(b)
	d := make([][]int, la+1)
	for i := range d {
		d[i] = make([]int, lb+1)
		d[i][0] = i
	}
	for j := 0; j <= lb; j++ {
		d[0][j] = j
	}
	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, min(d[i][j-1]+1, d[i-1][j-1]+cost))
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[la][lb]
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// clip truncates s to at most n bytes, backing up to a rune boundary so it never emits
// invalid UTF-8 (clipped text flows into prompts and Telegram messages).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n]) + "…"
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// stopwords are common words too frequent to be useful as search terms.
var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true, "of": true,
	"to": true, "in": true, "on": true, "at": true, "for": true, "with": true, "is": true,
	"are": true, "was": true, "were": true, "be": true, "been": true, "it": true, "this": true,
	"that": true, "as": true, "by": true, "from": true, "do": true, "did": true, "what": true,
	"when": true, "where": true, "who": true, "how": true, "i": true, "you": true, "we": true,
	"my": true, "me": true, "about": true, "had": true, "has": true, "have": true,
}
