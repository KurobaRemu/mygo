package richtext

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestKeyTypingThenDeletingRestoresSource(t *testing.T) {
	src := "# Head\n\n这是**粗体**和 `code` 的段落，见[链接](https://x.y)。\n\n- one\n- two\n\n> quote\n\n```go\nfunc main() {\n}\n```\n"
	for _, tc := range []struct{ after, typed string }{
		{"Head", "Headqr"},
		{"粗体", "粗体qr**"},
		{"段落", "段落qr"},
		{"链接", "x.y)qr"},
		{"one", "oneqr"},
		{"quote", "quoteqr"},
		{"main() {", "main() {qr"},
		{"code", "`code`qr"},
	} {
		for _, del := range []ui.Key{ui.KeyBackspace, ui.KeyDelete} {
			t.Run(tc.after, func(t *testing.T) {
				doc := NewMarkdownDocument(src)
				tt, _ := richTester(t, doc)
				text := doc.Text().String()
				i := strings.Index(text, tc.after)
				if i < 0 {
					t.Fatalf("%q not shown", tc.after)
				}
				at := len([]rune(text[:i+len(tc.after)]))
				doc.SetSelection(at, at)
				tt.Frame()
				tt.Type("q")
				tt.Frame()
				tt.Type("r")
				tt.Frame()
				if !strings.Contains(doc.Source(), tc.typed) {
					t.Fatalf("typed into %q", doc.Source())
				}
				if del == ui.KeyDelete {
					tt.Key(0, ui.KeyLeft)
					tt.Frame()
					tt.Key(0, ui.KeyLeft)
					tt.Frame()
				}
				tt.Key(0, del)
				tt.Frame()
				tt.Key(0, del)
				tt.Frame()
				if got := doc.Source(); got != src {
					t.Fatalf("key %v left %q", del, got)
				}
			})
		}
	}
}
