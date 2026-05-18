// cmd/headscale-spike — Phase 11 spike D3 helper.
//
// Standalone binary that loads a Headscale yaml config + runs
// hscontrol.NewHeadscale(cfg).Serve(). No Polar dock boot, no
// Postgres/Redis requirement (headscale can use SQLite via its own
// config). Lets us validate the embedded-headscale path end-to-end
// without disturbing the live polar-dock on .57.
//
// Usage:
//   go build -o /tmp/hs-spike ./cmd/headscale-spike
//   /tmp/hs-spike /path/to/headscale-config.yaml
//
// Then in another shell:
//   tailscale up --login-server=http://localhost:8080 --authkey=<key>
//
// The key is minted via headscale CLI (see spike report doc/wg-mac-tailscale-spike-report.md
// for the exact sequence).
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/juanfont/headscale/hscontrol"
	"github.com/juanfont/headscale/hscontrol/types"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hs-spike <config.yaml>")
		os.Exit(2)
	}
	cfgPath := os.Args[1]

	log.Printf("hs-spike: LoadConfig(%s)", cfgPath)
	if err := types.LoadConfig(cfgPath, true); err != nil {
		log.Fatalf("LoadConfig: %v", err)
	}
	cfg, err := types.LoadServerConfig()
	if err != nil {
		log.Fatalf("LoadServerConfig: %v", err)
	}
	log.Printf("hs-spike: NewHeadscale (listen_addr=%s, server_url=%s, db=%s)",
		cfg.Addr, cfg.ServerURL, cfg.Database.Type)
	hs, err := hscontrol.NewHeadscale(cfg)
	if err != nil {
		log.Fatalf("NewHeadscale: %v", err)
	}
	log.Printf("hs-spike: Serve() (use Ctrl-C to stop)")
	if err := hs.Serve(); err != nil {
		log.Fatalf("Serve: %v", err)
	}
}
