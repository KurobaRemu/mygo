package richtext

import (
	"fmt"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestExtraBlankLinesShowEmptyParagraphsAsTypora(t *testing.T) {
	for _, c := range []struct {
		src, text string
		paras     int
		last      string
	}{
		{"p1.\n\n\nH1\n", "p1.\nH1", 2, ""},
		{"p1.\n\n\n\nH1\n", "p1.\n\nH1", 3, ""},
		{"p1.\n\n\n\n\n\nH1\n", "p1.\n\n\nH1", 4, ""},
		{"p1.\n\n", "p1.\n", 2, "5:5"},
		{"p1.\n\n\n", "p1.\n", 2, ""},
		{"> a\n> \n", "a", 1, ""},
		{"> a\n>\n> \n\nb\n", "a\n\nb", 3, ""},
		{"> a\n>\n>\n>\n> b\n", "a\n\nb", 3, ""},
	} {
		d := NewMarkdownDocument(c.src)
		if got := d.Text().ParagraphCount(); got != c.paras {
			t.Errorf("%q has %d paragraphs, want %d", c.src, got, c.paras)
		}
		if got := d.Text().String(); got != c.text {
			t.Errorf("%q shows %q, want %q", c.src, got, c.text)
		}
		if c.last != "" {
			start, end, _ := d.SourceMap().Paragraph(c.paras - 1)
			if got := fmt.Sprintf("%d:%d", start, end); got != c.last {
				t.Errorf("%q last paragraph is sourced at %s, want %s", c.src, got, c.last)
			}
		}
		if err := d.CheckSource(); err != nil {
			t.Errorf("%q: %v", c.src, err)
		}
	}
}

func TestEnterAtBlockEndOpensEmptyParagraphThatTakesComposition(t *testing.T) {
	for _, c := range []struct {
		src, after string
		key        ui.Key
		mod        ui.Modifiers
		want       string
	}{
		{"# T\n\npara.\n\n## H\n", "para.", ui.KeyEnter, 0, "# T\n\npara.\n\n灰\n\n## H\n"},
		{"# T\n\npara.\n", "para.", ui.KeyEnter, 0, "# T\n\npara.\n\n灰\n"},
		{"a\n\n```\nx\n```\n", "x", ui.KeyEnter, ui.Cmd, "a\n\n```\nx\n```\n\n灰\n"},
		{"# T\n\n## H\n", "T", ui.KeyEnter, 0, "# T\n\n灰\n\n## H\n"},
	} {
		doc := NewMarkdownDocument(c.src)
		tt, _ := richTester(t, doc)
		text := doc.Text().String()
		at := len([]rune(text[:strings.Index(text, c.after)+len(c.after)]))
		doc.SetSelection(at, at)
		tt.Frame()
		before := doc.Source()
		tt.Key(c.mod, c.key)
		tt.Frame()
		if got := doc.Source(); got != before {
			t.Fatalf("%q: Enter wrote %q before content", c.src, got)
		}
		doc.lock()
		v := doc.activeView()
		transient := v != nil && v.transient != nil &&
			v.shown.paraAt(v.caret) == v.transient.para
		doc.mu.Unlock()
		if !transient {
			t.Fatalf("%q: caret is not in the transient paragraph", c.src)
		}
		tt.Compose("hui", 3)
		tt.Frame()
		tt.Type("灰")
		tt.Frame()
		if got := doc.Source(); got != c.want {
			t.Fatalf("%q: wrote %q, want %q", c.src, got, c.want)
		}
	}
}

func TestEnterAtFirstHeadingStartOpensEmptyParagraphAbove(t *testing.T) {
	doc := NewMarkdownDocument("# T\n\nb\n")
	tt, _ := richTester(t, doc)
	doc.SetSelection(0, 0)
	tt.Frame()
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if got := doc.Text().String(); got != "\nT\nb" {
		t.Fatalf("text %q", got)
	}
	tt.Key(0, ui.KeyUp)
	tt.Frame()
	tt.Type("x")
	tt.Frame()
	if got := doc.Source(); got != "x\n\n# T\n\nb\n" {
		t.Fatalf("wrote %q", got)
	}
}
