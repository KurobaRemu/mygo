package richtext

import (
	"slices"
	"testing"

	"github.com/egoist/mygo/ui/markdown/parser"
)

func TestSessionStateCloneIsolatesEveryTable(t *testing.T) {
	base := fullSessionState()
	clone := base.clone()

	clone.literals[0].Line = 99
	clone.liveBreaks[0] = 99
	clone.heldBreaks[0] = 99
	clone.liveClosers[0].from = 99
	clone.splitMarkers[0].lineStart = 99
	clone.openedLists[0] = 99
	clone.openedNested[0] = 99

	if !slices.Equal(base.literals, fullSessionState().literals) ||
		!slices.Equal(base.liveBreaks, fullSessionState().liveBreaks) ||
		!slices.Equal(base.heldBreaks, fullSessionState().heldBreaks) ||
		!slices.Equal(base.liveClosers, fullSessionState().liveClosers) ||
		!slices.Equal(base.splitMarkers, fullSessionState().splitMarkers) ||
		!slices.Equal(base.openedLists, fullSessionState().openedLists) ||
		!slices.Equal(base.openedNested, fullSessionState().openedNested) {
		t.Fatalf("clone mutated its source: %+v", base)
	}
}

func TestSessionStateMapPatchMovesAllOffsetTables(t *testing.T) {
	base := fullSessionState()
	src := "0123456789abcdefghij"
	nextSrc := "0123456789XXXabcdefghij"
	got := base.mapPatch(10, 10, 3, src, nextSrc)

	want := sessionState{
		literals:     []parser.LiteralLine{{Line: 2, Content: 5}, {Line: 15, Content: 17}},
		liveBreaks:   []int{3, 13},
		heldBreaks:   []int{3, 13},
		liveClosers:  []liveCloser{{2, 5}, {13, 16}},
		splitMarkers: []splitMarker{{lineStart: 13, outerEnd: 15, innerEnd: 16, line: "def"}},
		openedLists:  []int{3},
		openedNested: []int{2, 12},
	}
	if !slices.Equal(got.literals, want.literals) ||
		!slices.Equal(got.liveBreaks, want.liveBreaks) ||
		!slices.Equal(got.heldBreaks, want.heldBreaks) ||
		!slices.Equal(got.liveClosers, want.liveClosers) ||
		!slices.Equal(got.splitMarkers, want.splitMarkers) ||
		!slices.Equal(got.openedLists, want.openedLists) ||
		!slices.Equal(got.openedNested, want.openedNested) {
		t.Fatalf("mapPatch = %+v, want %+v", got, want)
	}
	if !slices.Equal(base.literals, fullSessionState().literals) ||
		!slices.Equal(base.openedLists, fullSessionState().openedLists) ||
		!slices.Equal(base.openedNested, fullSessionState().openedNested) {
		t.Fatalf("mapPatch mutated its source: %+v", base)
	}
}

func TestSessionStateMapPatchDropsOverwrittenOffsets(t *testing.T) {
	base := fullSessionState()
	src := "0123456789abcdefghij"
	nextSrc := "01234567"
	got := base.mapPatch(8, 14, 0, src, nextSrc)

	want := sessionState{
		literals:     []parser.LiteralLine{{Line: 2, Content: 5}},
		liveBreaks:   []int{3},
		heldBreaks:   []int{3},
		liveClosers:  []liveCloser{{2, 5}},
		openedLists:  []int{3},
		openedNested: []int{2, 12},
	}
	if !slices.Equal(got.literals, want.literals) ||
		!slices.Equal(got.liveBreaks, want.liveBreaks) ||
		!slices.Equal(got.heldBreaks, want.heldBreaks) ||
		!slices.Equal(got.liveClosers, want.liveClosers) ||
		!slices.Equal(got.splitMarkers, want.splitMarkers) ||
		!slices.Equal(got.openedLists, want.openedLists) ||
		!slices.Equal(got.openedNested, want.openedNested) {
		t.Fatalf("mapPatch = %+v, want %+v", got, want)
	}
}

func TestSessionStateUndoRedoRestoresFullSession(t *testing.T) {
	d := NewMarkdownDocument("1. alpha ")
	before := d.state()
	before.heldBreaks = []int{1}
	before.liveClosers = []liveCloser{{2, 4}}
	before.splitMarkers = []splitMarker{{lineStart: 2, outerEnd: 3, innerEnd: 4, line: "pha"}}
	d.setState(before)
	wantBefore := d.state()

	d.Edit(func(e *Edit) {
		e.ReplaceSource(6, 6, "beta ")
		e.d.setState(e.d.state().addLiveCloser(8, 10))
		e.d.setState(e.d.state().addSplitMarker(splitMarker{lineStart: 8, outerEnd: 9, innerEnd: 10, line: "eta"}))
	})
	if err := d.CheckSource(); err != nil {
		t.Fatalf("edit left a stale projection: %v", err)
	}
	after := d.state()
	if !after.openedListAt(0) {
		t.Fatal("edit lost the opened list annotation")
	}
	if !slices.Contains(after.liveClosers, liveCloser{8, 10}) {
		t.Fatalf("edit lost live closers: %+v", after.liveClosers)
	}
	if !slices.Contains(after.splitMarkers, splitMarker{lineStart: 8, outerEnd: 9, innerEnd: 10, line: "eta"}) {
		t.Fatalf("edit lost split markers: %+v", after.splitMarkers)
	}

	d.Undo()
	gotBefore := d.state()
	if !slices.Equal(gotBefore.heldBreaks, wantBefore.heldBreaks) ||
		!slices.Equal(gotBefore.liveClosers, wantBefore.liveClosers) ||
		!slices.Equal(gotBefore.splitMarkers, wantBefore.splitMarkers) ||
		!slices.Equal(gotBefore.openedLists, wantBefore.openedLists) ||
		!slices.Equal(gotBefore.openedNested, wantBefore.openedNested) {
		t.Fatalf("undo session = %+v, want %+v", gotBefore, wantBefore)
	}
	if err := d.CheckSource(); err != nil {
		t.Fatalf("undo left a stale projection: %v", err)
	}
	d.Redo()
	gotAfter := d.state()
	if !slices.Equal(gotAfter.heldBreaks, after.heldBreaks) ||
		!slices.Equal(gotAfter.liveClosers, after.liveClosers) ||
		!slices.Equal(gotAfter.splitMarkers, after.splitMarkers) ||
		!slices.Equal(gotAfter.openedLists, after.openedLists) ||
		!slices.Equal(gotAfter.openedNested, after.openedNested) {
		t.Fatalf("redo session = %+v, want %+v", gotAfter, after)
	}
	if err := d.CheckSource(); err != nil {
		t.Fatalf("redo left a stale projection: %v", err)
	}
}

func fullSessionState() sessionState {
	return sessionState{
		literals:     []parser.LiteralLine{{Line: 2, Content: 5}, {Line: 12, Content: 14}},
		liveBreaks:   []int{3, 10},
		heldBreaks:   []int{3, 10},
		liveClosers:  []liveCloser{{2, 5}, {10, 13}},
		splitMarkers: []splitMarker{{lineStart: 10, outerEnd: 12, innerEnd: 13, line: "def"}},
		openedLists:  []int{3, 10},
		openedNested: []int{2, 12},
	}
}
