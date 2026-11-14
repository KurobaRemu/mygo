package richtext

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
)

func TestAppendBlockAtEndProjects(t *testing.T) {
	for _, src := range []string{"a\n\n", "# h\n\n", "```\nx\n```\n\n", "- a\n\n"} {
		doc := NewMarkdownDocument(src)
		if err := doc.ReplaceSource(len(src), len(src), "b"); err != nil {
			t.Fatal(err)
		}
		if err := doc.CheckSource(); err != nil {
			t.Fatalf("%q: %v", src, err)
		}
	}
}

func TestNewParagraphBeforeHeadingKeepsBlankLine(t *testing.T) {
	for _, paste := range []bool{false, true} {
		doc := NewMarkdownDocument("段落。\n\n## 待办\n")
		tt, _ := richTester(t, doc)
		doc.SetSelection(3, 3)
		tt.Frame()
		tt.Key(0, ui.KeyEnter)
		tt.Frame()
		if paste {
			tt.SetClipboardData(transfer.New(transfer.NewItem(transfer.Bytes(transfer.Text, []byte("新段")))))
			tt.Key(ui.Cmd, ui.KeyV)
		} else {
			tt.Type("新段")
		}
		tt.Frame()
		tt.Type("x")
		tt.Frame()
		if got := doc.Source(); got != "段落。\n\n新段x\n\n## 待办\n" {
			t.Fatalf("paste=%v wrote %q", paste, got)
		}
	}
}

func TestExitFinalCodeBlockThenType(t *testing.T) {
	src := "```\nx\n```\n"
	doc := NewMarkdownDocument(src)
	tt, _ := richTester(t, doc)
	doc.SetSelection(1, 1)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyEnter)
	tt.Frame()
	tt.Type("ab")
	tt.Frame()
	if got := doc.Source(); got != src+"\nab\n" {
		t.Fatalf("typed into %q", got)
	}
	if err := doc.CheckSource(); err != nil {
		t.Fatal(err)
	}
}

func TestEnterAtInlineRightEdgeSplitsOutside(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"详见 [面板](https://e.com/d)。\n", "详见 [面板](https://e.com/d)\n\n。\n"},
		{"- a `code` b\n", "- a `code`\n-  b\n"},
	} {
		doc := NewMarkdownDocument(c.src)
		tt, _ := richTester(t, doc)
		text := []rune(doc.Text().String())
		i := 0
		for i < len(text) && text[i] != '板' && text[i] != 'e' {
			i++
		}
		doc.SetSelection(i+1, i+1)
		tt.Frame()
		tt.Key(0, ui.KeyEnter)
		tt.Frame()
		if got := doc.Source(); got != c.want {
			t.Fatalf("%q wrote %q", c.src, got)
		}
	}
}

func TestNewLastParagraphKeepsFileEnding(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"a", "a\n\nbc"},
		{"a\n", "a\n\nbc\n"},
	} {
		doc := NewMarkdownDocument(c.src)
		tt, _ := richTester(t, doc)
		end := doc.Text().Len()
		doc.SetSelection(end, end)
		tt.Frame()
		tt.Key(0, ui.KeyEnter)
		tt.Frame()
		tt.Type("bc")
		tt.Frame()
		if got := doc.Source(); got != c.want {
			t.Fatalf("%q wrote %q", c.src, got)
		}
	}
}

func TestCaretMovedAfterLinkCommandTypesAtCaret(t *testing.T) {
	doc := NewMarkdownDocument("- 补充压测报告\n\nend\n")
	tt, _ := richTester(t, doc)
	doc.SetSelection(2, 6)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyK)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyDown)
	tt.Frame()
	tt.Type("Y")
	tt.Frame()
	if got := doc.Source(); got != "- 补充[压测报告]()\n\nendY\n" {
		t.Fatalf("wrote %q", got)
	}
}

// selectLinkTarget puts the caret in the empty destination of the link that
// ⌘K inserted, around the selection.
func selectLinkTarget(t *testing.T, doc *Document) {
	t.Helper()
	src := doc.Source()
	at := strings.Index(src, "](")
	if at < 0 {
		t.Fatalf("no link in %q", src)
	}
	doc.SetSourceSelection(sourceCaret(at + 2))
}

func TestUndoInLinkTargetKeepsCaretInTarget(t *testing.T) {
	doc := NewMarkdownDocument("- 补充压测报告\n")
	tt, _ := richTester(t, doc)
	doc.SetSelection(2, 6)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyK)
	tt.Frame()
	selectLinkTarget(t, doc)
	tt.Frame()
	tt.Type("y")
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyZ)
	tt.Frame()
	tt.Type("z")
	tt.Frame()
	if got := doc.Source(); got != "- 补充[压测报告](z)\n" {
		t.Fatalf("wrote %q", got)
	}
}

func TestPasteIntoLinkTargetKeepsBytes(t *testing.T) {
	doc := NewMarkdownDocument("- 补充压测报告\n")
	tt, _ := richTester(t, doc)
	doc.SetSelection(2, 6)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyK)
	tt.Frame()
	selectLinkTarget(t, doc)
	tt.Frame()
	tt.SetClipboardData(transfer.New(transfer.NewItem(transfer.Bytes(transfer.Text, []byte("https://e.com/p_1")))))
	tt.Key(ui.Cmd, ui.KeyV)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyDown)
	tt.Frame()
	tt.SetClipboardData(transfer.New(transfer.NewItem(transfer.Bytes(transfer.Text, []byte(" *x*")))))
	tt.Key(ui.Cmd, ui.KeyV)
	tt.Frame()
	if got := doc.Source(); got != "- 补充[压测报告](https://e.com/p_1) *x*\n" {
		t.Fatalf("wrote %q", got)
	}
}

func TestClickBelowFinalCodeBlockStartsParagraph(t *testing.T) {
	src := "a\n\n```\nx\n```\n"
	doc := NewMarkdownDocument(src)
	tt, _ := richTester(t, doc)
	doc.SetSelection(0, 0)
	tt.Frame()
	tt.ClickAt(250, 380)
	tt.Frame()
	tt.Type("ab")
	tt.Frame()
	if got := doc.Source(); got != src+"\nab\n" {
		t.Fatalf("wrote %q", got)
	}
	tt.ClickAt(250, 380)
	tt.Frame()
	if got := doc.Source(); got != src+"\nab\n" {
		t.Fatalf("second click wrote %q", got)
	}
}
