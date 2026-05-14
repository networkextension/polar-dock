package dock

// Host module Phase 2 — in-memory shell session registry.
//
// Tracks open shell sessions per (host_id) for two reasons:
//   1. Concurrency caps. Per-host hardcoded to 4, global from
//      POLAR_MAX_SHELL_SESSIONS (default 32). The 5th attempt against
//      a host or the (4N+1)th globally returns 429.
//   2. Listing / kicking. GET /api/hosts/:id/skills/shell/sessions
//      reads from here; DELETE … kicks (closes the WS + sends
//      skill.stop to agent).
//
// The registry is intentionally not persisted: a dock restart drops
// every active session (the agent-side bash dies the moment the WS
// disconnect cleanup fires in loop.go). Operators don't expect
// shell sessions to survive a dock bounce.

import (
	"errors"
	"sync"
	"time"
)

const perHostShellCap = 4

var (
	errShellPerHostCapReached = errors.New("shell session cap reached for this host (4)")
	errShellGlobalCapReached  = errors.New("global shell session cap reached")
	errShellSessionNotFound   = errors.New("shell session not found")
)

// shellSession is the dock-side handle for one open shell. Created by
// handleHostShellOpen / handleHostShellWS, closed by either side
// disconnecting. operatorUserID + openedAt feed the sessions list UI.
type shellSession struct {
	RunID          int64     `json:"run_id"`
	HostID         string    `json:"host_id"`
	HostSkillID    int64     `json:"host_skill_id"`
	OperatorUserID string    `json:"operator_user_id"`
	OpenedAt       time.Time `json:"opened_at"`

	// cancel is the byte bridge's context cancel. Calling it tears
	// down both the browser-side and agent-side goroutines.
	cancel func() `json:"-"`
}

type shellSessionRegistry struct {
	mu        sync.Mutex
	globalCap int
	byHost    map[string][]*shellSession
	byRun     map[int64]*shellSession
}

func newShellSessionRegistry(globalCap int) *shellSessionRegistry {
	return &shellSessionRegistry{
		globalCap: globalCap,
		byHost:    map[string][]*shellSession{},
		byRun:     map[int64]*shellSession{},
	}
}

// reserve checks the per-host + global caps, but does NOT yet insert
// the session — caller has to call insert() once it has the runID.
// Two-phase so we can fail-fast on the cap before any DB / WS work.
func (r *shellSessionRegistry) reserve(hostID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byRun) >= r.globalCap {
		return errShellGlobalCapReached
	}
	if len(r.byHost[hostID]) >= perHostShellCap {
		return errShellPerHostCapReached
	}
	return nil
}

// insert finalizes a session into the registry. Caller passes a
// fully-populated *shellSession (with cancel func bound).
func (r *shellSessionRegistry) insert(sess *shellSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byRun) >= r.globalCap {
		return errShellGlobalCapReached
	}
	if len(r.byHost[sess.HostID]) >= perHostShellCap {
		return errShellPerHostCapReached
	}
	r.byHost[sess.HostID] = append(r.byHost[sess.HostID], sess)
	r.byRun[sess.RunID] = sess
	return nil
}

func (r *shellSessionRegistry) remove(runID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.byRun[runID]
	if !ok {
		return
	}
	delete(r.byRun, runID)
	list := r.byHost[sess.HostID]
	for i, s := range list {
		if s.RunID == runID {
			r.byHost[sess.HostID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(r.byHost[sess.HostID]) == 0 {
		delete(r.byHost, sess.HostID)
	}
}

func (r *shellSessionRegistry) get(runID int64) (*shellSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.byRun[runID]
	return sess, ok
}

func (r *shellSessionRegistry) listByHost(hostID string) []shellSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.byHost[hostID]
	out := make([]shellSession, 0, len(list))
	for _, s := range list {
		// Copy — cancel field not exposed to callers.
		out = append(out, shellSession{
			RunID:          s.RunID,
			HostID:         s.HostID,
			HostSkillID:    s.HostSkillID,
			OperatorUserID: s.OperatorUserID,
			OpenedAt:       s.OpenedAt,
		})
	}
	return out
}

// kick calls the session's cancel func, which the byte bridge picks
// up + tears down + removes from the registry on its way out.
func (r *shellSessionRegistry) kick(runID int64) error {
	r.mu.Lock()
	sess, ok := r.byRun[runID]
	r.mu.Unlock()
	if !ok {
		return errShellSessionNotFound
	}
	if sess.cancel != nil {
		sess.cancel()
	}
	return nil
}
