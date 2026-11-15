# AHP Server (`ahomepserver`)

An Agent Home Protocol (AHP) server in Go supporting interactive **end-user sessions** (with a built-in Web UI and HTTP/SSE API) and **automated workflows** triggered by external servers.

## Features

- **Monitored Persona Folder or In-Memory Git Repo**:
  - **Local Folder**: Loads agent personas from a configured directory (`-personas-dir` / `AHP_PERSONAS_DIR`) and rescans on a configurable cadence (`-scan-interval` / `AHP_SCAN_INTERVAL`, default `30s`).
  - **In-Memory Git Repository (`go-git`)**: Optionally clones and periodically pulls a Git repository directly into memory (`memory.NewStorage()` + `memfs.New()`) via `-personas-git-url` (`AHP_PERSONAS_GIT_URL`), with optional `-personas-git-branch`, `-personas-git-subdir`, and `-personas-git-token-key`.
- **Persona Structure**:
  - `config.yaml` (or `persona.yaml`): LLM endpoint, model, `token_key`, MCP server URLs with `token_key` & transport (`http` or `sse`), and optional workflow templates.
  - `prompt.md` (or `instructions.md`): Base instructions (system prompt).
  - `skills/*.md` or `skills/<name>/SKILL.md`: Markdown skill definitions with optional YAML frontmatter (`name`, `description`).
- **Token Key Resolution**: Secrets are never hardcoded in the git-synced persona files. Each `token_key` (e.g. `OPENAI_API_KEY` or `HOME_MCP_TOKEN`) is resolved from:
  1. An optional `-tokens-file` (`AHP_TOKENS_FILE`) `KEY=VALUE` file (reloaded on each scan)
  2. `AHP_TOKEN_<KEY>` environment variable
  3. `<KEY>` environment variable
- **Built-in Agent Loop**: OpenAI-compatible `/chat/completions` client with iterative MCP tool discovery (`tools/list`), tool execution (`tools/call`), and dynamic `load_skill` tool support.
- **PostgreSQL Persistence**: Stores sessions, messages, and workflow runs (with step-by-step tool traces) in PostgreSQL (auto-migrated on startup), or `memory://` for ephemeral/test runs.

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
| `-listen` | `AHP_LISTEN_ADDR` | `:8080` | HTTP server listen address |
| `-db` | `AHP_DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/ahomepserver?sslmode=disable` | PostgreSQL connection URL (or `memory://`) |
| `-personas-dir` | `AHP_PERSONAS_DIR` | `./personas` | Local directory containing persona subfolders |
| `-personas-git-url` | `AHP_PERSONAS_GIT_URL` | *(empty)* | Git repo URL cloned & synced into an in-memory `go-git` repository |
| `-personas-git-branch` | `AHP_PERSONAS_GIT_BRANCH` | *(empty)* | Optional Git branch to track for the in-memory persona repo |
| `-personas-git-subdir` | `AHP_PERSONAS_GIT_SUBDIR` | *(empty)* | Optional subdirectory inside the Git repo containing persona folders |
| `-personas-git-token-key` | `AHP_PERSONAS_GIT_TOKEN_KEY` | *(empty)* | Optional `token_key` for HTTPS authentication against the Git repo |
| `-scan-interval` | `AHP_SCAN_INTERVAL` | `30s` | Cadence for scanning the personas directory or pulling the in-memory Git repo |
| `-tokens-file` | `AHP_TOKENS_FILE` | *(empty)* | Optional `KEY=VALUE` file for resolving `token_key` references |
| `-api-key` | `AHP_API_KEY` | *(empty)* | Optional API key (`X-API-Key` or `Authorization: Bearer`) |
| `-max-turns` | `AHP_MAX_AGENT_TURNS` | `10` | Max tool-calling iterations per agent turn |

## HTTP API Summary

- `GET /` — Embedded Web UI (Sessions chat, Workflow trigger & run history, Personas inspector)
- `GET /healthz` — Health check & persona directory scan status
- `GET /api/v1/personas` — List all loaded personas and folder monitor status
- `POST /api/v1/personas/reload` — Trigger an immediate rescan of the personas directory
- `POST /api/v1/sessions` — Create a new end-user session (`{"persona_id": "...", "title": "..."}`)
- `GET /api/v1/sessions` — List sessions
- `GET /api/v1/sessions/{id}` — Get session and message history
- `POST /api/v1/sessions/{id}/messages` — Send a message (`?stream=true` or `Accept: text/event-stream` for live SSE events)
- `POST /api/v1/workflows/trigger` — Trigger an automated workflow (`{"persona_id": "...", "workflow_name": "...", "prompt": "...", "input": {...}, "async": true, "webhook_url": "..."}`)
- `GET /api/v1/workflows/runs` — List automated workflow runs
- `GET /api/v1/workflows/runs/{id}` — Inspect a workflow run and its step-by-step tool trace

