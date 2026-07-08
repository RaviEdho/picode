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

## Tools

The model has access to `run_command` — a shell tool that executes commands on your machine. This lets it run any CLI tool (`curl`, `grep`, `psql`, etc.).

## Session

Exit with `exit`, `quit`, or `Ctrl-D`. On exit, a token summary is printed:

| Symbol | Meaning |
|--------|---------|
| `↑` | Prompt tokens sent |
| `🗘` | Cached tokens |
| `↓` | Completion tokens received |
| `∑` | Total |

## Requirements

- Go 1.21+
- An LLM server with OpenAI-compatible API
