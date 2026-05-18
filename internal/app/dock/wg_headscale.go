package dock

// Phase 11 spike — Tailscale-client compat via vendored Headscale.
//
// D2 milestone: load a Headscale Config from a yaml file + run
// hscontrol.NewHeadscale(cfg). This is the smallest test that proves
// the dependency-injection surface is sane (constructor doesn't error
// on a clean config, init-time side effects don't conflict with
// Polar's other globals).
//
// We do NOT call hs.Serve() yet — that's D3. Serving binds a listener
// + starts background goroutines (mapBatcher, DERP STUN, SQLite/Postgres
// migrations). Keep the spike opt-in via env so the production .57
// deploy stays unchanged when this branch lands.
//
// Env contract:
//   WG_HEADSCALE_ENABLED=1                — opt in (default off, no-op)
//   WG_HEADSCALE_CONFIG_PATH=<path>       — yaml file (default /etc/headscale/config.yaml)
//
// See doc/wg-mac-tailscale-compat-design.md for the broader plan.

import (
	"log"
	"os"
	"strings"

	"github.com/juanfont/headscale/hscontrol"
	"github.com/juanfont/headscale/hscontrol/types"
)

// startHeadscaleProxy loads + constructs Headscale when the env gate
// is on. No serving yet. Caller logs the outcome.
func (s *Server) startHeadscaleProxy() {
	if strings.TrimSpace(os.Getenv("WG_HEADSCALE_ENABLED")) != "1" {
		// Default path: spike is opt-in. Most builds skip silently
		// so the vendored headscale code is dead weight in the
		// binary (~+27 MB, see go.mod) until a deploy explicitly
		// turns it on.
		return
	}
	cfgPath := strings.TrimSpace(os.Getenv("WG_HEADSCALE_CONFIG_PATH"))
	if cfgPath == "" {
		cfgPath = "/etc/headscale/config.yaml"
	}

	log.Printf("wg-mac phase 11 spike: loading headscale config from %s", cfgPath)
	if err := types.LoadConfig(cfgPath, true); err != nil {
		log.Printf("wg-mac phase 11 spike: LoadConfig(%s) failed: %v — set WG_HEADSCALE_ENABLED=0 to bypass", cfgPath, err)
		return
	}
	cfg, err := types.LoadServerConfig()
	if err != nil {
		log.Printf("wg-mac phase 11 spike: LoadServerConfig failed: %v", err)
		return
	}
	hs, err := hscontrol.NewHeadscale(cfg)
	if err != nil {
		log.Printf("wg-mac phase 11 spike: NewHeadscale failed: %v", err)
		return
	}
	log.Printf("wg-mac phase 11 spike D2: NewHeadscale OK (listen_addr=%s, server_url=%s); Serve() deferred to D3",
		cfg.Addr, cfg.ServerURL)
	// D3: launch `go hs.Serve()` here + handle shutdown ctx.
	_ = hs
}
