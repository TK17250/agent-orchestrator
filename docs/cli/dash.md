# `ao dash`

`ao dash` opens an interactive terminal dashboard. It is the terminal
counterpart of the desktop app's project sidebar and session board, and like
every other command it is a thin client over the loopback daemon API: it reads
the same session read model the desktop app renders. When no daemon is
running it starts one, and stops it again on quit (see
[Daemon lifecycle](#daemon-lifecycle)). Its only files are under
`<data dir>/dash/`.

```bash
ao dash                 # every registered project
ao dash --project web   # one project only (-p also works)
ao dash --keep-daemon   # leave a daemon dash starts running after quitting
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

- **Header:** the scope, session counts (working and needing you), who owns
  the daemon (`Daemon: started by dash`, or `Daemon: shared` for one the
  desktop app or another client runs), and the connection state: `● Live`
  while the change stream is attached, `○ Polling` while it is not,
  `● Daemon not reachable` when the daemon is down.
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

If the daemon stops while the dashboard is open, the last data stays on
screen under a banner that points at `ao start`, actions are refused, and the
dashboard recovers on its own when a daemon returns.

While the stream is attached, the dashboard also checks every 3 seconds that
the daemon still accepts new connections. A daemon that is shutting down
closes its listener at once but keeps serving open streams until its graceful
timeout, so an open stream would hold it up. When two checks fail in a row the
dashboard drops the stream and reconnects to whichever daemon runs next.

## Daemon lifecycle

### Starting

`ao dash` checks the daemon the same way `ao status` does, from `running.json`
and the daemon's `/healthz` and `/readyz`:

| Found | What dash does |
| --- | --- |
| A ready daemon | Uses it. |
| A live daemon that is not ready yet (another client is starting it) | Waits for it; never starts a second one. |
| No daemon, or a run file for a dead process | Starts one. |

To start one it runs the hidden `ao daemon` command of the same binary,
detached from the terminal in its own session, so closing the terminal does
not stop it. Its output goes to `<data dir>/dash/daemon.log`, replaced on each
start. It prints `Starting the Orchestrator daemon…`, waits up to 90 seconds for
the daemon to report ready, then opens the dashboard. `AO_DATA_DIR`,
`AO_RUN_FILE` and `AO_PORT` are honored like everywhere else.

The daemon is started without the desktop app's supervision variables
(`AO_OWNER`, `AO_APP_RUN_ID`, the browser runtime handshake) and without
session-scoped ones (`AO_SESSION_ID`, `AO_PROJECT_ID` and similar), which are
set when dash runs inside an AO session terminal. Without them the daemon
records itself as a headless daemon with its own app run id. dash records
which daemon it started (pid, start time, app run id) in
`<data dir>/dash/daemon.json`.

Starting and stopping are serialized across dash processes with a lock under
`<data dir>/dash/start/`, so two dashes opened at the same moment start one
daemon, not two.

### Leases

Every running dash holds a lease, an OS file lock under
`<data dir>/dash/leases/`. The kernel drops the lock when the process exits,
however it exits (crash and `kill -9` included), so a lease whose lock can be
taken belongs to a dash that is gone. Such stale leases are ignored and
removed. On a filesystem without file locks, a lease counts while the pid it
records is alive.

### Quitting

On `q`, `ctrl+c`, SIGTERM, SIGINT, or SIGHUP (the terminal closing), dash
stops the daemon only when all of these hold:

1. `--keep-daemon` was not given, and you did not choose to keep it.
2. The running daemon is exactly the one an `ao dash` started: same pid, start
   time and app run id, and no desktop owner tag in `running.json`.
3. No other `ao dash` holds a lease.
4. No session is mid-turn, unless you confirmed stopping anyway.

A daemon that was already running when dash opened is never stopped. When two
dashes share a daemon that a dash started, the first to quit leaves it
running and the last one stops it. dash uses the same graceful shutdown as
`ao stop` (`POST /shutdown`, then waits for the daemon to exit) and closes its
own change stream first so the daemon is not held open.

When it leaves a daemon it started running, dash says why, for example
`Left the Orchestrator daemon running: 1 other ao dash is still using it.`

### Sessions mid-turn

A session is mid-turn when it is not terminated and its status is `working`,
`no_signal` or `needs_input`. This is the same rule the desktop app uses before
a restart to update. Stopping the daemon affects these sessions as follows:

| Session | What a daemon stop does |
| --- | --- |
| Terminal (TUI) session | The agent keeps running in its detached pty-host. AO records nothing for it (activity, reports, PR changes) until a daemon starts again, then the new daemon re-adopts it. |
| Chat session with a persistent provider host (`chatProviderPreserved`) | The host keeps the agent connection and any in-flight turn, and replays its output to the next daemon. Counted like a terminal session. |
| Other Chat sessions | The agent process stops with the daemon and its in-flight turn is lost. The session itself remains and can be resumed. |

If quitting would stop the daemon while any session is mid-turn, dash asks
first and lists how many sessions would be interrupted and how many keep
running untracked:

| Key | Choice |
| --- | --- |
| `enter` | Stop the daemon (default) |
| `k` | Keep it running in the background (stop it later with `ao stop`) |
| `esc` | Cancel and stay in the dashboard |

`ctrl+c` in this prompt leaves without stopping anything. A signal cannot
ask, so SIGTERM, SIGINT and SIGHUP keep the daemon running when any session is
mid-turn, and stop it otherwise. Sessions are counted across all projects,
whatever `--project` shows.

### The desktop app

The desktop app attaches only to a daemon it launched itself, matched by app
run id. When it opens while a dash-started daemon is running, it gracefully
replaces that daemon with its own (`POST /shutdown`, then starts a new one).
Because a dash-started daemon is headless, the replacement is kept alive after
the app quits. The two never run side by side on one data directory.

dash follows the change: its stream reconnects to the new daemon, the header
switches to `Daemon: shared`, and on quit the daemon no longer matches
`daemon.json`, so dash leaves it running. The replacement interrupts Chat
sessions without a persistent host, as any daemon restart does.

## Daemon reads

| Purpose | Route |
| --- | --- |
| Projects | `GET /api/v1/projects` |
| Sessions | `GET /api/v1/sessions?active=true[&project=<id>]` |
| PR CI, review and thread counts | `GET /api/v1/sessions/{id}/pr` |
| Worker reports (details) | `GET /api/v1/reports?projectId=<id>` |
| Recent turns (details) | `GET /api/v1/sessions/{id}/history?limit=3` |
| Change stream | `GET /api/v1/events?after=latest` |
| Daemon identity and health | `running.json`, `GET /healthz`, `GET /readyz` |
| Stopping a daemon dash started | `POST /shutdown` |

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
