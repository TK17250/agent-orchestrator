package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// dashBackend is everything `ao dash` reads from or asks of the daemon. The
// production implementation is a thin wrapper over the same loopback HTTP
// helpers the other commands use; tests substitute a fake.
type dashBackend interface {
	Snapshot(ctx context.Context, project string) (dashSnapshot, error)
	Detail(ctx context.Context, sess dashSession) (dashDetail, error)
	Send(ctx context.Context, sessionID, message string) error
	Kill(ctx context.Context, sessionID string) (killSessionResponse, error)
	Spawn(ctx context.Context, projectID, name, prompt string) (spawnedSession, error)
	// Watch follows the daemon's change stream until ctx ends, reporting
	// connection state and coalesced change signals on out.
	Watch(ctx context.Context, project string, out chan<- dashStreamEvent)
}

// dashSessionDTO extends the shared session mirror with the board fields the
// daemon already derives for the desktop app: the Kanban lane and the
// renderable status phrase. The CLI never derives these itself.
type dashSessionDTO struct {
	sessionDTO
	Model          string    `json:"model,omitempty"`
	KanbanColumn   string    `json:"kanbanColumn,omitempty"`
	DisplayStatus  string    `json:"displayStatus,omitempty"`
	ProvisionState string    `json:"provisionState,omitempty"`
	LastEventAt    time.Time `json:"lastEventAt"`
	// ChatProviderPreserved reports a live persistent Chat host that outlives
	// the daemon; false also covers unknown ownership.
	ChatProviderPreserved bool `json:"chatProviderPreserved"`
}

type dashSessionListResponse struct {
	Sessions []dashSessionDTO `json:"sessions"`
}

// dashSession is one session row: the daemon read model plus its PR summary.
type dashSession struct {
	dashSessionDTO
	PRList []sessionListPR
	// PRURLs keeps the PR links from the session read model by number.
	PRURLs map[int]string
	// PRStates keeps open/merged/closed/draft from the read model by number.
	PRStates map[int]string
}

type dashSnapshot struct {
	Projects []projectSummary
	Sessions []dashSession
	// DaemonRole describes who started the daemon, for the header.
	DaemonRole string
}

// dashReport mirrors controllers.ReportResponse.
type dashReport struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"sessionId"`
	ProjectID   string    `json:"projectId"`
	State       string    `json:"state,omitempty"`
	Note        string    `json:"note,omitempty"`
	Message     string    `json:"message,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	RepeatCount int64     `json:"repeatCount"`
	Outputs     []struct {
		Kind      string `json:"kind"`
		Reference string `json:"reference"`
		Label     string `json:"label,omitempty"`
	} `json:"outputs,omitempty"`
}

type dashReportListResponse struct {
	Reports []dashReport `json:"reports"`
}

// dashDetail is the secondary read for the session detail view.
type dashDetail struct {
	SessionID  string
	Reports    []dashReport         // newest first
	Turns      []domain.HandoffTurn // newest first
	TurnsTotal int
	// Problems lists the parts that could not be read; the rest still renders.
	Problems []string
}

// dashStreamEvent is one signal from the change-stream watcher.
type dashStreamEvent struct {
	// Connected reports whether the stream is currently attached.
	Connected bool
	// Changed reports that the daemon published a relevant change.
	Changed bool
}

const (
	dashDetailReports = 5
	dashDetailTurns   = 3
	// dashStreamMaxBackoff caps the reconnect delay after the stream drops.
	dashStreamMaxBackoff = 15 * time.Second
)

type httpDashBackend struct {
	c *commandContext
	// daemon, when set, reports the daemon role for the header.
	daemon *dashDaemon
}

func (b httpDashBackend) Snapshot(ctx context.Context, project string) (dashSnapshot, error) {
	var projects projectListResult
	if err := b.c.getJSON(ctx, "projects", &projects); err != nil {
		return dashSnapshot{}, err
	}
	snap := dashSnapshot{Projects: projects.Projects}
	if b.daemon != nil {
		snap.DaemonRole = b.daemon.Role()
	}
	if project != "" {
		snap.Projects = nil
		for _, p := range projects.Projects {
			if p.ID == project {
				snap.Projects = []projectSummary{p}
			}
		}
		if len(snap.Projects) == 0 {
			return dashSnapshot{}, usageError{fmt.Errorf("project %q is not registered; see `ao project ls`", project)}
		}
	}

	params := url.Values{}
	params.Set("active", "true")
	if project != "" {
		params.Set("project", project)
	}
	var list dashSessionListResponse
	if err := b.c.getJSON(ctx, apiPath("sessions", params), &list); err != nil {
		return dashSnapshot{}, err
	}
	base := make([]sessionDTO, 0, len(list.Sessions))
	for _, s := range list.Sessions {
		base = append(base, s.sessionDTO)
	}
	summaries, err := b.c.listSessionPRSummaries(ctx, base)
	if err != nil {
		return dashSnapshot{}, err
	}
	snap.Sessions = make([]dashSession, 0, len(list.Sessions))
	for _, s := range list.Sessions {
		row := dashSession{dashSessionDTO: s, PRList: listPRsForSession(s.sessionDTO, summaries[s.ID])}
		if len(s.PRs) > 0 {
			row.PRURLs = make(map[int]string, len(s.PRs))
			row.PRStates = make(map[int]string, len(s.PRs))
			for _, pr := range s.PRs {
				row.PRURLs[pr.Number] = pr.URL
				row.PRStates[pr.Number] = pr.State
			}
		}
		snap.Sessions = append(snap.Sessions, row)
	}
	return snap, nil
}

func (b httpDashBackend) Detail(ctx context.Context, sess dashSession) (dashDetail, error) {
	detail := dashDetail{SessionID: sess.ID}
	if sess.ProjectID != "" {
		var reports dashReportListResponse
		params := url.Values{}
		params.Set("projectId", sess.ProjectID)
		if err := b.c.getJSON(ctx, apiPath("reports", params), &reports); err != nil {
			if errors.Is(err, errDaemonUnavailable) {
				return dashDetail{}, err
			}
			detail.Problems = append(detail.Problems, "reports: "+err.Error())
		} else {
			detail.Reports = latestSessionReports(reports.Reports, sess.ID, dashDetailReports)
		}
	}
	var history sessionHistoryResponse
	query := url.Values{"offset": {"0"}, "limit": {fmt.Sprint(dashDetailTurns)}}
	if err := b.c.getJSON(ctx, "sessions/"+url.PathEscape(sess.ID)+"/history?"+query.Encode(), &history); err != nil {
		if errors.Is(err, errDaemonUnavailable) {
			return dashDetail{}, err
		}
		detail.Problems = append(detail.Problems, "turns: "+err.Error())
	} else {
		detail.Turns = history.Turns
		detail.TurnsTotal = history.Total
	}
	return detail, nil
}

// latestSessionReports keeps one session's newest reports, newest first.
func latestSessionReports(all []dashReport, sessionID string, limit int) []dashReport {
	out := make([]dashReport, 0, limit)
	for _, r := range all {
		if r.SessionID == sessionID {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Send uses the same daemon route and sender attribution as `ao send`.
func (b httpDashBackend) Send(ctx context.Context, sessionID, message string) error {
	return b.c.sendMessage(ctx, sendOptions{session: sessionID, message: message})
}

// Kill uses the same daemon route as `ao session kill`.
func (b httpDashBackend) Kill(ctx context.Context, sessionID string) (killSessionResponse, error) {
	return b.c.postKillSession(ctx, sessionID)
}

// Spawn starts a worker the way `ao spawn --project <id> --name <name>
// --prompt <prompt>` does: the project's configured worker agent, an async
// start, and a wait until the start settles.
func (b httpDashBackend) Spawn(ctx context.Context, projectID, name, prompt string) (spawnedSession, error) {
	project, err := b.c.fetchProjectDetails(ctx, projectID)
	if err != nil {
		return spawnedSession{}, err
	}
	harness, err := resolveSpawnHarness("", "worker", project)
	if err != nil {
		return spawnedSession{}, err
	}
	res, err := b.c.submitSpawn(ctx, spawnRequest{
		ProjectID:           project.ID,
		ParentSessionID:     strings.TrimSpace(os.Getenv("AO_SESSION_ID")),
		TrackerProvider:     "github",
		Kind:                "worker",
		Harness:             harness,
		Prompt:              prompt,
		DisplayName:         name,
		DisplayNameExplicit: true,
		Async:               true,
	})
	return res.Session, err
}

// Watch follows GET /api/v1/events (the CDC stream the desktop app uses) and
// reconnects with backoff. The model polls on its own while disconnected, so
// a missing or restarting daemon never stalls the dashboard.
func (b httpDashBackend) Watch(ctx context.Context, project string, out chan<- dashStreamEvent) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		_ = b.streamOnce(ctx, project, out)
		if !dashEmit(ctx, out, dashStreamEvent{Connected: false}) {
			return
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, dashStreamMaxBackoff)
	}
}

func (b httpDashBackend) streamOnce(ctx context.Context, project string, out chan<- dashStreamEvent) error {
	base, err := b.c.daemonBaseURL()
	if err != nil {
		return err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/api/v1/events?after=latest", http.NoBody) // #nosec G704 -- fixed loopback daemon URL.
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	client := *b.c.deps.HTTPClient
	client.Timeout = 0          // a stream stays open; ctx ends it.
	resp, err := client.Do(req) // #nosec G704 -- fixed loopback daemon URL.
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("event stream: HTTP %d", resp.StatusCode)
	}
	if !dashEmit(ctx, out, dashStreamEvent{Connected: true}) {
		return ctx.Err()
	}
	go dashWatchListener(streamCtx, cancel, base)
	return readDashEvents(resp.Body, project, func() {
		// A full buffer already holds a pending change; dropping is coalescing.
		select {
		case out <- dashStreamEvent{Connected: true, Changed: true}:
		default:
		}
	})
}

// dashListenerProbeInterval paces the listener probe. A var so tests can
// shorten it.
var dashListenerProbeInterval = 3 * time.Second

// dashWatchListener drops the stream once the daemon stops accepting new
// connections. A daemon shutting down closes its listener at once but keeps
// serving open streams until its graceful timeout, and while this stream stays
// open the daemon cannot exit: a desktop app replacing a dash-started daemon
// would wait for it. Two failed probes in a row end the stream; Watch then
// reconnects to whichever daemon is running next.
func dashWatchListener(ctx context.Context, cancel context.CancelFunc, base string) {
	client := &http.Client{Timeout: probeTimeout, Transport: &http.Transport{DisableKeepAlives: true}}
	ticker := time.NewTicker(dashListenerProbeInterval)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", http.NoBody) // #nosec G704 -- fixed loopback daemon URL.
		if err != nil {
			return
		}
		resp, err := client.Do(req) // #nosec G704 -- fixed loopback daemon URL.
		if err == nil {
			_ = resp.Body.Close()
			failures = 0
			continue
		}
		if ctx.Err() != nil {
			return
		}
		failures++
		if failures >= 2 {
			cancel()
			return
		}
	}
}

// readDashEvents parses an SSE body and calls changed for each data event that
// is relevant to project (all projects when empty). Cursor and heartbeat
// frames carry no change.
func readDashEvents(body io.Reader, project string, changed func()) error {
	reader := bufio.NewReader(body)
	var name, data string
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if name != "" && name != "heartbeat" && name != "cursor" && dashEventInScope(data, project) {
				changed()
			}
			name, data = "", ""
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func dashEventInScope(data, project string) bool {
	if project == "" {
		return true
	}
	var ev struct {
		ProjectID string `json:"projectId"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil || ev.ProjectID == "" {
		return true
	}
	return ev.ProjectID == project
}

func dashEmit(ctx context.Context, out chan<- dashStreamEvent, ev dashStreamEvent) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}
