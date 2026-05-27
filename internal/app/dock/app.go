package dock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"
	geoip2 "github.com/oschwald/geoip2-golang/v2"
	"github.com/redis/go-redis/v9"
)

type Server struct {
	db                  *sql.DB
	redis               *redis.Client
	router              *gin.Engine
	addr                string
	redisPrefix         string
	markdownDir         string
	uploadDir           string
	geoLiteDBPath       string
	geoIPReader         *geoip2.Reader
	webAuthn            *webauthn.WebAuthn
	passkeyAuto         bool
	passkeyRPID         string
	passkeyOrigin       string
	passkeyRPName       string
	passkeySessions     map[string]passkeySession
	passkeyMu           sync.Mutex
	wsHub               *wsHub
	agentHub            *agentHub
	workDir             string
	aiAgent             *aiAgent
	promptCache         *promptCache
	chatStorage         AttachmentStorage
	backgroundCtx       context.Context
	backgroundStop      context.CancelFunc
	applePushTopic      string
	applePushTopicDev   string
	applePushTopicProd  string
	applePushKeyID      string
	applePushKeyIDDev   string
	applePushKeyIDProd  string
	applePushTeamID     string
	applePushTeamIDDev  string
	applePushTeamIDProd string
	apnsMu              sync.Mutex
	apnsClients         map[string]*http.Client
	apnsTokens          map[string]cachedAPNSToken
	publicBaseURL       string
	mailer              MailSender
	// Video studio fields removed — video extracted to
	// github.com/networkextension/polar-video.

	// Platform-default LLM (Kimi). Seeded at startup if KIMI_API_KEY
	// is set; users / workspaces can pick this row in the bot
	// picker without touching their own LLM credentials.
	kimiBaseURL string
	kimiModel   string
	kimiAPIKey  string

	// iosdistResourceKey is the optional 32-byte AES-GCM key used to
	// encrypt sensitive iOS distribution credentials (today: .p12
	// passwords). nil means encryption is disabled and we'll store
	// cleartext with a flag so the UI can warn.
	iosdistResourceKey []byte

	// polarCredentialKey is the host_skill_credentials vault key
	// (P1c.4). Same AES-GCM shape as iosdistResourceKey, separate key
	// so the security domains are isolated.
	polarCredentialKey []byte

	// polarAllowLocalBootstrap (P1e): when true, POST
	// /api/hosts/local-bootstrap accepts no-auth requests from the
	// loopback interface and provisions a host + agent_token. Disabled
	// by default for safety.
	polarAllowLocalBootstrap bool

	// Host module Shell (P2). Kill-switch + concurrency registry.
	polarDisableShellSkill bool
	shellSessions          *shellSessionRegistry

	// Host module VNC. Kill-switch + concurrency registry. Same shape
	// as the Shell skill above; separate registry since per-host caps
	// + bridge semantics differ (VNC has no backscroll replay, lower
	// per-host cap, evicts bridge on backpressure rather than dropping
	// frames mid-stream).
	polarDisableVNCSkill bool
	vncSessions          *vncSessionRegistry

	// iosdistZsignPath is the resolved path to the zsign binary, set
	// once at server boot via exec.LookPath. When empty, the sign
	// endpoint returns a 503 with a helpful "install zsign" message
	// instead of crashing the worker.
	iosdistZsignPath  string
	iosdistSignQueue  chan int64

	// Observability. metricsRegistry is always non-nil (Phase 1 metrics
	// register unconditionally so the middleware can record); the
	// /metrics endpoint itself is gated by metricsToken being set.
	metricsRegistry *metrics
	metricsToken    string

	// cookieDomain controls the Domain attribute on the auth Set-Cookie
	// headers. Empty (default) → browser scopes the cookie to the
	// request host (zen.4950.store). Set to `.4950.store` (leading dot)
	// to enable SSO across plugin subdomains (project.4950.store,
	// expense.4950.store, …). See setAuthCookies in middleware.go.
	cookieDomain string

	// polarHostsTouchURL — post-H9 forward target for skill.advertise.
	// See polar_hosts_touch.go.
	polarHostsTouchURL string

	// Plugin cutover flags removed — all plugin REST surfaces are now
	// remote. The H-series cleanups dropped the per-plugin gated route
	// blocks; see open-platform.md for history.

	// LLM proxy in-memory rate limiter. Per-token sliding window;
	// see llm_proxy_rate_limit.go.
	llmProxyRateLimiter *llmProxyRateLimiter

	// LLM upstream health prober — synthetic-call goroutine that
	// updates polar_llm_upstream_up gauges every 30s.
	llmProxyProber *llmProxyProber

	// Phase 11 C3: when embedded Headscale is enabled
	// (WG_HEADSCALE_ENABLED=1), this stores the unix-socket path
	// from the loaded cfg so wg_headscale_client.go can dial it
	// to mint matching PreAuthKeys on token create.
	headscaleSocketPath string

	// Per-token monthly cost tracker (P4). 60s in-memory cache of
	// SUM(cost_usd) for the current UTC month.
	llmProxyQuota *llmProxyQuotaTracker

	// Issue #344: per-(host_id, iface) snapshot of the most recent
	// wg_peer_status sample reported by polar-agent's wireguard skill.
	// Read by /internal/v1/wg-peer-status (polar-wg) and the host
	// detail SSE in dock-ui.
	wgPeerCache *wgPeerCache
}

func NewServer(cfg Config) (*Server, error) {
	db, err := openDB(cfg.PostgresDSN)
	if err != nil {
		return nil, err
	}

	server := &Server{
		db:                  db,
		addr:                cfg.Addr,
		redisPrefix:         cfg.RedisPrefix,
		markdownDir:         cfg.MarkdownDir,
		uploadDir:           cfg.UploadDir,
		geoLiteDBPath:       cfg.GeoLiteDBPath,
		applePushTopic:      cfg.ApplePushTopic,
		applePushTopicDev:   cfg.ApplePushTopicDev,
		applePushTopicProd:  cfg.ApplePushTopicProd,
		applePushKeyID:      cfg.ApplePushKeyID,
		applePushKeyIDDev:   cfg.ApplePushKeyIDDev,
		applePushKeyIDProd:  cfg.ApplePushKeyIDProd,
		applePushTeamID:     cfg.ApplePushTeamID,
		applePushTeamIDDev:  cfg.ApplePushTeamIDDev,
		applePushTeamIDProd: cfg.ApplePushTeamIDProd,
		publicBaseURL:       strings.TrimRight(cfg.PublicBaseURL, "/"),
		mailer:              newSMTPMailer(cfg),
		apnsClients:         make(map[string]*http.Client),
		apnsTokens:          make(map[string]cachedAPNSToken),
		redis: redis.NewClient(&redis.Options{
			Addr:     cfg.RedisAddr,
			Password: cfg.RedisPassword,
			DB:       cfg.RedisDB,
		}),
	}
	server.backgroundCtx, server.backgroundStop = context.WithCancel(context.Background())

	if cfg.IOSDistResourceKey != "" {
		key, err := decodeIOSDistResourceKey(cfg.IOSDistResourceKey)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("invalid IOSDIST_RESOURCE_KEY: %w", err)
		}
		server.iosdistResourceKey = key
		log.Printf("iosdist: resource encryption enabled (32-byte AES-GCM key)")
	} else {
		log.Printf("iosdist: IOSDIST_RESOURCE_KEY not set — .p12 passwords will be stored in plaintext")
	}

	if cfg.PolarCredentialKey != "" {
		key, err := decodeIOSDistResourceKey(cfg.PolarCredentialKey)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("invalid POLAR_CREDENTIAL_KEY: %w", err)
		}
		server.polarCredentialKey = key
		log.Printf("coder credentials: vault encryption enabled (32-byte AES-GCM key)")
	} else {
		log.Printf("coder credentials: POLAR_CREDENTIAL_KEY not set — host_skill_credentials stored in plaintext")
	}

	server.polarAllowLocalBootstrap = cfg.PolarAllowLocalBootstrap
	if cfg.PolarAllowLocalBootstrap {
		log.Printf("host module: local-bootstrap endpoint ENABLED (loopback-only, no auth)")
	}

	// P2 Shell: kill-switch + concurrency cap (per-host hard-coded 4;
	// per-dock from env, default 32).
	server.polarDisableShellSkill = cfg.PolarDisableShellSkill
	globalShellMax := cfg.PolarMaxShellSessions
	if globalShellMax <= 0 {
		globalShellMax = 32
	}
	server.shellSessions = newShellSessionRegistry(globalShellMax)
	if cfg.PolarDisableShellSkill {
		log.Printf("host module: Shell skill DISABLED (POLAR_DISABLE_SHELL_SKILL=true)")
	} else {
		log.Printf("host module: Shell skill enabled (per-host cap=4, global cap=%d)", globalShellMax)
	}

	// VNC skill registry. Per-host cap is hard-coded at 2 (vs shell's
	// 4) — VNC pushes full framebuffer streams, two sessions is plenty.
	server.polarDisableVNCSkill = cfg.PolarDisableVNCSkill
	globalVNCMax := cfg.PolarMaxVNCSessions
	if globalVNCMax <= 0 {
		globalVNCMax = 16
	}
	server.vncSessions = newVNCSessionRegistry(globalVNCMax)
	if cfg.PolarDisableVNCSkill {
		log.Printf("host module: VNC skill DISABLED (POLAR_DISABLE_VNC_SKILL=true)")
	} else {
		log.Printf("host module: VNC skill enabled (per-host cap=2, global cap=%d)", globalVNCMax)
	}

	workDir, err := os.Getwd()
	if err == nil {
		server.workDir = workDir
	}

	chatStorage, err := newAttachmentStorage(cfg.UploadDir, cfg)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init chat storage: %w", err)
	}
	server.chatStorage = chatStorage
	if chatStorage.IsRemote() {
		log.Printf("chat attachment storage: Cloudflare R2 bucket=%s", cfg.CloudflareR2Bucket)
	} else {
		log.Printf("chat attachment storage: local filesystem dir=%s", cfg.UploadDir)
	}

	if err := server.redis.Ping(context.Background()).Err(); err != nil {
		_ = db.Close()
		return nil, err
	}

	if cfg.GeoLiteDBPath != "" {
		reader, err := geoip2.Open(cfg.GeoLiteDBPath)
		if err != nil {
			log.Printf("geolite disabled: %v", err)
		} else {
			server.geoIPReader = reader
		}
	}

	server.passkeyRPID = cfg.PasskeyRPID
	server.passkeyOrigin = cfg.PasskeyOrigin
	server.passkeyRPName = cfg.PasskeyRPName
	server.passkeyAuto = cfg.PasskeyOrigin == DefaultPasskeyOrigin && cfg.PasskeyRPID == DefaultPasskeyRPID

	rpID := cfg.PasskeyRPID
	if rpID == DefaultPasskeyRPID {
		if parsed, err := url.Parse(cfg.PasskeyOrigin); err == nil && parsed.Host != "" {
			host := parsed.Host
			if strings.Contains(host, ":") {
				if splitHost, _, err := net.SplitHostPort(host); err == nil && splitHost != "" {
					host = splitHost
				}
			}
			if host != "" {
				rpID = host
			}
		}
	}

	webAuthn, err := webauthn.New(&webauthn.Config{
		RPDisplayName: cfg.PasskeyRPName,
		RPID:          rpID,
		RPOrigins:     []string{cfg.PasskeyOrigin},
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	server.webAuthn = webAuthn
	server.passkeySessions = make(map[string]passkeySession)
	server.wsHub = newWSHub()
	server.agentHub = newAgentHub()
	server.wgPeerCache = newWGPeerCache()
	server.wsHub.onPresenceChanged = server.handlePresenceChange
	server.wsHub.onThreadViewChanged = server.handleThreadViewChange
	server.wsHub.onConnectionTouched = server.handleConnectionTouch
	go server.wsHub.run()
	go server.runChatEventSubscriber(server.backgroundCtx)
	go server.runPushDeliveryWorker(server.backgroundCtx)
	// Qunliao P3 — autonomous bot idle loop. Driver ticks every 30s,
	// fans per-(room, bot) decisions out into ephemeral goroutines.
	// Default-off per row, billing-gated, hourly capped — see
	// chat_rooms_idle_loop.go.
	go server.runRoomAgentIdleLoop(server.backgroundCtx)

	if err := server.ensureSystemUser(); err != nil {
		_ = db.Close()
		return nil, err
	}

	// Every user must have a personal team. Cheap and idempotent: scans
	// users without one and creates them. Failure is logged but not fatal
	// — the server can still come up; missing personal teams will get
	// created on demand by handlers that touch teams.
	if err := server.backfillPersonalTeams(); err != nil {
		log.Printf("teams: personal team backfill failed: %v", err)
	}

	// T6-A: populate workspace_id on legacy resource rows from each
	// owner's personal team. Idempotent. Must run after the personal
	// team backfill above so the lookup never misses.
	if err := server.backfillResourceWorkspaceIDs(); err != nil {
		log.Printf("teams: resource workspace_id backfill failed: %v", err)
	}

	// attachments-refactor: migrate legacy chat_messages.attachment
	// JSON column into attachments + chat_message_attachments, then
	// DROP COLUMN. Idempotent — no-op on fresh / already-migrated
	// DBs (probes information_schema first). Must run BEFORE the
	// NOT NULL enforcement below because DROP COLUMN runs against a
	// table that the enforcement step also touches.
	if err := server.migrateChatMessageAttachments(); err != nil {
		log.Printf("attachments: chat message migration failed: %v", err)
	}

	// T6-D enforcement: lock workspace_id NOT NULL only AFTER the
	// backfill above has had a chance to fill in legacy NULLs. This
	// is the order that Postgres needs but the embedded schema
	// string in openDB can't express (schema apply runs before any
	// Go-side backfill).
	if err := server.enforceWorkspaceIDNotNull(); err != nil {
		log.Printf("teams: workspace_id NOT NULL enforcement failed: %v", err)
	}

	// Mark any pending invitations whose TTL has elapsed as expired.
	// Idempotent and quick; lazy expiration also happens at access time.
	if err := server.expireOldInvitations(); err != nil {
		log.Printf("teams: invitation expiry sweep failed: %v", err)
	}

	// Backfill public slugs for legacy iosdist apps. Cheap, idempotent.
	if err := server.backfillIOSAppSlugs(); err != nil {
		log.Printf("iosdist: slug backfill failed: %v", err)
	}

	// zsign discovery — if the binary is on PATH, the sign worker can run.
	// Otherwise the sign endpoint will refuse with a clear 503.
	if zsignPath, lookErr := exec.LookPath("zsign"); lookErr == nil {
		server.iosdistZsignPath = zsignPath
		log.Printf("[iosdist sign] zsign found at %s — sign worker enabled", zsignPath)
	} else {
		log.Printf("[iosdist sign] zsign NOT on PATH — sign endpoint will return 503 until installed")
	}

	// Sign worker queue + recovery: re-enqueue pending tasks, fail any
	// stuck "running" task left over from a crash.
	server.iosdistSignQueue = make(chan int64, 64)
	if recoverIDs, err := server.recoverIOSSignTasks(); err != nil {
		log.Printf("[iosdist sign] recovery failed: %v", err)
	} else if len(recoverIDs) > 0 {
		log.Printf("[iosdist sign] re-enqueueing %d pending task(s) after restart", len(recoverIDs))
		go func(ids []int64) {
			for _, id := range ids {
				select {
				case server.iosdistSignQueue <- id:
				case <-server.backgroundCtx.Done():
					return
				}
			}
		}(recoverIDs)
	}
	// iosdist sign worker moved to github.com/networkextension/polar-iosdist.

	// Bootstrap prompt template registry — inserts any newly-added
	// slugs and refreshes the default_content column on every start.
	// Existing user edits in `content` are preserved; only the
	// "default" snapshot is updated. Failure is logged but not fatal
	// — handlers fall back to the code-baked default at render time.
	if err := server.bootstrapPromptTemplates(promptTemplateDefaults()); err != nil {
		log.Printf("prompts: bootstrap failed: %v", err)
	}

	server.aiAgent = newAIAgent(server, cfg)
	if server.aiAgent != nil {
		go server.aiAgent.run()
	}

	// Video studio + Seedance system-config moved to
	// github.com/networkextension/polar-video.
	server.kimiBaseURL = cfg.KimiBaseURL
	server.kimiModel = cfg.KimiModel
	server.kimiAPIKey = cfg.KimiAPIKey
	if err := server.ensureKimiPlatformConfig(); err != nil {
		log.Printf("seed kimi platform config failed: %v", err)
	}

	server.metricsRegistry = newMetrics()
	server.llmProxyRateLimiter = newLLMProxyRateLimiter()
	server.metricsToken = strings.TrimSpace(cfg.MetricsToken)
	server.cookieDomain = strings.TrimSpace(cfg.CookieDomain)
	if server.cookieDomain != "" {
		log.Printf("auth: cookies scoped to Domain=%s (cross-subdomain SSO enabled)", server.cookieDomain)
	}
	server.polarHostsTouchURL = strings.TrimSpace(cfg.PolarHostsTouchURL)
	if server.polarHostsTouchURL != "" {
		log.Printf("hosts: skill.advertise forwarding to %s", server.polarHostsTouchURL)
	}
	server.startMetricsSampler()
	server.llmProxyProber = newLLMProxyProber(server)
	server.llmProxyProber.start()
	server.llmProxyQuota = newLLMProxyQuotaTracker(server)
	// wg + iosdist background workers moved to their plugin repos
	// (github.com/networkextension/polar-{wg,iosdist}).

	server.router = gin.Default()
	// Increase max upload size for video files (default gin limit is 32 MiB)
	server.router.MaxMultipartMemory = 512 << 20 // 512 MiB
	server.router.Use(corsMiddleware())
	server.router.Use(server.LanguageMiddleware())
	server.router.Use(server.metricsRegistry.httpMiddleware())
	if server.uploadDir != "" {
		server.router.Static("/uploads", server.uploadDir)
	}
	server.registerRoutes()

	return server, nil
}

func (s *Server) Run() error {
	return s.router.Run(s.addr)
}

func (s *Server) Close() error {
	if s.backgroundStop != nil {
		s.backgroundStop()
	}
	if s.aiAgent != nil {
		s.aiAgent.stop()
	}
	if s.geoIPReader != nil {
		_ = s.geoIPReader.Close()
	}
	if s.redis != nil {
		_ = s.redis.Close()
	}
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// ensureSeedanceSystemConfig seeds an LLMConfig row with provider_kind =
// ensureSeedanceSystemConfig moved to github.com/networkextension/polar-video
// (polar-video now seeds its own Seedance llm_configs row at first boot).

// ensureKimiPlatformConfig seeds the platform-default Kimi LLM
// config. Once set, every workspace's bot picker / agent surfaces
// this row tagged "(平台默认)" without anyone supplying credentials.
// Idempotent: existing platform-kind row is updated in place so an
// operator key rotation just needs a restart with the new env var.
func (s *Server) ensureKimiPlatformConfig() error {
	if s == nil {
		return nil
	}
	apiKey := strings.TrimSpace(s.kimiAPIKey)
	baseURL := strings.TrimSpace(s.kimiBaseURL)
	model := strings.TrimSpace(s.kimiModel)
	if apiKey == "" || baseURL == "" || model == "" {
		return nil
	}
	workspaceID, err := s.systemWorkspaceID()
	if err != nil {
		return err
	}
	now := time.Now()
	// Look for an existing platform-default text config first; if
	// found, refresh the credentials in place.
	var existingID int64
	err = s.db.QueryRow(
		`SELECT id FROM llm_configs
		   WHERE is_platform = TRUE
		     AND (provider_kind = 'text' OR provider_kind = '' OR provider_kind IS NULL)
		   ORDER BY id ASC LIMIT 1`,
	).Scan(&existingID)
	if err == nil && existingID > 0 {
		_, uerr := s.db.Exec(
			`UPDATE llm_configs
			    SET base_url = $1, model = $2, api_key = $3, name = $4, updated_at = $5
			  WHERE id = $6`,
			baseURL, model, apiKey, "Kimi · 平台默认", now, existingID,
		)
		if uerr != nil {
			return uerr
		}
		log.Printf("refreshed kimi platform config id=%d (model=%s)", existingID, model)
		return nil
	}
	if _, err := s.db.Exec(
		`INSERT INTO llm_configs (owner_user_id, workspace_id, share_id, shared, is_platform, name, base_url, model, api_key, system_prompt, streaming, provider_kind, extras, created_at, updated_at)
		 VALUES ($1, $2, '', TRUE, TRUE, $3, $4, $5, $6, '', TRUE, '', '{}'::jsonb, $7, $7)`,
		systemUserID,
		workspaceID,
		"Kimi · 平台默认",
		baseURL,
		model,
		apiKey,
		now,
	); err != nil {
		return err
	}
	log.Printf("seeded kimi platform config (model=%s)", model)
	return nil
}

func (s *Server) ensureSystemUser() error {
	if s == nil {
		return errors.New("server is nil")
	}
	user, err := s.getUserByID(systemUserID)
	if err != nil {
		return err
	}
	if user != nil {
		return nil
	}

	password, err := hashPassword(generateSessionID())
	if err != nil {
		return err
	}

	now := time.Now()
	return s.createUser(&User{
		ID:        systemUserID,
		Username:  systemUsername,
		Email:     systemUserEmail,
		Password:  password,
		Role:      "admin",
		Bio:       "站内 AI 助理",
		CreatedAt: now,
	})
}

func (s *Server) registerRoutes() {
	tmpl := template.Must(template.New("layout").Parse(layoutTemplate))
	template.Must(tmpl.New("login").Parse(loginTemplate))
	template.Must(tmpl.New("register").Parse(registerTemplate))
	template.Must(tmpl.New("dashboard").Parse(dashboardTemplate))
	s.router.SetHTMLTemplate(tmpl)

	s.router.GET("/", func(c *gin.Context) {
		if session := s.getAccessSession(extractAccessToken(c)); session != nil {
			c.Redirect(http.StatusFound, "/dashboard")
			return
		}
		c.Redirect(http.StatusFound, "/login")
	})

	s.router.GET("/login", s.GuestMiddleware(), func(c *gin.Context) {
		c.HTML(http.StatusOK, "login", gin.H{"Title": "登录"})
	})

	s.router.GET("/register", s.GuestMiddleware(), func(c *gin.Context) {
		c.HTML(http.StatusOK, "register", gin.H{"Title": "注册"})
	})

	s.router.GET("/dashboard", s.AuthMiddleware(), func(c *gin.Context) {
		username, _ := c.Get("username")
		userID, _ := c.Get("user_id")
		role, _ := c.Get("role")
		roleLabel := "普通用户组"
		if roleStr, ok := role.(string); ok && roleStr == "admin" {
			roleLabel = "管理用户组"
		}
		c.HTML(http.StatusOK, "dashboard", gin.H{
			"Title":     "控制台",
			"Username":  username,
			"UserID":    userID,
			"Role":      role,
			"RoleLabel": roleLabel,
			"LoginTime": time.Now().Format("2006-01-02 15:04:05"),
		})
	})
	s.router.GET("/ws/chat", s.handleChatWS)
	s.router.GET("/ws/agent", s.handleAgentWS)
	// P2 (Shell): byte-bridge WS between browser xterm and the
	// agent's pty. Auth + admin check inline because gin middleware
	// can't run cleanly across an upgrade.
	s.router.GET("/ws/host/:id/shell/:run_id", s.AuthMiddleware(), s.handleHostShellWS)
	// VNC byte-bridge WS — same shape as the shell bridge but binary
	// in both directions (noVNC ↔ agent TCP relay).
	s.router.GET("/ws/host/:id/vnc/:run_id", s.AuthMiddleware(), s.handleHostVNCWS)

	// Prometheus exposition. Gated inside handleMetrics by METRICS_TOKEN —
	// 404 if unset, 401 if mismatched. Lives outside /api so the scrape
	// path can be granted in a reverse proxy without dragging in the rest
	// of the API surface.
	s.router.GET("/metrics", s.handleMetrics)

	// Phase 1-A — internal API surface (HMAC-gated, plugin-only). All
	// routes hang off /internal/v1/ and require an X-Polar-Plugin-Sig
	// header signed with the plugin's stored key hash. Production
	// deployments MUST block /internal/ at the outermost nginx (the
	// HMAC is the second wall, not the only one).
	internal := s.router.Group("/internal/v1", s.hmacPluginAuth())
	{
		// Smoke route — keep around as ops-time health check.
		internal.GET("/ping", s.handleInternalPing)

		// Phase 1-B — documented v1 contract. See
		// doc/arch/internal-api-v1.md for shapes + error codes.
		internal.GET("/auth/verify", s.handleInternalAuthVerify)
		internal.GET("/users/:id", s.handleInternalUserGet)
		internal.GET("/users/:id/workspace", s.handleInternalUserWorkspace)
		internal.GET("/teams/:id", s.handleInternalTeamGet)
		internal.GET("/teams/:id/members/:user", s.handleInternalTeamMember)
		internal.GET("/llm-configs/:id", s.handleInternalLLMConfigGet)
		internal.POST("/llm/chat-completion", s.handleInternalLLMChatCompletion)
		internal.POST("/notify", s.handleInternalNotify)
		internal.POST("/plugin-registry/heartbeat", s.handleInternalHeartbeat)

		// Issue #344: read-only view of the wg_peer_status samples
		// the agent reports via skill.event{metric}. polar-wg polls
		// this every ~30s to populate the per-hub status table.
		internal.GET("/wg-peer-status", s.handleInternalWGPeerStatus)

		// F1 — Phase 2 handler-migration prereqs. See
		// doc/arch/internal-api-v1.md §v1.1.
		internal.GET("/bot-users/:id", s.handleInternalBotUserGet)
		internal.GET("/chat-threads/:id", s.handleInternalChatThreadGet)
		internal.GET("/agent-presence/:bot_id", s.handleInternalAgentPresence)
		internal.POST("/agent/dispatch", s.handleInternalAgentDispatch)
		internal.POST("/agent/llm-call-record", s.handleInternalLLMCallRecord)
		internal.POST("/billing/video-shots", s.handleInternalVideoShotCall)
		internal.GET("/workspace-plugin-access", s.handleInternalWorkspacePluginAccess)
	}

	// iOS distribution OTA endpoints + public share/plaza/install pages
	// moved to github.com/networkextension/polar-iosdist. nginx routes
	// /iosdist/{install,manifest,share,plaza}* to polar-iosdist-svc.

	// wg-mac join control plane (see github.com/networkextension/polar-wg doc/JOIN_PROTOCOL.md). All /v1/*
	// routes are unauthenticated at the gin layer; /v1/register
	// validates the join token in-handler, /v1/peers + heartbeat +
	// leave + token/refresh validate the bearer device-token also
	// in-handler. /v1/install + /v1/bundle are intentionally public.
	//
	// wg routes (/v1/{register,peers,hub/peers,dns,heartbeat,leave,
	// token/refresh,install,bundle}) live in github.com/networkextension/polar-wg.
	// nginx on the deploy box routes /v1/* to that service directly.

	api := s.router.Group("/api")
	{
		api.POST("/register", s.handleRegister)
		api.POST("/login", s.handleLogin)
		api.POST("/logout", s.handleLogout)
		api.POST("/token/refresh", s.handleTokenRefresh)
		api.POST("/passkey/register/begin", s.AuthMiddleware(), s.handlePasskeyRegisterBegin)
		api.POST("/passkey/register/finish", s.AuthMiddleware(), s.handlePasskeyRegisterFinish)
		api.GET("/passkeys", s.AuthMiddleware(), s.handlePasskeyList)
		api.DELETE("/passkeys/:credentialId", s.AuthMiddleware(), s.handlePasskeyDelete)
		api.POST("/passkey/login/begin", s.GuestMiddleware(), s.handlePasskeyLoginBegin)
		api.POST("/passkey/login/finish", s.GuestMiddleware(), s.handlePasskeyLoginFinish)
		api.POST("/email-verification/send", s.AuthMiddleware(), s.handleEmailVerificationSend)
		api.GET("/email-verification/verify", s.handleEmailVerificationConfirm)
		api.POST("/user/icon", s.AuthMiddleware(), s.handleUserIconUpload)
		api.GET("/users", s.AuthMiddleware(), s.handleUserSearch)
		api.PUT("/users/me/profile", s.AuthMiddleware(), s.handleMyProfileUpdate)
		api.GET("/users/me/bookmarks", s.AuthMiddleware(), s.handleMyBookmarkList)
		api.GET("/users/me/following", s.AuthMiddleware(), s.handleMyFollowing)
		api.GET("/users/:id/profile", s.AuthMiddleware(), s.handleUserProfileGet)
		api.POST("/users/:id/recommendations", s.AuthMiddleware(), s.handleProfileRecommendationUpsert)
		api.POST("/users/:id/block", s.AuthMiddleware(), s.handleUserBlockCreate)
		api.DELETE("/users/:id/block", s.AuthMiddleware(), s.handleUserBlockDelete)
		api.POST("/users/:id/follow", s.AuthMiddleware(), s.handleUserFollow)
		api.DELETE("/users/:id/follow", s.AuthMiddleware(), s.handleUserUnfollow)
		api.GET("/users/:id/followers", s.AuthMiddleware(), s.handleUserFollowers)
		api.GET("/users/:id/following", s.AuthMiddleware(), s.handleUserFollowing)
		api.GET("/site-settings", s.handleSiteSettingsGet)
		api.PUT("/site-settings", s.AuthMiddleware(), s.AdminMiddleware(), s.handleSiteSettingsUpdate)
		api.GET("/dashboard/layout", s.AuthMiddleware(), s.handleDashboardLayoutGet)
		api.PUT("/dashboard/layout", s.AuthMiddleware(), s.handleDashboardLayoutUpdate)
		api.POST("/iosdist/agent/builds/init", s.PolarAgentAuthMiddleware(), s.handleIOSAgentBuildInit)
		api.POST("/iosdist/agent/builds/:id/start", s.PolarAgentAuthMiddleware(), s.handleIOSAgentBuildStart)
		api.POST("/iosdist/agent/builds/:id/result", s.PolarAgentAuthMiddleware(), s.handleIOSAgentBuildResult)
		// /api/iosdist/asc/bundle-ids + /asc/apps moved to
		// github.com/networkextension/polar-iosdist.
		api.GET("/prompts", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePromptTemplateList)
		api.GET("/prompts/:slug", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePromptTemplateGet)
		api.PUT("/prompts/:slug", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePromptTemplateUpdate)
		api.GET("/site-settings/invite-codes", s.AuthMiddleware(), s.AdminMiddleware(), s.handleInviteCodeList)
		api.POST("/site-settings/invite-codes", s.AuthMiddleware(), s.AdminMiddleware(), s.handleInviteCodeGenerate)
		api.POST("/site-settings/icon", s.AuthMiddleware(), s.AdminMiddleware(), s.handleSiteIconUpload)
		api.POST("/site-settings/apple-push-cert", s.AuthMiddleware(), s.AdminMiddleware(), s.handleApplePushCertificateUpload)
		api.DELETE("/site-settings/apple-push-cert", s.AuthMiddleware(), s.AdminMiddleware(), s.handleApplePushCertificateDelete)
		api.GET("/llm-configs", s.AuthMiddleware(), s.handleLLMConfigList)
		api.GET("/llm-configs/available", s.AuthMiddleware(), s.handleAvailableLLMConfigList)
		api.POST("/llm-configs/test", s.AuthMiddleware(), s.handleLLMConfigTest)
		api.POST("/llm-configs", s.AuthMiddleware(), s.handleLLMConfigCreate)
		api.GET("/llm-configs/shared/:shareId", s.AuthMiddleware(), s.handleLLMConfigGetByShareID)
		api.PUT("/llm-configs/:id", s.AuthMiddleware(), s.handleLLMConfigUpdate)
		api.DELETE("/llm-configs/:id", s.AuthMiddleware(), s.handleLLMConfigDelete)
		// LLM billing (B1) — shared-config marketplace, applications,
		// and per-team budgets.
		api.GET("/llm-configs/marketplace", s.AuthMiddleware(), s.handleSharedLLMConfigList)
		api.GET("/llm-configs/health", s.AuthMiddleware(), s.handleLLMConfigsHealth)
		api.GET("/llm-config-applications", s.AuthMiddleware(), s.handleLLMConfigApplicationsListMine)
		api.POST("/llm-config-applications", s.AuthMiddleware(), s.handleLLMConfigApply)
		api.PUT("/admin/llm-configs/:id/pricing", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminLLMConfigPricingUpdate)
		api.GET("/admin/llm-config-team-quotas", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminLLMConfigTeamQuotaList)
		api.PUT("/admin/llm-config-team-quotas", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminLLMConfigTeamQuotaUpsert)
		api.GET("/admin/llm-config-applications", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminLLMConfigApplicationsList)
		// B4: prepaid credit / recharge — admin tops up a team's balance,
		// chat handler immediately enforces the new floor.
		api.POST("/admin/llm-config-team-quotas/recharge", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminLLMConfigTeamRecharge)
		api.GET("/admin/llm-config-team-quotas/recharges", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminLLMConfigTeamRechargeList)
		// Phase 1-A — plugin registry. Each row authenticates a plugin
		// service that calls /internal/v1/*. Plaintext shown once at
		// create + rotate; only the hash persists. See
		// doc/arch/internal-api-v1.md + doc/arch/decision.md.
		api.GET("/admin/plugin-modules", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminPluginModuleList)
		api.POST("/admin/plugin-modules", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminPluginModuleCreate)
		api.PATCH("/admin/plugin-modules/:name/enable", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminPluginModuleEnable)
		api.PATCH("/admin/plugin-modules/:name/disable", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminPluginModuleDisable)
		api.POST("/admin/plugin-modules/:name/rotate-key", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminPluginModuleRotateKey)
		api.DELETE("/admin/plugin-modules/:name", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminPluginModuleDelete)
		// LLM proxy — admin-gated token CRUD. The plaintext is shown
		// once at create-time and never returnable again.
		api.GET("/llm-proxy/dashboard-stats", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLLMProxyDashboardStats)
		api.GET("/llm-proxy/tokens", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLLMProxyTokenList)
		api.POST("/llm-proxy/tokens", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLLMProxyTokenCreate)
		api.DELETE("/llm-proxy/tokens/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLLMProxyTokenRevoke)
		api.GET("/llm-proxy/tokens/:id/calls", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLLMProxyTokenCalls)
		api.GET("/llm-proxy/tokens/:id/calls/aggregate", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLLMProxyTokenCallsAggregate)
		api.GET("/llm-proxy/tokens/:id/routes", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLLMProxyTokenRoutesList)
		api.PUT("/llm-proxy/tokens/:id/routes", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLLMProxyTokenRoutesReplace)
		// B6: member-side audit. The three GET handlers above are
		// already workspace-scoped via requireWorkspaceID +
		// getLLMProxyToken(workspaceID, id) — only the admin gate
		// blocked non-admin team members from viewing their own
		// tokens. Re-wire under /me/ without AdminMiddleware so a
		// customer team can see its own consumption history.
		api.GET("/me/llm-proxy/tokens", s.AuthMiddleware(), s.handleLLMProxyTokenList)
		api.GET("/me/llm-proxy/tokens/:id/calls", s.AuthMiddleware(), s.handleLLMProxyTokenCalls)
		api.GET("/me/llm-proxy/tokens/:id/calls/aggregate", s.AuthMiddleware(), s.handleLLMProxyTokenCallsAggregate)
		// Video billing ledger reads (write side is /internal/v1/billing/video-shots).
		// See doc/llm/video-billing.md.
		api.GET("/video-shot-calls", s.AuthMiddleware(), s.handleVideoShotCallsList)
		api.GET("/video-burn", s.AuthMiddleware(), s.handleVideoBurn)
		api.GET("/admin/video-burn", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminVideoBurn)
		// Tenant access control — root team grants per-workspace
		// plugin access. See [[project-llm-billing-state]].
		api.GET("/admin/workspace-plugin-access", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWorkspacePluginAccessList)
		api.PUT("/admin/workspace-plugin-access", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWorkspacePluginAccessUpsert)
		// Dynamic sidebar source for polar-dock-ui. Each plugin
		// pushes its routes via heartbeat → dock aggregates +
		// filters by workspace_plugin_access for non-root tenants.
		api.GET("/plugin-ui-routes", s.AuthMiddleware(), s.handlePluginUIRoutes)
		// Public OpenAI-compatible chat completion endpoint. Auth is
		// done in-handler via Bearer <plaintext> from the
		// llm_proxy_tokens table — no cookie/session auth.
		api.POST("/proxy/v1/chat/completions", s.handleLLMProxyChatCompletions)
		// wg-mac admin endpoints (/api/admin/wg-{hubs,tokens,devices,
		// sites,bundles}) moved to github.com/networkextension/polar-wg.
		// nginx on the deploy box routes /api/admin/wg-* to wg-svc.

		// Public system-info endpoint — intentionally no auth so
		// deploy tooling can probe a fresh box. See sysinfo.go for
		// what's exposed vs not.
		api.GET("/system/info", s.handleSystemInfo)
		// Knowledge-base (rev_devices/firmwares/functions) moved to
		// github.com/networkextension/polar-library. rev_agent_handlers
		// stays in dock (depends on PolarAgentAuthMiddleware).
		// packtunnel (+legacy /proxy-configs aliases) moved to
		// github.com/networkextension/polar-packtunnel.
		// Latch (proxies/rules/profiles/service-nodes/agent) moved to
		// github.com/networkextension/polar-latch.
		api.GET("/bots", s.AuthMiddleware(), s.handleBotUserList)
		api.POST("/bots", s.AuthMiddleware(), s.handleBotUserCreate)
		api.PUT("/bots/:id", s.AuthMiddleware(), s.handleBotUserUpdate)
		api.DELETE("/bots/:id", s.AuthMiddleware(), s.handleBotUserDelete)
		api.GET("/me", s.AuthMiddleware(), s.handleMe)
		api.POST("/devices/push-token", s.AuthMiddleware(), s.handleDevicePushTokenUpdate)
		api.DELETE("/devices/push-token", s.AuthMiddleware(), s.handleDevicePushTokenDelete)
		api.GET("/login-history", s.AuthMiddleware(), s.handleLoginHistory)
		api.POST("/markdown", s.AuthMiddleware(), s.handleMarkdownSubmit)
		api.POST("/markdown/assist-with-bot", s.AuthMiddleware(), s.handleMarkdownAssistWithBot)
		api.GET("/markdown", s.AuthMiddleware(), s.handleMarkdownList)
		api.GET("/markdown/workspace", s.AuthMiddleware(), s.handleWorkspaceMarkdownList)
		api.GET("/public/markdowns", s.handlePublicMarkdownList)
		api.GET("/public/markdown/:id", s.handlePublicMarkdownRead)
		api.GET("/markdown/:id", s.AuthMiddleware(), s.handleMarkdownRead)
		api.PUT("/markdown/:id", s.AuthMiddleware(), s.handleMarkdownUpdate)
		api.DELETE("/markdown/:id", s.AuthMiddleware(), s.handleMarkdownDelete)
		api.POST("/markdown/:id/like", s.AuthMiddleware(), s.handleMarkdownLike)
		api.DELETE("/markdown/:id/like", s.AuthMiddleware(), s.handleMarkdownUnlike)
		api.POST("/markdown/:id/replies", s.AuthMiddleware(), s.handleMarkdownReplyCreate)
		api.GET("/markdown/:id/replies", s.AuthMiddleware(), s.handleMarkdownReplyList)
		api.POST("/markdown/:id/bookmark", s.AuthMiddleware(), s.handleMarkdownBookmark)
		api.DELETE("/markdown/:id/bookmark", s.AuthMiddleware(), s.handleMarkdownUnbookmark)
		api.GET("/tags", s.AuthMiddleware(), s.handleTagList)
		api.POST("/tags", s.AuthMiddleware(), s.AdminMiddleware(), s.handleTagCreate)
		api.PUT("/tags/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleTagUpdate)
		api.DELETE("/tags/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleTagDelete)
		api.POST("/posts", s.AuthMiddleware(), s.handlePostCreate)
		api.GET("/posts", s.AuthMiddleware(), s.handlePostList)
		api.POST("/posts/assist-with-bot", s.AuthMiddleware(), s.handlePostAssistWithBot)
		api.GET("/posts/:id", s.AuthMiddleware(), s.handlePostRead)
		api.DELETE("/posts/:id", s.AuthMiddleware(), s.handlePostDelete)
		api.POST("/posts/:id/like", s.AuthMiddleware(), s.handlePostLike)
		api.DELETE("/posts/:id/like", s.AuthMiddleware(), s.handlePostUnlike)
		api.POST("/posts/:id/bookmark", s.AuthMiddleware(), s.handlePostBookmark)
		api.DELETE("/posts/:id/bookmark", s.AuthMiddleware(), s.handlePostUnbookmark)
		api.POST("/posts/:id/replies", s.AuthMiddleware(), s.handleReplyCreate)
		api.GET("/posts/:id/replies", s.AuthMiddleware(), s.handleReplyList)
		api.POST("/posts/:id/replies/assist-with-bot", s.AuthMiddleware(), s.handleReplyAssistWithBot)
		api.POST("/tasks/:id/apply", s.AuthMiddleware(), s.handleTaskApply)
		api.DELETE("/tasks/:id/apply", s.AuthMiddleware(), s.handleTaskWithdraw)
		api.GET("/tasks/:id/applications", s.AuthMiddleware(), s.handleTaskApplications)
		api.POST("/tasks/:id/close", s.AuthMiddleware(), s.handleTaskClose)
		api.POST("/tasks/:id/select-candidate", s.AuthMiddleware(), s.handleTaskSelectCandidate)
		api.GET("/tasks/:id/results", s.AuthMiddleware(), s.handleTaskResultsList)
		api.POST("/tasks/:id/results", s.AuthMiddleware(), s.handleTaskResultCreate)
		// Qunliao P0a — multi-participant rooms. Coexists with the
		// 1:1 /chats/* surface; message wiring lands in P0b. See
		// doc/qunliao/dev-plan.md for the phasing.
		api.GET("/rooms", s.AuthMiddleware(), s.handleChatRoomList)
		api.POST("/rooms", s.AuthMiddleware(), s.handleChatRoomCreate)
		api.GET("/rooms/:id", s.AuthMiddleware(), s.handleChatRoomGet)
		api.PUT("/rooms/:id", s.AuthMiddleware(), s.handleChatRoomUpdate)
		api.DELETE("/rooms/:id", s.AuthMiddleware(), s.handleChatRoomDelete)
		api.GET("/rooms/:id/members", s.AuthMiddleware(), s.handleChatRoomMemberList)
		api.POST("/rooms/:id/members", s.AuthMiddleware(), s.handleChatRoomMemberAdd)
		api.DELETE("/rooms/:id/members/:userId", s.AuthMiddleware(), s.handleChatRoomMemberRemove)
		api.GET("/rooms/:id/messages", s.AuthMiddleware(), s.handleChatRoomMessageList)
		api.POST("/rooms/:id/messages", s.AuthMiddleware(), s.handleChatRoomMessageSend)
		// P3 idle loop — per-(room, bot) "speak up on your own" config.
		api.GET("/rooms/:id/agent-loops", s.AuthMiddleware(), s.handleChatRoomAgentLoopList)
		api.PUT("/rooms/:id/agent-loops/:botUserId", s.AuthMiddleware(), s.handleChatRoomAgentLoopUpsert)
		api.DELETE("/rooms/:id/agent-loops/:botUserId", s.AuthMiddleware(), s.handleChatRoomAgentLoopDelete)

		api.GET("/chats", s.AuthMiddleware(), s.handleChatList)
		api.POST("/chats/start", s.AuthMiddleware(), s.handleChatStart)
		api.GET("/chats/:id/llm-threads", s.AuthMiddleware(), s.handleChatLLMThreads)
		api.POST("/chats/:id/llm-threads", s.AuthMiddleware(), s.handleChatLLMThreadCreate)
		api.PUT("/chats/:id/llm-threads/:threadId", s.AuthMiddleware(), s.handleChatLLMThreadUpdate)
		api.DELETE("/chats/:id/llm-threads/:threadId", s.AuthMiddleware(), s.handleChatLLMThreadDelete)
		api.PUT("/chats/:id/llm-threads/:threadId/config", s.AuthMiddleware(), s.handleChatLLMThreadConfigUpdate)
		api.GET("/chats/:id/messages", s.AuthMiddleware(), s.handleChatMessages)
		api.GET("/chats/:id/summary", s.AuthMiddleware(), s.handleChatThreadSummary)
		api.POST("/chats/:id/summary/refresh", s.AuthMiddleware(), s.handleChatThreadSummaryRefresh)
		api.POST("/system-agent/bio/refine", s.AuthMiddleware(), s.handleBioRefine)
		api.POST("/projects/:id/tasks/:task_id/sharpen", s.AuthMiddleware(), s.handleProjectTaskSharpen)
		api.GET("/projects/:id/tasks/:task_id/retrospect", s.AuthMiddleware(), s.handleProjectTaskRetrospectGet)
		api.POST("/projects/:id/tasks/:task_id/retrospect", s.AuthMiddleware(), s.handleProjectTaskRetrospectGenerate)
		api.POST("/chats/:id/messages", s.AuthMiddleware(), s.handleChatSend)
		api.POST("/chats/:id/messages/attachment", s.AuthMiddleware(), s.handleChatSendAttachment)
		api.GET("/chats/:id/messages/:messageId", s.AuthMiddleware(), s.handleChatMessageDetail)
		api.POST("/chats/:id/messages/:messageId/retry", s.AuthMiddleware(), s.handleChatRetry)
		api.GET("/chats/:id/messages/:messageId/markdown", s.AuthMiddleware(), s.handleChatSharedMarkdown)
		api.DELETE("/chats/:id/messages/:messageId", s.AuthMiddleware(), s.handleChatDelete)
		// Video studio (projects + shots + assets + render) moved to
		// github.com/networkextension/polar-video.
		api.GET("/system-agent", s.AuthMiddleware(), s.handleSystemAgentStatus)
		// polar-agent local CLI: long-lived bearer tokens + debug
		// dispatch endpoint that proves the WS round-trip without
		// going through the LLM. ai_agent ↔ agent integration is
		// the next phase.
		// attachments: workspace-scoped uploads (zip/pdf/doc/docx/
		// ppt/pptx). Backs posts + chat_messages via junction tables.
		// markdown_entries are out of scope (their .md body stays
		// path-based; future md→pdf renders will produce regular
		// attachments here).
		api.POST("/attachments", s.AuthMiddleware(), s.handleAttachmentUpload)
		api.GET("/attachments/:id", s.AuthMiddleware(), s.handleAttachmentGet)
		api.GET("/attachments/:id/download", s.AuthMiddleware(), s.handleAttachmentDownload)
		api.DELETE("/attachments/:id", s.AuthMiddleware(), s.handleAttachmentDelete)
		api.POST("/agent/tokens", s.AuthMiddleware(), s.handleAgentTokenCreate)
		api.GET("/agent/tokens", s.AuthMiddleware(), s.handleAgentTokenList)
		api.DELETE("/agent/tokens/:id", s.AuthMiddleware(), s.handleAgentTokenRevoke)
		api.GET("/agent/whoami", s.handleAgentWhoAmI)
		api.GET("/agent/binary", s.handleAgentBinary)
		// P1a: dock-initiated bundle install/uninstall. Admin issues
		// the REST call; dock translates into a WS skill.install /
		// skill.uninstall frame to the agent attached to the host.
		api.POST("/hosts/:host_id/skills/install", s.AuthMiddleware(), s.handleHostSkillInstall)
		api.POST("/hosts/:host_id/skills/uninstall", s.AuthMiddleware(), s.handleHostSkillUninstall)
		// P4: list (host, publisher, kind) tuples with catalog versions
		// newer than what's installed. UI uses count for a badge +
		// rows for a "1-click upgrade" link.
		api.GET("/skill-updates", s.AuthMiddleware(), s.handleSkillUpdatesList)
		api.GET("/agent/bots/:bot_id/config", s.PolarAgentAuthMiddleware(), s.handleAgentBotConfig)
		api.POST("/agent/auto-register", s.PolarAgentAuthMiddleware(), s.handleAgentAutoRegister)
		// P-recovery-0a: knowledge-base device upsert via agent token.
		api.POST("/agent/library/devices/upsert", s.PolarAgentAuthMiddleware(), s.handleAgentRevDeviceUpsert)
		api.GET("/agent/bots/:bot_id/status", s.AuthMiddleware(), s.handleAgentStatus)
		api.POST("/agent/bots/:bot_id/dispatch", s.AuthMiddleware(), s.handleAgentDispatch)
		// hosts REST + console layouts now served by polar-hosts-svc; nginx
		// routes /api/hosts/*, /api/host-skills/*, /api/console/layouts/* to
		// the plugin. /ws/host/:id/{shell,vnc}/:run_id remains on dock
		// (router-level, see /ws routes above) so the shell + VNC bridges
		// continue to use dock's agent_hub.
		//
		// projects CRUD + decompose + plan + task lifecycle moved to
		// github.com/networkextension/polar-projects. Sharpen + retrospect
		// stay in dock (registered above near the chat block) because
		// their handlers need system_agent LLM resolution. The 4 research
		// routes below stay because they bridge through agent_hub.
		api.POST("/projects/:id/tasks/:task_id/research/dispatch", s.AuthMiddleware(), s.handleResearchDispatch)
		api.GET("/projects/:id/research/runs", s.AuthMiddleware(), s.handleResearchRunsList)
		api.POST("/projects/research/runs/:run_id/start", s.PolarAgentAuthMiddleware(), s.handleResearchRunStart)
		api.POST("/projects/research/runs/:run_id/result", s.PolarAgentAuthMiddleware(), s.handleResearchRunResult)
		// teams (collaboration workspaces). T2 = CRUD + read-only members
		// list. T3 = invitations. T5 = RequireTeamRole middleware
		// chained on team-scoped routes (replaces inline role checks).
		api.GET("/teams", s.AuthMiddleware(), s.handleTeamList)
		api.POST("/teams", s.AuthMiddleware(), s.handleTeamCreate)
		api.GET("/teams/:id", s.AuthMiddleware(), s.RequireTeamRole("id", TeamRoleViewer), s.handleTeamDetail)
		api.PUT("/teams/:id", s.AuthMiddleware(), s.RequireTeamRole("id", TeamRoleAdmin), s.handleTeamUpdate)
		api.DELETE("/teams/:id", s.AuthMiddleware(), s.RequireTeamRole("id", TeamRoleOwner), s.handleTeamDelete)
		api.GET("/teams/:id/members", s.AuthMiddleware(), s.RequireTeamRole("id", TeamRoleViewer), s.handleTeamMembers)
		api.GET("/teams/:id/logs", s.AuthMiddleware(), s.RequireTeamRole("id", TeamRoleViewer), s.handleTeamLogs)
		// T7: member ops + ownership transfer. Auth via the
		// member-context middleware; the handlers themselves do the
		// finer-grained gates (admin can demote members but only
		// owner can touch admins; everyone can self-leave; owner
		// must transfer before leaving; etc).
		api.PUT("/teams/:id/members/:user_id", s.AuthMiddleware(), s.RequireTeamRole("id", TeamRoleViewer), s.handleTeamMemberRoleUpdate)
		api.DELETE("/teams/:id/members/:user_id", s.AuthMiddleware(), s.RequireTeamRole("id", TeamRoleViewer), s.handleTeamMemberRemove)
		api.POST("/teams/:id/transfer", s.AuthMiddleware(), s.RequireTeamRole("id", TeamRoleOwner), s.handleTeamTransfer)
		// T3 invitations: admin-side (under team) + invitee-side
		// (token-based) + inbox (under user/me, separate path to
		// avoid Gin's literal-vs-wildcard collision at /invitations/*).
		api.POST("/teams/:id/invitations", s.AuthMiddleware(), s.RequireTeamRole("id", TeamRoleAdmin), s.handleTeamInvitationCreate)
		api.GET("/teams/:id/invitations", s.AuthMiddleware(), s.RequireTeamRole("id", TeamRoleAdmin), s.handleTeamInvitationList)
		api.DELETE("/teams/:id/invitations/:inv_id", s.AuthMiddleware(), s.RequireTeamRole("id", TeamRoleAdmin), s.handleTeamInvitationRevoke)
		api.GET("/invitations/:token", s.handleInvitationPublicPreview)
		api.POST("/invitations/:token/accept", s.AuthMiddleware(), s.handleInvitationAccept)
		api.POST("/invitations/:token/decline", s.handleInvitationDecline)
		api.GET("/users/me/invitations", s.AuthMiddleware(), s.handleInvitationInbox)
		api.GET("/users/me/invitations/count", s.AuthMiddleware(), s.handleInvitationInboxCount)
		// iOS distribution module — handlers + public install + sign worker
		// + ASC bridge moved to github.com/networkextension/polar-iosdist.
		// Only the agent endpoints (/api/iosdist/agent/*, lines 766-768)
		// stay in dock because they share PolarAgentAuthMiddleware with
		// the agent_hub still living in dock core.

		// expense (家庭账本, L1 CRUD + L2 OCR/multimodal extraction) moved to
		// github.com/networkextension/polar-expense.
		api.GET("/admin/logs", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminLogs)
		api.GET("/admin/users", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminUserList)
		api.GET("/admin/users/:id/login-history", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminUserLoginHistory)
		api.PUT("/admin/users/:id/password", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminUserPasswordUpdate)
	}
}
