package main

import "testing"

func TestOccupiedRow(t *testing.T) {
	cases := []struct {
		width, columns, want int
	}{
		{0, 80, 0},
		{1, 80, 0},
		{80, 80, 0},
		{81, 80, 1},
		{160, 80, 1},
		{161, 80, 2},
	}
	for _, tc := range cases {
		if got := occupiedRow(tc.width, tc.columns); got != tc.want {
			t.Errorf("occupiedRow(%d, %d)=%d want %d", tc.width, tc.columns, got, tc.want)
		}
	}
}

func TestAnsiDisplayWidth(t *testing.T) {
	cases := map[string]int{
		"":                       0,
		"abc":                    3,
		"\033[1mabc\033[0m":      3,
		"\033[38;5;81mxy\033[0m": 2,
		"日本語":                     3,
	}
	for input, want := range cases {
		if got := ansiDisplayWidth(input); got != want {
			t.Errorf("ansiDisplayWidth(%q)=%d want %d", input, got, want)
		}
	}
}

func TestMultilineCursorPosition(t *testing.T) {
	// Short single line: 3 chars after a width-2 prompt.
	row, col := multilineCursorPosition(2, []rune("abc"), 2, 80)
	if row != 0 || col != 5 {
		t.Fatalf("got (%d,%d) want (0,5)", row, col)
	}
	// A newline advances one row and resets to the continuation width.
	row, col = multilineCursorPosition(0, []rune("ab\ncd"), 2, 80)
	if row != 1 || col != 4 {
		t.Fatalf("got (%d,%d) want (1,4)", row, col)
	}
}

func TestEditableLineMoveUpDown(t *testing.T) {
	// Three logical lines "ab\ncdef\nghi": \n@2, \n@7, so line two "cdef" spans 3-6.
	line := editableLine{text: []rune("ab\ncdef\nghi"), cursor: 5} // column 2 of line two
	if !line.moveUp() {
		t.Fatal("moveUp returned false")
	}
	// Column 2 exceeds line one "ab" (length 2), so it clamps to end-of-line index 2.
	if line.cursor != 2 {
		t.Errorf("after moveUp cursor=%d want 2", line.cursor)
	}
	// Round-trip back down to the same column on line two.
	if !line.moveDown() {
		t.Fatal("moveDown returned false")
	}
	if line.cursor != 5 {
		t.Errorf("after moveDown cursor=%d want 5", line.cursor)
	}

	// At the first line, moveUp yields false so history recall can take over.
	line.cursor = 0
	if line.moveUp() {
		t.Error("moveUp at top should return false")
	}
	// At the last line, moveDown yields false.
	line.cursor = len(line.text)
	if line.moveDown() {
		t.Error("moveDown at bottom should return false")
	}

	// Empty single-line buffer: both return false.
	empty := editableLine{}
	if empty.moveUp() || empty.moveDown() {
		t.Error("single-line buffer should not move vertically")
	}
}

func TestEditableLineExtent(t *testing.T) {
	line := editableLine{text: []rune("ab\ncdef\nghi"), cursor: 6}
	start, end, index := line.lineExtent()
	if start != 3 || end != 7 || index != 1 {
		t.Errorf("lineExtent=(%d,%d,%d) want (3,7,1)", start, end, index)
	}
}
