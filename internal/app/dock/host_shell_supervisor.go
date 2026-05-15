package dock

// Per-run supervisor goroutine for shell sessions. Spawned by
// handleHostShellOpen once skill.start is dispatched; lives for the
// full lifetime of the run (NOT just the browser bridge). The
// supervisor owns the agent event channel — so when the operator
// refreshes the browser, the bridge tears down but the supervisor
// keeps observing, and the agent-side PTY stays alive until either
// a) the agent reports EventExit (idle timeout, hard cap, eof, etc.)
// b) the agent connection drops
// c) the operator explicitly kicks the run via DELETE /shell/sessions/:run_id
//
// Reattach is bridge-level: any new WS to /ws/host/:id/shell/:run_id
// gets fan-out through the supervisor's forwardEvent.

import (
	"time"
)

// shellStartSignal is what /shell/open waits on briefly before
// returning — lets the dispatcher report early skill.start failures
// (e.g. agent rejects: missing device_path) synchronously rather than
// the browser later seeing a 404 on the WS connect.
type shellStartSignal struct {
	failed bool
	reason string
}

// startShellSupervisor spawns the supervisor goroutine for runID. It
// must be called exactly once per shell run, right after the session
// is inserted into the registry. The returned channel fires once with
// the agent's first state (running / exit) so /shell/open can detect
// early failures.
func (s *Server) startShellSupervisor(conn *agentConn, runID int64) <-chan shellStartSignal {
	first := make(chan shellStartSignal, 1)
	go s.runShellSupervisor(conn, runID, first)
	return first
}

func (s *Server) runShellSupervisor(conn *agentConn, runID int64, first chan<- shellStartSignal) {
	// Own both channels for the run's lifetime — the bridge no longer
	// calls track/untrack directly, eliminating the race where a
	// detaching old bridge would untrack the new bridge's freshly-
	// installed channel.
	eventCh := conn.trackSkillPending(runID)
	defer conn.untrackSkillPending(runID)
	stdoutCh := conn.trackSkillStdout(runID)
	defer conn.untrackSkillStdout(runID)

	signalFirst := func(failed bool, reason string) {
		if first == nil {
			return
		}
		select {
		case first <- shellStartSignal{failed: failed, reason: reason}:
		default:
		}
		first = nil
	}

	cleanup := func(state, reason string) {
		// If we never signaled first, do it now — exit-before-running
		// is a failure from /open's perspective.
		signalFirst(true, reason)
		_ = s.finalizeHostSkillRun(runID, state, reason, time.Now().UTC())
		if b := s.shellSessions.snatchBridge(runID); b != nil && b.cancel != nil {
			b.cancel()
		}
		s.shellSessions.remove(runID)
	}

	for {
		select {
		case ev, ok := <-eventCh:
			if !ok {
				cleanup("ended", "agent_disconnect")
				return
			}
			s.shellSessions.forwardEvent(runID, ev)
			if ev.EventKind == "state" {
				// First state event = agent confirmed start. Tell /open
				// the run is live.
				signalFirst(false, "")
			}
			if ev.EventKind == "exit" {
				reason, _ := ev.Data["reason"].(string)
				cleanup("ended", reason)
				return
			}
		case chunk, ok := <-stdoutCh:
			if !ok {
				cleanup("ended", "agent_disconnect")
				return
			}
			// First stdout also implies a working PTY — not all skills
			// emit a state=running event before bytes start flowing.
			signalFirst(false, "")
			s.shellSessions.forwardStdout(runID, chunk)
		case <-conn.close:
			cleanup("ended", "agent_disconnect")
			return
		}
	}
}
