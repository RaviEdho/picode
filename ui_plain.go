package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ANSI styles used by the plain terminal renderer.
const (
	colorReset  = ansiReset
	colorCyan   = ansiCyan
	colorGreen  = ansiGreen
	colorYellow = ansiYellow
	colorFaded  = ansiDim + "\033[38;5;248m"
	clearEOL    = "\033[K"
)

// statusGradient is the subtle trail of the highlight that sweeps across the
// response status text.
var statusGradient = []string{
	ansiDim + "\033[38;5;248m",
	ansiDim + "\033[38;5;250m",
	ansiDim + "\033[38;5;253m",
	ansiDim + "\033[38;5;255m",
	ansiDim + "\033[38;5;253m",
	ansiDim + "\033[38;5;250m",
	ansiDim + "\033[38;5;248m",
}

// PlainUI implements the current line-oriented terminal interface.
type PlainUI struct {
	in  io.Reader
	out io.Writer
	err io.Writer

	// renderOut routes controller and line-editor output through renderLoop.
	// renderLoop is the only goroutine that writes to out while Run is active.
	renderOut io.Writer
	renderCh  chan renderCommand
	renderWG  sync.WaitGroup
	renderMu  sync.Mutex
	rendering bool

	statusUpdate  func(string)
	statusStop    func()
	statusID      uint64
	textOpen      bool
	markdownLive  streamingMarkdown
	toolOpen      bool
	activeTools   int
	printedTools  map[int]bool
	printedCmdLen map[int]int

	inputOpen      bool
	inputPrompt    string
	inputLine      editableLine
	inputGhostText string
	inputColumns   int
	inputCursorRow int
	inputEndRow    int
	inputGhost     bool
}

// NewPlainUI creates a frontend around injectable terminal streams.
func NewPlainUI(in io.Reader, out, errOut io.Writer) *PlainUI {
	ui := &PlainUI{
		in:       in,
		out:      out,
		err:      errOut,
		renderCh: make(chan renderCommand, 256),
	}
	ui.renderOut = plainUIRenderWriter{ui: ui}
	return ui
}

// Warning writes a startup diagnostic to stderr.
func (ui *PlainUI) Warning(message string) {
	fmt.Fprintf(ui.err, "warning: %s\n", message)
}

// Run reads user input and drives the session until exit.
func (ui *PlainUI) Run(ctx context.Context, session Conversation) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ui.startRenderer()
	defer func() {
		ui.clearInput()
		ui.printSummary(session.Usage(), session.SessionID(), len(session.History()) > 0)
		ui.flushRenderer()
		ui.stopRenderer()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	// Ctrl-C cancels an active tool; SIGTERM always exits the session.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-sigCh:
				if sig == os.Interrupt && session.CancelActiveTool() {
					continue
				}
				cancel()
			}
		}
	}()

	// Use platform line editing for terminals and scanner input for pipes, files, and tests.
	// The editor uses the original output only for terminal detection and size;
	// all of its rendering is delegated back to PlainUI.
	editor := newPlatformLineEditor(ui.in, ui.out)
	var scannedInput <-chan inputResult
	if editor == nil {
		input := make(chan inputResult, 1)
		scannedInput = input
		go func() {
			scanner := bufio.NewScanner(ui.in)
			for scanner.Scan() {
				select {
				case input <- inputResult{text: scanner.Text(), ok: true}:
				case <-ctx.Done():
					return
				}
			}
			select {
			case input <- inputResult{err: scanner.Err()}:
			case <-ctx.Done():
			}
		}()
	}

	fmt.Fprintf(ui.renderOut, "picode [%s] — type 'exit' or Ctrl-D to quit\n", session.SessionID())
	ui.printHistory(session.History())
	// History must reach the terminal before the editor enables raw mode. In raw
	// mode OPOST is disabled, so queued newlines would not return to column zero
	// and a resumed transcript would render diagonally or overwrite itself.
	ui.flushRenderer()
	for {
		input := scannedInput
		if editor != nil {
			result := make(chan inputResult, 1)
			input = result
			go func() {
				prompt := colorCyan + "you>" + colorReset + " "
				result <- editorInput(ctx, editor, prompt, ui)
			}()
		} else {
			fmt.Fprintf(ui.renderOut, "%syou>%s ", colorCyan, colorReset)
		}
		select {
		case <-ctx.Done():
			// Wait for the editor's context poll so it restores temporary terminal settings before exit.
			if editor != nil {
				<-input
			}
			return nil
		case result := <-input:
			if errors.Is(result.err, errInputInterrupt) {
				return nil
			}
			if result.err != nil {
				return fmt.Errorf("read input: %w", result.err)
			}
			if !result.ok {
				return nil
			}
			text := strings.TrimSpace(result.text)
			if text == "" {
				continue
			}
			if text == "exit" || text == "quit" {
				return nil
			}
			err := session.RunTurn(ctx, text, ui)
			// RunTurn emits its final stream event before returning, but Emit is
			// asynchronous. Drain those transcript commands before the next editor
			// instance enables raw mode for another prompt.
			ui.flushRenderer()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				fmt.Fprintf(ui.err, "error: %v\n", err)
			}
		}
	}
}

// Emit queues one semantic UI event. renderLoop is its sole renderer.
func (ui *PlainUI) Emit(event UIEvent) {
	ui.renderMu.Lock()
	if ui.rendering {
		ui.renderCh <- eventRenderCommand{event: event}
	}
	ui.renderMu.Unlock()
}

// renderEvent runs only in renderLoop.
func (ui *PlainUI) renderEvent(event UIEvent) {
	// Each event updates only its related renderer state.
	switch event := event.(type) {
	case StatusEvent:
		if ui.statusStop == nil {
			ui.resetResponseState()
			ui.statusID++
			ui.statusUpdate, ui.statusStop = ui.animateStatus(string(event.Phase), ui.statusID)
		} else {
			ui.statusUpdate(string(event.Phase))
		}
	case AssistantDeltaEvent:
		ui.stopStatus()
		if !ui.textOpen {
			fmt.Fprintf(ui.out, "%spicode>%s ", colorGreen, colorReset)
			ui.textOpen = true
		}
		ui.markdownLive.write(ui.out, event.Text, false)
	case ToolCallUpdateEvent:
		ui.stopStatus()
		if event.Name == "" {
			return
		}
		ui.markdownLive.write(ui.out, "", true)
		if !ui.printedTools[event.Index] {
			if ui.textOpen || ui.toolOpen {
				fmt.Fprintln(ui.out)
			}
			fmt.Fprintf(ui.out, "%s%s>%s ", colorYellow, event.Name, colorReset)
			ui.printedTools[event.Index] = true
			ui.toolOpen = true
		}
		printed := ui.printedCmdLen[event.Index]
		if len(event.Input) > printed {
			fmt.Fprint(ui.out, event.Input[printed:])
			ui.printedCmdLen[event.Index] = len(event.Input)
		}
	case StreamFinishedEvent:
		ui.stopStatus()
		ui.markdownLive.write(ui.out, "", true)
		if ui.textOpen {
			fmt.Fprintln(ui.out)
		}
		if ui.toolOpen {
			fmt.Fprintln(ui.out)
		}
		ui.textOpen = false
		ui.toolOpen = false
		ui.markdownLive.reset()
	case ToolResultEvent:
		if event.Status == ToolCancelled {
			fmt.Fprintf(ui.out, "%s^C cancelled %s%s\n", colorYellow, event.Name, colorReset)
		}
		fmt.Fprintf(ui.out, "%s   output>%s ", colorYellow, colorReset)
		printTruncated(ui.out, event.Output, 5, colorFaded)
		fmt.Fprintln(ui.out)
	case ToolProgressEvent:
		if event.Done {
			if ui.activeTools > 0 {
				ui.activeTools--
			}
			if ui.activeTools == 0 {
				ui.stopStatus()
			} else if ui.statusUpdate != nil {
				ui.statusUpdate(fmt.Sprintf("running %d tools", ui.activeTools))
			}
		} else {
			ui.activeTools++
			if ui.statusStop == nil {
				ui.statusID++
				ui.statusUpdate, ui.statusStop = ui.animateStatus(fmt.Sprintf("running %d tools", ui.activeTools), ui.statusID)
			} else if ui.statusUpdate != nil {
				ui.statusUpdate(fmt.Sprintf("running %d tools", ui.activeTools))
			}
		}
	case EmptyResponseEvent:
		fmt.Fprintln(ui.out, "(empty response)")
	}
}

type renderCommand interface{ isRenderCommand() }

type eventRenderCommand struct{ event UIEvent }
type writeRenderCommand struct {
	text []byte
	done chan struct{}
}
type statusRenderCommand struct {
	id     uint64
	status string
	phase  int
}
type drawInputRenderCommand struct {
	prompt  string
	line    editableLine
	ghost   string
	columns int
	done    chan struct{}
}
type finishInputRenderCommand struct {
	suffix string
	done   chan struct{}
}
type clearInputRenderCommand struct{ done chan struct{} }
type flushRenderCommand struct{ done chan struct{} }
type stopRenderCommand struct{ done chan struct{} }

func (eventRenderCommand) isRenderCommand()       {}
func (writeRenderCommand) isRenderCommand()       {}
func (statusRenderCommand) isRenderCommand()      {}
func (drawInputRenderCommand) isRenderCommand()   {}
func (finishInputRenderCommand) isRenderCommand() {}
func (clearInputRenderCommand) isRenderCommand()  {}
func (flushRenderCommand) isRenderCommand()       {}
func (stopRenderCommand) isRenderCommand()        {}

// plainUIRenderWriter makes existing fmt-based callers renderer-safe.
type plainUIRenderWriter struct{ ui *PlainUI }

func (w plainUIRenderWriter) Write(text []byte) (int, error) {
	copyText := append([]byte(nil), text...)
	w.ui.renderMu.Lock()
	if !w.ui.rendering {
		w.ui.renderMu.Unlock()
		return w.ui.out.Write(copyText)
	}
	w.ui.renderCh <- writeRenderCommand{text: copyText}
	w.ui.renderMu.Unlock()
	return len(text), nil
}

func (ui *PlainUI) startRenderer() {
	ui.renderMu.Lock()
	if ui.rendering {
		ui.renderMu.Unlock()
		return
	}
	ui.rendering = true
	ui.renderWG.Add(1)
	ui.renderMu.Unlock()
	go ui.renderLoop()
}

// renderLoop is the sole writer to ui.out during Run.
func (ui *PlainUI) renderLoop() {
	defer ui.renderWG.Done()
	for command := range ui.renderCh {
		switch command := command.(type) {
		case eventRenderCommand:
			ui.renderTranscript(func() { ui.renderEvent(command.event) })
		case writeRenderCommand:
			ui.renderTranscript(func() { _, _ = ui.out.Write(command.text) })
			if command.done != nil {
				close(command.done)
			}
		case statusRenderCommand:
			if command.id == ui.statusID && ui.statusStop != nil {
				ui.renderTranscript(func() { writeStatus(ui.out, command.status, command.phase) })
			}
		case drawInputRenderCommand:
			ui.drawInput(command.prompt, command.line, command.ghost, command.columns)
			close(command.done)
		case finishInputRenderCommand:
			ui.finishInput(command.suffix)
			close(command.done)
		case clearInputRenderCommand:
			ui.clearRenderedInput()
			close(command.done)
		case flushRenderCommand:
			close(command.done)
		case stopRenderCommand:
			ui.stopStatus()
			close(command.done)
			return
		}
	}
}

// WriteTerminal implements terminalInputRenderer for terminal protocol setup.
func (ui *PlainUI) WriteTerminal(text string) { ui.writeRenderer(text) }

// DrawInput implements terminalInputRenderer. It waits until the complete
// redraw is committed before the editor reads the next key.
func (ui *PlainUI) DrawInput(prompt string, line editableLine, ghost string, columns int) {
	done := make(chan struct{})
	if !ui.sendRendererCommand(drawInputRenderCommand{
		prompt: prompt, line: editableLine{text: append([]rune(nil), line.text...), cursor: line.cursor},
		ghost: ghost, columns: columns, done: done,
	}) {
		return
	}
	<-done
}

// FinishInput implements terminalInputRenderer.
func (ui *PlainUI) FinishInput(suffix string) {
	done := make(chan struct{})
	if !ui.sendRendererCommand(finishInputRenderCommand{suffix: suffix, done: done}) {
		return
	}
	<-done
}

func (ui *PlainUI) clearInput() {
	done := make(chan struct{})
	if !ui.sendRendererCommand(clearInputRenderCommand{done: done}) {
		return
	}
	<-done
}

func (ui *PlainUI) writeRenderer(text string) {
	done := make(chan struct{})
	if !ui.sendRendererCommand(writeRenderCommand{text: []byte(text), done: done}) {
		return
	}
	<-done
}

func (ui *PlainUI) sendRendererCommand(command renderCommand) bool {
	ui.renderMu.Lock()
	defer ui.renderMu.Unlock()
	if ui.rendering {
		ui.renderCh <- command
		return true
	}
	return false
}

// drawInput runs in renderLoop and owns prompt, input, and cursor layout.
func (ui *PlainUI) drawInput(prompt string, line editableLine, ghost string, columns int) {
	if columns <= 0 {
		columns = 80
	}
	fmt.Fprint(ui.out, "\r")
	if ui.inputOpen && ui.inputCursorRow > 0 {
		fmt.Fprintf(ui.out, "\033[%dA", ui.inputCursorRow)
	}
	displayValue := append([]rune(nil), line.text...)
	displayValue = append(displayValue, []rune(ghost)...)
	display := strings.ReplaceAll(line.String(), "\n", "\r\n")
	if ghost != "" {
		display += ansiDim + ghost + ansiReset
	}
	fmt.Fprintf(ui.out, "\033[J%s%s", prompt, display)
	promptWidth := ansiDisplayWidth(prompt)
	endRow := multilineEndRow(promptWidth, displayValue, 0, columns)
	cursorRow, cursorColumn := multilineCursorPosition(promptWidth, line.text[:line.cursor], 0, columns)
	fmt.Fprint(ui.out, "\r")
	if endRow > cursorRow {
		fmt.Fprintf(ui.out, "\033[%dA", endRow-cursorRow)
	}
	if cursorColumn > 0 {
		fmt.Fprintf(ui.out, "\033[%dC", cursorColumn)
	}
	ui.inputOpen = true
	ui.inputPrompt = prompt
	ui.inputLine = editableLine{text: append([]rune(nil), line.text...), cursor: line.cursor}
	ui.inputGhostText = ghost
	ui.inputColumns = columns
	ui.inputCursorRow, ui.inputEndRow = cursorRow, endRow
	ui.inputGhost = ghost != ""
}

func (ui *PlainUI) finishInput(suffix string) {
	if !ui.inputOpen {
		return
	}
	if ui.inputGhost {
		fmt.Fprint(ui.out, "\033[K")
	}
	if ui.inputEndRow > ui.inputCursorRow {
		fmt.Fprintf(ui.out, "\033[%dB", ui.inputEndRow-ui.inputCursorRow)
		if ui.inputGhost {
			fmt.Fprint(ui.out, "\033[J")
		}
	}
	fmt.Fprintf(ui.out, "\r%s\r\n", suffix)
	ui.inputOpen = false
	ui.inputPrompt = ""
	ui.inputLine = editableLine{}
	ui.inputGhostText = ""
	ui.inputColumns = 0
	ui.inputCursorRow, ui.inputEndRow = 0, 0
	ui.inputGhost = false
}

// renderTranscript temporarily removes the active input region before writing
// asynchronous transcript or status output, then restores it below the new
// content. This keeps late stream events from overwriting a typed draft.
func (ui *PlainUI) renderTranscript(write func()) {
	if !ui.inputOpen {
		write()
		return
	}
	prompt, line := ui.inputPrompt, ui.inputLine
	ghost, columns := ui.inputGhostText, ui.inputColumns
	ui.clearRenderedInput()
	write()
	ui.drawInput(prompt, line, ghost, columns)
}

func (ui *PlainUI) clearRenderedInput() {
	if !ui.inputOpen {
		return
	}
	fmt.Fprint(ui.out, "\r")
	if ui.inputCursorRow > 0 {
		fmt.Fprintf(ui.out, "\033[%dA", ui.inputCursorRow)
	}
	fmt.Fprint(ui.out, "\033[J")
	ui.inputOpen = false
	ui.inputCursorRow, ui.inputEndRow = 0, 0
	ui.inputGhost = false
}

func (ui *PlainUI) flushRenderer() {
	ui.renderMu.Lock()
	defer ui.renderMu.Unlock()
	if !ui.rendering {
		return
	}
	done := make(chan struct{})
	ui.renderCh <- flushRenderCommand{done: done}
	<-done
}

func (ui *PlainUI) stopRenderer() {
	// Keep rendering marked active until the stop command has drained all
	// earlier commands. This prevents a concurrent Emit or renderOut.Write from
	// enqueueing behind stop after renderLoop has exited.
	ui.renderMu.Lock()
	if !ui.rendering {
		ui.renderMu.Unlock()
		return
	}
	done := make(chan struct{})
	ui.renderCh <- stopRenderCommand{done: done}
	<-done
	ui.renderWG.Wait()
	ui.rendering = false
	ui.renderMu.Unlock()
}

// resetResponseState prepares tracking for a new model stream.
func (ui *PlainUI) resetResponseState() {
	ui.textOpen = false
	ui.toolOpen = false
	ui.printedTools = make(map[int]bool)
	ui.printedCmdLen = make(map[int]int)
}

// stopStatus is safe to call after the status animation has already stopped.
func (ui *PlainUI) stopStatus() {
	if ui.statusStop != nil {
		ui.statusStop()
		ui.statusUpdate = nil
		ui.statusStop = nil
	}
}

// animateStatus sweeps the response status until its stop function is called.
func (ui *PlainUI) animateStatus(initial string, id uint64) (update func(string), stop func()) {
	done := make(chan struct{})
	var once sync.Once
	var statusMu sync.Mutex
	status := initial
	const (
		frameInterval = 60 * time.Millisecond
		sweepInterval = 2 * time.Second
	)
	phase := 0
	nextSweep := time.Now().Add(sweepInterval)
	var wg sync.WaitGroup
	wg.Add(1)
	writeStatus(ui.out, status, phase)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(frameInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			statusMu.Lock()
			now := time.Now()
			cycle := len([]rune(status)) + len(statusGradient)
			if !now.Before(nextSweep) {
				phase = 0
				nextSweep = now.Add(sweepInterval)
			} else if phase < cycle-1 {
				phase++
			}
			select {
			case ui.renderCh <- statusRenderCommand{id: id, status: status, phase: phase}:
			default:
			}
			statusMu.Unlock()
		}
	}()
	update = func(value string) {
		statusMu.Lock()
		if status != value {
			status = value
			// Start the new status with a fresh sweep and reset its timer.
			phase = 0
			nextSweep = time.Now().Add(sweepInterval)
		}
		writeStatus(ui.out, value, phase)
		statusMu.Unlock()
	}
	stop = func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			fmt.Fprint(ui.out, "\r", clearEOL)
		})
	}
	return update, stop
}

// writeStatus sweeps a subtle gradient over the response status text.
func writeStatus(out io.Writer, status string, phase int) {
	runes := []rune(status)
	fmt.Fprintf(out, "\r%spicode>%s ", colorGreen, colorReset)
	if status == string(StatusWaiting) || status == string(StatusThinking) {
		cycle := len(runes) + len(statusGradient)
		position := phase % cycle
		for i, glyph := range runes {
			color := colorFaded
			trailIndex := position - i
			if trailIndex >= 0 && trailIndex < len(statusGradient) {
				color = statusGradient[trailIndex]
			}
			fmt.Fprintf(out, "%s%c%s", color, glyph, colorReset)
		}
	} else {
		fmt.Fprint(out, colorFaded, status, colorReset)
	}
	fmt.Fprint(out, clearEOL)
}

// printHistory restores a saved transcript with live-output labels and truncation but no resume marker.
func (ui *PlainUI) printHistory(messages []Message) {
	for _, message := range messages {
		switch message.Role {
		case "user":
			fmt.Fprintf(ui.renderOut, "%syou>%s %s\n", colorCyan, colorReset, message.Content)
		case "assistant":
			if message.Content != "" {
				fmt.Fprintf(ui.renderOut, "%spicode>%s ", colorGreen, colorReset)
				renderMarkdown(ui.renderOut, message.Content)
			}
			for _, call := range message.ToolCalls {
				fmt.Fprintf(ui.renderOut, "%s%s>%s %s\n", colorYellow, call.Function.Name, colorReset,
					displayToolInput(call.Function.Name, call.Function.Arguments))
			}
		case "tool":
			fmt.Fprintf(ui.renderOut, "%s   output>%s ", colorYellow, colorReset)
			printTruncated(ui.renderOut, message.Content, 5, colorFaded)
			fmt.Fprintln(ui.renderOut)
		}
	}
}

// printSummary renders the final session token counts.
func (ui *PlainUI) printSummary(usage UsageTotals, sessionID string, resumable bool) {
	tokensAvailable := usage.Total() > 0 || (usage.Cost != nil && *usage.Cost > 0)
	if tokensAvailable {
		fmt.Fprintf(ui.renderOut, "\n%s session ended%s - %s%d%s tokens total, %s%d%s sent (+%s%d%s cached), %s%d%s received",
			ansiBlue, colorReset,
			ansiYellow, usage.Total(), colorReset,
			ansiCyan, usage.Prompt-usage.Cached, colorReset,
			colorFaded, usage.Cached, colorReset,
			ansiGreen, usage.Completion, colorReset)
		if usage.Cost != nil {
			fmt.Fprintf(ui.renderOut, ", %s$%.6f%s cost", ansiMagenta, *usage.Cost, colorReset)
		}
	} else {
		fmt.Fprintf(ui.renderOut, "\n%s session ended%s", ansiBlue, colorReset)
	}
	fmt.Fprintln(ui.renderOut)
	if resumable {
		fmt.Fprintf(ui.renderOut, "resume session with %spicode -resume %s%s\n", colorFaded, sessionID, colorReset)
	}
	fmt.Fprintln(ui.renderOut)
}

// printTruncated limits command output shown in the transcript.
func printTruncated(out io.Writer, output string, limit int, color string) {
	lines := strings.Split(output, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= limit {
		fmt.Fprint(out, color, output, colorReset)
		return
	}
	shown := strings.Join(lines[:limit], "\n")
	fmt.Fprint(out, color, shown, "\n(... ", len(lines)-limit, " more lines)", colorReset)
}
