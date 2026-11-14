package editor

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type markdownListLine struct {
	indent    string
	quote     string
	pad       string
	marker    string
	after     string
	end       int
	number    int
	delimiter byte
	ordered   bool
	task      bool
	list      bool
}

func parseMarkdownListLine(s string) (markdownListLine, bool) {
	var p markdownListLine
	n := len(s)
	i := 0
	for i < n && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	p.indent = s[:i]
	q := i
	for i < n && s[i] == '>' {
		i++
		if i < n && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	p.quote = s[q:i]
	q = i
	for i < n && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	p.pad = s[q:i]
	if i >= n {
		p.end = i
		return p, p.quote != ""
	}
	switch {
	case strings.ContainsRune("*+-", rune(s[i])) && i+1 < n && markdownSpace(s[i+1]):
		start := i
		i++
		gapStart := i
		for i < n && markdownSpace(s[i]) {
			i++
		}
		gap := s[gapStart:i]
		if gap != "" && i+2 < n && s[i] == '[' && (s[i+1] == ' ' || s[i+1] == 'x' || s[i+1] == 'X') && s[i+2] == ']' && i+3 < n && markdownSpace(s[i+3]) {
			i += 3
			p.task = true
		}
		p.marker = s[start:i]
		p.list = true
	case s[i] >= '0' && s[i] <= '9':
		start := i
		for i < n && i-start < 9 && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i >= n || s[i] != '.' && s[i] != ')' || i+1 >= n || !markdownSpace(s[i+1]) {
			return p, p.quote != ""
		}
		p.number, _ = strconv.Atoi(s[start:i])
		p.delimiter = s[i]
		i++
		p.marker = s[start:i]
		p.ordered = true
		p.list = true
	default:
		p.end = i
		return p, p.quote != ""
	}
	q = i
	for i < n && markdownSpace(s[i]) {
		i++
	}
	p.after = s[q:i]
	p.end = i
	return p, true
}

func markdownSpace(c byte) bool {
	return c == ' ' || c == '\t'
}

func (p markdownListLine) listIndent() string {
	if p.quote != "" {
		return p.pad
	}
	return p.indent
}

func (p markdownListLine) markerStart() int {
	return len(p.indent) + len(p.quote) + len(p.pad)
}

func (ed *editor) markdown() bool {
	l := ed.language()
	return l != nil && l.Name == "Markdown"
}

func (ed *editor) markdownFenced(line int) bool {
	h, ok := ed.highlighter().(*tokenHighlighter)
	if ok {
		if state, final := h.stateOf(line); final {
			return state&mdFence != 0
		}
	}
	l := ed.language()
	if l == nil || l.Tokenizer == nil {
		return false
	}
	var state uint64
	r := &ed.view.text
	for i := 0; i < line; i++ {
		_, state = l.Tokenizer.Tokenize(r.Line(i), state, nil)
	}
	return state&mdFence != 0
}

var (
	mdListItem  = regexp.MustCompile(`^(\s*)(>[> ]*|[*+-] \[[x ]\]\s|[*+-]\s|(\d+)([.)]))(\s*)`)
	mdEmptyItem = regexp.MustCompile(`^(\s*)(>[> ]*|[*+-] \[[x ]\]|[*+-]|(\d+)[.)])(\s*)$`)
	mdBullet    = regexp.MustCompile(`[*+-]\s`)
	mdMarkerAt0 = regexp.MustCompile(`^\s*([*+>-]|\d+\.)\s`)
	mdQuoteEnd  = regexp.MustCompile(`>\s*$`)
)

func (ed *editor) markdownNewline() bool {
	if !ed.markdown() {
		return false
	}
	r := &ed.view.text
	var edits []Edit
	for _, s := range ed.sels {
		if !s.empty() {
			return false
		}
		line := r.LineOf(s.head)
		a := r.LineStart(line)
		text := r.Line(line)
		m := mdListItem.FindStringSubmatch(text)
		if s.head == a && mdMarkerAt0.MatchString(text) || m == nil || ed.markdownFenced(line) {
			return false
		}
		if p, ok := parseMarkdownListLine(text); !ok || !ed.markdownListActive(line, p) {
			return false
		}
		if mdEmptyItem.MatchString(text) {
			if mdQuoteEnd.MatchString(text) {
				edits = append(edits, Edit{Start: s.head, End: s.head, Text: "\n"})
			} else {
				edits = append(edits, Edit{Start: a, End: min(s.head+1, a+len(text)), Text: "\n"})
			}
			continue
		}
		marker := m[2]
		if !mdBullet.MatchString(m[2]) && !strings.Contains(m[2], ">") {
			n, _ := strconv.Atoi(m[3])
			marker = strconv.Itoa(n+1) + m[4]
		} else {
			marker = strings.Replace(marker, "x", " ", 1)
		}
		edits = append(edits, Edit{Start: s.head, End: s.head, Text: "\n" + m[1] + marker + m[5]})
	}
	sels := slices.Clone(ed.sels)
	return ed.apply(edits, editOther, func(move func(int, bias) int) []sel {
		for i := range sels {
			sels[i].collapse(move(sels[i].head, biasRight))
		}
		return sels
	})
}

func (ed *editor) markdownTab() bool {
	if !ed.markdown() {
		return false
	}
	for _, s := range ed.sels {
		if !s.empty() {
			return ed.indentLines(1)
		}
	}
	return ed.replaceEach(editTyping, false, func(int, sel) string { return "\t" })
}

func (ed *editor) markdownOutdent() bool {
	if !ed.markdown() {
		return false
	}
	r := &ed.view.text
	var edits []Edit
	for _, sp := range ed.selLines() {
		for line := sp.from; line <= sp.to; line++ {
			s := r.Line(line)
			lead := len(s) - len(strings.TrimLeft(s, " \t"))
			if lead == 0 {
				continue
			}
			remove := 0
			switch {
			case s[0] == '\t':
				remove = 1
			case strings.HasPrefix(s, "  "):
				remove = 2
			case strings.HasPrefix(s, " \t"):
				remove = 2
			case s[0] == ' ':
				remove = 1
			}
			a := r.LineStart(line)
			edits = append(edits, Edit{Start: a, End: a + remove})
		}
	}
	sels := slices.Clone(ed.sels)
	return ed.apply(edits, editOther, func(move func(int, bias) int) []sel {
		for i := range sels {
			sels[i].anchor, sels[i].head = move(sels[i].anchor, biasRight), move(sels[i].head, biasRight)
		}
		return sels
	})
}

func (ed *editor) markdownListActive(line int, p markdownListLine) bool {
	if !p.list {
		return p.quote != ""
	}
	if indentWidth(p.listIndent(), ed.tabSize()) < 4 {
		return true
	}
	_, _, ok := ed.markdownParent(line, p, false)
	return ok
}

func (ed *editor) markdownParent(line int, p markdownListLine, strict bool) (markdownListLine, int, bool) {
	r := &ed.view.text
	width := indentWidth(p.listIndent(), ed.tabSize())
	for i := line - 1; i >= 0; i-- {
		q, ok := parseMarkdownListLine(r.Line(i))
		if !ok || !q.list || q.quote != p.quote || (p.quote != "" && q.indent != p.indent) {
			continue
		}
		w := indentWidth(q.listIndent(), ed.tabSize())
		if w < width || !strict && w == width {
			return q, i, true
		}
	}
	return markdownListLine{}, 0, false
}
