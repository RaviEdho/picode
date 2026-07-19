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

	// mu serializes event rendering with status animation output.
	mu sync.Mutex

	statusUpdate  func(string)
	statusStop    func()
	textOpen      bool
	markdownLive  streamingMarkdown
	toolOpen      bool
	activeTools   int
	printedTools  map[int]bool
	printedCmdLen map[int]int
}

// NewPlainUI creates a frontend around injectable terminal streams.
func NewPlainUI(in io.Reader, out, errOut io.Writer) *PlainUI {
	return &PlainUI{in: in, out: out, err: errOut}
}

// Warning writes a startup diagnostic to stderr.
func (ui *PlainUI) Warning(message string) {
	fmt.Fprintf(ui.err, "warning: %s\n", message)
}

// Run reads user input and drives the session until exit.
func (ui *PlainUI) Run(ctx context.Context, session Conversation) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	fmt.Fprintf(ui.out, "picode [%s] — type 'exit' or Ctrl-D to quit\n", session.SessionID())
	ui.printHistory(session.History())
	defer func() { ui.printSummary(session.Usage(), session.SessionID(), len(session.History()) > 0) }()
	for {
		input := scannedInput
		if editor != nil {
			result := make(chan inputResult, 1)
			input = result
			go func() {
				prompt := colorCyan + "you>" + colorReset + " "
				result <- editorInput(ctx, editor, prompt)
			}()
		} else {
			fmt.Fprintf(ui.out, "%syou>%s ", colorCyan, colorReset)
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
			if err := session.RunTurn(ctx, text, ui); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				fmt.Fprintf(ui.err, "error: %v\n", err)
			}
		}
	}
}

// Emit renders one semantic UI event.
func (ui *PlainUI) Emit(event UIEvent) {
	ui.mu.Lock()
	defer ui.mu.Unlock()

	// Each event updates only its related renderer state.
	switch event := event.(type) {
	case StatusEvent:
		if ui.statusStop == nil {
			ui.resetResponseState()
			ui.statusUpdate, ui.statusStop = ui.animateStatus(string(event.Phase))
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
				ui.statusUpdate, ui.statusStop = ui.animateStatus(fmt.Sprintf("running %d tools", ui.activeTools))
			} else if ui.statusUpdate != nil {
				ui.statusUpdate(fmt.Sprintf("running %d tools", ui.activeTools))
			}
		}
	case EmptyResponseEvent:
		fmt.Fprintln(ui.out, "(empty response)")
	}
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
func (ui *PlainUI) animateStatus(initial string) (update func(string), stop func()) {
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
			writeStatus(ui.out, status, phase)
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
			fmt.Fprintf(ui.out, "%syou>%s %s\n", colorCyan, colorReset, message.Content)
		case "assistant":
			if message.Content != "" {
				fmt.Fprintf(ui.out, "%spicode>%s ", colorGreen, colorReset)
				renderMarkdown(ui.out, message.Content)
			}
			for _, call := range message.ToolCalls {
				fmt.Fprintf(ui.out, "%s%s>%s %s\n", colorYellow, call.Function.Name, colorReset,
					displayToolInput(call.Function.Name, call.Function.Arguments))
			}
		case "tool":
			fmt.Fprintf(ui.out, "%s   output>%s ", colorYellow, colorReset)
			printTruncated(ui.out, message.Content, 5, colorFaded)
			fmt.Fprintln(ui.out)
		}
	}
}

// printSummary renders the final session token counts.
func (ui *PlainUI) printSummary(usage UsageTotals, sessionID string, resumable bool) {
	fmt.Fprintf(ui.out, "\n%s session ended%s - %s%d%s tokens total, %s%d%s sent (+%s%d%s cached), %s%d%s received",
		ansiBlue, colorReset,
		ansiYellow, usage.Total(), colorReset,
		ansiCyan, usage.Prompt-usage.Cached, colorReset,
		colorFaded, usage.Cached, colorReset,
		ansiGreen, usage.Completion, colorReset)
	if usage.Cost != nil {
		fmt.Fprintf(ui.out, ", %s$%.6f%s cost", ansiMagenta, *usage.Cost, colorReset)
	}
	fmt.Fprintln(ui.out)
	if resumable {
		fmt.Fprintf(ui.out, "resume session with %spicode -resume %s%s\n", colorFaded, sessionID, colorReset)
	}
	fmt.Fprintln(ui.out)
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
