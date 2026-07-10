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
	colorReset  = "\033[0m"
	colorCyan   = "\033[1;36m"
	colorGreen  = "\033[1;32m"
	colorYellow = "\033[1;33m"
	colorFaded  = "\033[2;37m"
	clearEOL    = "\033[K"
)

// PlainUI implements the current line-oriented terminal interface.
type PlainUI struct {
	in  io.Reader
	out io.Writer
	err io.Writer

	// mu serializes event rendering with spinner output.
	mu sync.Mutex

	spinnerUpdate func(string)
	spinnerStop   func()
	textOpen      bool
	toolOpen      bool
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

	// Terminal input uses the platform line editor. Pipes, files, and injected
	// test streams retain the scanner path.
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
	defer func() { ui.printSummary(session.Usage(), session.SessionID()) }()
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
			// The editor owns temporary terminal settings. Wait for its short
			// context poll so it restores them before the process can exit.
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
		if ui.spinnerStop == nil {
			ui.resetResponseState()
			ui.spinnerUpdate, ui.spinnerStop = ui.spinWithStatus(string(event.Phase))
		} else {
			ui.spinnerUpdate(string(event.Phase))
		}
	case AssistantDeltaEvent:
		ui.stopSpinner()
		if !ui.textOpen {
			fmt.Fprintf(ui.out, "%smodel>%s ", colorGreen, colorReset)
			ui.textOpen = true
		}
		fmt.Fprint(ui.out, event.Text)
	case ToolCallUpdateEvent:
		ui.stopSpinner()
		if !ui.printedTools[event.Index] {
			if ui.textOpen || ui.toolOpen {
				fmt.Fprintln(ui.out)
			}
			fmt.Fprintf(ui.out, "%srun_command>%s ", colorYellow, colorReset)
			ui.printedTools[event.Index] = true
			ui.toolOpen = true
		}
		printed := ui.printedCmdLen[event.Index]
		if len(event.Command) > printed {
			fmt.Fprint(ui.out, event.Command[printed:])
			ui.printedCmdLen[event.Index] = len(event.Command)
		}
	case StreamFinishedEvent:
		ui.stopSpinner()
		if ui.textOpen {
			fmt.Fprintln(ui.out)
		}
		if ui.toolOpen {
			fmt.Fprintln(ui.out)
		}
		ui.textOpen = false
		ui.toolOpen = false
	case ToolResultEvent:
		if event.Status == ToolCancelled {
			fmt.Fprintf(ui.out, "%s^C cancelled run_command%s\n", colorYellow, colorReset)
		}
		fmt.Fprintf(ui.out, "%s   output>%s ", colorYellow, colorReset)
		printTruncated(ui.out, event.Output, 5, colorFaded)
		fmt.Fprintln(ui.out)
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

// stopSpinner is safe to call after the spinner has already stopped.
func (ui *PlainUI) stopSpinner() {
	if ui.spinnerStop != nil {
		ui.spinnerStop()
		ui.spinnerUpdate = nil
		ui.spinnerStop = nil
	}
}

// spinWithStatus runs a spinner until its stop function is called.
func (ui *PlainUI) spinWithStatus(initial string) (update func(string), stop func()) {
	done := make(chan struct{})
	var once sync.Once
	var statusMu sync.Mutex
	status := initial
	var wg sync.WaitGroup
	wg.Add(1)
	fmt.Fprintf(ui.out, "\r%smodel>%s %s%s%s |%s", colorGreen, colorReset, colorFaded, status, colorReset, clearEOL)
	go func() {
		defer wg.Done()
		frames := []string{"|", "/", "-", "\\"}
		for i := 1; ; i++ {
			select {
			case <-done:
				return
			case <-time.After(100 * time.Millisecond):
			}
			statusMu.Lock()
			fmt.Fprintf(ui.out, "\r%smodel>%s %s%s %s%s%s", colorGreen, colorReset, colorFaded, status, frames[i%len(frames)], colorReset, clearEOL)
			statusMu.Unlock()
		}
	}()
	update = func(value string) {
		statusMu.Lock()
		status = value
		fmt.Fprintf(ui.out, "\r%smodel>%s %s%s%s |%s", colorGreen, colorReset, colorFaded, value, colorReset, clearEOL)
		statusMu.Unlock()
	}
	stop = func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			fmt.Fprint(ui.out, "\r"+strings.Repeat(" ", 60)+"\r")
		})
	}
	return update, stop
}

// printHistory restores the saved transcript using the same labels and
// truncation as live output. It intentionally adds no resume-specific marker.
func (ui *PlainUI) printHistory(messages []Message) {
	for _, message := range messages {
		switch message.Role {
		case "user":
			fmt.Fprintf(ui.out, "%syou>%s %s\n", colorCyan, colorReset, message.Content)
		case "assistant":
			if message.Content != "" {
				fmt.Fprintf(ui.out, "%smodel>%s %s\n", colorGreen, colorReset, message.Content)
			}
			for _, call := range message.ToolCalls {
				fmt.Fprintf(ui.out, "%srun_command>%s %s\n", colorYellow, colorReset,
					displayCommand(call.Function.Arguments))
			}
		case "tool":
			fmt.Fprintf(ui.out, "%s   output>%s ", colorYellow, colorReset)
			printTruncated(ui.out, message.Content, 5, colorFaded)
			fmt.Fprintln(ui.out)
		}
	}
}

// printSummary renders the final session token counts.
func (ui *PlainUI) printSummary(usage UsageTotals, sessionID string) {
	fmt.Fprintf(ui.out, "\nsession ended - %d tokens total, %d sent (+%d cached), %d received\n",
		usage.Total(), usage.Prompt-usage.Cached, usage.Cached, usage.Completion)
	fmt.Fprintf(ui.out, "resume session with %spicode -resume %s%s\n\n", colorFaded, sessionID, colorReset)
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
