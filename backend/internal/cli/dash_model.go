package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// dashPane is the pane that owns j/k navigation.
type dashPane int

const (
	paneProjects dashPane = iota
	paneSessions
)

// dashMode is the interaction layer on top of browsing.
type dashMode int

const (
	modeBrowse dashMode = iota
	modeFilter
	modeSend
	modeKill
	modeSpawn
	modeHelp
	modeQuit
)

const (
	// dashPollLive is the safety-net refresh while the change stream is up.
	dashPollLive = 30 * time.Second
	// dashPollOffline is the refresh cadence while the stream is down or the
	// daemon is unreachable, so the board reconnects on its own.
	dashPollOffline = 3 * time.Second
	// dashDebounce coalesces a burst of change events into one refresh.
	dashDebounce     = 250 * time.Millisecond
	dashFetchTimeout = 15 * time.Second
)

type (
	dashSnapshotMsg struct {
		snap dashSnapshot
		err  error
	}
	dashDetailMsg struct {
		detail dashDetail
		err    error
	}
	dashStreamMsg  dashStreamEvent
	dashTickMsg    time.Time
	dashRefreshMsg struct{}
	dashActionMsg  struct {
		text string
		err  error
		// selectID moves the selection to a newly created session.
		selectID string
	}
	dashQuitPlanMsg struct {
		plan dashQuitPlan
		err  error
	}
	// dashSignalMsg asks the dashboard to quit because of a signal (SIGTERM,
	// SIGHUP when the terminal closes): there is no one left to ask.
	dashSignalMsg struct{}
)

type dashModel struct {
	ctx     context.Context
	backend dashBackend
	scope   string
	stream  <-chan dashStreamEvent
	now     func() time.Time

	width, height int

	snap      dashSnapshot
	loaded    bool
	lastFetch time.Time
	// fetchErr is the last refresh failure that is not a missing daemon.
	fetchErr   error
	daemonDown bool
	live       bool

	refreshing       bool
	refreshQueued    bool
	refreshScheduled bool

	focus         dashPane
	projectCursor int // 0 is "All projects"; i is projects()[i-1]
	selectedID    string

	detailOpen    bool
	detail        dashDetail
	detailLoading bool

	mode        dashMode
	filter      textinput.Model
	compose     textarea.Model
	spawnName   textinput.Model
	spawnPrompt textarea.Model
	spawnField  int
	spawnTarget string
	killTarget  string

	busy     string
	flash    string
	flashErr bool

	// planQuit, when set, is asked whether quitting needs a confirmation
	// because this exit would stop a daemon with sessions mid-turn.
	planQuit func(context.Context) (dashQuitPlan, error)
	quitPlan dashQuitPlan
	// exitChoice is read by the command after the program ends.
	exitChoice dashExitChoice
}

func newDashModel(ctx context.Context, backend dashBackend, scope string, stream <-chan dashStreamEvent, now func() time.Time) dashModel {
	filter := textinput.New()
	filter.Prompt = "/"
	filter.Placeholder = "filter sessions"

	compose := newDashTextarea("Message")

	name := textinput.New()
	name.Prompt = ""
	name.Placeholder = "Task name"
	name.CharLimit = maxDisplayNameLen

	m := dashModel{
		ctx:         ctx,
		backend:     backend,
		scope:       scope,
		stream:      stream,
		now:         now,
		width:       100,
		height:      30,
		focus:       paneSessions,
		filter:      filter,
		compose:     compose,
		spawnName:   name,
		spawnPrompt: newDashTextarea("What should the worker do?"),
	}
	m.layoutInputs()
	return m
}

func newDashTextarea(placeholder string) textarea.Model {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.Prompt = "  "
	ta.Placeholder = placeholder
	ta.SetHeight(4)
	// Enter submits; a newline needs ctrl+j (or shift+enter where the terminal
	// reports it), matching chat composers.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "shift+enter"))
	return ta
}

func (m dashModel) Init() tea.Cmd {
	cmds := []tea.Cmd{dashTick(), m.waitStream()}
	if !m.loaded {
		cmds = append(cmds, m.fetchSnapshot())
	}
	return tea.Batch(cmds...)
}

func dashTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return dashTickMsg(t) })
}

func (m dashModel) waitStream() tea.Cmd {
	if m.stream == nil {
		return nil
	}
	ch := m.stream
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return dashStreamMsg(ev)
	}
}

func (m dashModel) fetchSnapshot() tea.Cmd {
	backend, ctx, scope := m.backend, m.ctx, m.scope
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, dashFetchTimeout)
		defer cancel()
		snap, err := backend.Snapshot(ctx, scope)
		return dashSnapshotMsg{snap: snap, err: err}
	}
}

func (m dashModel) fetchDetail(sess dashSession) tea.Cmd {
	backend, ctx := m.backend, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, dashFetchTimeout)
		defer cancel()
		detail, err := backend.Detail(ctx, sess)
		return dashDetailMsg{detail: detail, err: err}
	}
}

// startRefresh begins a snapshot fetch, or queues one behind a fetch already
// in flight so a change during the fetch is never lost.
func (m *dashModel) startRefresh() tea.Cmd {
	if m.refreshing {
		m.refreshQueued = true
		return nil
	}
	m.refreshing = true
	m.lastFetch = m.now()
	return m.fetchSnapshot()
}

func (m dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layoutInputs()
		return m, nil
	case dashSnapshotMsg:
		return m.applySnapshot(msg)
	case dashDetailMsg:
		if msg.err != nil {
			m.detailLoading = false
			m.setFlash(msg.err.Error(), true)
			return m, nil
		}
		if msg.detail.SessionID == m.selectedID {
			m.detail = msg.detail
			m.detailLoading = false
		}
		return m, nil
	case dashStreamMsg:
		m.live = msg.Connected
		var cmd tea.Cmd
		if msg.Changed && !m.refreshScheduled {
			m.refreshScheduled = true
			cmd = tea.Tick(dashDebounce, func(time.Time) tea.Msg { return dashRefreshMsg{} })
		}
		return m, tea.Batch(cmd, m.waitStream())
	case dashRefreshMsg:
		m.refreshScheduled = false
		cmd := m.startRefresh()
		return m, cmd
	case dashTickMsg:
		interval := dashPollLive
		if !m.live || m.daemonDown {
			interval = dashPollOffline
		}
		var cmd tea.Cmd
		if m.now().Sub(m.lastFetch) >= interval {
			cmd = m.startRefresh()
		}
		return m, tea.Batch(cmd, dashTick())
	case dashActionMsg:
		m.busy = ""
		if msg.err != nil {
			m.setFlash(msg.err.Error(), true)
		} else {
			m.setFlash(msg.text, false)
		}
		// A failed start still leaves a session behind; select it too.
		if msg.selectID != "" {
			m.selectedID = msg.selectID
		}
		cmd := m.startRefresh()
		return m, cmd
	case dashQuitPlanMsg:
		m.busy = ""
		if msg.err == nil && msg.plan.NeedsConfirm {
			m.mode = modeQuit
			m.quitPlan = msg.plan
			return m, nil
		}
		// Without a plan the command decides on exit and keeps a daemon that
		// has sessions mid-turn, so quitting here is always safe.
		m.exitChoice = exitAutomatic
		return m, tea.Quit
	case dashSignalMsg:
		m.exitChoice = exitAutomatic
		return m, tea.Quit
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m.updateInputs(msg)
}

func (m dashModel) applySnapshot(msg dashSnapshotMsg) (dashModel, tea.Cmd) {
	m.refreshing = false
	var cmds []tea.Cmd
	if m.refreshQueued {
		m.refreshQueued = false
		cmds = append(cmds, m.startRefresh())
	}
	if msg.err != nil {
		if errors.Is(msg.err, errDaemonUnavailable) {
			m.daemonDown = true
			m.live = false
		} else {
			m.fetchErr = msg.err
		}
		return m, tea.Batch(cmds...)
	}
	m.daemonDown = false
	m.fetchErr = nil
	m.loaded = true
	m.snap = msg.snap
	m.projectCursor = min(m.projectCursor, len(m.projects()))
	before := m.selectedID
	m.ensureSelection()
	if m.detailOpen {
		if sess, ok := m.selected(); ok {
			if sess.ID != before {
				m.detail = dashDetail{}
				m.detailLoading = true
			}
			cmds = append(cmds, m.fetchDetail(sess))
		} else {
			m.detailOpen = false
		}
	}
	return m, tea.Batch(cmds...)
}

func (m *dashModel) setFlash(text string, isErr bool) {
	m.flash = dashClean(text)
	m.flashErr = isErr
}

func (m dashModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	keyName := msg.String()
	if m.mode == modeQuit {
		return m.handleQuitKey(keyName)
	}
	if keyName == "ctrl+c" {
		return m.requestQuit()
	}
	switch m.mode {
	case modeHelp:
		m.mode = modeBrowse
		return m, nil
	case modeFilter:
		return m.handleFilterKey(msg)
	case modeSend:
		return m.handleSendKey(msg)
	case modeKill:
		return m.handleKillKey(keyName)
	case modeSpawn:
		return m.handleSpawnKey(msg)
	}
	return m.handleBrowseKey(keyName)
}

func (m dashModel) handleBrowseKey(keyName string) (tea.Model, tea.Cmd) {
	m.flash = ""
	switch keyName {
	case "q":
		return m.requestQuit()
	case "?":
		m.mode = modeHelp
	case "tab", "shift+tab":
		if m.focus == paneProjects {
			m.focus = paneSessions
		} else {
			m.focus = paneProjects
		}
	case "h", "left":
		m.focus = paneProjects
	case "l", "right":
		m.focus = paneSessions
	case "j", "down":
		return m.move(1)
	case "k", "up":
		return m.move(-1)
	case "g", "home":
		return m.move(-1 << 20)
	case "G", "end":
		return m.move(1 << 20)
	case "enter":
		if m.focus == paneProjects {
			m.focus = paneSessions
			return m, nil
		}
		return m.openDetail()
	case "esc":
		switch {
		case m.detailOpen:
			m.detailOpen = false
		case m.filter.Value() != "":
			m.filter.SetValue("")
			m.ensureSelection()
		}
	case "/":
		m.mode = modeFilter
		m.focus = paneSessions
		focusCmd := m.filter.Focus()
		return m, focusCmd
	case "r":
		cmd := m.startRefresh()
		return m, cmd
	case "m":
		return m.beginSend()
	case "x":
		return m.beginKill()
	case "n":
		return m.beginSpawn()
	}
	return m, nil
}

func (m dashModel) move(delta int) (tea.Model, tea.Cmd) {
	if m.focus == paneProjects {
		next := clampInt(m.projectCursor+delta, 0, len(m.projects()))
		if next != m.projectCursor {
			m.projectCursor = next
			m.detailOpen = false
			m.selectedID = ""
			m.ensureSelection()
		}
		return m, nil
	}
	visible := m.visibleSessions()
	if len(visible) == 0 {
		return m, nil
	}
	idx := clampInt(m.selectedIndex(visible)+delta, 0, len(visible)-1)
	if visible[idx].ID == m.selectedID {
		return m, nil
	}
	m.selectedID = visible[idx].ID
	if m.detailOpen {
		m.detail = dashDetail{}
		m.detailLoading = true
		return m, m.fetchDetail(visible[idx])
	}
	return m, nil
}

func (m dashModel) openDetail() (tea.Model, tea.Cmd) {
	sess, ok := m.selected()
	if !ok {
		return m, nil
	}
	m.detailOpen = true
	if m.detail.SessionID != sess.ID {
		m.detail = dashDetail{}
	}
	m.detailLoading = true
	return m, m.fetchDetail(sess)
}

// requestQuit asks the planner whether this exit would stop a daemon with
// sessions mid-turn, and quits straight away when there is nothing to ask.
func (m dashModel) requestQuit() (tea.Model, tea.Cmd) {
	if m.planQuit == nil {
		m.exitChoice = exitAutomatic
		return m, tea.Quit
	}
	if m.busy == "Checking sessions…" {
		return m, nil
	}
	m.mode = modeBrowse
	m.busy = "Checking sessions…"
	plan, ctx := m.planQuit, m.ctx
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, dashFetchTimeout)
		defer cancel()
		p, err := plan(ctx)
		return dashQuitPlanMsg{plan: p, err: err}
	}
}

// handleQuitKey answers the stop-the-daemon confirmation. Stopping is the
// default; ctrl+c leaves without stopping anything.
func (m dashModel) handleQuitKey(keyName string) (tea.Model, tea.Cmd) {
	switch keyName {
	case "enter", "s", "y":
		m.exitChoice = exitStopDaemon
		return m, tea.Quit
	case "k", "ctrl+c":
		m.exitChoice = exitKeepDaemon
		return m, tea.Quit
	case "esc", "c", "n", "q":
		m.mode = modeBrowse
		m.setFlash("Quit cancelled", false)
	}
	return m, nil
}

func (m dashModel) handleFilterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.mode = modeBrowse
		m.filter.Blur()
		return m, nil
	case "esc":
		m.mode = modeBrowse
		m.filter.Blur()
		m.filter.SetValue("")
		m.ensureSelection()
		return m, nil
	}
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	m.detailOpen = false
	m.ensureSelection()
	return m, cmd
}

func (m dashModel) actionBlocked() (dashModel, bool) {
	if m.busy != "" {
		m.setFlash("Wait for "+strings.ToLower(strings.TrimSuffix(m.busy, "…"))+" to finish", true)
		return m, true
	}
	if m.daemonDown {
		m.setFlash("The daemon is not reachable", true)
		return m, true
	}
	return m, false
}

func (m dashModel) beginSend() (tea.Model, tea.Cmd) {
	m, blocked := m.actionBlocked()
	if blocked {
		return m, nil
	}
	if _, ok := m.selected(); !ok {
		m.setFlash("Select a session first", true)
		return m, nil
	}
	m.mode = modeSend
	m.compose.Reset()
	focusCmd := m.compose.Focus()
	return m, focusCmd
}

func (m dashModel) handleSendKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeBrowse
		m.compose.Blur()
		return m, nil
	case "enter":
		text := strings.TrimSpace(m.compose.Value())
		sess, ok := m.selected()
		if text == "" || !ok {
			return m, nil
		}
		m.mode = modeBrowse
		m.compose.Blur()
		m.busy = "Sending…"
		backend, ctx, id, label := m.backend, m.ctx, sess.ID, dashSessionTitle(sess)
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(ctx, commandTimeout)
			defer cancel()
			if err := backend.Send(ctx, id, text); err != nil {
				return dashActionMsg{err: err}
			}
			return dashActionMsg{text: "Message sent to " + label}
		}
	}
	var cmd tea.Cmd
	m.compose, cmd = m.compose.Update(msg)
	return m, cmd
}

func (m dashModel) beginKill() (tea.Model, tea.Cmd) {
	m, blocked := m.actionBlocked()
	if blocked {
		return m, nil
	}
	sess, ok := m.selected()
	if !ok {
		m.setFlash("Select a session first", true)
		return m, nil
	}
	m.mode = modeKill
	m.killTarget = sess.ID
	return m, nil
}

func (m dashModel) handleKillKey(keyName string) (tea.Model, tea.Cmd) {
	m.mode = modeBrowse
	target := m.killTarget
	m.killTarget = ""
	if keyName != "y" && keyName != "Y" {
		m.setFlash("Kill cancelled", false)
		return m, nil
	}
	m.busy = "Killing…"
	backend, ctx := m.backend, m.ctx
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		res, err := backend.Kill(ctx, target)
		if err != nil {
			return dashActionMsg{err: err}
		}
		id := res.SessionID
		if id == "" {
			id = target
		}
		switch {
		case res.CleanupPending:
			return dashActionMsg{text: fmt.Sprintf("Session %s killed (workspace cleanup pending)", id)}
		case res.Freed:
			return dashActionMsg{text: fmt.Sprintf("Session %s killed", id)}
		default:
			return dashActionMsg{text: fmt.Sprintf("Session %s killed (workspace preserved)", id)}
		}
	}
}

// spawnProject picks the project a new worker goes to: the sidebar's
// project, else the selected session's, else the only project in view.
func (m dashModel) spawnProject() string {
	projects := m.projects()
	if m.projectCursor > 0 && m.projectCursor <= len(projects) {
		return projects[m.projectCursor-1].ID
	}
	if sess, ok := m.selected(); ok && sess.ProjectID != "" {
		return sess.ProjectID
	}
	if len(projects) == 1 {
		return projects[0].ID
	}
	return ""
}

func (m dashModel) beginSpawn() (tea.Model, tea.Cmd) {
	m, blocked := m.actionBlocked()
	if blocked {
		return m, nil
	}
	project := m.spawnProject()
	if project == "" {
		m.setFlash("Select a project in the sidebar first", true)
		return m, nil
	}
	m.mode = modeSpawn
	m.spawnTarget = project
	m.spawnField = 0
	m.spawnName.SetValue("")
	m.spawnPrompt.Reset()
	m.spawnPrompt.Blur()
	focusCmd := m.spawnName.Focus()
	return m, focusCmd
}

func (m dashModel) handleSpawnKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeBrowse
		m.spawnName.Blur()
		m.spawnPrompt.Blur()
		return m, nil
	case "tab", "shift+tab":
		return m.toggleSpawnField()
	case "enter":
		if m.spawnField == 0 {
			return m.toggleSpawnField()
		}
		return m.submitSpawn()
	}
	var cmd tea.Cmd
	if m.spawnField == 0 {
		m.spawnName, cmd = m.spawnName.Update(msg)
	} else {
		m.spawnPrompt, cmd = m.spawnPrompt.Update(msg)
	}
	return m, cmd
}

func (m dashModel) toggleSpawnField() (tea.Model, tea.Cmd) {
	if m.spawnField == 0 {
		m.spawnField = 1
		m.spawnName.Blur()
		focusCmd := m.spawnPrompt.Focus()
		return m, focusCmd
	}
	m.spawnField = 0
	m.spawnPrompt.Blur()
	focusCmd := m.spawnName.Focus()
	return m, focusCmd
}

func (m dashModel) submitSpawn() (tea.Model, tea.Cmd) {
	name := strings.TrimSpace(m.spawnName.Value())
	if name == "" {
		m.setFlash("A task name is required", true)
		m.spawnField = 1
		return m.toggleSpawnField()
	}
	if utf8.RuneCountInString(name) > maxDisplayNameLen {
		m.setFlash(fmt.Sprintf("Task names are at most %d characters", maxDisplayNameLen), true)
		return m, nil
	}
	prompt := strings.TrimSpace(m.spawnPrompt.Value())
	m.mode = modeBrowse
	m.spawnName.Blur()
	m.spawnPrompt.Blur()
	m.busy = "Starting worker…"
	m.flash = ""
	backend, ctx, project := m.backend, m.ctx, m.spawnTarget
	return m, func() tea.Msg {
		sess, err := backend.Spawn(ctx, project, name, prompt)
		if err != nil {
			return dashActionMsg{err: err, selectID: sess.ID}
		}
		return dashActionMsg{text: fmt.Sprintf("Started %s (%s)", sess.ID, name), selectID: sess.ID}
	}
}

// updateInputs forwards non-key messages (cursor blink, paste) to the
// focused input.
func (m dashModel) updateInputs(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.mode {
	case modeFilter:
		m.filter, cmd = m.filter.Update(msg)
	case modeSend:
		m.compose, cmd = m.compose.Update(msg)
	case modeSpawn:
		if m.spawnField == 0 {
			m.spawnName, cmd = m.spawnName.Update(msg)
		} else {
			m.spawnPrompt, cmd = m.spawnPrompt.Update(msg)
		}
	}
	return m, cmd
}

func (m *dashModel) layoutInputs() {
	w := max(m.mainWidth()-4, 10)
	m.compose.SetWidth(w)
	m.spawnPrompt.SetWidth(w)
	m.spawnName.SetWidth(w)
	m.filter.SetWidth(max(m.width-4, 10))
}

// projects returns the projects in sidebar order.
func (m dashModel) projects() []projectSummary {
	out := append([]projectSummary(nil), m.snap.Projects...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(dashProjectName(out[i])), strings.ToLower(dashProjectName(out[j]))
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// visibleSessions returns the sessions the main pane lists, grouped by
// project in sidebar order. Inside a project, orchestrators lead and workers
// follow newest first; the order never depends on status, so rows do not
// jump when a status changes.
func (m dashModel) visibleSessions() []dashSession {
	projects := m.projects()
	rank := make(map[string]int, len(projects))
	for i, p := range projects {
		rank[p.ID] = i
	}
	scopeID := ""
	if m.projectCursor > 0 && m.projectCursor <= len(projects) {
		scopeID = projects[m.projectCursor-1].ID
	}
	query := strings.ToLower(strings.TrimSpace(m.filter.Value()))
	out := make([]dashSession, 0, len(m.snap.Sessions))
	for _, s := range m.snap.Sessions {
		if scopeID != "" && s.ProjectID != scopeID {
			continue
		}
		if query != "" && !strings.Contains(dashSearchText(s), query) {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ra, oka := rank[a.ProjectID]
		rb, okb := rank[b.ProjectID]
		if !oka {
			ra = len(projects)
		}
		if !okb {
			rb = len(projects)
		}
		if ra != rb {
			return ra < rb
		}
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		if (a.Kind == "orchestrator") != (b.Kind == "orchestrator") {
			return a.Kind == "orchestrator"
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return out
}

func dashSearchText(s dashSession) string {
	parts := make([]string, 0, 8+len(s.PRList))
	parts = append(parts, s.ID, s.DisplayName, s.ProjectID, s.Harness, s.Branch, s.DisplayStatus, s.Status, sessionRole(s.sessionDTO))
	for _, pr := range s.PRList {
		parts = append(parts, fmt.Sprintf("#%d", pr.Number))
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func (m dashModel) selectedIndex(visible []dashSession) int {
	for i, s := range visible {
		if s.ID == m.selectedID {
			return i
		}
	}
	return 0
}

func (m dashModel) selected() (dashSession, bool) {
	for _, s := range m.visibleSessions() {
		if s.ID == m.selectedID {
			return s, true
		}
	}
	return dashSession{}, false
}

// ensureSelection keeps the selection on a visible session, falling back to
// the first one so the cursor never points at nothing.
func (m *dashModel) ensureSelection() {
	visible := m.visibleSessions()
	for _, s := range visible {
		if s.ID == m.selectedID {
			return
		}
	}
	if len(visible) == 0 {
		m.selectedID = ""
		return
	}
	m.selectedID = visible[0].ID
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
