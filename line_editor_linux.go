//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

const escapeReadTimeout = 30 * time.Millisecond

type linuxLineEditor struct {
	in              *os.File
	out             io.Writer
	outFD           uintptr
	history         []string
	renderCursorRow int
	renderEndRow    int
}

func newPlatformLineEditor(in io.Reader, out io.Writer) lineEditor {
	input, inputOK := in.(*os.File)
	output, outputOK := out.(*os.File)
	if !inputOK || !outputOK || !isLinuxTerminal(input.Fd()) || !isLinuxTerminal(output.Fd()) {
		return nil
	}
	return &linuxLineEditor{in: input, out: out, outFD: output.Fd()}
}

func isLinuxTerminal(fd uintptr) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

func getLinuxTermios(fd uintptr) (syscall.Termios, error) {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios)))
	if errno != 0 {
		return termios, errno
	}
	return termios, nil
}

func setLinuxTermios(fd uintptr, termios *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(termios)))
	if errno != 0 {
		return errno
	}
	return nil
}

func rawLinuxTermios(termios syscall.Termios) syscall.Termios {
	termios.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	termios.Oflag &^= syscall.OPOST
	termios.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	termios.Cflag &^= syscall.CSIZE | syscall.PARENB
	termios.Cflag |= syscall.CS8
	termios.Cc[syscall.VMIN] = 1
	termios.Cc[syscall.VTIME] = 0
	return termios
}

func (e *linuxLineEditor) ReadLine(ctx context.Context, prompt string) (line string, err error) {
	fd := e.in.Fd()
	original, err := getLinuxTermios(fd)
	if err != nil {
		return "", fmt.Errorf("read terminal settings: %w", err)
	}
	raw := rawLinuxTermios(original)
	if err := setLinuxTermios(fd, &raw); err != nil {
		return "", fmt.Errorf("enable terminal line editing: %w", err)
	}
	defer func() {
		if restoreErr := setLinuxTermios(fd, &original); err == nil && restoreErr != nil {
			err = fmt.Errorf("restore terminal settings: %w", restoreErr)
		}
	}()

	current := editableLine{}
	e.renderCursorRow = 0
	e.renderEndRow = 0
	historyIndex := len(e.history)
	draft := ""
	fmt.Fprint(e.out, prompt)

	for {
		value, readErr := e.readByte(ctx, 0)
		if readErr != nil {
			return "", readErr
		}
		switch value {
		case '\r', '\n':
			e.finishLine("")
			line = current.String()
			if line != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != line) {
				e.history = append(e.history, line)
			}
			return line, nil
		case 3: // Ctrl-C
			e.finishLine("^C")
			return "", errInputInterrupt
		case 4: // Ctrl-D deletes at the cursor, or exits on an empty line.
			if len(current.text) == 0 {
				e.finishLine("")
				return "", io.EOF
			}
			current.delete()
			e.redraw(prompt, &current)
		case 1: // Ctrl-A
			current.cursor = 0
			e.redraw(prompt, &current)
		case 5: // Ctrl-E
			current.cursor = len(current.text)
			e.redraw(prompt, &current)
		case 11: // Ctrl-K
			current.text = current.text[:current.cursor]
			e.redraw(prompt, &current)
		case 21: // Ctrl-U
			current.text = current.text[current.cursor:]
			current.cursor = 0
			e.redraw(prompt, &current)
		case 8, 127:
			current.backspace()
			e.redraw(prompt, &current)
		case 27:
			key, keyErr := e.readEscape(ctx)
			if keyErr != nil && !errors.Is(keyErr, errEscapeTimeout) {
				return "", keyErr
			}
			switch key {
			case "left":
				if current.cursor > 0 {
					current.cursor--
				}
			case "right":
				if current.cursor < len(current.text) {
					current.cursor++
				}
			case "home":
				current.cursor = 0
			case "end":
				current.cursor = len(current.text)
			case "delete":
				current.delete()
			case "up":
				if historyIndex == len(e.history) {
					draft = current.String()
				}
				if historyIndex > 0 {
					historyIndex--
					current.set(e.history[historyIndex])
				}
			case "down":
				if historyIndex < len(e.history) {
					historyIndex++
					if historyIndex == len(e.history) {
						current.set(draft)
					} else {
						current.set(e.history[historyIndex])
					}
				}
			}
			e.redraw(prompt, &current)
		default:
			if value < 32 {
				continue
			}
			r, runeErr := e.readRune(ctx, value)
			if runeErr != nil {
				return "", runeErr
			}
			current.insert(r)
			e.redraw(prompt, &current)
		}
	}
}

func (e *linuxLineEditor) redraw(prompt string, line *editableLine) {
	columns := linuxTerminalColumns(e.outFD)
	if columns <= 0 {
		columns = 80
	}

	// Return to the beginning of the previously rendered input before clearing.
	// A bare carriage return only reaches the start of the current visual row,
	// which caused wrapped input to be appended to and redrawn repeatedly.
	fmt.Fprint(e.out, "\r")
	if e.renderCursorRow > 0 {
		fmt.Fprintf(e.out, "\033[%dA", e.renderCursorRow)
	}
	fmt.Fprintf(e.out, "\033[J%s%s", prompt, line.String())

	promptWidth := ansiDisplayWidth(prompt)
	totalWidth := promptWidth + runeDisplayWidth(line.text)
	cursorWidth := promptWidth + runeDisplayWidth(line.text[:line.cursor])
	endRow := occupiedRow(totalWidth, columns)
	cursorRow, cursorColumn := cursorPosition(cursorWidth, totalWidth, columns)

	// Position from the end of the rendered text rather than moving left: left
	// movement does not cross terminal rows consistently.
	fmt.Fprint(e.out, "\r")
	if endRow > cursorRow {
		fmt.Fprintf(e.out, "\033[%dA", endRow-cursorRow)
	}
	if cursorColumn > 0 {
		fmt.Fprintf(e.out, "\033[%dC", cursorColumn)
	}
	e.renderCursorRow = cursorRow
	e.renderEndRow = endRow
}

func (e *linuxLineEditor) finishLine(suffix string) {
	if e.renderEndRow > e.renderCursorRow {
		fmt.Fprintf(e.out, "\033[%dB", e.renderEndRow-e.renderCursorRow)
	}
	fmt.Fprintf(e.out, "\r%s\r\n", suffix)
	e.renderCursorRow = 0
	e.renderEndRow = 0
}

type linuxWinsize struct {
	rows, columns, xpixel, ypixel uint16
}

func linuxTerminalColumns(fd uintptr) int {
	var size linuxWinsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return 0
	}
	return int(size.columns)
}

func occupiedRow(width, columns int) int {
	if width <= 0 {
		return 0
	}
	return (width - 1) / columns
}

func cursorPosition(cursorWidth, totalWidth, columns int) (row, column int) {
	// At the exact right edge, terminals retain the cursor in the final column
	// until another printable character triggers wrapping.
	if cursorWidth == totalWidth && cursorWidth > 0 && cursorWidth%columns == 0 {
		return (cursorWidth - 1) / columns, columns - 1
	}
	return cursorWidth / columns, cursorWidth % columns
}

func ansiDisplayWidth(value string) int {
	width := 0
	state := byte(0)
	for _, r := range value {
		switch state {
		case 1: // Escape introducer.
			if r == '[' {
				state = 2
			} else {
				state = 0
			}
		case 2: // CSI parameters, ending with a final byte.
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

func runeDisplayWidth(value []rune) int {
	// Keep the editor's existing rune-based cursor model. This correctly handles
	// UTF-8 input; combining and double-width glyphs remain a display limitation.
	return len(value)
}

var errEscapeTimeout = errors.New("incomplete escape sequence")

func (e *linuxLineEditor) readEscape(ctx context.Context) (string, error) {
	first, err := e.readByte(ctx, escapeReadTimeout)
	if err != nil {
		return "", err
	}
	if first == 'O' {
		last, err := e.readByte(ctx, escapeReadTimeout)
		if err != nil {
			return "", err
		}
		if last == 'H' {
			return "home", nil
		}
		if last == 'F' {
			return "end", nil
		}
		return "", nil
	}
	if first != '[' {
		return "", nil
	}
	sequence := make([]byte, 0, 4)
	for len(sequence) < 8 {
		b, err := e.readByte(ctx, escapeReadTimeout)
		if err != nil {
			return "", err
		}
		sequence = append(sequence, b)
		if (b >= 'A' && b <= 'Z') || b == '~' {
			break
		}
	}
	switch string(sequence) {
	case "A":
		return "up", nil
	case "B":
		return "down", nil
	case "C":
		return "right", nil
	case "D":
		return "left", nil
	case "H", "1~", "7~":
		return "home", nil
	case "F", "4~", "8~":
		return "end", nil
	case "3~":
		return "delete", nil
	default:
		return "", nil
	}
}

func (e *linuxLineEditor) readRune(ctx context.Context, first byte) (rune, error) {
	width := 1
	switch {
	case first&0xe0 == 0xc0:
		width = 2
	case first&0xf0 == 0xe0:
		width = 3
	case first&0xf8 == 0xf0:
		width = 4
	}
	encoded := []byte{first}
	for len(encoded) < width {
		b, err := e.readByte(ctx, escapeReadTimeout)
		if err != nil {
			return utf8.RuneError, err
		}
		encoded = append(encoded, b)
	}
	r, _ := utf8.DecodeRune(encoded)
	return r, nil
}

func (e *linuxLineEditor) readByte(ctx context.Context, timeout time.Duration) (byte, error) {
	fd := int(e.in.Fd())
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		wait := 100 * time.Millisecond
		if !deadline.IsZero() {
			wait = time.Until(deadline)
			if wait <= 0 {
				return 0, errEscapeTimeout
			}
		}
		var readSet syscall.FdSet
		index, bit := fd/64, uint(fd%64)
		if index >= len(readSet.Bits) {
			return 0, fmt.Errorf("terminal file descriptor %d is too large", fd)
		}
		readSet.Bits[index] |= int64(1) << bit
		tv := syscall.NsecToTimeval(wait.Nanoseconds())
		ready, err := syscall.Select(fd+1, &readSet, nil, nil, &tv)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if ready == 0 {
			continue
		}
		var buffer [1]byte
		n, err := syscall.Read(fd, buffer[:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		return buffer[0], nil
	}
}
