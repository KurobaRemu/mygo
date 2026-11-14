package parser

import (
	"slices"
	"testing"
)

// The session's live lines are line-break offsets: text inserted at a
// break's own offset joins the line before it, so the break moves with the
// bytes it ends.
func TestLiveLinesFollowTheBreaksTheyName(t *testing.T) {
	d := New(Typora)
	d.Set("\nga")
	d.SetLiveLine(0)
	if got := d.LiveLines(); !slices.Equal(got, []int{0}) {
		t.Fatalf("live lines = %v, want [0]", got)
	}

	d.Edit(0, 0, "x")
	if got := d.Source(); got != "x\nga" {
		t.Fatalf("source = %q", got)
	}
	if got := d.LiveLines(); !slices.Equal(got, []int{1}) {
		t.Fatalf("live lines after text before the break = %v, want [1]", got)
	}

	d.Edit(2, 2, "y")
	if got := d.Source(); got != "x\nyga" {
		t.Fatalf("source = %q", got)
	}
	if got := d.LiveLines(); !slices.Equal(got, []int{1}) {
		t.Fatalf("live lines after text after the break = %v, want [1]", got)
	}

	// Replacing the break's bytes takes the fact with them, and one
	// inserted elsewhere is not a break the session typed.
	d.Edit(1, 2, "z")
	if got := d.Source(); got != "xzyga" {
		t.Fatalf("source = %q", got)
	}
	if got := d.LiveLines(); len(got) != 0 {
		t.Fatalf("live lines after replacing the break = %v, want none", got)
	}
}

func TestSetLiveLinesSortsAndCopies(t *testing.T) {
	d := New(Typora)
	d.Set("a\nb\nc")
	d.SetLiveLines([]int{3, 1, 3})
	if got := d.LiveLines(); !slices.Equal(got, []int{1, 3}) {
		t.Fatalf("live lines = %v, want [1 3]", got)
	}
	got := d.LiveLines()
	got[0] = 99
	if got := d.LiveLines(); !slices.Equal(got, []int{1, 3}) {
		t.Fatalf("LiveLines aliased the document: %v", got)
	}
	d.ClearLiveLineAt(1)
	if got := d.LiveLines(); !slices.Equal(got, []int{3}) {
		t.Fatalf("live lines after clearing = %v, want [3]", got)
	}
	d.ClearLiveLineAt(3)
	if got := d.LiveLines(); len(got) != 0 {
		t.Fatalf("live lines after clearing = %v, want none", got)
	}
}

func TestShiftLiveLinesMapsInsertions(t *testing.T) {
	for _, c := range []struct {
		name     string
		lines    []int
		from, to int
		delta    int
		want     []int
	}{
		{"insertion at the break", []int{0}, 0, 0, 2, []int{2}},
		{"insertion before the break", []int{2}, 1, 1, 3, []int{5}},
		{"insertion after the break", []int{1}, 2, 2, 3, []int{1}},
		{"replacement before the break", []int{3}, 0, 2, -1, []int{2}},
		{"replacement over the break", []int{1}, 0, 2, 1, nil},
		{"replacement after the break", []int{0}, 1, 2, 1, []int{0}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ShiftLiveLines(slices.Clone(c.lines), c.from, c.to, c.delta)
			if !slices.Equal(got, c.want) {
				t.Fatalf("ShiftLiveLines(%v, %d, %d, %d) = %v, want %v",
					c.lines, c.from, c.to, c.delta, got, c.want)
			}
		})
	}
}
