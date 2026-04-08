# Artificial

An open source AI agent harness for orchestrating multiple AI workers from a single dashboard. Built in Go.

Artificial lets you spawn, manage, and coordinate AI agents (Claude Code, OpenAI Codex, ACP-compatible models, local LLMs) as a team. Agents get personas, skills, communication channels, and a shared task board. You manage them from a real-time web dashboard or the REST API.

## What it does

- **Multi-agent orchestration** — run multiple AI agents in parallel, each with their own role and persona
- **Real-time dashboard** — chat with agents, manage tasks on a kanban board, monitor activity
- **Channel-based communication** — agents can message each other and receive notifications mid-task
- **Multiple backends** — supports Claude Code, OpenAI Codex, ACP (Agent Communication Protocol), and local models via OpenAI-compatible APIs
- **MCP integration** — each worker exposes tools to its agent via Model Context Protocol
- **Session persistence** — stop and resume agent sessions without losing context

## Architecture

```
┌───────────────────────────────────────────────────┐
│                 svc-artificial                    │
│       Dashboard · REST API · WebSocket Hub        │
│                   SQLite DB                       │
└─────────┬──────────────┬─────────────┬────────────┘
          │ WebSocket    │ WebSocket   │ WebSocket
    ┌─────┴──────┐  ┌────┴──────┐  ┌───┴────────┐
    │ cmd-worker │  │ cmd-worker│  │ cmd-worker │
    │  (Codex)   │  │ (Claude)  │  │   (ACP)    │
    │  ┌──────┐  │  │ ┌──────┐  │  │ ┌────────┐ │
    │  │ GPT  │  │  │ │Claude│  │  │ │Cursor/ │ │
    │  │ 5.4  │  │  │ │Opus  │  │  │ │opencode│ │
    │  └──────┘  │  │ └──────┘  │  │ └────────┘ │
    └────────────┘  └───────────┘  └────────────┘
```

## Quick start

```bash
# Build both binaries
make build

# Start the central service (dashboard at http://localhost:4000)
make run-artificial

# In another terminal, start a worker (or spawn from the Dashboard)
make run-worker EMPLOYEE_ID=1
```

### Prerequisites

- Go 1.25+
- Claude Code CLI, OpenAI Codex CLI, or an ACP-compatible agent server

### First steps

1. **Set up company knowledge** — create a folder with a README describing your project, conventions, and priorities (see [Company knowledge](#company-knowledge)). Set the path in dashboard Settings.
2. **Create a CEO** — add an employee with the "CEO" role from the dashboard. This is the lead agent that can hire and fire other workers, and spawn new agents on its own.
3. **Add a project** — set up the project you want the team to work on, with its path and description.
4. **Grow the team** — either add employees manually from the dashboard, or chat with the CEO and ask it to hire more people to work on the project. The CEO can spawn and manage workers autonomously.

## Project structure

```
src/
├── svc-artificial/     # Central service (dashboard, API, WebSocket hub, SQLite)
├── cmd-worker/         # Worker binary (agent lifecycle, MCP server, hub client)
└── pkg-go-shared/      # Shared protocol types
```

## Dashboard

The built-in web dashboard provides:

- **Chat** — direct messages and channel conversations with agents
- **Board** — kanban task management (todo, in progress, review, done)
- **Team** — view and manage active workers, spawn new agents
- **Live TTY** — stream agent terminal output in real-time

## Company knowledge

Create a folder that will serve as your team's shared knowledge base. This is where agents look for context about your project, conventions, and goals.

```bash
mkdir my-company
cat > my-company/README.md << 'EOF'
# My Company

## What we're building
Describe your product/project here.

## Conventions
- Language, framework, and style preferences
- How we name things, structure code, etc.

## Tools
- List CLI tools, scripts, or services agents can use
- e.g. `make test`, `npm run lint`, deployment commands
- Any internal APIs or databases they should know about

## Current priorities
- What the team is focused on right now
EOF
```

Once the service is running, set the knowledge path in the dashboard Settings. Every agent spawned will have access to this context.

## API

The REST API exposes endpoints for managing employees, tasks, channels, messages, and worker lifecycle. See `src/svc-artificial/internal/server/api.go` for the full list.

## Harness backends


| Backend      | Description                                                     | Status                                |
| ------------ | --------------------------------------------------------------- | ------------------------------------- |
| Claude Code  | Spawns Claude Code CLI via PTY with MCP tools                   | Tested                                |
| Codex        | Spawns OpenAI Codex CLI via PTY with MCP tools                  | Tested                                |
| ACP          | Connects to any Agent Communication Protocol server             | Tested with Cursor Agent and opencode |
| Local models | Via opencode + OpenAI-compatible APIs (LM Studio, ollama, etc.) | Works, but needs a strong local model |


## How I use it

I built [MyUpMonitor](https://myupmonitor.com) — a complete uptime monitoring SaaS with billing, teams, status pages, CLI, Terraform provider, and more — in about 24 hours of focused work using Artificial to orchestrate my AI development workflow.

## Author

Built by [André Baltazar](https://x.com/AndreBaltazar/)

## License

MIT