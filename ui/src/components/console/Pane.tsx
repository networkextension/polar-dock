// One tile in the grid. Three internal states:
//   "picker"  → empty, shows the host/skill/shell picker
//   "running" → connected to a live shell run; renders Terminal
//   "error"   → /shell/open failed; shows the error + "重新选择"
//
// Layering trick (the key thing fixed in this revision): the Terminal
// is ALWAYS mounted from pane-mount time, sized to fill the pane via
// CSS absolute positioning. The picker / error states overlay it via
// the same absolute-positioned `.console-pane-empty` div on top. This
// way:
// - measure() returns accurate rows/cols at /shell/open time (the
//   xterm has had time to layout + fit), so the agent's PTY opens at
//   the right dimensions and bash's banner doesn't end up clumped at
//   the bottom of an over-sized buffer.
// - The Terminal's wsURL stays null until /shell/open succeeds; the
//   internal useEffect only opens a WS when wsURL changes from null
//   to a real value.
//
// Connection state overlay (Connecting / Disconnected / Error) is
// also a pure CSS overlay — exact pattern from webmux's Terminal.tsx.

import { useCallback, useEffect, useImperativeHandle, useRef, useState, forwardRef } from "react";
import { Terminal, type ConnectionState, type TerminalHandle } from "./Terminal";
import { HostSkillPicker, type PickerChoice } from "./HostSkillPicker";
import { kickHostShellSession, openHostShell } from "../../api/hosts.js";
import type { Host, HostSkill } from "../../types/hosts.js";

export interface PaneHandle {
  /** Fully terminate the agent-side PTY (best effort). */
  closeFully: () => Promise<void>;
  /** Current (hostId, hostSkillId, runId) for layout save / restore. */
  snapshot: () => { hostId?: string; hostSkillId?: number; runId?: number };
}

interface Props {
  paneId: string;
  hosts: Host[];
  skillsByHost: Map<string, HostSkill[]>;
  onSkillsResolved: (hostId: string, skills: HostSkill[]) => void;
  /** Pre-fill the picker / auto-start a session on mount. */
  autofill?: { hostId: string; hostSkillId: number };
  /** Parent informs us when × is clicked; we kick the PTY then ask
   *  parent to remove us from the panes[] array. */
  onCloseRequested: () => void;
}

type Kind =
  | { kind: "picker" }
  | { kind: "running"; hostId: string; hostSkillId: number; runId: number; wsURL: string }
  | { kind: "error"; hostId: string; hostSkillId: number; reason: string };

export const Pane = forwardRef<PaneHandle, Props>(function Pane(
  { paneId, hosts, skillsByHost, onSkillsResolved, autofill, onCloseRequested },
  ref,
) {
  const [state, setState] = useState<Kind>({ kind: "picker" });
  const [connState, setConnState] = useState<ConnectionState>("connecting");
  const termRef = useRef<TerminalHandle>(null);

  const start = useCallback(async (choice: PickerChoice) => {
    // Terminal is mounted from the start, so measure() returns the
    // real fitted dimensions (after the rAF inside Terminal has
    // settled). The agent's PTY opens at exactly the right size, no
    // bash-banner-clumped-at-bottom surprises later.
    const termSize = termRef.current?.measure() || { rows: 30, cols: 100 };
    const { response, data } = await openHostShell(choice.hostId, choice.hostSkillId, {
      rows: termSize.rows,
      cols: termSize.cols,
      shell: choice.shell,
    });
    if (!response.ok || !data.ws_url || typeof data.run_id !== "number") {
      setState({
        kind: "error",
        hostId: choice.hostId,
        hostSkillId: choice.hostSkillId,
        reason: data.error || `HTTP ${response.status}`,
      });
      return;
    }
    setState({
      kind: "running",
      hostId: choice.hostId,
      hostSkillId: choice.hostSkillId,
      runId: data.run_id,
      wsURL: data.ws_url,
    });
  }, []);

  // Autofill: pre-filled host_skill → call /shell/open on mount. Used
  // by restoreActiveSessions and saved-layout load. The Terminal is
  // already mounted from pane-mount, so wait one tick for it to fit.
  useEffect(() => {
    if (!autofill) return;
    const id = window.setTimeout(() => {
      void start({ hostId: autofill.hostId, hostSkillId: autofill.hostSkillId });
    }, 0);
    return () => window.clearTimeout(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // × handler — kick the PTY using local state directly. Don't go
  // through the parent's ref (the ref is for the parent's
  // closeAll / loadLayout flows, not the per-pane × click).
  const onCloseClick = useCallback(async () => {
    if (state.kind === "running") {
      try {
        await kickHostShellSession(state.hostId, state.runId);
      } catch {
        /* best effort — the supervisor's agent_disconnect path will
           reap eventually, OR the agent's idle timeout will */
      }
    }
    onCloseRequested();
  }, [state, onCloseRequested]);

  useImperativeHandle(
    ref,
    () => ({
      async closeFully() {
        if (state.kind === "running") {
          try {
            await kickHostShellSession(state.hostId, state.runId);
          } catch {
            /* best effort */
          }
        }
      },
      snapshot() {
        if (state.kind === "running") {
          return {
            hostId: state.hostId,
            hostSkillId: state.hostSkillId,
            runId: state.runId,
          };
        }
        return {};
      },
    }),
    [state],
  );

  const hostName = (id: string) => hosts.find((h) => h.id === id)?.name || id;
  let title = "未选择";
  if (state.kind === "running") {
    title = `${hostName(state.hostId)} · run=${state.runId}`;
  } else if (state.kind === "error") {
    title = `${hostName(state.hostId)} · 失败`;
  }

  const wsURL = state.kind === "running" ? state.wsURL : null;
  const showPicker = state.kind === "picker";
  const showError = state.kind === "error";
  const showConnOverlay = state.kind === "running" && connState !== "connected";

  return (
    <div className="console-pane" data-pane-id={paneId}>
      <div className="console-pane-head">
        <span className="console-pane-title">{title}</span>
        <button
          type="button"
          className="btn-inline"
          onClick={onCloseClick}
          title="关闭并终止 agent 端 PTY"
          style={{
            background: "#dc2626",
            color: "#fff",
            borderColor: "#dc2626",
            fontSize: 10,
            padding: "2px 8px",
            cursor: "pointer",
          }}
        >
          ×
        </button>
      </div>
      <div className="console-pane-body">
        {/* Always-mounted Terminal — sized by CSS absolute positioning.
            wsURL=null until /shell/open succeeds; the Terminal's WS
            useEffect skips the bridge until wsURL is real. */}
        <div className="console-pane-term">
          <Terminal
            ref={termRef}
            wsURL={wsURL}
            onStateChange={setConnState}
            onMeasured={() => {
              /* parent doesn't need it post-mount */
            }}
          />
        </div>
        {showPicker && (
          <div className="console-pane-empty">
            <HostSkillPicker
              hosts={hosts}
              skillsByHost={skillsByHost}
              onSkillsResolved={onSkillsResolved}
              onStart={start}
            />
          </div>
        )}
        {showError && state.kind === "error" && (
          <div className="console-pane-empty">
            <div style={{ color: "#fca5a5" }}>
              无法启动 host={state.hostId} skill={state.hostSkillId}
            </div>
            <div style={{ fontSize: 11, color: "#94a3b8" }}>{state.reason}</div>
            <button
              type="button"
              className="btn-inline btn-secondary"
              onClick={() => setState({ kind: "picker" })}
            >
              重新选择
            </button>
          </div>
        )}
        {showConnOverlay && (
          <div
            style={{
              position: "absolute",
              inset: 0,
              background:
                connState === "error"
                  ? "rgba(60,13,13,0.6)"
                  : connState === "disconnected"
                  ? "rgba(13,13,26,0.7)"
                  : "rgba(13,13,26,0.4)",
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
              color: "#aaa",
              fontSize: 13,
              pointerEvents: "none",
            }}
          >
            {connState === "connecting" && "Connecting..."}
            {connState === "disconnected" && "Disconnected"}
            {connState === "error" && "Connection error"}
          </div>
        )}
      </div>
    </div>
  );
});
