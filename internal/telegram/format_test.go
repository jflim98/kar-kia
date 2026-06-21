package telegram

import (
	"strings"
	"testing"
)

func TestSplitSourceShortIsSingle(t *testing.T) {
	chunks := splitSource("just a short **message**")
	if len(chunks) != 1 {
		t.Fatalf("short message should be one chunk, got %d", len(chunks))
	}
}

func TestSplitSourceLongStaysUnderLimit(t *testing.T) {
	// ~200 paragraphs of ~60 chars => well over 4096.
	var b strings.Builder
	for range 200 {
		b.WriteString("This is paragraph number with some **bold** text here.\n\n")
	}
	chunks := splitSource(b.String())
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := utf16Len(toTelegramHTML(c)); n > telegramMaxLen {
			t.Fatalf("chunk %d converts to %d > %d", i, n, telegramMaxLen)
		}
		// Each chunk must be balanced HTML (independent conversion guarantees this).
		html := toTelegramHTML(c)
		if strings.Count(html, "<b>") != strings.Count(html, "</b>") {
			t.Fatalf("chunk %d has unbalanced <b> tags", i)
		}
	}
}

func TestSplitSourceHardSplitsHugeLine(t *testing.T) {
	huge := strings.Repeat("x", 10000) // single line, no breaks
	chunks := splitSource(huge)
	if len(chunks) < 2 {
		t.Fatalf("a 10k-char line must hard-split, got %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if n := utf16Len(c); n > telegramMaxLen {
			t.Fatalf("hard-split chunk %d is %d runes > %d", i, n, telegramMaxLen)
		}
	}
}

func TestToTelegramHTML(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"bold", "hello **world**", "hello <b>world</b>"},
		{"bold underscores", "a __b__ c", "a <b>b</b> c"},
		{"italic star", "an *important* note", "an <i>important</i> note"},
		{"italic underscore", "an _important_ note", "an <i>important</i> note"},
		{"snake_case not italic", "call do_the_thing now", "call do_the_thing now"},
		{"strike", "~~gone~~", "<s>gone</s>"},
		{"inline code", "use `go build` here", "use <code>go build</code> here"},
		{"link", "see [docs](https://x.io)", `see <a href="https://x.io">docs</a>`},
		{"heading", "## Title", "<b>Title</b>"},
		{"bullets", "- one\n- two", "• one\n• two"},
		{"escape", "1 < 2 & 3 > 0", "1 &lt; 2 &amp; 3 &gt; 0"},
		{"no markdown inside code", "`a*b*c`", "<code>a*b*c</code>"},
		{"bold not italicized", "**strong**", "<b>strong</b>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toTelegramHTML(c.in); got != c.want {
				t.Fatalf("toTelegramHTML(%q)\n  got:  %q\n  want: %q", c.in, got, c.want)
			}
		})
	}
}

func TestFencedCodeBlock(t *testing.T) {
	in := "before\n```go\nfmt.Println(\"<hi>\")\n```\nafter"
	got := toTelegramHTML(in)
	want := "before\n<pre><code class=\"language-go\">fmt.Println(&quot;&lt;hi&gt;&quot;)</code></pre>\nafter"
	// We don't escape quotes, so adjust expectation: only < > & are escaped.
	want = "before\n<pre><code class=\"language-go\">fmt.Println(\"&lt;hi&gt;\")</code></pre>\nafter"
	if got != want {
		t.Fatalf("fenced block:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestCodeContentNotFormatted(t *testing.T) {
	// Markdown markers and HTML inside code must be preserved/escaped, not transformed.
	got := toTelegramHTML("```\n**not bold** <tag> & x\n```")
	want := "<pre>**not bold** &lt;tag&gt; &amp; x</pre>"
	if got != want {
		t.Fatalf("code content:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestBlockquote(t *testing.T) {
	got := toTelegramHTML("> line one\n> line two")
	want := "<blockquote>line one\nline two</blockquote>"
	if got != want {
		t.Fatalf("blockquote:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestHeadingWithEscaping(t *testing.T) {
	// A heading whose text contains an angle bracket must be escaped and bolded.
	got := toTelegramHTML("# A > B")
	want := "<b>A &gt; B</b>"
	if got != want {
		t.Fatalf("heading:\n  got:  %q\n  want: %q", got, want)
	}
}
