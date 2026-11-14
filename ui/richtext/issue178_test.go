package richtext

import (
	"testing"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
)

// The preedit an input method shows inside paired delimiters. The display
// used to append it after the whole part holding the delimiters, because the
// insertion point was measured by the rune count a part had already drawn
// rather than by the source offset in front of the rune at the caret: typing
// pinyin between `[` and `]` drew `zhong[]`, and a caret on either bracket of
// a verbatim pair drew the preedit outside it (record 2026-11-13, issue 178).

// issue178Case is one caret in one line of source, with what the composition
// must draw there: the display text, where the preedit starts and ends, and
// the display column the caret, the underline and the candidate window share.
type issue178Case struct {
	name    string
	src     string
	source  int
	display string
	start   int
}

func (c issue178Case) end() int { return c.start + len([]rune("zhong")) }

func issue178Cases() []issue178Case {
	return []issue178Case{
		// A literal pair is one verbatim part: `[]` draws no element, so the
		// composition cuts the part between its runes.
		{"bracketsOpen", "[]\n", 0, "zhong[]", 0},
		{"bracketsInside", "[]\n", 1, "[zhong]", 1},
		{"bracketsAfter", "[]\n", 2, "[]zhong", 2},
		{"parensInside", "()\n", 1, "(zhong)", 1},
		// An empty link draws its delimiters as markers and keeps them in
		// the parts, so every byte of `[]()` is a place the preedit can go.
		{"emptyLinkOpen", "[]()\n", 0, "zhong[]()", 0},
		{"emptyLinkLabel", "[]()\n", 1, "[zhong]()", 1},
		{"emptyLinkBetween", "[]()\n", 2, "[]zhong()", 2},
		{"emptyLinkDestination", "[]()\n", 3, "[](zhong)", 3},
		{"emptyLinkAfter", "[]()\n", 4, "[]()zhong", 4},
		// A link reveals its markers under the caret, so the label takes
		// the preedit after `[`, in its middle and before `]`.
		{"linkAfterBracket", "[ab](https://example.com)\n", 1, "[zhongab](https://example.com)", 1},
		{"linkLabelMiddle", "[ab](https://example.com)\n", 2, "[azhongb](https://example.com)", 2},
		{"linkBeforeBracket", "[ab](https://example.com)\n", 3, "[abzhong](https://example.com)", 3},
		{"linkAfterLabel", "[ab](https://example.com)\n", 4, "[ab]zhong(https://example.com)", 4},
		// Strong, emphasis and code: the opening run is revealed under a
		// caret in it or inside the content, the closing one only when the
		// caret reaches its bytes.
		{"strongOpenRun", "**bold**\n", 1, "*zhong*bold**", 1},
		{"strongContentStart", "**bold**\n", 2, "**zhongbold**", 2},
		{"strongContentMiddle", "**bold**\n", 3, "**bzhongold**", 3},
		{"strongContentEnd", "**bold**\n", 6, "**boldzhong**", 6},
		{"strongCloseRun", "**bold**\n", 7, "**bold*zhong*", 7},
		{"emphasisOpenRun", "*e*\n", 1, "*zhonge*", 1},
		{"emphasisContentEnd", "*e*\n", 2, "*ezhong*", 2},
		{"codeOpenRun", "`c`\n", 1, "`zhongc`", 1},
		{"codeContentEnd", "`c`\n", 2, "`czhong`", 2},
		// A paragraph of CJK has one part per style run, and the bytes of
		// a rune are three: the insertion point is measured in runes of the
		// part, not in bytes of the source.
		{"cjkSecond", "中文段落\n", 3, "中zhong文段落", 1},
		{"cjkThird", "中文段落\n", 6, "中文zhong段落", 2},
		{"cjkEnd", "中文段落\n", 12, "中文段落zhong", 4},
	}
}

// TestIssue178CompositionInsideDelimiters pins the display and the four
// places the composition shows at: the text the paragraph draws, the preedit
// range, the caret, the underline and the candidate window's rectangle.
func TestIssue178CompositionInsideDelimiters(t *testing.T) {
	for _, tc := range issue178Cases() {
		t.Run(tc.name, func(t *testing.T) {
			_, v := markerView(tc.src)
			v.rect = ui.Rect{W: 800, H: 400}
			v.width, v.viewH = 800, 400
			setSourceCaret(v, tc.source)
			v.SetMarkedText(nil, "zhong", ui.TextInputRange{Start: 5, End: 5})
			v.layout(800, 400)

			pv := v.paraLayout(0)
			if pv.display == nil {
				t.Fatal("the composition laid the paragraph out without a display")
			}
			dp := *pv.display
			if got := displayText(dp); got != tc.display {
				t.Fatalf("display %q, want %q", got, tc.display)
			}
			if dp.composeStart != tc.start || dp.composeEnd != tc.end() {
				t.Fatalf("preedit %d..%d, want %d..%d in %q", dp.composeStart, dp.composeEnd, tc.start, tc.end(), tc.display)
			}

			// The underline and the caret share the display column, which is
			// the preedit's start.
			if col := v.composeColumn(0, pv); col != tc.start {
				t.Fatalf("underline starts at column %d, want %d", col, tc.start)
			}

			// The caret enters the preedit at its start and the input method
			// puts it at the end of the syllable, both inside the delimiters;
			// the candidate window's left edge is that start.
			startX := issue178ColumnX(v, pv, tc.start)
			endX := issue178ColumnX(v, pv, tc.start+len([]rune("zhong")))
			if x, _, _ := v.caretAt(v.caret, 0, v.affinity); !issue178Near(x, startX) {
				t.Fatalf("caret at x %.2f, want the preedit's start %.2f", x, startX)
			}
			if x, _, _ := v.caretAt(v.caret, utf8.RuneCountInString("zhong"), v.affinity); !issue178Near(x, endX) {
				t.Fatalf("caret at x %.2f, want the preedit's end %.2f", x, endX)
			}
			caret := ui.TextInputRange{Start: v.units(v.caret), End: v.units(v.caret)}
			bounds, actual, ok := v.BoundsForRange(caret)
			if !ok {
				t.Fatalf("no candidate window rectangle at %v", caret)
			}
			if !issue178Near(bounds.X, v.padX+startX) {
				t.Fatalf("candidate window at x %.2f, want %.2f", bounds.X, v.padX+startX)
			}
			if actual != caret {
				t.Fatalf("candidate window range %v, want %v", actual, caret)
			}

			// The input method reads back the preedit where it draws.
			marked, has := v.MarkedRange()
			if !has {
				t.Fatal("no marked range while composing")
			}
			if text, _ := v.TextForRange(marked); text != "zhong" {
				t.Fatalf("marked range reads %q, want the preedit", text)
			}
		})
	}
}

// issue178ColumnX is the content x at display column l of a laid out
// paragraph, which is where caretAt puts the caret for that column: the
// paragraph's origin plus the layout's own x.
func issue178ColumnX(v *view, pv *paraView, l int) float32 {
	l = max(0, min(l, len(pv.units)-1))
	ox, _ := v.origin(0)
	return ox + pv.layout.CaretAt(ui.TextCaretPosition{Index: pv.units[l], Affinity: ui.TextDownstream}).X
}

// issue178Near reports whether a and b are the same x up to the layout's own
// rounding: a rune's advance is fractional, and the two paths sum it
// differently.
func issue178Near(a, b float32) bool {
	d := a - b
	return d > -0.5 && d < 0.5
}

// TestIssue178CompositionUpdatesInPlace pins a pinyin sequence updating the
// preedit inside `[]`: each step redraws the same place, and committing the
// syllable writes it between the brackets.
func TestIssue178CompositionUpdatesInPlace(t *testing.T) {
	d := NewMarkdownDocument("[]\n")
	tt, _ := richTester(t, d)
	d.SetSelection(1, 1)
	tt.Frame()
	for _, step := range []struct {
		preedit string
		display string
	}{
		{"z", "[z]"},
		{"zh", "[zh]"},
		{"zho", "[zho]"},
		{"zhon", "[zhon]"},
		{"zhong", "[zhong]"},
	} {
		tt.Compose(step.preedit, len([]rune(step.preedit)))
		tt.Frame()
		if got := issue178Display(t, d); got != step.display {
			t.Fatalf("preedit %q drew %q, want %q", step.preedit, got, step.display)
		}
	}
	tt.Type("中")
	tt.Frame()
	assertSource(t, d, "[中]\n")
	if a, z := d.Selection(); a != 2 || z != 2 {
		t.Fatalf("caret %d-%d after the commit, want 2", a, z)
	}
}

// TestIssue178CompositionCancellationInsideBrackets pins the empty composition
// macOS sends to end one: the source and the caret stay as they were, because
// the preedit was never in the bytes.
func TestIssue178CompositionCancellationInsideBrackets(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		at   int
	}{
		{"brackets", "[]\n", 1},
		{"linkLabel", "[ab](https://example.com)\n", 2},
		{"strong", "**bold**\n", 3},
		{"code", "`c`\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, v := markerView(tc.src)
			v.setSourceSelection(sourceCaret(tc.at))
			v.SetMarkedText(nil, "zhong", ui.TextInputRange{Start: 5, End: 5})
			v.SetMarkedText(nil, "", ui.TextInputRange{})
			if got := d.Source(); got != tc.src {
				t.Fatalf("the empty composition wrote %q, want %q", got, tc.src)
			}
			if sel := d.SourceSelection(); sel.Caret.Offset != tc.at || sel.Anchor.Offset != tc.at {
				t.Fatalf("the empty composition left %+v, want the caret at %d", sel, tc.at)
			}
		})
	}
}

// TestIssue178UnmarkRetainsThePreedit pins what UnmarkText does when the input
// method gives up the marked state: the preedit becomes the document's text
// where it was drawn, which is inside the delimiters it composed in.
func TestIssue178UnmarkRetainsThePreedit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		src    string
		source int
		want   string
		caret  int
	}{
		{"bracketsInside", "[]\n", 1, "[zhong]\n", 6},
		{"linkLabelMiddle", "[ab](https://example.com)\n", 2, "[azhongb](https://example.com)\n", 6},
		{"strongContentMiddle", "**bold**\n", 3, "**bzhongold**\n", 6},
		{"codeContent", "`c`\n", 1, "`zhongc`\n", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, v := markerView(tc.src)
			v.setSourceSelection(sourceCaret(tc.source))
			v.SetMarkedText(nil, "zhong", ui.TextInputRange{Start: 5, End: 5})
			v.UnmarkText()
			assertSource(t, d, tc.want)
			if a, z := d.Selection(); a != tc.caret || z != tc.caret {
				t.Fatalf("caret %d-%d, want %d", a, z, tc.caret)
			}
		})
	}
}

// TestIssue178CompositionReplacesSelection pins a selection replaced by a
// composition: the preedit draws where the selection was, at the left edge of
// what it covers, and the commit writes over the selected bytes only.
func TestIssue178CompositionReplacesSelection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		src     string
		from    int
		to      int
		display string
		start   int
		want    string
	}{
		{"linkLabel", "[ab](https://example.com)\n", 1, 3, "zhong", 0, "[中](https://example.com)\n"},
		{"linkLabelTail", "[ab](https://example.com)\n", 2, 3, "azhong", 1, "[a中](https://example.com)\n"},
		{"strongContent", "**bold**\n", 2, 6, "zhong", 0, "**中**\n"},
		{"strongMiddle", "**bold**\n", 3, 5, "bzhongd", 1, "**b中d**\n"},
		{"codeContent", "`c`\n", 1, 2, "zhong", 0, "`中`\n"},
		{"cjkMiddle", "中文段落\n", 3, 9, "中zhong落", 1, "中中落\n"},
		{"wholePair", "[]\n", 0, 2, "zhong", 0, "中\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, v := markerView(tc.src)
			v.rect = ui.Rect{W: 800, H: 400}
			v.width, v.viewH = 800, 400
			v.setSourceSelection(sourceSelection(tc.from, tc.to))
			v.SetMarkedText(nil, "zhong", ui.TextInputRange{Start: 5, End: 5})
			v.layout(800, 400)

			pv := v.paraLayout(0)
			dp := *pv.display
			if got := displayText(dp); got != tc.display {
				t.Fatalf("display %q, want %q", got, tc.display)
			}
			if dp.composeStart != tc.start || dp.composeEnd != tc.start+5 {
				t.Fatalf("preedit %d..%d, want %d..%d in %q", dp.composeStart, dp.composeEnd, tc.start, tc.start+5, tc.display)
			}
			// The view caret stands at the selection's end, which the pending
			// range maps past the preedit; what draws at the preedit is its
			// underline and the candidate window, both at its start.
			if col := v.composeColumn(0, pv); col != tc.start {
				t.Fatalf("underline starts at column %d, want %d", col, tc.start)
			}
			startX := issue178ColumnX(v, pv, tc.start)
			marked, has := v.MarkedRange()
			if !has {
				t.Fatal("no marked range while composing over a selection")
			}
			bounds, actual, ok := v.BoundsForRange(marked)
			if !ok {
				t.Fatalf("no candidate window rectangle at %v", marked)
			}
			if !issue178Near(bounds.X, v.padX+startX) {
				t.Fatalf("candidate window at x %.2f, want the preedit's start %.2f", bounds.X, v.padX+startX)
			}
			if actual.Start != marked.Start {
				t.Fatalf("candidate window range %v starts past %v", actual, marked)
			}

			v.ReplaceText(nil, "中")
			assertSource(t, d, tc.want)
		})
	}
}

// TestIssue178TypedBracketsThenCompose pins the path the user reported: typing
// `[` pairs it with `]` and puts the caret between them, and the next
// composition shows there rather than after the pair.
func TestIssue178TypedBracketsThenCompose(t *testing.T) {
	d := NewMarkdownDocument("")
	tt, _ := richTester(t, d)
	tt.Type("[")
	tt.Frame()
	assertSource(t, d, "[]")
	for _, step := range []struct {
		preedit string
		display string
	}{
		{"z", "[z]"},
		{"zh", "[zh]"},
		{"zhong", "[zhong]"},
	} {
		tt.Compose(step.preedit, len([]rune(step.preedit)))
		tt.Frame()
		if got := issue178Display(t, d); got != step.display {
			t.Fatalf("preedit %q drew %q, want %q", step.preedit, got, step.display)
		}
	}
	tt.Type("中")
	tt.Frame()
	assertSource(t, d, "[中]")
	assertCaretIsInput(t, d, tt, "after composing between typed brackets")
}

// TestIssue178CompositionAtFoldedEdges pins the boundary the display keeps
// between folded delimiters and the text around them: a caret in front of a
// link's `[` or past its `](url)` draws the preedit outside the element, and
// the caret inside the label draws it inside, with the same one-column rules.
func TestIssue178CompositionAtFoldedEdges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		src     string
		source  int
		display string
		start   int
	}{
		{"beforeLink", "x [ab](https://example.com) y\n", 2, "x zhong[ab](https://example.com) y", 2},
		{"afterLink", "x [ab](https://example.com) y\n", 27, "x [ab](https://example.com)zhong y", 27},
		{"labelStart", "x [ab](https://example.com) y\n", 3, "x [zhongab](https://example.com) y", 3},
		{"beforeStrong", "a **bold** c\n", 2, "a zhong**bold** c", 2},
		{"afterStrong", "a **bold** c\n", 10, "a **bold**zhong c", 10},
		{"strongStart", "a **bold** c\n", 4, "a **zhongbold** c", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, v := markerView(tc.src)
			v.rect = ui.Rect{W: 800, H: 400}
			v.width, v.viewH = 800, 400
			setSourceCaret(v, tc.source)
			v.SetMarkedText(nil, "zhong", ui.TextInputRange{Start: 5, End: 5})
			v.layout(800, 400)

			pv := v.paraLayout(0)
			dp := *pv.display
			if got := displayText(dp); got != tc.display {
				t.Fatalf("display %q, want %q", got, tc.display)
			}
			if dp.composeStart != tc.start {
				t.Fatalf("preedit at column %d, want %d", dp.composeStart, tc.start)
			}
			if col := v.composeColumn(0, pv); col != tc.start {
				t.Fatalf("underline at column %d, want %d", col, tc.start)
			}
			slice := []rune(tc.display)
			if got := string(slice[tc.start : tc.start+5]); got != "zhong" {
				t.Fatalf("columns %d..%d of %q hold %q, want the preedit", tc.start, tc.start+5, tc.display, got)
			}
		})
	}
}

// TestIssue178CommitInsideEveryDelimiter pins the commit, not just the
// preedit: the syllables land between the delimiters they were composed in,
// and the source stays parseable with the caret right after them.
func TestIssue178CommitInsideEveryDelimiter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		src    string
		source int
		want   string
		caret  int
	}{
		{"bracketsInside", "[]\n", 1, "[中]\n", 4},
		{"emptyLinkLabel", "[]()\n", 1, "[中]()\n", 4},
		{"linkAfterBracket", "[ab](https://example.com)\n", 1, "[中ab](https://example.com)\n", 4},
		{"linkLabelMiddle", "[ab](https://example.com)\n", 2, "[a中b](https://example.com)\n", 5},
		{"linkBeforeBracket", "[ab](https://example.com)\n", 3, "[ab中](https://example.com)\n", 6},
		{"strongContentStart", "**bold**\n", 2, "**中bold**\n", 5},
		{"strongContentMiddle", "**bold**\n", 3, "**b中old**\n", 6},
		{"strongContentEnd", "**bold**\n", 6, "**bold中**\n", 9},
		{"emphasisContentEnd", "*e*\n", 2, "*e中*\n", 5},
		{"codeContent", "`c`\n", 1, "`中c`\n", 4},
		{"cjkSecond", "中文段落\n", 3, "中中文段落\n", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, v := markerView(tc.src)
			v.setSourceSelection(sourceCaret(tc.source))
			v.SetMarkedText(nil, "zhong", ui.TextInputRange{Start: 5, End: 5})
			v.ReplaceText(nil, "中")
			assertSource(t, d, tc.want)
		})
	}
}

// issue178Display is what the editor's live view draws for paragraph 0.
func issue178Display(t *testing.T, d *Document) string {
	t.Helper()
	d.lock()
	v := d.activeView()
	d.mu.Unlock()
	if v == nil {
		t.Fatal("the editor has no view")
	}
	return displayText(v.displayParagraph(0, Attributes{}))
}
