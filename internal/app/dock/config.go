package dock

import "time"

const (
	// AccessCookieName is the cookie that carries the short-lived access
	// token on every authenticated API request. See doc/auth/auth-refresh.md.
	AccessCookieName = "access_token"
	// RefreshCookieName is the cookie that carries the long-lived refresh
	// token; it is only sent on the refresh endpoint because of the
	// narrow Path scope.
	RefreshCookieName = "refresh_token"
	// RefreshCookiePath limits where the refresh cookie is transmitted so
	// the token doesn't accompany every API call.
	RefreshCookiePath = "/api/token"
	// AccessTokenTTL controls the access cookie Max-Age and the Redis TTL
	// of the access token record. 24h so an active session survives a full
	// workday (and a dock restart) without re-login; the web client also
	// refreshes proactively when <1h remains (refresh rotation limits the
	// leaked-token window in practice).
	AccessTokenTTL = 24 * time.Hour
	// RefreshTokenTTL controls the refresh cookie Max-Age and the Redis
	// TTL of the refresh token record. Refresh rotation on use means a
	// clone is detected long before this window elapses.
	RefreshTokenTTL = 30 * 24 * time.Hour

	DefaultRedisAddr     = "localhost:6379"
	DefaultRedisDB       = 0
	DefaultRedisPrefix   = "polar"
	DefaultMarkdownDir   = "data/markdown"
	DefaultUploadDir     = "data/uploads"
	DefaultGeoLiteDBPath = "data/GeoLite2-City.mmdb"
	DefaultAddr          = ":8080"
	DefaultPostgresDSN   = "postgres://ideamesh:test123456@localhost:5432/ideamesh?sslmode=disable"
	DefaultPasskeyRPID   = "localhost"
	DefaultPasskeyOrigin = "http://localhost:8080"
	DefaultPasskeyRPName = "Gin Auth Demo"
)

type Config struct {
	Addr          string
	PostgresDSN   string
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	RedisPrefix   string
	MarkdownDir   string
	UploadDir     string
	GeoLiteDBPath string
	PasskeyRPID   string
	PasskeyOrigin string
	PasskeyRPName string
	// AIAgent* env vars were removed: the system AI agent is now
	// configured via the llm_configs row marked is_system=TRUE.
	// See store.go::getSystemLLMConfig.
	ApplePushTopic      string
	ApplePushTopicDev   string
	ApplePushTopicProd  string
	ApplePushKeyID      string
	ApplePushKeyIDDev   string
	ApplePushKeyIDProd  string
	ApplePushTeamID     string
	ApplePushTeamIDDev  string
	ApplePushTeamIDProd string
	PublicBaseURL       string
	SMTPHost            string
	SMTPPort            int
	SMTPUsername        string
	SMTPPassword        string
	SMTPFromEmail       string
	SMTPFromName        string

	// Cloudflare R2 object storage for chat attachments (optional).
	// When all five fields are set, chat files are stored in R2.
	// Otherwise the server falls back to local filesystem storage.
	CloudflareR2AccountID       string // Cloudflare account ID
	CloudflareR2AccessKeyID     string // R2 access key ID
	CloudflareR2SecretAccessKey string // R2 secret access key
	CloudflareR2Bucket          string // R2 bucket name
	CloudflareR2PublicURL       string // public base URL, e.g. https://pub-xxx.r2.dev

	// Video studio. Defaults are sane for a localhost dev setup; in prod
	// these should come from env so the operator can swap providers / keys.
	VideoPollIntervalSeconds int    // poll cadence for in-flight Seedance tasks; default 10
	VideoSeedanceBaseURL     string // e.g. https://ark.cn-beijing.volces.com/api/v3
	VideoSeedanceModel       string // e.g. doubao-seedance-1-0-pro-250528
	VideoSeedanceAPIKey      string // ARK_API_KEY equivalent; seeds a system video config on first boot

	// Platform-default LLM (Kimi). Set on the operator side; users
	// don't manage their own AI key — the bot picker surfaces this
	// row tagged "(平台默认)" and any workspace can pick it without
	// configuring anything. Unset = no platform default; users must
	// supply their own LLM config to use bots.
	KimiBaseURL string // default https://api.moonshot.cn/v1
	KimiModel   string // e.g. kimi-k2-turbo-preview
	KimiAPIKey  string

	// iOS distribution: 32-byte hex AES-GCM key used to encrypt .p12
	// passwords at rest. When unset the platform stores cert passwords
	// in plaintext and flags it on the API response so the operator
	// knows to set the key before going to production.
	IOSDistResourceKey string

	// Host module credential vault (P1c.4): 32-byte hex AES-GCM key
	// used to encrypt API keys / tokens stored in host_skill_credentials
	// (per-coder-skill secrets injected as env vars at skill.start).
	// Separate from IOSDIST_RESOURCE_KEY so the security domains stay
	// independent — operator can rotate one without the other. Unset =
	// credentials stored in plaintext with an "unencrypted" badge in
	// the UI.
	PolarCredentialKey string

	// PolarAllowLocalBootstrap (P1e): enables the
	// POST /api/hosts/local-bootstrap shortcut for co-located deploys
	// where polar-dock + polar-agent run on the same machine. The
	// handler only honors requests arriving on the loopback interface
	// with no proxy headers — but the endpoint is opt-in so a stock
	// remote-only deploy doesn't expose it at all. Set via
	// POLAR_ALLOW_LOCAL_BOOTSTRAP=true on the dock launch env.
	PolarAllowLocalBootstrap bool

	// PolarDisableShellSkill (P2): kill-switch for the Shell skill's
	// dock-side endpoints + WS upgrade. Default false (shell enabled
	// for admins). Set true on compliance-sensitive deploys to refuse
	// new shell sessions without rebuilding polar-dock.
	PolarDisableShellSkill bool

	// PolarMaxShellSessions (P2): global ceiling on concurrent shell
	// sessions across all hosts in this dock. Each host also has a
	// per-host cap of 4 sessions (hardcoded). Default 32.
	PolarMaxShellSessions int

	// PolarDisableVNCSkill: kill-switch for the VNC skill's dock-side
	// endpoints + WS upgrade. Default false (vnc enabled for admins).
	// Set true on deploys that should not expose VNC bridges (the
	// agent skill can also be opted out via POLAR_AGENT_VNC_DISABLED).
	PolarDisableVNCSkill bool

	// PolarMaxVNCSessions: global ceiling on concurrent VNC sessions
	// across all hosts. Per-host cap is hard-coded at 2. Default 16.
	PolarMaxVNCSessions int

	// PolarAssetsUseProvider: feature flag for the P3 integration
	// milestone. When false (default) the assets module keeps its P1
	// behavior of writing/reading blob bytes through dock's local
	// uploadDir filesystem stub. When true, dock streams uploads to a
	// polar-assets-svc provider via POST /v1/receive and 302-redirects
	// downloads to GET /v1/blob/<sha>?token=<signed>. The flag stays
	// off in prod until the operator has registered at least one
	// provider via /api/admin/asset-providers AND deployed
	// polar-assets-svc with the matching POLAR_ASSETS_DOCK_HMAC_SECRET.
	// Set via POLAR_ASSETS_USE_PROVIDER=true on the dock launch env.
	PolarAssetsUseProvider bool

	// PolarEnforceTeamStatus: R2-3 flag (default off). When true, a user
	// who explicitly selects a non-active team (status != 'active', e.g.
	// a pending team awaiting site-admin approval) via X-Workspace-Id
	// does NOT get that workspace as their active context — resolution
	// falls back to their always-active home team. Soft gate: blocks
	// operating resources in a non-active team without ever locking the
	// user out of the app. Set POLAR_ENFORCE_TEAM_STATUS=true to enable.
	PolarEnforceTeamStatus bool

	// PolarAssetsPostsDualWrite: P8 flag. When true, each new post-image
	// upload that succeeds through the legacy /uploads/ path ALSO
	// gets registered as an asset (kind=post-image) in the assets
	// catalog via a fire-and-forget background goroutine. Read path
	// stays on the legacy /uploads/<file> URL — this flag only
	// builds the inventory so a future cutover PR can flip reads.
	// Failures are logged + skipped; user's post still succeeds.
	// Default OFF until the operator opts in via
	// POLAR_ASSETS_POSTS_DUAL_WRITE=true.
	PolarAssetsPostsDualWrite bool

	// PolarEnforceLLMAdmin: RBAC fine-grained gate (default off). When true,
	// the LLM-config WRITE endpoints (create/update/delete) require the
	// caller to hold the `llm:admin` permission in the active workspace
	// (owner/admin/site-admin short-circuit; member/viewer or anyone without
	// an explicit llm:admin role grant are blocked). When false (default)
	// the legacy behavior stands — any workspace member may edit configs —
	// so this is behavior-preserving until an operator opts in via
	// POLAR_ENFORCE_LLM_ADMIN=true. The first concrete wiring of Can() into
	// a per-handler resource gate (see rbac.go). Rollback = unset the env.
	PolarEnforceLLMAdmin bool

	// PolarEnforceResourceRBAC: extends the per-handler Can() gate (default off)
	// to bot (bot:admin), asset (asset:write) and prompt (prompt:admin) writes.
	// bot/asset are workspace-scoped (member keeps access; viewer tightened out);
	// prompt is site-scoped (site-admin OR a root-team prompt:admin grant). When
	// false the legacy gates stand (bot/asset = any member; prompt = site-admin),
	// so behavior-preserving until opt-in via POLAR_ENFORCE_RESOURCE_RBAC=true.
	PolarEnforceResourceRBAC bool

	// PolarWorkspaceMounts: 南北向 resource-sharing axis (default off). When
	// true, list queries that opt in (currently LLM configs) union resources
	// from workspaces MOUNTED into the active workspace via workspace_mounts.
	// When false the mount table + CRUD still work but no query consults them,
	// so visibility is unchanged. See doc/arch/workspace-resource-visibility.md.
	// Opt in via POLAR_WORKSPACE_MOUNTS=true.
	PolarWorkspaceMounts bool

	// MetricsToken gates GET /metrics. When unset the endpoint returns
	// 404 (fail-closed); when set, the request must carry
	// `Authorization: Bearer <MetricsToken>`. Configure Prometheus's
	// scrape job with the same value via `bearer_token`. See
	// doc/ops/grafana-integration.md.
	MetricsToken string

	// CookieDomain sets the Domain attribute on the auth Set-Cookie
	// headers (access + refresh). Empty (default) leaves it unset, so
	// the browser scopes the cookie to the request host only. Set to
	// `.4950.store` (leading dot) so the cookie is sent for every
	// `*.4950.store` subdomain — required for SSO across plugin UIs
	// at project.4950.store, expense.4950.store, etc. Configure via
	// POLAR_COOKIE_DOMAIN env on the dock launch env.
	CookieDomain string

	// Plugin cutover flags fully removed in the H-series cleanups. All
	// plugin REST surfaces are remote now; the previous POLAR_<X>_REMOTE
	// env vars are unused and the corresponding fields are gone.

	// PolarHostsTouchURL forwards skill.advertise WS frames to polar-hosts
	// so the row the UI reads (in polar_hosts DB) gets refreshed. Pre-H9
	// dock owned the hosts table and updated it directly on every
	// advertise; post-H9 the data lives in the plugin's DB. Set to
	// http://127.0.0.1:8095/internal/v1/hosts/touch in the standard
	// .57-style deploy. Empty disables forwarding (legacy behavior).
	PolarHostsTouchURL string
}
