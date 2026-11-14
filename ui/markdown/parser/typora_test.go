package parser

import (
	"strings"
	"testing"
)

func TestTyporaEmphasis(t *testing.T) {
	for _, c := range []struct {
		src, want string
	}{
		{"中文**“加粗带引号”**中文", "<p>中文<strong>“加粗带引号”</strong>中文</p>\n"},
		{"中文**（粗体）**中文", "<p>中文<strong>（粗体）</strong>中文</p>\n"},
		{"**加粗：**正文", "<p><strong>加粗：</strong>正文</p>\n"},
		{"**加粗:**正文", "<p><strong>加粗:</strong>正文</p>\n"},
		{"中文*“斜体”*中文", "<p>中文<em>“斜体”</em>中文</p>\n"},
		{"中文**加粗**中文", "<p>中文<strong>加粗</strong>中文</p>\n"},
	} {
		if got := render(c.src, ExtTyporaEmphasis); got != c.want {
			t.Errorf("%q: got %q, want %q", c.src, got, c.want)
		}
	}
	if got := render("中文**“粗体”**中文", 0); strings.Contains(got, "<strong>") {
		t.Fatalf("strict parse read %q as strong", got)
	}
}

// TestTyporaEmphasisDelimiterRuns covers Typora's pairing of a strong
// pair and a three-star run, reopened from the saved bytes: the strong
// closes one star into the run, so the star before its close stays
// content and the stars after it stay literal (record 2026-11-10,
// probe75/a, /c, /h and seed10/c12).
func TestTyporaEmphasisDelimiterRuns(t *testing.T) {
	for _, tt := range []struct{ src, want string }{
		{"**a***b**\n", "<p><strong>a*</strong>b**</p>\n"},
		{"**a***\n", "<p><strong>a*</strong></p>\n"},
		{"***a**\n", "<p>*<strong>a</strong></p>\n"},
		{"****a**\n", "<p><strong></strong>a**</p>\n"},
		{"> alpha**ca***lyhl**\n", "<blockquote>\n<p>alpha<strong>ca*</strong>lyhl**</p>\n</blockquote>\n"},
		// A `***` closer after a space closes strong with its last two stars
		// and takes part in no emphasis, so the run stays one strong segment
		// (record 2026-11-11, newops/c46-6).
		{"- [ ] ***beta1. ***\n", "<ul>\n<li><input type=\"checkbox\" disabled=\"\" /> *<strong>beta1. </strong>*</li>\n</ul>\n"},
		// A `**` opener pairs with the middle two stars of a longer closer
		// run: the star before them joins the content and the stars after
		// them stay literal (`x**a****b**` shows `xa**b**`, record
		// 2026-11-12, probe116/ascii-ab, /cjk-pair4).
		{"x**a****b**\n", "<p>x<strong>a*</strong>*b**</p>\n"},
		{"试**vj****vj**\n", "<p>试<strong>vj*</strong>*vj**</p>\n"},
		{"alpha **bold** `code` [link](url)试**vj****vj**\n", "<p>alpha <strong>bold</strong> <code>code</code> <a href=\"url\">link</a>试<strong>vj*</strong>*vj**</p>\n"},
		// A `**` after a space still closes strong, so the run before the
		// space and the run at the end pair and leave the middle stars
		// literal, while a paragraph that starts with `_> ` opens a strong
		// whose content is `- ` (record 2026-11-13, seed21/c3, seed21/c10).
		{"alpha**- **rs**\n", "<p>alpha<strong>- </strong>rs**</p>\n"},
		{"_> **- **\n", "<p>_&gt; <strong>- </strong></p>\n"},
		// A one-star emphasis closing a run of three takes the run's last
		// star and keeps the two before it as content (record 2026-11-13,
		// seed21/c15).
		{"*beta> 试***\n", "<p><em>beta&gt; 试**</em></p>\n"},
	} {
		if got := render(tt.src, Typora); got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.src, got, tt.want)
		}
	}
	// The empty strong element is a document reading: a live preview that
	// holds its transient marker right after the stars must not take them
	// for one.
	marker := string(rune(0xE000))
	if got, want := render("****"+marker+"\n", Typora), "<p><strong></strong>"+marker+"</p>\n"; got != want {
		t.Errorf("with the empty strong extension: got %q, want %q", got, want)
	}
	if got, want := render("****"+marker+"\n", Typora&^ExtTyporaEmptyStrong), "<p>****"+marker+"</p>\n"; got != want {
		t.Errorf("without the empty strong extension: got %q, want %q", got, want)
	}
}

func TestTyporaHeadingNeedsSpace(t *testing.T) {
	for _, src := range []string{"#\n", "##\n", "#A\n", "##A\n"} {
		if got, want := render(src, Typora), "<p>"+strings.TrimSuffix(src, "\n")+"</p>\n"; got != want {
			t.Fatalf("%q: got %q, want %q", src, got, want)
		}
	}
	// CommonMark reads a line of hashes alone as an empty heading.
	if got := render("#\n", 0); got != "<h1></h1>\n" {
		t.Fatalf("strict #: got %q, want an empty heading", got)
	}
}

// TestTyporaListMarkerNeedsSpace: Typora reads a list marker ending its
// line as paragraph text, as its live editor does before the separating
// space arrives (record 2026-11-10, seed6/empty-emphasis-leaves-list and
// the open-state probe of `*\n\n*`). The strict profiles keep CommonMark's
// empty item.
func TestTyporaListMarkerNeedsSpace(t *testing.T) {
	for _, src := range []string{"-\n", "*\n", "+\n", "1.\n", "1)\n"} {
		if got, want := render(src, Typora), "<p>"+strings.TrimSuffix(src, "\n")+"</p>\n"; got != want {
			t.Fatalf("%q: got %q, want %q", src, got, want)
		}
	}
	for _, tt := range []struct{ src, want string }{
		{"-\n", "<ul>\n<li></li>\n</ul>\n"},
		{"1.\n", "<ol>\n<li></li>\n</ol>\n"},
	} {
		if got := render(tt.src, GFM); got != tt.want {
			t.Fatalf("strict %q: got %q, want %q", tt.src, got, tt.want)
		}
	}
}

// TestTyporaEmptyItemEndsAtItsLine: an item with no child block is its own
// marker line, so it ends where that line ends whatever the marker's width.
// Ending it one byte into the marker cut `1. ` short, the projection read the
// blank line after it from inside the marker, and the empty paragraph Typora
// shows below the exit was lost (record 2026-11-12, seed14/c4).
func TestTyporaEmptyItemEndsAtItsLine(t *testing.T) {
	for _, tt := range []struct {
		src        string
		start, end int
	}{
		{"- \n\n", 0, 2},
		{"1. \n\n", 0, 3},
		{"10. \n\n", 0, 4},
		{"-\t\n\n", 0, 2},
		{"1. a\n2. \n\n", 5, 8},
		{"- - \n\n", 2, 4},
		{"1. ", 0, 3},
	} {
		d := New(Typora)
		d.Set(tt.src)
		found := false
		for i := 1; i < d.Len(); i++ {
			b := d.Block(i)
			if b.Kind != Item || b.Start != tt.start {
				continue
			}
			found = true
			if b.End != tt.end {
				t.Errorf("%q: item span [%d,%d), want [%d,%d)", tt.src, b.Start, b.End, tt.start, tt.end)
			}
		}
		if !found {
			t.Errorf("%q: no item at %d", tt.src, tt.start)
		}
	}
}

// TestTyporaItemWhitespaceContinuation: a whitespace-only line that runs
// past a list item's continuation indent continues the item's paragraph.
// It is the line Shift+Enter's continuation prefix leaves when the caret
// sat before trailing whitespace, which Typora's editor keeps in the
// paragraph as a soft break (record 2026-11-10,
// seed6/ordered-softbreak-in-item). A line of just the indent holds no
// content and stays a blank line, as the strict profiles read every
// whitespace-only line.
func TestTyporaItemWhitespaceContinuation(t *testing.T) {
	for _, tt := range []struct {
		src     string
		content string
		breaks  int
	}{
		{"1. alph1.\n    \n", "alph1.\n ", 1},
		{"1. alph1.\n    ", "alph1.\n ", 1},
		{"- a\n   \n", "a\n ", 1},
		{"1. alph1.\n   \n", "alph1.", 0},
		{"- a\n  \n", "a", 0},
		// A paragraph outside a list reads a whitespace-only line as blank.
		{"alpha\n \n", "alpha", 0},
	} {
		d := New(Typora)
		d.Set(tt.src)
		para := -1
		for i := 1; i < d.Len(); i++ {
			if d.Block(i).Kind == Paragraph {
				para = i
			}
		}
		if para < 0 {
			t.Fatalf("%q has no paragraph", tt.src)
		}
		if text, _ := d.contentOf(para); text != tt.content {
			t.Errorf("%q: paragraph content %q, want %q", tt.src, text, tt.content)
		}
		breaks := 0
		for _, n := range d.Inlines(para) {
			if n.Kind == SoftBreak {
				breaks++
			}
		}
		if breaks != tt.breaks {
			t.Errorf("%q: %d soft break(s), want %d", tt.src, breaks, tt.breaks)
		}
	}
	if got := render("1. alph1.\n    \n", GFM); got != "<ol>\n<li>alph1.</li>\n</ol>\n" {
		t.Fatalf("strict parse kept the line as content: %q", got)
	}
}

func TestTyporaDoubleTildeOnly(t *testing.T) {
	want := "<p>~x~ <del>y</del></p>\n"
	if got := render("~x~ ~~y~~", Typora); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTyporaFrontMatter(t *testing.T) {
	d := New(ExtFrontMatter)
	d.Set("---\ntitle: x\n---\n正文")
	if got := d.Block(1).Kind; got != FrontMatter {
		t.Fatalf("first block is %v", got)
	}
	d = New(ExtFrontMatter)
	d.Set("---\n---\n")
	if got := d.Block(1).Kind; got == FrontMatter {
		t.Fatal("empty front matter accepted")
	}
}

func TestTyporaContentSpace(t *testing.T) {
	tests := []struct {
		src, typora, strict string
	}{
		{" alpha\n", "<p> alpha</p>\n", "<p>alpha</p>\n"},
		{"#  beta\n", "<h1> beta</h1>\n", "<h1>beta</h1>\n"},
		{"# \talpha\n", "<h1>\talpha</h1>\n", "<h1>alpha</h1>\n"},
		{">  beta\n", "<blockquote>\n<p> beta</p>\n</blockquote>\n", "<blockquote>\n<p>beta</p>\n</blockquote>\n"},
		{"-  beta\n", "<ul>\n<li> beta</li>\n</ul>\n", "<ul>\n<li>beta</li>\n</ul>\n"},
		{"1.   beta\n", "<ol>\n<li>  beta</li>\n</ol>\n", "<ol>\n<li>beta</li>\n</ol>\n"},
		{"- [ ]  beta\n", "<ul>\n<li><input type=\"checkbox\" disabled=\"\" />  beta</li>\n</ul>\n", "<ul>\n<li><input type=\"checkbox\" disabled=\"\" /> beta</li>\n</ul>\n"},
		{"> -  beta\n", "<blockquote>\n<ul>\n<li> beta</li>\n</ul>\n</blockquote>\n", "<blockquote>\n<ul>\n<li>beta</li>\n</ul>\n</blockquote>\n"},
	}
	for _, tt := range tests {
		if got := render(tt.src, Typora); got != tt.typora {
			t.Errorf("Typora %q: got %q, want %q", tt.src, got, tt.typora)
		}
		if got := render(tt.src, GFM); got != tt.strict {
			t.Errorf("strict %q: got %q, want %q", tt.src, got, tt.strict)
		}
	}
}

func TestTyporaContentSpaceInlines(t *testing.T) {
	tests := []struct {
		src   string
		kind  Kind
		start int
		text  string
	}{
		{" alpha\n", Paragraph, 0, " alpha"},
		{"#  beta\n", Heading, 2, " beta"},
		{">  beta\n", Paragraph, 2, " beta"},
		{"-  beta\n", Paragraph, 2, " beta"},
		{"1.   beta\n", Paragraph, 3, "  beta"},
		{"- [ ]  beta\n", Paragraph, 6, " beta"},
	}
	for _, tt := range tests {
		d := New(Typora)
		d.Set(tt.src)
		var in []Inline
		for i := 1; i < d.Len(); i++ {
			if d.Block(i).Kind == tt.kind && len(d.Inlines(i)) > 0 {
				in = d.Inlines(i)
				break
			}
		}
		if len(in) == 0 {
			t.Errorf("%q has no %v inlines", tt.src, tt.kind)
			continue
		}
		if in[0].Kind != Text || in[0].Start != tt.start || in[0].Text != tt.text {
			t.Errorf("%q: first inline %+v, want text %q at %d", tt.src, in[0], tt.text, tt.start)
		}
	}
}

func TestTyporaContentSpaceKeepsReferenceDefinitions(t *testing.T) {
	tests := []struct {
		src, want string
	}{
		{" [a]: /u\n\n[a]\n", "<p><a href=\"/u\">a</a></p>\n"},
		// The definition stops the paragraph from becoming a setext heading.
		{" [a]: /u\n---\n", "<p>---</p>\n"},
	}
	for _, tt := range tests {
		if got := render(tt.src, Typora); got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.src, got, tt.want)
		}
	}
}

func TestTyporaContentSpaceKeepsListIndentAsContainer(t *testing.T) {
	tests := []struct{ src, want string }{
		{"- alpha\n  continuation\n", "<ul>\n<li>alpha\ncontinuation</li>\n</ul>\n"},
		// A paragraph after a blank line is an indented continuation: the
		// item's content indent W (2 here) is the container's, and the
		// whitespace past it is content Typora shows (record 2026-11-11,
		// seed9/c9).
		{"- alpha\n\n  continuation\n", "<ul>\n<li>\n<p>alpha</p>\n<p>continuation</p>\n</li>\n</ul>\n"},
		{"- alpha\n\n   continuation\n", "<ul>\n<li>\n<p>alpha</p>\n<p> continuation</p>\n</li>\n</ul>\n"},
		{"- alpha\n\n    continuation\n", "<ul>\n<li>\n<p>alpha</p>\n<p>  continuation</p>\n</li>\n</ul>\n"},
		{"> - alpha\n>\n>      continuation\n", "<blockquote>\n<ul>\n<li>\n<p>alpha</p>\n<p>   continuation</p>\n</li>\n</ul>\n</blockquote>\n"},
	}
	for _, tt := range tests {
		if got := render(tt.src, Typora); got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.src, got, tt.want)
		}
	}
}

// TestTyporaContentSpaceStrictUnchanged pins the strict profiles to
// CommonMark: the whitespace past a list item's content indent is not
// content there, so the extension must not move the paragraph start.
func TestTyporaContentSpaceStrictUnchanged(t *testing.T) {
	tests := []struct{ src, want string }{
		{"- item\n\n  two\n", "<ul>\n<li>\n<p>item</p>\n<p>two</p>\n</li>\n</ul>\n"},
		{"- item\n\n   two\n", "<ul>\n<li>\n<p>item</p>\n<p>two</p>\n</li>\n</ul>\n"},
		{"- item\n\n    two\n", "<ul>\n<li>\n<p>item</p>\n<p>two</p>\n</li>\n</ul>\n"},
		{"- alpha\n\n    continuation\n", "<ul>\n<li>\n<p>alpha</p>\n<p>continuation</p>\n</li>\n</ul>\n"},
		{"- alpha\n   continuation\n", "<ul>\n<li>alpha\ncontinuation</li>\n</ul>\n"},
	}
	for _, tt := range tests {
		if got := render(tt.src, GFM); got != tt.want {
			t.Errorf("strict %q: got %q, want %q", tt.src, got, tt.want)
		}
	}
}

// TestTyporaEmphasisAcrossCodeSpan pins that a code span is no barrier: // a delimiter run pairs with one on its far side, as CommonMark reads it // and as Typora shows it (VM record 2026-11-12-192939, probe124c/cross-em, // /cross-strong, /cross-em-backtick-in). Issue 124 seed14/c5 was a fence // at the start of the line, not this rule: the emphasis markers there came // from bytes M0 wrote and Typora did not.
func TestTyporaEmphasisAcrossCodeSpan(t *testing.T) {
	for _, tt := range []struct{ src, want string }{
		{"*foo `bar` baz*\n", "<p><em>foo <code>bar</code> baz</em></p>\n"},
		{"**foo `bar` baz**\n", "<p><strong>foo <code>bar</code> baz</strong></p>\n"},
		{"*foo ``bar`` baz*\n", "<p><em>foo <code>bar</code> baz</em></p>\n"},
	} {
		if got := render(tt.src, Typora); got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.src, got, tt.want)
		}
	}
}

func TestTyporaEmphasisDropsRuleOfThree(t *testing.T) {
	// Typora matches * / ** with its own regexes, which have no CommonMark
	// multiple-of-3 restriction: `**bold*x` keeps the emphasis around bold.
	// The extra star stays outside the emphasis node, where Typora's regex
	// keeps it as the node's first glyph; both render the same text
	// (record 2026-11-09, p0/inline/bold/backspace-close).
	src := "alpha **bold*x omega\n"
	want := "<p>alpha *<em>bold</em>x omega</p>\n"
	if got := render(src, Typora); got != want {
		t.Fatalf("Typora %q: got %q, want %q", src, got, want)
	}
	if got := render(src, GFM); got != "<p>alpha **bold*x omega</p>\n" {
		t.Fatalf("strict %q changed: got %q", src, got)
	}
}

func TestTyporaStrikethroughIgnoresPunctuationFlanking(t *testing.T) {
	// Typora's del regex needs only a non-space after `~~` and before the
	// closing `~~`; a closer after a nested strong and before text still
	// matches, where CommonMark's right-flanking rule rejects it.
	src := "alpha ~~**a**~~x omega\n"
	want := "<p>alpha <del><strong>a</strong></del>x omega</p>\n"
	if got := render(src, Typora); got != want {
		t.Fatalf("Typora %q: got %q, want %q", src, got, want)
	}
	if got := render(src, GFM); got != "<p>alpha ~~<strong>a</strong>~~x omega</p>\n" {
		t.Fatalf("strict %q changed: got %q", src, got)
	}
}

func TestTyporaEmptySetextHeading(t *testing.T) {
	// A setext underline with no paragraph above it, only a blank line, is
	// the underline of an empty level-1 heading in Typora: the block spans
	// the blank content line through the underline. The strict profiles
	// keep CommonMark's paragraph (record 2026-11-10, seed9/c13).
	const src = "alpha\n=====\n\n\n=====\n"
	d := New(Typora)
	d.Set(src)
	if got, want := d.HTML(), "<h1>alpha</h1>\n<h1></h1>\n"; got != want {
		t.Fatalf("Typora %q: got %q, want %q", src, got, want)
	}
	h := -1
	for i := 0; i < d.Len(); i++ {
		if d.Block(i).Kind == Heading && d.Block(i).Start > 0 {
			h = i
		}
	}
	if h < 0 {
		t.Fatalf("Typora %q: no empty heading in %d blocks", src, d.Len())
	}
	b := d.Block(h)
	// The blank line before the underline, at 13, is the empty content
	// line; the heading ends where that line ends.
	if !b.Setext || b.Level != 1 || b.Start != 13 || b.End != 13 {
		t.Fatalf("empty heading = %+v, want setext level 1 at 13:13", *b)
	}
	if in := d.Inlines(h); len(in) != 0 {
		t.Fatalf("empty heading inlines = %+v, want none", in)
	}
	if got := render("\n====\n", 0); got != "<p>====</p>\n" {
		t.Fatalf("strict %q changed: got %q", "\n====\n", got)
	}
	if got := render("=====\n", Typora); got != "<p>=====</p>\n" {
		// The document's first line has no blank line above it.
		t.Fatalf("Typora first line %q: got %q", "=====\n", got)
	}
}

// TestTyporaCodeSpanPairing pins the code span Typora's dialect reads from
// runs of backticks of different lengths: with no run of the opener's own
// length after it, the next run of any length closes the span, and both
// fences are as wide as the shorter run, so the opener's other backticks are
// the span's text (record 2026-11-12, seed18/c1, /c7). CommonMark reads the
// same bytes as literal text.
func TestTyporaCodeSpanPairing(t *testing.T) {
	for _, tt := range []struct{ src, want string }{
		{"```beta`\n", "<p><code>``beta</code></p>\n"},
		{"```alpha **bold** `code` [link](url)\n", "<p><code>``alpha **bold** </code>code` <a href=\"url\">link</a></p>\n"},
		{"`` a ` b ``\n", "<p><code>a ` b</code></p>\n"},
		{"`a`\n", "<p><code>a</code></p>\n"},
	} {
		if got := render(tt.src, Typora); got != tt.want {
			t.Errorf("Typora %q: got %q, want %q", tt.src, got, tt.want)
		}
	}
	for _, tt := range []struct{ src, want string }{
		{"```beta`\n", "<p>```beta`</p>\n"},
		{"`foo``bar``\n", "<p>`foo<code>bar</code></p>\n"},
	} {
		if got := render(tt.src, 0); got != tt.want {
			t.Errorf("strict %q: got %q, want %q", tt.src, got, tt.want)
		}
	}
}
