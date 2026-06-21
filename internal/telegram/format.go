package telegram

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"
)

// telegramMaxLen is Telegram's per-message text limit, counted in UTF-16 code units.
const telegramMaxLen = 4096

// utf16Len counts a string's length the way Telegram does (UTF-16 code units), so
// emoji and other astral-plane characters are measured correctly.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// Claude replies in CommonMark, but Telegram doesn't render Markdown — it wants its own
// "HTML style" (parse_mode=HTML). toTelegramHTML converts the common Markdown Claude
// produces into the subset of HTML Telegram supports:
//
//	<b> <i> <s> <code> <pre> <a href> <blockquote>  (+ <pre><code class="language-x">)
//
// Telegram does NOT support <h1>/<ul>/<li>/<p>, so headings become bold lines and
// bullets become "• ". Only < > & are escaped. If our output is ever malformed,
// the gateway falls back to sending the original text un-parsed, so delivery is safe.

var (
	reFenced  = regexp.MustCompile("(?s)```([a-zA-Z0-9_+#.-]*)\n?(.*?)```")
	reInline  = regexp.MustCompile("`([^`\n]+)`")
	reBoldA   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reBoldU   = regexp.MustCompile(`__(.+?)__`)
	reStrike  = regexp.MustCompile(`~~(.+?)~~`)
	reItalicU = regexp.MustCompile(`(^|[^\w])_([^_\n]+?)_($|[^\w])`)
	reItalicA = regexp.MustCompile(`\*([^*\n]+?)\*`)
	reLink    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	reHeading = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*\S)\s*$`)
	reBullet  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
)

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// toTelegramHTML converts Markdown to Telegram-flavored HTML.
func toTelegramHTML(md string) string {
	md = strings.ReplaceAll(md, "\r\n", "\n")

	// 1. Pull code out first (it must not be touched by other transforms), render it,
	//    and leave a placeholder. Fenced blocks before inline spans.
	var codes []string
	store := func(html string) string {
		codes = append(codes, html)
		return fmt.Sprintf("\x00%d\x00", len(codes)-1)
	}
	md = reFenced.ReplaceAllStringFunc(md, func(m string) string {
		sub := reFenced.FindStringSubmatch(m)
		body := htmlEscape(strings.Trim(sub[2], "\n"))
		if lang := sub[1]; lang != "" {
			return store(fmt.Sprintf("<pre><code class=\"language-%s\">%s</code></pre>", lang, body))
		}
		return store("<pre>" + body + "</pre>")
	})
	md = reInline.ReplaceAllStringFunc(md, func(m string) string {
		return store("<code>" + htmlEscape(reInline.FindStringSubmatch(m)[1]) + "</code>")
	})

	// 2. Escape everything else (placeholders are \x00N\x00 — safe).
	md = htmlEscape(md)

	// 3. Block-level, line by line: headings, blockquotes, bullets, rules.
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	var quote []string
	flushQuote := func() {
		if len(quote) > 0 {
			out = append(out, "<blockquote>"+strings.Join(quote, "\n")+"</blockquote>")
			quote = nil
		}
	}
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		switch {
		case isHRule(trimmed):
			flushQuote()
			out = append(out, "──────────")
		case strings.HasPrefix(trimmed, "&gt;"): // a Markdown "> quote" line (> was escaped)
			quote = append(quote, strings.TrimSpace(strings.TrimPrefix(trimmed, "&gt;")))
		default:
			flushQuote()
			if mm := reHeading.FindStringSubmatch(ln); mm != nil {
				out = append(out, "<b>"+mm[1]+"</b>")
			} else if mm := reBullet.FindStringSubmatch(ln); mm != nil {
				out = append(out, mm[1]+"• "+mm[2])
			} else {
				out = append(out, ln)
			}
		}
	}
	flushQuote()
	md = strings.Join(out, "\n")

	// 4. Inline spans (bold before italic so ** isn't eaten by single-* italic).
	md = reBoldA.ReplaceAllString(md, "<b>$1</b>")
	md = reBoldU.ReplaceAllString(md, "<b>$1</b>")
	md = reStrike.ReplaceAllString(md, "<s>$1</s>")
	md = reItalicU.ReplaceAllString(md, "$1<i>$2</i>$3")
	md = reItalicA.ReplaceAllString(md, "<i>$1</i>")
	md = reLink.ReplaceAllString(md, `<a href="$2">$1</a>`)

	// 5. Restore code placeholders.
	for i, c := range codes {
		md = strings.ReplaceAll(md, fmt.Sprintf("\x00%d\x00", i), c)
	}
	return strings.TrimSpace(md)
}

// splitSource breaks Markdown into source chunks such that each chunk's converted HTML
// fits Telegram's length limit. Splitting the SOURCE (not the HTML) and converting each
// piece independently guarantees every chunk is self-contained, balanced HTML. It breaks
// at line boundaries, and hard-splits a single over-long line by runes as a last resort.
func splitSource(md string) []string {
	if utf16Len(toTelegramHTML(md)) <= telegramMaxLen {
		return []string{md}
	}

	var chunks []string
	cur := ""
	add := func(line string) {
		if cur == "" {
			cur = line
		} else {
			cur += "\n" + line
		}
	}
	flush := func() {
		if cur != "" {
			chunks = append(chunks, cur)
			cur = ""
		}
	}

	for line := range strings.SplitSeq(md, "\n") {
		candidate := line
		if cur != "" {
			candidate = cur + "\n" + line
		}
		if utf16Len(toTelegramHTML(candidate)) <= telegramMaxLen {
			cur = candidate
			continue
		}
		// Adding this line overflows: flush what we have, then place the line.
		flush()
		if utf16Len(toTelegramHTML(line)) > telegramMaxLen {
			// A single line is itself too long — hard-split it. A too-large HTML chunk
			// still falls back to plain text at send time, so a rune budget is safe.
			for _, piece := range hardSplitRunes(line, telegramMaxLen-200) {
				chunks = append(chunks, piece)
			}
			continue
		}
		add(line)
	}
	flush()
	return chunks
}

func hardSplitRunes(s string, budget int) []string {
	r := []rune(s)
	var out []string
	for len(r) > budget {
		out = append(out, string(r[:budget]))
		r = r[budget:]
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}

// isHRule reports whether a trimmed line is a Markdown horizontal rule (---, ***, ___).
func isHRule(s string) bool {
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != c {
			return false
		}
	}
	return true
}
