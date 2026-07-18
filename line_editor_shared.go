package main

// Terminal-independent line layout helpers shared by the Unix and Windows
// editors. Display width intentionally matches the editor's rune-based model.
func occupiedRow(width, columns int) int {
	if width <= 0 {
		return 0
	}
	return (width - 1) / columns
}

func ansiDisplayWidth(value string) int {
	width := 0
	state := byte(0)
	for _, r := range value {
		switch state {
		case 1:
			if r == '[' {
				state = 2
			} else {
				state = 0
			}
		case 2:
			if r >= 0x40 && r <= 0x7e {
				state = 0
			}
		default:
			if r == 0x1b {
				state = 1
			} else {
				width++
			}
		}
	}
	return width
}

func multilineEndRow(promptWidth int, value []rune, continuationWidth, columns int) int {
	row, _ := multilineCursorPosition(promptWidth, value, continuationWidth, columns)
	return row
}

func multilineCursorPosition(promptWidth int, value []rune, continuationWidth, columns int) (row, column int) {
	if columns <= 0 {
		columns = 80
	}
	lineWidth := promptWidth
	for _, r := range value {
		if r == '\n' {
			row += occupiedRow(lineWidth, columns) + 1
			lineWidth = continuationWidth
			continue
		}
		lineWidth++
	}
	row += occupiedRow(lineWidth, columns)
	if lineWidth > 0 && lineWidth%columns == 0 {
		return row, columns - 1
	}
	return row, lineWidth % columns
}
