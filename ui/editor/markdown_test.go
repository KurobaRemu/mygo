package editor

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

func newMarkdownCaretTest(t *testing.T, text string) *editorTest {
	t.Helper()
	at := strings.IndexByte(text, '|')
	if at < 0 {
		t.Fatal("no caret")
	}
	plain := strings.ReplaceAll(text, "|", "")
	st := &State{Buffer: NewBuffer(plain)}
	st.Buffer.SetLanguage(LanguageNamed("markdown"))
	ct := &editorTest{t: t, st: st}
	ct.tt = ui.NewTester(func(c *ui.Context) {
		View(c, st).Fill().AutoFocus()
	}, 600, 400)
	ct.st.SetSelections(Selection{Anchor: at, Head: at})
	ct.tt.Frame()
	return ct
}

func TestMarkdownAutoPair(t *testing.T) {
	tests := []struct {
		name string
		text string
		typ  string
		want string
	}{
		{"parentheses", "|", "(", "(|)"},
		{"brackets", "|", "[", "[|]"},
		{"braces", "|", "{", "{|}"},
		{"double quote", "|", "\"", "\"|\""},
		{"single quote", "|", "'", "'|'"},
		{"word middle", "a|b", "(", "a(|b"},
		{"cjk quote", "中文|", "\"", "中文\"|\""},
		{"cjk punctuation", "中文|。", "(", "中文(|)。"},
		{"cjk word middle", "中|文", "(", "中(|文"},
		{"latin quote", "hello|", "\"", "hello\"|"},
		{"fullwidth bracket", "|", "［", "［|"},
		{"fullwidth brace", "|", "｛", "｛|"},
		{"fullwidth parenthesis", "|", "（", "（|"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := newEditorTest(t, "markdown", tt.text)
			ct.typ(tt.typ)
			ct.want(tt.want)
		})
	}
}

func TestMarkdownBacktickLiteral(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		want string
	}{
		{"single", "`", "`|"},
		{"fence", "```", "```|"},
		{"language fence", "```go", "```go|"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := newEditorTest(t, "markdown", "|")
			ct.typ(tt.typ)
			ct.want(tt.want)
		})
	}
}

func TestMarkdownBackspacePair(t *testing.T) {
	tests := []struct {
		name string
		typ  string
	}{
		{"bracket", "("},
		{"double quote", "\""},
		{"single quote", "'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := newEditorTest(t, "markdown", "|")
			ct.typ(tt.typ)
			ct.key(0, ui.KeyBackspace)
			ct.want("|")
		})
	}
}

func TestMarkdownNewlineList(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"unordered", "- item|", "- item\n- |"},
		{"plus", "+ item|", "+ item\n+ |"},
		{"ordered period", "1. item|", "1. item\n2. |"},
		{"ordered parenthesis", "3) item|", "3) item\n4) |"},
		{"task", "- [x] item|", "- [x] item\n- [ ] |"},
		{"task uppercase", "- [X] item|", "- [X] item\n- |"},
		{"quote", "> quote|", "> quote\n> |"},
		{"nested quote", "> > item|", "> > item\n> > |"},
		{"empty item", "- |", "\n|"},
		{"empty task", "- [ ] |", "\n|"},
		{"empty quote", "> |", "> \n|"},
		{"ordered siblings", "1. one|\n2. two", "1. one\n2. |\n2. two"},
		{"quoted list continues quote", "> - item|", "> - item\n> |"},
		{"not a list", "text|", "text\n|"},
		{"before marker", "| - item", "\n| - item"},
		{"four spaces", "    - item|", "    - item\n    |"},
		{"fenced code", "```\n- item|\n```", "```\n- item\n|\n```"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := newMarkdownCaretTest(t, tt.text)
			ct.key(0, ui.KeyEnter)
			ct.want(tt.want)
		})
	}
}

func TestMarkdownTab(t *testing.T) {
	tests := []struct {
		name string
		text string
		mods ui.Modifiers
		want string
	}{
		{"list end", "- child|", 0, "- child\t|"},
		{"line start", "|- item", 0, "\t|- item"},
		{"outdent bullet", "  - |child", ui.Shift, "- |child"},
		{"outdent task", "  - [ ] |t", ui.Shift, "- [ ] |t"},
		{"outdent ordered", "   1. |o", ui.Shift, " 1. |o"},
		{"outdent quoted", "  > - |q", ui.Shift, "> - |q"},
		{"outdent tab", "\t- |item", ui.Shift, "- |item"},
		{"outdent two tabs", "\t\t- |item", ui.Shift, "\t- |item"},
		{"outdent space tab", " \t- |item", ui.Shift, "- |item"},
		{"outdent tab space", "\t - |item", ui.Shift, " - |item"},
		{"outdent none", "|text", ui.Shift, "|text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := newMarkdownCaretTest(t, tt.text)
			ct.key(tt.mods, ui.KeyTab)
			ct.want(tt.want)
		})
	}
}
