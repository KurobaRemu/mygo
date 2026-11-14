package richtext

import (
	"strings"
	"testing"
)

func TestSpaceTypedAtLineEndStaysOnItsLine(t *testing.T) {
	for _, tc := range []struct{ src, after, want string }{
		{"# A\n\npara one\n\n## B\n", "one", "para one \n"},
		{"line a\nline b\n", "a", "line a \n"},
	} {
		doc := NewMarkdownDocument(tc.src)
		tt, _ := richTester(t, doc)
		text := doc.Text().String()
		at := len([]rune(text[:strings.Index(text, tc.after)+len(tc.after)]))
		doc.SetSelection(at, at)
		tt.Frame()
		tt.Type(" ")
		tt.Frame()
		if _, c := doc.Selection(); c != at+1 || doc.Text().paraAt(c) != doc.Text().paraAt(at) {
			t.Fatalf("%q: caret %d left the line, text %q", tc.src, c, doc.Text().String())
		}
		tt.Compose("x", 1)
		tt.Frame()
		tt.Type("x")
		tt.Frame()
		if !strings.Contains(doc.Source(), strings.Replace(tc.want, " \n", " x\n", 1)) {
			t.Fatalf("%q: wrote %q", tc.src, doc.Source())
		}
	}
}
