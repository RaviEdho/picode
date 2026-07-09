package main

import (
	"bufio"
	"context"
	"flag"
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
	colorFaded  = "\033[2;37m" // dim/bright-black-ish white for placeholder text
	clearEOL    = "\033[K"     // ANSI: clear from cursor to end of line
)

func main() {
	baseURL := flag.String("base-url", "http://localhost:8080", "llama-server base URL")
	apiKey := flag.String("api-key", "", "API key (empty for local)")
	model := flag.String("model", "", "model name (empty = server default)")
	systemFlag := flag.String("system", "", "system prompt text (overrides the built-in default)")
	systemFileFlag := flag.String("system-file", "", "path to a file containing the system prompt")
	noSystem := flag.Bool("no-system", false, "send no system message (original harness behaviour)")
	showSystem := flag.Bool("show-system", false, "print the resolved system prompt at startup and exit")
	flag.Parse()

	// Resolve the system prompt according to the precedence implemented in
	// resolveSystemPrompt (flags > env > built-in default).
	systemText, systemOn := resolveSystemPrompt(*noSystem, *systemFlag, *systemFileFlag)
	var systemMsg Message
	if systemOn {
		// Append the runtime environment block (OS/arch/shell/cwd/start time)
		// once at startup so the whole system message stays constant for the
		// session and the server can cache its prompt tokens.
		systemText = systemText + "\n\n" + buildEnvironmentBlock()
		systemMsg = Message{Role: "system", Content: systemText}
	}

	if *showSystem {
		if systemOn {
			fmt.Printf("--- system prompt (%d chars) ---\n%s\n--- end system prompt ---\n",
				len(systemText), systemText)
		} else {
			fmt.Println("--- no system prompt (disabled) ---")
		}
		return
	}

	// Session context: cancelled only when the user requests exit (or a fatal
	// interrupt while idle). We manage Ctrl-C manually below so that a Ctrl-C
	// while a command is running cancels just that command and keeps the
	// session alive, instead of always ending the session.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Manual interrupt handling: bisect Ctrl-C by whether a shell command is
	// currently executing. If one is running, cancel only that command
	// (commandRunning + currentCommandCancel in tools.go). If idle at the
	// prompt, end the whole session. This replaces signal.NotifyContext so the
	// two cases get different behaviour.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range sigCh {
			commandMu.Lock()
			running := commandRunning.Load()
			cancelFn := currentCommandCancel
			commandMu.Unlock()
			if running && cancelFn != nil {
				// Cancel only the in-flight command; session continues.
				cancelFn()
			} else {
				// Idle at prompt (or missing cancel fn): end the session.
				cancel()
			}
		}
	}()

	client := NewClient(*baseURL, *apiKey, *model)
	client.Tools = allTools()

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
				// Build the request message list fresh each turn: the (optional)
				// system prompt first, then the visible user/assistant/tool history.
				reqMessages := make([]Message, 0, len(history)+1)
				if systemOn {
					reqMessages = append(reqMessages, systemMsg)
				}
				reqMessages = append(reqMessages, history...)

				assistant, usage, finishReason, err := streamAssistant(ctx, client, reqMessages)
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
					// Fresh per-command context so a manual Ctrl-C can cancel only
					// this command (via currentCommandCancel) without ending the
					// session. It still inherits ctx, so a session exit also cancels.
					cmdCtx, cmdCancel := context.WithCancel(ctx)
					_, output := executeToolCall(cmdCtx, tc)
					cmdCancel()

					if strings.Contains(output, "command cancelled by user") {
						fmt.Printf("%s^C cancelled run_command%s\n", colorYellow, colorReset)
					}
					if ctx.Err() != nil {
						// Session itself was cancelled (e.g. Ctrl-C at idle prompt):
						// bail out, dropping the rest of the turn.
						break outer
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
	printedToolPrefix := make(map[int]bool)
	printedCmdLen := make(map[int]int) // tracks how many chars of the parsed command have been printed per tool call

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
			// Tool call: clear the placeholder and stream the tool
			// name + arguments live so the user sees immediate feedback.
			stop()
			for len(toolCalls) <= tc.Index {
				toolCalls = append(toolCalls, ToolCall{})
			}
			// Print prefix on first chunk for this tool call index.
			if !printedToolPrefix[tc.Index] {
				printedToolPrefix[tc.Index] = true
				// Newline if text content or a previous tool call line was open.
				if printedPrefix || tc.Index > 0 {
					fmt.Println()
				}
				fmt.Printf("%srun_command>%s ", colorYellow, colorReset)
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
				// Name already shown in the "run_command> " prefix.
			}
			if tc.Function.Arguments != "" {
				cur.Function.Arguments += tc.Function.Arguments
				raw := extractCommandValue(cur.Function.Arguments)
				cmd := unescapeJSONString(raw)
				// If the raw value ends with an odd number of backslashes,
				// the last escape sequence is still incomplete — withhold
				// the trailing character until the next chunk completes it.
				trailing := 0
				for i := len(raw) - 1; i >= 0 && raw[i] == '\\'; i-- {
					trailing++
				}
				display := cmd
				if trailing%2 == 1 && len(cmd) > 0 {
					display = cmd[:len(cmd)-1]
				}
				if len(display) > printedCmdLen[tc.Index] {
					fmt.Print(display[printedCmdLen[tc.Index]:])
					printedCmdLen[tc.Index] = len(display)
				}
			}
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}

	if printedPrefix {
		fmt.Println() // newline after streamed text
	}
	if len(printedToolPrefix) > 0 {
		fmt.Println() // newline after streamed tool call line
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

// extractCommandValue extracts the command string value from a (possibly
// partial) JSON arguments blob like {"command": "echo hello"}. It returns the
// raw JSON string content (still with escapes like \" and \\) up to the first
// unescaped closing quote, or everything after the opening quote if the closing
// quote hasn't arrived yet (streaming in progress).
func extractCommandValue(args string) string {
	idx := strings.Index(args, `"command"`)
	if idx < 0 {
		return ""
	}
	rest := args[idx+len(`"command"`):]
	rest = strings.TrimLeft(rest, " \t\r\n")
	if !strings.HasPrefix(rest, ":") {
		return ""
	}
	rest = rest[1:]
	rest = strings.TrimLeft(rest, " \t\r\n")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:] // skip opening "
	// Scan for the closing unescaped ".
	escaped := false
	for i, c := range rest {
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			return rest[:i]
		}
	}
	return rest // still streaming
}

// unescapeJSONString converts a raw JSON string value (without surrounding
// quotes) into a Go string, handling the standard JSON escape sequences.
func unescapeJSONString(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case '/':
			b.WriteByte('/')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		default:
			b.WriteByte(s[i])
			b.WriteByte(s[i+1])
		}
		i++ // skip the escaped character
	}
	return b.String()
}
