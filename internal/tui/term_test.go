package tui

import (
	"fmt"
	"slices"
	"testing"
)

func TestRingKeepsTheLastLines(t *testing.T) {
	r := &Ring{}
	for i := range ringSize + 5 {
		fmt.Fprintf(r, "line %d\n", i)
	}
	fmt.Fprint(r, "a\nb\n")
	if got := r.Last(3); !slices.Equal(got, []string{fmt.Sprintf("line %d", ringSize+4), "a", "b"}) {
		t.Fatalf("last 3 = %q", got)
	}
	if n := len(r.Last(100)); n != ringSize {
		t.Fatalf("kept %d lines", n)
	}
}
