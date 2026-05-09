// polar-agent is the local-side CLI that pairs a developer's
// machine with a Polar bot user, then receives tool-call messages
// over WebSocket and executes them in a pinned working directory.
//
// Three commands:
//
//   polar-agent login   --server=https://polar.example.com --token=<raw>
//   polar-agent status
//   polar-agent attach  --bot=<bot_user_id> --workdir=.
//
// Config lives at ~/.polar/agent.toml after login. attach is the
// long-running mode; everything else is one-shot.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// fetchBotPreferredTool asks the dock what the bot's stored
// preferred_tool is. Used by `attach --tool=auto` so the operator
// running the docker image doesn't have to set POLAR_BOT_TOOLS
// per bot — the wizard set the bot's preferred_tool already.
//
// Returns "" + nil error when the bot has no preferred_tool set
// (legitimate signal: tool-loop mode). Real network/auth errors
// bubble up.
func fetchBotPreferredTool(cfg AgentConfig, botID string) (string, error) {
	url := strings.TrimRight(cfg.Server, "/") + "/api/agent/bots/" + botID + "/config"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("dock returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		PreferredTool string `json:"preferred_tool"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode bot config: %w", err)
	}
	return strings.TrimSpace(out.PreferredTool), nil
}

// autoRegisteredBot is one row returned by /api/agent/auto-register.
type autoRegisteredBot struct {
	BotUserID     string `json:"bot_user_id"`
	Name          string `json:"name"`
	PreferredTool string `json:"preferred_tool"`
}

// callAutoRegister POSTs to /api/agent/auto-register declaring
// this agent's capabilities (coder list + research flag). Server
// creates / reuses bot_users rows owned by the agent token's user.
// Returns the bot ids paired with their preferred_tool so the
// entrypoint can spawn one `polar-agent attach --tool=<coder>` per
// coder bot, or tool-loop attach (no --tool) for the research bot.
func callAutoRegister(cfg AgentConfig, coders []string, research bool, agentName string) ([]autoRegisteredBot, error) {
	body, _ := json.Marshal(map[string]any{
		"coders":     coders,
		"research":   research,
		"agent_name": agentName,
	})
	url := strings.TrimRight(cfg.Server, "/") + "/api/agent/auto-register"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("auto-register status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out struct {
		Bots []autoRegisteredBot `json:"bots"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, err
	}
	return out.Bots, nil
}

const (
	exitOK         = 0
	exitUsage      = 2
	exitConfig     = 3
	exitNet        = 4
	exitAuth       = 5
	exitToolFailed = 6
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(exitUsage)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "login":
		os.Exit(runLogin(args))
	case "status":
		os.Exit(runStatus(args))
	case "attach":
		os.Exit(runAttach(args))
	case "auto-register":
		os.Exit(runAutoRegister(args))
	case "submit-build":
		os.Exit(runSubmitBuild(args))
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(exitUsage)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `polar-agent — local executor for Polar bot users

Usage:
  polar-agent login   --server=<url> --token=<raw>     # write ~/.polar/agent.toml
  polar-agent status                                    # show config + last verify
  polar-agent attach  --bot=<bot_id> --workdir=<path>             # tool-call loop mode
  polar-agent attach  --bot=<bot_id> --workdir=<path> --tool=auto  # ask dock for bot's preferred_tool
  polar-agent attach  --bot=<bot_id> --workdir=<path> --tool=kimi  # passthrough to local kimi-cli
  polar-agent attach  --bot=<bot_id> --workdir=<path> --tool=claude
  polar-agent attach  --bot=<bot_id> --workdir=<path> --tool=codex
  polar-agent submit-build <ipa> [--project=<id>] [--sign-method=auto|zsign|codesign]
                                                  # iOS: sign + upload IPA to TestFlight

Tool whitelist (executed in workdir, default tool-call mode):
  read_file   — args: { path }
  write_file  — args: { path, content }
  list_dir    — args: { path }
  run_cmd     — args: { cmd, args[], stdin }   # cmd in {git, swift, xcodebuild, xcrun, ls, cat, find, grep}

--tool=<name> passthrough mode: chat content forwarded to the named local tool.
              Built-ins: kimi (kimi-cli), claude, codex. Override or add via
              ~/.polar/tools.json (template at cmd/polar-agent/tools.example.json).
              Platform LLM is bypassed; the local tool IS the agent.

--kimi (legacy alias for --tool=kimi).

Config file: ~/.polar/agent.toml
  server = "https://polar.example.com"
  token  = "polar_agent_..."`)
}

func runLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	server := fs.String("server", "", "Polar server base URL (e.g. https://polar.example.com)")
	tokenF := fs.String("token", "", "raw agent token from /api/agent/tokens")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *server == "" || *tokenF == "" {
		fmt.Fprintln(os.Stderr, "both --server and --token are required")
		return exitUsage
	}
	cfg := AgentConfig{Server: *server, Token: *tokenF}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to save config: %v\n", err)
		return exitConfig
	}
	if err := verifyToken(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: token verify failed (config saved anyway): %v\n", err)
		return exitAuth
	}
	fmt.Printf("logged in. config saved to %s\n", configPath())
	return exitOK
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, err := LoadAgentConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no config (run `polar-agent login` first): %v\n", err)
		return exitConfig
	}
	fmt.Printf("server : %s\n", cfg.Server)
	fmt.Printf("config : %s\n", configPath())
	if err := verifyToken(cfg); err != nil {
		fmt.Printf("status : invalid (%v)\n", err)
		return exitAuth
	}
	fmt.Println("status : ok")
	return exitOK
}

func runAttach(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	bot := fs.String("bot", "", "bot_user_id to attach to")
	workdir := fs.String("workdir", ".", "local working directory the bot may operate on")
	verbose := fs.Bool("verbose", false, "log each tool call")
	tool := fs.String("tool", "", "passthrough tool name (kimi/claude/codex/<custom>); empty = tool-call loop")
	kimi := fs.Bool("kimi", false, "legacy alias for --tool=kimi")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *bot == "" {
		fmt.Fprintln(os.Stderr, "--bot=<bot_user_id> is required")
		return exitUsage
	}
	if *kimi && *tool == "" {
		*tool = "kimi"
	}

	cfg, err := LoadAgentConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no config (run `polar-agent login` first): %v\n", err)
		return exitConfig
	}

	// --tool=auto: ask the dock what this bot's preferred_tool is.
	// Empty preferred_tool → fall back to tool-loop mode (no --tool).
	// Non-empty → use that as the passthrough tool name.
	if strings.EqualFold(strings.TrimSpace(*tool), "auto") {
		resolvedTool, ferr := fetchBotPreferredTool(cfg, *bot)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "warning: --tool=auto fetch failed (%v); falling back to tool-loop mode\n", ferr)
			*tool = ""
		} else {
			log.Printf("tool=auto resolved to %q for bot=%s", resolvedTool, *bot)
			*tool = resolvedTool
		}
	}

	resolved, err := resolveWorkdir(*workdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workdir invalid: %v\n", err)
		return exitConfig
	}

	// Resolve tool spec up front so we fail fast on bad name.
	var spec *toolSpec
	if *tool != "" {
		specs, _, lerr := loadToolsConfig()
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "tools config invalid: %v\n", lerr)
			return exitConfig
		}
		s, ferr := findToolSpec(specs, *tool)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", ferr)
			return exitConfig
		}
		spec = s
	}

	log.SetFlags(log.Ldate | log.Ltime)
	mode := "tool-loop"
	if spec != nil {
		mode = "passthrough(" + spec.Name + ")"
	}
	log.Printf("attach bot=%s workdir=%s server=%s mode=%s", *bot, resolved, cfg.Server, mode)
	if err := runAgentLoop(cfg, *bot, resolved, *verbose, spec); err != nil {
		log.Printf("agent loop ended: %v", err)
		return exitNet
	}
	return exitOK
}

// runAutoRegister implements `polar-agent auto-register`. Output
// format: one line per bot, "bot_id<TAB>preferred_tool", with empty
// preferred_tool meaning the row is a research bot (tool-loop mode,
// no --tool= when attaching).
//
// Flags:
//
//	--coders=kimi,claude,codex  comma-separated coder names (default all 3)
//	--research                  also register a research bot (default on)
//	--no-research               skip research bot
//	--agent-name=<str>          display name (default = hostname)
func runAutoRegister(args []string) int {
	fs := flag.NewFlagSet("auto-register", flag.ContinueOnError)
	coders := fs.String("coders", "kimi,claude,codex", "comma-separated coder names")
	research := fs.Bool("research", true, "also register a research bot (uses workspace default LLM + agent tools)")
	noResearch := fs.Bool("no-research", false, "skip research bot (overrides --research)")
	agentName := fs.String("agent-name", "", "display name (optional; from container hostname)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *noResearch {
		*research = false
	}
	cfg, err := LoadAgentConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no config (run `polar-agent login` first): %v\n", err)
		return exitConfig
	}
	list := []string{}
	for _, c := range strings.Split(*coders, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		list = append(list, c)
	}
	if len(list) == 0 && !*research {
		fmt.Fprintln(os.Stderr, "nothing to register: no coders + research disabled")
		return exitUsage
	}
	if *agentName == "" {
		if h, err := os.Hostname(); err == nil {
			*agentName = h
		}
	}
	bots, err := callAutoRegister(cfg, list, *research, *agentName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auto-register failed: %v\n", err)
		return exitNet
	}
	for _, b := range bots {
		fmt.Printf("%s\t%s\n", b.BotUserID, b.PreferredTool)
	}
	return exitOK
}
