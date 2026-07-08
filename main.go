package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	colorFaded  = "\033[2;37m" // dim/bright-black-ish white for placeholder text
	clearEOL    = "\033[K"     // ANSI: clear from cursor to end of line
)

func main() {
	baseURL := flag.String("base-url", "http://localhost:8080", "llama-server base URL")
	apiKey := flag.String("api-key", "", "API key (empty for local)")
	model := flag.String("model", "", "model name (empty = server default)")
	flag.Parse()

	// Trap Ctrl-C (and SIGTERM) so the process is not killed with the
	// default "signal: interrupt" behaviour. This lets us unwind cleanly
	// and still print the session summary below.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client := NewClient(*baseURL, *apiKey, *model)
	client.Tools = []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "run_command",
			Description: "Execute a shell command and return its stdout/stderr",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"command": map[string]any{"type": "string", "description": "The shell command to execute"}},
				"required":   []string{"command"},
			},
		},
	}}

	// Read stdin in a goroutine so that Ctrl-C (which cancels ctx) can
	// interrupt a blocking read on the prompt instead of leaving Scan()
	// hanging forever.
	type inputResult struct {
		text string
		ok   bool
	}
	inCh := make(chan inputResult, 1)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
	readLoop:
		for scanner.Scan() {
			select {
			case inCh <- inputResult{text: scanner.Text(), ok: true}:
			case <-ctx.Done():
				break readLoop
			}
		}
		if err := scanner.Err(); err != nil {
			// Report a genuine read error so it isn't silently swallowed,
			// but never block on the channel if we were cancelled.
			select {
			case inCh <- inputResult{ok: false}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case inCh <- inputResult{ok: false}:
		case <-ctx.Done():
		}
	}()

	var history []Message
	var totalPrompt, totalCached, totalCompletion int

	fmt.Println("picode — type 'exit' or Ctrl-D to quit")

outer:
	for {
		fmt.Printf("%syou>%s ", colorCyan, colorReset)
		select {
		case <-ctx.Done():
			break outer
		case res := <-inCh:
			if !res.ok {
				break outer
			}
			input := strings.TrimSpace(res.text)
			if input == "" {
				continue
			}
			if input == "exit" || input == "quit" {
				break outer
			}

			history = append(history, Message{Role: "user", Content: input})

			// Loop until the model produces a final (non-tool-call) response.
			for {
				assistant, usage, finishReason, err := streamAssistant(ctx, client, history)
				if err != nil {
					if ctx.Err() != nil {
						// Interrupted: bail (we drop the whole session on Ctrl-C).
						break outer
					}
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
					// remove failed user message
					history = history[:len(history)-1]
					break
				}
				if assistant == nil {
					if ctx.Err() != nil {
						// Interrupted: bail (we drop the whole session on Ctrl-C).
						break outer
					}
					fmt.Println("(empty response)")
					history = history[:len(history)-1]
					break
				}

				history = append(history, *assistant)
				if usage != nil {
					accumulateUsage(*usage, &totalPrompt, &totalCached, &totalCompletion)
				}

				if finishReason != "tool_calls" {
					break
				}

				// Execute each tool call and append results to history.
				for _, tc := range assistant.ToolCalls {
					var args struct {
						Command string `json:"command"`
					}
					if uErr := json.Unmarshal([]byte(tc.Function.Arguments), &args); uErr != nil {
						history = append(history, Message{Role: "tool", ToolCallID: tc.ID, Content: fmt.Sprintf("error: invalid arguments: %v", uErr)})
						continue
					}

					fmt.Printf("%srun_command>%s %s\n", colorYellow, colorReset, args.Command)
					output, cmdErr := runShellCommand(ctx, args.Command)
					if ctx.Err() != nil {
						break outer
					}
					if cmdErr != nil {
						output = fmt.Sprintf("error: %v", cmdErr)
					}
					fmt.Printf("%s   output>%s %s\n", colorYellow, colorReset, output)

					history = append(history, Message{Role: "tool", ToolCallID: tc.ID, Content: output})
				}
			}
		}
	}

	// Ensure any lingering operations are cancelled before we print the summary.
	cancel()

	fmt.Printf("\nsession ended - %d tokens total, %d sent (+%d cached), %d received\n",
		totalPrompt+totalCached+totalCompletion,
		totalPrompt, totalCached, totalCompletion)
	fmt.Println()
}

// streamAssistant streams the model's response, printing tokens live.
// It returns the assembled assistant message, usage (if any), and finish reason.
func streamAssistant(ctx context.Context, client *Client, history []Message) (*Message, *Usage, string, error) {
	// Show "waiting for response" the instant the request is sent.
	update, stop := spinWithStatus("waiting for response")
	defer stop() // clears the line on every exit path

	stream, err := client.StreamChat(ctx, history)
	if err != nil {
		return nil, nil, "", err
	}
	defer stream.Close()

	var (
		content   strings.Builder
		toolCalls []ToolCall
		role      string
		finish    string
		usage     *Usage
	)

	gotFirstChunk := false
	printedPrefix := false

	for {
		// Bail out immediately if the user pressed Ctrl-C.
		select {
		case <-ctx.Done():
			return nil, nil, "", ctx.Err()
		default:
		}

		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, "", err
		}

		// Server has started responding: move from "waiting" to "thinking".
		if !gotFirstChunk {
			gotFirstChunk = true
			update("thinking")
		}

		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.FinishReason != nil {
			finish = *ch.FinishReason
		}
		d := ch.Delta
		if d.Role != "" {
			role = d.Role
		}
		if d.Content != "" {
			// First visible content: only now do we clear the spinner
			// and print the "model> " prefix.
			if !printedPrefix {
				printedPrefix = true
				stop()
				fmt.Printf("%smodel>%s ", colorGreen, colorReset)
			}
			fmt.Printf("%s", d.Content)
			content.WriteString(d.Content)
		}
		for _, tc := range d.ToolCalls {
			// Tool call: clear the placeholder; no "model> " text to print.
			stop()
			for len(toolCalls) <= tc.Index {
				toolCalls = append(toolCalls, ToolCall{})
			}
			cur := &toolCalls[tc.Index]
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Type != "" {
				cur.Type = tc.Type
			}
			if tc.Function.Name != "" {
				cur.Function.Name = tc.Function.Name
			}
			cur.Function.Arguments += tc.Function.Arguments
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}

	if printedPrefix {
		fmt.Println() // newline after streamed text
	}

	// Nothing produced (empty stream / no choices): return nil so the
	// caller shows "(empty response)" rather than a dangling cleared prompt.
	if content.Len() == 0 && len(toolCalls) == 0 {
		return nil, usage, finish, nil
	}

	msg := &Message{Role: role, Content: content.String()}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return msg, usage, finish, nil
}

// spinWithStatus shows a spinner with a status label until stop is invoked,
// then clears the line. The label can be changed via the returned update fn.
func spinWithStatus(initial string) (update func(string), stop func()) {
	done := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	status := initial
	var wg sync.WaitGroup
	wg.Add(1)
	// Print the placeholder immediately so the user sees feedback the
	// moment the message is sent.
	fmt.Printf("\r%smodel>%s %s%s%s |%s", colorGreen, colorReset, colorFaded, status, colorReset, clearEOL)
	go func() {
		defer wg.Done()
		frames := []string{"|", "/", "-", "\\"}
		i := 1
		for {
			select {
			case <-done:
				return
			case <-time.After(100 * time.Millisecond):
			}
			mu.Lock()
			s := status
			fmt.Printf("\r%smodel>%s %s%s %s%s%s", colorGreen, colorReset, colorFaded, s, frames[i%len(frames)], colorReset, clearEOL)
			mu.Unlock()
			i++
		}
	}()
	update = func(s string) {
		mu.Lock()
		status = s
		fmt.Printf("\r%smodel>%s %s%s%s |%s", colorGreen, colorReset, colorFaded, s, colorReset, clearEOL)
		mu.Unlock()
	}
	stop = func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			fmt.Print("\r" + strings.Repeat(" ", 60) + "\r") // clear spinner line
		})
	}
	return update, stop
}

func accumulateUsage(u Usage, prompt, cached, completion *int) {
	*prompt += u.PromptTokens
	if u.PromptTokensDetails != nil {
		*cached += u.PromptTokensDetails.CachedTokens
	}
	*completion += u.CompletionTokens
}

func runShellCommand(ctx context.Context, command string) (string, error) {
	// Derive a child context that inherits cancellation from the caller
	// (e.g. Ctrl-C) but also enforces a hard 30s timeout per command.
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
	out, err := cmd.CombinedOutput()
	// trim trailing whitespace so output stays compact (no spurious blank lines)
	return strings.TrimRight(string(out), "\n\t "), err
}
