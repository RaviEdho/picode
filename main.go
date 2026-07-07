package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	colorReset = "\033[0m"
	colorCyan  = "\033[1;36m"
	colorGreen = "\033[1;32m"
)

func main() {
	baseURL := flag.String("base-url", "http://localhost:8080", "llama-server base URL")
	apiKey := flag.String("api-key", "", "API key (empty for local)")
	model := flag.String("model", "", "model name (empty = server default)")
	flag.Parse()

	client := NewClient(*baseURL, *apiKey, *model)
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

		// spinner while waiting
		var wg sync.WaitGroup
		done := make(chan struct{})
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

		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			// remove failed user message
			history = history[:len(history)-1]
			continue
		}

		if len(resp.Choices) == 0 {
			fmt.Println("(empty response)")
			history = history[:len(history)-1]
			continue
		}

		assistant := resp.Choices[0].Message
		history = append(history, assistant)

		cached := 0
		if resp.Usage.PromptTokensDetails != nil {
			cached = resp.Usage.PromptTokensDetails.CachedTokens
		}
		totalPrompt += resp.Usage.PromptTokens
		totalCached += cached
		totalCompletion += resp.Usage.CompletionTokens
		fmt.Printf("%smodel>%s %s\n", colorGreen, colorReset, assistant.Content)
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "read error: %v\n", err)
	}
	fmt.Printf("\nsession ended [↑%d 🗘 %d ↓%d ∑%d]\n",
		totalPrompt, totalCached, totalCompletion,
		totalPrompt+totalCached+totalCompletion)
	fmt.Println()
}
