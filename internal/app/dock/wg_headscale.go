package dock

// Phase 11 spike — Tailscale-client compat via vendored Headscale.
// This file is the integration seam between Polar dock and the
// upstream Headscale Coordinator server. Currently a vendor-import
// stub: confirms that `github.com/juanfont/headscale` compiles into
// our binary on Go 1.25.5 + CGO_ENABLED=0.
//
// Subsequent days of the spike (D2-D4 per the plan) will wire:
//   D2: hscontrol.NewHeadscale(cfg) construction + gin route mount
//   D3: smoke `tailscale up --login-server=http://localhost:8080`
//   D4: spike report → C1-C7 timeline confirmation OR pivot to sidecar
//
// See doc/wg-mac-tailscale-compat-design.md for the broader plan.

import (
	"context"
	"log"

	// Blank import to force module resolution + verify compile linkage.
	// Once D2 lands a real constructor wiring this becomes a named
	// import.
	_ "github.com/juanfont/headscale/hscontrol"
)

// startHeadscaleProxy will (in D2) construct a Headscale coordinator
// using Polar's Postgres pool and mount its HTTP handlers on
// /machine/* + /derp/* under our gin router. For now it just logs
// the spike status so we can confirm the binary started up cleanly.
func (s *Server) startHeadscaleProxy(_ context.Context) {
	log.Printf("wg-mac phase 11 spike: headscale vendored (Coordinator mount: not yet wired — see wg_headscale.go D2)")
}
