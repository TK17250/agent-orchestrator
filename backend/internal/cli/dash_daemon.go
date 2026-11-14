package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/datadirlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// `ao dash` daemon lifecycle.
//
// When no daemon is running, `ao dash` starts one (the hidden `ao daemon`
// command of the same binary, detached from the terminal) and records which
// daemon it started in <data dir>/dash/daemon.json. Every running dash holds a
// lease: an OS file lock under <data dir>/dash/leases/, which the kernel drops
// when the process exits however it exits, so a crashed dash never leaves a
// live-looking lease behind.
//
// On exit a dash stops the daemon only when all of these hold:
//   - --keep-daemon was not given and the user did not choose to keep it;
//   - the running daemon is exactly the one an `ao dash` started (same pid,
//     start time and app run id, and no desktop owner tag). The desktop app
//     never attaches to a daemon it did not launch: it gracefully replaces it
//     with its own, so a replaced daemon fails this identity check and dash
//     leaves the app's daemon alone;
//   - no other `ao dash` still holds a lease;
//   - no session is mid-turn, unless the user confirmed stopping anyway.
//
// Ownership belongs to "a dash-started daemon", not to one dash process: when
// two dashes share it, the last one to quit stops it, so the daemon does not
// outlive every dash that used it.

const (
	dashStateDirName   = "dash"
	dashRecordFileName = "daemon.json"
	dashLeaseFileName  = "lease.json"
	dashDaemonLogName  = "daemon.log"
	// dashStartLockWait bounds the wait for another dash that is starting or
	// stopping the daemon at the same moment.
	dashStartLockWait = 2 * time.Minute
	// dashDaemonReadyTimeout bounds the wait for a daemon to become ready. A
	// first boot runs migrations and agent discovery, so it is generous.
	dashDaemonReadyTimeout = 90 * time.Second
	dashDaemonPoll         = 200 * time.Millisecond
)

// dashDaemonEnvDenylist lists the variables a dash-started daemon must not
// inherit. Supervision identity would make the desktop app treat the daemon as
// its own (it attaches to a daemon only when the app run id matches), and
// session-scoped variables appear when dash runs in an AO session terminal.
var dashDaemonEnvDenylist = map[string]bool{
	"AO_OWNER":                       true,
	"AO_APP_RUN_ID":                  true,
	"AO_KEEP_DAEMON":                 true,
	"AO_BROWSER_RUNTIME_TOKEN":       true,
	"AO_BROWSER_RUNTIME_TOKEN_STDIN": true,
	"AO_BROWSER_RUNTIME_ADDRESS":     true,
	"AO_BROWSER_STREAM_ADDRESS":      true,
	"AO_BROWSER_CAPABILITY":          true,
	"AO_BOUNDARY_ELECTRON":           true,
	"AO_MUX_ELECTRON":                true,
	"AO_MOBILE_CONTROL_ELECTRON":     true,
	"AO_SESSION_ID":                  true,
	"AO_PROJECT_ID":                  true,
	"AO_ISSUE_ID":                    true,
	"AO_SIDE_CONVERSATION_ID":        true,
	"AO_RUNTIME_LAUNCH_ID":           true,
	"AO_WORKTREE_PATH":               true,
	"AO_SOURCE_TREE_PATH":            true,
	"AO_LAUNCH_SPEC":                 true,
	"AO_PERMISSION_MODE":             true,
	"AO_READY":                       true,
	"AO_SUPERVISED_PROCESS":          true,
	"AO_AGENT_BROWSER_URL":           true,
	"AO_AGENT_BROWSER_TOKEN":         true,
	"AO_PATH_PIN":                    true,
}

// dashDaemonEnv returns env without the denylisted variables.
func dashDaemonEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if dashDaemonEnvDenylist[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// dashDaemonRecord identifies the daemon an `ao dash` started.
type dashDaemonRecord struct {
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"startedAt"`
	AppRunID     string    `json:"appRunId,omitempty"`
	StartedByPID int       `json:"startedByPid"`
	Log          string    `json:"log"`
}

// matches reports whether info describes the daemon this record names. A
// desktop-owned daemon (owner "app" or "persistent") never matches.
func (r *dashDaemonRecord) matches(info *runfile.Info) bool {
	return r != nil && info != nil && info.Owner == "" &&
		r.PID == info.PID && r.StartedAt.Equal(info.StartedAt) && r.AppRunID == info.AppRunID
}

// dashExitChoice is how the dashboard was left.
type dashExitChoice int

const (
	// exitAutomatic decides without asking: a quit that needed no
	// confirmation, or a signal (SIGTERM, SIGHUP, SIGINT) with no one to ask.
	exitAutomatic dashExitChoice = iota
	// exitStopDaemon is a quit where the user confirmed stopping the daemon
	// even though sessions are mid-turn.
	exitStopDaemon
	// exitKeepDaemon is a quit where the user chose to leave the daemon running.
	exitKeepDaemon
)

// dashInFlight counts sessions that are mid-turn, split by what a daemon stop
// does to them. It mirrors the desktop's restart-to-update rule.
type dashInFlight struct {
	// Interrupted are Chat sessions without a persistent provider host: their
	// agent process stops with the daemon, losing the turn.
	Interrupted int
	// Unsupervised keep running (terminal sessions run in detached pty-hosts,
	// persistent Chat hosts keep their connection), but nothing records their
	// activity, reports or PR changes until a daemon starts again.
	Unsupervised int
}

func (f dashInFlight) Total() int { return f.Interrupted + f.Unsupervised }

// dashSessionModeChat is the Chat session mode on the session read model.
const dashSessionModeChat = "chat"

// dashTurnMayBeInFlight matches the desktop's TURN_MAY_BE_IN_FLIGHT.
var dashTurnMayBeInFlight = map[string]bool{"working": true, "no_signal": true, "needs_input": true}

func countDashInFlight(sessions []dashSessionDTO) dashInFlight {
	var f dashInFlight
	for _, s := range sessions {
		if s.IsTerminated || !dashTurnMayBeInFlight[s.Status] {
			continue
		}
		if s.Mode == dashSessionModeChat && !s.ChatProviderPreserved {
			f.Interrupted++
		} else {
			f.Unsupervised++
		}
	}
	return f
}

// dashStopFacts is everything the stop decision reads.
type dashStopFacts struct {
	KeepFlag     bool
	Choice       dashExitChoice
	Record       *dashDaemonRecord
	Current      *runfile.Info
	CurrentAlive bool
	OtherLeases  int
	InFlight     dashInFlight
}

// dashStopDecision is the outcome: stop, or keep with a reason. NeedsConfirm
// marks the one case a person could still overrule: everything allows a stop
// except sessions that are mid-turn.
type dashStopDecision struct {
	Stop         bool
	NeedsConfirm bool
	Reason       string
}

// decideDashDaemonStop is the pure stop rule; see the package comment above.
func decideDashDaemonStop(f dashStopFacts) dashStopDecision {
	switch {
	case f.Current == nil || !f.CurrentAlive:
		return dashStopDecision{Reason: "the daemon is not running"}
	case !f.Record.matches(f.Current):
		return dashStopDecision{Reason: "it was not started by ao dash"}
	case f.KeepFlag:
		return dashStopDecision{Reason: "--keep-daemon was given"}
	case f.Choice == exitKeepDaemon:
		return dashStopDecision{Reason: "you chose to keep it running"}
	case f.OtherLeases > 0:
		return dashStopDecision{Reason: fmt.Sprintf("%d other ao dash %s still using it", f.OtherLeases, isAre(f.OtherLeases))}
	case f.InFlight.Total() > 0 && f.Choice != exitStopDaemon:
		n := f.InFlight.Total()
		return dashStopDecision{NeedsConfirm: true, Reason: fmt.Sprintf("%d session%s %s mid-turn", n, pluralS(n), isAre(n))}
	}
	return dashStopDecision{Stop: true}
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// dashQuitPlan is what quitting now would do, for the confirmation prompt.
type dashQuitPlan struct {
	NeedsConfirm bool
	InFlight     dashInFlight
}

// dashDaemon owns one dash process's share of the daemon lifecycle.
type dashDaemon struct {
	c        *commandContext
	keepFlag bool
	runFile  string
	dataDir  string
	root     string
	lease    *dashLease
	// startedHere reports that this dash process spawned the daemon.
	startedHere bool
}

func newDashDaemon(c *commandContext, keep bool) (*dashDaemon, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return &dashDaemon{
		c:        c,
		keepFlag: keep,
		runFile:  cfg.RunFilePath,
		dataDir:  cfg.DataDir,
		root:     filepath.Join(cfg.DataDir, dashStateDirName),
	}, nil
}

func (d *dashDaemon) logPath() string    { return filepath.Join(d.root, dashDaemonLogName) }
func (d *dashDaemon) recordPath() string { return filepath.Join(d.root, dashRecordFileName) }
func (d *dashDaemon) leasesDir() string  { return filepath.Join(d.root, "leases") }

// lockStart serializes starting and stopping across dash processes, so one
// dash never stops the daemon while another is registering its lease. On a
// filesystem without file locks it returns a no-op release.
func (d *dashDaemon) lockStart(ctx context.Context) (func(), error) {
	lock, err := datadirlock.Acquire(ctx, filepath.Join(d.root, "start"), dashStartLockWait)
	switch {
	case errors.Is(err, datadirlock.ErrUnsupported):
		return func() {}, nil
	case err != nil:
		return nil, fmt.Errorf("wait for another ao dash: %w", err)
	}
	return func() { _ = lock.Release() }, nil
}

// Start takes this process's lease and makes sure a ready daemon is running,
// starting one when none is. Progress lines go to status.
func (d *dashDaemon) Start(ctx context.Context, status io.Writer) error {
	unlock, err := d.lockStart(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	lease, err := acquireDashLease(ctx, d.leasesDir(), os.Getpid(), d.c.deps.Now())
	if err != nil {
		return err
	}
	d.lease = lease

	st, err := d.c.inspectDaemon(ctx)
	if err != nil {
		return err
	}
	switch st.State {
	case stateReady:
		return nil
	case stateUnhealthy, stateNotReady:
		// Alive but not ready: most likely the desktop app or another client
		// is still booting it. Wait for it rather than starting a second one.
		_, _ = fmt.Fprintln(status, "Waiting for the Orchestrator daemon to become ready…")
		_, err := d.waitReady(ctx)
		return err
	}
	return d.spawn(ctx, status)
}

func (d *dashDaemon) spawn(ctx context.Context, status io.Writer) error {
	exe, err := d.c.deps.Executable()
	if err != nil {
		return fmt.Errorf("start daemon: resolve executable: %w", err)
	}
	if err := os.MkdirAll(d.root, 0o750); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	logFile, err := os.OpenFile(d.logPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("start daemon: open log: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	_, _ = fmt.Fprintf(status, "Starting the Orchestrator daemon… (log: %s)\n", d.logPath())
	if err := d.c.deps.StartProcess(processStartConfig{
		Path:   exe,
		Args:   []string{"daemon"},
		Env:    dashDaemonEnv(os.Environ()),
		Stdout: logFile,
		Stderr: logFile,
	}); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	info, err := d.waitReady(ctx)
	if err != nil {
		return fmt.Errorf("%w; see %s", err, d.logPath())
	}
	d.startedHere = true
	if info.Owner != "" {
		// Someone else's daemon won the race (the desktop app launching at the
		// same moment); it is not ours to stop.
		return nil
	}
	return writeDashRecord(d.recordPath(), dashDaemonRecord{
		PID: info.PID, StartedAt: info.StartedAt, AppRunID: info.AppRunID,
		StartedByPID: os.Getpid(), Log: d.logPath(),
	})
}

// waitReady polls until the daemon reports ready and returns its run file.
func (d *dashDaemon) waitReady(ctx context.Context) (*runfile.Info, error) {
	deadline := d.c.deps.Now().Add(dashDaemonReadyTimeout)
	for {
		st, err := d.c.inspectDaemon(ctx)
		if err != nil {
			return nil, err
		}
		if st.State == stateReady {
			info, err := runfile.Read(d.runFile)
			if err != nil {
				return nil, err
			}
			if info != nil {
				return info, nil
			}
		}
		if !d.c.deps.Now().Before(deadline) {
			detail := string(st.State)
			if st.Error != "" {
				detail += ": " + st.Error
			}
			return nil, fmt.Errorf("the Orchestrator daemon did not become ready within %s (%s)", dashDaemonReadyTimeout, detail)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		d.c.deps.Sleep(dashDaemonPoll)
	}
}

// Role describes the running daemon for the header.
func (d *dashDaemon) Role() string {
	info, err := runfile.Read(d.runFile)
	if err != nil || info == nil || !d.c.deps.ProcessAlive(info.PID) {
		return ""
	}
	rec, _ := readDashRecord(d.recordPath())
	if rec.matches(info) {
		if d.keepFlag {
			return "Daemon: started by dash, kept on quit"
		}
		return "Daemon: started by dash"
	}
	return "Daemon: shared"
}

// facts gathers the stop decision inputs. Sessions are listed across every
// project, whatever --project scopes the view to.
func (d *dashDaemon) facts(ctx context.Context, choice dashExitChoice) (dashStopFacts, error) {
	f := dashStopFacts{KeepFlag: d.keepFlag, Choice: choice}
	info, err := runfile.Read(d.runFile)
	if err != nil {
		return f, err
	}
	f.Current = info
	f.CurrentAlive = info != nil && d.c.deps.ProcessAlive(info.PID)
	rec, err := readDashRecord(d.recordPath())
	if err != nil {
		return f, err
	}
	f.Record = rec
	if !f.Record.matches(f.Current) || !f.CurrentAlive {
		return f, nil
	}
	self := ""
	if d.lease != nil {
		self = d.lease.dir
	}
	f.OtherLeases, err = countOtherDashLeases(ctx, d.leasesDir(), self, d.c.deps.ProcessAlive)
	if err != nil {
		return f, err
	}
	params := url.Values{}
	params.Set("active", "true")
	var list dashSessionListResponse
	if err := d.c.getJSON(ctx, apiPath("sessions", params), &list); err != nil {
		return f, err
	}
	f.InFlight = countDashInFlight(list.Sessions)
	return f, nil
}

// Plan reports whether quitting now needs the user's confirmation.
func (d *dashDaemon) Plan(ctx context.Context) (dashQuitPlan, error) {
	f, err := d.facts(ctx, exitAutomatic)
	if err != nil {
		return dashQuitPlan{}, err
	}
	dec := decideDashDaemonStop(f)
	return dashQuitPlan{NeedsConfirm: dec.NeedsConfirm, InFlight: f.InFlight}, nil
}

// Finish decides again, under the start lock, and stops the daemon when the
// decision allows it. Facts are re-read here because another dash may have
// started, or the desktop app may have taken over, since the prompt. It
// releases this process's lease either way.
func (d *dashDaemon) Finish(ctx context.Context, choice dashExitChoice, out io.Writer) error {
	defer d.Close()
	unlock, err := d.lockStart(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := d.facts(ctx, choice)
	if err != nil {
		return fmt.Errorf("decide whether to stop the daemon: %w", err)
	}
	dec := decideDashDaemonStop(f)
	ours := f.Record.matches(f.Current) && f.CurrentAlive
	if !dec.Stop {
		switch {
		case !ours:
		case f.OtherLeases > 0:
			_, _ = fmt.Fprintf(out, "Left the Orchestrator daemon running: %s. The last ao dash to quit stops it.\n", dec.Reason)
		default:
			_, _ = fmt.Fprintf(out, "Left the Orchestrator daemon running: %s. Stop it with `ao stop`.\n", dec.Reason)
		}
		return nil
	}
	_, _ = fmt.Fprintln(out, "Stopping the Orchestrator daemon…")
	if err := d.c.requestShutdown(ctx, f.Current.Port); err != nil {
		return fmt.Errorf("stop daemon: %w", err)
	}
	if _, err := d.c.waitForStopped(ctx, f.Current.PID, d.runFile, d.dataDir, defaultStopTimeout); err != nil {
		return fmt.Errorf("stop daemon: %w", err)
	}
	_ = os.Remove(d.recordPath())
	_, _ = fmt.Fprintln(out, "Orchestrator daemon stopped")
	return nil
}

// Close releases this process's lease. It is safe to call more than once.
func (d *dashDaemon) Close() {
	if d.lease != nil {
		d.lease.Release()
		d.lease = nil
	}
}

func writeDashRecord(path string, rec dashDaemonRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readDashRecord returns nil when no dash-started daemon is recorded.
func readDashRecord(path string) (*dashDaemonRecord, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec dashDaemonRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		// A torn or foreign file names no daemon; never act on it.
		return nil, nil //nolint:nilerr // an unreadable record is treated as "no record"
	}
	return &rec, nil
}

// dashLease is one dash process's claim on the daemon: a directory holding an
// OS-locked file. The kernel drops the lock when the process exits, so a
// lease whose lock can be taken belongs to a dash that is gone.
type dashLease struct {
	dir  string
	lock *datadirlock.Lock
}

type dashLeaseInfo struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
}

func acquireDashLease(ctx context.Context, leasesDir string, pid int, now time.Time) (*dashLease, error) {
	dir := filepath.Join(leasesDir, strconv.Itoa(pid)+"-"+strconv.FormatInt(now.UnixNano(), 36))
	// A short wait covers another dash probing this lease at the instant it is
	// created.
	lock, err := datadirlock.Acquire(ctx, dir, 2*time.Second)
	if err != nil && !errors.Is(err, datadirlock.ErrUnsupported) {
		return nil, fmt.Errorf("register ao dash: %w", err)
	}
	data, _ := json.Marshal(dashLeaseInfo{PID: pid, StartedAt: now})
	if werr := os.WriteFile(filepath.Join(dir, dashLeaseFileName), data, 0o600); werr != nil && lock == nil {
		return nil, fmt.Errorf("register ao dash: %w", werr)
	}
	return &dashLease{dir: dir, lock: lock}, nil
}

// Release drops the lock and removes the lease directory.
func (l *dashLease) Release() {
	if l == nil {
		return
	}
	if l.lock != nil {
		_ = l.lock.Release()
	}
	_ = os.RemoveAll(l.dir)
}

// countOtherDashLeases counts live leases other than self, removing stale
// ones. A lease is live while its lock is held; where the filesystem cannot
// lock files, it is live while its recorded pid is alive.
func countOtherDashLeases(ctx context.Context, leasesDir, self string, alive func(int) bool) (int, error) {
	entries, err := os.ReadDir(leasesDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	live := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(leasesDir, e.Name())
		if dir == self {
			continue
		}
		lock, err := datadirlock.Acquire(ctx, dir, 0)
		switch {
		case errors.Is(err, datadirlock.ErrLocked):
			live++
		case errors.Is(err, datadirlock.ErrUnsupported):
			if leasePIDAlive(dir, alive) {
				live++
			} else {
				_ = os.RemoveAll(dir)
			}
		case err != nil:
			return 0, err
		default:
			// Nobody holds it: its dash exited without cleaning up.
			_ = lock.Release()
			_ = os.RemoveAll(dir)
		}
	}
	return live, nil
}

func leasePIDAlive(dir string, alive func(int) bool) bool {
	data, err := os.ReadFile(filepath.Join(dir, dashLeaseFileName))
	if err != nil {
		return false
	}
	var info dashLeaseInfo
	if json.Unmarshal(data, &info) != nil || info.PID <= 0 {
		return false
	}
	return alive(info.PID)
}
