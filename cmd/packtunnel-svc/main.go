// Command packtunnel-svc is the proxy/VPN profile plugin binary.
// Phase 2-W2 skeleton — DB pool + dock heartbeat + /healthz.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"

	"gin-auth-app/internal/plugins/packtunnel"
)

func main() {
	cfg := packtunnel.Config{
		DBDSN:        envOrDefault("POLAR_PACKTUNNEL_DB_DSN", "postgres://ideamesh:test123456@127.0.0.1:5432/polar_packtunnel?sslmode=disable"),
		DockBase:     envOrDefault("POLAR_DOCK_BASE", "http://127.0.0.1:8080"),
		PluginName:   envOrDefault("POLAR_PLUGIN_NAME", "packtunnel"),
		PluginToken:  os.Getenv("POLAR_PLUGIN_TOKEN"),
		Listen:       envOrDefault("POLAR_PACKTUNNEL_LISTEN", "127.0.0.1:8093"),
		BuildVersion: envOrDefault("POLAR_PACKTUNNEL_VERSION", "0.0.1"),
		BlobDir:      envOrDefault("POLAR_PACKTUNNEL_BLOB_DIR", "/Users/local/packtunnel-svc-data"),
		MetricsToken: os.Getenv("POLAR_PACKTUNNEL_METRICS_TOKEN"),
	}
	if strings.TrimSpace(cfg.PluginToken) == "" {
		log.Fatal("POLAR_PLUGIN_TOKEN unset — get plaintext from /admin-plugins.html")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	plugin, err := packtunnel.New(ctx, cfg)
	if err != nil {
		log.Fatalf("packtunnel.New: %v", err)
	}
	defer plugin.Close()

	gin.SetMode(envOrDefault("GIN_MODE", gin.ReleaseMode))
	r := gin.New()
	r.Use(gin.Recovery())
	plugin.RegisterRoutes(r)
	plugin.Start(ctx)

	srv := &http.Server{Addr: cfg.Listen, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("packtunnel-svc listening on %s (dock=%s, name=%s, ver=%s, blob=%s)",
			cfg.Listen, cfg.DockBase, cfg.PluginName, cfg.BuildVersion, cfg.BlobDir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("packtunnel-svc: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("packtunnel-svc: shutdown: %v", err)
	}
}

func envOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
