package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// run wires configuration, services, the session, and the frontend.
func run() error {
	baseURL := flag.String("base-url", "http://localhost:8080", "server base URL")
	apiKey := flag.String("api-key", "", "API key (empty for local)")
	model := flag.String("model", "", "model name (empty = server default)")
	systemFlag := flag.String("system", "", "system prompt text (overrides the built-in default)")
	systemFileFlag := flag.String("system-file", "", "path to a file containing the system prompt")
	logSession := flag.Bool("log", false, "log full request JSON to stderr and ~/.picode/logs/<timestamp>.log")
	listSessions := flag.Bool("sessions", false, "list saved sessions for the current directory and exit")
	defaults := defaultLLMParameters()
	temperature := flag.Float64("temperature", defaults.Temperature, "sampling temperature (0 to 2)")
	topP := flag.Float64("top-p", defaults.TopP, "nucleus sampling probability (0 to 1)")
	maxCompletionTokens := flag.Int("max-completion-tokens", defaults.MaxCompletionTokens, "maximum tokens per model response")
	presencePenalty := flag.Float64("presence-penalty", defaults.PresencePenalty, "presence penalty (-2 to 2)")
	frequencyPenalty := flag.Float64("frequency-penalty", defaults.FrequencyPenalty, "frequency penalty (-2 to 2)")
	seed := flag.Int64("seed", 0, "best-effort deterministic sampling seed")
	serviceTier := flag.String("service-tier", defaults.ServiceTier, "service tier: none, auto, default, flex, or priority")
	reasoningEffort := flag.String("reasoning-effort", defaults.ReasoningEffort, "reasoning effort: low, medium, or high")
	verbosity := flag.String("verbosity", defaults.Verbosity, "response verbosity: low, medium, or high")
	var resume resumeFlag
	flag.Var(&resume, "resume", "resume the latest session, or a specific 12-character session ID")
	if err := flag.CommandLine.Parse(normalizeResumeArgs(os.Args[1:])); err != nil {
		return err
	}

	explicit := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	parameters, err := resolveLLMParameters(LLMParameters{
		Temperature: *temperature, TopP: *topP, MaxCompletionTokens: *maxCompletionTokens,
		PresencePenalty: *presencePenalty, FrequencyPenalty: *frequencyPenalty,
		ServiceTier: *serviceTier, ReasoningEffort: *reasoningEffort, Verbosity: *verbosity,
	}, *seed, explicit["seed"])
	if err != nil {
		return err
	}
	resuming := resume.Enabled
	if resuming {
		for _, name := range []string{"system", "system-file"} {
			if explicit[name] {
				return fmt.Errorf("-%s cannot be used when resuming a session", name)
			}
		}
	}

	workingDirectory, err := currentWorkingDirectory()
	if err != nil {
		return err
	}
	store, err := NewDefaultFileSessionStore()
	if err != nil {
		return err
	}
	if *listSessions {
		if resume.Enabled {
			return errors.New("-sessions cannot be used with -resume")
		}
		return printSessions(store, workingDirectory, os.Stdout)
	}

	var state *PersistedSession
	var lock SessionLock
	var startupWarnings []string
	if resume.SessionID != "" {
		state, err = store.Load(resume.SessionID)
		if err == nil {
			err = requireSessionWorkingDirectory(state, workingDirectory)
		}
	} else if resume.Enabled {
		state, err = store.LoadLatest(workingDirectory)
		if errors.Is(err, ErrSessionNotFound) {
			return fmt.Errorf("no saved sessions to resume in %q", workingDirectory)
		}
	} else {
		prompt, promptErr := resolveSystemPrompt(*systemFlag, *systemFileFlag)
		if promptErr != nil {
			return promptErr
		}
		state, lock, err = createAutomaticSession(store, prompt, true, *model, workingDirectory)
		startupWarnings = append(startupWarnings, prompt.Warnings...)
	}
	if err != nil {
		return err
	}

	if lock == nil {
		lock, err = store.Lock(state.ID)
		if err != nil {
			return fmt.Errorf("open session %q: %w", state.ID, err)
		}
	}
	defer lock.Close()

	// Reload after locking so a resume cannot use state saved just before the lock.
	if resuming {
		state, err = store.Load(state.ID)
		if err != nil {
			return err
		}
		if err := requireSessionWorkingDirectory(state, workingDirectory); err != nil {
			return err
		}
	}

	prompt := PromptResolution{
		Text:    state.System.BasePrompt,
		Enabled: state.System.Enabled,
	}

	// A resumed session uses its saved model unless this invocation explicitly
	// supplies a model override. Endpoints and credentials are never persisted.
	selectedModel := state.Model
	if !resuming || explicit["model"] {
		selectedModel = *model
		if resuming && selectedModel != state.Model {
			startupWarnings = append(startupWarnings,
				fmt.Sprintf("session used model %q; continuing with %q", state.Model, selectedModel))
			state.Model = selectedModel
		}
	}

	// PlainUI is the only frontend until a full TUI is added.
	var ui Frontend = NewPlainUI(os.Stdin, os.Stdout, os.Stderr)
	for _, warning := range startupWarnings {
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

	client := NewClient(*baseURL, *apiKey, selectedModel)
	client.Parameters = parameters
	client.Logger = logger
	client.Tools = allTools()

	executor := NewToolExecutor()
	session := NewSession(client, executor, prompt, logger, SessionSnapshot{
		Messages: state.Messages,
		Usage:    state.Usage,
	})
	conversation := NewPersistentConversation(session, store, state)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger.LogEvent("session started")
	err = ui.Run(ctx, conversation)
	logger.LogEvent("session ended")
	if len(conversation.History()) == 0 {
		if deleteErr := store.Delete(state.ID); deleteErr != nil {
			return errors.Join(err, fmt.Errorf("remove empty session %q: %w", state.ID, deleteErr))
		}
	}
	return err
}

// printSessions displays enough context to identify an older conversation
// without loading it into a new chat process.
func printSessions(store *FileSessionStore, workingDirectory string, out io.Writer) error {
	sessions, err := store.List(workingDirectory)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Fprintf(out, "No saved sessions in %s\n", workingDirectory)
		return nil
	}
	fmt.Fprintf(out, "Saved sessions in %s (newest first):\n", workingDirectory)
	for _, state := range sessions {
		turns := 0
		preview := "(empty session)"
		for _, message := range state.Messages {
			if message.Role != "user" {
				continue
			}
			turns++
			if preview == "(empty session)" {
				preview = sessionPreview(message.Content, 64)
			}
		}
		model := state.Model
		if model == "" {
			model = "default model"
		}
		fmt.Fprintf(out, "%s  %s  %3d turns  %-16s  %s\n",
			state.ID, state.UpdatedAt.Local().Format("2006-01-02 15:04"), turns, model, preview)
	}
	fmt.Fprintln(out, "\nResume one with: picode -resume <session-id>")
	return nil
}

func sessionPreview(content string, limit int) string {
	preview := strings.Join(strings.Fields(content), " ")
	if preview == "" {
		return "(blank first message)"
	}
	runes := []rune(preview)
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return preview
}

// resumeFlag supports both bare -resume (latest) and -resume=<session-id>.
type resumeFlag struct {
	Enabled   bool
	SessionID string
}

func (r *resumeFlag) String() string { return r.SessionID }

func (r *resumeFlag) Set(value string) error {
	if value == "false" {
		r.Enabled = false
		r.SessionID = ""
		return nil
	}
	r.Enabled = true
	if value == "true" {
		r.SessionID = ""
	} else {
		r.SessionID = value
	}
	return nil
}

func (r *resumeFlag) IsBoolFlag() bool { return true }

// normalizeResumeArgs lets the standard flag package accept the friendlier
// separated form, -resume <session-id>, in addition to -resume=<session-id>.
func normalizeResumeArgs(args []string) []string {
	normalized := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if (arg == "-resume" || arg == "--resume") && i+1 < len(args) &&
			!strings.HasPrefix(args[i+1], "-") {
			normalized = append(normalized, "-resume="+args[i+1])
			i++
			continue
		}
		normalized = append(normalized, arg)
	}
	return normalized
}

// createAutomaticSession allocates and exclusively creates a fresh session.
func createAutomaticSession(store *FileSessionStore, prompt PromptResolution, includeEnvironment bool, model, workingDirectory string) (*PersistedSession, SessionLock, error) {
	for attempts := 0; attempts < 10; attempts++ {
		id, err := GenerateSessionID()
		if err != nil {
			return nil, nil, err
		}
		lock, err := store.Lock(id)
		if errors.Is(err, ErrSessionLocked) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		now := time.Now()
		state := &PersistedSession{
			Version:          currentSessionVersion,
			ID:               id,
			CreatedAt:        now,
			UpdatedAt:        now,
			Model:            model,
			WorkingDirectory: workingDirectory,
			System: PersistedSystem{
				Enabled:            prompt.Enabled,
				BasePrompt:         prompt.Text,
				IncludeEnvironment: includeEnvironment,
			},
			Messages: []Message{},
		}
		if err := store.Create(state); errors.Is(err, ErrSessionExists) {
			lock.Close()
			continue
		} else if err != nil {
			lock.Close()
			return nil, nil, err
		}
		return state, lock, nil
	}
	return nil, nil, fmt.Errorf("could not allocate a unique session ID")
}
