package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	colorReset  = "\033[0m"
	colorCyan   = "\033[1;36m"
	colorGreen  = "\033[1;32m"
	colorYellow = "\033[1;33m"
	colorFaded  = "\033[2;37m"
	clearEOL    = "\033[K"
)

type PlainUI struct {
	in  io.Reader
	out io.Writer
	err io.Writer

	mu sync.Mutex

	spinnerUpdate func(string)
	spinnerStop   func()
	textOpen      bool
	toolOpen      bool
	printedTools  map[int]bool
	printedCmdLen map[int]int
}

func NewPlainUI(in io.Reader, out, errOut io.Writer) *PlainUI {
	return &PlainUI{in: in, out: out, err: errOut}
}

func (ui *PlainUI) Warning(message string) {
	fmt.Fprintf(ui.err, "warning: %s\n", message)
}

func (ui *PlainUI) Run(ctx context.Context, session *Session) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigCh:
				if !session.CancelActiveTool() {
					cancel()
				}
			}
		}
	}()

	type inputResult struct {
		text string
		ok   bool
		err  error
	}
	input := make(chan inputResult, 1)
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

	fmt.Fprintln(ui.out, "picode — type 'exit' or Ctrl-D to quit")
	defer func() { ui.printSummary(session.Usage()) }()
	for {
		fmt.Fprintf(ui.out, "%syou>%s ", colorCyan, colorReset)
		select {
		case <-ctx.Done():
			return nil
		case result := <-input:
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

func (ui *PlainUI) Emit(event UIEvent) {
	ui.mu.Lock()
	defer ui.mu.Unlock()

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
		if event.Cancelled {
			fmt.Fprintf(ui.out, "%s^C cancelled run_command%s\n", colorYellow, colorReset)
		}
		fmt.Fprintf(ui.out, "%s   output>%s ", colorYellow, colorReset)
		printTruncated(ui.out, event.Output, 5, colorFaded)
		fmt.Fprintln(ui.out)
	case EmptyResponseEvent:
		fmt.Fprintln(ui.out, "(empty response)")
	}
}

func (ui *PlainUI) resetResponseState() {
	ui.textOpen = false
	ui.toolOpen = false
	ui.printedTools = make(map[int]bool)
	ui.printedCmdLen = make(map[int]int)
}

func (ui *PlainUI) stopSpinner() {
	if ui.spinnerStop != nil {
		ui.spinnerStop()
		ui.spinnerUpdate = nil
		ui.spinnerStop = nil
	}
}

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

func (ui *PlainUI) printSummary(usage UsageTotals) {
	fmt.Fprintf(ui.out, "\nsession ended - %d tokens total, %d sent (+%d cached), %d received\n\n",
		usage.Total(), usage.Prompt-usage.Cached, usage.Cached, usage.Completion)
}

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
