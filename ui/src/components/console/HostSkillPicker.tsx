// The picker UI that lives in an empty pane: host → skill → (optional
// shell-program) → 启动. Mirrors the vanilla renderPicker() but in
// React-controlled state. Returns the operator's choice to the parent,
// which then calls /shell/open + spawns the Terminal.

import { useEffect, useMemo, useState } from "react";
import { fetchHostSkillsForHost } from "../../api/hosts.js";
import type { Host, HostSkill } from "../../types/hosts.js";

export interface PickerChoice {
  hostId: string;
  hostSkillId: number;
  shell?: string;
}

interface Props {
  hosts: Host[];
  skillsByHost: Map<string, HostSkill[]>;
  onSkillsResolved: (hostId: string, skills: HostSkill[]) => void;
  onStart: (choice: PickerChoice) => Promise<void>;
}

export function HostSkillPicker({ hosts, skillsByHost, onSkillsResolved, onStart }: Props) {
  const [hostId, setHostId] = useState("");
  const [skillId, setSkillId] = useState("");
  const [shellPref, setShellPref] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const skills = hostId ? skillsByHost.get(hostId) : undefined;

  // Lazy-fetch skills when host changes.
  useEffect(() => {
    if (!hostId || skills) return;
    let cancelled = false;
    setLoading(true);
    setErr(null);
    (async () => {
      const { response, data } = await fetchHostSkillsForHost(hostId);
      if (cancelled) return;
      setLoading(false);
      if (!response.ok) {
        setErr(data.error || `加载 skills 失败 (HTTP ${response.status})`);
        return;
      }
      onSkillsResolved(hostId, data.skills || []);
    })();
    return () => {
      cancelled = true;
    };
  }, [hostId, skills, onSkillsResolved]);

  // Only shell skills are user-selectable. Nanokdp needs device_path
  // that the picker doesn't ask for; see PR #208.
  const usable = useMemo(() => (skills || []).filter((s) => s.kind === "shell"), [skills]);

  const selectedSkill = useMemo(
    () => usable.find((s) => String(s.id) === skillId),
    [usable, skillId],
  );

  const canStart = !!hostId && !!skillId && !busy;

  const submit = async () => {
    if (!canStart) return;
    setBusy(true);
    setErr(null);
    try {
      await onStart({
        hostId,
        hostSkillId: Number(skillId),
        shell: shellPref || undefined,
      });
    } catch (e) {
      setBusy(false);
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <>
      <div>选择主机 + skill 启动</div>
      <select
        className="input"
        style={{ maxWidth: 240 }}
        value={hostId}
        onChange={(e) => {
          setHostId(e.target.value);
          setSkillId("");
        }}
      >
        <option value="">— host —</option>
        {hosts.map((h) => (
          <option key={h.id} value={h.id}>
            {h.name} ({h.slug})
          </option>
        ))}
      </select>
      <select
        className="input"
        style={{ maxWidth: 240 }}
        value={skillId}
        onChange={(e) => setSkillId(e.target.value)}
        disabled={!hostId || loading}
      >
        <option value="">{loading ? "加载中..." : "— skill —"}</option>
        {usable.map((s) => (
          <option key={s.id} value={s.id}>
            {s.kind} (id={s.id})
          </option>
        ))}
      </select>
      {selectedSkill?.kind === "shell" && (
        <select
          className="input"
          style={{ maxWidth: 240 }}
          value={shellPref}
          onChange={(e) => setShellPref(e.target.value)}
        >
          <option value="">默认 shell (auto)</option>
          <option value="bash">bash</option>
          <option value="zsh">zsh</option>
          <option value="sh">sh</option>
        </select>
      )}
      <button
        type="button"
        onClick={submit}
        disabled={!canStart}
        style={{
          padding: "8px 24px",
          border: "none",
          borderRadius: 8,
          background: "#16a34a",
          color: "#fff",
          fontWeight: 600,
          fontSize: 13,
          cursor: canStart ? "pointer" : "not-allowed",
          opacity: canStart ? 1 : 0.5,
        }}
      >
        {busy ? "启动中..." : "启动"}
      </button>
      {err && <div style={{ color: "#fca5a5", fontSize: 11 }}>{err}</div>}
    </>
  );
}
