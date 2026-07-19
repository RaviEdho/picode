package main

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

// ANSI styles used by the terminal renderer.  256-colour foregrounds are
// supported by current terminals and gracefully fall back to a readable
// colour on older ones.
const (
	ansiReset     = "\033[0m"
	ansiBold      = "\033[1m"
	ansiItalic    = "\033[3m"
	ansiDim       = "\033[2m"
	ansiCyan      = "\033[38;5;81m"
	ansiBlue      = "\033[38;5;111m"
	ansiGreen     = "\033[38;5;114m"
	ansiYellow    = "\033[38;5;221m"
	ansiMagenta   = "\033[38;5;177m"
	ansiRed       = "\033[38;5;203m"
	ansiCode      = "\033[48;5;236m\033[38;5;252m"
	ansiCodeBlock = "\033[38;5;151m"
)

// renderMarkdown renders the useful, unambiguous subset of Markdown without
// depending on a pager or a third-party parser. It is intentionally block
// oriented so tables can be laid out as a unit while ordinary responses remain
// pleasant in a narrow terminal.
func renderMarkdown(out io.Writer, markdown string) {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	inFence := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if i == len(lines)-1 && line == "" {
			break
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			if inFence {
				fmt.Fprintf(out, "%s  %s%s", ansiDim, strings.TrimSpace(strings.TrimLeft(trimmed, "`~")), ansiReset)
			}
			fmt.Fprintln(out)
			continue
		}
		if inFence {
			fmt.Fprintf(out, "  %s%s%s\n", ansiCodeBlock, sanitizeTerminalText(line), ansiReset)
			continue
		}
		if trimmed == "" {
			fmt.Fprintln(out)
			continue
		}
		if table, rows, ok := parseMarkdownTable(lines[i:]); ok {
			renderMarkdownTable(out, table)
			i += rows - 1
			continue
		}

		renderMarkdownLine(out, line)
	}
}

func renderMarkdownLine(out io.Writer, line string) {
	// A streaming newline can arrive as its own delta. Keep it explicit here;
	// renderMarkdown intentionally drops the synthetic trailing empty element
	// produced by strings.Split, but a live renderer must not drop a real line.
	if line == "" {
		fmt.Fprintln(out)
		return
	}
	for _, listLine := range splitInlineOrderedList(line) {
		renderMarkdownTextLine(out, listLine)
	}
}

func renderMarkdownTextLine(out io.Writer, line string) {
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	content := strings.TrimLeft(line, " \t")
	if level, heading := markdownHeading(content); level > 0 {
		fmt.Fprintf(out, "%s%s\n", ansiBlue, heading)
		return
	}
	if bullet, rest, ok := markdownBullet(content); ok {
		fmt.Fprintf(out, "%s%s%s%s %s\n", indent, ansiYellow, bullet, ansiReset, renderInlineMarkdown(rest))
		return
	}
	if quote, ok := strings.CutPrefix(content, "> "); ok {
		fmt.Fprintf(out, "%s%s│%s %s\n", indent, ansiMagenta, ansiReset, renderInlineMarkdown(quote))
		return
	}
	fmt.Fprintf(out, "%s%s\n", indent, renderInlineMarkdown(content))
}

// streamingMarkdown renders ordinary text as soon as it arrives and buffers
// only an incomplete Markdown construct, table, or block prefix. Complete
// lines use the same formatter as restored history.
type streamingMarkdown struct {
	pending    string
	inFence    bool
	tableLines []string
}

func (s *streamingMarkdown) write(out io.Writer, delta string, final bool) {
	s.pending += delta
	for {
		if s.inFence {
			newline := strings.IndexByte(s.pending, '\n')
			if newline < 0 {
				break
			}
			s.writeLine(out, s.pending[:newline])
			s.pending = s.pending[newline+1:]
			continue
		}

		newline := strings.IndexByte(s.pending, '\n')
		if newline >= 0 {
			s.writeLine(out, s.pending[:newline])
			s.pending = s.pending[newline+1:]
			continue
		}
		// A completed table is held until the next block boundary. If the next
		// chunk is ordinary text without a pipe, flush the table first so that
		// delayed table output cannot appear after that text.
		if len(s.tableLines) > 0 && s.pending != "" && !isPotentialMarkdownTableRow(s.pending) {
			s.flushTable(out)
		}
		if markdownBlockPrefix(s.pending) {
			break
		}
		if !s.flushInline(out) {
			break
		}
		if s.pending == "" {
			break
		}
	}
	if final && s.pending != "" {
		if s.inFence || markdownBlockPrefix(s.pending) {
			s.writeLine(out, s.pending)
		} else {
			s.flushInline(out)
			if s.pending != "" {
				fmt.Fprint(out, sanitizeTerminalText(s.pending))
			}
		}
		s.pending = ""
	}
	if final {
		s.flushTable(out)
	}
}

// flushInline consumes text that is safe to display. It returns false when
// the remaining text starts an incomplete span.
func (s *streamingMarkdown) flushInline(out io.Writer) bool {
	for s.pending != "" {
		marker, index := nextMarkdownMarker(s.pending)
		if index < 0 {
			fmt.Fprint(out, sanitizeTerminalText(s.pending))
			s.pending = ""
			return true
		}
		if index > 0 {
			fmt.Fprint(out, sanitizeTerminalText(s.pending[:index]))
			s.pending = s.pending[index:]
			continue
		}

		end := -1
		if marker == "[" {
			closeLabel := strings.Index(s.pending, "](")
			if closeLabel >= 0 {
				if closeURL := strings.IndexByte(s.pending[closeLabel+2:], ')'); closeURL >= 0 {
					end = closeLabel + 2 + closeURL + 1
				}
			}
		} else if close := strings.Index(s.pending[len(marker):], marker); close >= 0 {
			end = len(marker) + close + len(marker)
		}
		if end < 0 {
			return false
		}
		fmt.Fprint(out, renderInlineMarkdown(s.pending[:end]))
		s.pending = s.pending[end:]
	}
	return true
}

func markdownBlockPrefix(value string) bool {
	// Indentation can arrive in a separate stream delta from the list marker.
	// Keep it pending so nested items are not emitted without their nesting
	// level before the following delta arrives.
	if value != "" && strings.Trim(value, " \t") == "" {
		return true
	}
	if isPotentialMarkdownTableRow(value) {
		return true
	}
	content := strings.TrimLeft(value, " \t")
	if content == "-" || content == "*" || content == "+" || content == ">" || content == "`" || content == "``" || content == "~" || content == "~~" {
		return true
	}
	if markdownOrderedListPrefix(content) {
		return true
	}
	// Unlike the incomplete-prefix check above, a complete ordered marker
	// (including an indented one) must also remain buffered until the newline.
	// Otherwise streaming "  1. item" is flushed as plain text before the
	// block renderer can preserve its nesting and number.
	if _, ok := orderedListMarkerEnd(content, 0); ok {
		return true
	}
	// Keep every ATX heading buffered until its newline. The previous
	// single-character check only covered "# title"; for "## title" and
	// "### title" the stream could flush the hashes as ordinary text before
	// writeLine got a chance to render the heading.
	heading := content
	if strings.HasPrefix(heading, "#") {
		end := 0
		for end < len(heading) && heading[end] == '#' {
			end++
		}
		if end == len(heading) || (end > 0 && heading[end] == ' ') {
			return true
		}
	}
	if strings.HasPrefix(content, "```") || strings.HasPrefix(content, "~~~") || strings.HasPrefix(content, "> ") {
		return true
	}
	if len(content) >= 2 && (content[0] == '#' || content[0] == '-' || content[0] == '*' || content[0] == '+') && content[1] == ' ' {
		return true
	}
	return false
}

// markdownOrderedListPrefix keeps an ordered-list line buffered until its
// newline arrives. Stream chunks commonly split after "1." or "1. "; if the
// prefix is flushed as ordinary inline text, the line renderer cannot format
// the list item afterward.
func markdownOrderedListPrefix(value string) bool {
	value = strings.TrimLeft(value, " \t")
	if value == "" {
		return false
	}
	i := 0
	for i < len(value) && value[i] >= '0' && value[i] <= '9' {
		i++
	}
	if i == 0 {
		return false
	}
	if i == len(value) {
		return true
	}
	return value[i] == '.' && (i+1 == len(value) || value[i+1] == ' ')
}

func (s *streamingMarkdown) writeLine(out io.Writer, line string) {
	trimmed := strings.TrimSpace(line)
	isFence := strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
	if isFence {
		s.flushTable(out)
		if !s.inFence {
			fmt.Fprintf(out, "%s  %s%s\n", ansiDim, strings.TrimSpace(strings.TrimLeft(trimmed, "`~")), ansiReset)
		} else {
			fmt.Fprintln(out)
		}
		s.inFence = !s.inFence
		return
	}
	if s.inFence {
		fmt.Fprintf(out, "  %s%s%s\n", ansiCodeBlock, sanitizeTerminalText(line), ansiReset)
		return
	}
	s.writeTableLine(out, line)
}

func (s *streamingMarkdown) writeTableLine(out io.Writer, line string) {
	if len(s.tableLines) == 0 {
		if isPotentialMarkdownTableRow(line) {
			s.tableLines = []string{line}
			return
		}
		renderMarkdownLine(out, line)
		return
	}

	if len(s.tableLines) == 1 {
		if isMarkdownTableDelimiter(line) {
			s.tableLines = append(s.tableLines, line)
			return
		}
		s.flushTable(out)
		s.writeTableLine(out, line)
		return
	}

	if isPotentialMarkdownTableRow(line) {
		s.tableLines = append(s.tableLines, line)
		return
	}
	s.flushTable(out)
	renderMarkdownLine(out, line)
}

func (s *streamingMarkdown) flushTable(out io.Writer) {
	if len(s.tableLines) == 0 {
		return
	}
	if table, used, ok := parseMarkdownTable(s.tableLines); ok {
		renderMarkdownTable(out, table)
		for _, line := range s.tableLines[used:] {
			renderMarkdownLine(out, line)
		}
	} else {
		for _, line := range s.tableLines {
			renderMarkdownLine(out, line)
		}
	}
	s.tableLines = nil
}

type markdownTableAlignment byte

const (
	markdownTableLeft markdownTableAlignment = iota
	markdownTableCenter
	markdownTableRight
)

type markdownTable struct {
	indent  string
	headers []string
	align   []markdownTableAlignment
	rows    [][]string
}

// parseMarkdownTable recognizes a pipe table only after validating its
// delimiter row. This avoids turning prose that happens to contain a pipe
// into a table.
func parseMarkdownTable(lines []string) (markdownTable, int, bool) {
	var table markdownTable
	if len(lines) < 2 || !isPotentialMarkdownTableRow(lines[0]) || !isMarkdownTableDelimiter(lines[1]) {
		return table, 0, false
	}

	header, ok := splitMarkdownTableRow(lines[0])
	if !ok {
		return table, 0, false
	}
	delimiter, ok := splitMarkdownTableRow(lines[1])
	if !ok || len(header) != len(delimiter) || len(header) == 0 {
		return table, 0, false
	}

	table.indent = lines[0][:len(lines[0])-len(strings.TrimLeft(lines[0], " \t"))]
	table.headers = header
	table.align = make([]markdownTableAlignment, len(delimiter))
	for i, cell := range delimiter {
		cell = strings.TrimSpace(cell)
		left := strings.HasPrefix(cell, ":")
		right := strings.HasSuffix(cell, ":")
		switch {
		case left && right:
			table.align[i] = markdownTableCenter
		case right:
			table.align[i] = markdownTableRight
		default:
			table.align[i] = markdownTableLeft
		}
	}

	used := 2
	for used < len(lines) && isPotentialMarkdownTableRow(lines[used]) {
		row, ok := splitMarkdownTableRow(lines[used])
		if !ok || len(row) != len(table.headers) {
			break
		}
		table.rows = append(table.rows, row)
		used++
	}
	return table, used, true
}

func isPotentialMarkdownTableRow(line string) bool {
	return strings.Contains(line, "|")
}

func isMarkdownTableDelimiter(line string) bool {
	cells, ok := splitMarkdownTableRow(line)
	if !ok || len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		cell = strings.TrimPrefix(cell, ":")
		cell = strings.TrimSuffix(cell, ":")
		if len(cell) < 3 || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

func splitMarkdownTableRow(line string) ([]string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.Contains(trimmed, "|") {
		return nil, false
	}
	trimmed = strings.TrimPrefix(trimmed, "|")
	if strings.HasSuffix(trimmed, "|") && !isEscapedMarkdownPipe(trimmed, len(trimmed)-1) {
		trimmed = trimmed[:len(trimmed)-1]
	}

	cells := make([]string, 0, 4)
	start := 0
	escaped := false
	inCode := false
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] == '\\' && !escaped {
			escaped = true
			continue
		}
		if trimmed[i] == '`' && !escaped {
			inCode = !inCode
		}
		if trimmed[i] == '|' && !escaped && !inCode {
			cells = append(cells, strings.ReplaceAll(strings.TrimSpace(trimmed[start:i]), `\|`, `|`))
			start = i + 1
		}
		escaped = false
	}
	cells = append(cells, strings.ReplaceAll(strings.TrimSpace(trimmed[start:]), `\|`, `|`))
	return cells, len(cells) > 0
}

func isEscapedMarkdownPipe(value string, index int) bool {
	backslashes := 0
	for index > 0 && value[index-1] == '\\' {
		backslashes++
		index--
	}
	return backslashes%2 == 1
}

func renderMarkdownTable(out io.Writer, table markdownTable) {
	allRows := make([][]string, 0, len(table.rows)+1)
	allRows = append(allRows, table.headers)
	allRows = append(allRows, table.rows...)

	widths := make([]int, len(table.headers))
	rendered := make([][]string, len(allRows))
	for rowIndex, row := range allRows {
		rendered[rowIndex] = make([]string, len(widths))
		for column := range widths {
			if column < len(row) {
				cell := strings.ReplaceAll(row[column], "\n", " ")
				rendered[rowIndex][column] = renderInlineMarkdown(cell)
				if width := ansiDisplayWidth(rendered[rowIndex][column]); width > widths[column] {
					widths[column] = width
				}
			}
		}
	}

	renderTableRow := func(row []string, header bool) {
		fmt.Fprint(out, table.indent)
		for column, cell := range row {
			padding := widths[column] - ansiDisplayWidth(cell)
			left, right := tableCellPadding(padding, table.align[column])
			fmt.Fprint(out, " ")
			if header {
				fmt.Fprint(out, ansiBlue, ansiBold)
			}
			fmt.Fprint(out, strings.Repeat(" ", left), cell, strings.Repeat(" ", right))
			if header {
				fmt.Fprint(out, ansiReset)
			}
			fmt.Fprint(out, " ")
			if column+1 < len(row) {
				fmt.Fprint(out, "│")
			}
		}
		fmt.Fprintln(out)
	}

	renderTableRow(rendered[0], true)
	fmt.Fprint(out, table.indent, ansiDim)
	for column, width := range widths {
		fmt.Fprint(out, strings.Repeat("─", width+2))
		if column+1 < len(widths) {
			fmt.Fprint(out, "┼")
		}
	}
	fmt.Fprintln(out, ansiReset)
	for _, row := range rendered[1:] {
		renderTableRow(row, false)
	}
}

func tableCellPadding(padding int, alignment markdownTableAlignment) (int, int) {
	switch alignment {
	case markdownTableCenter:
		return padding / 2, padding - padding/2
	case markdownTableRight:
		return padding, 0
	default:
		return 0, padding
	}
}

func (s *streamingMarkdown) reset() {
	s.pending = ""
	s.inFence = false
	s.tableLines = nil
}

func nextMarkdownMarker(value string) (string, int) {
	markers := []string{"***", "___", "**", "__", "~~", "`", "*", "_", "["}
	best := -1
	marker := ""
	for _, candidate := range markers {
		if index := strings.Index(value, candidate); index >= 0 && (best < 0 || index < best) {
			best, marker = index, candidate
		}
	}
	return marker, best
}

func markdownHeading(value string) (int, string) {
	level := 0
	for level < len(value) && value[level] == '#' {
		level++
	}
	if level == 0 || level >= len(value) || value[level] != ' ' {
		return 0, ""
	}
	return level, ansiBold + renderInlineMarkdown(strings.TrimSpace(value[level+1:])) + ansiReset
}

func markdownBullet(value string) (string, string, bool) {
	if len(value) >= 2 && (value[0] == '-' || value[0] == '*' || value[0] == '+') && value[1] == ' ' {
		return "•", strings.TrimSpace(value[2:]), true
	}
	// Keep ordered lists aligned while preserving the model's numbering.
	for i := 0; i < len(value); i++ {
		if value[i] == '.' && i+1 < len(value) && value[i+1] == ' ' && i > 0 {
			return value[:i+1], strings.TrimSpace(value[i+2:]), true
		}
		if value[i] < '0' || value[i] > '9' {
			break
		}
	}
	return "", "", false
}

// splitInlineOrderedList handles providers that put several ordered-list
// items on one physical line (for example, "1. first 2. second"). Markdown
// normally requires a line break here, but making each item visible on its
// own line is more useful in the terminal and matches streamed list output.
func splitInlineOrderedList(line string) []string {
	indentLen := len(line) - len(strings.TrimLeft(line, " \t"))
	content := line[indentLen:]
	firstEnd, ok := orderedListMarkerEnd(content, 0)
	if !ok {
		return []string{line}
	}

	starts := []int{0}
	for i := firstEnd; i < len(content); i++ {
		if content[i] < '0' || content[i] > '9' || (i > 0 && content[i-1] != ' ' && content[i-1] != '\t') {
			continue
		}
		if _, ok := orderedListMarkerEnd(content, i); ok {
			starts = append(starts, i)
		}
	}
	if len(starts) == 1 {
		return []string{line}
	}

	result := make([]string, 0, len(starts))
	for i, start := range starts {
		end := len(content)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		item := strings.TrimRight(content[start:end], " \t")
		result = append(result, line[:indentLen]+item)
	}
	return result
}

func orderedListMarkerEnd(value string, start int) (int, bool) {
	if start >= len(value) || value[start] < '0' || value[start] > '9' {
		return 0, false
	}
	i := start
	for i < len(value) && value[i] >= '0' && value[i] <= '9' {
		i++
	}
	if i >= len(value) || value[i] != '.' || i+1 >= len(value) || value[i+1] != ' ' {
		return 0, false
	}
	return i + 2, true
}

func renderInlineMarkdown(value string) string {
	value = sanitizeTerminalText(value)
	var out strings.Builder
	for i := 0; i < len(value); {
		switch {
		case value[i] == '`':
			if end := strings.IndexByte(value[i+1:], '`'); end >= 0 {
				end += i + 1
				fmt.Fprintf(&out, "%s%s%s", ansiCode, value[i+1:end], ansiReset)
				i = end + 1
				continue
			}
		case strings.HasPrefix(value[i:], "***") || strings.HasPrefix(value[i:], "___"):
			marker := value[i : i+3]
			if end := strings.Index(value[i+3:], marker); end >= 0 {
				end += i + 3
				fmt.Fprintf(&out, "%s%s%s%s%s", ansiBold, ansiItalic, ansiGreen,
					renderInlineMarkdown(value[i+3:end]), ansiReset)
				i = end + 3
				continue
			}
		case strings.HasPrefix(value[i:], "**") || strings.HasPrefix(value[i:], "__"):
			marker := value[i : i+2]
			if end := strings.Index(value[i+2:], marker); end >= 0 {
				end += i + 2
				fmt.Fprintf(&out, "%s%s%s%s", ansiBold, ansiGreen,
					renderInlineMarkdown(value[i+2:end]), ansiReset)
				i = end + 2
				continue
			}
		case strings.HasPrefix(value[i:], "~~"):
			if end := strings.Index(value[i+2:], "~~"); end >= 0 {
				end += i + 2
				fmt.Fprintf(&out, "%s\033[9m%s%s", ansiDim, value[i+2:end], ansiReset)
				i = end + 2
				continue
			}
		case value[i] == '*' || value[i] == '_':
			marker := value[i]
			if end := strings.IndexByte(value[i+1:], marker); end >= 0 && end > 0 {
				end += i + 1
				fmt.Fprintf(&out, "%s%s%s%s", ansiItalic, ansiMagenta,
					renderInlineMarkdown(value[i+1:end]), ansiReset)
				i = end + 1
				continue
			}
		case value[i] == '[':
			if close := strings.IndexByte(value[i+1:], ']'); close >= 0 {
				close += i + 1
				if close+1 < len(value) && value[close+1] == '(' {
					if end := strings.IndexByte(value[close+2:], ')'); end >= 0 {
						end += close + 2
						label := value[i+1 : close]
						url := value[close+2 : end]
						fmt.Fprintf(&out, "%s%s%s %s(%s)%s", ansiCyan, label, ansiReset, ansiDim, url, ansiReset)
						i = end + 1
						continue
					}
				}
			}
		}
		out.WriteByte(value[i])
		i++
	}
	return out.String()
}

// sanitizeTerminalText prevents model content from moving the user's cursor
// or changing terminal modes while retaining tabs and printable Unicode.
func sanitizeTerminalText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' || unicode.IsPrint(r) {
			return r
		}
		return -1
	}, value)
}
