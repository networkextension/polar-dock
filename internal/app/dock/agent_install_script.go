package dock

// One-shot install script renderer. Output is a POSIX sh script
// returned in the agent-token-create response payload (the wizard
// shows it to the user once, copy-or-download). Not exposed via a
// fetchable endpoint — token id leaks would otherwise hand out raw
// tokens (per user's safety preference at create-wizard design time).
//
// Script behavior (per user's failure-mode pick):
//
//   - Detects host OS + arch via uname -s / -m, maps to release
//     tarball naming (darwin/arm64 vs linux/amd64 etc.)
//   - Downloads polar-agent binary from the running dock's release
//     mirror, drops it in ~/.local/bin, makes executable
//   - Runs `polar-agent login --server --token` (token is baked in)
//   - For each enabled coder: install via the natural channel
//     (uv tool install / npm i -g / brew tap), echo a follow-up
//     reminder for OAuth-mode coders. Continues on per-coder
//     failure; final summary line lists "✅" / "⚠️" per coder.
//   - Drops a starter ~/.polar/start_agents.env + tools.json so the
//     operator only has to fill in bot IDs.
//
// Tokens are baked into the script literal — the operator should
// keep this file private (chmod 600 + delete after first run).

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

type installScriptInput struct {
	ServerURL string
	Token     string
	Coders    AgentCoderConfig
	AgentName string
}

const installScriptTemplate = `#!/bin/sh
# polar-agent installer. Generated for: {{.AgentName}}
# Server: {{.ServerURL}}
# Re-running is safe — every step is idempotent. Continues on
# per-coder failure; final summary lists what worked + what didn't.

set -u
SERVER='{{.ServerURL}}'
TOKEN='{{.Token}}'
INSTALL_PREFIX="$HOME/.local/bin"
mkdir -p "$INSTALL_PREFIX"
mkdir -p "$HOME/.polar"
case ":$PATH:" in
  *":$INSTALL_PREFIX:"*) ;;
  *) echo "ℹ️  $INSTALL_PREFIX is not in PATH — add it to your shell rc";;
esac

# ---- platform detection ----
OS_RAW=$(uname -s)
ARCH_RAW=$(uname -m)
case "$OS_RAW" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) echo "❌ unsupported OS: $OS_RAW (need Darwin or Linux)"; exit 1 ;;
esac
case "$ARCH_RAW" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "❌ unsupported arch: $ARCH_RAW"; exit 1 ;;
esac
echo "ℹ️  detected platform: $OS/$ARCH"

# ---- summary buffer ----
SUMMARY=""
record_ok()  { SUMMARY="$SUMMARY
✅ $1"; }
record_err() { SUMMARY="$SUMMARY
⚠️ $1"; }

# ---- polar-agent binary ----
echo "→ downloading polar-agent binary..."
ASSET_URL="$SERVER/releases/latest/polar-agent_${OS}_${ARCH}"
if curl -fsSL "$ASSET_URL" -o "$INSTALL_PREFIX/polar-agent.new"; then
  mv "$INSTALL_PREFIX/polar-agent.new" "$INSTALL_PREFIX/polar-agent"
  chmod +x "$INSTALL_PREFIX/polar-agent"
  record_ok "polar-agent binary installed at $INSTALL_PREFIX/polar-agent"
else
  rm -f "$INSTALL_PREFIX/polar-agent.new"
  record_err "polar-agent binary download failed from $ASSET_URL — check release mirror"
fi

# ---- login (writes ~/.polar/agent.toml) ----
if [ -x "$INSTALL_PREFIX/polar-agent" ]; then
  if "$INSTALL_PREFIX/polar-agent" login --server="$SERVER" --token="$TOKEN" >/dev/null 2>&1; then
    record_ok "polar-agent login → ~/.polar/agent.toml"
  else
    record_err "polar-agent login failed — verify dock URL is reachable + token unrevoked"
  fi
fi

{{if .Coders.Kimi.Enabled}}
# ---- kimi-cli ----
echo "→ installing kimi-cli (uv tool install)..."
if command -v uv >/dev/null 2>&1; then
  if uv tool install kimi-cli; then
    record_ok "kimi-cli installed"
{{if eq .Coders.Kimi.AuthMode "api_key"}}
    record_ok "kimi auth = api_key — set KIMI_API_KEY in your shell rc before attaching"
{{else}}
    record_ok "kimi auth = login — run 'kimi login' once before attaching"
{{end}}
  else
    record_err "kimi-cli install failed (uv tool install kimi-cli) — check uv/network"
  fi
else
  record_err "kimi-cli skipped: uv not on PATH (install: curl -LsSf https://astral.sh/uv/install.sh | sh)"
fi
{{end}}

{{if .Coders.Claude.Enabled}}
# ---- claude (claude-code CLI) ----
echo "→ installing claude-code (npm)..."
if command -v npm >/dev/null 2>&1; then
  if npm install -g @anthropic-ai/claude-code; then
    record_ok "claude-code installed"
{{if eq .Coders.Claude.AuthMode "api_key"}}
    record_ok "claude auth = api_key — set ANTHROPIC_API_KEY in your shell rc before attaching"
{{else}}
    record_ok "claude auth = login — run 'claude' once and complete the browser OAuth"
{{end}}
  else
    record_err "claude-code install failed (npm install -g @anthropic-ai/claude-code) — check npm/network"
  fi
else
  record_err "claude-code skipped: npm not on PATH (install Node.js first)"
fi
{{end}}

{{if .Coders.Codex.Enabled}}
# ---- codex (OpenAI codex CLI) ----
echo "→ installing codex (npm)..."
if command -v npm >/dev/null 2>&1; then
  if npm install -g @openai/codex-cli; then
    record_ok "codex installed"
{{if eq .Coders.Codex.AuthMode "api_key"}}
    record_ok "codex auth = api_key — set OPENAI_API_KEY in your shell rc before attaching"
{{else}}
    record_ok "codex auth = login — run 'codex login' once before attaching"
{{end}}
  else
    record_err "codex install failed (npm install -g @openai/codex-cli) — check npm/network"
  fi
else
  record_err "codex skipped: npm not on PATH (install Node.js first)"
fi
{{end}}

# ---- starter env files ----
if [ ! -f "$HOME/.polar/start_agents.env" ]; then
  cat > "$HOME/.polar/start_agents.env" << ENVEOF
# polar-agent attach env. Fill in POLAR_BOT_IDS with your bot user
# ids (comma-separated) then run: polar-agent attach (per bot) or
# scripts/start_agents.sh from the Polar repo.
POLAR_DOCK_URL=$SERVER
POLAR_AGENT_TOKEN=$TOKEN
POLAR_BOT_IDS=
ENVEOF
  record_ok "starter ~/.polar/start_agents.env written"
fi

# ---- summary ----
echo ""
echo "═══════════════════════════════════════"
echo "  Install summary for {{.AgentName}}"
echo "═══════════════════════════════════════"
printf '%s\n' "$SUMMARY"
echo ""
echo "Next steps:"
echo "  1. Add bot user ids to ~/.polar/start_agents.env"
echo "  2. (For OAuth coders) run their login commands"
echo "  3. Start: $INSTALL_PREFIX/polar-agent attach --bot=<bot_id> --workdir=<path> --tool=<kimi|claude|codex>"
`

func renderAgentInstallScript(in installScriptInput) (string, error) {
	tpl, err := template.New("install").Parse(installScriptTemplate)
	if err != nil {
		return "", fmt.Errorf("parse install template: %w", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, in); err != nil {
		return "", fmt.Errorf("execute install template: %w", err)
	}
	out := strings.TrimRight(buf.String(), "\n") + "\n"
	return out, nil
}
