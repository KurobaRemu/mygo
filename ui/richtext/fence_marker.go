package richtext

import "github.com/egoist/mygo/ui/markdown/parser"

// snapFenceMarkerOffsetLocked maps an offset on a fenced code block's
// closing marker line to the end of the block's last code line. The rich
// text view hides the marker: Typora holds no caret on it (source carets at
// 12 and at 15 in "```go\nalpha\n```\n" snap to 11), and typing at the
// drawn end of the block appends to its last code line. A block with no
// code lines keeps the offset: its marker is the block's empty line. A
// transient preview holds its own offsets: the empty paragraph an exit opens
// after a fence anchors on the marker's end (record 2026-11-11,
// probe92b/down-escape: the paragraph sits at 15:15, not at the code's 11).
func (d *Document) snapFenceMarkerOffsetLocked(off int) int {
	if d.md == nil || d.md.keepFinalEOLCaret || off <= 0 {
		return off
	}
	p := d.md.p
	src := p.String()
	if off > len(src) {
		return off
	}
	line := lineAt(src, off)
	if off < line.start || off > line.end {
		return off
	}
	for i := 1; i < p.Block(0).Next; i++ {
		b := p.Block(i)
		if b.Kind != parser.CodeBlock || !b.Fenced || b.Open || b.Start >= line.start || b.End < line.start || p.LineCount(i) == 0 {
			continue
		}
		end := min(b.End, len(src))
		if end > 0 && src[end-1] == '\n' {
			end--
			if end > 0 && src[end-1] == '\r' {
				end--
			}
		}
		for end > 0 && src[end-1] != '\n' && src[end-1] != '\r' {
			end--
		}
		if end != line.start {
			continue
		}
		at := line.start
		if src[at-1] == '\n' {
			at--
			if at > 0 && src[at-1] == '\r' {
				at--
			}
		}
		return at
	}
	return off
}
