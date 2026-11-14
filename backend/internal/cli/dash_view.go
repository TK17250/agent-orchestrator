package cli

import (
	"fmt"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Semantic colours from DESIGN.md "Operational state and color". Every
// coloured glyph is paired with a distinct shape and a text label, so colour
// never carries meaning alone.
var (
	dashColorWorking  = lipgloss.Color("#60a5fa")
	dashColorNeedsYou = lipgloss.Color("#f06445")
	dashColorValidate = lipgloss.Color("#e58bb5")
	dashColorReview   = lipgloss.Color("#f59e0b")
	dashColorReady    = lipgloss.Color("#4ade80")
	dashColorMerged   = lipgloss.Color("#c084fc")
	dashColorFailure  = lipgloss.Color("#ef4444")
	dashColorMuted    = lipgloss.Color("#8a8a8a")
	dashColorSelected = lipgloss.Color("#2b3140")

	dashMuted = lipgloss.NewStyle().Foreground(dashColorMuted)
	dashBold  = lipgloss.NewStyle().Bold(true)
	dashPlain = lipgloss.NewStyle()
)

// dashTone is the one status signal a row carries, mirroring the desktop
// sidebar's row indicator: the working spinner first, then what the user has
// to act on, then the PR state, else a quiet dot.
type dashTone int

const (
	toneQuiet dashTone = iota
	toneWorking
	toneAttention
	toneReview
	toneReady
	toneMerged
)

func (t dashTone) glyph() string {
	switch t {
	case toneWorking:
		return "◐"
	case toneAttention:
		return "▲"
	case toneReview:
		return "◆"
	case toneReady, toneMerged:
		return "✓"
	default:
		return "○"
	}
}

func (t dashTone) style() lipgloss.Style {
	switch t {
	case toneWorking:
		return lipgloss.NewStyle().Foreground(dashColorWorking)
	case toneAttention:
		return lipgloss.NewStyle().Foreground(dashColorNeedsYou)
	case toneReview:
		return lipgloss.NewStyle().Foreground(dashColorReview)
	case toneReady:
		return lipgloss.NewStyle().Foreground(dashColorReady)
	case toneMerged:
		return lipgloss.NewStyle().Foreground(dashColorMerged)
	default:
		return dashMuted
	}
}

// dashSessionNeedsInput mirrors the desktop's sessionNeedsInput.
func dashSessionNeedsInput(s dashSession) bool {
	return s.Status == "needs_input" || s.Activity.State == "waiting_input" || s.Activity.State == "blocked" ||
		s.DisplayStatus == "Blocked" || s.DisplayStatus == "Needs input"
}

// dashIndicator mirrors the desktop's getSessionRowIndicator from the
// daemon-derived displayStatus and kanbanColumn; it derives nothing new.
func dashIndicator(s dashSession) dashTone {
	display := s.DisplayStatus
	if display == "Terminated" || s.Status == "terminated" || display == "Closed without merge" {
		return toneQuiet
	}
	if s.ProvisionState == "provisioning" || s.Activity.State == "active" {
		return toneWorking
	}
	if dashSessionNeedsInput(s) {
		return toneAttention
	}
	switch display {
	case "CI failing", "Merge conflicts", "Changes requested":
		return toneAttention
	case "":
		if s.Status == "ci_failed" || s.Status == "changes_requested" {
			return toneAttention
		}
	}
	switch s.KanbanColumn {
	case "needs_review":
		if display == "Draft" {
			return toneQuiet
		}
		return toneReview
	case "validating":
		if display == "Review failed" {
			return toneReview
		}
	case "ready":
		switch display {
		case "Merged":
			return toneMerged
		case "Mergeable", "Approved":
			return toneReady
		}
	}
	return toneQuiet
}

// dashLaneLabel is the desktop board's lane name for a Kanban column.
func dashLaneLabel(column string) string {
	switch column {
	case "building":
		return "Building"
	case "validating":
		return "Validating"
	case "needs_review":
		return "In review"
	case "ready":
		return "Ready"
	case "archive":
		return "Archive"
	default:
		return ""
	}
}

func dashLaneColor(column string) lipgloss.Style {
	switch column {
	case "building":
		return lipgloss.NewStyle().Foreground(dashColorWorking)
	case "validating":
		return lipgloss.NewStyle().Foreground(dashColorValidate)
	case "needs_review":
		return lipgloss.NewStyle().Foreground(dashColorReview)
	case "ready":
		return lipgloss.NewStyle().Foreground(dashColorReady)
	default:
		return dashMuted
	}
}

func dashStatusText(s dashSession) string {
	if s.DisplayStatus != "" {
		return dashClean(s.DisplayStatus)
	}
	return dashSentence(s.Status)
}

func dashCILabel(state string) (string, lipgloss.Style) {
	switch state {
	case "passing":
		return "Passing", lipgloss.NewStyle().Foreground(dashColorReady)
	case "failing":
		return "Failing", lipgloss.NewStyle().Foreground(dashColorFailure)
	case "pending":
		return "Pending", lipgloss.NewStyle().Foreground(dashColorReview)
	default:
		return "-", dashMuted
	}
}

func dashReviewLabel(decision string) (string, lipgloss.Style) {
	switch decision {
	case "approved":
		return "Approved", lipgloss.NewStyle().Foreground(dashColorReady)
	case "changes_requested":
		return "Changes requested", lipgloss.NewStyle().Foreground(dashColorNeedsYou)
	case "review_required":
		return "Review required", lipgloss.NewStyle().Foreground(dashColorReview)
	default:
		return "-", dashMuted
	}
}

func dashActivityLabel(state string) string {
	switch state {
	case "active":
		return "Working"
	case "waiting_input":
		return "Waiting for input"
	case "":
		return "-"
	default:
		return dashSentence(state)
	}
}

func dashKindLabel(s dashSession) string {
	if s.Kind == "orchestrator" {
		return "Orchestrator"
	}
	return "Worker"
}

// dashSentence turns an enum value such as "needs_input" into "Needs input".
func dashSentence(v string) string {
	v = strings.TrimSpace(strings.ReplaceAll(dashClean(v), "_", " "))
	if v == "" {
		return "-"
	}
	r := []rune(v)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// dashClean makes daemon-provided text safe for one terminal line: escape
// sequences and control or bidi-format characters are removed (session
// names, reports and turns are untrusted model output) and whitespace runs
// collapse to one space.
func dashClean(s string) string {
	s = ansi.Strip(s)
	var b strings.Builder
	pendingSpace := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r) || unicode.IsControl(r):
			pendingSpace = b.Len() > 0
			continue
		case unicode.Is(unicode.Cf, r):
			continue
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

func dashProjectName(p projectSummary) string {
	if name := dashClean(p.Name); name != "" {
		return name
	}
	return p.ID
}

func dashSessionTitle(s dashSession) string {
	if name := dashClean(s.DisplayName); name != "" {
		return name
	}
	return s.ID
}

// fitText truncates s to w cells and pads it to exactly w.
func fitText(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// fitLine pads or truncates an already styled line to w cells.
func fitLine(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

func (m dashModel) sidebarWidth() int {
	if m.width < 60 {
		return 0
	}
	return min(28, max(18, m.width/5))
}

func (m dashModel) mainWidth() int {
	if sw := m.sidebarWidth(); sw > 0 {
		return max(m.width-sw-3, 10)
	}
	return max(m.width, 10)
}

func (m dashModel) bodyHeight() int {
	return max(m.height-4, 3)
}

// View renders the full screen in the alternate buffer.
func (m dashModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m dashModel) render() string {
	width := max(m.width, 20)
	rule := dashMuted.Render(strings.Repeat("─", width))
	lines := []string{fitLine(m.renderHeader(width), width), rule}

	bodyH := m.bodyHeight()
	mainLines := m.renderMain(m.mainWidth(), bodyH)
	sw := m.sidebarWidth()
	var sideLines []string
	if sw > 0 {
		sideLines = m.renderSidebar(sw, bodyH)
	}
	divider := dashMuted.Render(" │ ")
	for i := range bodyH {
		main := ""
		if i < len(mainLines) {
			main = mainLines[i]
		}
		if sw == 0 {
			lines = append(lines, fitLine(main, width))
			continue
		}
		side := ""
		if i < len(sideLines) {
			side = sideLines[i]
		}
		lines = append(lines, fitLine(side, sw)+divider+fitLine(main, m.mainWidth()))
	}
	lines = append(lines, rule, fitLine(m.renderFooter(width), width))
	return strings.Join(lines, "\n")
}

func (m dashModel) renderHeader(width int) string {
	scope := "All projects"
	projects := m.projects()
	if m.scope != "" && len(projects) == 1 {
		scope = dashProjectName(projects[0])
	} else if m.projectCursor > 0 && m.projectCursor <= len(projects) {
		scope = dashProjectName(projects[m.projectCursor-1])
	}
	left := dashBold.Render("Agent Orchestrator") + dashMuted.Render(" · "+scope)

	var conn string
	switch {
	case m.daemonDown:
		conn = lipgloss.NewStyle().Foreground(dashColorFailure).Render("● Daemon not reachable")
	case m.live:
		conn = lipgloss.NewStyle().Foreground(dashColorReady).Render("● Live")
	default:
		conn = dashMuted.Render("○ Polling")
	}
	if role := dashClean(m.snap.DaemonRole); role != "" {
		conn = dashMuted.Render(role+" · ") + conn
	}
	right := conn
	if m.loaded {
		working, needYou := 0, 0
		for _, s := range m.snap.Sessions {
			switch dashIndicator(s) {
			case toneWorking:
				working++
			case toneAttention:
				needYou++
			}
		}
		counts := fmt.Sprintf("%d session%s · %d working", len(m.snap.Sessions), pluralS(len(m.snap.Sessions)), working)
		counts = dashMuted.Render(counts)
		if needYou > 0 {
			verb := "need"
			if needYou == 1 {
				verb = "needs"
			}
			counts += dashMuted.Render(" · ") + lipgloss.NewStyle().Foreground(dashColorNeedsYou).Render(fmt.Sprintf("%d %s you", needYou, verb))
		}
		right = counts + "   " + conn
	}
	gap := width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 2 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m dashModel) renderSidebar(w, h int) []string {
	counts := map[string]int{}
	for _, s := range m.snap.Sessions {
		counts[s.ProjectID]++
	}
	type item struct {
		label string
		count int
	}
	projects := m.projects()
	items := make([]item, 0, 1+len(projects))
	items = append(items, item{"All projects", len(m.snap.Sessions)})
	for _, p := range projects {
		items = append(items, item{dashProjectName(p), counts[p.ID]})
	}
	lines := make([]string, 0, 1+len(items))
	lines = append(lines, dashMuted.Render("Projects"))
	for i, it := range items {
		selected := i == m.projectCursor
		cursor := " "
		if selected {
			cursor = "›"
		}
		count := fmt.Sprint(it.count)
		labelW := w - 2 - len(count) - 1
		text := cursor + " " + fitText(it.label, labelW) + " " + count
		style := dashPlain
		switch {
		case selected && m.focus == paneProjects:
			style = style.Background(dashColorSelected).Bold(true)
		case selected:
			style = style.Bold(true)
		}
		lines = append(lines, style.Render(fitText(text, w)))
	}
	return clipLines(lines, h, max(m.projectCursor+1, 0))
}

// clipLines returns at most h lines, scrolled so that line keep is visible.
func clipLines(lines []string, h, keep int) []string {
	if len(lines) <= h {
		return lines
	}
	start := 0
	if keep >= h {
		start = keep - h + 1
	}
	end := min(start+h, len(lines))
	return lines[start:end]
}

func (m dashModel) renderMain(w, h int) []string {
	var lines []string
	if m.daemonDown {
		lines = append(lines,
			lipgloss.NewStyle().Foreground(dashColorFailure).Render("The Orchestrator daemon is not reachable."),
			dashMuted.Render("Start it with `ao start`; this dashboard reconnects on its own."),
			"")
	} else if m.fetchErr != nil {
		lines = append(lines, lipgloss.NewStyle().Foreground(dashColorFailure).Render("Refresh failed: "+dashClean(m.fetchErr.Error())), "")
	}
	switch {
	case m.mode == modeHelp:
		return append(lines, m.renderHelp(w)...)
	case m.mode == modeQuit:
		return append(lines, m.renderQuit(w)...)
	case m.mode == modeSend:
		return append(lines, m.renderSend(w)...)
	case m.mode == modeKill:
		return append(lines, m.renderKill(w)...)
	case m.mode == modeSpawn:
		return append(lines, m.renderSpawn(w)...)
	case !m.loaded:
		if m.daemonDown {
			return lines
		}
		return append(lines, dashMuted.Render("Loading…"))
	case m.detailOpen:
		return clipLines(append(lines, m.renderDetail(w)...), h, 0)
	}
	head := len(lines)
	table, selLine := m.renderTable(w)
	lines = append(lines, table...)
	return clipLines(lines, h, head+selLine)
}

type dashColumn struct {
	title string
	width int
}

// dashTableColumns picks the columns that fit, dropping the least important
// first.
func dashTableColumns(w int) []dashColumn {
	cols := []dashColumn{
		{title: "Session", width: 18},
		{title: "Kind", width: 12},
		{title: "Agent", width: 11},
		{title: "Status", width: 19},
		{title: "Branch", width: 11},
		{title: "PR", width: 6},
		{title: "CI", width: 7},
		{title: "Review", width: 17},
		{title: "Updated", width: 7},
	}
	dropOrder := []string{"Branch", "Agent", "Review", "Kind", "Updated", "CI"}
	used := func() int {
		total := 2 // indicator
		for _, c := range cols {
			total += c.width + 1
		}
		return total
	}
	for _, name := range dropOrder {
		if used() <= w {
			break
		}
		for i, c := range cols {
			if c.title == name {
				cols = append(cols[:i], cols[i+1:]...)
				break
			}
		}
	}
	// Spare width goes to the name first, then the branch, then the name again.
	spare := w - used()
	grow := map[string]int{"Session": 14, "Branch": 20}
	for i := range cols {
		if add := min(spare, grow[cols[i].title]); add > 0 {
			cols[i].width += add
			spare -= add
		}
	}
	if spare > 0 {
		cols[0].width += spare
	}
	return cols
}

// renderTable lists sessions grouped by project and returns the line index of
// the selected row.
func (m dashModel) renderTable(w int) ([]string, int) {
	var lines []string
	if q := strings.TrimSpace(m.filter.Value()); q != "" && m.mode != modeFilter {
		lines = append(lines, dashMuted.Render(fmt.Sprintf("Filtered by “%s” · esc clears", dashClean(q))))
	}
	projects := m.projects()
	if len(projects) == 0 && len(m.snap.Sessions) == 0 {
		return append(lines,
			"No projects yet.",
			dashMuted.Render("Add one with `ao project add --path <repo>`.")), 0
	}
	visible := m.visibleSessions()
	if len(visible) == 0 {
		if q := strings.TrimSpace(m.filter.Value()); q != "" {
			return append(lines, fmt.Sprintf("No sessions match “%s”.", dashClean(q))), 0
		}
		return append(lines, "No active sessions.", dashMuted.Render("Press n to start a worker.")), 0
	}

	cols := dashTableColumns(w)
	// Rows are cursor + glyph, then " " + cell per column; align with them.
	header := "  "
	for _, c := range cols {
		header += " " + fitText(c.title, c.width)
	}
	lines = append(lines, dashMuted.Render(header))

	names := map[string]string{}
	for _, p := range projects {
		names[p.ID] = dashProjectName(p)
	}
	grouped := m.projectCursor == 0 && (m.scope == "" || len(projects) != 1)
	counts := map[string]int{}
	for _, s := range visible {
		counts[s.ProjectID]++
	}
	selLine := 0
	current := "\x00"
	for _, s := range visible {
		if grouped && s.ProjectID != current {
			current = s.ProjectID
			name := names[s.ProjectID]
			if name == "" {
				name = s.ProjectID
			}
			if name == "" {
				name = "No project"
			}
			if len(lines) > 1 {
				lines = append(lines, "")
			}
			lines = append(lines, dashBold.Render(name)+dashMuted.Render(fmt.Sprintf("  %d", counts[s.ProjectID])))
		}
		selected := s.ID == m.selectedID
		if selected {
			selLine = len(lines)
		}
		lines = append(lines, m.renderRow(s, cols, selected))
	}
	return lines, selLine
}

func (m dashModel) renderRow(s dashSession, cols []dashColumn, selected bool) string {
	highlight := selected && m.focus == paneSessions && m.mode == modeBrowse
	paint := func(text string, st lipgloss.Style) string {
		if highlight {
			st = st.Background(dashColorSelected)
		}
		return st.Render(text)
	}
	tone := dashIndicator(s)
	cursor := " "
	if selected {
		cursor = "›"
	}
	var b strings.Builder
	b.WriteString(paint(cursor, dashPlain))
	b.WriteString(paint(tone.glyph(), tone.style()))
	pr, ci, review := dashPRCells(s)
	for _, c := range cols {
		var text string
		st := dashPlain
		switch c.title {
		case "Session":
			text = dashSessionTitle(s)
			if selected {
				st = st.Bold(true)
			}
		case "Kind":
			text = dashKindLabel(s)
			st = dashMuted
		case "Agent":
			text = emptyDash(dashClean(s.Harness))
			st = dashMuted
		case "Status":
			text = dashStatusText(s)
			if tone != toneQuiet {
				st = tone.style()
			}
		case "Branch":
			text = emptyDash(dashClean(s.Branch))
			st = dashMuted
		case "PR":
			text = pr
		case "CI":
			text, st = ci.text, ci.style
		case "Review":
			text, st = review.text, review.style
		case "Updated":
			text = dashAge(m, s)
			st = dashMuted
		}
		b.WriteString(paint(" ", dashPlain))
		b.WriteString(paint(fitText(text, c.width), st))
	}
	return b.String()
}

type dashCell struct {
	text  string
	style lipgloss.Style
}

// dashPRCells summarizes a session's PRs: the first PR's number (with a count
// of any others), and that PR's CI and review state.
func dashPRCells(s dashSession) (string, dashCell, dashCell) {
	if len(s.PRList) == 0 {
		return "-", dashCell{"-", dashMuted}, dashCell{"-", dashMuted}
	}
	pr := s.PRList[0]
	num := fmt.Sprintf("#%d", pr.Number)
	if len(s.PRList) > 1 {
		num += fmt.Sprintf("+%d", len(s.PRList)-1)
	}
	ciText, ciStyle := dashCILabel(pr.CI)
	reviewText, reviewStyle := dashReviewLabel(pr.Review)
	return num, dashCell{ciText, ciStyle}, dashCell{reviewText, reviewStyle}
}

func dashAge(m dashModel, s dashSession) string {
	at := s.LastEventAt
	if at.IsZero() {
		at = s.Activity.LastActivityAt
	}
	if at.IsZero() {
		at = s.UpdatedAt
	}
	return sessionAge(m.now(), at)
}

func (m dashModel) renderDetail(w int) []string {
	s, ok := m.selected()
	if !ok {
		return []string{dashMuted.Render("No session selected.")}
	}
	tone := dashIndicator(s)
	lines := []string{tone.style().Render(tone.glyph()) + " " + dashBold.Render(dashSessionTitle(s))}
	status := dashStatusText(s)
	if tone != toneQuiet {
		status = tone.style().Render(status)
	}
	if lane := dashLaneLabel(s.KanbanColumn); lane != "" {
		status += dashMuted.Render(" · ") + dashLaneColor(s.KanbanColumn).Render(lane)
	}
	lines = append(lines, status, "")

	field := func(label, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		lines = append(lines, dashMuted.Render(fitText(label, 10))+" "+fitText(value, max(w-11, 1)))
	}
	project := s.ProjectID
	for _, p := range m.snap.Projects {
		if p.ID == s.ProjectID {
			project = dashProjectName(p)
		}
	}
	agent := dashClean(s.Harness)
	if model := dashClean(s.Model); model != "" {
		agent += " · " + model
	}
	activity := dashActivityLabel(s.Activity.State)
	if !s.Activity.LastActivityAt.IsZero() {
		activity += " · " + sessionAge(m.now(), s.Activity.LastActivityAt)
	}
	field("Session", s.ID)
	field("Project", project)
	field("Kind", dashKindLabel(s))
	field("Agent", agent)
	field("Branch", dashClean(formatBranchState(s.sessionDTO)))
	field("Issue", dashClean(s.IssueID))
	field("Activity", activity)
	if !s.CreatedAt.IsZero() {
		field("Created", sessionAge(m.now(), s.CreatedAt))
	}

	if len(s.PRList) > 0 {
		lines = append(lines, "", dashBold.Render("Pull requests"))
		for _, pr := range s.PRList {
			parts := []string{fmt.Sprintf("#%d", pr.Number)}
			if state := s.PRStates[pr.Number]; state != "" {
				parts = append(parts, dashSentence(state))
			}
			ciText, ciStyle := dashCILabel(pr.CI)
			if ciText != "-" {
				parts = append(parts, ciStyle.Render("CI "+strings.ToLower(ciText)))
			}
			if reviewText, reviewStyle := dashReviewLabel(pr.Review); reviewText != "-" {
				parts = append(parts, reviewStyle.Render(reviewText))
			}
			if pr.Threads != nil && *pr.Threads > 0 {
				parts = append(parts, fmt.Sprintf("%d unresolved thread%s", *pr.Threads, pluralS(*pr.Threads)))
			}
			lines = append(lines, "  "+strings.Join(parts, dashMuted.Render(" · ")))
			if u := dashClean(s.PRURLs[pr.Number]); u != "" {
				lines = append(lines, "  "+dashMuted.Render(fitText(u, max(w-2, 1))))
			}
		}
	}

	if m.detail.SessionID != s.ID {
		if m.detailLoading {
			lines = append(lines, "", dashMuted.Render("Loading reports and turns…"))
		}
		return lines
	}
	if len(m.detail.Reports) > 0 {
		lines = append(lines, "", dashBold.Render("Reports"))
		for _, r := range m.detail.Reports {
			text := dashClean(r.Note)
			if text == "" {
				text = dashClean(r.Message)
			}
			if r.RepeatCount > 1 {
				text += fmt.Sprintf(" (×%d)", r.RepeatCount)
			}
			lines = append(lines, "  "+dashMuted.Render(fitText(sessionAge(m.now(), r.CreatedAt), 8))+" "+
				fitText(dashSentence(r.State), 12)+" "+fitText(text, max(w-24, 1)))
		}
	}
	if len(m.detail.Turns) > 0 {
		lines = append(lines, "", dashBold.Render(fmt.Sprintf("Recent turns (%d of %d)", len(m.detail.Turns), m.detail.TurnsTotal)))
		for _, t := range m.detail.Turns {
			age := "-"
			if !t.At.IsZero() {
				age = sessionAge(m.now(), t.At)
			}
			lines = append(lines,
				"  "+dashMuted.Render(fitText(age, 8))+" "+fitText("User: "+dashClean(t.UserRequest), max(w-11, 1)),
				"  "+strings.Repeat(" ", 8)+" "+dashMuted.Render(fitText("Agent: "+emptyDash(dashClean(t.AgentResponse)), max(w-11, 1))))
		}
	}
	if len(m.detail.Reports) == 0 && len(m.detail.Turns) == 0 && len(m.detail.Problems) == 0 {
		lines = append(lines, "", dashMuted.Render("No reports or turns yet."))
	}
	for _, p := range m.detail.Problems {
		lines = append(lines, dashMuted.Render(fitText("Could not read "+dashClean(p), w)))
	}
	return lines
}

func (m dashModel) renderSend(w int) []string {
	title := "Message"
	if s, ok := m.selected(); ok {
		title = "Message to " + dashSessionTitle(s)
	}
	return append([]string{dashBold.Render(fitText(title, w)), ""}, strings.Split(m.compose.View(), "\n")...)
}

func (m dashModel) renderKill(w int) []string {
	name := m.killTarget
	for _, s := range m.snap.Sessions {
		if s.ID == m.killTarget {
			name = dashSessionTitle(s) + " (" + s.ID + ")"
		}
	}
	return []string{
		lipgloss.NewStyle().Foreground(dashColorFailure).Bold(true).Render(fitText("Kill "+name+"?", w)),
		"",
		"The agent stops and the session moves to the archive.",
		dashMuted.Render("A worktree with uncommitted changes is kept for inspection."),
		"",
		"Press y to kill, any other key to cancel.",
	}
}

func (m dashModel) renderSpawn(w int) []string {
	project := m.spawnTarget
	for _, p := range m.snap.Projects {
		if p.ID == m.spawnTarget {
			project = dashProjectName(p)
		}
	}
	label := func(text string, active bool) string {
		if active {
			return dashBold.Render(text)
		}
		return dashMuted.Render(text)
	}
	return append([]string{
		dashBold.Render(fitText("New worker in "+project, w)),
		dashMuted.Render("Uses the project's worker agent, like `ao spawn`."),
		"",
		label("Task name", m.spawnField == 0),
		"  " + m.spawnName.View(),
		"",
		label("Prompt", m.spawnField == 1),
	}, strings.Split(m.spawnPrompt.View(), "\n")...)
}

// renderQuit asks before stopping a daemon that has sessions mid-turn.
func (m dashModel) renderQuit(w int) []string {
	f := m.quitPlan.InFlight
	lines := []string{
		dashBold.Render(fitText("Stop the Orchestrator daemon?", w)),
		"",
		fmt.Sprintf("ao dash started this daemon, and %d session%s %s mid-turn:", f.Total(), pluralS(f.Total()), isAre(f.Total())),
	}
	if f.Interrupted > 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(dashColorNeedsYou).Render(
			fmt.Sprintf("  %d Chat session%s would be interrupted: the agent stops with the daemon.", f.Interrupted, pluralS(f.Interrupted))))
	}
	if n := f.Unsupervised; n > 0 {
		verb, pronoun := "keep", "them"
		if n == 1 {
			verb, pronoun = "keeps", "it"
		}
		lines = append(lines, fmt.Sprintf("  %d %s running, but AO stops tracking %s until the daemon starts again.", n, verb, pronoun))
	}
	return append(lines, "",
		"enter  Stop the daemon",
		"k      Keep it running in the background (stop it later with `ao stop`)",
		"esc    Cancel and stay in the dashboard")
}

func (m dashModel) renderHelp(w int) []string {
	rows := [][2]string{
		{"j / k, ↓ / ↑", "Move in the focused pane"},
		{"g / G", "First / last"},
		{"tab, h / l", "Switch between projects and sessions"},
		{"enter", "Open session details (sessions) or focus sessions (projects)"},
		{"esc", "Close details or clear the filter"},
		{"/", "Filter sessions by name, id, branch, agent, status or PR"},
		{"m", "Send a message to the selected session"},
		{"x", "Kill the selected session (asks first)"},
		{"n", "Start a new worker in the selected project"},
		{"r", "Refresh now"},
		{"?", "Show or hide this help"},
		{"q, ctrl+c", "Quit"},
	}
	lines := make([]string, 0, len(rows)+4)
	lines = append(lines, dashBold.Render("Keyboard shortcuts"), "")
	for _, r := range rows {
		lines = append(lines, fitText(r[0], 14)+" "+dashMuted.Render(fitText(r[1], max(w-15, 1))))
	}
	lines = append(lines, "",
		dashMuted.Render("Glyphs: ◐ working  ▲ needs you  ◆ in review  ✓ ready (purple when merged)  ○ quiet"))
	return lines
}

func (m dashModel) renderFooter(width int) string {
	if m.mode == modeFilter {
		return m.filter.View()
	}
	if m.busy != "" {
		return lipgloss.NewStyle().Foreground(dashColorWorking).Render(m.busy)
	}
	if m.flash != "" {
		color := dashColorReady
		if m.flashErr {
			color = dashColorFailure
		}
		return lipgloss.NewStyle().Foreground(color).Render(fitText(m.flash, width))
	}
	var hints string
	switch m.mode {
	case modeSend:
		hints = "enter send · ctrl+j newline · esc cancel"
	case modeKill:
		hints = "y kill · any other key cancels"
	case modeSpawn:
		hints = "tab switch field · enter next / start · ctrl+j newline · esc cancel"
	case modeHelp:
		hints = "any key closes help"
	case modeQuit:
		hints = "enter stop daemon · k keep it running · esc cancel"
	default:
		switch {
		case m.focus == paneProjects:
			hints = "j/k project · enter sessions · n new worker · / filter · ? help · q quit"
		case m.detailOpen:
			hints = "j/k next session · esc back · m message · x kill · n new worker · ? help · q quit"
		default:
			hints = "j/k move · enter details · tab projects · / filter · m message · x kill · n new worker · ? help · q quit"
		}
	}
	return dashMuted.Render(fitText(hints, width))
}
