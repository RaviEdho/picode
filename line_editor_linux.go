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
	in      *os.File
	out     io.Writer
	history []string
}

func newPlatformLineEditor(in io.Reader, out io.Writer) lineEditor {
	input, inputOK := in.(*os.File)
	output, outputOK := out.(*os.File)
	if !inputOK || !outputOK || !isLinuxTerminal(input.Fd()) || !isLinuxTerminal(output.Fd()) {
		return nil
	}
	return &linuxLineEditor{in: input, out: out}
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
			fmt.Fprint(e.out, "\r\n")
			line = current.String()
			if line != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != line) {
				e.history = append(e.history, line)
			}
			return line, nil
		case 3: // Ctrl-C
			fmt.Fprint(e.out, "^C\r\n")
			return "", errInputInterrupt
		case 4: // Ctrl-D deletes at the cursor, or exits on an empty line.
			if len(current.text) == 0 {
				fmt.Fprint(e.out, "\r\n")
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
	fmt.Fprintf(e.out, "\r%s%s\033[K", prompt, line.String())
	if remaining := len(line.text) - line.cursor; remaining > 0 {
		fmt.Fprintf(e.out, "\033[%dD", remaining)
	}
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
