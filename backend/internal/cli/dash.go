package cli

import (
	"context"
	"errors"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// errDashNeedsTerminal is returned when stdin or stdout is not a terminal.
var errDashNeedsTerminal = errors.New("ao dash needs an interactive terminal; for plain output use `ao status` or `ao session ls`")

func newDashCommand(ctx *commandContext) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "dash",
		Short: "Open an interactive terminal dashboard of projects and sessions",
		Long: `Open an interactive terminal dashboard: the terminal counterpart of the
desktop app's project sidebar and session board.

Sessions are grouped by project with their kind, agent, status, branch, and
pull request CI and review state. The dashboard follows the daemon's change
stream and falls back to polling while the stream is down.

Keys: j/k move, tab switches panes, enter opens details, / filters,
m sends a message, x kills a session (asks first), n starts a worker,
? shows help, q quits.

The dashboard needs an interactive terminal. For plain or scripted output use
` + "`ao status`" + ` or ` + "`ao session ls`" + `.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.runDash(cmd.Context(), strings.TrimSpace(project))
		},
	}
	addSessionProjectFlag(cmd.Flags(), &project, "Show only this project")
	return cmd
}

func (c *commandContext) runDash(ctx context.Context, project string) error {
	if !dashIsTerminal(c.deps.In) || !dashIsTerminal(c.deps.Out) {
		return errDashNeedsTerminal
	}
	backend := httpDashBackend{c: c}
	// Read once before taking over the screen, so a missing daemon or an
	// unknown project fails like any other command.
	first, err := backend.Snapshot(ctx, project)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := make(chan dashStreamEvent, 16)
	go backend.Watch(runCtx, project, stream)

	model := newDashModel(runCtx, backend, project, stream, c.deps.Now).seeded(first)
	program := tea.NewProgram(model,
		tea.WithContext(runCtx),
		tea.WithInput(c.deps.In),
		tea.WithOutput(c.deps.Out),
	)
	_, err = program.Run()
	if errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	return err
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
