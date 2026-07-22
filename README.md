# picode

Short, dependency-free CLI chat for local LLMs using an OpenAI-compatible `/v1/chat/completions` server.

Terminal UI architecture: [`docs/terminal-ui.md`](docs/terminal-ui.md).

## Install

```bash
go build -o picode .       # Linux/macOS
go build -o picode.exe .   # Windows
```

## Use

```bash
go run .
```

## Flags

### Connection

| Flag | Default | Description |
|---|---|---|
| `-base-url` | `http://localhost:8080` | OpenAI-compatible server URL |
| `-api-key` | *(empty)* | Bearer token sent with requests |
| `-model` | *(empty)* | Model name; empty uses the server default |

### Prompt and sessions

| Flag | Default | Description |
|---|---|---|
| `-system` | *(empty)* | Inline system prompt |
| `-system-file` | *(empty)* | Read the system prompt from a file |
| `-resume [session-id]` | *(unset)* | Resume the latest or specified session |
| `-sessions` | `false` | List sessions for the current directory and exit |
| `-log` | `false` | Log full request JSON to stderr and `~/.picode/logs/` |

### Generation and cost

| Flag | Default | Description |
|---|---|---|
| `-temperature` | `0.2` | Sampling randomness (0-2); lower is more predictable |
| `-top-p` | `1` | Nucleus sampling (0-1); normally leave at 1 |
| `-max-completion-tokens` | `65536` | Initial response-token limit |
| `-reasoning-effort` | `high` | Reasoning level: `low`, `medium`, `high`, `xhigh`, or `max` |
| `-verbosity` | `low` | Response detail: `low`, `medium`, or `high` |

### Advanced and provider-specific

| Flag | Default | Description |
|---|---|---|
| `-presence-penalty` | `0` | Discourages staying on previously used topics (-2 to 2) |
| `-frequency-penalty` | `0` | Discourages repeated tokens (-2 to 2) |
| `-seed` | *(unset)* | Best-effort deterministic sampling seed |
| `-service-tier` | *(unset)* | Provider tier: `auto`, `default`, `flex`, or `priority` |
 Run `picode -h` for flag syntax.

## Tools

- `list_file` - list bounded directory contents with depth and entry limits.
- `read_file` - read bounded, numbered ranges from UTF-8 text files.
- `run_command` - execute local commands; 30-second timeout.
- `apply_patch` - safely edit files with structured patches.
- Windows commands use PowerShell; invoke `cmd.exe /d /c "..."` for Command Prompt.

## Sessions

Sessions are saved under `~/.picode/sessions/`, scoped to the working directory, and never store credentials.
The submitted prompt is checkpointed before a response starts, so a crash or
network failure leaves it available when the session is resumed.

```bash
picode -resume
picode -resume <session-id>
picode -sessions
```

Exit with `exit`, `quit`, or `Ctrl-D`.

## Multi-line input

The prompt supports multi-line messages. Press Enter to send. Insert a newline within the message instead with any of:

- **Shift+Enter** (kitty keyboard-protocol terminals)
- **Alt+Enter** / **Option+Enter** (Linux/macOS, works in most terminals)
- **Shift+Enter** or **Alt+Enter** on Windows

Use the Up and Down arrow keys to move between lines of the current message; at the top edge, Up recalls history, and at the bottom edge, Down returns to the draft or later history entries.

## System prompt

The built-in prompt can be overridden with `-system` or `-system-file`. Without those flags, Picode checks `PICODE_SYSTEM`, then `PICODE_SYSTEM_FILE`, then uses the built-in prompt. Runtime platform, shell, working directory, and date are appended automatically.

## Requirements

- Go 1.21+
- An OpenAI-compatible LLM server