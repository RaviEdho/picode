package main

import "testing"

func TestSplitMarkdownTableRow(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string
	}{
		{"simple", "| a | b |", []string{"a", "b"}},
		{"no leading pipe", "a | b", []string{"a", "b"}},
		{"single column", "| only |", []string{"only"}},
		{"escaped pipe", `| a\|c | b |`, []string{"a|c", "b"}},
		{"no pipe returns false", "plain text", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := splitMarkdownTableRow(tc.line)
			if tc.want == nil {
				if ok {
					t.Fatalf("got ok=true cells=%v, want false", got)
				}
				return
			}
			if !ok {
				t.Fatalf("got ok=false, want true")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("cell %d: got=%q want=%q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSplitMarkdownTableRowBacktickGuardsPipes(t *testing.T) {
	cells, ok := splitMarkdownTableRow("| `code|with|pipe` | b |")
	if !ok {
		t.Fatal("want ok=true")
	}
	if len(cells) != 2 || cells[0] != "`code|with|pipe`" || cells[1] != "b" {
		t.Fatalf("got=%v", cells)
	}
}

func TestParseMarkdownTable(t *testing.T) {
	lines := []string{
		"| Name | Age |",
		"| :--- | ---: |",
		"| Ada  | 36  |",
	}
	table, used, ok := parseMarkdownTable(lines)
	if !ok {
		t.Fatal("want table recognized")
	}
	if used != 3 {
		t.Fatalf("used=%d want 3", used)
	}
	if len(table.headers) != 2 || table.headers[0] != "Name" || table.headers[1] != "Age" {
		t.Fatalf("headers=%v", table.headers)
	}
	if len(table.rows) != 1 || table.rows[0][0] != "Ada" || table.rows[0][1] != "36" {
		t.Fatalf("rows=%v", table.rows)
	}
	if table.align[0] != markdownTableLeft || table.align[1] != markdownTableRight {
		t.Fatalf("align=%v", table.align)
	}
}

func TestParseMarkdownTableRequiresDelimiter(t *testing.T) {
	// A header-like line followed by ordinary rows must not be parsed as a table.
	lines := []string{"| Name | Age |", "| Ada | 36 |"}
	if _, _, ok := parseMarkdownTable(lines); ok {
		t.Fatal("want table rejected without delimiter row")
	}
}

func TestIsMarkdownTableDelimiter(t *testing.T) {
	cases := map[string]bool{
		"| --- | --- |":    true,
		"| :---: | ---: |": true,
		"| --- |":          true,
		"| a | b |":        false,
		"| :-- | --: |":    false, // requires >=3 dashes after trimming colons
	}
	for line, want := range cases {
		if got := isMarkdownTableDelimiter(line); got != want {
			t.Errorf("isMarkdownTableDelimiter(%q)=%v want %v", line, got, want)
		}
	}
}
