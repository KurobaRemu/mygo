package richtext

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

type step struct {
	mods ui.Modifiers
	key  ui.Key
	typ  string
}

func play(t *testing.T, src string, steps []step) string {
	doc := NewMarkdownDocument(src)
	tt, _ := richTester(t, doc)
	tt.Frame()
	for _, s := range steps {
		if s.typ != "" {
			tt.Type(s.typ)
		} else {
			tt.Key(s.mods, s.key)
		}
		tt.Frame()
	}
	return doc.Source()
}

func TestKeysEditAsTypora(t *testing.T) {
	top, end := step{ui.Cmd, ui.KeyUp, ""}, step{ui.Cmd, ui.KeyDown, ""}
	enter := step{0, ui.KeyEnter, ""}
	nav := "alpha beta gamma\n\n- item one\n- item two long text\n\nlast para here\n"
	for _, c := range []struct {
		name, src string
		steps     []step
		want      string
	}{
		{"up crosses blocks", nav, []step{end, {typ: "1"}, {0, ui.KeyUp, ""}, {typ: "2"}, {0, ui.KeyUp, ""}, {typ: "3"}}, "alpha beta gamma\n\n- item one3\n- item two lo2ng text\n\nlast para here1\n"},
		{"down after edit", nav, []step{top, {typ: "A"}, {0, ui.KeyDown, ""}, {typ: "B"}}, "Aalpha beta gamma\n\n- Bitem one\n- item two long text\n\nlast para here\n"},
		{"typing at bold end goes outside", "line1\n\nline2 **b**\n", []step{end, {typ: "X"}}, "line1\n\nline2 **b**X\n"},
		{"enter at quote end", "> a\n", []step{end, enter, {typ: "Y"}, enter, {typ: "Z"}}, "> a\n>\n> Y\n>\n> Z\n"},
		{"enter twice leaves quote", "> a\n", []step{end, enter, enter, {typ: "Z"}}, "> a\n\nZ\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := play(t, c.src, c.steps); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}
