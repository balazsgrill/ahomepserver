# AHP Server (`ahomepserver`)

A standard-compliant [Agent Host Protocol (AHP)](https://microsoft.github.io/agent-host-protocol/) server in Go supporting synchronized multi-client AI agent sessions, pure state reducers, automated workflows, and external webhook triggers.

## Features

- **Agent Host Protocol (AHP 1.1.0) Standard Compliance**:
  - Transport: WebSocket with JSON-RPC 2.0 framing (one complete message per text frame).
  - Version negotiation: supports `1.1.0`, `1.0.0`, and compatible caret ranges via `ahptypes.NegotiateProtocolVersion`.
  - Monotonic `serverSeq` counter and replay ring buffer for reliable `reconnect` action replay.
  - Channels & State:
    - `ahp-root://`: exposes `RootState` containing loaded agent backends, models, skills, active session counts, and lifecycle notifications (`root/sessionAdded`, `root/sessionRemoved`, `root/sessionSummaryChanged`).
    - `ahp-session:/<uuid>`: session coordination scope managing session lifecycle, default chat routing, and chat catalogues.
    - `ahp-chat:/<cid>`: conversation channel with pure reducer state transitions (`chat/turnStarted`, `chat/delta`, `chat/toolCall*`, `chat/turnComplete`, `chat/error`).
    - `ahp-automations://`: durable automation catalogue with `runAutomation` execution.
    - `ahp-automation-run:/<id>`: automation run provenance and execution lifecycle.
- **Embedded Web UI as an AHP Client**:
  - The built-in Web UI (`GET /`) is a native AHP WebSocket client connecting to `ws://${location.host}/ahp`.
  - Real-time streaming of model responses, iterative tool execution steps, and live session updates.
- **External Webhook Triggers**:
  - Standard webhook trigger endpoint (`POST /webhook/{id}`) allowing external systems, alerting services, and home automation hubs to trigger automated workflows without speaking WebSocket.
- **Monitored Persona Folder or In-Memory Git Repo**:
  - **Local Folder**: Loads agent personas from a configured directory (`-personas-dir` / `AHP_PERSONAS_DIR`) and rescans on a configurable cadence (`-scan-interval` / `AHP_SCAN_INTERVAL`, default `30s`).
  - **In-Memory Git Repository (`go-git`)**: Optionally clones and periodically pulls a Git repository directly into memory (`memory.NewStorage()` + `memfs.New()`) via `-personas-git-url` (`AHP_PERSONAS_GIT_URL`).
- **Built-in Agent Loop & MCP Tool Discovery**:
  - OpenAI-compatible `/chat/completions` runner with dynamic MCP tool discovery (`tools/list`), tool calling (`tools/call`), and prompt hydration (`prompts/get`).
- **PostgreSQL Persistence**:
  - Stores sessions, messages, and workflow runs in PostgreSQL (or `memory://` for ephemeral/test runs).

## Persona Directory Layout

```text
personas/
└── home-assistant/
    ├── config.yaml
    ├── prompt.md
    └── skills/
        └── diagnostics.md
```

## Configuration

| Flag | Env Var | Default | Description |
|---|---|---|---|
| `-listen` | `AHP_LISTEN_ADDR` | `:8080` | HTTP & WebSocket server listen address |
| `-db` | `AHP_DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/ahomepserver?sslmode=disable` | PostgreSQL connection URL (or `memory://`) |
| `-personas-dir` | `AHP_PERSONAS_DIR` | `./personas` | Local directory containing persona subfolders |
| `-personas-git-url` | `AHP_PERSONAS_GIT_URL` | *(empty)* | Git repo URL cloned & synced into an in-memory `go-git` repository |
| `-personas-git-branch` | `AHP_PERSONAS_GIT_BRANCH` | *(empty)* | Optional Git branch to track for the in-memory persona repo |
| `-personas-git-subdir` | `AHP_PERSONAS_GIT_SUBDIR` | *(empty)* | Optional subdirectory inside the Git repo containing persona folders |
| `-personas-git-token-key` | `AHP_PERSONAS_GIT_TOKEN_KEY` | *(empty)* | Optional `token_key` for HTTPS authentication against the Git repo |
| `-scan-interval` | `AHP_SCAN_INTERVAL` | `30s` | Cadence for scanning the personas directory or pulling the in-memory Git repo |
| `-tokens-file` | `AHP_TOKENS_FILE` | *(empty)* | Optional `KEY=VALUE` file for resolving `token_key` references |
| `-api-key` | `AHP_API_KEY` | *(empty)* | Optional API key (`X-API-Key`, `Authorization: Bearer`, or `?token=`) |
| `-max-turns` | `AHP_MAX_AGENT_TURNS` | `10` | Max tool-calling iterations per agent turn |

## Protocols & Endpoints

### 1. Agent Host Protocol (WebSocket)
Connect via WebSocket to `ws://localhost:8080/ahp` (or `ws://localhost:8080/` with WebSocket upgrade):
- **Root Channel (`ahp-root://`)**:
  - `initialize`: Version handshake, initial channel subscriptions, snapshot delivery.
  - `ping`: Liveness check.
  - `reconnect`: Resumes session state and replays missed actions using `lastSeenServerSeq`.
  - `listSessions`: Fetches all persistent sessions.
  - `createSession`: Allocates a new session at `ahp-session:/<uuid>` and assigns default chat `ahp-chat:/<cid>`.
  - `disposeSession`: Closes and deletes a session and its associated chats.
- **Chat Channel (`ahp-chat:/<cid>`)**:
  - `dispatchAction`: Dispatches write-ahead actions such as `chat/turnStarted`.
  - Streams `chat/delta`, `chat/toolCallStart`, `chat/toolCallReady`, `chat/toolCallComplete`, and `chat/turnComplete`.
- **Automations Channel (`ahp-automations://`)**:
  - `runAutomation`: Executes an automation run and returns `ahp-automation-run:/<id>`.

### 2. External Webhook Triggers (HTTP POST)
- `POST /webhook/{id}` — Trigger an automated workflow run from external systems (e.g. GitHub webhooks, Home Assistant automations). Accepts an optional JSON payload and runs the automation asynchronously.

### 3. Management & Health Endpoints
- `GET /` — Embedded Web UI (AHP client)
- `GET /healthz` — Health check, server sequence, and persona scan status
- `POST /api/v1/personas/reload` — Trigger an immediate rescan of personas and broadcast `root/agentsChanged`
