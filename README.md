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

## System prompt

`picode` sends a **system message** before your first message so the model knows it
is a local, terminal-based coding assistant with a `run_command` shell tool. The
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

The model has access to `run_command` — a shell tool that executes commands on your machine. This lets it run any CLI tool (`curl`, `grep`, `psql`, etc.).

## Session

Exit with `exit`, `quit`, or `Ctrl-D`. On exit, a token summary is printed:

```
session ended - 1234 tokens total, 1000 sent (+500 cached), 234 received
```

## Requirements

- Go 1.21+
- An LLM server with OpenAI-compatible API
