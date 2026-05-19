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
	// Video studio: provider client + bg context for submissions, in-process
	// render queue for ffmpeg concat. Workers (poll + render) are spawned in
	// NewServer and shut down via backgroundCtx cancellation.
	videoProvider        *videoProviderClient
	videoProviderCtx     context.Context
	videoRenderQueue     chan int64
	videoPollIntervalSec int
	videoSeedanceBaseURL string
	videoSeedanceModel   string
	videoSeedanceAPIKey  string

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
	go server.runIOSSignWorker(server.backgroundCtx)

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

	// Video studio wiring. The provider client is stateless — created here
	// once so handlers and the poll worker share the same HTTP transport.
	server.videoProvider = newVideoProviderClient()
	server.videoProviderCtx = server.backgroundCtx
	server.videoPollIntervalSec = cfg.VideoPollIntervalSeconds
	server.videoSeedanceBaseURL = cfg.VideoSeedanceBaseURL
	server.videoSeedanceModel = cfg.VideoSeedanceModel
	server.videoSeedanceAPIKey = cfg.VideoSeedanceAPIKey
	server.kimiBaseURL = cfg.KimiBaseURL
	server.kimiModel = cfg.KimiModel
	server.kimiAPIKey = cfg.KimiAPIKey
	if err := server.ensureSeedanceSystemConfig(); err != nil {
		log.Printf("seed seedance system config failed: %v", err)
	}
	if err := server.ensureKimiPlatformConfig(); err != nil {
		log.Printf("seed kimi platform config failed: %v", err)
	}
	server.startVideoRenderWorker(server.backgroundCtx)
	go server.runVideoPollWorker(server.backgroundCtx)

	server.metricsRegistry = newMetrics()
	server.llmProxyRateLimiter = newLLMProxyRateLimiter()
	server.metricsToken = strings.TrimSpace(cfg.MetricsToken)
	server.startMetricsSampler()
	server.llmProxyProber = newLLMProxyProber(server)
	server.llmProxyProber.start()
	server.llmProxyQuota = newLLMProxyQuotaTracker(server)
	server.startWGStaleDeviceGC(server.backgroundCtx)
	server.startIOSDistStatusSync(server.backgroundCtx)
	server.startHeadscaleProxy()

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
// 'video.seedance' on first boot when VIDEO_SEEDANCE_API_KEY is set, so the
// operator can ship a working default without clicking through the UI form.
// Idempotent: if a row already exists for the system user we leave it alone
// (operators can edit it via the LLM-config form afterwards).
//
// Post T6: lives under the system user's personal team workspace so
// the workspace_id NOT NULL constraint is satisfied. shared=TRUE +
// is_platform=TRUE both true so any workspace can pick this row in
// the video-config picker.
func (s *Server) ensureSeedanceSystemConfig() error {
	if s == nil {
		return nil
	}
	apiKey := strings.TrimSpace(s.videoSeedanceAPIKey)
	baseURL := strings.TrimSpace(s.videoSeedanceBaseURL)
	model := strings.TrimSpace(s.videoSeedanceModel)
	if apiKey == "" || baseURL == "" || model == "" {
		return nil
	}
	var existingID int64
	err := s.db.QueryRow(
		`SELECT id FROM llm_configs WHERE owner_user_id = $1 AND provider_kind = $2 LIMIT 1`,
		systemUserID, LLMConfigKindVideoSeedance,
	).Scan(&existingID)
	if err == nil && existingID > 0 {
		return nil
	}
	workspaceID, err := s.systemWorkspaceID()
	if err != nil {
		return err
	}
	now := time.Now()
	if _, err := s.db.Exec(
		`INSERT INTO llm_configs (owner_user_id, workspace_id, share_id, shared, is_platform, name, base_url, model, api_key, system_prompt, provider_kind, extras, created_at, updated_at)
		 VALUES ($1, $2, '', TRUE, TRUE, $3, $4, $5, $6, '', $7, $8::jsonb, $9, $9)`,
		systemUserID,
		workspaceID,
		"Seedance · 默认",
		baseURL,
		model,
		apiKey,
		LLMConfigKindVideoSeedance,
		`{"ratio":"9:16","duration":10,"generate_audio":true,"watermark":false}`,
		now,
	); err != nil {
		return err
	}
	log.Printf("seeded seedance system config (model=%s)", model)
	return nil
}

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

	// iOS distribution OTA endpoints (no auth — gated by a short-lived,
	// random install token bound to a single version). The manifest.plist
	// must be served with content-type application/xml; see iosdist_handlers.go.
	s.router.GET("/iosdist/install/:token", s.handleIOSInstallPage)
	s.router.GET("/iosdist/manifest/:token", s.handleIOSInstallManifest)
	// Public share page (App Store-style landing). Slug-keyed; visibility
	// is gated server-side by iosdist_apps.is_public.
	s.router.GET("/iosdist/share/:slug", s.handleIOSPublicShareHTML)
	s.router.POST("/iosdist/share/:slug/install", s.handleIOSPublicShareInstall)
	s.router.POST("/iosdist/share/:slug/test-request", s.handleIOSPublicShareTestRequest)
	// Public discovery / "App 广场" — lists all apps that opted in to is_public.
	s.router.GET("/iosdist/plaza", s.handleIOSPublicPlaza)

	// wg-mac join control plane (see doc/JOIN_PROTOCOL.md). All /v1/*
	// routes are unauthenticated at the gin layer; /v1/register
	// validates the join token in-handler, /v1/peers + heartbeat +
	// leave + token/refresh validate the bearer device-token also
	// in-handler. /v1/install + /v1/bundle are intentionally public.
	v1 := s.router.Group("/v1")
	{
		v1.POST("/register", s.handleWGRegister)
		v1.GET("/peers", s.handleWGPeers)
		v1.GET("/hub/peers", s.handleWGHubPeers)
		v1.GET("/dns/:hub_slug", s.handleWGDNSZone)
		v1.GET("/dns", s.handleWGDNSZone)
		v1.POST("/heartbeat", s.handleWGHeartbeat)
		v1.POST("/leave", s.handleWGLeave)
		v1.POST("/token/refresh", s.handleWGTokenRefresh)
		v1.GET("/install", s.handleWGInstallScript)
		v1.GET("/install/:version", s.handleWGInstallScript)
		v1.GET("/bundle", s.handleWGBundleLatest)
		v1.GET("/bundle/:version", s.handleWGBundleDownload)
	}

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
		api.POST("/iosdist/asc/bundle-ids", s.AuthMiddleware(), s.handleIOSASCBundleIDCreate)
		api.POST("/iosdist/asc/apps", s.AuthMiddleware(), s.handleIOSASCAppCreate)
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
		// Public OpenAI-compatible chat completion endpoint. Auth is
		// done in-handler via Bearer <plaintext> from the
		// llm_proxy_tokens table — no cookie/session auth.
		api.POST("/proxy/v1/chat/completions", s.handleLLMProxyChatCompletions)
		// wg-mac Phase 2 admin (see doc/JOIN_PROTOCOL.md). Public /v1/*
		// counterparts are wired at the root router below.
		api.GET("/admin/wg-hubs", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGHubList)
		api.PUT("/admin/wg-hubs/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGHubUpdate)
		api.DELETE("/admin/wg-hubs/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGHubDelete)
		api.POST("/admin/wg-hubs/:id/reset-bind", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGHubResetBind)
		api.GET("/admin/wg-tokens", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGTokenList)
		api.POST("/admin/wg-tokens", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGTokenCreate)
		api.DELETE("/admin/wg-tokens/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGTokenRevoke)
		api.GET("/admin/wg-devices", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGDeviceList)
		api.DELETE("/admin/wg-devices/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGDeviceRemove)
		api.GET("/admin/wg-sites", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGSiteList)
		api.GET("/admin/wg-bundles", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGBundleList)
		api.POST("/admin/wg-bundles/upload", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGBundleUpload)
		api.PUT("/admin/wg-bundles/:id/latest", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGBundleSetLatest)
		api.DELETE("/admin/wg-bundles/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminWGBundleDelete)
		// Public system-info endpoint — intentionally no auth so
		// deploy tooling can probe a fresh box. See sysinfo.go for
		// what's exposed vs not.
		api.GET("/system/info", s.handleSystemInfo)
		// Knowledge-base (P-library-0a) — see doc/jtag-skill-design.md
		// §3.5. Global tables; writes admin-only, reads any-logged-in.
		api.GET("/library/devices", s.AuthMiddleware(), s.handleRevDeviceList)
		api.GET("/library/devices/recent", s.AuthMiddleware(), s.handleRevDeviceRecent)
		api.GET("/library/devices/:id", s.AuthMiddleware(), s.handleRevDeviceGet)
		api.POST("/library/devices", s.AuthMiddleware(), s.AdminMiddleware(), s.handleRevDeviceUpsert)
		api.DELETE("/library/devices/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleRevDeviceDelete)
		api.GET("/library/firmwares", s.AuthMiddleware(), s.handleRevFirmwareList)
		api.GET("/library/firmwares/matching", s.AuthMiddleware(), s.handleRevFirmwareMatching)
		api.GET("/library/firmwares/:id", s.AuthMiddleware(), s.handleRevFirmwareGet)
		api.POST("/library/firmwares", s.AuthMiddleware(), s.AdminMiddleware(), s.handleRevFirmwareCreate)
		api.POST("/library/firmwares/upload", s.AuthMiddleware(), s.AdminMiddleware(), s.handleRevFirmwareUpload)
		api.GET("/library/firmwares/:id/download", s.AuthMiddleware(), s.handleRevFirmwareDownload)
		api.DELETE("/library/firmwares/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleRevFirmwareDelete)
		api.GET("/library/functions", s.AuthMiddleware(), s.handleRevFunctionList)
		api.GET("/library/functions/lookup-by-address", s.AuthMiddleware(), s.handleRevFunctionLookupByAddress)
		api.GET("/library/functions/lookup-by-symbol", s.AuthMiddleware(), s.handleRevFunctionLookupBySymbol)
		api.GET("/library/functions/search", s.AuthMiddleware(), s.handleRevFunctionSearch)
		api.GET("/library/functions/match-signature", s.AuthMiddleware(), s.handleRevFunctionMatchSignature)
		api.GET("/library/functions/:id", s.AuthMiddleware(), s.handleRevFunctionGet)
		api.POST("/library/functions", s.AuthMiddleware(), s.AdminMiddleware(), s.handleRevFunctionCreate)
		api.DELETE("/library/functions/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleRevFunctionDelete)
		api.GET("/packtunnel/profiles", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileList)
		api.GET("/packtunnel/profiles/active", s.AuthMiddleware(), s.handlePackTunnelActiveProfileGet)
		api.GET("/packtunnel/profiles/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileGet)
		api.POST("/packtunnel/profiles", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileCreate)
		api.PUT("/packtunnel/profiles/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileUpdate)
		api.DELETE("/packtunnel/profiles/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileDelete)
		api.PUT("/packtunnel/profiles/:id/activate", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileActivate)
		api.POST("/packtunnel/rules", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelRuleUpload)
		api.GET("/packtunnel/rules", s.AuthMiddleware(), s.handlePackTunnelRuleDownload)
		api.DELETE("/packtunnel/rules", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelRuleDelete)
		// Legacy aliases for older clients that still use /proxy-configs paths.
		api.GET("/proxy-configs", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileList)
		api.GET("/proxy-configs/active", s.AuthMiddleware(), s.handlePackTunnelActiveProfileGet)
		api.GET("/proxy-configs/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileGet)
		api.POST("/proxy-configs", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileCreate)
		api.PUT("/proxy-configs/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileUpdate)
		api.DELETE("/proxy-configs/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileDelete)
		api.POST("/proxy-configs/:id/activate", s.AuthMiddleware(), s.AdminMiddleware(), s.handlePackTunnelProfileActivate)
		// Latch — proxies (admin)
		api.GET("/latch/proxies", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchProxyList)
		api.POST("/latch/proxies", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchProxyCreate)
		api.GET("/latch/proxies/:group_id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchProxyGet)
		api.PUT("/latch/proxies/:group_id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchProxyUpdate)
		api.DELETE("/latch/proxies/:group_id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchProxyDelete)
		api.GET("/latch/proxies/:group_id/versions", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchProxyVersions)
		api.PUT("/latch/proxies/:group_id/rollback/:version", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchProxyRollback)
		// Latch — rules (admin)
		api.GET("/latch/rules", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchRuleList)
		api.POST("/latch/rules", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchRuleCreate)
		api.POST("/latch/rules/upload", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchRuleCreateUpload)
		api.GET("/latch/rules/:group_id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchRuleGet)
		api.GET("/latch/rules/:group_id/content", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchRuleContent)
		api.GET("/latch/rules/:group_id/versions", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchRuleVersions)
		api.PUT("/latch/rules/:group_id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchRuleUpdate)
		api.POST("/latch/rules/:group_id/upload", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchRuleUpload)
		api.DELETE("/latch/rules/:group_id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchRuleDelete)
		api.PUT("/latch/rules/:group_id/rollback/:version", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchRuleRollback)
		// Latch — profiles (admin)
		api.GET("/latch/admin/profiles", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchAdminProfileList)
		api.POST("/latch/admin/profiles", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchAdminProfileCreate)
		api.GET("/latch/admin/profiles/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchAdminProfileGet)
		api.PUT("/latch/admin/profiles/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchAdminProfileUpdate)
		api.DELETE("/latch/admin/profiles/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchAdminProfileDelete)
		// Latch — service nodes (admin)
		api.GET("/latch/admin/service-nodes", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchServiceNodeList)
		api.POST("/latch/admin/service-nodes", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchServiceNodeCreate)
		api.PUT("/latch/admin/service-nodes/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchServiceNodeUpdate)
		api.DELETE("/latch/admin/service-nodes/:id", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchServiceNodeDelete)
		api.POST("/latch/admin/service-nodes/:id/agent-token", s.AuthMiddleware(), s.AdminMiddleware(), s.handleLatchServiceNodeIssueAgentToken)
		// Latch — lightweight agent runtime
		api.POST("/latch/agent/register", s.AgentAuthMiddleware(), s.handleLatchAgentRegister)
		api.POST("/latch/agent/heartbeat", s.AgentAuthMiddleware(), s.handleLatchAgentHeartbeat)
		// Latch — profiles (user: enabled+shared)
		api.GET("/latch/profiles", s.AuthMiddleware(), s.handleLatchProfileList)
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
		// Video studio. Project + shot + asset CRUD, provider submit/retry,
		// and final render. All endpoints require auth and verify ownership.
		api.GET("/video-llm-configs", s.AuthMiddleware(), s.handleVideoLLMConfigList)
		api.GET("/video-projects", s.AuthMiddleware(), s.handleVideoProjectList)
		api.POST("/video-projects", s.AuthMiddleware(), s.handleVideoProjectCreate)
		api.GET("/video-projects/:id", s.AuthMiddleware(), s.handleVideoProjectDetail)
		api.PATCH("/video-projects/:id", s.AuthMiddleware(), s.handleVideoProjectUpdate)
		api.DELETE("/video-projects/:id", s.AuthMiddleware(), s.handleVideoProjectDelete)
		api.POST("/video-projects/:id/shots", s.AuthMiddleware(), s.handleVideoShotCreate)
		api.PATCH("/video-projects/:id/shots/:shotId", s.AuthMiddleware(), s.handleVideoShotUpdate)
		api.DELETE("/video-projects/:id/shots/:shotId", s.AuthMiddleware(), s.handleVideoShotDelete)
		api.POST("/video-projects/:id/shots/:shotId/submit", s.AuthMiddleware(), s.handleVideoShotSubmit)
		api.POST("/video-projects/:id/shots/:shotId/retry", s.AuthMiddleware(), s.handleVideoShotRetry)
		api.POST("/video-projects/:id/shots/:shotId/duplicate", s.AuthMiddleware(), s.handleVideoShotDuplicate)
		api.POST("/video-projects/:id/shots/:shotId/extract-frame", s.AuthMiddleware(), s.handleVideoShotExtractFrame)
		api.POST("/video-projects/:id/submit-all", s.AuthMiddleware(), s.handleVideoProjectSubmitAll)
		api.POST("/video-projects/:id/assets", s.AuthMiddleware(), s.handleVideoAssetUpload)
		api.PATCH("/video-projects/:id/assets/:assetId", s.AuthMiddleware(), s.handleVideoAssetUpdate)
		api.DELETE("/video-projects/:id/assets/:assetId", s.AuthMiddleware(), s.handleVideoAssetDelete)
		api.POST("/video-projects/:id/render", s.AuthMiddleware(), s.handleVideoProjectRender)
		api.GET("/video-projects/:id/download", s.AuthMiddleware(), s.handleVideoProjectDownload)
		api.GET("/video-projects/:id/shots.zip", s.AuthMiddleware(), s.handleVideoProjectShotsZip)
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
		api.GET("/agent/bots/:bot_id/config", s.PolarAgentAuthMiddleware(), s.handleAgentBotConfig)
		api.POST("/agent/auto-register", s.PolarAgentAuthMiddleware(), s.handleAgentAutoRegister)
		// P-recovery-0a: knowledge-base device upsert via agent token.
		api.POST("/agent/library/devices/upsert", s.PolarAgentAuthMiddleware(), s.handleAgentRevDeviceUpsert)
		api.GET("/agent/bots/:bot_id/status", s.AuthMiddleware(), s.handleAgentStatus)
		api.POST("/agent/bots/:bot_id/dispatch", s.AuthMiddleware(), s.handleAgentDispatch)
		// hosts (workspace-scoped named machines reachable via
		// polar-agent; see doc/host-module-design.md). Phase 0
		// landed CRUD + enrollment; skill enable/start lands in P1+.
		api.POST("/hosts/enroll", s.AuthMiddleware(), s.handleHostEnroll)
		api.POST("/hosts/register", s.handleHostRegister) // bearer = enroll token, no session
		// P1e: loopback-only co-located bootstrap. Opt-in via
		// POLAR_ALLOW_LOCAL_BOOTSTRAP=true; the handler also rejects
		// requests with proxy headers or non-loopback RemoteAddr.
		api.POST("/hosts/local-bootstrap", s.handleHostLocalBootstrap)
		api.GET("/hosts", s.AuthMiddleware(), s.handleHostList)
		api.GET("/hosts/:id", s.AuthMiddleware(), s.handleHostDetail)
		api.DELETE("/hosts/:id", s.AuthMiddleware(), s.handleHostDelete)
		// P1d: list endpoints used by the UI surfaces.
		api.GET("/host-skills", s.AuthMiddleware(), s.handleHostSkillsListWorkspace)
		api.GET("/hosts/:id/skills", s.AuthMiddleware(), s.handleHostSkillsListForHost)
		// Rename a host_skill row's display name (does not change kind / id).
		api.PATCH("/hosts/:id/skills/:skillId", s.AuthMiddleware(), s.handleHostSkillRename)
		// P1c.4: per-skill credential vault. Admin-only; never surfaces
		// raw plaintext on read.
		api.GET("/hosts/:id/skills/:skillId/credentials", s.AuthMiddleware(), s.handleHostSkillCredentialsList)
		api.PUT("/hosts/:id/skills/:skillId/credentials/:key", s.AuthMiddleware(), s.handleHostSkillCredentialsPut)
		api.DELETE("/hosts/:id/skills/:skillId/credentials/:key", s.AuthMiddleware(), s.handleHostSkillCredentialsDelete)
		// P2 (Shell): open / sessions / kick. Admin-only.
		api.POST("/hosts/:id/skills/:skillId/shell/open", s.AuthMiddleware(), s.handleHostShellOpen)
		api.GET("/hosts/:id/skills/shell/sessions", s.AuthMiddleware(), s.handleHostShellSessions)
		api.DELETE("/hosts/:id/skills/shell/sessions/:run_id", s.AuthMiddleware(), s.handleHostShellKick)
		// VNC: open / sessions / kick. Admin-only. Wire shape mirrors
		// the Shell endpoints; binary RFB stream bridges via /ws/host/
		// :id/vnc/:run_id.
		api.POST("/hosts/:id/skills/:skillId/vnc/open", s.AuthMiddleware(), s.handleHostVNCOpen)
		api.GET("/hosts/:id/skills/vnc/sessions", s.AuthMiddleware(), s.handleHostVNCSessions)
		api.DELETE("/hosts/:id/skills/vnc/sessions/:run_id", s.AuthMiddleware(), s.handleHostVNCKick)
		// /console.html named layouts (per workspace + per user).
		api.GET("/console/layouts", s.AuthMiddleware(), s.handleConsoleLayoutList)
		api.POST("/console/layouts", s.AuthMiddleware(), s.handleConsoleLayoutCreate)
		api.PATCH("/console/layouts/:layoutId", s.AuthMiddleware(), s.handleConsoleLayoutUpdate)
		api.DELETE("/console/layouts/:layoutId", s.AuthMiddleware(), s.handleConsoleLayoutDelete)
		// projects (workspace-scoped product container with AI
		// requirement decomposition + plan generation).
		api.GET("/projects", s.AuthMiddleware(), s.handleProjectList)
		api.POST("/projects", s.AuthMiddleware(), s.handleProjectCreate)
		api.GET("/projects/:id", s.AuthMiddleware(), s.handleProjectDetail)
		api.PUT("/projects/:id", s.AuthMiddleware(), s.handleProjectUpdate)
		api.DELETE("/projects/:id", s.AuthMiddleware(), s.handleProjectDelete)
		api.POST("/projects/:id/decompose", s.AuthMiddleware(), s.handleProjectDecompose)
		api.PUT("/projects/:id/features/:feature_id", s.AuthMiddleware(), s.handleProjectFeatureUpdate)
		api.DELETE("/projects/:id/features/:feature_id", s.AuthMiddleware(), s.handleProjectFeatureDelete)
		api.POST("/projects/:id/plan", s.AuthMiddleware(), s.handleProjectGeneratePlan)
		api.PUT("/projects/:id/tasks/:task_id", s.AuthMiddleware(), s.handleProjectTaskUpdate)
		api.POST("/projects/:id/tasks/:task_id/pickup", s.AuthMiddleware(), s.handleProjectTaskPickup)
		api.POST("/projects/:id/tasks/:task_id/research/dispatch", s.AuthMiddleware(), s.handleResearchDispatch)
		api.GET("/projects/:id/research/runs", s.AuthMiddleware(), s.handleResearchRunsList)
		api.POST("/projects/research/runs/:run_id/start", s.PolarAgentAuthMiddleware(), s.handleResearchRunStart)
		api.POST("/projects/research/runs/:run_id/result", s.PolarAgentAuthMiddleware(), s.handleResearchRunResult)
		api.POST("/projects/:id/tasks/bulk-status", s.AuthMiddleware(), s.handleProjectTaskBulkStatus)
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
		// iOS distribution module (M1 — owner-scoped CRUD + IPA upload + OTA token).
		// Signing pipeline lands in M2, see DEV_PLAN.md.
		api.GET("/iosdist/apps", s.AuthMiddleware(), s.handleIOSAppList)
		api.POST("/iosdist/apps", s.AuthMiddleware(), s.handleIOSAppCreate)
		api.GET("/iosdist/apps/:id", s.AuthMiddleware(), s.handleIOSAppDetail)
		api.DELETE("/iosdist/apps/:id", s.AuthMiddleware(), s.handleIOSAppDelete)
		api.POST("/iosdist/apps/:id/icon", s.AuthMiddleware(), s.handleIOSAppIconUpload)
		api.PUT("/iosdist/apps/:id/testflight", s.AuthMiddleware(), s.handleIOSAppTestFlightUpdate)
		api.PUT("/iosdist/apps/:id/visibility", s.AuthMiddleware(), s.handleIOSAppVisibilityUpdate)
		api.GET("/iosdist/apps/:id/test-requests", s.AuthMiddleware(), s.handleIOSAppTestRequestList)
		api.POST("/iosdist/apps/:id/versions", s.AuthMiddleware(), s.handleIOSVersionUpload)
		api.DELETE("/iosdist/apps/:id/versions/:versionId", s.AuthMiddleware(), s.handleIOSVersionDelete)
		api.POST("/iosdist/apps/:id/versions/:versionId/install-token", s.AuthMiddleware(), s.handleIOSVersionInstallToken)
		api.POST("/iosdist/apps/:id/versions/:versionId/sign", s.AuthMiddleware(), s.handleIOSVersionSign)
		api.GET("/iosdist/apps/:id/sign-tasks", s.AuthMiddleware(), s.handleIOSAppSignTaskList)
		api.GET("/iosdist/sign-tasks/:id", s.AuthMiddleware(), s.handleIOSSignTaskDetail)
		api.GET("/iosdist/certificates", s.AuthMiddleware(), s.handleIOSCertificateList)
		api.POST("/iosdist/certificates", s.AuthMiddleware(), s.handleIOSCertificateUpload)
		api.DELETE("/iosdist/certificates/:id", s.AuthMiddleware(), s.handleIOSCertificateDelete)
		api.GET("/iosdist/profiles", s.AuthMiddleware(), s.handleIOSProfileList)
		api.POST("/iosdist/profiles", s.AuthMiddleware(), s.handleIOSProfileUpload)
		api.DELETE("/iosdist/profiles/:id", s.AuthMiddleware(), s.handleIOSProfileDelete)
		// App Store Connect bridge: per-owner key + ASC API forwarders.
		api.GET("/iosdist/asc/config", s.AuthMiddleware(), s.handleIOSASCConfigGet)
		api.POST("/iosdist/asc/config", s.AuthMiddleware(), s.handleIOSASCConfigUpsert)
		api.DELETE("/iosdist/asc/config", s.AuthMiddleware(), s.handleIOSASCConfigDelete)
		api.GET("/iosdist/asc/apps", s.AuthMiddleware(), s.handleIOSASCAppList)
		api.GET("/iosdist/asc/beta-groups", s.AuthMiddleware(), s.handleIOSASCBetaGroupList)
		api.PUT("/iosdist/apps/:id/asc-binding", s.AuthMiddleware(), s.handleIOSAppASCBindingUpdate)
		api.POST("/iosdist/apps/:id/invite-tester", s.AuthMiddleware(), s.handleIOSAppInviteTester)
		api.POST("/iosdist/apps/:id/asc-sync", s.AuthMiddleware(), s.handleIOSAppASCSyncMeta)
		// iosdist M4: ops status snapshot (review state, latest build, TF testers).
		// GET reads the cached row (worker refreshes every IOSDIST_STATUS_SYNC_INTERVAL_SEC,
		// default 900s); POST forces an immediate refresh.
		api.GET("/iosdist/apps/:id/status", s.AuthMiddleware(), s.handleIOSAppStatusGet)
		api.POST("/iosdist/apps/:id/status/sync", s.AuthMiddleware(), s.handleIOSAppStatusSync)

		// expense (家庭账本) L1 — manual CRUD over per-workspace expenses
		// + categories. Every workspace member can read/write — no admin
		// gate (household ledger model). OCR/LLM extraction → L2.
		api.GET("/expense-categories", s.AuthMiddleware(), s.handleExpenseCategoryList)
		api.POST("/expense-categories", s.AuthMiddleware(), s.handleExpenseCategoryCreate)
		api.PUT("/expense-categories/:id", s.AuthMiddleware(), s.handleExpenseCategoryUpdate)
		api.DELETE("/expense-categories/:id", s.AuthMiddleware(), s.handleExpenseCategoryDelete)
		api.GET("/expenses", s.AuthMiddleware(), s.handleExpenseList)
		api.POST("/expenses", s.AuthMiddleware(), s.handleExpenseCreate)
		api.PUT("/expenses/:id", s.AuthMiddleware(), s.handleExpenseUpdate)
		api.DELETE("/expenses/:id", s.AuthMiddleware(), s.handleExpenseDelete)
		// L2: receipt upload — multipart image → Apple Vision OCR →
		// LLM-structured draft (status=0). User confirms via PUT above.
		api.POST("/expenses/from-image", s.AuthMiddleware(), s.handleExpenseFromImage)
		api.GET("/expenses/:id/image", s.AuthMiddleware(), s.handleExpenseImageDownload)
		api.GET("/admin/logs", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminLogs)
		api.GET("/admin/users", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminUserList)
		api.GET("/admin/users/:id/login-history", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminUserLoginHistory)
		api.PUT("/admin/users/:id/password", s.AuthMiddleware(), s.AdminMiddleware(), s.handleAdminUserPasswordUpdate)
	}
}
