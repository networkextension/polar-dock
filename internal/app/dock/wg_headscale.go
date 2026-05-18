package dock

// Phase 11 — Tailscale-client compat via vendored Headscale.
//
// C1 milestone (this file): boot Headscale in-process alongside
// polar-dock when the env gate is on. Headscale takes its own
// listener (configured via cfg.Addr; example file at
// scripts/headscale.yaml.example uses :8081 to avoid Polar dock's
// :8080). nginx fronts both:
//
//   /v1/* /api/*  → polar dock :8080
//   /machine/*    → headscale :8081      (tailscale clients)
//   /derp/*       → headscale :8081
//
// Env contract:
//   WG_HEADSCALE_ENABLED=1                — opt in (default off, no-op)
//   WG_HEADSCALE_CONFIG_PATH=<path>       — yaml file (default /etc/headscale/config.yaml)
//
// Failure mode: any error during load/construct/serve is logged but
// does NOT take down polar-dock. Headscale is an opt-in addition; the
// wg-mac native /v1/* control plane is unaffected.
//
// See doc/wg-mac-tailscale-compat-design.md for the broader plan +
// doc/wg-mac-tailscale-spike-report.md for the spike findings.

import (
	"log"
	"os"
	"strings"

	"github.com/juanfont/headscale/hscontrol"
	"github.com/juanfont/headscale/hscontrol/types"
)

// startHeadscaleProxy loads the yaml config, constructs the embedded
// Headscale coordinator, and launches its HTTP/gRPC servers in a
// background goroutine. Idempotent + safe to call on every boot;
// when WG_HEADSCALE_ENABLED is unset, this is a no-op.
func (s *Server) startHeadscaleProxy() {
	if strings.TrimSpace(os.Getenv("WG_HEADSCALE_ENABLED")) != "1" {
		// Default path: opt-in. The vendored headscale code is dead
		// weight in the binary (~+27 MB; see doc/wg-mac-tailscale-spike-report.md)
		// until a deploy explicitly sets the env.
		return
	}
	cfgPath := strings.TrimSpace(os.Getenv("WG_HEADSCALE_CONFIG_PATH"))
	if cfgPath == "" {
		cfgPath = "/etc/headscale/config.yaml"
	}

	log.Printf("headscale: loading config from %s", cfgPath)
	if err := types.LoadConfig(cfgPath, true); err != nil {
		log.Printf("headscale: LoadConfig(%s) failed: %v — set WG_HEADSCALE_ENABLED=0 to bypass", cfgPath, err)
		return
	}
	cfg, err := types.LoadServerConfig()
	if err != nil {
		log.Printf("headscale: LoadServerConfig failed: %v", err)
		return
	}
	hs, err := hscontrol.NewHeadscale(cfg)
	if err != nil {
		log.Printf("headscale: NewHeadscale failed: %v", err)
		return
	}
	log.Printf("headscale: starting Serve in goroutine (listen_addr=%s, metrics=%s, grpc=%s, server_url=%s)",
		cfg.Addr, cfg.MetricsAddr, cfg.GRPCAddr, cfg.ServerURL)
	go func() {
		// Serve blocks until shutdown OR returns an error mid-flight.
		// On error, log + return — polar-dock keeps running (operator
		// flips WG_HEADSCALE_ENABLED=0 + restarts to recover).
		if err := hs.Serve(); err != nil {
			log.Printf("headscale: Serve exited: %v (polar-dock continues; restart with WG_HEADSCALE_ENABLED=0 to fully disable)", err)
		}
	}()
}
