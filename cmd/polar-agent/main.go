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
	"flag"
	"fmt"
	"log"
	"os"
)

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
  polar-agent attach  --bot=<bot_id> --workdir=<path> --tool=kimi  # passthrough to local kimi-cli
  polar-agent attach  --bot=<bot_id> --workdir=<path> --tool=claude
  polar-agent attach  --bot=<bot_id> --workdir=<path> --tool=codex

Tool whitelist (executed in workdir, default tool-call mode):
  read_file   — args: { path }
  write_file  — args: { path, content }
  list_dir    — args: { path }
  run_cmd     — args: { cmd, args[], stdin }   # cmd in {git, swift, xcodebuild, xcrun, ls, cat, find, grep}

--tool=<name> passthrough mode: chat content forwarded to the named local tool.
              Built-ins: kimi (kimi-cli), claude, codex. Override or add via
              ~/.polar/tools.json (see toolSpec / builtinTools in tools.go).
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
