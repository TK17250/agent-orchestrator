# `ao dash`

`ao dash` opens an interactive terminal dashboard. It is the terminal
counterpart of the desktop app's project sidebar and session board, and like
every other command it is a thin client over the loopback daemon API: it reads
the same session read model the desktop app renders and changes nothing
outside the daemon.

```bash
ao dash                 # every registered project
ao dash --project web   # one project only (-p also works)
```

The dashboard needs an interactive terminal on both stdin and stdout. When
either is a pipe or a file, it exits 1 and suggests `ao status` or
`ao session ls` for plain or scripted output. There is no `--json` mode; use
`ao session ls --json` for that.

## Layout

```
Agent Orchestrator · All projects               5 sessions · 1 working · 2 need you   ● Live
──────────────────────────────────────────────────────────────────────────────────────────────
Projects                 │    Session            Kind         Agent       Status          …
› All projects         5 │ API  2
  API                  2 │ ›▲ Pick a queue       Worker       claude-code Needs input     …
  Web app              3 │  ▲ Migrate schema     Worker       codex       CI failing      …
                         │
                         │ Web app  3
                         │  ○ web-orch           Orchestrator claude-code Idle            …
                         │  ◐ Fix login redirect Worker       codex       Working         …
                         │  ◆ Add rate limiter   Worker       claude-code Awaiting review …
──────────────────────────────────────────────────────────────────────────────────────────────
j/k move · enter details · tab projects · / filter · m message · x kill · n new worker · ? help
```

- **Header:** the scope, session counts (working and needing you), and the
  connection state: `● Live` while the change stream is attached, `○ Polling`
  while it is not, `● Daemon not reachable` when the daemon is down.
- **Projects sidebar:** a compact directory, as in the desktop app. Choosing a
  project scopes the session list to it. It collapses below 60 columns.
- **Sessions:** grouped by project. Within a project the orchestrator leads
  and workers follow newest first. The order never depends on status, so rows
  do not jump when a session changes state. Columns are session name, kind
  (worker or orchestrator), agent, status, branch, PR, CI, review and last
  update. Narrow terminals drop branch, agent, review, kind, update and CI, in
  that order.
- **Details** (`enter`): status and board lane, session id, project, kind,
  agent and model, branch with commit and push state, issue, activity, age,
  each pull request with its state, CI, review decision, unresolved threads
  and link, the session's five newest worker reports, and its three most
  recent turns.

### Status vocabulary

The status text is the daemon's `displayStatus`, the same phrase the desktop
board prints (for example "Needs input", "CI failing", "Awaiting review",
"Mergeable"). The details view adds the board lane: Building, Validating, In
review, or Ready. The dashboard never derives status of its own.

Each row has one status glyph, chosen the way the desktop sidebar chooses its
row indicator. Every glyph has its own shape as well as its colour:

| Glyph | Meaning |
| --- | --- |
| `◐` blue | The agent is working (or the session is starting). |
| `▲` red | Needs you: waiting for input, blocked, CI failing, merge conflicts, or changes requested. |
| `◆` amber | In review and the next turn is a person's, or AO's review failed. |
| `✓` green | Ready: mergeable or approved. Purple when merged. |
| `○` grey | Nothing needed: idle, draft, or AO's own review loop. |

## Keys

| Key | Action |
| --- | --- |
| `j` / `k`, `↓` / `↑` | Move in the focused pane |
| `g` / `G` | First / last |
| `tab`, `h` / `l` | Switch between the projects and sessions panes |
| `enter` | Open session details; on a project, focus its sessions |
| `esc` | Close details, or clear the filter |
| `/` | Filter sessions by name, id, branch, agent, status, or PR number (`#42`) |
| `m` | Send a message to the selected session |
| `x` | Kill the selected session (asks first) |
| `n` | Start a new worker |
| `r` | Refresh now |
| `?` | Show or hide help |
| `q`, `ctrl+c` | Quit |

In details, `j` / `k` move to the next or previous session and keep details
open.

## Actions

Actions call the same daemon routes and shared CLI code as their commands:

| Action | Equivalent command | Route |
| --- | --- | --- |
| `m` send a message (`enter` sends, `ctrl+j` adds a newline) | `ao send --session <id> --message <text>` | `POST /api/v1/sessions/{id}/send` |
| `x` kill, after a `y` confirmation | `ao session kill <id>` | `POST /api/v1/sessions/{id}/kill` |
| `n` new worker with a task name and an optional prompt | `ao spawn --project <id> --name <name> --prompt <prompt>` | `POST /api/v1/sessions`, then `GET /api/v1/sessions/{id}` until the start settles |

A new worker goes to the project selected in the sidebar, else the selected
session's project, else the only project in view. It uses the project's
configured worker agent; if the project has none, the dashboard shows the
same error `ao spawn` does. A kill keeps a worktree that has uncommitted
changes, as `ao session kill` does, and the result line says so.

## Live updates

The dashboard follows `GET /api/v1/events?after=latest`, the change stream the
desktop app uses. A burst of changes is coalesced into one refresh. While the
stream is attached the dashboard also refreshes every 30 seconds as a safety
net; while it is down, or the daemon is unreachable, it polls every 3 seconds
and reconnects to the stream with backoff.

If the daemon is not running when `ao dash` starts, the command exits 1 with
the usual hint to run `ao start`. If the daemon stops while the dashboard is
open, the last data stays on screen under a banner that points at `ao start`,
actions are refused, and the dashboard recovers on its own when the daemon
returns.

## Daemon reads

| Purpose | Route |
| --- | --- |
| Projects | `GET /api/v1/projects` |
| Sessions | `GET /api/v1/sessions?active=true[&project=<id>]` |
| PR CI, review and thread counts | `GET /api/v1/sessions/{id}/pr` |
| Worker reports (details) | `GET /api/v1/reports?projectId=<id>` |
| Recent turns (details) | `GET /api/v1/sessions/{id}/history?limit=3` |
| Change stream | `GET /api/v1/events?after=latest` |

Session names, reports and turns are untrusted agent output. The dashboard
removes terminal escape sequences and control characters from every value
before drawing it.

## Not yet supported

- Attaching to a session's terminal or chat from the dashboard. Use the
  desktop app.
- Archived (terminated) sessions, restore and delete. Use
  `ao session ls --include-terminated`, `ao session restore`, and
  `ao session delete`.
- Choosing the agent, model, branch or issue for a new worker. Use `ao spawn`.
- PR actions (merge, resolve comments) and AO reviews. Use `ao pr` and
  `ao review`.
