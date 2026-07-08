package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	colorReset  = "\033[0m"
	colorCyan   = "\033[1;36m"
	colorGreen  = "\033[1;32m"
	colorYellow = "\033[1;33m"
)

func main() {
	baseURL := flag.String("base-url", "http://localhost:8080", "llama-server base URL")
	apiKey := flag.String("api-key", "", "API key (empty for local)")
	model := flag.String("model", "", "model name (empty = server default)")
	flag.Parse()

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
	scanner := bufio.NewScanner(os.Stdin)
	var history []Message
	var totalPrompt, totalCached, totalCompletion int

	fmt.Println("picode — type 'exit' or Ctrl-D to quit")

	for {
		fmt.Printf("%syou>%s ", colorCyan, colorReset)
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			break
		}

		history = append(history, Message{Role: "user", Content: input})

		resp, err := chatWithSpinner(client, history)

		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			// remove failed user message
			history = history[:len(history)-1]
			continue
		}

		// Handle tool calls in a loop until the model produces a final text response.
		for {
			if len(resp.Choices) == 0 {
				fmt.Println("(empty response)")
				history = history[:len(history)-1]
				break
			}

			assistant := resp.Choices[0].Message
			history = append(history, assistant)
			accumulateUsage(resp, &totalPrompt, &totalCached, &totalCompletion)

			if resp.Choices[0].FinishReason != "tool_calls" {
				fmt.Printf("%smodel>%s %s\n", colorGreen, colorReset, assistant.Content)
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
				output, cmdErr := runShellCommand(args.Command)
				if cmdErr != nil {
					output = fmt.Sprintf("error: %v", cmdErr)
				}
				fmt.Printf("%s   output>%s %s\n", colorYellow, colorReset, output)

				history = append(history, Message{Role: "tool", ToolCallID: tc.ID, Content: output})
			}

			// Continue the conversation with tool results.
			resp, err = chatWithSpinner(client, history)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				history = history[:len(history)-1]
				break
			}
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "read error: %v\n", err)
	}
	fmt.Printf("\nsession ended - %d tokens total, %d sent (+%d cached), %d received\n",
		totalPrompt+totalCached+totalCompletion,
		totalPrompt, totalCached, totalCompletion)
	fmt.Println()
}

func chatWithSpinner(client *Client, history []Message) (*ChatCompletionResponse, error) {
	// show a spinner while waiting for the model to respond
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		frames := []string{"|", "/", "-", "\\"}
		i := 0
		for {
			fmt.Printf("\r%smodel>%s thinking %s", colorGreen, colorReset, frames[i%len(frames)])
			i++
			select {
			case <-done:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()

	resp, err := client.ChatCompletion(history)

	close(done)
	wg.Wait()
	fmt.Print("\r" + strings.Repeat(" ", 60) + "\r") // clear spinner line
	return resp, err
}

func accumulateUsage(resp *ChatCompletionResponse, prompt, cached, completion *int) {
	*prompt += resp.Usage.PromptTokens
	if resp.Usage.PromptTokensDetails != nil {
		*cached += resp.Usage.PromptTokensDetails.CachedTokens
	}
	*completion += resp.Usage.CompletionTokens
}

func runShellCommand(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
