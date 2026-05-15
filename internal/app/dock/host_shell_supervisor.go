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

// startShellSupervisor spawns the supervisor goroutine for runID. It
// must be called exactly once per shell run, right after the session
// is inserted into the registry.
func (s *Server) startShellSupervisor(conn *agentConn, runID int64) {
	go s.runShellSupervisor(conn, runID)
}

func (s *Server) runShellSupervisor(conn *agentConn, runID int64) {
	eventCh := conn.trackSkillPending(runID)
	defer conn.untrackSkillPending(runID)

	cleanup := func(state, reason string) {
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
				// agentConn closed its dispatcher; treat as agent gone.
				cleanup("ended", "agent_disconnect")
				return
			}
			// Forward to attached bridge (drop-on-full inside).
			s.shellSessions.forwardEvent(runID, ev)
			if ev.EventKind == "exit" {
				reason, _ := ev.Data["reason"].(string)
				cleanup("ended", reason)
				return
			}
		case <-conn.close:
			cleanup("ended", "agent_disconnect")
			return
		}
	}
}
