package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var dashTestNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// fakeDashBackend records the actions the model asks for. It is safe for the
// concurrent use a running program would make of it.
type fakeDashBackend struct {
	mu       sync.Mutex
	snap     dashSnapshot
	snapErr  error
	detail   dashDetail
	sent     [][2]string
	killed   []string
	spawned  [][3]string
	spawnID  string
	spawnErr error
	details  []string
}

func (f *fakeDashBackend) Snapshot(context.Context, string) (dashSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap, f.snapErr
}

func (f *fakeDashBackend) Detail(_ context.Context, sess dashSession) (dashDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.details = append(f.details, sess.ID)
	d := f.detail
	d.SessionID = sess.ID
	return d, nil
}

func (f *fakeDashBackend) Send(_ context.Context, id, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, [2]string{id, message})
	return nil
}

func (f *fakeDashBackend) Kill(_ context.Context, id string) (killSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, id)
	return killSessionResponse{SessionID: id, Freed: false}, nil
}

func (f *fakeDashBackend) Spawn(_ context.Context, project, name, prompt string) (spawnedSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawned = append(f.spawned, [3]string{project, name, prompt})
	return spawnedSession{ID: f.spawnID}, f.spawnErr
}

func (f *fakeDashBackend) Watch(context.Context, string, chan<- dashStreamEvent) {}

func dashFixture() dashSnapshot {
	at := func(minutesAgo int) time.Time { return dashTestNow.Add(-time.Duration(minutesAgo) * time.Minute) }
	session := func(id, project, kind, name, harness, status, column, display, activity string, created int) dashSession {
		s := dashSession{}
		s.ID, s.ProjectID, s.Kind, s.DisplayName, s.Harness = id, project, kind, name, harness
		s.Status, s.KanbanColumn, s.DisplayStatus = status, column, display
		s.Activity = sessionActivity{State: activity, LastActivityAt: at(2)}
		s.CreatedAt = at(created)
		s.LastEventAt = at(created / 2)
		return s
	}
	orch := session("web-orch", "web", "orchestrator", "", "claude-code", "idle", "building", "Idle", "idle", 300)
	login := session("web-3", "web", "worker", "Fix login redirect", "codex", "working", "building", "Working", "active", 30)
	login.Branch = "ao/web-3/root"
	review := session("web-2", "web", "worker", "Add rate limiter", "claude-code", "review_pending", "needs_review", "Awaiting review", "idle", 90)
	review.Branch = "feat/rate-limit"
	review.PRList = []sessionListPR{{Number: 42, CI: "passing", Review: "review_required", Threads: intPtr(2)}}
	review.PRURLs = map[int]string{42: "https://github.com/acme/web/pull/42"}
	review.PRStates = map[int]string{42: "open"}
	failing := session("api-1", "api", "worker", "Migrate \x1b[31mschema\x1b[0m", "codex", "ci_failed", "validating", "CI failing", "idle", 60)
	failing.Branch = "ao/api-1/root"
	failing.PRList = []sessionListPR{{Number: 7, CI: "failing", Review: "changes_requested"}}
	ask := session("api-2", "api", "worker", "Pick a queue", "claude-code", "needs_input", "building", "Needs input", "waiting_input", 10)
	return dashSnapshot{
		Projects: []projectSummary{{ID: "web", Name: "Web app"}, {ID: "api", Name: "API"}},
		Sessions: []dashSession{orch, login, review, failing, ask},
	}
}

func newTestDashModel(t *testing.T, backend *fakeDashBackend, width, height int) dashModel {
	t.Helper()
	m := newDashModel(context.Background(), backend, "", nil, func() time.Time { return dashTestNow }).seeded(backend.snap)
	return dashUpdate(t, m, tea.WindowSizeMsg{Width: width, Height: height})
}

func dashUpdate(t *testing.T, m dashModel, msg tea.Msg) dashModel {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(dashModel)
}

func dashUpdateCmd(t *testing.T, m dashModel, msg tea.Msg) (dashModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	return next.(dashModel), cmd
}

func dashKey(name string) tea.KeyPressMsg {
	switch name {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	}
	r := []rune(name)[0]
	return tea.KeyPressMsg{Code: r, Text: name}
}

func dashType(t *testing.T, m dashModel, text string) dashModel {
	t.Helper()
	for _, r := range text {
		m = dashUpdate(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

func plainScreen(m dashModel) string {
	lines := strings.Split(ansi.Strip(m.render()), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.Join(lines, "\n")
}

func TestDashViewRendersSessionsGroupedByProject(t *testing.T) {
	m := newTestDashModel(t, &fakeDashBackend{snap: dashFixture()}, 150, 22)
	screen := plainScreen(m)

	want := []string{
		"Agent Orchestrator · All projects",
		"5 sessions · 1 working · 2 need you   ○ Polling",
		"Projects",
		"› All projects",
		"  API",
		"  Web app",
		"Session",
		"Status",
		"Branch",
		"Review",
		"API  2",
		"Web app  3",
		"▲ Pick a queue",
		"Worker       claude-code Needs input",
		"▲ Migrate schema",
		"CI failing          ao/api-1/r… #7     Failing Changes requested 30m ago",
		"   Session            Kind         Agent       Status              Branch      PR     CI      Review            Updated",
		"◐ Fix login redirect",
		"◆ Add rate limiter",
		"#42    Passing Review required",
		"○ web-orch",
		"Orchestrator",
		"j/k move · enter details",
	}
	for _, w := range want {
		if !strings.Contains(screen, w) {
			t.Fatalf("screen missing %q:\n%s", w, screen)
		}
	}
	// Escape sequences in session names are untrusted and must not reach the
	// terminal.
	if strings.Contains(m.render(), "\x1b[31m") {
		t.Fatalf("raw escape sequence from a session name reached the screen")
	}
	// Projects follow sidebar order; the orchestrator leads its project and
	// workers follow newest first.
	order := []string{"Pick a queue", "Migrate schema", "web-orch", "Fix login redirect", "Add rate limiter"}
	last := -1
	for _, name := range order {
		idx := strings.Index(screen, name)
		if idx <= last {
			t.Fatalf("row %q out of order:\n%s", name, screen)
		}
		last = idx
	}
	// The first row is selected on open.
	if !strings.Contains(screen, "›▲ Pick a queue") {
		t.Fatalf("first session should be selected:\n%s", screen)
	}
	if got := len(strings.Split(screen, "\n")); got != 22 {
		t.Fatalf("screen has %d lines, want the terminal height 22", got)
	}
}

func TestDashViewNarrowTerminalDropsLowPriorityColumns(t *testing.T) {
	m := newTestDashModel(t, &fakeDashBackend{snap: dashFixture()}, 58, 20)
	screen := plainScreen(m)
	if strings.Contains(screen, "Projects") {
		t.Fatalf("sidebar should collapse below 60 columns:\n%s", screen)
	}
	header := strings.Split(screen, "\n")[2]
	for _, gone := range []string{"Branch", "Agent", "Kind"} {
		if strings.Contains(header, gone) {
			t.Fatalf("column %q should be dropped at 58 columns:\n%s", gone, screen)
		}
	}
	for _, kept := range []string{"Session", "Status", "PR"} {
		if !strings.Contains(header, kept) {
			t.Fatalf("column %q should remain:\n%s", kept, screen)
		}
	}
	for _, line := range strings.Split(m.render(), "\n") {
		if w := ansi.StringWidth(line); w > 58 {
			t.Fatalf("line is %d cells wide, terminal is 58: %q", w, ansi.Strip(line))
		}
	}
}

func TestDashViewEmptyStates(t *testing.T) {
	m := newTestDashModel(t, &fakeDashBackend{}, 100, 12)
	if screen := plainScreen(m); !strings.Contains(screen, "No projects yet.") || !strings.Contains(screen, "ao project add") {
		t.Fatalf("empty install should point at ao project add:\n%s", screen)
	}
	m = newTestDashModel(t, &fakeDashBackend{snap: dashSnapshot{Projects: []projectSummary{{ID: "web", Name: "Web"}}}}, 100, 12)
	if screen := plainScreen(m); !strings.Contains(screen, "No active sessions.") || !strings.Contains(screen, "Press n to start a worker.") {
		t.Fatalf("empty project should offer n:\n%s", screen)
	}
}

func TestDashNavigationMovesSelectionAndScopesByProject(t *testing.T) {
	m := newTestDashModel(t, &fakeDashBackend{snap: dashFixture()}, 150, 22)
	if m.selectedID != "api-2" {
		t.Fatalf("initial selection = %q, want api-2", m.selectedID)
	}
	m = dashUpdate(t, m, dashKey("j"))
	m = dashUpdate(t, m, dashKey("down"))
	if m.selectedID != "web-orch" {
		t.Fatalf("after two moves selection = %q, want web-orch (crossing projects)", m.selectedID)
	}
	m = dashUpdate(t, m, dashKey("G"))
	if m.selectedID != "web-2" {
		t.Fatalf("G should select the last session, got %q", m.selectedID)
	}
	m = dashUpdate(t, m, dashKey("j"))
	if m.selectedID != "web-2" {
		t.Fatalf("moving past the end must clamp, got %q", m.selectedID)
	}

	m = dashUpdate(t, m, dashKey("tab"))
	if m.focus != paneProjects {
		t.Fatalf("tab should focus the projects pane")
	}
	m = dashUpdate(t, m, dashKey("j")) // API
	m = dashUpdate(t, m, dashKey("j")) // Web app
	if got := len(m.visibleSessions()); got != 3 {
		t.Fatalf("Web app scope shows %d sessions, want 3", got)
	}
	if m.selectedID != "web-orch" {
		t.Fatalf("scoping should select the project's first session, got %q", m.selectedID)
	}
	screen := plainScreen(m)
	if !strings.Contains(screen, "Agent Orchestrator · Web app") || strings.Contains(screen, "Pick a queue") {
		t.Fatalf("scoped screen should show only Web app:\n%s", screen)
	}
	m = dashUpdate(t, m, dashKey("enter"))
	if m.focus != paneSessions {
		t.Fatalf("enter on a project should focus the sessions pane")
	}
}

func TestDashSelectionSurvivesRefresh(t *testing.T) {
	backend := &fakeDashBackend{snap: dashFixture()}
	m := newTestDashModel(t, backend, 150, 22)
	m = dashUpdate(t, m, dashKey("G"))
	snap := dashFixture()
	snap.Sessions = append([]dashSession{{dashSessionDTO: dashSessionDTO{sessionDTO: sessionDTO{ID: "api-9", ProjectID: "api", CreatedAt: dashTestNow}}}}, snap.Sessions...)
	m = dashUpdate(t, m, dashSnapshotMsg{snap: snap})
	if m.selectedID != "web-2" {
		t.Fatalf("selection moved to %q after refresh, want web-2", m.selectedID)
	}
	snap.Sessions = snap.Sessions[:1]
	m = dashUpdate(t, m, dashSnapshotMsg{snap: snap})
	if m.selectedID != "api-9" {
		t.Fatalf("a vanished selection should fall back to the first row, got %q", m.selectedID)
	}
}

func TestDashFilterNarrowsAndEscClears(t *testing.T) {
	m := newTestDashModel(t, &fakeDashBackend{snap: dashFixture()}, 150, 22)
	m = dashUpdate(t, m, dashKey("/"))
	if m.mode != modeFilter {
		t.Fatalf("/ should open the filter")
	}
	m = dashType(t, m, "#42")
	if got := m.visibleSessions(); len(got) != 1 || got[0].ID != "web-2" {
		t.Fatalf("filter #42 = %v, want only web-2", got)
	}
	if m.selectedID != "web-2" {
		t.Fatalf("filter should move selection onto a match, got %q", m.selectedID)
	}
	m = dashUpdate(t, m, dashKey("enter"))
	if m.mode != modeBrowse || !strings.Contains(plainScreen(m), "Filtered by “#42” · esc clears") {
		t.Fatalf("enter keeps the filter and returns to browsing:\n%s", plainScreen(m))
	}
	m = dashUpdate(t, m, dashKey("esc"))
	if got := len(m.visibleSessions()); got != 5 {
		t.Fatalf("esc should clear the filter, %d sessions visible", got)
	}

	m = dashUpdate(t, m, dashKey("/"))
	m = dashType(t, m, "nothing-matches")
	if screen := plainScreen(m); !strings.Contains(screen, "No sessions match “nothing-matches”.") {
		t.Fatalf("no-match state missing:\n%s", screen)
	}
}

func TestDashDetailShowsMetadataPRReportsAndTurns(t *testing.T) {
	backend := &fakeDashBackend{snap: dashFixture(), detail: dashDetail{
		Reports: []dashReport{{State: "checkpoint", Note: "Added the limiter middleware", CreatedAt: dashTestNow.Add(-5 * time.Minute)}},
		Turns: []domain.HandoffTurn{{
			UserRequest:   "Add a rate limiter\nto the API",
			AgentResponse: "Done, see PR #42",
			At:            dashTestNow.Add(-20 * time.Minute),
		}},
		TurnsTotal: 4,
	}}
	m := newTestDashModel(t, backend, 120, 40)
	m = dashUpdate(t, m, dashKey("G")) // web-2
	m, cmd := dashUpdateCmd(t, m, dashKey("enter"))
	if !m.detailOpen || cmd == nil {
		t.Fatalf("enter should open details and fetch them")
	}
	if screen := plainScreen(m); !strings.Contains(screen, "Loading reports and turns…") {
		t.Fatalf("details should show loading first:\n%s", screen)
	}
	m = dashUpdate(t, m, cmd())
	screen := plainScreen(m)
	for _, w := range []string{
		"◆ Add rate limiter",
		"Awaiting review · In review",
		"Session    web-2",
		"Project    Web app",
		"Kind       Worker",
		"Agent      claude-code",
		"Branch     feat/rate-limit",
		"Activity   Idle · 2m ago",
		"Created    1h ago",
		"Pull requests",
		"#42 · Open · CI passing · Review required · 2 unresolved threads",
		"https://github.com/acme/web/pull/42",
		"Reports",
		"5m ago   Checkpoint   Added the limiter middleware",
		"Recent turns (1 of 4)",
		"20m ago  User: Add a rate limiter to the API",
		"Agent: Done, see PR #42",
		"j/k next session · esc back",
	} {
		if !strings.Contains(screen, w) {
			t.Fatalf("detail missing %q:\n%s", w, screen)
		}
	}

	m, cmd = dashUpdateCmd(t, m, dashKey("k"))
	if m.selectedID != "web-3" || cmd == nil {
		t.Fatalf("k in details should move to web-3 and fetch its details (got %q)", m.selectedID)
	}
	m = dashUpdate(t, m, cmd())
	m = dashUpdate(t, m, dashKey("esc"))
	if m.detailOpen {
		t.Fatalf("esc should close details")
	}
	if want := []string{"web-2", "web-3"}; !reflect.DeepEqual(backend.details, want) {
		t.Fatalf("detail fetches = %v, want %v", backend.details, want)
	}
}

func TestDashKillAsksFirst(t *testing.T) {
	backend := &fakeDashBackend{snap: dashFixture()}
	m := newTestDashModel(t, backend, 120, 24)

	m = dashUpdate(t, m, dashKey("x"))
	if m.mode != modeKill {
		t.Fatalf("x should ask for confirmation")
	}
	if screen := plainScreen(m); !strings.Contains(screen, "Kill Pick a queue (api-2)?") || !strings.Contains(screen, "Press y to kill") {
		t.Fatalf("confirmation missing:\n%s", screen)
	}
	m, cmd := dashUpdateCmd(t, m, dashKey("n"))
	if cmd != nil || len(backend.killed) != 0 || m.mode != modeBrowse {
		t.Fatalf("declining must not kill anything")
	}
	if !strings.Contains(plainScreen(m), "Kill cancelled") {
		t.Fatalf("decline should be acknowledged:\n%s", plainScreen(m))
	}

	m = dashUpdate(t, m, dashKey("x"))
	m, cmd = dashUpdateCmd(t, m, dashKey("y"))
	if cmd == nil || m.busy == "" {
		t.Fatalf("y should start the kill")
	}
	m, refresh := dashUpdateCmd(t, m, cmd())
	if !reflect.DeepEqual(backend.killed, []string{"api-2"}) {
		t.Fatalf("killed = %v, want [api-2]", backend.killed)
	}
	if refresh == nil {
		t.Fatalf("a finished action should refresh the board")
	}
	if screen := plainScreen(m); !strings.Contains(screen, "Session api-2 killed (workspace preserved)") {
		t.Fatalf("kill result missing:\n%s", screen)
	}
}

func TestDashSendMessage(t *testing.T) {
	backend := &fakeDashBackend{snap: dashFixture()}
	m := newTestDashModel(t, backend, 120, 24)
	m = dashUpdate(t, m, dashKey("m"))
	if m.mode != modeSend || !strings.Contains(plainScreen(m), "Message to Pick a queue") {
		t.Fatalf("m should open the composer:\n%s", plainScreen(m))
	}
	m, cmd := dashUpdateCmd(t, m, dashKey("enter"))
	if cmd != nil || m.mode != modeSend {
		t.Fatalf("an empty message must not be sent")
	}
	m = dashType(t, m, "Use SQS")
	m, cmd = dashUpdateCmd(t, m, dashKey("enter"))
	if cmd == nil || m.mode != modeBrowse {
		t.Fatalf("enter should send")
	}
	m = dashUpdate(t, m, cmd())
	if want := [][2]string{{"api-2", "Use SQS"}}; !reflect.DeepEqual(backend.sent, want) {
		t.Fatalf("sent = %v, want %v", backend.sent, want)
	}
	if !strings.Contains(plainScreen(m), "Message sent to Pick a queue") {
		t.Fatalf("send result missing:\n%s", plainScreen(m))
	}

	m = dashUpdate(t, m, dashKey("m"))
	m = dashUpdate(t, m, dashKey("esc"))
	if m.mode != modeBrowse || len(backend.sent) != 1 {
		t.Fatalf("esc should cancel without sending")
	}
}

func TestDashSpawnUsesSidebarProject(t *testing.T) {
	backend := &fakeDashBackend{snap: dashFixture(), spawnID: "web-4"}
	m := newTestDashModel(t, backend, 120, 30)
	m = dashUpdate(t, m, dashKey("tab"))
	m = dashUpdate(t, m, dashKey("j"))
	m = dashUpdate(t, m, dashKey("j")) // Web app
	m = dashUpdate(t, m, dashKey("n"))
	if m.mode != modeSpawn || !strings.Contains(plainScreen(m), "New worker in Web app") {
		t.Fatalf("n should open the new-worker form:\n%s", plainScreen(m))
	}
	m = dashType(t, m, "Add caching")
	m = dashUpdate(t, m, dashKey("enter")) // to the prompt field
	if m.spawnField != 1 {
		t.Fatalf("enter on the name should move to the prompt")
	}
	m = dashType(t, m, "Cache the user lookup")
	m, cmd := dashUpdateCmd(t, m, dashKey("enter"))
	if cmd == nil {
		t.Fatalf("enter on the prompt should start the worker")
	}
	if !strings.Contains(plainScreen(m), "Starting worker…") {
		t.Fatalf("spawn should show progress:\n%s", plainScreen(m))
	}
	m = dashUpdate(t, m, cmd())
	if want := [][3]string{{"web", "Add caching", "Cache the user lookup"}}; !reflect.DeepEqual(backend.spawned, want) {
		t.Fatalf("spawned = %v, want %v", backend.spawned, want)
	}
	if m.selectedID != "web-4" || !strings.Contains(plainScreen(m), "Started web-4 (Add caching)") {
		t.Fatalf("spawn should report and select the new session (selected %q):\n%s", m.selectedID, plainScreen(m))
	}
}

func TestDashSpawnValidatesName(t *testing.T) {
	backend := &fakeDashBackend{snap: dashFixture()}
	m := newTestDashModel(t, backend, 120, 30)
	m = dashUpdate(t, m, dashKey("n")) // selected session's project (api)
	if m.spawnTarget != "api" {
		t.Fatalf("spawn target = %q, want the selected session's project", m.spawnTarget)
	}
	m = dashUpdate(t, m, dashKey("tab"))
	m, cmd := dashUpdateCmd(t, m, dashKey("enter"))
	if len(backend.spawned) != 0 || m.mode != modeSpawn || m.spawnField != 0 {
		t.Fatalf("an empty name must keep the form open on the name field")
	}
	_ = cmd
	if !strings.Contains(plainScreen(m), "A task name is required") {
		t.Fatalf("missing name error:\n%s", plainScreen(m))
	}
}

func TestDashSpawnNeedsAProject(t *testing.T) {
	m := newTestDashModel(t, &fakeDashBackend{snap: dashSnapshot{Projects: []projectSummary{{ID: "a"}, {ID: "b"}}}}, 120, 20)
	m = dashUpdate(t, m, dashKey("n"))
	if m.mode != modeBrowse || !strings.Contains(plainScreen(m), "Select a project in the sidebar first") {
		t.Fatalf("n without a project should explain itself:\n%s", plainScreen(m))
	}
}

func TestDashDaemonDownShowsStartHintAndPollsFaster(t *testing.T) {
	m := newTestDashModel(t, &fakeDashBackend{snap: dashFixture()}, 120, 24)
	down := daemonUnavailableError{message: "Orchestrator daemon is not running — start it with `ao start`", cause: errDaemonNotRunning}
	m = dashUpdate(t, m, dashSnapshotMsg{err: down})
	screen := plainScreen(m)
	for _, w := range []string{"● Daemon not reachable", "The Orchestrator daemon is not reachable.", "Start it with `ao start`", "Fix login redirect"} {
		if !strings.Contains(screen, w) {
			t.Fatalf("daemon-down screen missing %q (stale rows stay visible):\n%s", w, screen)
		}
	}
	m = dashUpdate(t, m, dashKey("m"))
	if m.mode != modeBrowse || !strings.Contains(plainScreen(m), "The daemon is not reachable") {
		t.Fatalf("actions must be refused while the daemon is down")
	}

	clock := dashTestNow
	m.now = func() time.Time { return clock }
	m.lastFetch = clock
	clock = clock.Add(dashPollOffline)
	m, cmd := dashUpdateCmd(t, m, dashTickMsg(clock))
	if !m.refreshing || cmd == nil {
		t.Fatalf("an offline dashboard should poll every %s", dashPollOffline)
	}
	m = dashUpdate(t, m, dashSnapshotMsg{snap: dashFixture()})
	if m.daemonDown || strings.Contains(plainScreen(m), "The Orchestrator daemon is not reachable.") {
		t.Fatalf("a successful refresh should clear the daemon-down state")
	}
}

func TestDashLivePollsRarelyAndCoalescesChanges(t *testing.T) {
	m := newTestDashModel(t, &fakeDashBackend{snap: dashFixture()}, 120, 24)
	clock := dashTestNow
	m.now = func() time.Time { return clock }
	m.lastFetch = clock

	m, _ = dashUpdateCmd(t, m, dashStreamMsg{Connected: true})
	if !m.live || !strings.Contains(plainScreen(m), "● Live") {
		t.Fatalf("connected stream should show Live")
	}
	clock = clock.Add(dashPollOffline)
	m, _ = dashUpdateCmd(t, m, dashTickMsg(clock))
	if m.refreshing {
		t.Fatalf("a live dashboard must not poll every %s", dashPollOffline)
	}

	m, cmd := dashUpdateCmd(t, m, dashStreamMsg{Connected: true, Changed: true})
	if !m.refreshScheduled || cmd == nil {
		t.Fatalf("a change should schedule a debounced refresh")
	}
	m, cmd = dashUpdateCmd(t, m, dashStreamMsg{Connected: true, Changed: true})
	if cmd != nil {
		t.Fatalf("a second change inside the debounce window must not schedule another refresh")
	}
	m, cmd = dashUpdateCmd(t, m, dashRefreshMsg{})
	if !m.refreshing || cmd == nil {
		t.Fatalf("the debounce should start a refresh")
	}
	m, cmd = dashUpdateCmd(t, m, dashKey("r"))
	if cmd != nil || !m.refreshQueued {
		t.Fatalf("a refresh during a refresh should queue, not run in parallel")
	}
	m, cmd = dashUpdateCmd(t, m, dashSnapshotMsg{snap: dashFixture()})
	if cmd == nil || !m.refreshing || m.refreshQueued {
		t.Fatalf("the queued refresh should start when the first one lands")
	}

	m = dashUpdate(t, m, dashStreamMsg{Connected: false})
	if m.live || !strings.Contains(plainScreen(m), "○ Polling") {
		t.Fatalf("a dropped stream should fall back to polling")
	}
}

func TestDashHelpAndQuit(t *testing.T) {
	m := newTestDashModel(t, &fakeDashBackend{snap: dashFixture()}, 120, 30)
	m = dashUpdate(t, m, dashKey("?"))
	if screen := plainScreen(m); !strings.Contains(screen, "Keyboard shortcuts") || !strings.Contains(screen, "Kill the selected session (asks first)") {
		t.Fatalf("help missing:\n%s", screen)
	}
	m, cmd := dashUpdateCmd(t, m, dashKey("q"))
	if cmd != nil || m.mode != modeBrowse {
		t.Fatalf("q in help should close help, not quit")
	}
	_, cmd = dashUpdateCmd(t, m, dashKey("q"))
	if cmd == nil {
		t.Fatalf("q should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("q should return tea.Quit")
	}
}

func TestDashIndicatorMirrorsDesktopRowMarker(t *testing.T) {
	cases := []struct {
		name                            string
		status, column, display, active string
		want                            dashTone
	}{
		{"working wins over PR state", "ci_failed", "validating", "Fixing CI failures", "active", toneWorking},
		{"needs input", "needs_input", "building", "Needs input", "waiting_input", toneAttention},
		{"blocked activity", "working", "building", "Blocked", "blocked", toneAttention},
		{"ci failing", "ci_failed", "validating", "CI failing", "idle", toneAttention},
		{"changes requested", "changes_requested", "needs_review", "Changes requested", "idle", toneAttention},
		{"old daemon ci failed", "ci_failed", "", "", "idle", toneAttention},
		{"awaiting review", "review_pending", "needs_review", "Awaiting review", "idle", toneReview},
		{"draft stays quiet", "draft", "needs_review", "Draft", "idle", toneQuiet},
		{"AO review loop is quiet", "pr_open", "validating", "Reviewing", "idle", toneQuiet},
		{"AO review failed", "pr_open", "validating", "Review failed", "idle", toneReview},
		{"mergeable", "mergeable", "ready", "Mergeable", "idle", toneReady},
		{"merged", "merged", "ready", "Merged", "idle", toneMerged},
		{"idle", "idle", "building", "Idle", "idle", toneQuiet},
		{"terminated", "terminated", "archive", "Terminated", "active", toneQuiet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := dashSession{}
			s.Status, s.KanbanColumn, s.DisplayStatus = tc.status, tc.column, tc.display
			s.Activity.State = tc.active
			if got := dashIndicator(s); got != tc.want {
				t.Fatalf("dashIndicator = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDashCleanStripsTerminalControl(t *testing.T) {
	in := "a\x1b[2J\x1b]0;pwned\x07b\tc\n\nd\u202eе\x00"
	if got := dashClean(in); got != "ab c dе" {
		t.Fatalf("dashClean = %q", got)
	}
}

func TestReadDashEventsSignalsRelevantChangesOnly(t *testing.T) {
	body := strings.Join([]string{
		"id: 4", "event: cursor", "data: {}", "",
		"event: heartbeat", "data: {}", "",
		"id: 5", "event: session_updated", `data: {"seq":5,"projectId":"web","type":"session_updated"}`, "",
		"id: 6", "event: pr_updated", `data: {"seq":6,"projectId":"api","type":"pr_updated"}`, "",
		"id: 7", "event: session_created", `data: {"seq":7,"projectId":"","type":"session_created"}`, "",
	}, "\n") + "\n"
	count := 0
	if err := readDashEvents(strings.NewReader(body), "", func() { count++ }); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("unscoped changes = %d, want 3 (cursor and heartbeat carry no change)", count)
	}
	count = 0
	if err := readDashEvents(strings.NewReader(body), "web", func() { count++ }); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("web-scoped changes = %d, want 2 (its own and the unscoped one)", count)
	}
}

func dashDaemonServer(t *testing.T, events func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/projects":
			_, _ = io.WriteString(w, `{"projects":[{"id":"web","name":"Web app","kind":"git"},{"id":"api","name":"API","kind":"git"}]}`)
		case r.URL.Path == "/api/v1/sessions":
			if r.URL.Query().Get("active") != "true" {
				t.Errorf("dashboard must list active sessions, got %q", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"sessions":[{"id":"web-2","projectId":"web","kind":"worker","harness":"codex","displayName":"Add rate limiter",`+
				`"activity":{"state":"idle","lastActivityAt":"2026-11-12T11:58:00Z"},"isTerminated":false,"status":"review_pending",`+
				`"kanbanColumn":"needs_review","displayStatus":"Awaiting review","model":"gpt-5","createdAt":"2026-11-12T10:00:00Z","updatedAt":"2026-11-12T11:58:00Z",`+
				`"branch":"feat/rate-limit","prs":[{"url":"https://github.com/acme/web/pull/42","number":42,"state":"open","ci":"pending","review":"none"}]},`+
				`{"id":"web-3","projectId":"web","kind":"worker","activity":{"state":"active"},"status":"working","kanbanColumn":"building","displayStatus":"Working","prs":[]}]}`)
		case r.URL.Path == "/api/v1/sessions/web-2/pr":
			_, _ = io.WriteString(w, `{"prs":[{"number":42,"ci":{"state":"passing"},"review":{"decision":"approved","unresolvedThreadCount":1}}]}`)
		case r.URL.Path == "/api/v1/reports":
			if r.URL.Query().Get("projectId") != "web" {
				t.Errorf("reports query = %q", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"reports":[`+
				`{"id":"r1","sessionId":"web-2","projectId":"web","state":"checkpoint","note":"old","createdAt":"2026-11-12T10:00:00Z","repeatCount":1},`+
				`{"id":"r2","sessionId":"web-3","projectId":"web","state":"done","note":"other session","createdAt":"2026-11-12T11:00:00Z","repeatCount":1},`+
				`{"id":"r3","sessionId":"web-2","projectId":"web","state":"done","note":"new","createdAt":"2026-11-12T11:30:00Z","repeatCount":1}]}`)
		case r.URL.Path == "/api/v1/sessions/web-2/history":
			if r.URL.Query().Get("limit") != "3" {
				t.Errorf("history query = %q", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not_found","code":"HISTORY_UNAVAILABLE","message":"no history"}`)
		case r.URL.Path == "/api/v1/events" && events != nil:
			events(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDashBackendSnapshotMergesPRSummaries(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := dashDaemonServer(t, nil)
	writeRunFileFor(t, cfg, srv)
	backend := httpDashBackend{c: &commandContext{deps: Deps{ProcessAlive: func(int) bool { return true }}.withDefaults()}}

	snap, err := backend.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Projects) != 2 || len(snap.Sessions) != 2 {
		t.Fatalf("snapshot = %d projects, %d sessions", len(snap.Projects), len(snap.Sessions))
	}
	s := snap.Sessions[0]
	if s.KanbanColumn != "needs_review" || s.DisplayStatus != "Awaiting review" || s.Model != "gpt-5" {
		t.Fatalf("board fields not decoded: %+v", s.dashSessionDTO)
	}
	if len(s.PRList) != 1 || s.PRList[0].CI != "passing" || s.PRList[0].Review != "approved" || s.PRList[0].Threads == nil || *s.PRList[0].Threads != 1 {
		t.Fatalf("PR summary should override list facts: %+v", s.PRList)
	}
	if s.PRURLs[42] != "https://github.com/acme/web/pull/42" || s.PRStates[42] != "open" {
		t.Fatalf("PR link/state lost: %+v %+v", s.PRURLs, s.PRStates)
	}

	scoped, err := backend.Snapshot(context.Background(), "api")
	if err != nil || len(scoped.Projects) != 1 || scoped.Projects[0].ID != "api" {
		t.Fatalf("scoped snapshot = %+v, %v", scoped.Projects, err)
	}
	_, err = backend.Snapshot(context.Background(), "nope")
	if ExitCode(err) != 2 || !strings.Contains(fmt.Sprint(err), `project "nope" is not registered`) {
		t.Fatalf("unknown project should be a usage error, got %v", err)
	}

	detail, err := backend.Detail(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Reports) != 2 || detail.Reports[0].Note != "new" || detail.Reports[1].Note != "old" {
		t.Fatalf("reports should be this session's, newest first: %+v", detail.Reports)
	}
	if len(detail.Problems) != 1 || !strings.Contains(detail.Problems[0], "HISTORY_UNAVAILABLE") {
		t.Fatalf("a failed history read should be reported, not fatal: %v", detail.Problems)
	}
}

func TestDashBackendWatchFollowsStreamAndReportsDrops(t *testing.T) {
	cfg := setConfigEnv(t)
	release := make(chan struct{})
	srv := dashDaemonServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") != "latest" {
			t.Errorf("stream should start at head, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 3\nevent: cursor\ndata: {}\n\nid: 4\nevent: session_updated\ndata: {\"projectId\":\"web\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	writeRunFileFor(t, cfg, srv)
	backend := httpDashBackend{c: &commandContext{deps: Deps{ProcessAlive: func(int) bool { return true }}.withDefaults()}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan dashStreamEvent, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		backend.Watch(ctx, "", out)
	}()
	next := func() dashStreamEvent {
		t.Helper()
		select {
		case ev := <-out:
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a stream event")
			return dashStreamEvent{}
		}
	}
	if ev := next(); ev != (dashStreamEvent{Connected: true}) {
		t.Fatalf("first event = %+v, want connected", ev)
	}
	if ev := next(); ev != (dashStreamEvent{Connected: true, Changed: true}) {
		t.Fatalf("second event = %+v, want a change", ev)
	}
	close(release)
	if ev := next(); ev.Connected {
		t.Fatalf("a closed stream should report disconnected, got %+v", ev)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not stop after cancel")
	}
}

func TestDashCommandRequiresTerminal(t *testing.T) {
	setConfigEnv(t)
	_, _, err := executeCLI(t, Deps{In: strings.NewReader("")}, "dash")
	if !errors.Is(err, errDashNeedsTerminal) {
		t.Fatalf("dash without a terminal = %v, want errDashNeedsTerminal", err)
	}
	if ExitCode(err) != 1 {
		t.Fatalf("exit code = %d, want 1", ExitCode(err))
	}
	for _, hint := range []string{"ao status", "ao session ls"} {
		if !strings.Contains(err.Error(), hint) {
			t.Fatalf("error should suggest %s: %v", hint, err)
		}
	}
	_, _, err = executeCLI(t, Deps{}, "dash", "extra")
	if ExitCode(err) != 2 {
		t.Fatalf("positional args should be a usage error, got %v", err)
	}
}

func TestDashTableColumnsShareSpareWidth(t *testing.T) {
	widths := func(w int) map[string]int {
		out := map[string]int{}
		total := 2
		for _, c := range dashTableColumns(w) {
			out[c.title] = c.width
			total += c.width + 1
		}
		if total != w {
			t.Fatalf("columns at %d fill %d cells", w, total)
		}
		return out
	}
	if got := widths(139); got["Session"] != 32 || got["Branch"] != 17 {
		t.Fatalf("139 cells: session %d, branch %d; want 32 and 17", got["Session"], got["Branch"])
	}
	if got := widths(200); got["Session"] != 32+(200-139-14) || got["Branch"] != 31 {
		t.Fatalf("200 cells: session %d, branch %d", got["Session"], got["Branch"])
	}
	if got := widths(60); got["Branch"] != 0 || got["Agent"] != 0 || got["Session"] < 18 {
		t.Fatalf("60 cells should drop branch and agent and keep the name: %v", got)
	}
}
