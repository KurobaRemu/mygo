package richtext

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/egoist/mygo/ui/markdown/parser"
)

func typeMarkdownWith(d *Document, at int, s string, a Attributes) error {
	d.SetSelection(at, at)
	d.SetTypingAttributes(a)
	var err error
	d.Edit(func(e *Edit) {
		e.ReplaceString(at, at, s, a)
		err = e.Err()
	})
	return err
}

func TestMarkdownCJKPunctuationAtBoldEnd(t *testing.T) {
	for _, punct := range []string{"：", "。", "，", "”", "）", ".", ","} {
		d := NewMarkdownDocument("中文**加粗**中文\n")
		s := d.Text().String()
		at := utf8.RuneCountInString(s[:strings.Index(s, "粗")]) + 1
		if err := typeMarkdown(d, at, punct); err != nil {
			t.Fatalf("%q: %v", punct, err)
		}
		want := "中文**加粗" + punct + "**中文\n"
		if got := d.Source(); got != want {
			t.Fatalf("%q: got %q, want %q", punct, got, want)
		}
		if err := d.CheckSource(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMarkdownCJKPunctuationWritesMarkdown(t *testing.T) {
	for _, c := range []struct {
		runs []Run
		want string
	}{
		{[]Run{{Text: "中文"}, {Text: "“加粗”", Attributes: Attributes{Weight: 700}}, {Text: "中文"}}, "中文**“加粗”**中文"},
		{[]Run{{Text: "中文"}, {Text: "（粗体）", Attributes: Attributes{Weight: 700}}, {Text: "中文"}}, "中文**（粗体）**中文"},
		{[]Run{{Text: "加粗：", Attributes: Attributes{Weight: 700}}, {Text: "正文"}}, "**加粗：**正文"},
		{[]Run{{Text: "中文"}, {Text: "“斜体”", Attributes: Attributes{Italic: true}}, {Text: "中文"}}, "中文*“斜体”*中文"},
	} {
		if got := TextOf(Paragraph{Runs: c.runs}).Markdown(); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestMarkdownPlainPunctuationOutsideBold(t *testing.T) {
	for _, punct := range []string{"：", "。", "，", "”", "）", ".", ","} {
		d := NewMarkdownDocument("中文**加粗**中文\n")
		s := d.Text().String()
		at := utf8.RuneCountInString(s[:strings.Index(s, "粗")]) + 1
		d.SetSelection(at, at)
		var err error
		d.Edit(func(e *Edit) {
			e.ReplaceString(at, at, punct, Attributes{})
			err = e.Err()
		})
		if err != nil {
			t.Fatalf("%q: %v", punct, err)
		}
		if got := d.Source(); got != "中文**加粗**"+punct+"中文\n" {
			t.Fatalf("%q: source %q", punct, got)
		}
	}
}

func TestMarkdownDeleteBoundarySpace(t *testing.T) {
	for _, c := range []struct {
		src, del, text string
	}{
		{"a b c\n", "a", " b c"},
		{"a b c\n", "c", "a b "},
		{"**b** tail\n", "b", " tail"},
	} {
		d := NewMarkdownDocument(c.src)
		at := strings.Index(d.Text().String(), c.del)
		var err error
		d.Edit(func(e *Edit) {
			e.ReplaceString(at, at+len([]rune(c.del)), "", Attributes{})
			err = e.Err()
		})
		if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		if got := d.Text().String(); got != c.text {
			t.Fatalf("%q: text %q", c.src, got)
		}
		if err := d.CheckSource(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMarkdownFormattingSurvivesDeletedInline(t *testing.T) {
	d := NewMarkdownDocument("a **bold** c\n")
	at := strings.Index(d.Text().String(), "bold")
	d.Edit(func(e *Edit) { e.ReplaceString(at, at+4, "", Attributes{Weight: 700}) })
	if err := typeMarkdownWith(d, at, "n", Attributes{Weight: 700}); err != nil {
		t.Fatal(err)
	}
	if err := typeMarkdownWith(d, at+1, "e", Attributes{Weight: 700}); err != nil {
		t.Fatal(err)
	}
	if err := typeMarkdownWith(d, at+2, "w", Attributes{Weight: 700}); err != nil {
		t.Fatal(err)
	}
	if got := d.Source(); got != "a **new** c\n" {
		t.Fatalf("source %q", got)
	}
}

func TestMarkdownTypingBoldInEmptyParagraph(t *testing.T) {
	d := NewMarkdownDocument("\n")
	if err := typeMarkdownWith(d, 0, "X", Attributes{Weight: 700}); err != nil {
		t.Fatal(err)
	}
	if got := d.Source(); got != "**X**\n" {
		t.Fatalf("source %q", got)
	}
}

func TestMarkdownEmptyFencedCodeEditable(t *testing.T) {
	for _, c := range []struct {
		src, want string
	}{
		{"```go\n```\n", "```go\nx\n```\n"},
		{"```go\n", "```go\nx"},
		{"```go\n\n```\n", "```go\nx\n```\n"},
	} {
		d := NewMarkdownDocument(c.src)
		if err := typeMarkdown(d, 0, "x"); err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		if got := d.Source(); got != c.want {
			t.Fatalf("%q: got %q, want %q", c.src, got, c.want)
		}
		if err := d.CheckSource(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMarkdownUnsupportedSyntaxStaysLiteral(t *testing.T) {
	for _, src := range []string{
		"~x~\n",
		"![alt](image.png)\n",
		"a <span>x</span> b\n",
		"<div>hello</div>\n",
		"---\ntitle: x\n---\n正文\n",
	} {
		txt, _ := ParseMarkdownSource(src)
		got := strings.TrimSuffix(txt.String(), "\n")
		want := strings.TrimSuffix(src, "\n")
		if got != want {
			t.Errorf("%q: text %q", src, got)
		}
	}
}

func TestMarkdownLiteralFallbackEditable(t *testing.T) {
	for _, c := range []struct {
		src, at, insert, want string
	}{
		{"---\ntitle: x\n---\n正文\n", "x", "y", "---\ntitle: y\n---\n正文\n"},
		{"![alt](image.png)\n", "alt", "ALT", "![ALT](image.png)\n"},
		{"<div>hello</div>\n", "hello", "hi", "<div>hi</div>\n"},
		{"| A | B |\n|---|---|\n", "A", "X", "| X | B |\n|---|---|\n"},
	} {
		d := NewMarkdownDocument(c.src)
		text := d.Text().String()
		at := utf8.RuneCountInString(text[:strings.Index(text, c.at)])
		d.Edit(func(e *Edit) { e.ReplaceString(at, at+utf8.RuneCountInString(c.at), c.insert, Attributes{}) })
		if got := d.Source(); got != c.want {
			t.Errorf("%q: got %q, want %q", c.src, got, c.want)
		}
		if err := d.CheckSource(); err != nil {
			t.Errorf("%q: %v", c.src, err)
		}
	}
}

func TestMarkdownMathFallback(t *testing.T) {
	d := parser.New(parser.Typora | parser.ExtMath | parser.ExtDollarMath)
	d.Set("$x$ and $$y$$")
	txt, _ := projectSource(d)
	if got := txt.String(); got != "$x$ and $$y$$" {
		t.Fatalf("text %q", got)
	}
	d = parser.New(parser.Typora | parser.ExtMath)
	d.Set("$$\nx+1\n$$\n")
	txt, _ = projectSource(d)
	if got := txt.String(); got != "$$\nx+1\n$$" {
		t.Fatalf("block text %q", got)
	}
}

func TestMarkdownBreakProjection(t *testing.T) {
	for _, src := range []string{"a\nb\n", "a  \nb\n", "a\\\nb\n"} {
		txt, _ := ParseMarkdownSource(src)
		if got := txt.String(); got != "a\nb" {
			t.Errorf("%q: text %q", src, got)
		}
	}
}

func TestMarkdownBreakEditable(t *testing.T) {
	for _, c := range []struct {
		src, want string
	}{
		{"a\nb\n", "ab\n"},
		{"a  \nb\n", "ab\n"},
		{"a\\\nb\n", "ab\n"},
		{"a<br>b\n", "ab\n"},
		{"> a  \n> b\n", "> ab\n"},
		{"- a\\\n  b\n", "- ab\n"},
	} {
		d := NewMarkdownDocument(c.src)
		at := strings.Index(d.Text().String(), "\n")
		d.Edit(func(e *Edit) { e.ReplaceString(at, at+1, "", Attributes{}) })
		if got := d.Source(); got != c.want {
			t.Errorf("%q: got %q, want %q", c.src, got, c.want)
		}
		if err := d.CheckSource(); err != nil {
			t.Errorf("%q: %v", c.src, err)
		}
	}
}

func TestMarkdownStructuralEditChangesVersion(t *testing.T) {
	d := NewMarkdownDocument("a\n")
	v := d.Version()
	var err error
	d.Edit(func(e *Edit) {
		e.ReplaceString(0, 1, "\n", Attributes{})
		err = e.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Version() == v || !d.CanUndo() {
		t.Fatalf("version %d, want after %d", d.Version(), v)
	}
}

func TestMarkdownAdjacentCodeBlocksHaveDifferentIDs(t *testing.T) {
	d := NewMarkdownDocument("```\na\n```\n\n```\nb\n```\n")
	var ids []uint32
	m := d.SourceMap()
	i := 0
	for _, p := range d.Text().Paragraphs() {
		if p.Style.CodeBlock {
			ids = append(ids, seqAt(m.paras, i).owner)
		}
		i++
	}
	if len(ids) != 2 || ids[0] == 0 || ids[0] == ids[1] {
		t.Fatalf("block ids %v", ids)
	}
}
