package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// run wires configuration, services, the session, and the frontend.
func run() error {
	baseURL := flag.String("base-url", "http://localhost:8080", "llama-server base URL")
	apiKey := flag.String("api-key", "", "API key (empty for local)")
	model := flag.String("model", "", "model name (empty = server default)")
	systemFlag := flag.String("system", "", "system prompt text (overrides the built-in default)")
	systemFileFlag := flag.String("system-file", "", "path to a file containing the system prompt")
	noSystem := flag.Bool("no-system", false, "send no system message (original harness behaviour)")
	noEnvironment := flag.Bool("no-environment", false, "do not append runtime environment details to the system prompt")
	logSession := flag.Bool("log", false, "log full request JSON to stderr and ~/.picode/logs/<timestamp>.log")
	flag.Parse()

	// Resolve the system message before creating session resources.
	prompt, err := resolveSystemPrompt(*noSystem, *systemFlag, *systemFileFlag)
	if err != nil {
		return err
	}
	if prompt.Enabled && !*noEnvironment {
		prompt.Text += "\n\n" + buildEnvironmentBlock()
	}

	// PlainUI is the only frontend until a full TUI is added.
	var ui Frontend = NewPlainUI(os.Stdin, os.Stdout, os.Stderr)
	for _, warning := range prompt.Warnings {
		ui.Warning(warning)
	}

	// Logging is optional and remains a no-op when logger is nil.
	var logger *RequestLogger
	if *logSession {
		logger, err = NewRequestLogger()
		if err != nil {
			ui.Warning(fmt.Sprintf("could not create log file: %v", err))
		}
	}
	if logger != nil {
		defer logger.Close()
	}

	// The session coordinates the API client, tools, and frontend.
	client := NewClient(*baseURL, *apiKey, *model)
	client.Logger = logger
	client.Tools = allTools()

	executor := NewToolExecutor()
	session := NewSession(client, executor, prompt, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger.LogEvent("session started")
	err = ui.Run(ctx, session)
	logger.LogEvent("session ended")
	return err
}
