package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

func TestDecideDashDaemonStop(t *testing.T) {
	startedAt := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	ours := &runfile.Info{PID: 100, Port: 3001, StartedAt: startedAt, AppRunID: "apprun-dash"}
	record := &dashDaemonRecord{PID: 100, StartedAt: startedAt, AppRunID: "apprun-dash"}
	guiPersistent := &runfile.Info{PID: 200, Port: 3001, StartedAt: startedAt.Add(time.Minute), Owner: "persistent", AppRunID: "apprun-gui"}
	sameProcessTaggedByApp := &runfile.Info{PID: 100, Port: 3001, StartedAt: startedAt, Owner: "app", AppRunID: "apprun-dash"}
	busy := dashInFlight{Interrupted: 1, Unsupervised: 2}

	cases := []struct {
		name  string
		facts dashStopFacts
		want  dashStopDecision
	}{
		{"started by dash, idle, last client", dashStopFacts{Record: record, Current: ours, CurrentAlive: true}, dashStopDecision{Stop: true}},
		{"daemon already gone", dashStopFacts{Record: record, Current: nil}, dashStopDecision{Reason: "the daemon is not running"}},
		{"run file points at a dead process", dashStopFacts{Record: record, Current: ours, CurrentAlive: false}, dashStopDecision{Reason: "the daemon is not running"}},
		{"was already running when dash started", dashStopFacts{Record: nil, Current: ours, CurrentAlive: true}, dashStopDecision{Reason: "it was not started by ao dash"}},
		{"desktop app replaced it", dashStopFacts{Record: record, Current: guiPersistent, CurrentAlive: true}, dashStopDecision{Reason: "it was not started by ao dash"}},
		{"desktop owner tag never matches", dashStopFacts{Record: record, Current: sameProcessTaggedByApp, CurrentAlive: true}, dashStopDecision{Reason: "it was not started by ao dash"}},
		{"restarted daemon with the same pid", dashStopFacts{Record: record, Current: &runfile.Info{PID: 100, StartedAt: startedAt.Add(time.Hour), AppRunID: "apprun-dash"}, CurrentAlive: true}, dashStopDecision{Reason: "it was not started by ao dash"}},
		{"--keep-daemon", dashStopFacts{KeepFlag: true, Record: record, Current: ours, CurrentAlive: true}, dashStopDecision{Reason: "--keep-daemon was given"}},
		{"user chose keep", dashStopFacts{Choice: exitKeepDaemon, Record: record, Current: ours, CurrentAlive: true}, dashStopDecision{Reason: "you chose to keep it running"}},
		{"another dash still open", dashStopFacts{Record: record, Current: ours, CurrentAlive: true, OtherLeases: 1}, dashStopDecision{Reason: "1 other ao dash is still using it"}},
		{"other dashes win over a confirmed stop", dashStopFacts{Choice: exitStopDaemon, Record: record, Current: ours, CurrentAlive: true, OtherLeases: 2}, dashStopDecision{Reason: "2 other ao dash are still using it"}},
		{"sessions mid-turn need confirmation", dashStopFacts{Record: record, Current: ours, CurrentAlive: true, InFlight: busy}, dashStopDecision{NeedsConfirm: true, Reason: "3 sessions are mid-turn"}},
		{"confirmed stop with sessions mid-turn", dashStopFacts{Choice: exitStopDaemon, Record: record, Current: ours, CurrentAlive: true, InFlight: busy}, dashStopDecision{Stop: true}},
		{"keep flag wins over sessions", dashStopFacts{KeepFlag: true, Record: record, Current: ours, CurrentAlive: true, InFlight: busy}, dashStopDecision{Reason: "--keep-daemon was given"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideDashDaemonStop(tc.facts); got != tc.want {
				t.Fatalf("decision = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCountDashInFlightMirrorsDesktopUpdateRisk(t *testing.T) {
	s := func(mode, status string, preserved, terminated bool) dashSessionDTO {
		var d dashSessionDTO
		d.Mode, d.Status, d.ChatProviderPreserved, d.IsTerminated = mode, status, preserved, terminated
		return d
	}
	got := countDashInFlight([]dashSessionDTO{
		s("chat", "working", false, false),     // interrupted
		s("chat", "needs_input", false, false), // interrupted: a turn parked for input
		s("chat", "no_signal", true, false),    // persistent host: keeps running
		s("tui", "working", false, false),      // pty-host outlives the daemon
		s("", "working", false, false),         // no mode: not Chat
		s("chat", "idle", false, false),        // not mid-turn
		s("chat", "working", false, true),      // terminated
		s("tui", "mergeable", false, false),    // not mid-turn
	})
	if want := (dashInFlight{Interrupted: 2, Unsupervised: 3}); got != want {
		t.Fatalf("in flight = %+v, want %+v", got, want)
	}
}

func TestDashDaemonEnvDropsSupervisionAndSessionVariables(t *testing.T) {
	got := dashDaemonEnv([]string{
		"PATH=/usr/bin", "AO_DATA_DIR=/x/data", "AO_RUN_FILE=/x/running.json", "AO_PORT=3047", "AO_TMUX_BINARY=/opt/tmux",
		"AO_OWNER=app", "AO_APP_RUN_ID=apprun-gui", "AO_KEEP_DAEMON=1", "AO_BROWSER_RUNTIME_TOKEN_STDIN=1",
		"AO_SESSION_ID=web-3", "AO_PROJECT_ID=web", "AO_RUNTIME_LAUNCH_ID=l1", "AO_PATH_PIN=/x/bin",
	})
	want := []string{"PATH=/usr/bin", "AO_DATA_DIR=/x/data", "AO_RUN_FILE=/x/running.json", "AO_PORT=3047", "AO_TMUX_BINARY=/opt/tmux"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
}

func TestDashLeasesCountLiveAndReapStale(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "leases")
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	self, err := acquireDashLease(ctx, dir, 11, now)
	if err != nil {
		t.Fatal(err)
	}
	defer self.Release()
	other, err := acquireDashLease(ctx, dir, 22, now)
	if err != nil {
		t.Fatal(err)
	}
	alive := func(int) bool { return true }
	if n, err := countOtherDashLeases(ctx, dir, self.dir, alive); err != nil || n != 1 {
		t.Fatalf("other leases = %d, %v; want 1", n, err)
	}

	// A crashed dash leaves its directory but no lock.
	stale := filepath.Join(dir, "33-crashed")
	if err := os.MkdirAll(stale, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, dashLeaseFileName), []byte(`{"pid":33}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := countOtherDashLeases(ctx, dir, self.dir, alive); err != nil || n != 1 {
		t.Fatalf("with a stale lease = %d, %v; want 1 (stale ones never count)", n, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale lease should be removed, stat err = %v", err)
	}

	other.Release()
	if n, err := countOtherDashLeases(ctx, dir, self.dir, alive); err != nil || n != 0 {
		t.Fatalf("after release = %d, %v; want 0", n, err)
	}
}

func TestLeasePIDAliveFallback(t *testing.T) {
	dir := t.TempDir()
	if leasePIDAlive(dir, func(int) bool { return true }) {
		t.Fatal("a lease without lease.json is not alive")
	}
	if err := os.WriteFile(filepath.Join(dir, dashLeaseFileName), []byte(`{"pid":42}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !leasePIDAlive(dir, func(pid int) bool { return pid == 42 }) {
		t.Fatal("a lease whose pid is alive is alive")
	}
	if leasePIDAlive(dir, func(int) bool { return false }) {
		t.Fatal("a lease whose pid is gone is stale")
	}
}

// fakeDashDaemon serves the probes, session list and /shutdown of a daemon
// whose run file names this test process.
type fakeDashDaemon struct {
	t        *testing.T
	cfg      testConfig
	srv      *httptest.Server
	mu       sync.Mutex
	running  bool
	sessions string
	spawned  []processStartConfig
	shutdown int
}

var dashTestDaemonStart = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

func newFakeDashDaemon(t *testing.T) *fakeDashDaemon {
	t.Helper()
	f := &fakeDashDaemon{t: t, cfg: setConfigEnv(t), sessions: `{"sessions":[]}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz", "/readyz":
			status := "ok"
			if r.URL.Path == "/readyz" {
				status = "ready"
			}
			_ = json.NewEncoder(w).Encode(probeResult{Status: status, Service: "agent-orchestrator-daemon", PID: os.Getpid()})
		case "/api/v1/sessions":
			f.mu.Lock()
			body := f.sessions
			f.mu.Unlock()
			_, _ = w.Write([]byte(body))
		case "/shutdown":
			f.mu.Lock()
			f.shutdown++
			f.running = false
			f.mu.Unlock()
			_ = runfile.Remove(f.cfg.runFile)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// writeRunFile publishes a running daemon with the given owner identity.
func (f *fakeDashDaemon) writeRunFile(owner, appRunID string, startedAt time.Time) {
	f.t.Helper()
	f.mu.Lock()
	f.running = true
	f.mu.Unlock()
	if err := runfile.Write(f.cfg.runFile, runfile.Info{
		PID: os.Getpid(), Port: serverPort(f.t, f.srv.URL), StartedAt: startedAt, Owner: owner, AppRunID: appRunID,
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeDashDaemon) deps() Deps {
	return Deps{
		Executable: func() (string, error) { return "/opt/ao/bin/ao", nil },
		StartProcess: func(cfg processStartConfig) error {
			f.mu.Lock()
			f.spawned = append(f.spawned, cfg)
			f.mu.Unlock()
			// The daemon publishes a headless identity: no owner, its own run id.
			f.writeRunFile("", "apprun-dash-daemon", dashTestDaemonStart)
			return nil
		},
		ProcessAlive: func(int) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.running
		},
		Sleep: func(time.Duration) {},
	}.withDefaults()
}

func (f *fakeDashDaemon) newDash(t *testing.T, keep bool) *dashDaemon {
	t.Helper()
	d, err := newDashDaemon(&commandContext{deps: f.deps()}, keep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func (f *fakeDashDaemon) counts() (spawned, shutdown int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.spawned), f.shutdown
}

func TestDashDaemonStartsWhenNoneRunsAndStopsOnQuit(t *testing.T) {
	f := newFakeDashDaemon(t)
	t.Setenv("AO_APP_RUN_ID", "apprun-gui-inherited")
	t.Setenv("AO_SESSION_ID", "web-3")
	d := f.newDash(t, false)
	var out bytes.Buffer
	if err := d.Start(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if spawned, _ := f.counts(); spawned != 1 {
		t.Fatalf("spawned %d daemons, want 1", spawned)
	}
	cfg := f.spawned[0]
	if cfg.Path != "/opt/ao/bin/ao" || !reflect.DeepEqual(cfg.Args, []string{"daemon"}) {
		t.Fatalf("spawn = %s %v, want the current binary's hidden daemon command", cfg.Path, cfg.Args)
	}
	wantLog := filepath.Join(f.cfg.dataDir, "dash", "daemon.log")
	if cfg.Stdout == nil || cfg.Stdout.Name() != wantLog || cfg.Stderr.Name() != wantLog {
		t.Fatalf("daemon output should go to %s", wantLog)
	}
	for _, kv := range cfg.Env {
		if strings.HasPrefix(kv, "AO_APP_RUN_ID=") || strings.HasPrefix(kv, "AO_SESSION_ID=") {
			t.Fatalf("spawned daemon inherited %s", kv)
		}
	}
	if !strings.Contains(out.String(), "Starting the Orchestrator daemon… (log: "+wantLog+")") {
		t.Fatalf("start progress missing: %q", out.String())
	}
	if role := d.Role(); role != "Daemon: started by dash" {
		t.Fatalf("role = %q", role)
	}

	if plan, err := d.Plan(context.Background()); err != nil || plan.NeedsConfirm {
		t.Fatalf("idle quit should need no confirmation: %+v, %v", plan, err)
	}
	out.Reset()
	if err := d.Finish(context.Background(), exitAutomatic, &out); err != nil {
		t.Fatal(err)
	}
	if _, shutdown := f.counts(); shutdown != 1 {
		t.Fatalf("shutdown requests = %d, want 1", shutdown)
	}
	if !strings.Contains(out.String(), "Orchestrator daemon stopped") {
		t.Fatalf("stop output = %q", out.String())
	}
	if rec, _ := readDashRecord(filepath.Join(f.cfg.dataDir, "dash", "daemon.json")); rec != nil {
		t.Fatalf("the record should be removed after stopping, got %+v", rec)
	}
}

func TestDashDaemonNeverStopsADaemonThatWasAlreadyRunning(t *testing.T) {
	f := newFakeDashDaemon(t)
	f.writeRunFile("", "apprun-headless", dashTestDaemonStart)
	d := f.newDash(t, false)
	var out bytes.Buffer
	if err := d.Start(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if role := d.Role(); role != "Daemon: shared" {
		t.Fatalf("role = %q", role)
	}
	if err := d.Finish(context.Background(), exitStopDaemon, &out); err != nil {
		t.Fatal(err)
	}
	if spawned, shutdown := f.counts(); spawned != 0 || shutdown != 0 {
		t.Fatalf("spawned %d, shutdown %d; a shared daemon must be left alone", spawned, shutdown)
	}
	if out.Len() != 0 {
		t.Fatalf("nothing to report for a shared daemon, got %q", out.String())
	}
}

func TestDashDaemonLeavesTheDesktopAppsReplacementRunning(t *testing.T) {
	f := newFakeDashDaemon(t)
	d := f.newDash(t, false)
	if err := d.Start(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// The desktop app replaces a daemon it did not launch with its own; the
	// headless daemon's owner mode carries over as "persistent".
	f.writeRunFile("persistent", "apprun-gui", dashTestDaemonStart.Add(time.Minute))
	if role := d.Role(); role != "Daemon: shared" {
		t.Fatalf("role after takeover = %q", role)
	}
	var out bytes.Buffer
	if err := d.Finish(context.Background(), exitAutomatic, &out); err != nil {
		t.Fatal(err)
	}
	if _, shutdown := f.counts(); shutdown != 0 {
		t.Fatal("dash must not stop the desktop app's daemon")
	}
}

func TestDashDaemonTwoInstancesTheLastOneStopsIt(t *testing.T) {
	f := newFakeDashDaemon(t)
	first := f.newDash(t, false)
	second := f.newDash(t, false)
	if err := first.Start(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := second.Start(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if spawned, _ := f.counts(); spawned != 1 {
		t.Fatalf("spawned %d daemons, want 1 shared by both", spawned)
	}
	if role := second.Role(); role != "Daemon: started by dash" {
		t.Fatalf("second dash role = %q", role)
	}
	var out bytes.Buffer
	if err := first.Finish(context.Background(), exitAutomatic, &out); err != nil {
		t.Fatal(err)
	}
	if _, shutdown := f.counts(); shutdown != 0 {
		t.Fatal("the first dash to quit must leave the daemon for the other")
	}
	if !strings.Contains(out.String(), "Left the Orchestrator daemon running: 1 other ao dash is still using it. The last ao dash to quit stops it.") {
		t.Fatalf("keep reason missing: %q", out.String())
	}
	if err := second.Finish(context.Background(), exitAutomatic, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, shutdown := f.counts(); shutdown != 1 {
		t.Fatal("the last dash should stop the daemon a dash started")
	}
}

func TestDashDaemonIgnoresAStaleLeaseFromACrashedDash(t *testing.T) {
	f := newFakeDashDaemon(t)
	d := f.newDash(t, false)
	if err := d.Start(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(f.cfg.dataDir, "dash", "leases", "999-crashed")
	if err := os.MkdirAll(stale, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := d.Finish(context.Background(), exitAutomatic, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, shutdown := f.counts(); shutdown != 1 {
		t.Fatal("a lease nobody holds must not keep the daemon alive")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale lease should be reaped, stat err = %v", err)
	}
}

func TestDashDaemonAsksBeforeInterruptingSessions(t *testing.T) {
	f := newFakeDashDaemon(t)
	f.sessions = `{"sessions":[` +
		`{"id":"a","projectId":"p","kind":"worker","mode":"chat","status":"working","chatProviderPreserved":false,"isTerminated":false,"prs":[]},` +
		`{"id":"b","projectId":"p","kind":"worker","mode":"tui","status":"needs_input","isTerminated":false,"prs":[]},` +
		`{"id":"c","projectId":"p","kind":"worker","mode":"chat","status":"idle","isTerminated":false,"prs":[]}]}`
	d := f.newDash(t, false)
	if err := d.Start(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	plan, err := d.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.NeedsConfirm || plan.InFlight != (dashInFlight{Interrupted: 1, Unsupervised: 1}) {
		t.Fatalf("plan = %+v, want a confirmation for 1 interrupted and 1 unsupervised", plan)
	}

	// A signal cannot ask, so it keeps the daemon rather than interrupt work.
	var out bytes.Buffer
	if err := d.Finish(context.Background(), exitAutomatic, &out); err != nil {
		t.Fatal(err)
	}
	if _, shutdown := f.counts(); shutdown != 0 {
		t.Fatal("an unconfirmed exit must not stop a daemon with sessions mid-turn")
	}
	if !strings.Contains(out.String(), "2 sessions are mid-turn") {
		t.Fatalf("keep reason missing: %q", out.String())
	}

	// A confirmed stop goes ahead.
	again := f.newDash(t, false)
	if err := again.Start(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := again.Finish(context.Background(), exitStopDaemon, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, shutdown := f.counts(); shutdown != 1 {
		t.Fatal("a confirmed stop should stop the daemon")
	}
}

func TestDashDaemonKeepFlagLeavesItRunning(t *testing.T) {
	f := newFakeDashDaemon(t)
	d := f.newDash(t, true)
	if err := d.Start(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if role := d.Role(); role != "Daemon: started by dash, kept on quit" {
		t.Fatalf("role = %q", role)
	}
	var out bytes.Buffer
	if err := d.Finish(context.Background(), exitAutomatic, &out); err != nil {
		t.Fatal(err)
	}
	if _, shutdown := f.counts(); shutdown != 0 {
		t.Fatal("--keep-daemon must leave the daemon running")
	}
	if !strings.Contains(out.String(), "--keep-daemon was given. Stop it with `ao stop`.") {
		t.Fatalf("keep reason missing: %q", out.String())
	}
}

func TestDashQuitConfirmationFlow(t *testing.T) {
	newModel := func(plan dashQuitPlan) dashModel {
		m := newTestDashModel(t, &fakeDashBackend{snap: dashFixture()}, 120, 30)
		m.planQuit = func(context.Context) (dashQuitPlan, error) { return plan, nil }
		return m
	}
	busy := dashQuitPlan{NeedsConfirm: true, InFlight: dashInFlight{Interrupted: 1, Unsupervised: 2}}

	m, cmd := dashUpdateCmd(t, newModel(busy), dashKey("q"))
	if cmd == nil || !strings.Contains(plainScreen(m), "Checking sessions…") {
		t.Fatalf("q should check sessions first:\n%s", plainScreen(m))
	}
	m = dashUpdate(t, m, cmd())
	if m.mode != modeQuit {
		t.Fatalf("a plan that needs confirmation should prompt")
	}
	screen := plainScreen(m)
	for _, w := range []string{
		"Stop the Orchestrator daemon?",
		"ao dash started this daemon, and 3 sessions are mid-turn:",
		"1 Chat session would be interrupted: the agent stops with the daemon.",
		"2 keep running, but AO stops tracking them until the daemon starts again.",
		"enter  Stop the daemon",
		"k      Keep it running in the background",
		"enter stop daemon · k keep it running · esc cancel",
	} {
		if !strings.Contains(screen, w) {
			t.Fatalf("confirmation missing %q:\n%s", w, screen)
		}
	}

	stop, cmd := dashUpdateCmd(t, m, dashKey("enter"))
	if stop.exitChoice != exitStopDaemon || cmd == nil {
		t.Fatalf("enter should stop the daemon and quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("enter should quit")
	}
	keep, _ := dashUpdateCmd(t, m, dashKey("k"))
	if keep.exitChoice != exitKeepDaemon {
		t.Fatalf("k should keep the daemon")
	}
	ctrlC, _ := dashUpdateCmd(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if ctrlC.exitChoice != exitKeepDaemon {
		t.Fatalf("ctrl+c in the prompt should leave without stopping anything")
	}
	cancelled, cmd := dashUpdateCmd(t, m, dashKey("esc"))
	if cancelled.mode != modeBrowse || cmd != nil || !strings.Contains(plainScreen(cancelled), "Quit cancelled") {
		t.Fatalf("esc should return to the board")
	}

	// Nothing mid-turn: quit straight away.
	m, cmd = dashUpdateCmd(t, newModel(dashQuitPlan{}), dashKey("q"))
	m, cmd = dashUpdateCmd(t, m, cmd())
	if m.mode == modeQuit || m.exitChoice != exitAutomatic || cmd == nil {
		t.Fatalf("an idle quit should not prompt")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("an idle quit should quit")
	}
}

func TestDashSignalQuitsWithoutAsking(t *testing.T) {
	m := newTestDashModel(t, &fakeDashBackend{snap: dashFixture()}, 120, 30)
	m.planQuit = func(context.Context) (dashQuitPlan, error) {
		t.Fatal("a signal must not wait on a prompt")
		return dashQuitPlan{}, nil
	}
	m, cmd := dashUpdateCmd(t, m, dashSignalMsg{})
	if cmd == nil || m.exitChoice != exitAutomatic {
		t.Fatalf("a signal should quit with an automatic decision")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("a signal should quit")
	}
}

func TestDashHeaderShowsDaemonRole(t *testing.T) {
	snap := dashFixture()
	snap.DaemonRole = "Daemon: started by dash"
	m := newTestDashModel(t, &fakeDashBackend{snap: snap}, 150, 20)
	if header := strings.Split(plainScreen(m), "\n")[0]; !strings.Contains(header, "Daemon: started by dash · ○ Polling") {
		t.Fatalf("header = %q", header)
	}
}

func TestDashStreamDropsWhenTheDaemonStopsListening(t *testing.T) {
	cfg := setConfigEnv(t)
	prev := dashListenerProbeInterval
	dashListenerProbeInterval = 20 * time.Millisecond
	t.Cleanup(func() { dashListenerProbeInterval = prev })

	var mu sync.Mutex
	listening := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			mu.Lock()
			ok := listening
			mu.Unlock()
			if !ok {
				// A closed listener refuses the dial; hanging up is the
				// nearest an httptest server gets.
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/api/v1/events":
			// A shutting-down daemon keeps serving an open stream.
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	backend := httpDashBackend{c: &commandContext{deps: Deps{ProcessAlive: func(int) bool { return true }}.withDefaults()}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan dashStreamEvent, 16)
	done := make(chan error, 1)
	go func() { done <- backend.streamOnce(ctx, "", out) }()
	select {
	case ev := <-out:
		if !ev.Connected {
			t.Fatalf("first event = %+v, want connected", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream never connected")
	}
	mu.Lock()
	listening = false
	mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream stayed open after the daemon stopped accepting connections")
	}
}
