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
