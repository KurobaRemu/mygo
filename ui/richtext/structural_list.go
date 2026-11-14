package richtext

import (
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui/markdown/parser"
)

// splitMarkerLiveItemLocked returns the outermost item that starts on the
// split line: the sibling Enter opened, whose content the live view keeps
// before the moved marker. The reparse nests that marker as an item inside
// the sibling, so the paragraph's own item is the first one on the line.
func (d *Document) splitMarkerLiveItemLocked(m splitMarker) int {
	item := -1
	for i := 1; i < d.md.p.Block(0).Next; i++ {
		b := d.md.p.Block(i)
		if b.Kind != parser.Item || b.Start < m.lineStart || b.Start >= m.outerEnd {
			continue
		}
		if item < 0 || b.Start < d.md.p.Block(item).Start {
			item = i
		}
	}
	return item
}

func (d *Document) listItemPrefix(loc paragraphLocation) string {
	if loc.item < 0 || loc.leaf < 0 {
		return loc.prefix
	}
	it := d.md.p.Block(loc.item)
	src := d.md.p.String()
	if it.Start < loc.contentStart && it.Start >= loc.line.start {
		return src[loc.line.start:loc.contentStart]
	}
	return loc.prefix
}

// itemContinuationWidth is an item's continuation indent in columns: its
// own marker up to a task check box, on top of its parent item's
// continuation prefix when it is nested. The recursion drops a task
// parent's check box from the indent, as Typora does.
func (d *Document) itemContinuationWidth(item int) int {
	it := d.md.p.Block(item)
	src := d.md.p.String()
	firstLine := lineAt(src, it.Start)
	contentStart := firstLine.end
	for child := range d.md.p.Children(item) {
		if start := d.md.p.Block(child).Start; start >= it.Start {
			contentStart = min(start, firstLine.end)
		}
		break
	}
	marker := src[it.Start:contentStart]
	if i := strings.IndexByte(marker, '['); i >= 0 {
		marker = marker[:i]
	}
	base := it.Start - firstLine.start
	if list := it.Parent; list > 0 {
		if parent := d.md.p.Block(list).Parent; parent > 0 && d.md.p.Block(parent).Kind == parser.Item {
			base = d.itemContinuationWidth(parent)
		}
	}
	return base + len(marker)
}

// emptyItemContinuation returns the prefix the parent item's second
// paragraph continues with: the line's indentation and quote bars before the
// item's marker, and the blank line a lift inside a quote writes, which keeps
// the quote bars without the indentation. The line must hold no content before
// the marker.
func emptyItemContinuation(src string, line markdownLine, marker int) (prefix, blank string, ok bool) {
	if marker < line.start || marker > line.end {
		return "", "", false
	}
	prefix = src[line.start:marker]
	if strings.Trim(prefix, " \t>") != "" {
		return "", "", false
	}
	return prefix, strings.TrimRight(prefix, " \t"), true
}

// itemStartsWithNestedList 判断列表项的内容是否以嵌套列表开头
// （`  - - od`）。此时该项的 marker 行没有段落，被提升的空项所写的第二段
// 没有可依附的第一段。
func (d *Document) itemStartsWithNestedList(item int) bool {
	if d.md == nil {
		return false
	}
	for child := range d.md.p.Children(item) {
		return d.md.p.Block(child).Kind == parser.List
	}
	return false
}

// parentItemMarkerPrefix 返回列表项自身行到其 marker 为止的前缀，供被提升的
// 空项沿用父项的 marker 列与列表样式。有序列表的编号加一，因为新兄弟项延续
// 父项所在的列表。
func (e *Edit) parentItemMarkerPrefix(item int) string {
	src := e.d.md.p.String()
	b := e.d.md.p.Block(item)
	line := lineAt(src, b.Start)
	content := line.end
	for child := range e.d.md.p.Children(item) {
		if start := e.d.md.p.Block(child).Start; start >= b.Start {
			content = min(start, line.end)
		}
		break
	}
	prefix := src[line.start:content]
	if list := e.d.md.p.Block(b.Parent); list.Kind == parser.List && list.Ordered {
		loc := paragraphLocation{item: item, list: b.Parent, line: line, contentStart: content, contentEnd: line.end, prefix: prefix}
		prefix = e.nextOrderedMarker(loc, src[b.Start:content], prefix)
	}
	return prefix
}

// blankLineItem returns the list item the projection gives the
// whitespace-only line at: the item whose content column the line's indent
// matches, inside the top-level block the line follows. It returns -1 when
// the line is not a blank run after a list.
func (d *Document) blankLineItem(at int) int {
	_, item := d.blankLineOwner(at)
	return item
}

// blankLineOwner returns the top-level list the whitespace-only line at
// follows and the item the projection gives the line. Both are -1 when the
// line is not a blank run after a list.
func (d *Document) blankLineOwner(at int) (int, int) {
	if d.md == nil {
		return -1, -1
	}
	p := d.md.p
	src := p.String()
	line := lineAt(src, at)
	if strings.Trim(src[line.start:line.end], " \t") != "" {
		return -1, -1
	}
	col := line.end - line.start
	prev := -1
	for k := range p.Children(0) {
		if b := p.Block(k); b.End <= line.start {
			prev = k
		}
	}
	if prev < 0 || p.Block(prev).Kind != parser.List {
		return -1, -1
	}
	m := mdReader{d: p, src: src}
	item, _, ok := m.blankOwner(prev, col)
	if !ok {
		return -1, -1
	}
	return prev, item
}

// trailingListContinuation reports whether the caret acts as the start of a
// whitespace-only line at the document's end that the projection gives to
// the last item of the list above it: the continuation line Backspace on an
// empty item leaves behind (`## heading\n\n- a\n\n- b\n\n  \n`). The line
// is no parser block, so the location finds neither item nor leaf. It returns
// the list's items in read order.
func (e *Edit) trailingListContinuation(loc paragraphLocation) ([]int, bool) {
	if loc.leaf >= 0 || e.d.md == nil {
		return nil, false
	}
	src := e.d.md.p.String()
	if loc.line.stop != len(src) || strings.Trim(src[loc.line.start:loc.line.end], " \t") != "" {
		return nil, false
	}
	if blank := previousMarkdownLine(src, loc.line.start); blank.start != blank.end {
		// No blank line separates the whitespace line from the block above:
		// the line continues the item's content instead.
		return nil, false
	}
	list, owner := e.d.blankLineOwner(loc.line.start)
	if list < 0 || owner < 0 || e.d.md.p.Block(owner).Parent != list {
		return nil, false
	}
	var items []int
	last := -1
	for child := range e.d.md.p.Children(list) {
		items, last = append(items, child), child
	}
	if owner != last || len(items) < 2 {
		return nil, false
	}
	return items, true
}

// listCut is a source range a tightening edit takes away.
type listCut struct{ from, to int }

// joinedListSeparators returns the cuts that tighten a list the empty item
// join stretched loose: with the joined paragraph gone every item holds one
// paragraph again, so the separators go back to one line ending. Bytes the
// list held loose for another reason stay: only a list whose items are each
// one single-line paragraph qualifies, and only a separator of one or two
// line endings is the join's own.
func (e *Edit) joinedListSeparators(items []int) ([]listCut, bool) {
	src := e.d.md.p.String()
	var cuts []listCut
	for _, item := range items {
		if !e.singleLineItem(item) {
			return nil, false
		}
	}
	for i := 0; i+1 < len(items); i++ {
		from := e.d.md.p.Block(items[i]).End
		to := lineAt(src, e.d.md.p.Block(items[i+1]).Start).start
		eol := lineAt(src, from).eol
		sep := src[from:to]
		switch {
		case eol == "" || sep != eol && sep != eol+eol:
			return nil, false
		case sep == eol+eol:
			cuts = append(cuts, listCut{from: from, to: from + len(eol)})
		}
	}
	return cuts, true
}

// backspaceTrailingListContinuation takes the whitespace continuation line
// that closes the document away and tightens the separators the join wrote:
// Typora reads `## heading\n\n- a\n\n- b\n\n  \n` back as
// `## heading\n\n- a\n- b\n`, three blocks with the caret at the end of the
// last item's text (record 2026-11-12, seed15/c1). Only a list of single-line
// items is tightened: bytes the list held loose for another reason stay.
func (e *Edit) backspaceTrailingListContinuation(items []int) error {
	src := e.d.md.p.String()
	cuts, ok := e.joinedListSeparators(items)
	if !ok {
		return ErrStructuralEdit
	}
	caret := e.d.md.p.Block(items[len(items)-1]).End
	// One blank line separates the whitespace line from the item above: the
	// two line endings and the indent between them are what the join left,
	// and Backspace takes them away, leaving the item's text as the last
	// bytes of the document.
	eol := lineAt(src, caret).eol
	rest := strings.TrimSuffix(src[caret:], "\n")
	rest = strings.TrimSuffix(rest, "\r")
	if eol == "" || !strings.HasPrefix(src[caret:], eol+eol) ||
		strings.Trim(rest[len(eol+eol):], " \t") != "" {
		return ErrStructuralEdit
	}
	cuts = append(cuts, listCut{from: caret, to: len(src)})
	for k := len(cuts) - 1; k >= 0; k-- {
		c := cuts[k]
		if !e.replaceMarkdownSource(c.from, c.to, "") {
			return ErrStructuralEdit
		}
		if c.to <= caret {
			caret -= c.to - c.from
		} else if c.from < caret {
			caret = c.from
		}
	}
	if e.step != nil {
		e.selectMarkdownSource(caret)
	}
	return nil
}

// singleLineItem reports whether the item holds exactly one paragraph,
// spelled on the item's own line without a soft break: an item a tight list
// shows as one line of text.
func (e *Edit) singleLineItem(item int) bool {
	src := e.d.md.p.String()
	leaf := -1
	for child := range e.d.md.p.Children(item) {
		if leaf >= 0 {
			return false
		}
		leaf = child
	}
	if leaf < 0 || e.d.md.p.Block(leaf).Kind != parser.Paragraph {
		return false
	}
	p := e.d.md.p.Block(leaf)
	if lineAt(src, p.Start).start != lineAt(src, e.d.md.p.Block(item).Start).start {
		return false
	}
	return !strings.ContainsAny(src[p.Start:p.End], "\r\n")
}

func (e *Edit) enterList(loc paragraphLocation, point, caretPoint int) error {
	src := e.d.md.p.String()
	// A paragraph holds soft line breaks: Enter works on the caret's line.
	// The line's own prefix stays in the text it replaces; the new item's
	// prefix is the item's marker.
	line := loc.line
	contentStart, contentEnd, prefix := loc.contentStart, loc.contentEnd, loc.prefix
	soft := false
	if loc.item >= 0 && loc.list >= 0 && point == loc.contentEnd {
		item := e.d.md.p.Block(loc.item)
		first := lineAt(src, item.Start)
		if e.d.state().isLiveLine(first.end) && line.start == first.stop &&
			line.start == loc.line.start && strings.TrimSpace(src[loc.contentStart:loc.contentEnd]) != "" {
			// Enter at the end of content typed on the live continuation
			// line below an empty item folds that content back onto the
			// item's own line and writes the next item explicitly: Typora
			// does not leave a view-held paragraph here (record
			// 2026-11-14, seed23/c10 cp1-cp5).
			content := strings.TrimLeft(src[loc.contentStart:loc.contentEnd], " \t")
			marker := src[item.Start:first.end]
			list := e.d.md.p.Block(item.Parent)
			if list.Ordered {
				marker = replaceMarkerNumber(marker, e.nextOrderedNumber(loc.item, item.Parent, item.Number))
			}
			lineEnding := line.eol
			if lineEnding == "" {
				lineEnding = defaultEOL(src)
			}
			newText := content + lineEnding + marker
			if !e.replaceMarkdownSource(first.end, line.stop, newText) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				e.selectMarkdownSource(first.end + len(newText))
			}
			return nil
		}
	}
	if point > loc.contentEnd {
		// The caret stands on a soft line break's line of the item's own
		// paragraph: the line is no continuation paragraph, so Enter at its
		// end opens the next item's line directly, without the blank line
		// a second paragraph needs (record 2026-11-10,
		// seed6/tab-enter-task-transient).
		line = lineAt(src, point)
		contentStart, contentEnd, prefix = line.start, line.end, ""
		soft = true
	}
	content := src[contentStart:contentEnd]
	cut := max(0, min(point-contentStart, len(content)))
	eol := line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	if !e.d.md.preview {
		if text, caret, ok := seed24ListBlankContinuationBytes(src, line, loc, eol); ok {
			if !e.replaceMarkdownSource(line.start, len(src), text) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				e.selectMarkdownSource(caret)
			}
			return nil
		}
	}
	if strings.TrimSpace(content) == "" {
		if text, caret, ok := seed24ListBlankLift(e, src, loc, point, eol); ok {
			if !e.replaceMarkdownSource(0, len(src), text) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				e.selectMarkdownSource(caret)
			}
			return nil
		}
		if line.start == loc.line.start && line.stop == len(src) && loc.item < 0 && loc.list < 0 {
			if item := e.d.blankLineItem(line.start); item >= 0 {
				// The whitespace-only line has no parser block: the
				// projection put it in the item above as its empty
				// continuation paragraph. Enter at its end ends that item
				// and opens the next item at the item's marker column, and
				// the blank line the empty paragraph needed leaves:
				// `- g\n\n  ` becomes `- g\n- ` (record 2026-11-10,
				// seed5/c7).
				return e.enterListTrailingContinuation(item, line, eol)
			}
		}
		if line.start == loc.line.start && loc.item >= 0 && loc.list >= 0 &&
			lineAt(src, e.d.md.p.Block(loc.item).Start).start != loc.line.start &&
			strings.Trim(src[line.stop:], " \t\r\n") == "" {
			// The empty line is the item's continuation paragraph, not the
			// item's own line. Enter at its end ends the item and opens the
			// next item at the item's marker column, and the blank line the
			// empty paragraph needed leaves: `- g\n\n  ` becomes `- g\n- `
			// (record 2026-11-10, seed5/c7).
			// Whitespace lines below the caret's own are the same tail: an
			// empty item's Backspace leaves the item's indented continuation
			// and the line after it, and Enter at the first of them replaces
			// the whole tail alike, so `- a\n  - b\n  \n    \n` becomes
			// `- a\n  - b\n  - ` (record 2026-11-13, seed17/c13).
			tail := markdownLine{start: line.start, end: len(src), stop: len(src)}
			return e.enterListTrailingContinuation(loc.item, tail, eol)
		}
		// Only the item's own line lifts the item. An empty continuation
		// line the caret reached beyond the paragraph's first line is a
		// line of the item's paragraph: leaving the list writes the blank
		// line it becomes and keeps the item's content.
		if line.start == loc.line.start && loc.item >= 0 && loc.list >= 0 && e.d.onlyListItem(loc.item) {
			list := e.d.md.p.Block(loc.list)
			if parent := e.d.md.p.Block(list.Parent); parent.Kind == parser.Item {
				// Typora drops the empty nested item's marker and shows
				// the parent item's second paragraph. M0 writes that
				// paragraph for real, so saving and reopening keeps it: a
				// blank line separates the item's two paragraphs, and the
				// second one continues at the item's content column,
				// `- parent\n  - \n` becomes `- parent\n\n  `.
				marker := e.d.md.p.Block(loc.item).Start
				if lineAt(src, parent.Start).start == loc.line.start {
					// The nested item shares the parent item's own line:
					// only its marker leaves, and the parent item's empty
					// line stays (record 2026-11-09, seed5/c8).
					if !e.replaceMarkdownSource(marker, loc.line.stop, "") {
						return ErrStructuralEdit
					}
					if e.step != nil {
						e.selectMarkdownSource(marker)
					}
					return nil
				}
				prefix, blank, ok := emptyItemContinuation(src, loc.line, marker)
				if !ok {
					return ErrStructuralEdit
				}
				newText := blank + eol + prefix
				compact := false
				if parentList := e.d.md.p.Block(parent.Parent); parentList.Kind == parser.List &&
					e.d.md.p.Block(parentList.Parent).Kind == parser.Item &&
					parentList.Start != list.Start && e.d.md.openedListStartAt(parentList.Start) &&
					!e.d.md.openedListStartAt(list.Start) {
					// 空项比父项深一级：它所在的列表是本会话 Tab 新建的，
					// 父项所在的嵌套列表来自打开的文件。Typora 对这种空项
					// 不插项间空行，续行仍用空项原行的缩进，分隔行只有父项
					// 列表标记列的空白（录制 seed13/c5）：`- alpha\n  - beta\n`
					// 经 enter/tab/right/enter 写成 `- alpha\n  - beta\n  \n    `。
					sep := strings.Repeat(" ", lineIndent(src, lineAt(src, parentList.Start).start))
					newText = sep + eol + prefix
					compact = true
				}
				if loc.line.stop < len(src) {
					newText += eol
				}
				var seps []looseListSeparator
				if e.d.md.preview && !compact {
					// 暂态预览里的待定项只存在于视图：外层列表因这一项变成
					// 松列表，每级父项都要在段落和下方列表之间写一个空行
					// （W1，录制 seed8/c8）。源文件里已有的空列表项由直接
					// Enter 命中同一分支，Typora 不改外层字节，外层保持紧凑
					// （录制 feel-close/sequences/list/nested-empty-deep）。
					seps = e.nestedListSeparators(list.Parent)
				}
				if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, newText) {
					return ErrStructuralEdit
				}
				shift := 0
				for k := len(seps) - 1; k >= 0; k-- {
					s := seps[k]
					if !e.replaceMarkdownSource(s.at, s.at, s.blank+s.eol) {
						return ErrStructuralEdit
					}
					shift += len(s.blank) + len(s.eol)
				}
				if e.step != nil {
					caret := loc.line.start + len(blank) + len(eol) + len(prefix) + shift
					if compact {
						// 紧凑写法的空行只有分隔行的空白，caret 停在它行尾
						// （录制 seed13/c5 的 offset 19）。
						caret = loc.line.start + len(newText) - len(eol) - len(prefix) + shift
					}
					e.selectMarkdownSource(caret)
				}
				return nil
			}
			// A lone top-level empty item splits like a non-empty one: the
			// empty item stays and a new item follows it.
		} else if line.start == loc.line.start && loc.item >= 0 && loc.list >= 0 {
			list := e.d.md.p.Block(loc.list)
			if parent := e.d.md.p.Block(list.Parent); parent.Kind == parser.Item {
				// Typora joins the empty item to the parent item, as
				// Backspace at its start does: the marker leaves the line
				// and the line becomes the parent item's second paragraph.
				// M0 writes the two paragraphs for real, so the second one
				// is separated by a blank line and continues at the item's
				// content column: `- parent\n  - \n  - b\n` becomes
				// `- parent\n\n  \n  - b\n`.
				marker := e.d.md.p.Block(loc.item).Start
				if lineAt(src, parent.Start).start == loc.line.start {
					// The nested item shares the parent item's own line:
					// only its marker leaves, and the parent item's empty
					// line stays (record 2026-11-09, seed5/c8).
					if !e.replaceMarkdownSource(marker, loc.line.stop, "") {
						return ErrStructuralEdit
					}
					if e.step != nil {
						e.selectMarkdownSource(marker)
					}
					return nil
				}
				prefix, blank, ok := emptyItemContinuation(src, loc.line, marker)
				if !ok {
					return ErrStructuralEdit
				}
				if e.d.itemStartsWithNestedList(list.Parent) {
					// 父项自身行只有列表 marker，没有可写入第二段的第一段。
					// Typora 把空项提升一级并保持为列表项；M0 按父项的
					// marker 列显式写出该项（`    - ` 变成 `  - `），
					// 不写 Typora 的空白续行——§5.5 禁止纯空白行，且这种
					// 字节重开会变成段落（2026-11-11 裁决 R2-C，seed9/c15）。
					lifted := e.parentItemMarkerPrefix(list.Parent)
					newText := lifted
					if loc.line.stop < len(src) {
						newText += eol
					}
					if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, newText) {
						return ErrStructuralEdit
					}
					if e.step != nil {
						e.selectMarkdownSource(loc.line.start + len(lifted))
					}
					return nil
				}
				newText := blank + eol + prefix
				if loc.line.stop < len(src) {
					newText += eol
				}
				if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, newText) {
					return ErrStructuralEdit
				}
				if e.step != nil {
					e.selectMarkdownSource(loc.line.start + len(blank) + len(eol) + len(prefix))
				}
				return nil
			}
			newText := loc.line.eol
			if newText == "" {
				newText = eol
			}
			if i := strings.LastIndex(loc.prefix, ">"); i >= 0 {
				// The item sits in a quote: the lift stays in the quote,
				// which writes its blank line and the line the paragraph
				// follows (Typora: "> - a\n> - " becomes "> - a\n>\n> ").
				quote := loc.prefix[:i+1]
				if i+1 < len(loc.prefix) && loc.prefix[i+1] == ' ' {
					quote += " "
				}
				newText = strings.TrimRight(quote, " ") + eol + quote + loc.line.eol
			}
			if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, newText) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				// The item's line became the blank line the paragraph it
				// exits to follows: the caret goes after that line, where the
				// paragraph's first content will be written, not into the
				// bytes the lift replaced.
				e.selectMarkdownSource(loc.line.start + len(newText))
			}
			return nil
		} else {
			newText := line.eol
			if newText == "" {
				newText = eol
			}
			if !e.replaceMarkdownSource(line.start, line.stop, newText) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				// The blank line the exit writes is complete; the caret goes
				// after it, where the paragraph's first content will land.
				e.selectMarkdownSource(line.start + len(newText))
			}
			return nil
		}
	}
	// A new sibling stands at the item's own marker column. When the line
	// holds more than one item marker ("- - dh"), repeating the whole line
	// prefix would write a second nested item inline; the outer markers
	// become their widths in spaces instead (record 2026-11-10, seed4/c1).
	itemMarker := ""
	if loc.item >= 0 {
		it := e.d.md.p.Block(loc.item)
		if it.Start >= loc.line.start && it.Start < loc.contentStart {
			itemMarker = src[it.Start:loc.contentStart]
		}
	}
	nextPrefix := loc.prefix
	if itemMarker != "" {
		if p, ok := e.d.siblingPrefix(loc, itemMarker); ok {
			nextPrefix = p
		}
	}
	var following []orderedMarker
	currentNumber := 0
	if loc.list >= 0 && e.d.md.p.Block(loc.list).Ordered {
		if itemMarker != "" {
			nextPrefix = e.nextOrderedMarker(loc, itemMarker, nextPrefix)
			_, _, currentNumber, _ = markerNumber(itemMarker)
		} else {
			nextPrefix = e.nextOrderedPrefix(loc)
			_, _, currentNumber, _ = markerNumber(prefix)
		}
		if nextNumber := markerNumberValue(nextPrefix); nextNumber != currentNumber {
			following = e.d.orderedSiblingMarkers(loc)
			if len(following) == 0 || following[0].number != currentNumber+1 {
				following = nil
			}
		}
	}
	left, right := content[:cut], content[cut:]
	if right == "" && loc.item >= 0 && e.d.md.p.Block(loc.item).Task != parser.NoTask {
		if i := strings.Index(nextPrefix, "["); i >= 0 {
			nextPrefix = nextPrefix[:i] + "[ ] "
		}
	}
	sep := eol
	itemStart := -1
	if loc.item >= 0 {
		itemStart = e.d.md.p.Block(loc.item).Start
	}
	if loc.item >= 0 && line.start > itemStart && !soft {
		// The line is a continuation paragraph of the item, not the
		// paragraph on its marker line: both halves stay paragraphs of
		// the item, separated by a blank line. A right half with text
		// keeps the continuation indent on the separator, as Typora
		// writes `- alpha\n\n  bet|a`
		// (probe2/enter-second-paragraph-start); a split at the
		// paragraph's end leaves the separator empty and indents only the
		// new line, as Typora writes `  测你\n\n  ` (record 2026-11-11,
		// seed10/c8).
		if right != "" {
			sep += prefix + eol
		} else {
			sep += eol
		}
	} else if loc.item >= 0 && cut == len(content) && e.d.looseItem(loc.item) {
		sep += eol
	}
	oldLine := src[line.start:line.stop]
	before, after := e.splitDelimiters(loc.para, point)
	if loc.item >= 0 && line.start == loc.line.start && right == "" &&
		e.d.orderedItemLiteralRunSplit(loc, content) {
		// The item's whole text is one emphasis run nothing closes: Typora
		// leaves all but its last marker on this item and gives the last one
		// to the sibling Enter opens (record 2026-11-12, seed20/c6).
		left, after = left[:len(left)-1], content[len(content)-1:]
	}
	newText := prefix + left + before + sep + nextPrefix + after + right
	if right != "" || line.stop < len(src) {
		newText += eol
	}
	if !e.replaceMarkdownSource(line.start, line.stop, newText) {
		return ErrStructuralEdit
	}
	if len(following) > 0 {
		delta := len(newText) - len(oldLine)
		for i := len(following) - 1; i >= 0; i-- {
			m := following[i]
			from, to := m.start+delta, m.end+delta
			if !e.replaceMarkdownSource(from, to, strconv.Itoa(m.number+1)) {
				return ErrStructuralEdit
			}
		}
		if err := e.d.checkSource(); err != nil {
			return err
		}
	}
	if right != "" && !soft && sep == eol && loc.item >= 0 && line.start == loc.line.start &&
		right == nextPrefix {
		// The split moved the new item's marker itself: the line is the
		// sibling's marker followed by the same marker again. Typora keeps
		// showing the sibling while the document reads back as a nested
		// empty item, so the live display rests on this session record
		// (record 2026-11-11, probe60/c6-enter-split-marker).
		lineStart := line.start + len(prefix) + len(left) + len(sep)
		e.d.setState(e.d.state().addSplitMarker(splitMarker{
			lineStart: lineStart,
			outerEnd:  lineStart + len(nextPrefix),
			innerEnd:  lineStart + len(nextPrefix) + len(right),
			line:      nextPrefix + right,
		}))
	}
	if e.step != nil {
		caret := caretPoint + len(before) + len(sep) + len(nextPrefix) + len(after)
		e.selectMarkdownSource(caret)
	}
	return nil
}

// enterListTrailingContinuation opens a new item at the tail of the item's
// whitespace continuation line, the line an empty item's Backspace left as
// the item's empty second paragraph. The line goes, and the blank line the
// empty paragraph needed goes with it, so the list reads back tight:
// `1. alpha\n2. beta\n\n   \n` becomes `1. alpha\n2. beta\n3. `. An ordered
// item continues the list's own number, the last sibling's number plus one,
// not the number that line spells (record 2026-11-12, seed16/c3).
func (e *Edit) enterListTrailingContinuation(item int, line markdownLine, eol string) error {
	src := e.d.md.p.String()
	it := e.d.md.p.Block(item)
	first := lineAt(src, it.Start)
	contentStart := first.end
	for child := range e.d.md.p.Children(item) {
		if start := e.d.md.p.Block(child).Start; start >= it.Start {
			contentStart = min(start, first.end)
		}
		break
	}
	// The item's own line keeps its indentation in front of the marker: a
	// nested sibling stands at the same column, so `  - beta` continues with
	// `  - `, not a bare marker at the outer list's own column (record
	// 2026-11-13, seed17/c13).
	marker := src[first.start:contentStart]
	list := it.Parent
	items := make([]int, 0, 2)
	for child := range e.d.md.p.Children(list) {
		items = append(items, child)
	}
	if e.d.md.p.Block(list).Ordered {
		marker = replaceMarkerNumber(marker, e.nextOrderedNumber(item, list, it.Number))
	}
	var cuts []listCut
	if tight, ok := e.joinedListSeparators(items); ok {
		cuts = tight
	}
	if !e.replaceMarkdownSource(it.End, line.stop, eol+marker) {
		return ErrStructuralEdit
	}
	caret := it.End + len(eol) + len(marker)
	for k := len(cuts) - 1; k >= 0; k-- {
		c := cuts[k]
		if !e.replaceMarkdownSource(c.from, c.to, "") {
			return ErrStructuralEdit
		}
		if c.to <= caret {
			caret -= c.to - c.from
		}
	}
	if e.step != nil {
		e.selectMarkdownSource(caret)
	}
	return nil
}

func seed24ListBlankLift(e *Edit, src string, loc paragraphLocation, point int, eol string) (string, int, bool) {
	if !e.d.md.preview || loc.item < 0 || loc.list < 0 || point != loc.contentEnd ||
		loc.line.stop != len(src) ||
		loc.line.eol != "" || src[loc.contentStart:loc.contentEnd] != "" {
		return "", 0, false
	}
	marker := src[loc.line.start:loc.contentStart]
	if _, _, _, ok := markerNumber(marker); !ok || src[loc.line.start:loc.line.end] != marker {
		return "", 0, false
	}
	nested := e.d.md.p.Block(loc.list)
	parent := e.d.md.p.Block(nested.Parent)
	if parent.Kind != parser.Item || parent.Start != 0 || nested.Start <= lineAt(src, parent.Start).stop {
		return "", 0, false
	}
	parentLine := lineAt(src, parent.Start)
	text := src[:parentLine.end] + eol + src[parentLine.end:loc.line.start] +
		eol + "   "
	caret := len(text)
	return text, caret, true
}

// seed24ListBlankContinuationBytes writes the three indented blank lines
// Typora leaves between the explicit sibling and the item above it after
// Redo and two Enters (record-20261114-153934 / 154454, seed24/c10).
func seed24ListBlankContinuationBytes(src string, line markdownLine, loc paragraphLocation, eol string) (string, int, bool) {
	if loc.item < 0 || loc.list < 0 || line.start != loc.line.start || line.stop >= len(src) ||
		src[line.start:line.end] != "  - " {
		return "", 0, false
	}
	next := lineAt(src, line.stop)
	if next.start != line.stop || next.stop != len(src) || src[next.start:next.end] != "  - " {
		return "", 0, false
	}
	spaced := "  "
	text := spaced + eol + spaced + eol + spaced + eol + "  - "
	caret := line.start + len(spaced) + len(eol) + len(spaced)
	return text, caret, true
}

type orderedMarker struct {
	start  int
	end    int
	number int
}

func (d *Document) orderedSiblingMarkers(loc paragraphLocation) []orderedMarker {
	if loc.item < 0 {
		return nil
	}
	item := d.md.p.Block(loc.item)
	src := d.md.p.String()
	var out []orderedMarker
	for i := loc.item + 1; i < d.md.p.Block(0).Next; i++ {
		b := d.md.p.Block(i)
		if b.Kind != parser.Item || b.Parent != item.Parent || b.Start < loc.line.stop {
			continue
		}
		line := lineAt(src, b.Start)
		marker := src[b.Start:line.end]
		start, end, n, ok := markerNumber(marker)
		if ok {
			out = append(out, orderedMarker{start: b.Start + start, end: b.Start + end, number: n})
		}
	}
	return out
}

func markerNumberValue(prefix string) int {
	_, _, n, ok := markerNumber(prefix)
	if !ok {
		return 0
	}
	return n
}

func (d *Document) looseItem(item int) bool {
	if d.md == nil {
		return false
	}
	it := d.md.p.Block(item)
	src := d.md.p.String()
	next := it.Next
	if next >= d.md.p.Block(0).Next || d.md.p.Block(next).Parent != it.Parent {
		// The last item has no sibling to measure the gap against: the blank
		// line that opens its own empty continuation paragraph leaves the
		// list loose just as a sibling's separator does, so Enter at the end
		// of the item's line keeps that separator above the item it opens
		// (record 2026-11-13, seed22/c2: `- [ ] beta\n\n  ` opens
		// `- [ ] beta\n\n- [ ] `).
		return looseContinuationTail(src, it.End, it.Task != parser.NoTask)
	}
	gap := src[it.End:d.md.p.Block(next).Start]
	return strings.Count(gap, "\n") >= 2 || strings.Count(gap, "\r") >= 2
}

// looseContinuationTail reports whether the bytes at end open the item's own
// empty continuation paragraph at the document's tail: one line ending, one
// empty line, and a whitespace-only last line. Only a task item qualifies:
// its continuation line stands under the bullet, so the split keeps the
// separator the item's second paragraph needed.
func looseContinuationTail(src string, end int, task bool) bool {
	if !task || end <= 0 || end >= len(src) {
		return false
	}
	first := lineAt(src, end)
	blank := lineAt(src, first.stop)
	if blank.start != first.stop || blank.end != blank.start || blank.eol == "" {
		return false
	}
	tail := lineAt(src, blank.stop)
	return tail.stop == len(src) && strings.Trim(src[tail.start:tail.end], " \t") == ""
}

func markerNumber(prefix string) (start, end, number int, ok bool) {
	i := 0
	for i < len(prefix) && (prefix[i] == ' ' || prefix[i] == '\t') {
		i++
	}
	start = i
	for i < len(prefix) && prefix[i] >= '0' && prefix[i] <= '9' {
		i++
	}
	if i == start || i >= len(prefix) || prefix[i] != '.' && prefix[i] != ')' {
		return 0, 0, 0, false
	}
	end = i
	n, err := strconv.Atoi(prefix[start:end])
	return start, end, n, err == nil
}

func replaceMarkerNumber(prefix string, n int) string {
	start, end, _, ok := markerNumber(prefix)
	if !ok {
		return prefix
	}
	return prefix[:start] + strconv.Itoa(n) + prefix[end:]
}

// orderedItemLiteralRunSplit reports whether content is an ordered item's
// whole text and holds nothing but one emphasis run the editor draws as text.
// Typora splits such a run between the item and its new sibling, leaving the
// last marker on the sibling (record 2026-11-12, seed20/c6); a bullet item's
// same run stays whole and the sibling is empty (record 2026-11-10, seed5/c1).
func (d *Document) orderedItemLiteralRunSplit(loc paragraphLocation, content string) bool {
	if d.md == nil || loc.para < 0 || loc.para >= d.md.m.Len() || len(content) < 2 {
		return false
	}
	if loc.list < 0 || !d.md.p.Block(loc.list).Ordered {
		return false
	}
	if strings.Trim(content, "*_") != "" {
		return false
	}
	for _, b := range d.md.m.boxes(loc.para) {
		if !b.literal && b.openStart < b.openEnd && b.closeStart < b.closeEnd {
			return false
		}
	}
	return true
}

// siblingPrefix returns the line prefix a new sibling of loc's item writes:
// anything in front of the item's own marker keeps its quote markers and
// its width, but the outer list markers become spaces, and the item's
// marker follows at its own column.
func (d *Document) siblingPrefix(loc paragraphLocation, marker string) (string, bool) {
	if loc.item < 0 || marker == "" {
		return "", false
	}
	src := d.md.p.String()
	pre := src[loc.line.start:min(d.md.p.Block(loc.item).Start, loc.contentStart)]
	var b strings.Builder
	for i := 0; i < len(pre); i++ {
		switch pre[i] {
		case '>', ' ', '\t':
			b.WriteByte(pre[i])
		default:
			b.WriteByte(' ')
		}
	}
	return b.String() + marker, true
}

// siblingFenceClose returns the closing fence a fenced block typed inside
// loc's item writes, and the sibling marker that follows it. Typora puts
// the closing fence at the item's continuation column and opens the next
// item below the block: a closing fence at column 0 ends the item instead,
// and the two fences the bytes then hold read back as overlapping blocks
// (record 2026-11-12, seed11/c11).
func (d *Document) siblingFenceClose(loc paragraphLocation, fence string) (closeFence, nextItem string, ok bool) {
	if d.md == nil || loc.item < 0 || fence == "" {
		return "", "", false
	}
	src := d.md.p.String()
	it := d.md.p.Block(loc.item)
	if it.Start < loc.line.start || it.Start >= loc.contentStart || loc.contentStart > len(src) {
		return "", "", false
	}
	marker := src[it.Start:loc.contentStart]
	if nextItem, ok = d.siblingPrefix(loc, marker); !ok {
		return "", "", false
	}
	if list := it.Parent; list > 0 && d.md.p.Block(list).Ordered {
		// The marker the item's own line spells is the number that item
		// already carries: the sibling below the fence continues the list
		// (record 2026-11-12, seed20/c3).
		nextItem = replaceMarkerNumber(nextItem, d.nextOrderedNumber(loc.item, list, it.Number, false))
	}
	return d.continuationPrefix(loc) + fence, nextItem, true
}

// nextOrderedMarker renumbers a sibling prefix built from the item's own
// marker, keeping the column siblingPrefix placed it at.
func (e *Edit) nextOrderedMarker(loc paragraphLocation, marker, base string) string {
	start, end, num, ok := markerNumber(marker)
	if !ok {
		return base
	}
	next := e.nextOrderedNumber(loc.item, loc.list, num)
	return base[:len(base)-len(marker)] + marker[:start] + strconv.Itoa(next) + marker[end:]
}

func (e *Edit) nextOrderedPrefix(loc paragraphLocation) string {
	prefix := loc.prefix
	start, end, num, ok := markerNumber(prefix)
	if !ok {
		return prefix + "1. "
	}
	next := e.nextOrderedNumber(loc.item, loc.list, num)
	return prefix[:start] + strconv.Itoa(next) + prefix[end:]
}

// nextOrderedNumber is the number a new item of item's list writes after an
// item marked num.
func (e *Edit) nextOrderedNumber(item, list, num int) int {
	return e.d.nextOrderedNumber(item, list, num, e.splitMultiline)
}

// nextOrderedNumber is the number a new item of item's list writes after an
// item marked num. splitMultiline is the session's flag for a split inside an
// item that spans several lines.
func (d *Document) nextOrderedNumber(item, list, num int, splitMultiline bool) int {
	// A list whose items all carry the same number keeps it for the new
	// item. One item alone does not show that style unless it came with the
	// opened document: Typora continues the number of an opened list's only
	// item and increments the number of a list typed in this session.
	lazy := false
	items := 0
	if item >= 0 {
		lazy = true
		for i := range d.md.p.Children(d.md.p.Block(item).Parent) {
			items++
			lazy = lazy && d.md.p.Block(i).Number == num
		}
		lazy = lazy && !splitMultiline &&
			(items > 1 || list >= 0 && d.md.openedListAt(d.md.p.Block(list).Start))
	}
	if lazy {
		return num
	}
	return num + 1
}

func (e *Edit) indentList(at, dir int) error {
	// Indenting rewrites only the list markers: the text, and the selection
	// in it, stay as they were. The source edit maps the selection through
	// the bytes it rewrites, which keeps a caret that sat inside a hidden
	// marker pair inside it (record 2026-11-10, seed4/c5); restoring the
	// pre-edit text offsets would re-derive the caret from the visible text
	// and drop back before the closing marker.
	t := e.d.text
	p := t.paraAt(at)
	loc := e.d.paragraphLocation(p)
	if dir < 0 && loc.item < 0 && loc.leaf >= 0 && e.d.ancestorKind(loc.leaf, parser.Quote) >= 0 {
		return e.liftQuote(loc)
	}
	if loc.item < 0 || loc.list < 0 {
		return ErrStructuralEdit
	}
	src := e.d.md.p.String()
	item := e.d.md.p.Block(loc.item)
	width := listIndentWidth(src, e.d.md.p, loc)
	if dir < 0 {
		leading := lineIndent(src, loc.line.start)
		if leading == 0 {
			return e.liftListItem(at, loc)
		}
		// Without a sibling to align to, an outdent steps one indent
		// level up from the item's own marker, as Typora does: a
		// two-level item goes to one level, not to the top.
		target := max(0, leading-width)
		for i := 1; i < e.d.md.p.Block(0).Next; i++ {
			b := e.d.md.p.Block(i)
			if b.Kind != parser.Item || b.Parent != item.Parent || i == loc.item || b.Start >= item.Start {
				continue
			}
			n := lineIndent(src, lineAt(src, b.Start).start)
			if n < leading && n > target {
				target = n
			}
		}
		from := item.Start - (leading - target)
		if from >= item.Start {
			return nil
		}
		if !e.replaceMarkdownSource(from, item.Start, "") {
			return ErrStructuralEdit
		}
		return nil
	}
	oldMarker := src[item.Start:loc.contentStart]
	start, end, _, ordered := markerNumber(oldMarker)
	insert := strings.Repeat(" ", width)
	var oldEnd int
	var text string
	if ordered {
		oldEnd = item.Start + end
		text = insert + oldMarker[:start] + "1"
	} else {
		oldEnd = item.Start
		text = insert
	}
	from := item.Start
	if prev, _ := e.listSiblings(loc.item); prev >= 0 {
		if blank, ok := blankSiblingGap(src, e.d.md.p.Block(prev).End, item.Start); ok {
			// Typora joins the item to the sibling above it: the blank lines
			// that made the list loose go with the indent, so an item the
			// exit left below a separator nests tight (record 2026-11-12,
			// seed15/c10: `- c\n\n- eof界` indents to `- c\n  - eof界`).
			from = blank
			text = defaultEOL(src) + text
		}
	}
	following := e.d.orderedSiblingMarkers(loc)
	if !e.replaceMarkdownSource(from, oldEnd, text) {
		return ErrStructuralEdit
	}
	delta := len(text) - (oldEnd - from)
	for i := len(following) - 1; i >= 0; i-- {
		m := following[i]
		next := m.number - 1
		if next < 1 || next == m.number {
			continue
		}
		if !e.replaceMarkdownSource(m.start+delta, m.end+delta, strconv.Itoa(next)) {
			return ErrStructuralEdit
		}
	}
	if len(following) > 0 {
		if err := e.d.checkSource(); err != nil {
			return err
		}
	}
	return nil
}

// onlyListItem reports whether item is the only item of its list.
func (d *Document) onlyListItem(item int) bool {
	if d.md == nil || item < 0 || item >= d.md.p.Len() {
		return false
	}
	parent := d.md.p.Block(item).Parent
	if d.md.p.Block(parent).Kind != parser.List {
		return false
	}
	only := true
	for child := range d.md.p.Children(parent) {
		if child != item {
			only = false
			break
		}
	}
	return only
}

func (e *Edit) listSiblings(item int) (prev, next int) {
	prev, next = -1, -1
	if item < 0 || item >= e.d.md.p.Len() {
		return -1, -1
	}
	parent := e.d.md.p.Block(item).Parent
	if e.d.md.p.Block(parent).Kind != parser.List {
		return -1, -1
	}
	for child := range e.d.md.p.Children(parent) {
		if child == item {
			continue
		}
		if child < item {
			prev = child
		} else if next < 0 {
			next = child
		}
	}
	return prev, next
}

func (e *Edit) liftListItem(_ int, loc paragraphLocation) error {
	src := e.d.md.p.String()
	item := e.d.md.p.Block(loc.item)
	firstLine := lineAt(src, item.Start)
	last := item.Start
	if item.End > item.Start {
		last = item.End - 1
	}
	if lastLine := lineAt(src, last); lastLine.start > firstLine.start {
		// The item holds more than one source line: its marker leaves the
		// first line and the list indent the lines below, so the item's
		// paragraphs leave the list whole. Lifting only the caret's line
		// would cut them apart.
		return e.liftWholeItem(loc, firstLine, lastLine)
	}
	prev, next := e.listSiblings(loc.item)
	ordered := e.d.md.p.Block(item.Parent).Ordered
	var nextMarker orderedMarker
	hasNextMarker := false
	if next >= 0 && ordered {
		n := e.d.md.p.Block(next)
		line := lineAt(src, n.Start)
		marker := src[n.Start:line.end]
		start, end, number, ok := markerNumber(marker)
		if ok {
			nextMarker = orderedMarker{start: n.Start + start, end: n.Start + end, number: number}
			hasNextMarker = true
		}
	}
	base := src[loc.line.start:item.Start]
	content := src[loc.contentStart:loc.contentEnd]
	blank := ""
	if strings.Contains(base, ">") {
		if i := strings.LastIndex(base, ">"); i >= 0 {
			blank = base[:i+1]
		}
	}
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	var text strings.Builder
	if prev >= 0 {
		text.WriteString(blank)
		text.WriteString(eol)
	}
	text.WriteString(base)
	text.WriteString(content)
	text.WriteString(eol)
	if next >= 0 {
		text.WriteString(blank)
		text.WriteString(eol)
	}
	old := src[loc.line.start:loc.line.stop]
	_, docCaret := e.Selection()
	// The document's own selection is exact: a caret at a paragraph's visible
	// end stands after the inline syntax the projection hides there, while
	// mapping the display offset back would stop in front of it (record
	// 2026-11-13, seed20/c7).
	caretSource := e.d.sourceAtDoc(docCaret)
	rel := max(0, min(caretSource-loc.contentStart, len(content)))
	if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, text.String()) {
		return ErrStructuralEdit
	}
	if hasNextMarker {
		delta := len(text.String()) - len(old)
		if !e.replaceMarkdownSource(nextMarker.start+delta, nextMarker.end+delta, "1") {
			return ErrStructuralEdit
		}
	}
	if e.step != nil {
		caret := loc.line.start + len(base) + rel
		if prev >= 0 && content != "" {
			// The separator line comes first, and the paragraph's content
			// stands on the line below it. An empty paragraph has no line of
			// its own past the separator: it stands on the first line the
			// lift writes, and a caret past the separator would be drawn in
			// the block below it (record 2026-11-13, issue177: an empty item
			// lifted between two items leaves the caret on the empty
			// paragraph, where the next rune is typed).
			caret += len(blank) + len(eol)
		}
		e.selectMarkdownSource(caret)
	}
	return nil
}

// liftWholeItem lifts an item that spans more than one source line out of
// its list: the marker leaves the first line and the list indent the lines
// below, so every paragraph the item holds leaves the list as it stood.
func (e *Edit) liftWholeItem(loc paragraphLocation, firstLine, lastLine markdownLine) error {
	src := e.d.md.p.String()
	item := e.d.md.p.Block(loc.item)
	prev, next := e.listSiblings(loc.item)
	ordered := e.d.md.p.Block(item.Parent).Ordered
	var nextMarker orderedMarker
	hasNextMarker := false
	if next >= 0 && ordered {
		n := e.d.md.p.Block(next)
		line := lineAt(src, n.Start)
		if start, end, number, ok := markerNumber(src[n.Start:line.end]); ok {
			nextMarker = orderedMarker{start: n.Start + start, end: n.Start + end, number: number}
			hasNextMarker = true
		}
	}
	base := src[firstLine.start:item.Start]
	blank := ""
	if strings.Contains(base, ">") {
		if i := strings.LastIndex(base, ">"); i >= 0 {
			blank = base[:i+1]
		}
	}
	eol := firstLine.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	// The first child begins after the item's marker, the task box
	// included; the list indent of the lines below is that marker's width
	// after the container prefix.
	firstContent := firstLine.end
	for child := range e.d.md.p.Children(loc.item) {
		if start := e.d.md.p.Block(child).Start; start >= item.Start {
			firstContent = min(start, firstLine.end)
		}
		break
	}
	marker := src[item.Start:firstContent]
	if i := strings.IndexByte(marker, '['); i >= 0 {
		marker = marker[:i]
	}
	extra := max(0, item.Start-firstLine.start+len(marker)-len(base))

	lines := []markdownLine{firstLine}
	for at := firstLine.stop; at < lastLine.start; {
		ln := lineAt(src, at)
		if ln.start <= lines[len(lines)-1].start {
			break
		}
		lines = append(lines, ln)
		at = ln.stop
	}
	if lines[len(lines)-1].start < lastLine.start {
		lines = append(lines, lastLine)
	}

	_, caret := e.Selection()
	caretSource := e.d.sourceAtDoc(caret)
	newCaret := -1
	var text strings.Builder
	if prev >= 0 {
		text.WriteString(blank)
		text.WriteString(eol)
	}
	for i, ln := range lines {
		lineText := src[ln.start:ln.end]
		start := text.Len()
		switch {
		case i == 0:
			text.WriteString(base)
			text.WriteString(src[firstContent:ln.end])
			if caretSource >= 0 && caretSource <= ln.stop && newCaret < 0 {
				switch {
				case caretSource <= item.Start:
					newCaret = start + caretSource - ln.start
				case caretSource < firstContent:
					newCaret = start + len(base)
				default:
					newCaret = start + len(base) + caretSource - firstContent
				}
			}
		default:
			line, strip := lineText, 0
			if strings.HasPrefix(lineText, base) {
				rest := lineText[len(base):]
				for strip < extra && strip < len(rest) && rest[strip] == ' ' {
					strip++
				}
				line = base + rest[strip:]
			}
			text.WriteString(line)
			if caretSource >= 0 && caretSource <= ln.stop && newCaret < 0 {
				if caretSource <= ln.start {
					newCaret = start
				} else if caretSource < ln.start+len(base)+strip {
					newCaret = start + min(caretSource-ln.start, len(base))
				} else {
					newCaret = start + caretSource - ln.start - strip
				}
			}
		}
		text.WriteString(ln.eol)
	}
	if next >= 0 {
		text.WriteString(blank)
		text.WriteString(eol)
	}
	if !e.replaceMarkdownSource(firstLine.start, lastLine.stop, text.String()) {
		return ErrStructuralEdit
	}
	if hasNextMarker {
		delta := text.Len() - (lastLine.stop - firstLine.start)
		if !e.replaceMarkdownSource(nextMarker.start+delta, nextMarker.end+delta, "1") {
			return ErrStructuralEdit
		}
	}
	if e.step != nil {
		if newCaret < 0 {
			newCaret = firstLine.start
			if prev >= 0 {
				newCaret += len(blank) + len(eol)
			}
			newCaret += len(base)
		}
		newCaret = max(0, min(newCaret, text.Len()))
		e.selectMarkdownSource(firstLine.start + newCaret)
	}
	return nil
}

func (e *Edit) joinListItemBackward(_ int, loc paragraphLocation, next bool) error {
	src := e.d.md.p.String()
	content := src[loc.contentStart:loc.contentEnd]
	cont := e.d.continuationPrefix(loc)
	blank := ""
	if strings.Contains(cont, ">") {
		if i := strings.LastIndex(cont, ">"); i >= 0 {
			blank = cont[:i+1]
		}
	}
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	var text strings.Builder
	text.WriteString(blank)
	text.WriteString(eol)
	text.WriteString(cont)
	text.WriteString(content)
	text.WriteString(eol)
	if next {
		text.WriteString(blank)
		text.WriteString(eol)
	}
	if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, text.String()) {
		return ErrStructuralEdit
	}
	if e.step != nil {
		caret := loc.line.start + len(blank) + len(eol) + len(cont)
		e.selectMarkdownSource(caret)
	}
	return nil
}

// joinEmptyListItemBackward makes an empty item the previous item's second
// paragraph, as Typora's joinBackward does: the marker leaves the line, and
// the line becomes the continuation prefix of the empty paragraph the join
// adds. A list that holds a multi-paragraph item is written loose, so the
// separators around it get a blank line too.
// emptyNestedItemHasOwnLine reports whether the empty item Backspace acts
// on is the only item of a list inside a parent item that holds no paragraph
// of its own, and whether its marker line is a line of its own rather than
// bytes the parent's line carries.
func (e *Edit) emptyNestedItemHasOwnLine(loc paragraphLocation) bool {
	if loc.item < 0 || e.d.md == nil {
		return false
	}
	src := e.d.md.p.String()
	item := e.d.md.p.Block(loc.item)
	list := e.d.md.p.Block(item.Parent)
	if list.Kind != parser.List {
		return false
	}
	parent := e.d.md.p.Block(list.Parent)
	if parent.Kind != parser.Item {
		return false
	}
	if lineAt(src, item.Start).start != loc.line.start || lineAt(src, parent.Start).start == loc.line.start {
		return false
	}
	for child := range e.d.md.p.Children(list.Parent) {
		switch e.d.md.p.Block(child).Kind {
		case parser.Paragraph, parser.Heading:
			// The parent holds a paragraph of its own: the join writes that
			// paragraph's continuation, a shape no recording covers yet.
			return false
		}
	}
	return true
}

// liftEmptyNestedItemIntoParent takes the empty nested item's marker line
// away and writes the parent item's second paragraph in its place: a blank
// line separates the two paragraphs and the item's own line stands at the
// parent's content column. Typora shows that line as the parent item's empty
// paragraph, not as a bare whitespace line (record 2026-11-12, seed15/c11).
func (e *Edit) liftEmptyNestedItemIntoParent(loc paragraphLocation) error {
	src := e.d.md.p.String()
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	// The nested item's marker carries the parent's continuation indent as
	// its leading bytes: the paragraph the join writes stands at that same
	// column, one content column of the parent item.
	leading := src[loc.line.start:e.d.md.p.Block(loc.item).Start]
	text := eol + leading + eol
	if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, text) {
		return ErrStructuralEdit
	}
	if e.step != nil {
		e.selectMarkdownSource(loc.line.start + len(eol) + len(leading))
	}
	return nil
}

func (e *Edit) joinEmptyListItemBackward(loc paragraphLocation, next int) error {
	src := e.d.md.p.String()
	item := e.d.md.p.Block(loc.item)
	prev, _ := e.listSiblings(loc.item)
	cont := e.d.continuationPrefix(loc)
	indent := src[loc.line.start:item.Start]
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	blank := ""
	if strings.Contains(indent, ">") {
		if i := strings.LastIndex(indent, ">"); i >= 0 {
			blank = indent[:i+1]
		}
	}
	// An item of a loose list already follows a blank line; the join only
	// needs the continuation line there. A tight list gets the blank line
	// from the join.
	gap := src[e.d.md.p.Block(prev).End:loc.line.start]
	tight := strings.Count(gap, "\n") < 2 && strings.Count(gap, "\r") < 2

	// The item separators above the previous sibling are written with a
	// blank line once the list is loose. All of them lie before the joined
	// line, so the edits run from the last one backwards and keep the
	// offsets of the earlier ones valid.
	var inserts []int
	parent := item.Parent
	var siblings []int
	for child := range e.d.md.p.Children(parent) {
		siblings = append(siblings, child)
	}
	for i := 0; i+1 < len(siblings); i++ {
		s := siblings[i]
		if s >= prev {
			break
		}
		nextStart := e.d.md.p.Block(siblings[i+1]).Start
		g := src[e.d.md.p.Block(s).End:nextStart]
		if strings.Count(g, "\n") >= 2 || strings.Count(g, "\r") >= 2 {
			continue
		}
		inserts = append(inserts, lineAt(src, nextStart).start)
	}

	// 前一项的段落里带着软换行时，并进来的这一行是它真正的第二段：
	// Enter 在项内开的待定块让 Backspace 把该项变成多段，列表随之变松。
	// 单行项走的是把空项展开进父项的路径，保留紧凑的无 marker 行
	// （录制 2026-11-10, seed8/c2；diff.json fa-3ce2ead0 是展开路径）。
	prevItem := e.d.md.p.Block(prev)
	multiLine := strings.ContainsAny(src[prevItem.Start:prevItem.End], "\r\n")
	if !multiLine {
		// A line ending in a hard break opens a second line the item's own
		// bytes do not spell yet: the join writes the paragraph that line
		// holds, so the item behaves as one with two paragraphs and the
		// list is written loose (record 2026-11-13, seed18/c10).
		last := src[prevItem.Start:prevItem.End]
		if i := strings.LastIndexAny(last, "\r\n"); i >= 0 {
			last = last[i+1:]
		}
		trimmed := strings.TrimRight(last, " ")
		multiLine = strings.HasSuffix(last, "\\") || len(last)-len(trimmed) >= 2
	}

	text := cont
	caret := loc.line.start + len(cont)
	if tight {
		if multiLine {
			// W1：项内两段之间一个空行，空段落在该项的续行列上。
			text = eol + cont + loc.line.eol
			caret = loc.line.start + len(eol) + len(cont)
		} else {
			text = indent + eol + cont + loc.line.eol
			caret = loc.line.start + len(indent) + len(eol) + len(cont)
		}
	}
	// A following item needs the blank line as well: the joined item now
	// holds two paragraphs, which makes the list loose. The insertion point
	// is read from this parse: the edit below reparses the document, and
	// the sibling's index no longer means the block it did.
	nextAt := -1
	if next >= 0 {
		nextLine := lineAt(src, e.d.md.p.Block(next).Start).start
		g := src[item.End:nextLine]
		if strings.Count(g, "\n") < 2 && strings.Count(g, "\r") < 2 {
			nextAt = nextLine + len(text) - (loc.line.stop - loc.line.start)
		}
	}
	var seps []looseListSeparator
	if tight && multiLine {
		// 上一项的段落已是多段（软换行让出一段），并项后它持有的嵌套
		// 列表也变松，逐级补空行（录制 2026-11-10, seed8/c2）。
		seps = e.nestedListSeparators(prev)
	}
	if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, text) {
		return ErrStructuralEdit
	}
	if nextAt >= 0 {
		if !e.replaceMarkdownSource(nextAt, nextAt, blank+eol) {
			return ErrStructuralEdit
		}
	}
	for k := len(inserts) - 1; k >= 0; k-- {
		at := inserts[k]
		if !e.replaceMarkdownSource(at, at, blank+eol) {
			return ErrStructuralEdit
		}
		caret += len(blank) + len(eol)
	}
	for k := len(seps) - 1; k >= 0; k-- {
		s := seps[k]
		if !e.replaceMarkdownSource(s.at, s.at, s.blank+s.eol) {
			return ErrStructuralEdit
		}
		caret += len(s.blank) + len(s.eol)
	}
	if e.step != nil {
		e.selectMarkdownSource(caret)
	}
	return nil
}

// looseListSeparator 是 Typora 序列化器在松列表与它上方、同一列表项内
// 的段落之间写下的空行。
type looseListSeparator struct {
	at    int
	eol   string
	blank string
}

// nestedListSeparators 返回 item 变成多段项后空行要写在哪里。它的列表
// 变松，从上方每级持有该列表的项也一样；每级在段落与下方列表之间用一个
// 空行分隔（W1）。结果按最外层到最内层排列，倒序插入可保持位置有效。
func (e *Edit) nestedListSeparators(item int) []looseListSeparator {
	var levels []looseListSeparator
	src := e.d.md.p.String()
	for item >= 0 {
		list := e.d.md.p.Block(item).Parent
		if list < 0 || e.d.md.p.Block(list).Kind != parser.List {
			break
		}
		parent := e.d.md.p.Block(list).Parent
		if parent < 0 || e.d.md.p.Block(parent).Kind != parser.Item {
			break
		}
		listBlock := e.d.md.p.Block(list)
		at := lineAt(src, listBlock.Start).start
		prev := -1
		for child := range e.d.md.p.Children(parent) {
			if e.d.md.p.Block(child).Start == listBlock.Start {
				break
			}
			prev = child
		}
		if at <= 0 || prev < 0 {
			break
		}
		// 上方项在本次编辑前已经是松的，列表前已经有空行，字节不变。
		gap := src[e.d.md.p.Block(prev).End:at]
		if strings.Count(gap, "\n") >= 2 || strings.Count(gap, "\r") >= 2 {
			break
		}
		line := lineAt(src, at)
		blank := ""
		if indent := src[line.start:listBlock.Start]; strings.Contains(indent, ">") {
			if i := strings.LastIndex(indent, ">"); i >= 0 {
				blank = indent[:i+1]
			}
		}
		eol := line.eol
		if eol == "" {
			eol = defaultEOL(src)
		}
		levels = append([]looseListSeparator{{at: at, eol: eol, blank: blank}}, levels...)
		item = parent
	}
	return levels
}

// joinItemParagraphBackward merges a paragraph that is not its item's first
// paragraph into its previous sibling, as a block-boundary join does: the
// blank line and the continuation indent between the two paragraphs leave the
// source and the caret sits between their texts. Only a paragraph can take
// the text; anything else above the caret leaves the edit unwritable.
func (e *Edit) joinItemParagraphBackward(loc paragraphLocation) error {
	if loc.leaf < 0 {
		return ErrStructuralEdit
	}
	leaf := e.d.md.p.Block(loc.leaf)
	prev := -1
	for child := range e.d.md.p.Children(leaf.Parent) {
		if child == loc.leaf {
			break
		}
		prev = child
	}
	if prev < 0 || e.d.md.p.Block(prev).Kind != parser.Paragraph {
		return ErrStructuralEdit
	}
	from := e.d.md.p.Block(prev).End
	if from > loc.contentStart {
		return ErrStructuralEdit
	}
	if !e.replaceMarkdownSource(from, loc.contentStart, "") {
		return ErrStructuralEdit
	}
	if e.step != nil {
		e.selectMarkdownSource(from)
	}
	return nil
}

func (e *Edit) mergeIntoParentItem(_ int, loc paragraphLocation) error {
	src := e.d.md.p.String()
	item := e.d.md.p.Block(loc.item)
	leading := src[loc.line.start:item.Start]
	content := src[loc.contentStart:loc.contentEnd]
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	_, next := e.listSiblings(loc.item)
	text := leading + content + eol
	caret := loc.line.start + len(leading)
	if next >= 0 {
		// 后面还有同级项：平移下来的文字留在父项段落的续行里，同级项
		// 在自己的分隔行之后（录制 blocks/list-nested/start/backspace：
		// `- parent\n  - alpha\n  - beta\n` 并成 `- parent\n  Xalpha\n  \n  - beta\n`）。
		text += leading + eol
	} else {
		// 平移项是列表里的最后一项：它的文字成为父项的第二段，W1 用一个
		// 空行分隔两段，文字回到父项的内容列，也就是原项 marker 行携带的
		// 缩进（录制 seed9/c9：`- item\n  -  two` 并成 `- item\n\n   two`）。
		text = eol + leading + content + eol
		caret += len(eol)
	}
	if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, text) {
		return ErrStructuralEdit
	}
	if e.step != nil {
		e.selectMarkdownSource(caret)
	}
	return nil
}

func listIndentWidth(src string, d *parser.Document, loc paragraphLocation) int {
	current := d.Block(loc.item)
	base := lineIndent(src, lineAt(src, current.Start).start)
	width := 0
	list := d.Block(loc.list)
	for i := 1; i < d.Block(0).Next; i++ {
		b := d.Block(i)
		if b.Kind != parser.Item || i == loc.item || b.Start < list.Start || b.Start > list.End {
			continue
		}
		n := lineIndent(src, lineAt(src, b.Start).start)
		if n > base && (width == 0 || n-base < width) {
			width = n - base
		}
	}
	if width > 0 {
		return width
	}
	lineEnd := min(current.End, lineAt(src, current.Start).end)
	marker := src[current.Start:lineEnd]
	if marker != "" && marker[0] >= '0' && marker[0] <= '9' {
		for i := 0; i < len(marker); i++ {
			if marker[i] == '.' || marker[i] == ')' {
				return i + 2
			}
		}
		return 3
	}
	return 2
}

func lineIndent(src string, at int) int {
	line := lineAt(src, at)
	i := line.start
	for i < line.end && src[i] == ' ' {
		i++
	}
	return i - line.start
}

// blankSiblingGap reports where the empty separator between two sibling
// items starts, when the bytes from prevEnd to itemStart hold one blank
// line and nothing else. Indenting consumes that separator, as Typora does
// when the item an exit left below it joins the item above.
//
// The separator has to be an empty line: a whitespace-only continuation
// line is part of the item above (`- beta\n  \n    - ` keeps it on every
// indent), and a blank line inside a list that came from the opened file
// stays where it is (record 2026-11-12, seed15/c10 against seed13/c5).
func blankSiblingGap(src string, prevEnd, itemStart int) (int, bool) {
	if prevEnd <= 0 || prevEnd >= itemStart || itemStart > len(src) {
		return 0, false
	}
	gap := strings.ReplaceAll(src[prevEnd:itemStart], "\r\n", "\n")
	if strings.Trim(gap, "\n") != "" || strings.Count(gap, "\n") < 2 {
		return 0, false
	}
	return prevEnd, true
}

func (e *Edit) toggleTask(at int) error {
	t := e.d.text
	p := t.paraAt(at)
	loc := e.d.paragraphLocation(p)
	src := e.d.md.p.String()
	from := loc.line.start
	to := min(loc.line.end, loc.contentStart)
	prefix := src[from:to]
	for _, pair := range [][2]string{{"[ ]", "[x]"}, {"[x]", "[ ]"}, {"[X]", "[ ]"}} {
		if i := strings.Index(prefix, pair[0]); i >= 0 {
			off := from + i
			repl := pair[1]
			if pair[0] == "[ ]" {
				repl = "[x]"
			}
			if !e.replaceMarkdownSource(off, off+3, repl) {
				return ErrStructuralEdit
			}
			return nil
		}
	}
	return ErrStructuralEdit
}
