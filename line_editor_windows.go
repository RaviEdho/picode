//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

const (
	winEnableProcessed = 0x0001
	winEnableLine      = 0x0002
	winEnableEcho      = 0x0004
	winEnableMouse     = 0x0010
	winEnableWindow    = 0x0008
	winEnableVirtual   = 0x0004
	winKeyEvent        = 0x0001
	winControlKeyShift = 0x0010
	winLeftCtrl        = 0x0008
	winRightCtrl       = 0x0004
	winLeftAlt         = 0x0002
	winRightAlt        = 0x0001
)

var (
	winKernel32         = syscall.NewLazyDLL("kernel32.dll")
	winGetConsoleMode   = winKernel32.NewProc("GetConsoleMode")
	winSetConsoleMode   = winKernel32.NewProc("SetConsoleMode")
	winReadConsoleInput = winKernel32.NewProc("ReadConsoleInputW")
	winGetConsoleSize   = winKernel32.NewProc("GetConsoleScreenBufferInfo")
	winWaitForSingle    = winKernel32.NewProc("WaitForSingleObject")
)

type windowsLineEditor struct {
	in                 syscall.Handle
	out                io.Writer
	outHandle          syscall.Handle
	history            []string
	renderCursorRow    int
	renderEndRow       int
	originalMode       uint32
	originalOutputMode uint32
}

type windowsCoord struct{ X, Y int16 }

type windowsKeyEventRecord struct {
	KeyDown         int32
	RepeatCount     uint16
	VirtualKeyCode  uint16
	VirtualScanCode uint16
	UnicodeChar     uint16
	ControlKeyState uint32
}

type windowsInputRecord struct {
	EventType uint16
	_         [2]byte
	KeyEvent  windowsKeyEventRecord
}

type windowsSmallRect struct{ Left, Top, Right, Bottom int16 }
type windowsConsoleInfo struct {
	Size              windowsCoord
	CursorPosition    windowsCoord
	Attributes        uint16
	Window            windowsSmallRect
	MaximumWindowSize windowsCoord
}

func newPlatformLineEditor(in io.Reader, out io.Writer) lineEditor {
	input, inputOK := in.(*os.File)
	output, outputOK := out.(*os.File)
	if !inputOK || !outputOK {
		return nil
	}
	inHandle := syscall.Handle(input.Fd())
	outHandle := syscall.Handle(output.Fd())
	var mode uint32
	if ok, _, _ := winGetConsoleMode.Call(uintptr(inHandle), uintptr(unsafe.Pointer(&mode))); ok == 0 {
		return nil
	}
	var outMode uint32
	if ok, _, _ := winGetConsoleMode.Call(uintptr(outHandle), uintptr(unsafe.Pointer(&outMode))); ok == 0 {
		return nil
	}
	return &windowsLineEditor{in: inHandle, out: out, outHandle: outHandle, originalMode: mode, originalOutputMode: outMode}
}

func (e *windowsLineEditor) ReadLine(ctx context.Context, prompt string) (string, error) {
	mode := e.originalMode &^ (winEnableProcessed | winEnableLine | winEnableEcho | winEnableMouse | winEnableWindow)
	if ok, _, err := winSetConsoleMode.Call(uintptr(e.in), uintptr(mode)); ok == 0 {
		return "", fmt.Errorf("enable Windows console editing: %w", err)
	}
	defer winSetConsoleMode.Call(uintptr(e.in), uintptr(e.originalMode))
	outputMode := e.originalOutputMode | winEnableVirtual
	if ok, _, err := winSetConsoleMode.Call(uintptr(e.outHandle), uintptr(outputMode)); ok == 0 {
		return "", fmt.Errorf("enable Windows terminal rendering: %w", err)
	}
	defer winSetConsoleMode.Call(uintptr(e.outHandle), uintptr(e.originalOutputMode))

	line := &editableLine{}
	e.renderCursorRow, e.renderEndRow = 0, 0
	historyIndex := len(e.history)
	draft := ""
	fmt.Fprint(e.out, prompt)
	for {
		record, err := e.readRecord(ctx)
		if err != nil {
			return "", err
		}
		if record.EventType != winKeyEvent || record.KeyEvent.KeyDown == 0 {
			continue
		}
		key := record.KeyEvent
		if key.ControlKeyState&(winLeftCtrl|winRightCtrl) != 0 && key.VirtualKeyCode == 'C' {
			e.finishLine("^C")
			return "", errInputInterrupt
		}
		if key.ControlKeyState&(winLeftCtrl|winRightCtrl) != 0 && key.VirtualKeyCode == 'D' {
			if len(line.text) == 0 {
				e.finishLine("")
				return "", io.EOF
			}
			line.delete()
			e.redraw(prompt, line)
			continue
		}
		if key.VirtualKeyCode == 0x0D { // Enter.
			altOrShift := key.ControlKeyState&(winControlKeyShift|winLeftAlt|winRightAlt) != 0
			if altOrShift {
				line.insert('\n')
				e.redraw(prompt, line)
				continue
			}
			e.finishLine("")
			value := line.String()
			if value != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != value) {
				e.history = append(e.history, value)
			}
			return value, nil
		}
		switch key.VirtualKeyCode {
		case 0x09: // Tab.
			start, replacement, matches := completePath(line)
			if replacement != "" {
				line.replace(start, line.cursor, replacement)
				e.redraw(prompt, line)
			} else if len(matches) > 1 {
				e.showCompletions(prompt, line, matches)
			}
		case 0x08:
			line.backspace()
			e.redraw(prompt, line)
		case 0x25:
			if line.cursor > 0 {
				line.cursor--
			}
			e.redraw(prompt, line)
		case 0x27:
			if line.cursor < len(line.text) {
				line.cursor++
			}
			e.redraw(prompt, line)
		case 0x24:
			line.cursor = 0
			e.redraw(prompt, line)
		case 0x23:
			line.cursor = len(line.text)
			e.redraw(prompt, line)
		case 0x2E:
			line.delete()
			e.redraw(prompt, line)
		case 0x26:
			if line.moveUp() {
				e.redraw(prompt, line)
				break
			}
			if historyIndex == len(e.history) {
				draft = line.String()
			}
			if historyIndex > 0 {
				historyIndex--
				line.set(e.history[historyIndex])
				e.redraw(prompt, line)
			}
		case 0x28:
			if line.moveDown() {
				e.redraw(prompt, line)
				break
			}
			if historyIndex < len(e.history) {
				historyIndex++
				if historyIndex == len(e.history) {
					line.set(draft)
				} else {
					line.set(e.history[historyIndex])
				}
				e.redraw(prompt, line)
			}
		default:
			if key.UnicodeChar != 0 {
				runes := utf16.Decode([]uint16{key.UnicodeChar})
				if len(runes) > 0 && runes[0] >= 32 {
					line.insert(runes[0])
					e.redraw(prompt, line)
				}
			}
		}
	}
}

func (e *windowsLineEditor) readRecord(ctx context.Context) (windowsInputRecord, error) {
	for {
		select {
		case <-ctx.Done():
			return windowsInputRecord{}, ctx.Err()
		default:
		}
		var record windowsInputRecord
		var read uint32
		wait, _, waitErr := winWaitForSingle.Call(uintptr(e.in), 100)
		if wait == 0x00000102 { // WAIT_TIMEOUT
			continue
		}
		if wait != 0 {
			return record, waitErr
		}
		ok, _, err := winReadConsoleInput.Call(uintptr(e.in), uintptr(unsafe.Pointer(&record)), 1, uintptr(unsafe.Pointer(&read)))
		if ok == 0 {
			return record, err
		}
		if read > 0 {
			return record, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *windowsLineEditor) showCompletions(prompt string, line *editableLine, matches []string) {
	e.finishLine("")
	for _, match := range matches {
		fmt.Fprintf(e.out, "  %s\n", match)
	}
	e.redraw(prompt, line)
}

func (e *windowsLineEditor) redraw(prompt string, line *editableLine) {
	columns := windowsTerminalColumns(e.outHandle)
	if columns <= 0 {
		columns = 80
	}
	fmt.Fprint(e.out, "\r")
	if e.renderCursorRow > 0 {
		fmt.Fprintf(e.out, "\033[%dA", e.renderCursorRow)
	}
	// The console writes a bare LF as a line down with no carriage return, so
	// emit CR before each embedded newline to keep continuation lines flush left.
	display := strings.ReplaceAll(line.String(), "\n", "\r\n")
	fmt.Fprintf(e.out, "\033[J%s%s", prompt, display)
	promptWidth := ansiDisplayWidth(prompt)
	// Continuation lines start at column 0 (flush left).
	endRow := multilineEndRow(promptWidth, line.text, 0, columns)
	cursorRow, cursorColumn := multilineCursorPosition(promptWidth, line.text[:line.cursor], 0, columns)
	fmt.Fprint(e.out, "\r")
	if endRow > cursorRow {
		fmt.Fprintf(e.out, "\033[%dA", endRow-cursorRow)
	}
	if cursorColumn > 0 {
		fmt.Fprintf(e.out, "\033[%dC", cursorColumn)
	}
	e.renderCursorRow, e.renderEndRow = cursorRow, endRow
}

func (e *windowsLineEditor) finishLine(suffix string) {
	if e.renderEndRow > e.renderCursorRow {
		fmt.Fprintf(e.out, "\033[%dB", e.renderEndRow-e.renderCursorRow)
	}
	fmt.Fprintf(e.out, "\r%s\r\n", suffix)
	e.renderCursorRow, e.renderEndRow = 0, 0
}

func windowsTerminalColumns(handle syscall.Handle) int {
	var info windowsConsoleInfo
	ok, _, _ := winGetConsoleSize.Call(uintptr(handle), uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return 0
	}
	return int(info.Window.Right-info.Window.Left) + 1
}
