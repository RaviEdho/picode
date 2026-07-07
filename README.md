# picode

Minimal CLI chat harness for local LLMs. Connects to any OpenAI-compatible `/v1/chat/completions` endpoint — primarily built for [llama-server](https://github.com/ggerganov/llama.cpp/tree/master/tools/server).

Zero dependencies. Stdlib only.

## Install

```bash
go build -o picode .
```

Or run directly:

```bash
go run .
```

## Usage

```bash
# defaults: localhost:8080, no auth, server picks model
picode

# custom endpoint
picode -base-url http://192.168.1.5:8080 -model llama3

# with API key (e.g. OpenAI)
picode -base-url https://api.openai.com -api-key sk-... -model gpt-4
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-base-url` | `http://localhost:8080` | Server base URL |
| `-api-key` | *(empty)* | Bearer token (empty = no auth) |
| `-model` | *(empty)* | Model name (empty = server default) |

## Chat

```
picode — type 'exit' or Ctrl-D to quit
you> hello
model> Hi there! How can I help you today?
you> what is 2+2
model> The answer is 4.
you> exit

session ended [↑280 🗘 120 ↓45 ∑445]
```

Conversation history accumulates for the full session. Exit with `exit`, `quit`, or `Ctrl-D`.

### Token Summary

Printed once on session exit:

| Symbol | Meaning |
|--------|---------|
| `↑` | Prompt tokens sent |
| `🗘` | Cached tokens (KV cache hits) |
| `↓` | Completion tokens received |
| `∑` | Total (sent + cached + received) |

## Project Structure

| File | Purpose |
|------|---------|
| `main.go` | Flags, REPL loop, I/O |
| `client.go` | HTTP client, chat completion request |
| `types.go` | OpenAI-compatible request/response structs |

## Requirements

- Go 1.21+
- A running LLM server with OpenAI-compatible API
