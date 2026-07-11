# picode

CLI chat for local LLMs. Connects to any OpenAI-compatible `/v1/chat/completions` endpoint. Zero dependencies, stdlib only.

## Install

```bash
# Linux / macOS
go build -o picode .

# Windows
go build -o picode.exe .
```

Or run directly on any platform:

```bash
go run .
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-base-url` | `http://localhost:8080` | Server base URL |
| `-api-key` | *(empty)* | Bearer token |
| `-model` | *(empty)* | Model name (empty = server default) |
| `-system` | *(empty)* | Inline system prompt text (overrides the built-in default) |
| `-system-file` | *(empty)* | Path to a file containing the system prompt |
| `-no-system` | `false` | Send **no** system message (original harness behaviour) |
| `-no-environment` | `false` | Do not append runtime details to the system prompt |
| `-resume [session-id]` | *(unset)* | Resume the latest session, or the specified 12-character session |
| `-sessions` | `false` | List saved sessions for the current directory and exit |

## System prompt

`picode` sends a **system message** before your first message so the model knows it
is a local, terminal-based coding assistant with `run_command` and `apply_patch` tools. The
prompt is resolved by the following precedence (highest first):

1. `-system "text"` flag — inline override
2. `-system-file path` flag — load from a file
3. `PICODE_SYSTEM` environment variable
4. `PICODE_SYSTEM_FILE` environment variable
5. Built-in default prompt (see `system.go`)

An unreadable file passed explicitly with `-system-file` is a startup error. An
unreadable `PICODE_SYSTEM_FILE`, or an empty prompt file from either source,
produces a warning and falls back to the built-in prompt.

The default prompt teaches the model about the 30-second tool timeout, a
read-only-first workflow, verification after changes, and safe handling of
destructive commands. Edit `defaultSystemPrompt` in `system.go` to customise it.

> **Tip:** because the system message is always the first (unchanged) message in
> every request, an OpenAI-compatible server will typically cache those prompt
> tokens — so the cost is near-zero after the first turn.

## Runtime environment awareness

At startup `picode` appends a `Runtime environment` section to the system prompt,
including custom prompts, so the model knows the real platform it is operating on.
This block contains:

- **Platform** — OS (e.g. `Linux`, `Windows`, `macOS`) and architecture
- **Shell / interpreter** — `sh -c` on POSIX, `cmd /c` on Windows — plus syntax guidance
- **Working directory**, quoted with control characters escaped
- **Current local date**

The block is captured **once at startup**, so the entire system message stays constant
for the session. Unlike a timestamp, the date also permits prompt-cache reuse across
sessions started on the same day. Use `-no-environment` to omit the block.

On **Windows**, `run_command` actually invokes `cmd /c` (Command Prompt). The tool
description and the runtime block both reflect this, so the model is told to use CMD
syntax (or prefix PowerShell) instead of POSIX `sh`. On POSIX systems the behavior is
unchanged from before.

## Tools

The model has access to `run_command` for executing local shell commands and `apply_patch` for structured, validated file edits. Patches support adding, updating, and deleting files relative to the working directory.

## Saved sessions

Every normal `picode` invocation automatically creates and records a new session.
The generated 12-character ID is shown in the startup banner:

```
picode [a7k2m9x4q1bz] — type 'exit' or Ctrl-D to quit
```

Sessions are saved after each completed turn under
`~/.picode/sessions/<session-id>.json`. Resuming reprints the saved transcript
before the next prompt so the terminal conversation continues where it stopped.
Resume the latest populated session or a specific session later with:

```bash
picode -resume
picode -resume a7k2m9x4q1bz
```

To find an older session (including empty sessions), list all sessions for the
current directory, newest first:

```bash
picode -sessions
```

Sessions are scoped to the canonical working directory where they were created.
A bare `-resume` selects the latest populated session from the current directory,
and `-resume <session-id>` rejects a session created in another directory. This
prevents a saved conversation from accidentally operating on a different project.

System-prompt flags cannot be changed while resuming; the saved base prompt is
reused and runtime environment details are rebuilt for the current process. The current base URL and API key are
always used and credentials are never saved. An explicitly supplied `-model`
overrides the saved model.

Session files contain the complete conversation, shell commands, and full tool
output, and are created with private permissions where the platform supports it.
Only completed turns are persisted, so an interrupted response does not corrupt
resumable history. Sessions with no completed turns are automatically deleted on
exit. Concurrent use of the same session is rejected.

## Session summary

Exit with `exit`, `quit`, or `Ctrl-D`. On exit, a token summary is printed:

```
session ended - 1234 tokens total, 1000 sent (+500 cached), 234 received
```

## Requirements

- Go 1.21+
- An LLM server with OpenAI-compatible API
