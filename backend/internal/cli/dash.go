package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// errDashNeedsTerminal is returned when stdin or stdout is not a terminal.
var errDashNeedsTerminal = errors.New("ao dash needs an interactive terminal; for plain output use `ao status` or `ao session ls`")

type dashOptions struct {
	project    string
	keepDaemon bool
}

func newDashCommand(ctx *commandContext) *cobra.Command {
	var opts dashOptions
	cmd := &cobra.Command{
		Use:   "dash",
		Short: "Open an interactive terminal dashboard of projects and sessions",
		Long: `Open an interactive terminal dashboard: the terminal counterpart of the
desktop app's project sidebar and session board.

Sessions are grouped by project with their kind, agent, status, branch, and
pull request CI and review state. The dashboard follows the daemon's change
stream and falls back to polling while the stream is down.

When no Orchestrator daemon is running, ao dash starts one in the background
(its log is in <data dir>/dash/daemon.log) and stops it again on quit, unless
another ao dash is still open, the desktop app has taken it over, or you pass
--keep-daemon. A daemon that was already running is never stopped. Quitting
while sessions are mid-turn asks first.

Keys: j/k move, tab switches panes, enter opens details, / filters,
m sends a message, x kills a session (asks first), n starts a worker,
? shows help, q quits.

The dashboard needs an interactive terminal. For plain or scripted output use
` + "`ao status`" + ` or ` + "`ao session ls`" + `.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.project = strings.TrimSpace(opts.project)
			return ctx.runDash(cmd.Context(), opts)
		},
	}
	addSessionProjectFlag(cmd.Flags(), &opts.project, "Show only this project")
	cmd.Flags().BoolVar(&opts.keepDaemon, "keep-daemon", false, "Leave a daemon that ao dash started running after quitting")
	return cmd
}

// dashSignalGrace bounds how long a signalled dashboard may take to quit
// before it is killed; the terminal may already be gone.
const dashSignalGrace = 3 * time.Second

func (c *commandContext) runDash(ctx context.Context, opts dashOptions) error {
	if !dashIsTerminal(c.deps.In) || !dashIsTerminal(c.deps.Out) {
		return errDashNeedsTerminal
	}
	daemon, err := newDashDaemon(c, opts.keepDaemon)
	if err != nil {
		return err
	}
	// Starting (when needed) happens before the screen is taken over, so its
	// progress and any failure print like any other command's output.
	if err := daemon.Start(ctx, c.deps.Out); err != nil {
		daemon.Close()
		return err
	}
	backend := httpDashBackend{c: c, daemon: daemon}
	first, err := backend.Snapshot(ctx, opts.project)
	if err != nil {
		// An unknown project must not leave behind a daemon this run started.
		_ = daemon.Finish(ctx, exitAutomatic, c.deps.Out)
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	stream := make(chan dashStreamEvent, 16)
	go backend.Watch(runCtx, opts.project, stream)

	model := newDashModel(runCtx, backend, opts.project, stream, c.deps.Now).seeded(first)
	model.planQuit = daemon.Plan
	program := tea.NewProgram(model,
		tea.WithContext(runCtx),
		tea.WithInput(c.deps.In),
		tea.WithOutput(c.deps.Out),
		// dash handles signals itself so SIGTERM and SIGHUP reach the same
		// exit path as q; Bubble Tea would quit on SIGTERM and ignore SIGHUP.
		tea.WithoutSignalHandler(),
	)
	stopSignals := dashForwardSignals(program)
	final, runErr := program.Run()
	stopSignals()
	// Close the change stream before any stop: the daemon's graceful shutdown
	// waits for open streams.
	cancel()

	choice := exitAutomatic
	if fm, ok := final.(dashModel); ok {
		choice = fm.exitChoice
	}
	finishErr := daemon.Finish(ctx, choice, c.deps.Out)
	if runErr != nil && !errors.Is(runErr, tea.ErrInterrupted) && !errors.Is(runErr, tea.ErrProgramKilled) {
		return runErr
	}
	return finishErr
}

// dashForwardSignals turns SIGINT, SIGTERM and SIGHUP into a quit, killing
// the program if it cannot quit in time (a closed terminal). The returned
// function stops forwarding.
func dashForwardSignals(program *tea.Program) func() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigs:
		case <-done:
			return
		}
		program.Send(dashSignalMsg{})
		select {
		case <-time.After(dashSignalGrace):
			program.Kill()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
	}
}

// seeded applies the startup snapshot so the first frame already has data.
func (m dashModel) seeded(snap dashSnapshot) dashModel {
	seeded, _ := m.applySnapshot(dashSnapshotMsg{snap: snap})
	seeded.lastFetch = seeded.now()
	return seeded
}

// dashIsTerminal reports whether a CLI stream is a terminal; test buffers and
// pipes are not.
func dashIsTerminal(stream any) bool {
	file, ok := stream.(*os.File)
	return ok && term.IsTerminal(file.Fd())
}
