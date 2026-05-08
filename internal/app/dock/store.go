package dock

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

var (
	errEmailExists       = errors.New("email already exists")
	errNotMessageOwner   = errors.New("not message owner")
	errChatReplyRequired = errors.New("chat reply required")
	errTaskNotFound      = errors.New("task not found")
	errTaskClosed        = errors.New("task application closed")
	errTaskApplyEnded    = errors.New("task application deadline passed")
	errTaskSelfApply     = errors.New("task owner cannot apply")
	errTaskForbidden     = errors.New("task forbidden")
)

type User struct {
	ID            string     `json:"id"`
	Username      string     `json:"username"`
	Email         string     `json:"email"`
	EmailVerified bool       `json:"email_verified"`
	Password      string     `json:"-"` // password_hash
	Role          string     `json:"role"`
	Bio           string     `json:"bio"`
	IconURL       string     `json:"icon_url"`
	IsOnline      bool       `json:"is_online"`
	DeviceType    string     `json:"device_type"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

type AdminUserSummary struct {
	ID         string     `json:"id"`
	Username   string     `json:"username"`
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	IsOnline   bool       `json:"is_online"`
	DeviceType string     `json:"device_type"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Session is the short-lived access session injected into the Gin
// context by AuthMiddleware. It is the access-token half of the
// refresh-token scheme documented in doc/auth-refresh.md.
type Session struct {
	ID         string
	UserID     string
	Username   string
	Role       string
	DeviceType string
	DeviceID   string
	PushToken  string
	FamilyID   string
	RefreshID  string
	Scopes     []string
	ExpiresAt  time.Time
}

// RefreshToken is the long-lived record that can mint new access
// tokens. Rotated on every use; the whole family is revoked on
// replay (see doc/auth-refresh.md).
type RefreshToken struct {
	ID          string
	UserID      string
	DeviceType  string
	DeviceID    string
	PushToken   string
	FamilyID    string
	PrevRefresh string
	Revoked     bool
	ExpiresAt   time.Time
	IssuedAt    time.Time
}

type UserDevice struct {
	ID           int64
	UserID       string
	DeviceType   string
	DeviceID     string
	PushToken    string
	PushEnabled  bool
	AppVersion   string
	IsOnline     bool
	LastLoginAt  time.Time
	LastSeenAt   *time.Time
	LastActiveAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type PushDelivery struct {
	ID           int64
	MessageID    int64
	UserID       string
	DeviceID     string
	PushToken    string
	Provider     string
	Status       string
	APNSID       string
	ErrorMessage string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type builtinBotPreset struct {
	Name         string
	Description  string
	SystemPrompt string
}

var builtinBotPresets = []builtinBotPreset{
	{
		Name:         "美股分析师",
		Description:  "关注美股市场，提供结构化研究与风险提示。",
		SystemPrompt: "你是美股分析师。请用中文提供结构化分析：先给结论，再给关键数据、驱动因素、风险点与操作建议。严禁编造数据；无法确认时请明确说明不确定性。",
	},
	{
		Name:         "哲学家",
		Description:  "通过哲学视角帮助用户澄清问题与价值取向。",
		SystemPrompt: "你是哲学家。请通过提问与推理帮助用户澄清概念、前提与价值冲突，保持温和、理性、可实践，避免空泛说教。",
	},
	{
		Name:         "代码高手",
		Description:  "擅长代码实现、调试与工程化落地。",
		SystemPrompt: "你是代码高手。请优先给可执行方案与最小改动路径，必要时提供示例代码、边界条件和验证步骤。",
	},
	{
		Name:         "灵魂导师",
		Description:  "提供支持性对话，帮助梳理情绪与行动步骤。",
		SystemPrompt: "你是灵魂导师。请先共情，再帮助用户拆解当前困境，给出小步可执行建议，保持真诚、稳重，不做医疗诊断。",
	},
}

type MarkdownEntry struct {
	ID         int64     `json:"id"`
	UserID     string    `json:"user_id"`
	Username   string    `json:"username"`
	UserIcon   string    `json:"user_icon"`
	Title      string    `json:"title"`
	Summary    string    `json:"summary"`
	CoverURL   string    `json:"cover_url"`
	FilePath   string    `json:"file_path"`
	IsPublic   bool      `json:"is_public"`
	EditorMode string    `json:"editor_mode"`
	UploadedAt time.Time `json:"uploaded_at"`
}

type PublicMarkdownEntry struct {
	ID         int64     `json:"id"`
	UserID     string    `json:"user_id"`
	Username   string    `json:"username"`
	UserIcon   string    `json:"user_icon"`
	Title      string    `json:"title"`
	Summary    string    `json:"summary"`
	CoverURL   string    `json:"cover_url"`
	EditorMode string    `json:"editor_mode"`
	UploadedAt time.Time `json:"uploaded_at"`
}

type LoginRecord struct {
	ID          int64     `json:"id"`
	UserID      string    `json:"user_id"`
	IPAddress   string    `json:"ip_address"`
	Country     string    `json:"country"`
	Region      string    `json:"region"`
	City        string    `json:"city"`
	LoginMethod string    `json:"login_method"`
	DeviceType  string    `json:"device_type"`
	LoggedInAt  time.Time `json:"logged_in_at"`
}

type EmailVerificationToken struct {
	TokenHash  string
	UserID     string
	Email      string
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	CreatedAt  time.Time
}

type Tag struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type SiteSettings struct {
	Name                       string                `json:"name"`
	Description                string                `json:"description"`
	IconURL                    string                `json:"icon_url"`
	RegistrationRequiresInvite bool                  `json:"registration_requires_invite"`
	ApplePushDevCert           *ApplePushCertificate `json:"apple_push_dev_cert,omitempty"`
	ApplePushProdCert          *ApplePushCertificate `json:"apple_push_prod_cert,omitempty"`
	SystemInfo                 *SystemInfo           `json:"system_info,omitempty"`
	UpdatedAt                  time.Time             `json:"updated_at"`
}

type InviteCode struct {
	Code      string     `json:"code"`
	CreatedBy string     `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UsedBy    string     `json:"used_by,omitempty"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	Disabled  bool       `json:"disabled"`
}

type SystemInfo struct {
	GitTagVersion     string `json:"git_tag_version"`
	OS                string `json:"os"`
	CPUArch           string `json:"cpu_arch"`
	PartitionPath     string `json:"partition_path"`
	PartitionCapacity string `json:"partition_capacity"`
}

type LLMConfig struct {
	ID           int64           `json:"id"`
	OwnerUserID  string          `json:"owner_user_id"`
	WorkspaceID  string          `json:"workspace_id"`
	ShareID      string          `json:"share_id"`
	Shared       bool            `json:"shared"`
	Name         string          `json:"name"`
	BaseURL      string          `json:"base_url"`
	Model        string          `json:"model"`
	SystemPrompt string          `json:"system_prompt"`
	Streaming    bool            `json:"streaming"`
	HasAPIKey    bool            `json:"has_api_key"`
	ProviderKind string          `json:"provider_kind,omitempty"`
	IsPlatform   bool            `json:"is_platform,omitempty"`
	Extras       json.RawMessage `json:"extras,omitempty"`
	// ProxyURL: optional HTTP(S) proxy used by dock when calling
	// this provider's API. Empty = use the default Go transport
	// (which respects HTTP_PROXY / HTTPS_PROXY env on the dock
	// machine). Non-empty = override per-config. Useful when one
	// provider needs a proxy (e.g. xAI from CN) but another is
	// reachable directly (local Ollama).
	ProxyURL  string    `json:"proxy_url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LLMConfigKind discriminator values for the provider_kind column.
const (
	LLMConfigKindText           = "text"
	LLMConfigKindVideoSeedance  = "video.seedance"
)

// VideoProject is the top-level container for a multi-shot video production
// (a "script"). Owns shots and audio assets via FK cascade.
type VideoProject struct {
	ID                 int64     `json:"id"`
	OwnerUserID        string    `json:"owner_user_id"`
	WorkspaceID        string    `json:"workspace_id"`
	Title              string    `json:"title"`
	DefaultLLMConfigID *int64    `json:"default_llm_config_id,omitempty"`
	Status             string    `json:"status"`
	FinalVideoURL      string    `json:"final_video_url,omitempty"`
	FinalRenderError   string    `json:"final_render_error,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

const (
	VideoProjectStatusDraft     = "draft"
	VideoProjectStatusRendering = "rendering"
	VideoProjectStatusRendered  = "rendered"
	VideoProjectStatusFailed    = "failed"
)

// VideoShot is one prompt -> one external task -> one downloaded MP4. The
// trim_start_ms / trim_end_ms columns are reserved for a future browser-side
// trim UI; the ffmpeg concat pipeline already honors them.
type VideoShot struct {
	ID            int64      `json:"id"`
	ProjectID     int64      `json:"project_id"`
	Ord           int        `json:"ord"`
	Prompt        string     `json:"prompt"`
	Ratio         string     `json:"ratio"`
	Duration      int        `json:"duration"`
	GenerateAudio bool       `json:"generate_audio"`
	Watermark     bool       `json:"watermark"`
	LLMConfigID   *int64     `json:"llm_config_id,omitempty"`
	TaskID        string     `json:"task_id,omitempty"`
	Status        string     `json:"status"`
	VideoURL      string     `json:"video_url,omitempty"`
	PosterURL     string     `json:"poster_url,omitempty"`
	TrimStartMs   int        `json:"trim_start_ms"`
	TrimEndMs     int        `json:"trim_end_ms"`
	ErrorMessage  string     `json:"error_message,omitempty"`
	SubmittedAt   *time.Time `json:"submitted_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

const (
	VideoShotStatusPending   = "pending"
	VideoShotStatusQueued    = "queued"
	VideoShotStatusRunning   = "running"
	VideoShotStatusSucceeded = "succeeded"
	VideoShotStatusFailed    = "failed"
)

// VideoAsset is a per-project audio attachment: either background music
// (uploaded mp3/aac) or a voiceover (uploaded or recorded via MediaRecorder).
type VideoAsset struct {
	ID          int64     `json:"id"`
	ProjectID   int64     `json:"project_id"`
	Kind        string    `json:"kind"`
	URL         string    `json:"url"`
	FileName    string    `json:"file_name"`
	MimeType    string    `json:"mime_type"`
	Size        int64     `json:"size"`
	DurationMs  int       `json:"duration_ms"`
	BGMVolume   float64   `json:"bgm_volume"`
	VoiceVolume float64   `json:"voice_volume"`
	CreatedAt   time.Time `json:"created_at"`
}

const (
	VideoAssetKindBGM           = "audio_bgm"
	VideoAssetKindVoiceover     = "voiceover"
	VideoAssetKindCharacterRef  = "character_reference"
)

type PackTunnelProfile struct {
	ID          string                    `json:"id"`
	UserID      string                    `json:"user_id"`
	WorkspaceID string                    `json:"workspace_id"`
	Name        string                    `json:"name"`
	Type      string                    `json:"type"`
	Server    PackTunnelServerEndpoint  `json:"server"`
	Auth      PackTunnelAuth            `json:"auth"`
	Options   PackTunnelOptions         `json:"options"`
	Transport *PackTunnelTransport      `json:"transport,omitempty"`
	Metadata  PackTunnelProfileMetadata `json:"metadata"`
	CreatedAt time.Time                 `json:"created_at"`
	UpdatedAt time.Time                 `json:"updated_at"`
}

type PackTunnelServerEndpoint struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

type PackTunnelAuth struct {
	Password string `json:"password"`
	Method   string `json:"method"`
}

type PackTunnelOptions struct {
	TLSEnabled      bool `json:"tls_enabled"`
	UDPRelayEnabled bool `json:"udp_relay_enabled"`
	ChainEnabled    bool `json:"chain_enabled"`
}

type PackTunnelTransport struct {
	Kind   string                  `json:"kind"`
	KCPTun *PackTunnelKCPTunConfig `json:"kcptun,omitempty"`
}

type PackTunnelKCPTunConfig struct {
	Key         string `json:"key"`
	Crypt       string `json:"crypt"`
	Mode        string `json:"mode"`
	AutoExpire  int    `json:"auto_expire"`
	ScavengeTTL int    `json:"scavenge_ttl"`
	MTU         int    `json:"mtu"`
	SndWnd      int    `json:"snd_wnd"`
	RcvWnd      int    `json:"rcv_wnd"`
	DataShard   int    `json:"data_shard"`
	ParityShard int    `json:"parity_shard"`
	DSCP        int    `json:"dscp"`
	NoComp      bool   `json:"no_comp"`
	Salt        string `json:"salt"`
}

type PackTunnelProfileMetadata struct {
	Priority    int    `json:"priority"`
	Enabled     bool   `json:"enabled"`
	Editable    bool   `json:"editable"`
	Source      string `json:"source"`
	CountryCode string `json:"country_code"`
	CountryFlag string `json:"country_flag"`
	IsActive    bool   `json:"is_active"`
}

type PackTunnelRuleFile struct {
	UserID      string    `json:"user_id"`
	WorkspaceID string    `json:"workspace_id"`
	FileName    string    `json:"file_name"`
	StoredName  string    `json:"stored_name"`
	FilePath    string    `json:"file_path"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	UploadedAt  time.Time `json:"uploaded_at"`
}

type BotUser struct {
	ID            int64     `json:"id"`
	OwnerUserID   string    `json:"owner_user_id"`
	WorkspaceID   string    `json:"workspace_id"`
	BotUserID     string    `json:"bot_user_id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	SystemPrompt  string    `json:"system_prompt"`
	// LLMConfigID is 0 for passthrough bots (the column is NULL in
	// DB for those rows; we surface it as 0 in JSON for FE compat).
	LLMConfigID   int64     `json:"llm_config_id"`
	ConfigName    string    `json:"config_name"`
	BotKind       string    `json:"bot_kind"`        // "classic" | "passthrough"
	PreferredTool string    `json:"preferred_tool"`  // "" | "kimi" | "claude" | "codex" | custom
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Bot kind constants. Defined string literals (not iota) since they
// also live in DB rows.
const (
	BotKindClassic     = "classic"
	BotKindPassthrough = "passthrough"
)

type ApplePushCertificate struct {
	Environment string     `json:"environment"`
	FileName    string     `json:"file_name"`
	FileURL     string     `json:"file_url"`
	UploadedAt  *time.Time `json:"uploaded_at,omitempty"`
}

type Post struct {
	ID          int64        `json:"id"`
	UserID      string       `json:"user_id"`
	Username    string       `json:"username"`
	UserIcon    string       `json:"user_icon"`
	WorkspaceID string       `json:"workspace_id,omitempty"`
	TagID       *int64       `json:"tag_id,omitempty"`
	PostType    string       `json:"post_type"`
	Content     string       `json:"content"`
	CreatedAt   time.Time    `json:"created_at"`
	LikeCount   int          `json:"like_count"`
	ReplyCount  int          `json:"reply_count"`
	LikedByMe   bool         `json:"liked_by_me"`
	Images      []string     `json:"images"`
	ImageItems  []PostImage  `json:"image_items,omitempty"`
	Videos      []string     `json:"videos"`
	VideoItems  []PostVideo  `json:"video_items,omitempty"`
	Task        *TaskPost    `json:"task,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

type PostImage struct {
	OriginalURL string `json:"original_url"`
	MediumURL   string `json:"medium_url,omitempty"`
	SmallURL    string `json:"small_url,omitempty"`
}

type PostVideo struct {
	URL       string `json:"url"`
	PosterURL string `json:"poster_url,omitempty"`
}

type PostReply struct {
	ID        int64     `json:"id"`
	PostID    int64     `json:"post_id"`
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	UserIcon  string    `json:"user_icon"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type MarkdownReply struct {
	ID         int64     `json:"id"`
	MarkdownID int64     `json:"markdown_id"`
	UserID     string    `json:"user_id"`
	Username   string    `json:"username"`
	UserIcon   string    `json:"user_icon"`
	Content    string    `json:"content"`
	CreatedAt  time.Time `json:"created_at"`
}

type ChatThread struct {
	ID            int64      `json:"id"`
	UserLow       string     `json:"user_low"`
	UserHigh      string     `json:"user_high"`
	CreatedAt     time.Time  `json:"created_at"`
	LastMessage   string     `json:"last_message"`
	LastMessageID *int64     `json:"last_message_id,omitempty"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
}

type ChatSummary struct {
	ID                   int64      `json:"id"`
	OtherUserID          string     `json:"other_user_id"`
	OtherUsername        string     `json:"other_username"`
	OtherUserIcon        string     `json:"other_user_icon"`
	OtherUserOnline      bool       `json:"other_user_online"`
	OtherUserDeviceType  string     `json:"other_user_device_type"`
	OtherUserLastSeenAt  *time.Time `json:"other_user_last_seen_at,omitempty"`
	LastMessage          string     `json:"last_message"`
	LastMessageAt        *time.Time `json:"last_message_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UnreadCount          int        `json:"unread_count"`
	IsImplicitFriend     bool       `json:"is_implicit_friend"`
	ReplyRequired        bool       `json:"reply_required"`
	ReplyRequiredMessage string     `json:"reply_required_message"`
}

type ChatMessageAttachment struct {
	URL          string `json:"url"`
	FileName     string `json:"file_name"`
	Size         int64  `json:"size"`
	MimeType     string `json:"mime_type"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
}

type ChatMessage struct {
	ID              int64                  `json:"id"`
	ThreadID        int64                  `json:"thread_id"`
	LLMThreadID     *int64                 `json:"llm_thread_id,omitempty"`
	SenderID        string                 `json:"sender_id"`
	SenderUsername  string                 `json:"sender_username"`
	SenderIcon      string                 `json:"sender_icon"`
	MessageType     string                 `json:"message_type"`
	Failed          bool                   `json:"failed"`
	Content         string                 `json:"content"`
	MarkdownEntryID *int64                 `json:"markdown_entry_id,omitempty"`
	MarkdownTitle   string                 `json:"markdown_title,omitempty"`
	LatencyMs       *int64                 `json:"latency_ms,omitempty"`
	Streaming       bool                   `json:"streaming,omitempty"`
	Seq             int64                  `json:"seq,omitempty"`
	UpdatedAt       *time.Time             `json:"updated_at,omitempty"`
	Attachment      *ChatMessageAttachment `json:"attachment,omitempty"`
	CreatedAt       time.Time              `json:"created_at"`
	DeletedAt       *time.Time             `json:"deleted_at,omitempty"`
	DeletedBy       string                 `json:"deleted_by,omitempty"`
	Deleted         bool                   `json:"deleted"`
}

type LLMThread struct {
	ID            int64      `json:"id"`
	ChatThreadID  int64      `json:"chat_thread_id"`
	OwnerUserID   string     `json:"owner_user_id"`
	BotUserID     string     `json:"bot_user_id"`
	LLMConfigID   *int64     `json:"llm_config_id,omitempty"`
	ConfigName    string     `json:"config_name,omitempty"`
	ConfigModel   string     `json:"config_model,omitempty"`
	Title         string     `json:"title"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
}

type WebAuthnCredentialSummary struct {
	CredentialID string    `json:"credential_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type TaskPost struct {
	PostID                int64      `json:"post_id"`
	Location              string     `json:"location,omitempty"`
	StartAt               time.Time  `json:"start_at"`
	EndAt                 time.Time  `json:"end_at"`
	WorkingHours          string     `json:"working_hours"`
	ApplyDeadline         time.Time  `json:"apply_deadline"`
	ApplicationStatus     string     `json:"application_status"`
	SelectedApplicantID   string     `json:"selected_applicant_id,omitempty"`
	SelectedApplicantName string     `json:"selected_applicant_name,omitempty"`
	SelectedAt            *time.Time `json:"selected_at,omitempty"`
	InvitationTemplate    string     `json:"invitation_template,omitempty"`
	InvitationSentAt      *time.Time `json:"invitation_sent_at,omitempty"`
	ApplicantCount        int        `json:"applicant_count"`
	AppliedByMe           bool       `json:"applied_by_me"`
	CanApply              bool       `json:"can_apply"`
	CanManage             bool       `json:"can_manage"`
	SelectedByMe          bool       `json:"selected_by_me"`
	CanViewResults        bool       `json:"can_view_results"`
	CanSubmitResult       bool       `json:"can_submit_result"`
}

type TaskApplication struct {
	ID        int64     `json:"id"`
	PostID    int64     `json:"post_id"`
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	UserIcon  string    `json:"user_icon"`
	AppliedAt time.Time `json:"applied_at"`
}

type TaskResult struct {
	ID         int64       `json:"id"`
	PostID     int64       `json:"post_id"`
	UserID     string      `json:"user_id"`
	Username   string      `json:"username"`
	UserIcon   string      `json:"user_icon"`
	Note       string      `json:"note"`
	CreatedAt  time.Time   `json:"created_at"`
	Images     []string    `json:"images"`
	Videos     []string    `json:"videos"`
	VideoItems []PostVideo `json:"video_items,omitempty"`
}

type ProfileRecommendation struct {
	ID             int64     `json:"id"`
	TargetUserID   string    `json:"target_user_id"`
	AuthorUserID   string    `json:"author_user_id"`
	AuthorUsername string    `json:"author_username"`
	AuthorUserIcon string    `json:"author_user_icon"`
	Content        string    `json:"content"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type UserProfileDetail struct {
	UserID          string                  `json:"user_id"`
	Username        string                  `json:"username"`
	Email           string                  `json:"email,omitempty"`
	IconURL         string                  `json:"icon_url"`
	Bio             string                  `json:"bio"`
	CreatedAt       time.Time               `json:"created_at"`
	IsMe            bool                    `json:"is_me"`
	CanRecommend    bool                    `json:"can_recommend"`
	IBlockedUser    bool                    `json:"i_blocked_user"`
	BlockedMe       bool                    `json:"blocked_me"`
	IsFollowing     bool                    `json:"is_following"`
	FollowedMe      bool                    `json:"followed_me"`
	FollowerCount   int                     `json:"follower_count"`
	FollowingCount  int                     `json:"following_count"`
	Recommendations []ProfileRecommendation `json:"recommendations"`
}

type UserSummary struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	UserIcon    string `json:"user_icon"`
	Bio         string `json:"bio"`
	IsFollowing bool   `json:"is_following"`
}

func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}

	schema := `
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	username TEXT NOT NULL,
	email TEXT NOT NULL UNIQUE,
	email_verified BOOLEAN NOT NULL DEFAULT FALSE,
	password_hash TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'user',
	bio TEXT NOT NULL DEFAULT '',
	icon_url TEXT NOT NULL DEFAULT '',
	is_online BOOLEAN NOT NULL DEFAULT FALSE,
	last_active_device_type TEXT NOT NULL DEFAULT 'browser',
	last_seen_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE users
	ADD COLUMN IF NOT EXISTS email_verified BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE users
	ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'user';
ALTER TABLE users
	ADD COLUMN IF NOT EXISTS bio TEXT NOT NULL DEFAULT '';
ALTER TABLE users
	ADD COLUMN IF NOT EXISTS icon_url TEXT NOT NULL DEFAULT '';
ALTER TABLE users
	ADD COLUMN IF NOT EXISTS is_online BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE users
	ADD COLUMN IF NOT EXISTS last_active_device_type TEXT NOT NULL DEFAULT 'browser';
ALTER TABLE users
	ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ;
-- dashboard_widgets: comma-separated slug list controlling which
-- widgets render on the user's Dashboard, in order. Empty / NULL
-- falls back to the platform default (set in dashboardDefaultLayout
-- so we can change the default without a migration).
ALTER TABLE users
	ADD COLUMN IF NOT EXISTS dashboard_widgets TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS profile_recommendations (
	id BIGSERIAL PRIMARY KEY,
	target_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	author_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	content TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS email_verification_tokens (
	token_hash TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	email TEXT NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL,
	consumed_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_email_verification_tokens_user_id
	ON email_verification_tokens (user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS user_blocks (
	blocker_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	blocked_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (blocker_user_id, blocked_user_id)
);

CREATE TABLE IF NOT EXISTS user_follows (
	follower_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	followee_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (follower_user_id, followee_user_id),
	CHECK (follower_user_id <> followee_user_id)
);

CREATE TABLE IF NOT EXISTS markdown_entries (
	id BIGSERIAL PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title TEXT NOT NULL,
	file_path TEXT NOT NULL,
	is_public BOOLEAN NOT NULL DEFAULT FALSE,
	summary TEXT NOT NULL DEFAULT '',
	cover_url TEXT NOT NULL DEFAULT '',
	editor_mode TEXT NOT NULL DEFAULT 'markdown',
	uploaded_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE markdown_entries
	ADD COLUMN IF NOT EXISTS is_public BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE markdown_entries
	ADD COLUMN IF NOT EXISTS summary TEXT NOT NULL DEFAULT '';
ALTER TABLE markdown_entries
	ADD COLUMN IF NOT EXISTS cover_url TEXT NOT NULL DEFAULT '';
ALTER TABLE markdown_entries
	ADD COLUMN IF NOT EXISTS editor_mode TEXT NOT NULL DEFAULT 'markdown';

CREATE TABLE IF NOT EXISTS webauthn_credentials (
	credential_id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	credential_json JSONB NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS login_records (
	id BIGSERIAL PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	ip_address TEXT NOT NULL,
	country TEXT NOT NULL DEFAULT '',
	region TEXT NOT NULL DEFAULT '',
	city TEXT NOT NULL DEFAULT '',
	login_method TEXT NOT NULL DEFAULT 'password',
	device_type TEXT NOT NULL DEFAULT 'browser',
	logged_in_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE login_records
	ADD COLUMN IF NOT EXISTS device_type TEXT NOT NULL DEFAULT 'browser';

CREATE TABLE IF NOT EXISTS user_devices (
	id BIGSERIAL PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	device_type TEXT NOT NULL DEFAULT 'browser',
	device_id TEXT NOT NULL DEFAULT '',
	push_token TEXT NOT NULL DEFAULT '',
	push_enabled BOOLEAN NOT NULL DEFAULT TRUE,
	app_version TEXT NOT NULL DEFAULT '',
	is_online BOOLEAN NOT NULL DEFAULT FALSE,
	last_login_at TIMESTAMPTZ NOT NULL,
	last_seen_at TIMESTAMPTZ,
	last_active_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	UNIQUE (user_id, device_type, device_id)
);

ALTER TABLE user_devices
	ADD COLUMN IF NOT EXISTS device_id TEXT NOT NULL DEFAULT '';
ALTER TABLE user_devices
	ADD COLUMN IF NOT EXISTS push_enabled BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE user_devices
	ADD COLUMN IF NOT EXISTS app_version TEXT NOT NULL DEFAULT '';
ALTER TABLE user_devices
	ADD COLUMN IF NOT EXISTS last_active_at TIMESTAMPTZ;
UPDATE user_devices
   SET device_id = CASE
       WHEN COALESCE(device_id, '') <> '' THEN device_id
       WHEN device_type = 'ios' THEN 'default:ios'
       WHEN device_type = 'android' THEN 'default:android'
       ELSE 'default:browser'
   END
 WHERE COALESCE(device_id, '') = '';
ALTER TABLE user_devices
	DROP CONSTRAINT IF EXISTS user_devices_user_id_device_type_key;

CREATE TABLE IF NOT EXISTS push_deliveries (
	id BIGSERIAL PRIMARY KEY,
	message_id BIGINT NOT NULL,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	device_id TEXT NOT NULL DEFAULT '',
	push_token TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT 'apns',
	status TEXT NOT NULL,
	apns_id TEXT NOT NULL DEFAULT '',
	error_message TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS chat_member_state (
	thread_id BIGINT NOT NULL REFERENCES chat_threads(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	mute_until TIMESTAMPTZ,
	is_muted BOOLEAN NOT NULL DEFAULT FALSE,
	last_opened_at TIMESTAMPTZ,
	last_delivered_message_id BIGINT,
	last_push_message_id BIGINT,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (thread_id, user_id)
);

CREATE TABLE IF NOT EXISTS tags (
	id BIGSERIAL PRIMARY KEY,
	name TEXT NOT NULL,
	slug TEXT NOT NULL UNIQUE,
	description TEXT NOT NULL DEFAULT '',
	sort_order INT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS site_settings (
	id INT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT 'Polar-',
	description TEXT NOT NULL DEFAULT '',
	icon_url TEXT NOT NULL DEFAULT '',
	registration_requires_invite BOOLEAN NOT NULL DEFAULT FALSE,
	apple_push_dev_cert_url TEXT NOT NULL DEFAULT '',
	apple_push_dev_cert_name TEXT NOT NULL DEFAULT '',
	apple_push_dev_cert_uploaded_at TIMESTAMPTZ,
	apple_push_prod_cert_url TEXT NOT NULL DEFAULT '',
	apple_push_prod_cert_name TEXT NOT NULL DEFAULT '',
	apple_push_prod_cert_uploaded_at TIMESTAMPTZ,
	updated_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE site_settings
	ADD COLUMN IF NOT EXISTS registration_requires_invite BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE site_settings
	ADD COLUMN IF NOT EXISTS apple_push_dev_cert_url TEXT NOT NULL DEFAULT '';
ALTER TABLE site_settings
	ADD COLUMN IF NOT EXISTS apple_push_dev_cert_name TEXT NOT NULL DEFAULT '';
ALTER TABLE site_settings
	ADD COLUMN IF NOT EXISTS apple_push_dev_cert_uploaded_at TIMESTAMPTZ;
ALTER TABLE site_settings
	ADD COLUMN IF NOT EXISTS apple_push_prod_cert_url TEXT NOT NULL DEFAULT '';
ALTER TABLE site_settings
	ADD COLUMN IF NOT EXISTS apple_push_prod_cert_name TEXT NOT NULL DEFAULT '';
ALTER TABLE site_settings
	ADD COLUMN IF NOT EXISTS apple_push_prod_cert_uploaded_at TIMESTAMPTZ;

INSERT INTO site_settings (
	id, name, description, icon_url,
	registration_requires_invite,
	apple_push_dev_cert_url, apple_push_dev_cert_name, apple_push_dev_cert_uploaded_at,
	apple_push_prod_cert_url, apple_push_prod_cert_name, apple_push_prod_cert_uploaded_at,
	updated_at
)
VALUES (1, 'Polar-', '', '', FALSE, '', '', NULL, '', '', NULL, NOW())
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS invite_codes (
	code TEXT PRIMARY KEY,
	created_by TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	used_by TEXT NOT NULL DEFAULT '',
	used_at TIMESTAMPTZ,
	disabled BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX IF NOT EXISTS idx_invite_codes_created_at ON invite_codes(created_at DESC);

-- teams: top-level collaboration workspace. Every user gets a personal
-- team auto-created on first relevant action (kind='personal'); explicit
-- collaboration teams are kind='team'. Resources across modules
-- (iosdist, macapp, ascpool, ...) reference teams.id for ownership.
CREATE TABLE IF NOT EXISTS teams (
	id TEXT PRIMARY KEY,
	slug TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	avatar_url TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT 'team',
	owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_teams_owner ON teams(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_teams_kind ON teams(kind);

-- team_members: (team, user) -> role. role is 'owner'|'admin'|'member'|'viewer'.
-- A team always has exactly one row with role='owner', matching teams.owner_user_id.
-- Personal teams have exactly one member, the owner.
CREATE TABLE IF NOT EXISTS team_members (
	team_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	role TEXT NOT NULL,
	joined_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (team_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_team_members_user ON team_members(user_id);

-- team_invitations: pending/accepted/declined/revoked/expired. token is
-- the unguessable URL component. Same (team_id, lower(email)) at most one
-- 'pending' record at a time; second invite reuses the row and refreshes
-- token + expires_at.
CREATE TABLE IF NOT EXISTS team_invitations (
	id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	email TEXT NOT NULL,
	role TEXT NOT NULL,
	inviter_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token TEXT NOT NULL UNIQUE,
	status TEXT NOT NULL DEFAULT 'pending',
	expires_at TIMESTAMPTZ NOT NULL,
	accepted_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
	accepted_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_team_invitations_team ON team_invitations(team_id, status);
CREATE INDEX IF NOT EXISTS idx_team_invitations_email ON team_invitations(LOWER(email), status);

CREATE TABLE IF NOT EXISTS llm_configs (
	id BIGSERIAL PRIMARY KEY,
	owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	base_url TEXT NOT NULL,
	model TEXT NOT NULL,
	api_key TEXT NOT NULL DEFAULT '',
	system_prompt TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS bot_users (
	id BIGSERIAL PRIMARY KEY,
	owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	bot_user_id TEXT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	system_prompt TEXT NOT NULL DEFAULT '',
	llm_config_id BIGINT NOT NULL REFERENCES llm_configs(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE bot_users
	ADD COLUMN IF NOT EXISTS system_prompt TEXT NOT NULL DEFAULT '';
-- bot_kind:
--   "classic"     — bot's chat replies come from llm_config_id
--                   (regular LLM call, optionally tool-loop when an
--                   agent is attached)
--   "passthrough" — bot is only meaningful when polar-agent is
--                   attached in --tool=<name> mode; the platform
--                   refuses to fall back to LLM if no agent is
--                   attached. llm_config_id may be NULL.
-- preferred_tool: hint string ("kimi"/"claude"/"codex"/...) shown
-- in the UI so users know which polar-agent --tool= flag pairs
-- with this bot. Not enforced — the actual tool comes from the
-- attached agent's hello frame.
ALTER TABLE bot_users
	ADD COLUMN IF NOT EXISTS bot_kind TEXT NOT NULL DEFAULT 'classic';
ALTER TABLE bot_users
	ADD COLUMN IF NOT EXISTS preferred_tool TEXT NOT NULL DEFAULT '';
ALTER TABLE bot_users
	ALTER COLUMN llm_config_id DROP NOT NULL;

ALTER TABLE llm_configs
	ADD COLUMN IF NOT EXISTS share_id TEXT NOT NULL DEFAULT '';
ALTER TABLE llm_configs
	ADD COLUMN IF NOT EXISTS shared BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE llm_configs
	ADD COLUMN IF NOT EXISTS streaming BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE llm_configs
	ADD COLUMN IF NOT EXISTS is_platform BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS idx_llm_configs_platform
	ON llm_configs(is_platform) WHERE is_platform = TRUE;
-- proxy_url: dock's HTTP client uses this when calling the
-- provider's API. Empty = inherit dock's default Go transport
-- (HTTP_PROXY / HTTPS_PROXY env). Non-empty overrides per config.
ALTER TABLE llm_configs
	ADD COLUMN IF NOT EXISTS proxy_url TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS packtunnel_profiles (
	id TEXT PRIMARY KEY,
	owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	profile_type TEXT NOT NULL,
	server_config JSONB NOT NULL DEFAULT '{}'::jsonb,
	auth_config JSONB NOT NULL DEFAULT '{}'::jsonb,
	options_config JSONB NOT NULL DEFAULT '{}'::jsonb,
	transport_config JSONB,
	priority INTEGER NOT NULL DEFAULT 0,
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	editable BOOLEAN NOT NULL DEFAULT TRUE,
	source TEXT NOT NULL DEFAULT 'local',
	country_code TEXT NOT NULL DEFAULT '',
	country_flag TEXT NOT NULL DEFAULT '',
	is_active BOOLEAN NOT NULL DEFAULT FALSE,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS packtunnel_rule_files (
	owner_user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	file_name TEXT NOT NULL,
	stored_name TEXT NOT NULL,
	file_path TEXT NOT NULL,
	file_size BIGINT NOT NULL DEFAULT 0,
	content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
	uploaded_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS posts (
	id BIGSERIAL PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	tag_id BIGINT REFERENCES tags(id) ON DELETE SET NULL,
	post_type TEXT NOT NULL DEFAULT 'standard',
	content TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE posts
	ADD COLUMN IF NOT EXISTS post_type TEXT NOT NULL DEFAULT 'standard';
ALTER TABLE posts
	ADD COLUMN IF NOT EXISTS workspace_id TEXT REFERENCES teams(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_posts_workspace ON posts(workspace_id, created_at DESC);

-- Project structured constraints (added so the AI 拆解 prompt can
-- be tightened with concrete environment + scenario hints, instead
-- of producing wide / generic feature lists). Free text strings —
-- frontend supplies a curated dropdown but doesn't enforce; an
-- empty value means "no constraint".
ALTER TABLE projects ADD COLUMN IF NOT EXISTS runtime_env TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS business_scenario TEXT NOT NULL DEFAULT '';
-- code_provider: preferred coding agent for this project's tasks
-- ("kimi" / "claude" / "codex" / custom tool name; '' = no
-- preference). Used at task pickup to highlight matching bots.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS code_provider TEXT NOT NULL DEFAULT '';
-- Sprint iteration support. Each AI 拆解 increments current_sprint
-- and appends new features tagged with the new sprint_number,
-- instead of replacing the existing list. tasks generated from
-- those features inherit the same sprint_number. Old sprints
-- stay visible in the UI grouped by sprint.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS current_sprint INT NOT NULL DEFAULT 0;
-- git_remote_url: where polar-agent pushes coder-tool output after
-- a task. Empty = no push (agent skips git work entirely). Phase 1
-- relies on the host having git auth configured (SSH key or HTTPS
-- credential helper); per-project deploy keys land in Phase 2.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS git_remote_url TEXT NOT NULL DEFAULT '';

-- attachments: workspace-scoped file uploads with content-addressed
-- storage. Backs posts + chat_messages today via junction tables;
-- markdown_entries' .md body and cover stay separate (the body is
-- mutable and doesn't fit immutability of sha256-keyed storage).
--
-- storage_kind:
--   'local_fs' → storage_ref is a path relative to UploadDir
--                (new uploads: attachments/<ws>/<sha2>/<sha256>;
--                 migrated chat rows: original /uploads/<filename>)
--   'remote'   → storage_ref is an absolute URL (R2 etc.); download
--                handler 302-redirects rather than streaming
--
-- sha256 may be NULL for legacy migrated rows where we don't want
-- to hash large existing files at boot.
CREATE TABLE IF NOT EXISTS attachments (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	uploader_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	filename TEXT NOT NULL,
	mime_type TEXT NOT NULL DEFAULT '',
	size_bytes BIGINT NOT NULL DEFAULT 0,
	storage_kind TEXT NOT NULL DEFAULT 'local_fs',
	storage_ref TEXT NOT NULL,
	sha256 TEXT,
	width INT,
	height INT,
	thumbnail_url TEXT,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	deleted_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_attachments_workspace ON attachments(workspace_id, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_attachments_uploader ON attachments(uploader_user_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_attachments_workspace_sha ON attachments(workspace_id, sha256) WHERE sha256 IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS post_attachments (
	post_id BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
	attachment_id TEXT NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
	ord INT NOT NULL DEFAULT 0,
	PRIMARY KEY (post_id, attachment_id)
);
CREATE INDEX IF NOT EXISTS idx_post_attachments_post ON post_attachments(post_id, ord);

CREATE TABLE IF NOT EXISTS chat_message_attachments (
	message_id BIGINT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
	attachment_id TEXT NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
	ord INT NOT NULL DEFAULT 0,
	PRIMARY KEY (message_id, attachment_id)
);
CREATE INDEX IF NOT EXISTS idx_chat_message_attachments_msg ON chat_message_attachments(message_id, ord);

CREATE TABLE IF NOT EXISTS task_posts (
	post_id BIGINT PRIMARY KEY REFERENCES posts(id) ON DELETE CASCADE,
	location TEXT NOT NULL DEFAULT '',
	start_at TIMESTAMPTZ NOT NULL,
	end_at TIMESTAMPTZ NOT NULL,
	working_hours TEXT NOT NULL,
	apply_deadline TIMESTAMPTZ NOT NULL,
	application_status TEXT NOT NULL DEFAULT 'open',
	selected_applicant_id TEXT REFERENCES users(id) ON DELETE SET NULL,
	selected_at TIMESTAMPTZ,
	invitation_template TEXT NOT NULL DEFAULT '',
	invitation_sent_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS task_applications (
	id BIGSERIAL PRIMARY KEY,
	post_id BIGINT NOT NULL REFERENCES task_posts(post_id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	applied_at TIMESTAMPTZ NOT NULL,
	withdrawn_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS task_results (
	id BIGSERIAL PRIMARY KEY,
	post_id BIGINT NOT NULL REFERENCES task_posts(post_id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	note TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS task_result_images (
	id BIGSERIAL PRIMARY KEY,
	result_id BIGINT NOT NULL REFERENCES task_results(id) ON DELETE CASCADE,
	file_url TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS task_result_videos (
	id BIGSERIAL PRIMARY KEY,
	result_id BIGINT NOT NULL REFERENCES task_results(id) ON DELETE CASCADE,
	file_url TEXT NOT NULL,
	poster_url TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS post_images (
	id BIGSERIAL PRIMARY KEY,
	post_id BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
	file_url TEXT NOT NULL,
	small_url TEXT NOT NULL DEFAULT '',
	medium_url TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE post_images
	ADD COLUMN IF NOT EXISTS small_url TEXT NOT NULL DEFAULT '';

ALTER TABLE post_images
	ADD COLUMN IF NOT EXISTS medium_url TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS post_videos (
	id BIGSERIAL PRIMARY KEY,
	post_id BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
	file_url TEXT NOT NULL,
	poster_url TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE post_videos
	ADD COLUMN IF NOT EXISTS poster_url TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS post_likes (
	post_id BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (post_id, user_id)
);

CREATE TABLE IF NOT EXISTS post_replies (
	id BIGSERIAL PRIMARY KEY,
	post_id BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	content TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS post_bookmarks (
	post_id BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (post_id, user_id)
);

CREATE TABLE IF NOT EXISTS markdown_likes (
	markdown_id BIGINT NOT NULL REFERENCES markdown_entries(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (markdown_id, user_id)
);

CREATE TABLE IF NOT EXISTS markdown_replies (
	id BIGSERIAL PRIMARY KEY,
	markdown_id BIGINT NOT NULL REFERENCES markdown_entries(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	content TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS markdown_bookmarks (
	markdown_id BIGINT NOT NULL REFERENCES markdown_entries(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (markdown_id, user_id)
);

CREATE TABLE IF NOT EXISTS chat_threads (
	id BIGSERIAL PRIMARY KEY,
	user_low TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	user_high TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL,
	last_message TEXT NOT NULL DEFAULT '',
	last_message_id BIGINT,
	last_message_at TIMESTAMPTZ
);

ALTER TABLE chat_threads
	ADD COLUMN IF NOT EXISTS last_message_id BIGINT;

CREATE TABLE IF NOT EXISTS llm_threads (
	id BIGSERIAL PRIMARY KEY,
	chat_thread_id BIGINT NOT NULL REFERENCES chat_threads(id) ON DELETE CASCADE,
	owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	bot_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	llm_config_id BIGINT REFERENCES llm_configs(id) ON DELETE SET NULL,
	title TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	last_message_at TIMESTAMPTZ
);

ALTER TABLE llm_threads
	ADD COLUMN IF NOT EXISTS llm_config_id BIGINT REFERENCES llm_configs(id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS chat_messages (
	id BIGSERIAL PRIMARY KEY,
	thread_id BIGINT NOT NULL REFERENCES chat_threads(id) ON DELETE CASCADE,
	llm_thread_id BIGINT REFERENCES llm_threads(id) ON DELETE SET NULL,
	sender_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	message_type TEXT NOT NULL DEFAULT 'text',
	failed BOOLEAN NOT NULL DEFAULT FALSE,
	content TEXT NOT NULL,
	markdown_entry_id BIGINT REFERENCES markdown_entries(id) ON DELETE SET NULL,
	markdown_title TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE chat_messages
	ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

ALTER TABLE chat_messages
	ADD COLUMN IF NOT EXISTS deleted_by TEXT;

ALTER TABLE chat_messages
	ADD COLUMN IF NOT EXISTS message_type TEXT NOT NULL DEFAULT 'text';

ALTER TABLE chat_messages
	ADD COLUMN IF NOT EXISTS failed BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE chat_messages
	ADD COLUMN IF NOT EXISTS markdown_entry_id BIGINT REFERENCES markdown_entries(id) ON DELETE SET NULL;

ALTER TABLE chat_messages
	ADD COLUMN IF NOT EXISTS markdown_title TEXT NOT NULL DEFAULT '';

ALTER TABLE chat_messages
	ADD COLUMN IF NOT EXISTS llm_thread_id BIGINT REFERENCES llm_threads(id) ON DELETE SET NULL;

-- chat_messages.attachment (JSONB inline column) was migrated to
-- attachments + chat_message_attachments junction in the
-- attachments-refactor PR. The ADD COLUMN line that used to live
-- here is intentionally absent so a fresh boot leaves the column
-- gone; old DBs hit migrateChatMessageAttachments at startup,
-- which copies + DROP COLUMNs.

ALTER TABLE chat_messages
	ADD COLUMN IF NOT EXISTS latency_ms INTEGER;

ALTER TABLE chat_messages
	ADD COLUMN IF NOT EXISTS streaming BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE chat_messages
	ADD COLUMN IF NOT EXISTS seq BIGINT NOT NULL DEFAULT 0;

ALTER TABLE chat_messages
	ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS chat_reads (
	thread_id BIGINT NOT NULL REFERENCES chat_threads(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	last_read_at TIMESTAMPTZ NOT NULL,
	last_read_message_id BIGINT,
	PRIMARY KEY (thread_id, user_id)
);

ALTER TABLE chat_reads
	ADD COLUMN IF NOT EXISTS last_read_message_id BIGINT;

CREATE INDEX IF NOT EXISTS idx_markdown_entries_user_id ON markdown_entries(user_id);
CREATE INDEX IF NOT EXISTS idx_webauthn_credentials_user_id ON webauthn_credentials(user_id);
CREATE INDEX IF NOT EXISTS idx_login_records_user_id_logged_in_at ON login_records(user_id, logged_in_at DESC);
CREATE INDEX IF NOT EXISTS idx_user_devices_user_id ON user_devices(user_id);
CREATE INDEX IF NOT EXISTS idx_user_devices_push_token ON user_devices(push_token);
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_devices_user_device ON user_devices(user_id, device_type, device_id);
CREATE INDEX IF NOT EXISTS idx_push_deliveries_message_id ON push_deliveries(message_id);
CREATE INDEX IF NOT EXISTS idx_push_deliveries_user_id_created_at ON push_deliveries(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_llm_configs_owner_user_id ON llm_configs(owner_user_id, updated_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_llm_configs_share_id ON llm_configs(share_id) WHERE share_id <> '';
CREATE INDEX IF NOT EXISTS idx_packtunnel_profiles_owner_updated_at ON packtunnel_profiles(owner_user_id, updated_at DESC, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_packtunnel_profiles_owner_active ON packtunnel_profiles(owner_user_id) WHERE is_active = TRUE;
CREATE INDEX IF NOT EXISTS idx_packtunnel_rule_files_uploaded_at ON packtunnel_rule_files(uploaded_at DESC);
CREATE INDEX IF NOT EXISTS idx_bot_users_owner_user_id ON bot_users(owner_user_id, updated_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_profile_recommendations_target_author ON profile_recommendations(target_user_id, author_user_id);
CREATE INDEX IF NOT EXISTS idx_profile_recommendations_target_updated_at ON profile_recommendations(target_user_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_user_blocks_blocked_user_id ON user_blocks(blocked_user_id, blocker_user_id);
CREATE INDEX IF NOT EXISTS idx_user_follows_followee ON user_follows(followee_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_user_follows_follower ON user_follows(follower_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tags_sort_order_created_at ON tags(sort_order DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_posts_created_at ON posts(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_post_images_post_id ON post_images(post_id);
CREATE INDEX IF NOT EXISTS idx_post_videos_post_id ON post_videos(post_id);
CREATE INDEX IF NOT EXISTS idx_post_likes_post_id ON post_likes(post_id);
CREATE INDEX IF NOT EXISTS idx_post_replies_post_id ON post_replies(post_id);
CREATE INDEX IF NOT EXISTS idx_task_posts_apply_deadline ON task_posts(apply_deadline DESC);
CREATE INDEX IF NOT EXISTS idx_task_applications_post_id ON task_applications(post_id, applied_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_task_applications_active_pair
	ON task_applications(post_id, user_id)
	WHERE withdrawn_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_task_results_post_id_created_at ON task_results(post_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_task_result_images_result_id ON task_result_images(result_id);
CREATE INDEX IF NOT EXISTS idx_task_result_videos_result_id ON task_result_videos(result_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_threads_pair ON chat_threads(user_low, user_high);
CREATE INDEX IF NOT EXISTS idx_chat_threads_last_message_at ON chat_threads(last_message_at DESC);
CREATE INDEX IF NOT EXISTS idx_llm_threads_chat_thread_id_updated_at ON llm_threads(chat_thread_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_chat_messages_thread_id_created_at ON chat_messages(thread_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_chat_messages_llm_thread_id_created_at ON chat_messages(llm_thread_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_chat_reads_user_id ON chat_reads(user_id);

CREATE TABLE IF NOT EXISTS latch_proxies (
	id       TEXT NOT NULL,
	group_id TEXT NOT NULL,
	name     TEXT NOT NULL,
	type     TEXT NOT NULL,
	config   JSONB NOT NULL DEFAULT '{}'::jsonb,
	sha1     TEXT NOT NULL DEFAULT '',
	version  INT  NOT NULL DEFAULT 1,
	created_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (id)
);
CREATE INDEX IF NOT EXISTS idx_latch_proxies_group_version ON latch_proxies(group_id, version DESC);

CREATE TABLE IF NOT EXISTS latch_service_nodes (
	id              TEXT NOT NULL PRIMARY KEY,
	name            TEXT NOT NULL,
	ip              TEXT NOT NULL,
	port            INT NOT NULL,
	proxy_type      TEXT NOT NULL,
	config          JSONB NOT NULL DEFAULT '{}'::jsonb,
	status          TEXT NOT NULL DEFAULT 'unknown',
	last_updated_at TIMESTAMPTZ NOT NULL,
	created_at      TIMESTAMPTZ NOT NULL,
	updated_at      TIMESTAMPTZ NOT NULL,
	is_deleted      BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS idx_latch_service_nodes_active_updated ON latch_service_nodes(is_deleted, updated_at DESC);

CREATE TABLE IF NOT EXISTS latch_service_node_agent_tokens (
	id         TEXT NOT NULL PRIMARY KEY,
	node_id    TEXT NOT NULL REFERENCES latch_service_nodes(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	created_by TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	last_used_at TIMESTAMPTZ,
	revoked    BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS idx_latch_service_node_agent_tokens_node_active ON latch_service_node_agent_tokens(node_id, revoked, created_at DESC);

CREATE TABLE IF NOT EXISTS latch_service_node_heartbeats (
	id              TEXT NOT NULL PRIMARY KEY,
	node_id         TEXT NOT NULL REFERENCES latch_service_nodes(id) ON DELETE CASCADE,
	status          TEXT NOT NULL DEFAULT 'unknown',
	connected_peers INT NOT NULL DEFAULT 0,
	rx_bytes        BIGINT NOT NULL DEFAULT 0,
	tx_bytes        BIGINT NOT NULL DEFAULT 0,
	agent_version   TEXT NOT NULL DEFAULT '',
	hostname        TEXT NOT NULL DEFAULT '',
	payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
	reported_at     TIMESTAMPTZ NOT NULL,
	created_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_latch_service_node_heartbeats_node_time ON latch_service_node_heartbeats(node_id, reported_at DESC);

CREATE TABLE IF NOT EXISTS latch_rules (
	id         TEXT NOT NULL,
	group_id   TEXT NOT NULL,
	name       TEXT NOT NULL,
	content    TEXT NOT NULL DEFAULT '',
	sha1       TEXT NOT NULL DEFAULT '',
	version    INT  NOT NULL DEFAULT 1,
	created_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (id)
);
CREATE INDEX IF NOT EXISTS idx_latch_rules_group_version ON latch_rules(group_id, version DESC);

CREATE TABLE IF NOT EXISTS latch_profiles (
	id              TEXT NOT NULL PRIMARY KEY,
	name            TEXT NOT NULL,
	description     TEXT NOT NULL DEFAULT '',
	proxy_group_ids TEXT[] NOT NULL DEFAULT '{}',
	rule_group_id   TEXT,
	enabled         BOOLEAN NOT NULL DEFAULT TRUE,
	shareable       BOOLEAN NOT NULL DEFAULT FALSE,
	created_at      TIMESTAMPTZ NOT NULL,
	updated_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_latch_profiles_enabled_shareable ON latch_profiles(enabled, shareable, created_at DESC);

ALTER TABLE llm_configs
	ADD COLUMN IF NOT EXISTS provider_kind TEXT NOT NULL DEFAULT 'text';
ALTER TABLE llm_configs
	ADD COLUMN IF NOT EXISTS extras JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE TABLE IF NOT EXISTS video_projects (
	id BIGSERIAL PRIMARY KEY,
	owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title TEXT NOT NULL DEFAULT '',
	default_llm_config_id BIGINT REFERENCES llm_configs(id) ON DELETE SET NULL,
	status TEXT NOT NULL DEFAULT 'draft',
	final_video_url TEXT NOT NULL DEFAULT '',
	final_render_error TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_video_projects_owner ON video_projects(owner_user_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS video_shots (
	id BIGSERIAL PRIMARY KEY,
	project_id BIGINT NOT NULL REFERENCES video_projects(id) ON DELETE CASCADE,
	ord INT NOT NULL DEFAULT 0,
	prompt TEXT NOT NULL DEFAULT '',
	ratio TEXT NOT NULL DEFAULT '9:16',
	duration INT NOT NULL DEFAULT 10,
	generate_audio BOOLEAN NOT NULL DEFAULT TRUE,
	watermark BOOLEAN NOT NULL DEFAULT FALSE,
	llm_config_id BIGINT REFERENCES llm_configs(id) ON DELETE SET NULL,
	task_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	video_url TEXT NOT NULL DEFAULT '',
	trim_start_ms INT NOT NULL DEFAULT 0,
	trim_end_ms INT NOT NULL DEFAULT 0,
	error_message TEXT NOT NULL DEFAULT '',
	submitted_at TIMESTAMPTZ,
	completed_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_video_shots_project ON video_shots(project_id, ord);
CREATE INDEX IF NOT EXISTS idx_video_shots_status ON video_shots(status);

ALTER TABLE video_shots
	ADD COLUMN IF NOT EXISTS poster_url TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS video_assets (
	id BIGSERIAL PRIMARY KEY,
	project_id BIGINT NOT NULL REFERENCES video_projects(id) ON DELETE CASCADE,
	kind TEXT NOT NULL,
	url TEXT NOT NULL DEFAULT '',
	file_name TEXT NOT NULL DEFAULT '',
	mime_type TEXT NOT NULL DEFAULT '',
	size BIGINT NOT NULL DEFAULT 0,
	duration_ms INT NOT NULL DEFAULT 0,
	bgm_volume REAL NOT NULL DEFAULT 0.3,
	voice_volume REAL NOT NULL DEFAULT 1.0,
	created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_video_assets_project ON video_assets(project_id, kind);

CREATE TABLE IF NOT EXISTS iosdist_apps (
	id BIGSERIAL PRIMARY KEY,
	owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	bundle_id TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	icon_url TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_iosdist_apps_owner ON iosdist_apps(owner_user_id, updated_at DESC);

-- icon_source: '' (unset) | 'ipa' (auto-extracted from latest IPA upload) |
-- 'manual' (operator uploaded). Manual wins forever — future IPA uploads
-- never overwrite a manually-set icon.
ALTER TABLE iosdist_apps
	ADD COLUMN IF NOT EXISTS icon_source TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS testflight_url TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS asc_app_id TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS asc_beta_group_id TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS public_slug TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS is_public BOOLEAN NOT NULL DEFAULT FALSE,
	ADD COLUMN IF NOT EXISTS keywords TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS whats_new TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS promotional_text TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS marketing_url TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS support_url TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS uniq_iosdist_apps_public_slug
	ON iosdist_apps(public_slug) WHERE public_slug <> '';

-- iosdist_test_requests: every "申请测试" submission from the public app
-- page lands here. status: pending|sent|failed. asc_response carries the
-- ASC error message (when sent failed) or the new tester id (when ok).
CREATE TABLE IF NOT EXISTS iosdist_test_requests (
	id BIGSERIAL PRIMARY KEY,
	app_id BIGINT NOT NULL REFERENCES iosdist_apps(id) ON DELETE CASCADE,
	email TEXT NOT NULL,
	first_name TEXT NOT NULL DEFAULT '',
	last_name TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	asc_response TEXT NOT NULL DEFAULT '',
	source_ip TEXT NOT NULL DEFAULT '',
	user_agent TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	processed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_iosdist_test_requests_app ON iosdist_test_requests(app_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_iosdist_test_requests_email ON iosdist_test_requests(email, created_at DESC);

-- iosdist_sign_tasks: every "re-sign with cert+profile" job. Worker picks
-- pending → marks running → exec zsign → on success creates a new
-- iosdist_versions row pointed at by output_version_id. idempotency_key
-- = sha256(ipa_sha256 || cert_id || profile_id) so repeat clicks reuse
-- the previous successful task instead of spamming Apple's servers /
-- chatStorage with duplicate output IPAs.
CREATE TABLE IF NOT EXISTS iosdist_sign_tasks (
	id BIGSERIAL PRIMARY KEY,
	app_id BIGINT NOT NULL REFERENCES iosdist_apps(id) ON DELETE CASCADE,
	source_version_id BIGINT NOT NULL REFERENCES iosdist_versions(id) ON DELETE CASCADE,
	output_version_id BIGINT,
	cert_id BIGINT NOT NULL REFERENCES iosdist_certificates(id) ON DELETE CASCADE,
	profile_id BIGINT NOT NULL REFERENCES iosdist_profiles(id) ON DELETE CASCADE,
	idempotency_key TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	error_message TEXT NOT NULL DEFAULT '',
	log_text TEXT NOT NULL DEFAULT '',
	submitted_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	submitted_at TIMESTAMPTZ NOT NULL,
	started_at TIMESTAMPTZ,
	completed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_iosdist_sign_tasks_app ON iosdist_sign_tasks(app_id, submitted_at DESC);
CREATE INDEX IF NOT EXISTS idx_iosdist_sign_tasks_status ON iosdist_sign_tasks(status);
CREATE INDEX IF NOT EXISTS idx_iosdist_sign_tasks_idem ON iosdist_sign_tasks(idempotency_key);

-- Optional bundle_id / display name rewrite at sign time. Empty string
-- means "preserve original". Used for security-research workflows where
-- the original bundle id collides with another developer's registration
-- and zsign must rewrite (zsign auto-cascades to embedded extensions).
ALTER TABLE iosdist_sign_tasks
	ADD COLUMN IF NOT EXISTS new_bundle_id TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS new_app_name TEXT NOT NULL DEFAULT '';

-- ASC API credentials, scoped per owner so multi-tenant deployments don't
-- share keys. p8_cipher uses the same IOSDIST_RESOURCE_KEY AES-GCM as the
-- cert password column. p8_encrypted=false means the operator hadn't set
-- IOSDIST_RESOURCE_KEY at upload time — the file stays plaintext-on-disk
-- and the UI flags it.
CREATE TABLE IF NOT EXISTS iosdist_asc_configs (
	owner_user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	issuer_id TEXT NOT NULL,
	key_id TEXT NOT NULL,
	p8_cipher TEXT NOT NULL,
	p8_filename TEXT NOT NULL DEFAULT '',
	p8_encrypted BOOLEAN NOT NULL DEFAULT FALSE,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
ALTER TABLE iosdist_asc_configs
	ADD COLUMN IF NOT EXISTS account_holder_email TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS team_name TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS iosdist_versions (
	id BIGSERIAL PRIMARY KEY,
	app_id BIGINT NOT NULL REFERENCES iosdist_apps(id) ON DELETE CASCADE,
	version TEXT NOT NULL,
	build_number TEXT NOT NULL DEFAULT '',
	ipa_url TEXT NOT NULL DEFAULT '',
	ipa_filename TEXT NOT NULL DEFAULT '',
	ipa_size BIGINT NOT NULL DEFAULT 0,
	ipa_sha256 TEXT NOT NULL DEFAULT '',
	release_notes TEXT NOT NULL DEFAULT '',
	is_signed BOOLEAN NOT NULL DEFAULT TRUE,
	created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_iosdist_versions_app ON iosdist_versions(app_id, created_at DESC);

CREATE TABLE IF NOT EXISTS iosdist_install_tokens (
	token TEXT PRIMARY KEY,
	version_id BIGINT NOT NULL REFERENCES iosdist_versions(id) ON DELETE CASCADE,
	created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	last_accessed_at TIMESTAMPTZ,
	access_count INT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_iosdist_install_tokens_version ON iosdist_install_tokens(version_id);
CREATE INDEX IF NOT EXISTS idx_iosdist_install_tokens_expires ON iosdist_install_tokens(expires_at);

-- Distribution type carries the Apple signing channel for the IPA. Only
-- ad_hoc / enterprise / development can be installed OTA via itms-services;
-- app_store builds go through ASC and we refuse to issue install tokens for
-- them.
ALTER TABLE iosdist_versions
	ADD COLUMN IF NOT EXISTS distribution_type TEXT NOT NULL DEFAULT 'ad_hoc';

-- IPA introspection: parsed from Info.plist on upload. Empty strings mean
-- the file wasn't a valid IPA or the parser couldn't read its plist.
ALTER TABLE iosdist_versions
	ADD COLUMN IF NOT EXISTS ipa_bundle_id TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS ipa_short_version TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS ipa_build_number TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS ipa_display_name TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS ipa_min_os TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS ipa_has_embedded_profile BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS iosdist_certificates (
	id BIGSERIAL PRIMARY KEY,
	owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'distribution',
	file_url TEXT NOT NULL,
	file_filename TEXT NOT NULL DEFAULT '',
	file_size BIGINT NOT NULL DEFAULT 0,
	password_cipher TEXT NOT NULL DEFAULT '',
	password_encrypted BOOLEAN NOT NULL DEFAULT FALSE,
	team_id TEXT NOT NULL DEFAULT '',
	common_name TEXT NOT NULL DEFAULT '',
	notes TEXT NOT NULL DEFAULT '',
	expires_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_iosdist_certificates_owner ON iosdist_certificates(owner_user_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS iosdist_profiles (
	id BIGSERIAL PRIMARY KEY,
	owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'ad_hoc',
	file_url TEXT NOT NULL,
	file_filename TEXT NOT NULL DEFAULT '',
	file_size BIGINT NOT NULL DEFAULT 0,
	app_id TEXT NOT NULL DEFAULT '',
	team_id TEXT NOT NULL DEFAULT '',
	udid_count INT NOT NULL DEFAULT 0,
	notes TEXT NOT NULL DEFAULT '',
	expires_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_iosdist_profiles_owner ON iosdist_profiles(owner_user_id, updated_at DESC);

-- T6-A workspace migration: add workspace_id (FK -> teams.id) to
-- module resource tables. Column is nullable in this PR; a startup
-- backfill (backfillResourceWorkspaceIDs) populates it from each
-- row's owner_user_id personal team. Phase B (T6-D) flips to NOT
-- NULL after handler migration is verified. Column is named
-- workspace_id (not team_id) because iosdist_certificates +
-- iosdist_profiles already carry an Apple Developer "team_id" that
-- means something else, and "workspace" matches the user-facing
-- terminology established by the X-Workspace-Id header in T4.
ALTER TABLE iosdist_apps         ADD COLUMN IF NOT EXISTS workspace_id TEXT REFERENCES teams(id);
ALTER TABLE iosdist_certificates ADD COLUMN IF NOT EXISTS workspace_id TEXT REFERENCES teams(id);
ALTER TABLE iosdist_profiles     ADD COLUMN IF NOT EXISTS workspace_id TEXT REFERENCES teams(id);
ALTER TABLE iosdist_asc_configs  ADD COLUMN IF NOT EXISTS workspace_id TEXT REFERENCES teams(id);
ALTER TABLE llm_configs          ADD COLUMN IF NOT EXISTS workspace_id TEXT REFERENCES teams(id);
CREATE INDEX IF NOT EXISTS idx_iosdist_apps_workspace         ON iosdist_apps(workspace_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_iosdist_certificates_workspace ON iosdist_certificates(workspace_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_iosdist_profiles_workspace     ON iosdist_profiles(workspace_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_iosdist_asc_configs_workspace  ON iosdist_asc_configs(workspace_id);
CREATE INDEX IF NOT EXISTS idx_llm_configs_workspace          ON llm_configs(workspace_id, updated_at DESC);

-- T6-B: iosdist_asc_configs flips from per-user to per-workspace.
-- Drop the legacy PK on owner_user_id (which prevented a user from
-- having ASC configs in multiple workspaces) and replace with a
-- partial unique index on workspace_id (one ASC config per
-- workspace). owner_user_id stays as audit only; T6-D will rename
-- it to creator_user_id. Partial unique because legacy rows
-- pre-T6-A backfill may still have NULL workspace_id; NULLs aren't
-- constrained by UNIQUE.
ALTER TABLE iosdist_asc_configs DROP CONSTRAINT IF EXISTS iosdist_asc_configs_pkey;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_iosdist_asc_configs_workspace
	ON iosdist_asc_configs(workspace_id) WHERE workspace_id IS NOT NULL;

-- Misc-migrate: extend workspace ownership to bot_users,
-- packtunnel_profiles, packtunnel_rule_files, video_projects.
-- Same pattern as T6-A: nullable column + index, backfill at
-- startup, NOT NULL enforced post-backfill.
-- packtunnel_rule_files mirrors iosdist_asc_configs: legacy PK on
-- owner_user_id is dropped and replaced with a partial unique
-- index on workspace_id (one rule file per workspace).
ALTER TABLE bot_users             ADD COLUMN IF NOT EXISTS workspace_id TEXT REFERENCES teams(id);
ALTER TABLE packtunnel_profiles   ADD COLUMN IF NOT EXISTS workspace_id TEXT REFERENCES teams(id);
ALTER TABLE packtunnel_rule_files ADD COLUMN IF NOT EXISTS workspace_id TEXT REFERENCES teams(id);
ALTER TABLE video_projects        ADD COLUMN IF NOT EXISTS workspace_id TEXT REFERENCES teams(id);
CREATE INDEX IF NOT EXISTS idx_bot_users_workspace             ON bot_users(workspace_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_packtunnel_profiles_workspace   ON packtunnel_profiles(workspace_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_packtunnel_rule_files_workspace ON packtunnel_rule_files(workspace_id);
CREATE INDEX IF NOT EXISTS idx_video_projects_workspace        ON video_projects(workspace_id, updated_at DESC);
ALTER TABLE packtunnel_rule_files DROP CONSTRAINT IF EXISTS packtunnel_rule_files_pkey;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_packtunnel_rule_files_workspace
	ON packtunnel_rule_files(workspace_id) WHERE workspace_id IS NOT NULL;

-- Audit-role comments mirroring the iosdist / llm tables.
COMMENT ON COLUMN bot_users.owner_user_id             IS 'audit: who created/upserted; not for authorization (use workspace_id)';
COMMENT ON COLUMN packtunnel_profiles.owner_user_id   IS 'audit: who created/upserted; not for authorization (use workspace_id)';
COMMENT ON COLUMN packtunnel_rule_files.owner_user_id IS 'audit: who created/upserted; not for authorization (use workspace_id)';
COMMENT ON COLUMN video_projects.owner_user_id        IS 'audit: who created/upserted; not for authorization (use workspace_id)';

-- T6-D: locking workspace_id to NOT NULL on the migrated tables
-- happens AFTER backfill, not here. Doing it inline in this
-- bootstrap string would race the backfill: the schema runs at
-- openDB() time, but backfillPersonalTeams + backfillResourceWorkspaceIDs
-- run later in NewServer(). On a fresh upgrade with legacy rows
-- still NULL, the inline ALTER would 23502 and crash the boot.
-- See enforceWorkspaceIDNotNull(), called after the backfill.

-- agent_tokens: long-lived bearer tokens issued to local agent
-- (polar-agent CLI) instances. token_hash stored as SHA-256 hex
-- of the raw token; raw is shown to the user once at issue time
-- and never persisted server-side. last_used_at bumped on each
-- WS connect. revoked_at non-NULL = no longer valid.
CREATE TABLE IF NOT EXISTS agent_tokens (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL DEFAULT '',
	token_hash TEXT NOT NULL UNIQUE,
	last_used_at TIMESTAMPTZ,
	revoked_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_tokens_user ON agent_tokens(user_id, created_at DESC);

-- projects: top-level container for an AI-driven product. One row =
-- "the thing being built". The chat-with-AI flow lives under it:
-- user description → decomposed features → selected features → tasks
-- (one task per selected feature, 1:1 to a chat thread when a bot
-- picks it up). Workspace-scoped from day one, no T6-style migration.
--
-- Status state machine:
--   draft    — user just created, no decompose yet
--   planning — features generated, user is selecting
--   building — tasks created, bot(s) working
--   beta     — first build out, in TF / OTA
--   shipped  — public beta or live
--   archived — done / shelved
CREATE TABLE IF NOT EXISTS projects (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	creator_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'draft',
	template TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_projects_workspace ON projects(workspace_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_projects_status ON projects(status);

-- project_features: AI-decomposed feature list. selected drives
-- which ones make it into tasks when the user clicks "生成计划".
-- Re-decomposing replaces the list (DELETE + re-INSERT inside a tx)
-- so users can iterate on the prompt without leftover stale items.
CREATE TABLE IF NOT EXISTS project_features (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	ord INT NOT NULL DEFAULT 0,
	title TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	complexity TEXT NOT NULL DEFAULT 'medium',
	selected BOOLEAN NOT NULL DEFAULT TRUE,
	sprint_number INT NOT NULL DEFAULT 1,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE project_features ADD COLUMN IF NOT EXISTS sprint_number INT NOT NULL DEFAULT 1;
CREATE INDEX IF NOT EXISTS idx_project_features_project ON project_features(project_id, ord);
CREATE INDEX IF NOT EXISTS idx_project_features_project_sprint ON project_features(project_id, sprint_number, ord);

-- project_tasks: created from selected features when the user
-- generates the plan. 1:1 to a chat_thread (chat_thread_id set
-- when a bot picks it up). Sub-table of projects but cascades on
-- feature delete? No — features can be re-decomposed, but tasks
-- are the persistent execution record; nullable feature_id
-- preserves them across re-decompose.
CREATE TABLE IF NOT EXISTS project_tasks (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	feature_id TEXT REFERENCES project_features(id) ON DELETE SET NULL,
	title TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'todo',
	bot_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
	chat_thread_id BIGINT REFERENCES chat_threads(id) ON DELETE SET NULL,
	llm_thread_id BIGINT REFERENCES llm_threads(id) ON DELETE SET NULL,
	sprint_number INT NOT NULL DEFAULT 1,
	ord INT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	started_at TIMESTAMPTZ,
	completed_at TIMESTAMPTZ
);
ALTER TABLE project_tasks
	ADD COLUMN IF NOT EXISTS llm_thread_id BIGINT REFERENCES llm_threads(id) ON DELETE SET NULL;
ALTER TABLE project_tasks
	ADD COLUMN IF NOT EXISTS sprint_number INT NOT NULL DEFAULT 1;
CREATE INDEX IF NOT EXISTS idx_project_tasks_project ON project_tasks(project_id, ord);
CREATE INDEX IF NOT EXISTS idx_project_tasks_status ON project_tasks(project_id, status);
CREATE INDEX IF NOT EXISTS idx_project_tasks_project_sprint ON project_tasks(project_id, sprint_number, ord);

-- prompt_templates: site-wide admin-managed prompt strings used
-- across the platform (AI 拆解、pickup body、markdown/post 助手、
-- AI default system prompt, tool-use addendum, etc.). slug is the
-- stable identifier referenced by code; content is the live
-- editable text (Go text/template syntax); default_content is the
-- code-baked default — refreshed on every server start so admins
-- can see "what code thinks the default is" and reset to it. Only
-- the editable content column is preserved across deploys.
CREATE TABLE IF NOT EXISTS prompt_templates (
	slug TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL DEFAULT '',
	default_content TEXT NOT NULL DEFAULT '',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- logs_entries: append-only structured log table. Spans three
-- scopes: site-wide (workspace_id NULL — admin-only readable),
-- workspace (workspace_id set — readable by team members), and
-- by category. category is the high-level bucket; actor_kind +
-- subject_kind/_id let viewers filter by who did something to
-- what. details is a JSONB blob for whatever the writer wants
-- to record beyond the plain message — never query-load this
-- in hot paths, only for detail views.
--
-- Categories used by the platform (extend over time, no enum):
--   sign         — iosdist sign worker transitions
--   team_audit   — workspace-level events (member changes,
--                  invitations, ownership transfer, deletion)
--   deploy       — video render / external deploy outcomes
--   agent        — AI agent / bot tool calls and completions
--   server       — server-side opportunistic errors (replaces
--                  / supplements log.Printf at notable spots)
CREATE TABLE IF NOT EXISTS logs_entries (
	id BIGSERIAL PRIMARY KEY,
	category TEXT NOT NULL,
	level TEXT NOT NULL DEFAULT 'info',
	source TEXT NOT NULL DEFAULT '',
	workspace_id TEXT REFERENCES teams(id) ON DELETE CASCADE,
	actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
	actor_kind TEXT NOT NULL DEFAULT 'user',
	subject_kind TEXT NOT NULL DEFAULT '',
	subject_id TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT '',
	details JSONB NOT NULL DEFAULT '{}'::jsonb,
	request_id TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Workspace-scoped reads (team members) — partial index drops the
-- site-wide rows so the index stays tight.
CREATE INDEX IF NOT EXISTS idx_logs_entries_ws_ts
	ON logs_entries(workspace_id, created_at DESC, id DESC)
	WHERE workspace_id IS NOT NULL;
-- Site-wide reads (admin only).
CREATE INDEX IF NOT EXISTS idx_logs_entries_site_ts
	ON logs_entries(created_at DESC, id DESC)
	WHERE workspace_id IS NULL;
-- Filter by category (cheap; covers both scopes).
CREATE INDEX IF NOT EXISTS idx_logs_entries_category_ts
	ON logs_entries(category, created_at DESC, id DESC);
-- "What did user X do?" — actor breakdown.
CREATE INDEX IF NOT EXISTS idx_logs_entries_actor_ts
	ON logs_entries(actor_user_id, created_at DESC)
	WHERE actor_user_id IS NOT NULL;
-- "Show me everything that touched this resource."
CREATE INDEX IF NOT EXISTS idx_logs_entries_subject
	ON logs_entries(subject_kind, subject_id, created_at DESC);

-- The owner_user_id column (still NOT NULL) keeps an in-DB
-- comment as a hint that its semantics changed in T6: it's now
-- the audit "who created / last upserted this row" field, not
-- the ownership / authorization column. Authorization queries
-- should always be WHERE workspace_id = $X. The column is left
-- in place — no rename — to avoid the cross-cutting touch that
-- would entail (struct fields, JSON tags, frontend types). A
-- future cleanup PR may rename it to creator_user_id.
COMMENT ON COLUMN iosdist_apps.owner_user_id         IS 'audit: who created/upserted; not for authorization (use workspace_id)';
COMMENT ON COLUMN iosdist_certificates.owner_user_id IS 'audit: who created/upserted; not for authorization (use workspace_id)';
COMMENT ON COLUMN iosdist_profiles.owner_user_id     IS 'audit: who created/upserted; not for authorization (use workspace_id)';
COMMENT ON COLUMN iosdist_asc_configs.owner_user_id  IS 'audit: who created/upserted; not for authorization (use workspace_id)';
COMMENT ON COLUMN llm_configs.owner_user_id          IS 'audit: who created/upserted; not for authorization (use workspace_id)';
`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

// 生成随机 Session ID
func generateSessionID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}

func generateResourceID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) createLoginRecord(record *LoginRecord) error {
	if record == nil {
		return errors.New("login record is nil")
	}
	_, err := s.db.Exec(
		`INSERT INTO login_records (user_id, ip_address, country, region, city, login_method, device_type, logged_in_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		record.UserID,
		record.IPAddress,
		record.Country,
		record.Region,
		record.City,
		record.LoginMethod,
		record.DeviceType,
		record.LoggedInAt,
	)
	return err
}

func (s *Server) listLoginRecords(userID string, limit int) ([]LoginRecord, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(
		`SELECT id, user_id, ip_address, country, region, city, login_method, device_type, logged_in_at
		 FROM login_records
		 WHERE user_id = $1
		 ORDER BY logged_in_at DESC
		 LIMIT $2`,
		userID,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := make([]LoginRecord, 0, limit)
	for rows.Next() {
		var record LoginRecord
		if err := rows.Scan(
			&record.ID,
			&record.UserID,
			&record.IPAddress,
			&record.Country,
			&record.Region,
			&record.City,
			&record.LoginMethod,
			&record.DeviceType,
			&record.LoggedInAt,
		); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func (s *Server) listUsersForAdmin(query string, limit, offset int) ([]AdminUserSummary, int, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	filter := "%"
	if trimmed := strings.TrimSpace(query); trimmed != "" {
		filter = "%" + trimmed + "%"
	}

	var total int
	if err := s.db.QueryRow(
		`SELECT COUNT(*)
		   FROM users
		  WHERE id <> $1
		    AND role <> 'admin'
		    AND NOT EXISTS (
		      SELECT 1
		        FROM bot_users b
		       WHERE b.bot_user_id = users.id
		    )
		    AND ($2 = '%' OR id ILIKE $2 OR username ILIKE $2 OR email ILIKE $2)`,
		systemUserID,
		filter,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(
		`SELECT id, username, email, role, is_online, last_active_device_type, last_seen_at, created_at
		   FROM users
		  WHERE id <> $1
		    AND role <> 'admin'
		    AND NOT EXISTS (
		      SELECT 1
		        FROM bot_users b
		       WHERE b.bot_user_id = users.id
		    )
		    AND ($2 = '%' OR id ILIKE $2 OR username ILIKE $2 OR email ILIKE $2)
		  ORDER BY created_at DESC, id DESC
		  LIMIT $3 OFFSET $4`,
		systemUserID,
		filter,
		limit,
		offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]AdminUserSummary, 0, limit)
	for rows.Next() {
		var item AdminUserSummary
		var lastSeenAt sql.NullTime
		if err := rows.Scan(
			&item.ID,
			&item.Username,
			&item.Email,
			&item.Role,
			&item.IsOnline,
			&item.DeviceType,
			&lastSeenAt,
			&item.CreatedAt,
		); err != nil {
			return nil, 0, err
		}
		if lastSeenAt.Valid {
			item.LastSeenAt = &lastSeenAt.Time
		}
		item.DeviceType = normalizeDeviceType(item.DeviceType)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (s *Server) updateUserPasswordHash(userID, passwordHash string) (bool, error) {
	result, err := s.db.Exec(`UPDATE users SET password_hash = $2 WHERE id = $1`, userID, passwordHash)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func hashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

func checkPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

func (s *Server) getUserByEmail(email string) (*User, error) {
	var user User
	var lastSeenAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, username, email, email_verified, password_hash, role, bio, icon_url, is_online, last_active_device_type, last_seen_at, created_at FROM users WHERE email = $1`,
		email,
	).Scan(&user.ID, &user.Username, &user.Email, &user.EmailVerified, &user.Password, &user.Role, &user.Bio, &user.IconURL, &user.IsOnline, &user.DeviceType, &lastSeenAt, &user.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if lastSeenAt.Valid {
		user.LastSeenAt = &lastSeenAt.Time
	}
	return &user, nil
}

func (s *Server) getUserByID(userID string) (*User, error) {
	var user User
	var lastSeenAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, username, email, email_verified, password_hash, role, bio, icon_url, is_online, last_active_device_type, last_seen_at, created_at FROM users WHERE id = $1`,
		userID,
	).Scan(&user.ID, &user.Username, &user.Email, &user.EmailVerified, &user.Password, &user.Role, &user.Bio, &user.IconURL, &user.IsOnline, &user.DeviceType, &lastSeenAt, &user.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if lastSeenAt.Valid {
		user.LastSeenAt = &lastSeenAt.Time
	}
	return &user, nil
}

func (s *Server) getUserByUsername(username string) (*User, error) {
	var user User
	var lastSeenAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, username, email, email_verified, password_hash, role, bio, icon_url, is_online, last_active_device_type, last_seen_at, created_at FROM users WHERE username = $1`,
		username,
	).Scan(&user.ID, &user.Username, &user.Email, &user.EmailVerified, &user.Password, &user.Role, &user.Bio, &user.IconURL, &user.IsOnline, &user.DeviceType, &lastSeenAt, &user.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if lastSeenAt.Valid {
		user.LastSeenAt = &lastSeenAt.Time
	}
	return &user, nil
}

func (s *Server) listLLMConfigs(workspaceID string) ([]LLMConfig, error) {
	rows, err := s.db.Query(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), share_id, shared, name, base_url, model, system_prompt, streaming, (api_key <> '') AS has_api_key, COALESCE(proxy_url, ''), created_at, updated_at
		   FROM llm_configs
		  WHERE workspace_id = $1
		  ORDER BY updated_at DESC, id DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]LLMConfig, 0)
	for rows.Next() {
		var item LLMConfig
		if err := rows.Scan(&item.ID, &item.OwnerUserID, &item.WorkspaceID, &item.ShareID, &item.Shared, &item.Name, &item.BaseURL, &item.Model, &item.SystemPrompt, &item.Streaming, &item.HasAPIKey, &item.ProxyURL, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Server) getLLMConfigForOwner(workspaceID string, id int64) (*LLMConfig, string, error) {
	var item LLMConfig
	var apiKey string
	err := s.db.QueryRow(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), share_id, shared, name, base_url, model, api_key, system_prompt, streaming, (api_key <> '') AS has_api_key, COALESCE(proxy_url, ''), created_at, updated_at
		   FROM llm_configs
		  WHERE id = $1 AND workspace_id = $2`,
		id,
		workspaceID,
	).Scan(&item.ID, &item.OwnerUserID, &item.WorkspaceID, &item.ShareID, &item.Shared, &item.Name, &item.BaseURL, &item.Model, &apiKey, &item.SystemPrompt, &item.Streaming, &item.HasAPIKey, &item.ProxyURL, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", err
	}
	return &item, apiKey, nil
}

func (s *Server) getLLMConfigForBot(botUserID string) (*LLMConfig, string, error) {
	var item LLMConfig
	var apiKey string
	err := s.db.QueryRow(
		`SELECT c.id, c.owner_user_id, COALESCE(c.workspace_id, ''), c.share_id, c.shared, c.name, c.base_url, c.model, c.api_key, c.system_prompt, c.streaming, (c.api_key <> '') AS has_api_key, COALESCE(c.proxy_url, ''), c.created_at, c.updated_at
		   FROM bot_users b
		   JOIN llm_configs c ON c.id = b.llm_config_id
		  WHERE b.bot_user_id = $1`,
		botUserID,
	).Scan(&item.ID, &item.OwnerUserID, &item.WorkspaceID, &item.ShareID, &item.Shared, &item.Name, &item.BaseURL, &item.Model, &apiKey, &item.SystemPrompt, &item.Streaming, &item.HasAPIKey, &item.ProxyURL, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", err
	}
	return &item, apiKey, nil
}

// getAvailableLLMConfigWithAPIKey resolves a chat-side LLM config the
// caller can use. The workspace-scope filter is "your workspace's
// configs OR any shared config" — `shared = TRUE` rows cross workspace
// boundaries (matches the legacy cross-user sharing semantic). Also
// filters out video-kind rows so a forged config_id (someone pasting a
// Seedance id into chat-start) can't bypass list-side filtering.
func (s *Server) getAvailableLLMConfigWithAPIKey(workspaceID string, id int64) (*LLMConfig, string, error) {
	var item LLMConfig
	var apiKey string
	err := s.db.QueryRow(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), share_id, shared, name, base_url, model, api_key, system_prompt, streaming, (api_key <> '') AS has_api_key, COALESCE(proxy_url, ''), created_at, updated_at
		   FROM llm_configs
		  WHERE id = $1
		    AND (workspace_id = $2 OR shared = TRUE OR is_platform = TRUE)
		    AND (provider_kind = 'text' OR provider_kind = '' OR provider_kind IS NULL)`,
		id,
		workspaceID,
	).Scan(&item.ID, &item.OwnerUserID, &item.WorkspaceID, &item.ShareID, &item.Shared, &item.Name, &item.BaseURL, &item.Model, &apiKey, &item.SystemPrompt, &item.Streaming, &item.HasAPIKey, &item.ProxyURL, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", err
	}
	return &item, apiKey, nil
}

func (s *Server) createLLMConfig(workspaceID, ownerUserID, name, baseURL, model, apiKey, systemPrompt, shareID, proxyURL string, shared, streaming bool, now time.Time) (*LLMConfig, error) {
	var item LLMConfig
	err := s.db.QueryRow(
		`INSERT INTO llm_configs (owner_user_id, workspace_id, share_id, shared, name, base_url, model, api_key, system_prompt, streaming, proxy_url, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
		 RETURNING id, owner_user_id, COALESCE(workspace_id, ''), share_id, shared, name, base_url, model, system_prompt, streaming, (api_key <> '') AS has_api_key, COALESCE(proxy_url, ''), created_at, updated_at`,
		ownerUserID, workspaceID, shareID, shared, name, baseURL, model, apiKey, systemPrompt, streaming, proxyURL, now,
	).Scan(&item.ID, &item.OwnerUserID, &item.WorkspaceID, &item.ShareID, &item.Shared, &item.Name, &item.BaseURL, &item.Model, &item.SystemPrompt, &item.Streaming, &item.HasAPIKey, &item.ProxyURL, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Server) updateLLMConfig(workspaceID string, id int64, name, baseURL, model, apiKey, systemPrompt, proxyURL string, shared, streaming, replaceAPIKey bool, now time.Time) (*LLMConfig, error) {
	query := `UPDATE llm_configs
	             SET name = $3,
	                 base_url = $4,
	                 model = $5,
	                 system_prompt = $6,
	                 shared = $7,
	                 streaming = $8,
	                 proxy_url = $9,
	                 updated_at = $10`
	args := []any{id, workspaceID, name, baseURL, model, systemPrompt, shared, streaming, proxyURL, now}
	if replaceAPIKey {
		query += `, api_key = $11`
		args = append(args, apiKey)
	}
	query += ` WHERE id = $1 AND workspace_id = $2
	           RETURNING id, owner_user_id, COALESCE(workspace_id, ''), share_id, shared, name, base_url, model, system_prompt, streaming, (api_key <> '') AS has_api_key, COALESCE(proxy_url, ''), created_at, updated_at`
	var item LLMConfig
	err := s.db.QueryRow(query, args...).Scan(&item.ID, &item.OwnerUserID, &item.WorkspaceID, &item.ShareID, &item.Shared, &item.Name, &item.BaseURL, &item.Model, &item.SystemPrompt, &item.Streaming, &item.HasAPIKey, &item.ProxyURL, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (s *Server) deleteLLMConfig(workspaceID string, id int64) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM llm_configs WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// listAvailableLLMConfigs returns the workspace's own configs + shared
// configs from any workspace, but **only text-kind** ones — chat / bot
// pickers must not surface video providers (Seedance et al.) or the
// user gets confused when their bot can't reply. Empty provider_kind
// is treated as 'text' so existing rows pre-migration keep working.
func (s *Server) listAvailableLLMConfigs(workspaceID string) ([]LLMConfig, error) {
	// Order: workspace's own > platform defaults > shared from
	// other workspaces. The sort puts workspace_id-match first
	// (DESC=true first), then is_platform=TRUE among the rest, so
	// the bot picker surfaces my-team's first, falling back to the
	// platform default below.
	rows, err := s.db.Query(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), share_id, shared, is_platform, name, base_url, model, system_prompt, streaming, (api_key <> '') AS has_api_key, COALESCE(proxy_url, ''), created_at, updated_at
		   FROM llm_configs
		  WHERE (workspace_id = $1 OR shared = TRUE OR is_platform = TRUE)
		    AND (provider_kind = 'text' OR provider_kind = '' OR provider_kind IS NULL)
		  ORDER BY (workspace_id = $1) DESC, is_platform DESC, updated_at DESC, id DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]LLMConfig, 0)
	for rows.Next() {
		var item LLMConfig
		if err := rows.Scan(&item.ID, &item.OwnerUserID, &item.WorkspaceID, &item.ShareID, &item.Shared, &item.IsPlatform, &item.Name, &item.BaseURL, &item.Model, &item.SystemPrompt, &item.Streaming, &item.HasAPIKey, &item.ProxyURL, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Server) getLLMConfigByShareID(shareID string) (*LLMConfig, error) {
	var item LLMConfig
	err := s.db.QueryRow(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), share_id, shared, name, base_url, model, system_prompt, streaming, (api_key <> '') AS has_api_key, COALESCE(proxy_url, ''), created_at, updated_at
		   FROM llm_configs
		  WHERE share_id = $1 AND share_id <> ''`,
		shareID,
	).Scan(&item.ID, &item.OwnerUserID, &item.WorkspaceID, &item.ShareID, &item.Shared, &item.Name, &item.BaseURL, &item.Model, &item.SystemPrompt, &item.Streaming, &item.HasAPIKey, &item.ProxyURL, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (s *Server) listPackTunnelProfiles(workspaceID string) ([]PackTunnelProfile, error) {
	rows, err := s.db.Query(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), name, profile_type, server_config, auth_config, options_config, transport_config,
		        priority, enabled, editable, source, country_code, country_flag, is_active, created_at, updated_at
		   FROM packtunnel_profiles
		  WHERE workspace_id = $1
		  ORDER BY is_active DESC, priority DESC, updated_at DESC, id DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]PackTunnelProfile, 0)
	for rows.Next() {
		item, err := scanPackTunnelProfile(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func (s *Server) getPackTunnelProfile(workspaceID, id string) (*PackTunnelProfile, error) {
	item, err := scanPackTunnelProfile(s.db.QueryRow(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), name, profile_type, server_config, auth_config, options_config, transport_config,
		        priority, enabled, editable, source, country_code, country_flag, is_active, created_at, updated_at
		   FROM packtunnel_profiles
		  WHERE workspace_id = $1 AND id = $2`,
		workspaceID,
		id,
	).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return item, nil
}

func (s *Server) getActivePackTunnelProfile(workspaceID string) (*PackTunnelProfile, error) {
	item, err := scanPackTunnelProfile(s.db.QueryRow(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), name, profile_type, server_config, auth_config, options_config, transport_config,
		        priority, enabled, editable, source, country_code, country_flag, is_active, created_at, updated_at
		   FROM packtunnel_profiles
		  WHERE workspace_id = $1 AND is_active = TRUE`,
		workspaceID,
	).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return item, nil
}

func (s *Server) createPackTunnelProfile(workspaceID, ownerUserID string, item PackTunnelProfile, now time.Time) (*PackTunnelProfile, error) {
	if strings.TrimSpace(item.ID) == "" {
		item.ID = generateResourceID()
	}

	serverJSON, authJSON, optionsJSON, transportJSON, err := marshalPackTunnelProfile(item)
	if err != nil {
		return nil, err
	}

	if item.Metadata.IsActive {
		if _, err := s.db.Exec(`UPDATE packtunnel_profiles SET is_active = FALSE, updated_at = $2 WHERE workspace_id = $1 AND is_active = TRUE`, workspaceID, now); err != nil {
			return nil, err
		}
	}

	created, err := scanPackTunnelProfile(s.db.QueryRow(
		`INSERT INTO packtunnel_profiles (
			id, owner_user_id, workspace_id, name, profile_type, server_config, auth_config, options_config, transport_config,
			priority, enabled, editable, source, country_code, country_flag, is_active, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14, $15, $16, $17, $17
		)
		RETURNING id, owner_user_id, COALESCE(workspace_id, ''), name, profile_type, server_config, auth_config, options_config, transport_config,
		          priority, enabled, editable, source, country_code, country_flag, is_active, created_at, updated_at`,
		item.ID, ownerUserID, workspaceID, item.Name, item.Type, serverJSON, authJSON, optionsJSON, transportJSON,
		item.Metadata.Priority, item.Metadata.Enabled, item.Metadata.Editable, item.Metadata.Source,
		item.Metadata.CountryCode, item.Metadata.CountryFlag, item.Metadata.IsActive, now,
	).Scan)
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *Server) updatePackTunnelProfile(workspaceID, id string, item PackTunnelProfile, now time.Time) (*PackTunnelProfile, error) {
	serverJSON, authJSON, optionsJSON, transportJSON, err := marshalPackTunnelProfile(item)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if item.Metadata.IsActive {
		if _, err := tx.Exec(`UPDATE packtunnel_profiles SET is_active = FALSE, updated_at = $2 WHERE workspace_id = $1 AND id <> $3 AND is_active = TRUE`, workspaceID, now, id); err != nil {
			return nil, err
		}
	}

	updated, err := scanPackTunnelProfile(tx.QueryRow(
		`UPDATE packtunnel_profiles
		    SET name = $3,
		        profile_type = $4,
		        server_config = $5,
		        auth_config = $6,
		        options_config = $7,
		        transport_config = $8,
		        priority = $9,
		        enabled = $10,
		        editable = $11,
		        source = $12,
		        country_code = $13,
		        country_flag = $14,
		        is_active = $15,
		        updated_at = $16
		  WHERE workspace_id = $1 AND id = $2
		RETURNING id, owner_user_id, COALESCE(workspace_id, ''), name, profile_type, server_config, auth_config, options_config, transport_config,
		          priority, enabled, editable, source, country_code, country_flag, is_active, created_at, updated_at`,
		workspaceID, id, item.Name, item.Type, serverJSON, authJSON, optionsJSON, transportJSON,
		item.Metadata.Priority, item.Metadata.Enabled, item.Metadata.Editable, item.Metadata.Source,
		item.Metadata.CountryCode, item.Metadata.CountryFlag, item.Metadata.IsActive, now,
	).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Server) deletePackTunnelProfile(workspaceID, id string) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM packtunnel_profiles WHERE workspace_id = $1 AND id = $2`, workspaceID, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *Server) setActivePackTunnelProfile(workspaceID, id string, now time.Time) (*PackTunnelProfile, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE packtunnel_profiles SET is_active = FALSE, updated_at = $2 WHERE workspace_id = $1 AND is_active = TRUE`, workspaceID, now); err != nil {
		return nil, err
	}

	item, err := scanPackTunnelProfile(tx.QueryRow(
		`UPDATE packtunnel_profiles
		    SET is_active = TRUE, updated_at = $3
		  WHERE workspace_id = $1 AND id = $2
		RETURNING id, owner_user_id, COALESCE(workspace_id, ''), name, profile_type, server_config, auth_config, options_config, transport_config,
		          priority, enabled, editable, source, country_code, country_flag, is_active, created_at, updated_at`,
		workspaceID,
		id,
		now,
	).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return item, nil
}

// upsertPackTunnelRuleFile upserts the workspace's single rule file
// row. T6-style flip: keyed on workspace_id (one rule file per
// workspace), owner_user_id stays as audit only.
func (s *Server) upsertPackTunnelRuleFile(item PackTunnelRuleFile) error {
	_, err := s.db.Exec(
		`INSERT INTO packtunnel_rule_files (owner_user_id, workspace_id, file_name, stored_name, file_path, file_size, content_type, uploaded_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (workspace_id) WHERE workspace_id IS NOT NULL DO UPDATE
		     SET owner_user_id = EXCLUDED.owner_user_id,
		         file_name = EXCLUDED.file_name,
		         stored_name = EXCLUDED.stored_name,
		         file_path = EXCLUDED.file_path,
		         file_size = EXCLUDED.file_size,
		         content_type = EXCLUDED.content_type,
		         uploaded_at = EXCLUDED.uploaded_at`,
		item.UserID,
		item.WorkspaceID,
		item.FileName,
		item.StoredName,
		item.FilePath,
		item.Size,
		item.ContentType,
		item.UploadedAt,
	)
	return err
}

func (s *Server) getPackTunnelRuleFile(workspaceID string) (*PackTunnelRuleFile, error) {
	var item PackTunnelRuleFile
	err := s.db.QueryRow(
		`SELECT owner_user_id, COALESCE(workspace_id, ''), file_name, stored_name, file_path, file_size, content_type, uploaded_at
		   FROM packtunnel_rule_files
		  WHERE workspace_id = $1`,
		workspaceID,
	).Scan(
		&item.UserID,
		&item.WorkspaceID,
		&item.FileName,
		&item.StoredName,
		&item.FilePath,
		&item.Size,
		&item.ContentType,
		&item.UploadedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (s *Server) deletePackTunnelRuleFile(workspaceID string) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM packtunnel_rule_files WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func scanPackTunnelProfile(scan func(dest ...any) error) (*PackTunnelProfile, error) {
	var (
		item          PackTunnelProfile
		serverJSON    []byte
		authJSON      []byte
		optionsJSON   []byte
		transportJSON []byte
	)
	err := scan(
		&item.ID,
		&item.UserID,
		&item.WorkspaceID,
		&item.Name,
		&item.Type,
		&serverJSON,
		&authJSON,
		&optionsJSON,
		&transportJSON,
		&item.Metadata.Priority,
		&item.Metadata.Enabled,
		&item.Metadata.Editable,
		&item.Metadata.Source,
		&item.Metadata.CountryCode,
		&item.Metadata.CountryFlag,
		&item.Metadata.IsActive,
		&item.CreatedAt,
		&item.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	if err := decodePackTunnelJSON(serverJSON, &item.Server); err != nil {
		return nil, err
	}
	if err := decodePackTunnelJSON(authJSON, &item.Auth); err != nil {
		return nil, err
	}
	if err := decodePackTunnelJSON(optionsJSON, &item.Options); err != nil {
		return nil, err
	}
	if len(transportJSON) > 0 && string(transportJSON) != "null" {
		var transport PackTunnelTransport
		if err := decodePackTunnelJSON(transportJSON, &transport); err != nil {
			return nil, err
		}
		item.Transport = &transport
	}

	return &item, nil
}

func marshalPackTunnelProfile(item PackTunnelProfile) (string, string, string, any, error) {
	serverJSON, err := json.Marshal(item.Server)
	if err != nil {
		return "", "", "", nil, err
	}
	authJSON, err := json.Marshal(item.Auth)
	if err != nil {
		return "", "", "", nil, err
	}
	optionsJSON, err := json.Marshal(item.Options)
	if err != nil {
		return "", "", "", nil, err
	}
	var transportJSON any
	if item.Transport != nil {
		raw, marshalErr := json.Marshal(item.Transport)
		if marshalErr != nil {
			return "", "", "", nil, marshalErr
		}
		transportJSON = string(raw)
	} else {
		transportJSON = nil
	}
	return string(serverJSON), string(authJSON), string(optionsJSON), transportJSON, nil
}

func decodePackTunnelJSON(raw []byte, dest any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, dest)
}

// botUserSelectColumns is the canonical SELECT projection used by
// every bot_users read. Joined with llm_configs via LEFT JOIN
// (passthrough bots may not have an llm_config_id), so c.name is
// scanned through sql.NullString.
const botUserSelectColumns = `b.id, b.owner_user_id, COALESCE(b.workspace_id, ''), b.bot_user_id, b.name, b.description, b.system_prompt, b.llm_config_id, c.name, COALESCE(b.bot_kind, 'classic'), COALESCE(b.preferred_tool, ''), b.created_at, b.updated_at`

// scanBotUser reads one row from a query that selected
// botUserSelectColumns. nullable scan targets handle passthrough
// bots whose llm_config_id (and therefore c.name) is NULL.
func scanBotUser(scanFn func(...any) error) (*BotUser, error) {
	var item BotUser
	var llmConfigID sql.NullInt64
	var configName sql.NullString
	err := scanFn(
		&item.ID, &item.OwnerUserID, &item.WorkspaceID, &item.BotUserID,
		&item.Name, &item.Description, &item.SystemPrompt,
		&llmConfigID, &configName,
		&item.BotKind, &item.PreferredTool,
		&item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if llmConfigID.Valid {
		item.LLMConfigID = llmConfigID.Int64
	}
	if configName.Valid {
		item.ConfigName = configName.String
	}
	return &item, nil
}

func (s *Server) listBotUsers(workspaceID string) ([]BotUser, error) {
	rows, err := s.db.Query(
		`SELECT `+botUserSelectColumns+`
		   FROM bot_users b
		   LEFT JOIN llm_configs c ON c.id = b.llm_config_id
		  WHERE b.workspace_id = $1
		  ORDER BY b.updated_at DESC, b.id DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]BotUser, 0)
	for rows.Next() {
		item, err := scanBotUser(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func (s *Server) ensureBuiltinBotUsers(workspaceID, ownerUserID string, now time.Time) error {
	configs, err := s.listAvailableLLMConfigs(workspaceID)
	if err != nil {
		return err
	}
	if len(configs) == 0 {
		return nil
	}
	defaultLLMConfigID := configs[0].ID

	rows, err := s.db.Query(`SELECT name FROM bot_users WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return err
	}
	defer rows.Close()

	existingNames := make(map[string]bool, len(builtinBotPresets))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		existingNames[strings.TrimSpace(name)] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, preset := range builtinBotPresets {
		if existingNames[preset.Name] {
			continue
		}
		if _, err := s.createBotUser(CreateBotUserInput{
			WorkspaceID:  workspaceID,
			OwnerUserID:  ownerUserID,
			Name:         preset.Name,
			Description:  preset.Description,
			SystemPrompt: preset.SystemPrompt,
			LLMConfigID:  defaultLLMConfigID,
			BotKind:      BotKindClassic,
		}, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) getBotUserForOwner(workspaceID string, id int64) (*BotUser, error) {
	row := s.db.QueryRow(
		`SELECT `+botUserSelectColumns+`
		   FROM bot_users b
		   LEFT JOIN llm_configs c ON c.id = b.llm_config_id
		  WHERE b.id = $1 AND b.workspace_id = $2`,
		id, workspaceID,
	)
	item, err := scanBotUser(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return item, nil
}

// CreateBotUserInput captures the editable fields for a new bot.
// Kept as a struct (vs positional args) because passthrough adds
// 2 more knobs and the call sites would otherwise grow ugly.
type CreateBotUserInput struct {
	WorkspaceID   string
	OwnerUserID   string
	Name          string
	Description   string
	SystemPrompt  string
	LLMConfigID   int64  // 0 allowed when BotKind = passthrough
	BotKind       string // "" → "classic"; "passthrough" → llm_config_id may be 0
	PreferredTool string // freeform hint; "kimi"/"claude"/"codex" recommended
}

func (s *Server) createBotUser(in CreateBotUserInput, now time.Time) (*BotUser, error) {
	if in.BotKind == "" {
		in.BotKind = BotKindClassic
	}
	if in.BotKind != BotKindClassic && in.BotKind != BotKindPassthrough {
		return nil, fmt.Errorf("invalid bot_kind %q", in.BotKind)
	}
	if in.BotKind == BotKindClassic && in.LLMConfigID <= 0 {
		return nil, errors.New("classic bots require llm_config_id")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Validate the LLM config exists in this workspace when one is
	// supplied. Passthrough bots may legitimately have id=0; we
	// skip the validation in that case.
	if in.LLMConfigID > 0 {
		var name string
		err = tx.QueryRow(`SELECT name FROM llm_configs WHERE id = $1 AND (workspace_id = $2 OR shared = TRUE OR is_platform = TRUE)`, in.LLMConfigID, in.WorkspaceID).Scan(&name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
	}

	botUserID := "bot_" + generateSessionID()[:16]
	botEmail := botUserID + "@local.polar"
	password, err := hashPassword(generateSessionID())
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(
		`INSERT INTO users (id, username, email, password_hash, role, bio, icon_url, is_online, last_active_device_type, last_seen_at, created_at)
		 VALUES ($1, $2, $3, $4, 'bot', $5, '', FALSE, 'browser', NULL, $6)`,
		botUserID, in.Name, botEmail, password, in.Description, now,
	); err != nil {
		return nil, err
	}

	var llmConfigArg any
	if in.LLMConfigID > 0 {
		llmConfigArg = in.LLMConfigID
	} else {
		llmConfigArg = nil
	}
	if _, err = tx.Exec(
		`INSERT INTO bot_users (owner_user_id, workspace_id, bot_user_id, name, description, system_prompt, llm_config_id, bot_kind, preferred_tool, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)`,
		in.OwnerUserID, in.WorkspaceID, botUserID, in.Name, in.Description, in.SystemPrompt, llmConfigArg, in.BotKind, in.PreferredTool, now,
	); err != nil {
		return nil, err
	}

	if err = tx.Commit(); err != nil {
		return nil, err
	}
	// Re-read via the canonical projection so callers get the full
	// BotUser shape (including ConfigName from the LEFT JOIN).
	row := s.db.QueryRow(
		`SELECT `+botUserSelectColumns+`
		   FROM bot_users b
		   LEFT JOIN llm_configs c ON c.id = b.llm_config_id
		  WHERE b.bot_user_id = $1`,
		botUserID,
	)
	return scanBotUser(row.Scan)
}

// UpdateBotUserInput mirrors CreateBotUserInput for the edit path.
// Each field is required (no partial patches); the caller is the
// bots-page edit form which submits the full row anyway.
type UpdateBotUserInput struct {
	Name          string
	Description   string
	SystemPrompt  string
	LLMConfigID   int64
	BotKind       string
	PreferredTool string
}

func (s *Server) updateBotUser(workspaceID string, id int64, in UpdateBotUserInput, now time.Time) (*BotUser, error) {
	if in.BotKind == "" {
		in.BotKind = BotKindClassic
	}
	if in.BotKind != BotKindClassic && in.BotKind != BotKindPassthrough {
		return nil, fmt.Errorf("invalid bot_kind %q", in.BotKind)
	}
	if in.BotKind == BotKindClassic && in.LLMConfigID <= 0 {
		return nil, errors.New("classic bots require llm_config_id")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var botUserID string
	err = tx.QueryRow(`SELECT bot_user_id FROM bot_users WHERE id = $1 AND workspace_id = $2`, id, workspaceID).Scan(&botUserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if in.LLMConfigID > 0 {
		var name string
		err = tx.QueryRow(`SELECT name FROM llm_configs WHERE id = $1 AND (workspace_id = $2 OR shared = TRUE OR is_platform = TRUE)`, in.LLMConfigID, workspaceID).Scan(&name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
	}
	if _, err = tx.Exec(`UPDATE users SET username = $1, bio = $2 WHERE id = $3`, in.Name, in.Description, botUserID); err != nil {
		return nil, err
	}
	var llmConfigArg any
	if in.LLMConfigID > 0 {
		llmConfigArg = in.LLMConfigID
	} else {
		llmConfigArg = nil
	}
	if _, err = tx.Exec(
		`UPDATE bot_users
		    SET name = $3, description = $4, system_prompt = $5, llm_config_id = $6, bot_kind = $7, preferred_tool = $8, updated_at = $9
		  WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID, in.Name, in.Description, in.SystemPrompt, llmConfigArg, in.BotKind, in.PreferredTool, now,
	); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	row := s.db.QueryRow(
		`SELECT `+botUserSelectColumns+`
		   FROM bot_users b
		   LEFT JOIN llm_configs c ON c.id = b.llm_config_id
		  WHERE b.id = $1 AND b.workspace_id = $2`,
		id, workspaceID,
	)
	return scanBotUser(row.Scan)
}

func (s *Server) deleteBotUser(workspaceID string, id int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var botUserID string
	err = tx.QueryRow(`SELECT bot_user_id FROM bot_users WHERE id = $1 AND workspace_id = $2`, id, workspaceID).Scan(&botUserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if _, err = tx.Exec(`DELETE FROM bot_users WHERE id = $1 AND workspace_id = $2`, id, workspaceID); err != nil {
		return false, err
	}
	if _, err = tx.Exec(`DELETE FROM users WHERE id = $1`, botUserID); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Server) getBotUserByUserID(botUserID string) (*BotUser, error) {
	row := s.db.QueryRow(
		`SELECT `+botUserSelectColumns+`
		   FROM bot_users b
		   LEFT JOIN llm_configs c ON c.id = b.llm_config_id
		  WHERE b.bot_user_id = $1`,
		botUserID,
	)
	item, err := scanBotUser(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return item, nil
}

func (s *Server) getLLMConfigForThread(chatThreadID, llmThreadID int64, botUserID string) (*LLMConfig, string, error) {
	var item LLMConfig
	var apiKey string
	err := s.db.QueryRow(
		`SELECT c.id, c.owner_user_id, c.name, c.base_url, c.model, c.api_key, c.system_prompt, c.streaming, (c.api_key <> '') AS has_api_key, COALESCE(c.proxy_url, ''), c.created_at, c.updated_at
		   FROM llm_threads t
		   LEFT JOIN bot_users b ON b.bot_user_id = t.bot_user_id
		   JOIN llm_configs c ON c.id = COALESCE(t.llm_config_id, b.llm_config_id)
		  WHERE t.id = $1 AND t.chat_thread_id = $2 AND t.bot_user_id = $3`,
		llmThreadID,
		chatThreadID,
		botUserID,
	).Scan(&item.ID, &item.OwnerUserID, &item.Name, &item.BaseURL, &item.Model, &apiKey, &item.SystemPrompt, &item.Streaming, &item.HasAPIKey, &item.ProxyURL, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", err
	}
	return &item, apiKey, nil
}

func (s *Server) createUser(user *User) error {
	if user.Role == "" {
		user.Role = "user"
	}
	if user.IconURL == "" {
		user.IconURL = ""
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`LOCK TABLE users IN EXCLUSIVE MODE`); err != nil {
		return err
	}

	role := user.Role
	if role == "user" {
		var realUserCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE id <> $1`, systemUserID).Scan(&realUserCount); err != nil {
			return err
		}
		if realUserCount == 0 {
			role = "admin"
		}
	}

	_, err = tx.Exec(
		`INSERT INTO users (id, username, email, email_verified, password_hash, role, bio, icon_url, is_online, last_active_device_type, last_seen_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		user.ID,
		user.Username,
		user.Email,
		user.EmailVerified,
		user.Password,
		role,
		user.Bio,
		user.IconURL,
		user.IsOnline,
		normalizeDeviceType(user.DeviceType),
		user.LastSeenAt,
		user.CreatedAt,
	)
	if err != nil {
		if pgErr, ok := err.(*pq.Error); ok && pgErr.Code == "23505" {
			return errEmailExists
		}
		return err
	}

	user.Role = role
	return tx.Commit()
}

func hashEmailVerificationToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func generateEmailVerificationToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (s *Server) createEmailVerificationToken(userID, email string, now time.Time, ttl time.Duration) (string, error) {
	token, err := generateEmailVerificationToken()
	if err != nil {
		return "", err
	}
	tokenHash := hashEmailVerificationToken(token)
	expiresAt := now.Add(ttl)

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`DELETE FROM email_verification_tokens
		  WHERE user_id = $1
		    AND consumed_at IS NULL`,
		userID,
	); err != nil {
		return "", err
	}

	if _, err := tx.Exec(
		`INSERT INTO email_verification_tokens (token_hash, user_id, email, expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		tokenHash, userID, email, expiresAt, now,
	); err != nil {
		return "", err
	}

	return token, tx.Commit()
}

func (s *Server) deletePendingEmailVerificationTokens(userID string) error {
	_, err := s.db.Exec(
		`DELETE FROM email_verification_tokens
		  WHERE user_id = $1
		    AND consumed_at IS NULL`,
		userID,
	)
	return err
}

func (s *Server) consumeEmailVerificationToken(token string, now time.Time) (*User, error) {
	tokenHash := hashEmailVerificationToken(token)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var row EmailVerificationToken
	err = tx.QueryRow(
		`SELECT token_hash, user_id, email, expires_at, consumed_at, created_at
		   FROM email_verification_tokens
		  WHERE token_hash = $1`,
		tokenHash,
	).Scan(&row.TokenHash, &row.UserID, &row.Email, &row.ExpiresAt, &row.ConsumedAt, &row.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	if row.ConsumedAt != nil || now.After(row.ExpiresAt) {
		return nil, nil
	}

	if _, err := tx.Exec(
		`UPDATE email_verification_tokens
		    SET consumed_at = $2
		  WHERE token_hash = $1`,
		tokenHash, now,
	); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(
		`UPDATE users
		    SET email_verified = TRUE
		  WHERE id = $1 AND email = $2`,
		row.UserID, row.Email,
	); err != nil {
		return nil, err
	}

	var user User
	var lastSeenAt sql.NullTime
	err = tx.QueryRow(
		`SELECT id, username, email, email_verified, password_hash, role, bio, icon_url, is_online, last_active_device_type, last_seen_at, created_at
		   FROM users
		  WHERE id = $1`,
		row.UserID,
	).Scan(&user.ID, &user.Username, &user.Email, &user.EmailVerified, &user.Password, &user.Role, &user.Bio, &user.IconURL, &user.IsOnline, &user.DeviceType, &lastSeenAt, &user.CreatedAt)
	if err != nil {
		return nil, err
	}
	if lastSeenAt.Valid {
		user.LastSeenAt = &lastSeenAt.Time
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Server) upsertUserDevice(userID, deviceType, pushToken string, loginAt time.Time) error {
	deviceType = normalizeDeviceType(deviceType)
	deviceID := normalizeDeviceID("", deviceType)
	pushToken = sanitizePushToken(pushToken)
	_, err := s.db.Exec(
		`INSERT INTO user_devices (user_id, device_type, device_id, push_token, push_enabled, is_online, last_login_at, last_seen_at, last_active_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, TRUE, FALSE, $5, $5, $5, $5, $5)
		 ON CONFLICT (user_id, device_type, device_id)
		 DO UPDATE SET push_token = EXCLUDED.push_token,
		               push_enabled = CASE WHEN EXCLUDED.push_token <> '' THEN TRUE ELSE user_devices.push_enabled END,
		               last_login_at = EXCLUDED.last_login_at,
		               last_seen_at = EXCLUDED.last_seen_at,
		               last_active_at = EXCLUDED.last_active_at,
		               updated_at = EXCLUDED.updated_at`,
		userID,
		deviceType,
		deviceID,
		pushToken,
		loginAt,
	)
	return err
}

func (s *Server) upsertUserDeviceWithID(userID, deviceType, deviceID, pushToken string, loginAt time.Time) error {
	deviceType = normalizeDeviceType(deviceType)
	deviceID = normalizeDeviceID(deviceID, deviceType)
	pushToken = sanitizePushToken(pushToken)
	_, err := s.db.Exec(
		`INSERT INTO user_devices (user_id, device_type, device_id, push_token, push_enabled, is_online, last_login_at, last_seen_at, last_active_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, CASE WHEN $4 <> '' THEN TRUE ELSE TRUE END, FALSE, $5, $5, $5, $5, $5)
		 ON CONFLICT (user_id, device_type, device_id)
		 DO UPDATE SET push_token = CASE WHEN EXCLUDED.push_token <> '' THEN EXCLUDED.push_token ELSE user_devices.push_token END,
		               push_enabled = CASE WHEN EXCLUDED.push_token <> '' THEN TRUE ELSE user_devices.push_enabled END,
		               last_login_at = EXCLUDED.last_login_at,
		               last_seen_at = EXCLUDED.last_seen_at,
		               last_active_at = EXCLUDED.last_active_at,
		               updated_at = EXCLUDED.updated_at`,
		userID,
		deviceType,
		deviceID,
		pushToken,
		loginAt,
	)
	return err
}

func (s *Server) updateUserDevicePresence(userID, deviceType, deviceID string, isOnline bool, seenAt time.Time) error {
	deviceType = normalizeDeviceType(deviceType)
	deviceID = normalizeDeviceID(deviceID, deviceType)
	_, err := s.db.Exec(
		`INSERT INTO user_devices (user_id, device_type, device_id, push_token, push_enabled, is_online, last_login_at, last_seen_at, last_active_at, created_at, updated_at)
		 VALUES ($1, $2, $3, '', TRUE, $4, $5, $5, $5, $5, $5)
		 ON CONFLICT (user_id, device_type, device_id)
		 DO UPDATE SET is_online = EXCLUDED.is_online,
		               last_seen_at = EXCLUDED.last_seen_at,
		               last_active_at = EXCLUDED.last_active_at,
		               updated_at = EXCLUDED.updated_at`,
		userID,
		deviceType,
		deviceID,
		isOnline,
		seenAt,
	)
	return err
}

func (s *Server) updateUserDevicePushToken(userID, deviceType, deviceID, pushToken string, now time.Time) error {
	deviceType = normalizeDeviceType(deviceType)
	deviceID = normalizeDeviceID(deviceID, deviceType)
	pushToken = sanitizePushToken(pushToken)
	_, err := s.db.Exec(
		`INSERT INTO user_devices (user_id, device_type, device_id, push_token, push_enabled, is_online, last_login_at, last_seen_at, last_active_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, CASE WHEN $4 <> '' THEN TRUE ELSE FALSE END, FALSE, $5, $5, $5, $5, $5)
		 ON CONFLICT (user_id, device_type, device_id)
		 DO UPDATE SET push_token = EXCLUDED.push_token,
		               push_enabled = CASE WHEN EXCLUDED.push_token <> '' THEN TRUE ELSE FALSE END,
		               last_seen_at = EXCLUDED.last_seen_at,
		               last_active_at = EXCLUDED.last_active_at,
		               updated_at = EXCLUDED.updated_at`,
		userID,
		deviceType,
		deviceID,
		pushToken,
		now,
	)
	return err
}

func (s *Server) clearUserDevicePushToken(userID, deviceID string, now time.Time) error {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return errors.New("device id required")
	}
	_, err := s.db.Exec(
		`UPDATE user_devices
		    SET push_token = '',
		        push_enabled = FALSE,
		        last_seen_at = $3,
		        last_active_at = $3,
		        updated_at = $3
		  WHERE user_id = $1 AND device_id = $2`,
		userID,
		deviceID,
		now,
	)
	return err
}

func (s *Server) listUserDevices(userID string) ([]UserDevice, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, device_type, device_id, push_token, push_enabled, app_version, is_online, last_login_at, last_seen_at, last_active_at, created_at, updated_at
		   FROM user_devices
		  WHERE user_id = $1
		  ORDER BY updated_at DESC, id DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]UserDevice, 0)
	for rows.Next() {
		var item UserDevice
		var lastSeenAt sql.NullTime
		var lastActiveAt sql.NullTime
		if err := rows.Scan(
			&item.ID,
			&item.UserID,
			&item.DeviceType,
			&item.DeviceID,
			&item.PushToken,
			&item.PushEnabled,
			&item.AppVersion,
			&item.IsOnline,
			&item.LastLoginAt,
			&lastSeenAt,
			&lastActiveAt,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if lastSeenAt.Valid {
			item.LastSeenAt = &lastSeenAt.Time
		}
		if lastActiveAt.Valid {
			item.LastActiveAt = &lastActiveAt.Time
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Server) listPushableUserDevices(userID string) ([]UserDevice, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, device_type, device_id, push_token, push_enabled, app_version, is_online, last_login_at, last_seen_at, last_active_at, created_at, updated_at
		   FROM user_devices
		  WHERE user_id = $1
		    AND push_enabled = TRUE
		    AND push_token <> ''
		  ORDER BY updated_at DESC, id DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]UserDevice, 0)
	for rows.Next() {
		var item UserDevice
		var lastSeenAt sql.NullTime
		var lastActiveAt sql.NullTime
		if err := rows.Scan(
			&item.ID,
			&item.UserID,
			&item.DeviceType,
			&item.DeviceID,
			&item.PushToken,
			&item.PushEnabled,
			&item.AppVersion,
			&item.IsOnline,
			&item.LastLoginAt,
			&lastSeenAt,
			&lastActiveAt,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if lastSeenAt.Valid {
			item.LastSeenAt = &lastSeenAt.Time
		}
		if lastActiveAt.Valid {
			item.LastActiveAt = &lastActiveAt.Time
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Server) createMarkdownEntry(userID, title, filePath string, isPublic bool, uploadedAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO markdown_entries (user_id, title, file_path, is_public, uploaded_at) VALUES ($1, $2, $3, $4, $5)`,
		userID,
		title,
		filePath,
		isPublic,
		uploadedAt,
	)
	return err
}

func (s *Server) createMarkdownEntryReturningID(userID, title, filePath, summary, coverURL, editorMode string, isPublic bool, uploadedAt time.Time) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO markdown_entries (user_id, title, file_path, is_public, summary, cover_url, editor_mode, uploaded_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		userID,
		title,
		filePath,
		isPublic,
		summary,
		coverURL,
		editorMode,
		uploadedAt,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Server) getMarkdownEntryByID(id int64) (*MarkdownEntry, error) {
	var entry MarkdownEntry
	err := s.db.QueryRow(
		`SELECT m.id, m.user_id, COALESCE(u.username, ''), COALESCE(u.icon_url, ''),
		        m.title, m.summary, m.cover_url, m.file_path, m.is_public, m.editor_mode, m.uploaded_at
		   FROM markdown_entries m
		   LEFT JOIN users u ON u.id = m.user_id
		  WHERE m.id = $1`,
		id,
	).Scan(&entry.ID, &entry.UserID, &entry.Username, &entry.UserIcon, &entry.Title, &entry.Summary, &entry.CoverURL, &entry.FilePath, &entry.IsPublic, &entry.EditorMode, &entry.UploadedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &entry, nil
}

func (s *Server) listWebAuthnCredentials(userID string) ([]webauthn.Credential, error) {
	rows, err := s.db.Query(
		`SELECT credential_json FROM webauthn_credentials WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var creds []webauthn.Credential
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var cred webauthn.Credential
		if err := json.Unmarshal(payload, &cred); err != nil {
			return nil, err
		}
		creds = append(creds, cred)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return creds, nil
}

func (s *Server) listWebAuthnCredentialSummaries(userID string) ([]WebAuthnCredentialSummary, error) {
	rows, err := s.db.Query(
		`SELECT credential_id, created_at, updated_at
		   FROM webauthn_credentials
		  WHERE user_id = $1
		  ORDER BY updated_at DESC, created_at DESC, credential_id DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]WebAuthnCredentialSummary, 0)
	for rows.Next() {
		var item WebAuthnCredentialSummary
		if err := rows.Scan(&item.CredentialID, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Server) upsertWebAuthnCredential(userID string, credential *webauthn.Credential) error {
	if credential == nil {
		return errors.New("credential is nil")
	}
	payload, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	credentialID := base64.RawURLEncoding.EncodeToString(credential.ID)
	_, err = s.db.Exec(
		`INSERT INTO webauthn_credentials (credential_id, user_id, credential_json, created_at, updated_at)
		 VALUES ($1, $2, $3, NOW(), NOW())
		 ON CONFLICT (credential_id)
		 DO UPDATE SET credential_json = EXCLUDED.credential_json, updated_at = NOW()`,
		credentialID,
		userID,
		payload,
	)
	return err
}

func (s *Server) deleteWebAuthnCredential(userID, credentialID string) (bool, error) {
	result, err := s.db.Exec(
		`DELETE FROM webauthn_credentials
		  WHERE credential_id = $1 AND user_id = $2`,
		credentialID,
		userID,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *Server) updateUserIcon(userID, iconURL string) error {
	_, err := s.db.Exec(
		`UPDATE users SET icon_url = $1 WHERE id = $2`,
		iconURL,
		userID,
	)
	return err
}

func (s *Server) updateUserBio(userID, bio string) error {
	_, err := s.db.Exec(
		`UPDATE users SET bio = $1 WHERE id = $2`,
		bio,
		userID,
	)
	return err
}

func (s *Server) upsertProfileRecommendation(targetUserID, authorUserID, content string, now time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO profile_recommendations (target_user_id, author_user_id, content, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $4)
		 ON CONFLICT (target_user_id, author_user_id)
		 DO UPDATE SET content = EXCLUDED.content, updated_at = EXCLUDED.updated_at`,
		targetUserID,
		authorUserID,
		content,
		now,
	)
	return err
}

func (s *Server) blockUser(blockerUserID, blockedUserID string, now time.Time) error {
	if blockerUserID == blockedUserID {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO user_blocks (blocker_user_id, blocked_user_id, created_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (blocker_user_id, blocked_user_id) DO NOTHING`,
		blockerUserID,
		blockedUserID,
		now,
	)
	return err
}

func (s *Server) unblockUser(blockerUserID, blockedUserID string) (bool, error) {
	result, err := s.db.Exec(
		`DELETE FROM user_blocks
		  WHERE blocker_user_id = $1 AND blocked_user_id = $2`,
		blockerUserID,
		blockedUserID,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *Server) getUserBlockState(viewerUserID, targetUserID string) (bool, bool, error) {
	if strings.TrimSpace(viewerUserID) == "" || strings.TrimSpace(targetUserID) == "" || viewerUserID == targetUserID {
		return false, false, nil
	}
	var iBlockedUser bool
	var blockedMe bool
	err := s.db.QueryRow(
		`SELECT EXISTS(
		     SELECT 1 FROM user_blocks
		      WHERE blocker_user_id = $1 AND blocked_user_id = $2
		   ),
		   EXISTS(
		     SELECT 1 FROM user_blocks
		      WHERE blocker_user_id = $2 AND blocked_user_id = $1
		   )`,
		viewerUserID,
		targetUserID,
	).Scan(&iBlockedUser, &blockedMe)
	if err != nil {
		return false, false, err
	}
	return iBlockedUser, blockedMe, nil
}

func (s *Server) listProfileRecommendations(targetUserID string) ([]ProfileRecommendation, error) {
	rows, err := s.db.Query(
		`SELECT pr.id, pr.target_user_id, pr.author_user_id, u.username, u.icon_url, pr.content, pr.created_at, pr.updated_at
		   FROM profile_recommendations pr
		   JOIN users u ON u.id = pr.author_user_id
		  WHERE pr.target_user_id = $1
		  ORDER BY pr.updated_at DESC, pr.id DESC`,
		targetUserID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]ProfileRecommendation, 0)
	for rows.Next() {
		var item ProfileRecommendation
		if err := rows.Scan(
			&item.ID,
			&item.TargetUserID,
			&item.AuthorUserID,
			&item.AuthorUsername,
			&item.AuthorUserIcon,
			&item.Content,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Server) getUserProfileDetail(targetUserID, viewerUserID string) (*UserProfileDetail, error) {
	user, err := s.getUserByID(targetUserID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, nil
	}
	recommendations, err := s.listProfileRecommendations(targetUserID)
	if err != nil {
		return nil, err
	}
	iBlockedUser, blockedMe, err := s.getUserBlockState(viewerUserID, targetUserID)
	if err != nil {
		return nil, err
	}
	isFollowing, followedMe, err := s.getUserFollowState(viewerUserID, targetUserID)
	if err != nil {
		return nil, err
	}
	followerCount, err := s.countUserFollowers(targetUserID)
	if err != nil {
		return nil, err
	}
	followingCount, err := s.countUserFollowing(targetUserID)
	if err != nil {
		return nil, err
	}
	return &UserProfileDetail{
		UserID:   user.ID,
		Username: user.Username,
		Email: func() string {
			if targetUserID == viewerUserID {
				return user.Email
			}
			return ""
		}(),
		IconURL:         user.IconURL,
		Bio:             user.Bio,
		CreatedAt:       user.CreatedAt,
		IsMe:            targetUserID == viewerUserID,
		CanRecommend:    targetUserID != viewerUserID,
		IBlockedUser:    iBlockedUser,
		BlockedMe:       blockedMe,
		IsFollowing:     isFollowing,
		FollowedMe:      followedMe,
		FollowerCount:   followerCount,
		FollowingCount:  followingCount,
		Recommendations: recommendations,
	}, nil
}

func (s *Server) createUserFollow(followerID, followeeID string, now time.Time) error {
	if followerID == "" || followeeID == "" || followerID == followeeID {
		return errors.New("invalid follow pair")
	}
	_, err := s.db.Exec(
		`INSERT INTO user_follows (follower_user_id, followee_user_id, created_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (follower_user_id, followee_user_id) DO NOTHING`,
		followerID,
		followeeID,
		now,
	)
	return err
}

func (s *Server) deleteUserFollow(followerID, followeeID string) (bool, error) {
	result, err := s.db.Exec(
		`DELETE FROM user_follows
		  WHERE follower_user_id = $1 AND followee_user_id = $2`,
		followerID,
		followeeID,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *Server) getUserFollowState(viewerUserID, targetUserID string) (isFollowing, followedMe bool, err error) {
	if strings.TrimSpace(viewerUserID) == "" || strings.TrimSpace(targetUserID) == "" || viewerUserID == targetUserID {
		return false, false, nil
	}
	err = s.db.QueryRow(
		`SELECT EXISTS(
		     SELECT 1 FROM user_follows
		      WHERE follower_user_id = $1 AND followee_user_id = $2
		   ),
		   EXISTS(
		     SELECT 1 FROM user_follows
		      WHERE follower_user_id = $2 AND followee_user_id = $1
		   )`,
		viewerUserID,
		targetUserID,
	).Scan(&isFollowing, &followedMe)
	if err != nil {
		return false, false, err
	}
	return isFollowing, followedMe, nil
}

func (s *Server) countUserFollowers(userID string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM user_follows WHERE followee_user_id = $1`,
		userID,
	).Scan(&count)
	return count, err
}

func (s *Server) countUserFollowing(userID string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM user_follows WHERE follower_user_id = $1`,
		userID,
	).Scan(&count)
	return count, err
}

// listUserFollowers returns the users following targetUserID.
// viewerUserID is the caller; `is_following` reflects whether the viewer
// follows each listed user.
func (s *Server) listUserFollowers(targetUserID, viewerUserID string, limit, offset int) ([]UserSummary, int, error) {
	return s.listFollowRelation(targetUserID, viewerUserID, limit, offset, true)
}

// listUserFollowing returns the users that targetUserID follows.
func (s *Server) listUserFollowing(targetUserID, viewerUserID string, limit, offset int) ([]UserSummary, int, error) {
	return s.listFollowRelation(targetUserID, viewerUserID, limit, offset, false)
}

func (s *Server) listFollowRelation(targetUserID, viewerUserID string, limit, offset int, followers bool) ([]UserSummary, int, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var (
		totalQuery string
		listQuery  string
	)
	if followers {
		totalQuery = `SELECT COUNT(*) FROM user_follows WHERE followee_user_id = $1`
		listQuery = `
			SELECT u.id, u.username, u.icon_url, u.bio,
			       EXISTS(
			         SELECT 1 FROM user_follows vf
			          WHERE vf.follower_user_id = $2 AND vf.followee_user_id = u.id
			       ) AS is_following
			  FROM user_follows f
			  JOIN users u ON u.id = f.follower_user_id
			 WHERE f.followee_user_id = $1
			 ORDER BY f.created_at DESC
			 LIMIT $3 OFFSET $4`
	} else {
		totalQuery = `SELECT COUNT(*) FROM user_follows WHERE follower_user_id = $1`
		listQuery = `
			SELECT u.id, u.username, u.icon_url, u.bio,
			       EXISTS(
			         SELECT 1 FROM user_follows vf
			          WHERE vf.follower_user_id = $2 AND vf.followee_user_id = u.id
			       ) AS is_following
			  FROM user_follows f
			  JOIN users u ON u.id = f.followee_user_id
			 WHERE f.follower_user_id = $1
			 ORDER BY f.created_at DESC
			 LIMIT $3 OFFSET $4`
	}

	var total int
	if err := s.db.QueryRow(totalQuery, targetUserID).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(listQuery, targetUserID, viewerUserID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]UserSummary, 0, limit)
	for rows.Next() {
		var item UserSummary
		if err := rows.Scan(&item.ID, &item.Username, &item.UserIcon, &item.Bio, &item.IsFollowing); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// searchUsers returns real users (non-admin, non-bot) matching q, with
// is_following computed for the viewer. viewerUserID is excluded from results.
func (s *Server) searchUsers(q, viewerUserID string, limit, offset int) ([]UserSummary, int, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	filter := "%"
	if trimmed := strings.TrimSpace(q); trimmed != "" {
		filter = "%" + trimmed + "%"
	}

	var total int
	if err := s.db.QueryRow(
		`SELECT COUNT(*)
		   FROM users u
		  WHERE u.id <> $1
		    AND u.id <> $2
		    AND u.role <> 'admin'
		    AND NOT EXISTS (SELECT 1 FROM bot_users b WHERE b.bot_user_id = u.id)
		    AND ($3 = '%' OR u.username ILIKE $3 OR u.email ILIKE $3 OR u.id ILIKE $3)`,
		systemUserID,
		viewerUserID,
		filter,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(
		`SELECT u.id, u.username, u.icon_url, u.bio,
		        EXISTS(
		          SELECT 1 FROM user_follows vf
		           WHERE vf.follower_user_id = $2 AND vf.followee_user_id = u.id
		        ) AS is_following
		   FROM users u
		  WHERE u.id <> $1
		    AND u.id <> $2
		    AND u.role <> 'admin'
		    AND NOT EXISTS (SELECT 1 FROM bot_users b WHERE b.bot_user_id = u.id)
		    AND ($3 = '%' OR u.username ILIKE $3 OR u.email ILIKE $3 OR u.id ILIKE $3)
		  ORDER BY u.username ASC, u.id ASC
		  LIMIT $4 OFFSET $5`,
		systemUserID,
		viewerUserID,
		filter,
		limit,
		offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]UserSummary, 0, limit)
	for rows.Next() {
		var item UserSummary
		if err := rows.Scan(&item.ID, &item.Username, &item.UserIcon, &item.Bio, &item.IsFollowing); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (s *Server) listMarkdownEntries(userID string, limit, offset int) ([]MarkdownEntry, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(
		`SELECT m.id, m.user_id, u.username, u.icon_url, m.title, m.summary, m.cover_url, m.file_path, m.is_public, m.editor_mode, m.uploaded_at
		   FROM markdown_entries m
		   JOIN users u ON u.id = m.user_id
		  WHERE m.user_id = $1
		  ORDER BY m.uploaded_at DESC
		  LIMIT $2 OFFSET $3`,
		userID,
		limit+1,
		offset,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	entries := make([]MarkdownEntry, 0, limit+1)
	for rows.Next() {
		var entry MarkdownEntry
		if err := rows.Scan(&entry.ID, &entry.UserID, &entry.Username, &entry.UserIcon, &entry.Title, &entry.Summary, &entry.CoverURL, &entry.FilePath, &entry.IsPublic, &entry.EditorMode, &entry.UploadedAt); err != nil {
			return nil, false, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := false
	if len(entries) > limit {
		hasMore = true
		entries = entries[:limit]
	}
	return entries, hasMore, nil
}

func (s *Server) listPublicMarkdownEntries(limit, offset int) ([]PublicMarkdownEntry, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := s.db.Query(
		`SELECT m.id, m.user_id, u.username, u.icon_url, m.title, m.summary, m.cover_url, m.editor_mode, m.uploaded_at
		   FROM markdown_entries m
		   JOIN users u ON u.id = m.user_id
		  WHERE m.is_public = TRUE
		  ORDER BY m.uploaded_at DESC
		  LIMIT $1 OFFSET $2`,
		limit+1,
		offset,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	entries := make([]PublicMarkdownEntry, 0, limit+1)
	for rows.Next() {
		var entry PublicMarkdownEntry
		if err := rows.Scan(&entry.ID, &entry.UserID, &entry.Username, &entry.UserIcon, &entry.Title, &entry.Summary, &entry.CoverURL, &entry.EditorMode, &entry.UploadedAt); err != nil {
			return nil, false, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := false
	if len(entries) > limit {
		hasMore = true
		entries = entries[:limit]
	}
	return entries, hasMore, nil
}

func (s *Server) getSiteSettings() (*SiteSettings, error) {
	var settings SiteSettings
	var devURL sql.NullString
	var devName sql.NullString
	var devUploadedAt sql.NullTime
	var prodURL sql.NullString
	var prodName sql.NullString
	var prodUploadedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT name, description, icon_url, registration_requires_invite,
		        apple_push_dev_cert_url, apple_push_dev_cert_name, apple_push_dev_cert_uploaded_at,
		        apple_push_prod_cert_url, apple_push_prod_cert_name, apple_push_prod_cert_uploaded_at,
		        updated_at
		 FROM site_settings
		 WHERE id = 1`,
	).Scan(
		&settings.Name,
		&settings.Description,
		&settings.IconURL,
		&settings.RegistrationRequiresInvite,
		&devURL,
		&devName,
		&devUploadedAt,
		&prodURL,
		&prodName,
		&prodUploadedAt,
		&settings.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &SiteSettings{
				Name:                       "Polar-",
				Description:                "",
				IconURL:                    "",
				RegistrationRequiresInvite: false,
				UpdatedAt:                  time.Now(),
			}, nil
		}
		return nil, err
	}
	if devURL.Valid && devURL.String != "" {
		settings.ApplePushDevCert = &ApplePushCertificate{
			Environment: "dev",
			FileName:    devName.String,
			FileURL:     devURL.String,
		}
		if devUploadedAt.Valid {
			settings.ApplePushDevCert.UploadedAt = &devUploadedAt.Time
		}
	}
	if prodURL.Valid && prodURL.String != "" {
		settings.ApplePushProdCert = &ApplePushCertificate{
			Environment: "prod",
			FileName:    prodName.String,
			FileURL:     prodURL.String,
		}
		if prodUploadedAt.Valid {
			settings.ApplePushProdCert.UploadedAt = &prodUploadedAt.Time
		}
	}
	return &settings, nil
}

func (s *Server) updateSiteSettings(name, description string, registrationRequiresInvite bool) error {
	_, err := s.db.Exec(
		`INSERT INTO site_settings (
		     id, name, description, icon_url,
		     registration_requires_invite,
		     apple_push_dev_cert_url, apple_push_dev_cert_name, apple_push_dev_cert_uploaded_at,
		     apple_push_prod_cert_url, apple_push_prod_cert_name, apple_push_prod_cert_uploaded_at,
		     updated_at
		 )
		 VALUES (
		     1, $1, $2, COALESCE((SELECT icon_url FROM site_settings WHERE id = 1), ''), $3,
		     COALESCE((SELECT apple_push_dev_cert_url FROM site_settings WHERE id = 1), ''),
		     COALESCE((SELECT apple_push_dev_cert_name FROM site_settings WHERE id = 1), ''),
		     (SELECT apple_push_dev_cert_uploaded_at FROM site_settings WHERE id = 1),
		     COALESCE((SELECT apple_push_prod_cert_url FROM site_settings WHERE id = 1), ''),
		     COALESCE((SELECT apple_push_prod_cert_name FROM site_settings WHERE id = 1), ''),
		     (SELECT apple_push_prod_cert_uploaded_at FROM site_settings WHERE id = 1),
		     NOW()
		 )
		 ON CONFLICT (id)
		 DO UPDATE SET name = EXCLUDED.name,
		               description = EXCLUDED.description,
		               registration_requires_invite = EXCLUDED.registration_requires_invite,
		               updated_at = NOW()`,
		name,
		description,
		registrationRequiresInvite,
	)
	return err
}

func (s *Server) updateSiteIcon(iconURL string) error {
	_, err := s.db.Exec(
		`INSERT INTO site_settings (
		     id, name, description, icon_url,
		     registration_requires_invite,
		     apple_push_dev_cert_url, apple_push_dev_cert_name, apple_push_dev_cert_uploaded_at,
		     apple_push_prod_cert_url, apple_push_prod_cert_name, apple_push_prod_cert_uploaded_at,
		     updated_at
		 )
		 VALUES (1, COALESCE((SELECT name FROM site_settings WHERE id = 1), 'Polar-'),
		             COALESCE((SELECT description FROM site_settings WHERE id = 1), ''),
		             $1,
		             COALESCE((SELECT registration_requires_invite FROM site_settings WHERE id = 1), FALSE),
		             COALESCE((SELECT apple_push_dev_cert_url FROM site_settings WHERE id = 1), ''),
		             COALESCE((SELECT apple_push_dev_cert_name FROM site_settings WHERE id = 1), ''),
		             (SELECT apple_push_dev_cert_uploaded_at FROM site_settings WHERE id = 1),
		             COALESCE((SELECT apple_push_prod_cert_url FROM site_settings WHERE id = 1), ''),
		             COALESCE((SELECT apple_push_prod_cert_name FROM site_settings WHERE id = 1), ''),
		             (SELECT apple_push_prod_cert_uploaded_at FROM site_settings WHERE id = 1),
		             NOW())
		 ON CONFLICT (id)
		 DO UPDATE SET icon_url = EXCLUDED.icon_url, updated_at = NOW()`,
		iconURL,
	)
	return err
}

func (s *Server) updateApplePushCertificate(environment, fileURL, fileName string, uploadedAt time.Time) error {
	query := ""
	switch environment {
	case "dev":
		query = `INSERT INTO site_settings (
		           id, name, description, icon_url,
		           registration_requires_invite,
		           apple_push_dev_cert_url, apple_push_dev_cert_name, apple_push_dev_cert_uploaded_at,
		           apple_push_prod_cert_url, apple_push_prod_cert_name, apple_push_prod_cert_uploaded_at,
		           updated_at
		         )
		         VALUES (
		           1,
		           COALESCE((SELECT name FROM site_settings WHERE id = 1), 'Polar-'),
		           COALESCE((SELECT description FROM site_settings WHERE id = 1), ''),
		           COALESCE((SELECT icon_url FROM site_settings WHERE id = 1), ''),
		           COALESCE((SELECT registration_requires_invite FROM site_settings WHERE id = 1), FALSE),
		           $1, $2, $3,
		           COALESCE((SELECT apple_push_prod_cert_url FROM site_settings WHERE id = 1), ''),
		           COALESCE((SELECT apple_push_prod_cert_name FROM site_settings WHERE id = 1), ''),
		           (SELECT apple_push_prod_cert_uploaded_at FROM site_settings WHERE id = 1),
		           NOW()
		         )
		         ON CONFLICT (id)
		         DO UPDATE SET apple_push_dev_cert_url = EXCLUDED.apple_push_dev_cert_url,
		                       apple_push_dev_cert_name = EXCLUDED.apple_push_dev_cert_name,
		                       apple_push_dev_cert_uploaded_at = EXCLUDED.apple_push_dev_cert_uploaded_at,
		                       updated_at = NOW()`
	case "prod":
		query = `INSERT INTO site_settings (
		           id, name, description, icon_url,
		           registration_requires_invite,
		           apple_push_dev_cert_url, apple_push_dev_cert_name, apple_push_dev_cert_uploaded_at,
		           apple_push_prod_cert_url, apple_push_prod_cert_name, apple_push_prod_cert_uploaded_at,
		           updated_at
		         )
		         VALUES (
		           1,
		           COALESCE((SELECT name FROM site_settings WHERE id = 1), 'Polar-'),
		           COALESCE((SELECT description FROM site_settings WHERE id = 1), ''),
		           COALESCE((SELECT icon_url FROM site_settings WHERE id = 1), ''),
		           COALESCE((SELECT registration_requires_invite FROM site_settings WHERE id = 1), FALSE),
		           COALESCE((SELECT apple_push_dev_cert_url FROM site_settings WHERE id = 1), ''),
		           COALESCE((SELECT apple_push_dev_cert_name FROM site_settings WHERE id = 1), ''),
		           (SELECT apple_push_dev_cert_uploaded_at FROM site_settings WHERE id = 1),
		           $1, $2, $3,
		           NOW()
		         )
		         ON CONFLICT (id)
		         DO UPDATE SET apple_push_prod_cert_url = EXCLUDED.apple_push_prod_cert_url,
		                       apple_push_prod_cert_name = EXCLUDED.apple_push_prod_cert_name,
		                       apple_push_prod_cert_uploaded_at = EXCLUDED.apple_push_prod_cert_uploaded_at,
		                       updated_at = NOW()`
	default:
		return errors.New("invalid apple push certificate environment")
	}

	_, err := s.db.Exec(query, fileURL, fileName, uploadedAt)
	return err
}

func (s *Server) clearApplePushCertificate(environment string) error {
	query := ""
	switch environment {
	case "dev":
		query = `UPDATE site_settings
		            SET apple_push_dev_cert_url = '',
		                apple_push_dev_cert_name = '',
		                apple_push_dev_cert_uploaded_at = NULL,
		                updated_at = NOW()
		          WHERE id = 1`
	case "prod":
		query = `UPDATE site_settings
		            SET apple_push_prod_cert_url = '',
		                apple_push_prod_cert_name = '',
		                apple_push_prod_cert_uploaded_at = NULL,
		                updated_at = NOW()
		          WHERE id = 1`
	default:
		return errors.New("invalid apple push certificate environment")
	}
	_, err := s.db.Exec(query)
	return err
}

func normalizeInviteCode(value string) string {
	code := strings.ToUpper(strings.TrimSpace(value))
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")
	return code
}

func generateInviteCode() string {
	raw := strings.ToUpper(generateResourceID())
	if len(raw) < 10 {
		return "IM" + raw
	}
	return "IM" + raw[:10]
}

func (s *Server) createInviteCodes(createdBy string, count int, now time.Time) ([]InviteCode, error) {
	if count <= 0 {
		count = 1
	}
	if count > 50 {
		count = 50
	}

	codes := make([]InviteCode, 0, count)
	attempts := 0
	maxAttempts := count * 10
	for len(codes) < count && attempts < maxAttempts {
		attempts += 1
		code := generateInviteCode()
		res, err := s.db.Exec(
			`INSERT INTO invite_codes (code, created_by, created_at, used_by, used_at, disabled)
			 VALUES ($1, $2, $3, '', NULL, FALSE)
			 ON CONFLICT (code) DO NOTHING`,
			code,
			strings.TrimSpace(createdBy),
			now,
		)
		if err != nil {
			return nil, err
		}
		rows, _ := res.RowsAffected()
		if rows == 0 {
			continue
		}
		codes = append(codes, InviteCode{
			Code:      code,
			CreatedBy: strings.TrimSpace(createdBy),
			CreatedAt: now,
			Disabled:  false,
		})
	}
	if len(codes) != count {
		return nil, errors.New("failed to generate enough unique invite codes")
	}
	return codes, nil
}

func (s *Server) listInviteCodes(limit int) ([]InviteCode, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}

	rows, err := s.db.Query(
		`SELECT code, created_by, created_at, used_by, used_at, disabled
		 FROM invite_codes
		 ORDER BY created_at DESC
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]InviteCode, 0, limit)
	for rows.Next() {
		var item InviteCode
		var usedAt sql.NullTime
		if err := rows.Scan(&item.Code, &item.CreatedBy, &item.CreatedAt, &item.UsedBy, &usedAt, &item.Disabled); err != nil {
			return nil, err
		}
		if usedAt.Valid {
			item.UsedAt = &usedAt.Time
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Server) consumeInviteCode(code, usedBy string, usedAt time.Time) (bool, error) {
	normalized := normalizeInviteCode(code)
	if normalized == "" {
		return false, nil
	}
	res, err := s.db.Exec(
		`UPDATE invite_codes
		    SET used_by = $2, used_at = $3
		  WHERE code = $1
		    AND disabled = FALSE
		    AND used_at IS NULL`,
		normalized,
		strings.TrimSpace(usedBy),
		usedAt,
	)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (s *Server) releaseInviteCode(code, usedBy string) error {
	normalized := normalizeInviteCode(code)
	if normalized == "" {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE invite_codes
		    SET used_by = '', used_at = NULL
		  WHERE code = $1 AND used_by = $2`,
		normalized,
		strings.TrimSpace(usedBy),
	)
	return err
}

func (s *Server) bindInviteCodeToUser(code, pendingMarker, userID string) error {
	normalized := normalizeInviteCode(code)
	if normalized == "" {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE invite_codes
		    SET used_by = $3
		  WHERE code = $1 AND used_by = $2`,
		normalized,
		strings.TrimSpace(pendingMarker),
		strings.TrimSpace(userID),
	)
	return err
}

func (s *Server) getMarkdownEntryForUser(viewerUserID string, id int64) (*MarkdownEntry, bool, error) {
	var entry MarkdownEntry
	err := s.db.QueryRow(
		`SELECT m.id, m.user_id, COALESCE(u.username, ''), COALESCE(u.icon_url, ''),
		        m.title, m.summary, m.cover_url, m.file_path, m.is_public, m.editor_mode, m.uploaded_at
		   FROM markdown_entries m
		   LEFT JOIN users u ON u.id = m.user_id
		  WHERE m.id = $1 AND (m.user_id = $2 OR m.is_public = TRUE)`,
		id,
		viewerUserID,
	).Scan(&entry.ID, &entry.UserID, &entry.Username, &entry.UserIcon, &entry.Title, &entry.Summary, &entry.CoverURL, &entry.FilePath, &entry.IsPublic, &entry.EditorMode, &entry.UploadedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &entry, entry.UserID == viewerUserID, nil
}

func (s *Server) getOwnedMarkdownEntry(userID string, id int64) (*MarkdownEntry, error) {
	var entry MarkdownEntry
	err := s.db.QueryRow(
		`SELECT m.id, m.user_id, COALESCE(u.username, ''), COALESCE(u.icon_url, ''),
		        m.title, m.summary, m.cover_url, m.file_path, m.is_public, m.editor_mode, m.uploaded_at
		   FROM markdown_entries m
		   LEFT JOIN users u ON u.id = m.user_id
		  WHERE m.user_id = $1 AND m.id = $2`,
		userID,
		id,
	).Scan(&entry.ID, &entry.UserID, &entry.Username, &entry.UserIcon, &entry.Title, &entry.Summary, &entry.CoverURL, &entry.FilePath, &entry.IsPublic, &entry.EditorMode, &entry.UploadedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &entry, nil
}

func (s *Server) updateMarkdownEntry(userID string, id int64, title, filePath, summary, coverURL, editorMode string, isPublic bool) error {
	_, err := s.db.Exec(
		`UPDATE markdown_entries
		    SET title = $1, file_path = $2, is_public = $3, summary = $4, cover_url = $5, editor_mode = $6
		  WHERE user_id = $7 AND id = $8`,
		title,
		filePath,
		isPublic,
		summary,
		coverURL,
		editorMode,
		userID,
		id,
	)
	return err
}

func (s *Server) deleteMarkdownEntry(userID string, id int64) error {
	_, err := s.db.Exec(
		`DELETE FROM markdown_entries WHERE user_id = $1 AND id = $2`,
		userID,
		id,
	)
	return err
}

func (s *Server) createTag(tag *Tag) (*Tag, error) {
	if tag == nil {
		return nil, errors.New("tag is nil")
	}
	now := time.Now()
	if tag.CreatedAt.IsZero() {
		tag.CreatedAt = now
	}
	if tag.UpdatedAt.IsZero() {
		tag.UpdatedAt = now
	}
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO tags (name, slug, description, sort_order, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		tag.Name,
		tag.Slug,
		tag.Description,
		tag.SortOrder,
		tag.CreatedAt,
		tag.UpdatedAt,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	tag.ID = id
	return tag, nil
}

func (s *Server) listTags(limit, offset int) ([]Tag, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(
		`SELECT id, name, slug, description, sort_order, created_at, updated_at
		 FROM tags
		 ORDER BY sort_order DESC, created_at DESC
		 LIMIT $1 OFFSET $2`,
		limit+1,
		offset,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	tags := make([]Tag, 0, limit+1)
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(
			&tag.ID,
			&tag.Name,
			&tag.Slug,
			&tag.Description,
			&tag.SortOrder,
			&tag.CreatedAt,
			&tag.UpdatedAt,
		); err != nil {
			return nil, false, err
		}
		tags = append(tags, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := false
	if len(tags) > limit {
		hasMore = true
		tags = tags[:limit]
	}
	return tags, hasMore, nil
}

func (s *Server) updateTag(id int64, name, slug, description string, sortOrder int) error {
	_, err := s.db.Exec(
		`UPDATE tags
		 SET name = $1, slug = $2, description = $3, sort_order = $4, updated_at = NOW()
		 WHERE id = $5`,
		name,
		slug,
		description,
		sortOrder,
		id,
	)
	return err
}

func (s *Server) deleteTag(id int64) error {
	_, err := s.db.Exec(`DELETE FROM tags WHERE id = $1`, id)
	return err
}

func (s *Server) createPost(userID, workspaceID string, tagID *int64, postType, content string, createdAt time.Time) (int64, error) {
	if postType == "" {
		postType = "standard"
	}
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO posts (user_id, workspace_id, tag_id, post_type, content, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		userID,
		workspaceID,
		tagID,
		postType,
		content,
		createdAt,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Server) deletePost(postID int64) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM posts WHERE id = $1`, postID)
	if err != nil {
		return false, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rowsAffected > 0, nil
}

func (s *Server) addPostImage(postID int64, imageItem PostImage, createdAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO post_images (post_id, file_url, small_url, medium_url, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		postID,
		imageItem.OriginalURL,
		imageItem.SmallURL,
		imageItem.MediumURL,
		createdAt,
	)
	return err
}

func (s *Server) addPostVideo(postID int64, fileURL, posterURL string, createdAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO post_videos (post_id, file_url, poster_url, created_at)
		 VALUES ($1, $2, $3, $4)`,
		postID,
		fileURL,
		posterURL,
		createdAt,
	)
	return err
}

func normalizePostImageItem(originalURL, smallURL, mediumURL string) PostImage {
	item := PostImage{
		OriginalURL: originalURL,
		SmallURL:    smallURL,
		MediumURL:   mediumURL,
	}
	if item.OriginalURL == "" {
		if item.MediumURL != "" {
			item.OriginalURL = item.MediumURL
		} else {
			item.OriginalURL = item.SmallURL
		}
	}
	if item.MediumURL == "" {
		item.MediumURL = item.OriginalURL
	}
	if item.SmallURL == "" {
		item.SmallURL = item.MediumURL
	}
	return item
}

func legacyPostImageURL(item PostImage) string {
	if item.MediumURL != "" {
		return item.MediumURL
	}
	if item.OriginalURL != "" {
		return item.OriginalURL
	}
	return item.SmallURL
}

func (s *Server) listPosts(userID string, limit, offset int, filterTagID *int64, filterPostType, scope string) ([]Post, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	if filterPostType == "" {
		filterPostType = "all"
	}
	if scope == "" {
		scope = "all"
	}
	rows, err := s.db.Query(
		`SELECT p.id, p.user_id, u.username, u.icon_url, p.tag_id, p.post_type, p.content, p.created_at,
		        COALESCE(l.like_count, 0) AS like_count,
		        COALESCE(r.reply_count, 0) AS reply_count,
		        (pl.user_id IS NOT NULL) AS liked_by_me
		   FROM posts p
		   JOIN users u ON u.id = p.user_id
		   LEFT JOIN (
		     SELECT post_id, COUNT(*) AS like_count
		       FROM post_likes
		      GROUP BY post_id
		   ) l ON l.post_id = p.id
		   LEFT JOIN (
		     SELECT post_id, COUNT(*) AS reply_count
		       FROM post_replies
		      GROUP BY post_id
		   ) r ON r.post_id = p.id
		   LEFT JOIN post_likes pl ON pl.post_id = p.id AND pl.user_id = $1
		  WHERE ($2::BIGINT IS NULL OR p.tag_id = $2)
		    AND ($3 = 'all' OR p.post_type = $3)
		    AND (
		          $6 <> 'following'
		       OR p.user_id = $1
		       OR EXISTS (
		            SELECT 1 FROM user_follows f
		             WHERE f.follower_user_id = $1 AND f.followee_user_id = p.user_id
		          )
		        )
		  ORDER BY p.created_at DESC
		  LIMIT $4 OFFSET $5`,
		userID,
		filterTagID,
		filterPostType,
		limit+1,
		offset,
		scope,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	posts := make([]Post, 0, limit+1)
	postIDs := make([]int64, 0, limit+1)
	for rows.Next() {
		var post Post
		if err := rows.Scan(
			&post.ID,
			&post.UserID,
			&post.Username,
			&post.UserIcon,
			&post.TagID,
			&post.PostType,
			&post.Content,
			&post.CreatedAt,
			&post.LikeCount,
			&post.ReplyCount,
			&post.LikedByMe,
		); err != nil {
			return nil, false, err
		}
		posts = append(posts, post)
		postIDs = append(postIDs, post.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := false
	if len(posts) > limit {
		hasMore = true
		posts = posts[:limit]
		postIDs = postIDs[:limit]
	}

	if len(postIDs) == 0 {
		return posts, hasMore, nil
	}

	if err := s.attachPostMedia(posts, postIDs); err != nil {
		return posts, hasMore, err
	}
	if err := s.attachPostAttachments(posts, postIDs); err != nil {
		return posts, hasMore, err
	}
	if err := s.attachTaskData(posts, userID); err != nil {
		return posts, hasMore, err
	}

	return posts, hasMore, nil
}

// attachPostMedia fills in Images/ImageItems/Videos/VideoItems on each post in
// the slice. postIDs must contain the IDs of posts (in the same order or any
// order — lookup is by ID).
func (s *Server) attachPostMedia(posts []Post, postIDs []int64) error {
	if len(postIDs) == 0 {
		return nil
	}
	imageRows, err := s.db.Query(
		`SELECT post_id, file_url, small_url, medium_url FROM post_images
		  WHERE post_id = ANY($1)
		  ORDER BY id ASC`,
		pq.Array(postIDs),
	)
	if err != nil {
		return err
	}
	defer imageRows.Close()

	imageMap := make(map[int64][]string, len(postIDs))
	imageItemMap := make(map[int64][]PostImage, len(postIDs))
	for imageRows.Next() {
		var postID int64
		var fileURL, smallURL, mediumURL string
		if err := imageRows.Scan(&postID, &fileURL, &smallURL, &mediumURL); err != nil {
			return err
		}
		imageItem := normalizePostImageItem(fileURL, smallURL, mediumURL)
		imageMap[postID] = append(imageMap[postID], legacyPostImageURL(imageItem))
		imageItemMap[postID] = append(imageItemMap[postID], imageItem)
	}
	if err := imageRows.Err(); err != nil {
		return err
	}

	videoRows, err := s.db.Query(
		`SELECT post_id, file_url, poster_url FROM post_videos
		  WHERE post_id = ANY($1)
		  ORDER BY id ASC`,
		pq.Array(postIDs),
	)
	if err != nil {
		return err
	}
	defer videoRows.Close()

	videoMap := make(map[int64][]string, len(postIDs))
	videoItemMap := make(map[int64][]PostVideo, len(postIDs))
	for videoRows.Next() {
		var postID int64
		var fileURL, posterURL string
		if err := videoRows.Scan(&postID, &fileURL, &posterURL); err != nil {
			return err
		}
		videoMap[postID] = append(videoMap[postID], fileURL)
		videoItemMap[postID] = append(videoItemMap[postID], PostVideo{URL: fileURL, PosterURL: posterURL})
	}
	if err := videoRows.Err(); err != nil {
		return err
	}

	for i := range posts {
		posts[i].Images = imageMap[posts[i].ID]
		posts[i].ImageItems = imageItemMap[posts[i].ID]
		posts[i].Videos = videoMap[posts[i].ID]
		posts[i].VideoItems = videoItemMap[posts[i].ID]
	}
	return nil
}

// attachPostAttachments fans out junction lookups for the new
// attachments table (zip/pdf/doc/ppt). Image/video continue to use
// the legacy post_images/post_videos tables — those have
// per-format processing (resize, poster) that doesn't fit the
// generic attachments model.
func (s *Server) attachPostAttachments(posts []Post, postIDs []int64) error {
	if len(postIDs) == 0 {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT pa.post_id, a.id, a.workspace_id, a.uploader_user_id, a.filename, a.mime_type, a.size_bytes,
		        a.storage_kind, a.storage_ref, a.sha256, a.width, a.height, a.thumbnail_url, a.created_at, a.deleted_at
		   FROM post_attachments pa
		   JOIN attachments a ON a.id = pa.attachment_id
		  WHERE pa.post_id = ANY($1) AND a.deleted_at IS NULL
		  ORDER BY pa.post_id, pa.ord ASC, a.created_at ASC`,
		pq.Array(postIDs),
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	byPost := map[int64][]Attachment{}
	for rows.Next() {
		var postID int64
		var sha, thumb sql.NullString
		var width, height sql.NullInt64
		var deletedAt sql.NullTime
		var a Attachment
		if err := rows.Scan(
			&postID,
			&a.ID, &a.WorkspaceID, &a.UploaderUserID, &a.Filename, &a.MimeType, &a.SizeBytes,
			&a.StorageKind, &a.StorageRef, &sha, &width, &height, &thumb, &a.CreatedAt, &deletedAt,
		); err != nil {
			return err
		}
		if sha.Valid {
			a.Sha256 = sha.String
		}
		if thumb.Valid {
			a.ThumbnailURL = thumb.String
		}
		if width.Valid {
			a.Width = int(width.Int64)
		}
		if height.Valid {
			a.Height = int(height.Int64)
		}
		if deletedAt.Valid {
			t := deletedAt.Time
			a.DeletedAt = &t
		}
		a.DownloadURL = a.attachmentDownloadURL()
		byPost[postID] = append(byPost[postID], a)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range posts {
		posts[i].Attachments = byPost[posts[i].ID]
	}
	return nil
}

func (s *Server) getPostByID(userID string, postID int64) (*Post, error) {
	var post Post
	err := s.db.QueryRow(
		`SELECT p.id, p.user_id, u.username, u.icon_url, p.tag_id, p.post_type, p.content, p.created_at,
		        COALESCE(l.like_count, 0) AS like_count,
		        COALESCE(r.reply_count, 0) AS reply_count,
		        (pl.user_id IS NOT NULL) AS liked_by_me
		   FROM posts p
		   JOIN users u ON u.id = p.user_id
		   LEFT JOIN (
		     SELECT post_id, COUNT(*) AS like_count
		       FROM post_likes
		      GROUP BY post_id
		   ) l ON l.post_id = p.id
		   LEFT JOIN (
		     SELECT post_id, COUNT(*) AS reply_count
		       FROM post_replies
		      GROUP BY post_id
		   ) r ON r.post_id = p.id
		   LEFT JOIN post_likes pl ON pl.post_id = p.id AND pl.user_id = $1
		  WHERE p.id = $2`,
		userID,
		postID,
	).Scan(
		&post.ID,
		&post.UserID,
		&post.Username,
		&post.UserIcon,
		&post.TagID,
		&post.PostType,
		&post.Content,
		&post.CreatedAt,
		&post.LikeCount,
		&post.ReplyCount,
		&post.LikedByMe,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	rows, err := s.db.Query(
		`SELECT file_url, small_url, medium_url FROM post_images WHERE post_id = $1 ORDER BY id ASC`,
		postID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var images []string
	var imageItems []PostImage
	for rows.Next() {
		var url string
		var smallURL string
		var mediumURL string
		if err := rows.Scan(&url, &smallURL, &mediumURL); err != nil {
			return nil, err
		}
		imageItem := normalizePostImageItem(url, smallURL, mediumURL)
		images = append(images, legacyPostImageURL(imageItem))
		imageItems = append(imageItems, imageItem)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	post.Images = images
	post.ImageItems = imageItems

	videoRows, err := s.db.Query(
		`SELECT file_url, poster_url FROM post_videos WHERE post_id = $1 ORDER BY id ASC`,
		postID,
	)
	if err != nil {
		return nil, err
	}
	defer videoRows.Close()

	var videos []string
	var videoItems []PostVideo
	for videoRows.Next() {
		var url string
		var posterURL string
		if err := videoRows.Scan(&url, &posterURL); err != nil {
			return nil, err
		}
		videos = append(videos, url)
		videoItems = append(videoItems, PostVideo{URL: url, PosterURL: posterURL})
	}
	if err := videoRows.Err(); err != nil {
		return nil, err
	}
	post.Videos = videos
	post.VideoItems = videoItems

	if post.PostType == "task" {
		task, _, err := s.getTaskPostByID(post.ID, userID)
		if err != nil {
			return nil, err
		}
		post.Task = task
	}

	atts, err := s.listAttachmentsForPost(post.ID)
	if err != nil {
		return nil, err
	}
	post.Attachments = resolveDownloadURLs(atts)

	return &post, nil
}

func (s *Server) likePost(postID int64, userID string, createdAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO post_likes (post_id, user_id, created_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (post_id, user_id) DO NOTHING`,
		postID,
		userID,
		createdAt,
	)
	return err
}

func (s *Server) unlikePost(postID int64, userID string) error {
	_, err := s.db.Exec(`DELETE FROM post_likes WHERE post_id = $1 AND user_id = $2`, postID, userID)
	return err
}

func (s *Server) createReply(postID int64, userID, content string, createdAt time.Time) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO post_replies (post_id, user_id, content, created_at)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		postID,
		userID,
		content,
		createdAt,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Server) listReplies(postID int64, limit, offset int) ([]PostReply, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(
		`SELECT r.id, r.post_id, r.user_id, u.username, u.icon_url, r.content, r.created_at
		   FROM post_replies r
		   JOIN users u ON u.id = r.user_id
		  WHERE r.post_id = $1
		  ORDER BY r.created_at ASC
		  LIMIT $2 OFFSET $3`,
		postID,
		limit+1,
		offset,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	replies := make([]PostReply, 0, limit+1)
	for rows.Next() {
		var reply PostReply
		if err := rows.Scan(
			&reply.ID,
			&reply.PostID,
			&reply.UserID,
			&reply.Username,
			&reply.UserIcon,
			&reply.Content,
			&reply.CreatedAt,
		); err != nil {
			return nil, false, err
		}
		replies = append(replies, reply)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := false
	if len(replies) > limit {
		hasMore = true
		replies = replies[:limit]
	}

	return replies, hasMore, nil
}

func (s *Server) likeMarkdown(markdownID int64, userID string, createdAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO markdown_likes (markdown_id, user_id, created_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (markdown_id, user_id) DO NOTHING`,
		markdownID,
		userID,
		createdAt,
	)
	return err
}

func (s *Server) unlikeMarkdown(markdownID int64, userID string) error {
	_, err := s.db.Exec(`DELETE FROM markdown_likes WHERE markdown_id = $1 AND user_id = $2`, markdownID, userID)
	return err
}

func (s *Server) markdownLikeState(markdownID int64, userID string) (likeCount int, likedByMe bool, err error) {
	err = s.db.QueryRow(
		`SELECT
		    (SELECT COUNT(*) FROM markdown_likes WHERE markdown_id = $1),
		    EXISTS (SELECT 1 FROM markdown_likes WHERE markdown_id = $1 AND user_id = $2)`,
		markdownID, userID,
	).Scan(&likeCount, &likedByMe)
	return
}

func (s *Server) createMarkdownReply(markdownID int64, userID, content string, createdAt time.Time) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO markdown_replies (markdown_id, user_id, content, created_at)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		markdownID,
		userID,
		content,
		createdAt,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Server) listMarkdownReplies(markdownID int64, limit, offset int) ([]MarkdownReply, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(
		`SELECT r.id, r.markdown_id, r.user_id, u.username, u.icon_url, r.content, r.created_at
		   FROM markdown_replies r
		   JOIN users u ON u.id = r.user_id
		  WHERE r.markdown_id = $1
		  ORDER BY r.created_at ASC
		  LIMIT $2 OFFSET $3`,
		markdownID,
		limit+1,
		offset,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	replies := make([]MarkdownReply, 0, limit+1)
	for rows.Next() {
		var reply MarkdownReply
		if err := rows.Scan(
			&reply.ID,
			&reply.MarkdownID,
			&reply.UserID,
			&reply.Username,
			&reply.UserIcon,
			&reply.Content,
			&reply.CreatedAt,
		); err != nil {
			return nil, false, err
		}
		replies = append(replies, reply)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := false
	if len(replies) > limit {
		hasMore = true
		replies = replies[:limit]
	}

	return replies, hasMore, nil
}

func (s *Server) bookmarkMarkdown(markdownID int64, userID string, createdAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO markdown_bookmarks (markdown_id, user_id, created_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (markdown_id, user_id) DO NOTHING`,
		markdownID,
		userID,
		createdAt,
	)
	return err
}

func (s *Server) unbookmarkMarkdown(markdownID int64, userID string) error {
	_, err := s.db.Exec(`DELETE FROM markdown_bookmarks WHERE markdown_id = $1 AND user_id = $2`, markdownID, userID)
	return err
}

func (s *Server) bookmarkPost(postID int64, userID string, createdAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO post_bookmarks (post_id, user_id, created_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (post_id, user_id) DO NOTHING`,
		postID,
		userID,
		createdAt,
	)
	return err
}

func (s *Server) unbookmarkPost(postID int64, userID string) error {
	_, err := s.db.Exec(`DELETE FROM post_bookmarks WHERE post_id = $1 AND user_id = $2`, postID, userID)
	return err
}

// listBookmarkedPosts returns posts that userID has bookmarked, most recently
// bookmarked first. Reuses the same attachments/enrichment pipeline as listPosts.
func (s *Server) listBookmarkedPosts(userID string, limit, offset int) ([]Post, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(
		`SELECT p.id, p.user_id, u.username, u.icon_url, p.tag_id, p.post_type, p.content, p.created_at,
		        COALESCE(l.like_count, 0) AS like_count,
		        COALESCE(r.reply_count, 0) AS reply_count,
		        (pl.user_id IS NOT NULL) AS liked_by_me
		   FROM post_bookmarks b
		   JOIN posts p ON p.id = b.post_id
		   JOIN users u ON u.id = p.user_id
		   LEFT JOIN (
		     SELECT post_id, COUNT(*) AS like_count
		       FROM post_likes
		      GROUP BY post_id
		   ) l ON l.post_id = p.id
		   LEFT JOIN (
		     SELECT post_id, COUNT(*) AS reply_count
		       FROM post_replies
		      GROUP BY post_id
		   ) r ON r.post_id = p.id
		   LEFT JOIN post_likes pl ON pl.post_id = p.id AND pl.user_id = $1
		  WHERE b.user_id = $1
		  ORDER BY b.created_at DESC
		  LIMIT $2 OFFSET $3`,
		userID,
		limit+1,
		offset,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	posts := make([]Post, 0, limit+1)
	postIDs := make([]int64, 0, limit+1)
	for rows.Next() {
		var post Post
		if err := rows.Scan(
			&post.ID,
			&post.UserID,
			&post.Username,
			&post.UserIcon,
			&post.TagID,
			&post.PostType,
			&post.Content,
			&post.CreatedAt,
			&post.LikeCount,
			&post.ReplyCount,
			&post.LikedByMe,
		); err != nil {
			return nil, false, err
		}
		posts = append(posts, post)
		postIDs = append(postIDs, post.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := false
	if len(posts) > limit {
		hasMore = true
		posts = posts[:limit]
		postIDs = postIDs[:limit]
	}
	if len(postIDs) == 0 {
		return posts, hasMore, nil
	}

	if err := s.attachPostMedia(posts, postIDs); err != nil {
		return posts, hasMore, err
	}
	if err := s.attachTaskData(posts, userID); err != nil {
		return posts, hasMore, err
	}
	return posts, hasMore, nil
}

func (s *Server) createTaskPost(postID int64, location string, startAt, endAt time.Time, workingHours string, applyDeadline time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO task_posts (post_id, location, start_at, end_at, working_hours, apply_deadline, application_status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'open')`,
		postID,
		location,
		startAt,
		endAt,
		workingHours,
		applyDeadline,
	)
	return err
}

func (s *Server) attachTaskData(posts []Post, currentUserID string) error {
	postIDs := make([]int64, 0, len(posts))
	taskIndex := make(map[int64]*Post)
	for i := range posts {
		if posts[i].PostType != "task" {
			continue
		}
		postIDs = append(postIDs, posts[i].ID)
		taskIndex[posts[i].ID] = &posts[i]
	}
	if len(postIDs) == 0 {
		return nil
	}

	rows, err := s.db.Query(
		`SELECT tp.post_id, tp.location, tp.start_at, tp.end_at, tp.working_hours, tp.apply_deadline,
		        tp.application_status, tp.selected_applicant_id, sa.username, tp.selected_at,
		        tp.invitation_template, tp.invitation_sent_at,
		        COALESCE(ac.applicant_count, 0) AS applicant_count,
		        EXISTS(
		          SELECT 1 FROM task_applications ta
		           WHERE ta.post_id = tp.post_id AND ta.user_id = $2 AND ta.withdrawn_at IS NULL
		        ) AS applied_by_me
		   FROM task_posts tp
		   LEFT JOIN users sa ON sa.id = tp.selected_applicant_id
		   LEFT JOIN (
		     SELECT post_id, COUNT(*) AS applicant_count
		       FROM task_applications
		      WHERE withdrawn_at IS NULL
		        AND post_id = ANY($1)
		      GROUP BY post_id
		   ) ac ON ac.post_id = tp.post_id
		  WHERE tp.post_id = ANY($1)`,
		pq.Array(postIDs),
		currentUserID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	now := time.Now()
	for rows.Next() {
		var (
			postID                int64
			task                  TaskPost
			selectedApplicantID   sql.NullString
			selectedApplicantName sql.NullString
			selectedAt            sql.NullTime
			invitationSentAt      sql.NullTime
		)
		if err := rows.Scan(
			&postID,
			&task.Location,
			&task.StartAt,
			&task.EndAt,
			&task.WorkingHours,
			&task.ApplyDeadline,
			&task.ApplicationStatus,
			&selectedApplicantID,
			&selectedApplicantName,
			&selectedAt,
			&task.InvitationTemplate,
			&invitationSentAt,
			&task.ApplicantCount,
			&task.AppliedByMe,
		); err != nil {
			return err
		}
		task.PostID = postID
		if selectedApplicantID.Valid {
			task.SelectedApplicantID = selectedApplicantID.String
		}
		if selectedApplicantName.Valid {
			task.SelectedApplicantName = selectedApplicantName.String
		}
		if selectedAt.Valid {
			task.SelectedAt = &selectedAt.Time
		}
		if invitationSentAt.Valid {
			task.InvitationSentAt = &invitationSentAt.Time
		}
		if post := taskIndex[postID]; post != nil {
			task.SelectedByMe = task.SelectedApplicantID != "" && task.SelectedApplicantID == currentUserID
			task.CanManage = post.UserID == currentUserID
			task.CanApply = post.UserID != currentUserID &&
				task.SelectedApplicantID == "" &&
				(task.AppliedByMe || (task.ApplicationStatus == "open" && now.Before(task.ApplyDeadline)))
			task.CanViewResults = task.CanManage || task.SelectedByMe
			task.CanSubmitResult = task.SelectedByMe
			post.Task = &task
		}
	}
	return rows.Err()
}

func (s *Server) getTaskPostByID(postID int64, currentUserID string) (*TaskPost, string, error) {
	var (
		postOwner             string
		task                  TaskPost
		selectedApplicantID   sql.NullString
		selectedApplicantName sql.NullString
		selectedAt            sql.NullTime
		invitationSentAt      sql.NullTime
	)
	err := s.db.QueryRow(
		`SELECT p.user_id, tp.post_id, tp.location, tp.start_at, tp.end_at, tp.working_hours, tp.apply_deadline,
		        tp.application_status, tp.selected_applicant_id, sa.username, tp.selected_at,
		        tp.invitation_template, tp.invitation_sent_at,
		        COALESCE(ac.applicant_count, 0) AS applicant_count,
		        EXISTS(
		          SELECT 1 FROM task_applications ta
		           WHERE ta.post_id = tp.post_id AND ta.user_id = $2 AND ta.withdrawn_at IS NULL
		        ) AS applied_by_me
		   FROM task_posts tp
		   JOIN posts p ON p.id = tp.post_id
		   LEFT JOIN users sa ON sa.id = tp.selected_applicant_id
		   LEFT JOIN (
		     SELECT post_id, COUNT(*) AS applicant_count
		       FROM task_applications
		      WHERE withdrawn_at IS NULL
		      GROUP BY post_id
		   ) ac ON ac.post_id = tp.post_id
		  WHERE tp.post_id = $1`,
		postID,
		currentUserID,
	).Scan(
		&postOwner,
		&task.PostID,
		&task.Location,
		&task.StartAt,
		&task.EndAt,
		&task.WorkingHours,
		&task.ApplyDeadline,
		&task.ApplicationStatus,
		&selectedApplicantID,
		&selectedApplicantName,
		&selectedAt,
		&task.InvitationTemplate,
		&invitationSentAt,
		&task.ApplicantCount,
		&task.AppliedByMe,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", err
	}
	if selectedApplicantID.Valid {
		task.SelectedApplicantID = selectedApplicantID.String
	}
	if selectedApplicantName.Valid {
		task.SelectedApplicantName = selectedApplicantName.String
	}
	if selectedAt.Valid {
		task.SelectedAt = &selectedAt.Time
	}
	if invitationSentAt.Valid {
		task.InvitationSentAt = &invitationSentAt.Time
	}
	now := time.Now()
	task.SelectedByMe = task.SelectedApplicantID != "" && task.SelectedApplicantID == currentUserID
	task.CanManage = postOwner == currentUserID
	task.CanApply = postOwner != currentUserID &&
		task.SelectedApplicantID == "" &&
		(task.AppliedByMe || (task.ApplicationStatus == "open" && now.Before(task.ApplyDeadline)))
	task.CanViewResults = task.CanManage || task.SelectedByMe
	task.CanSubmitResult = task.SelectedByMe
	return &task, postOwner, nil
}

func (s *Server) applyTask(postID int64, applicantID string, appliedAt time.Time) error {
	task, ownerID, err := s.getTaskPostByID(postID, applicantID)
	if err != nil {
		return err
	}
	if task == nil {
		return errTaskNotFound
	}
	if ownerID == applicantID {
		return errTaskSelfApply
	}
	if task.ApplicationStatus != "open" || task.SelectedApplicantID != "" {
		return errTaskClosed
	}
	if !appliedAt.Before(task.ApplyDeadline) {
		return errTaskApplyEnded
	}
	_, err = s.db.Exec(
		`INSERT INTO task_applications (post_id, user_id, applied_at, withdrawn_at)
		 VALUES ($1, $2, $3, NULL)
		 ON CONFLICT (post_id, user_id) WHERE withdrawn_at IS NULL DO NOTHING`,
		postID,
		applicantID,
		appliedAt,
	)
	return err
}

func (s *Server) withdrawTaskApplication(postID int64, applicantID string, withdrawnAt time.Time) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE task_applications
		    SET withdrawn_at = $3
		  WHERE post_id = $1 AND user_id = $2 AND withdrawn_at IS NULL`,
		postID,
		applicantID,
		withdrawnAt,
	)
	if err != nil {
		return false, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rowsAffected > 0, nil
}

func (s *Server) listTaskApplications(postID int64) ([]TaskApplication, error) {
	rows, err := s.db.Query(
		`SELECT ta.id, ta.post_id, ta.user_id, u.username, u.icon_url, ta.applied_at
		   FROM task_applications ta
		   JOIN users u ON u.id = ta.user_id
		  WHERE ta.post_id = $1 AND ta.withdrawn_at IS NULL
		  ORDER BY ta.applied_at ASC`,
		postID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applications := make([]TaskApplication, 0)
	for rows.Next() {
		var item TaskApplication
		if err := rows.Scan(&item.ID, &item.PostID, &item.UserID, &item.Username, &item.UserIcon, &item.AppliedAt); err != nil {
			return nil, err
		}
		applications = append(applications, item)
	}
	return applications, rows.Err()
}

func (s *Server) closeTaskApplications(postID int64, ownerID string) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE task_posts tp
		    SET application_status = 'closed'
		   FROM posts p
		  WHERE tp.post_id = p.id AND tp.post_id = $1 AND p.user_id = $2`,
		postID,
		ownerID,
	)
	if err != nil {
		return false, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rowsAffected > 0, nil
}

func defaultTaskInvitation(postContent, workingHours string, startAt, endAt time.Time) string {
	return "你好，你已被选为该零工任务的候选人。\n\n任务内容：" + postContent +
		"\n时间范围：" + startAt.Format("2006-01-02 15:04") + " - " + endAt.Format("2006-01-02 15:04") +
		"\nWorking hours：" + workingHours +
		"\n如果你确认参与，请直接回复。"
}

func (s *Server) selectTaskApplicant(postID int64, ownerID, applicantID, invitationTemplate string, selectedAt time.Time) (int64, int64, string, error) {
	var (
		postContent   string
		taskStartAt   time.Time
		taskEndAt     time.Time
		workingHours  string
		ownerMatch    string
		applicationOK bool
	)
	err := s.db.QueryRow(
		`SELECT p.user_id, p.content, tp.start_at, tp.end_at, tp.working_hours,
		        EXISTS(
		          SELECT 1 FROM task_applications ta
		           WHERE ta.post_id = tp.post_id AND ta.user_id = $2 AND ta.withdrawn_at IS NULL
		        ) AS application_ok
		   FROM posts p
		   JOIN task_posts tp ON tp.post_id = p.id
		  WHERE p.id = $1`,
		postID,
		applicantID,
	).Scan(&ownerMatch, &postContent, &taskStartAt, &taskEndAt, &workingHours, &applicationOK)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, "", errTaskNotFound
		}
		return 0, 0, "", err
	}
	if ownerMatch != ownerID {
		return 0, 0, "", errTaskNotFound
	}
	if !applicationOK {
		return 0, 0, "", errTaskClosed
	}
	if strings.TrimSpace(invitationTemplate) == "" {
		invitationTemplate = defaultTaskInvitation(postContent, workingHours, taskStartAt, taskEndAt)
	}

	thread, err := s.ensureChatThread(ownerID, applicantID, selectedAt)
	if err != nil {
		return 0, 0, "", err
	}

	messageID, err := s.createChatMessage(thread.ID, nil, ownerID, invitationTemplate, selectedAt)
	if err != nil {
		return 0, 0, "", err
	}

	if _, err = s.db.Exec(
		`UPDATE task_posts
		    SET selected_applicant_id = $2,
		        selected_at = $3,
		        application_status = 'closed',
		        invitation_template = $4,
		        invitation_sent_at = $3
		  WHERE post_id = $1`,
		postID,
		applicantID,
		selectedAt,
		invitationTemplate,
	); err != nil {
		return 0, 0, "", err
	}
	return thread.ID, messageID, invitationTemplate, nil
}

func (s *Server) canAccessTaskResults(postID int64, userID string) (bool, bool, error) {
	task, _, err := s.getTaskPostByID(postID, userID)
	if err != nil {
		return false, false, err
	}
	if task == nil {
		return false, false, errTaskNotFound
	}
	return task.CanViewResults, task.CanSubmitResult, nil
}

func (s *Server) createTaskResult(postID int64, userID, note string, createdAt time.Time) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO task_results (post_id, user_id, note, created_at)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		postID,
		userID,
		note,
		createdAt,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Server) addTaskResultImage(resultID int64, fileURL string, createdAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO task_result_images (result_id, file_url, created_at)
		 VALUES ($1, $2, $3)`,
		resultID,
		fileURL,
		createdAt,
	)
	return err
}

func (s *Server) addTaskResultVideo(resultID int64, fileURL, posterURL string, createdAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO task_result_videos (result_id, file_url, poster_url, created_at)
		 VALUES ($1, $2, $3, $4)`,
		resultID,
		fileURL,
		posterURL,
		createdAt,
	)
	return err
}

func (s *Server) deleteTaskResult(resultID int64) error {
	_, err := s.db.Exec(`DELETE FROM task_results WHERE id = $1`, resultID)
	return err
}

func (s *Server) listTaskResults(postID int64, userID string) ([]TaskResult, error) {
	canView, _, err := s.canAccessTaskResults(postID, userID)
	if err != nil {
		return nil, err
	}
	if !canView {
		return nil, errTaskForbidden
	}

	rows, err := s.db.Query(
		`SELECT tr.id, tr.post_id, tr.user_id, u.username, u.icon_url, tr.note, tr.created_at
		   FROM task_results tr
		   JOIN users u ON u.id = tr.user_id
		  WHERE tr.post_id = $1
		  ORDER BY tr.created_at DESC, tr.id DESC`,
		postID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]TaskResult, 0)
	resultIDs := make([]int64, 0)
	for rows.Next() {
		var item TaskResult
		if err := rows.Scan(&item.ID, &item.PostID, &item.UserID, &item.Username, &item.UserIcon, &item.Note, &item.CreatedAt); err != nil {
			return nil, err
		}
		results = append(results, item)
		resultIDs = append(resultIDs, item.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(resultIDs) == 0 {
		return results, nil
	}

	imageRows, err := s.db.Query(
		`SELECT result_id, file_url FROM task_result_images
		  WHERE result_id = ANY($1)
		  ORDER BY id ASC`,
		pq.Array(resultIDs),
	)
	if err != nil {
		return nil, err
	}
	defer imageRows.Close()

	imageMap := make(map[int64][]string, len(resultIDs))
	for imageRows.Next() {
		var resultID int64
		var fileURL string
		if err := imageRows.Scan(&resultID, &fileURL); err != nil {
			return nil, err
		}
		imageMap[resultID] = append(imageMap[resultID], fileURL)
	}
	if err := imageRows.Err(); err != nil {
		return nil, err
	}

	videoRows, err := s.db.Query(
		`SELECT result_id, file_url, poster_url FROM task_result_videos
		  WHERE result_id = ANY($1)
		  ORDER BY id ASC`,
		pq.Array(resultIDs),
	)
	if err != nil {
		return nil, err
	}
	defer videoRows.Close()

	videoMap := make(map[int64][]string, len(resultIDs))
	videoItemMap := make(map[int64][]PostVideo, len(resultIDs))
	for videoRows.Next() {
		var resultID int64
		var fileURL string
		var posterURL string
		if err := videoRows.Scan(&resultID, &fileURL, &posterURL); err != nil {
			return nil, err
		}
		videoMap[resultID] = append(videoMap[resultID], fileURL)
		videoItemMap[resultID] = append(videoItemMap[resultID], PostVideo{URL: fileURL, PosterURL: posterURL})
	}
	if err := videoRows.Err(); err != nil {
		return nil, err
	}

	for i := range results {
		results[i].Images = imageMap[results[i].ID]
		results[i].Videos = videoMap[results[i].ID]
		results[i].VideoItems = videoItemMap[results[i].ID]
	}
	return results, nil
}

func normalizeChatPair(userA, userB string) (string, string) {
	if userA <= userB {
		return userA, userB
	}
	return userB, userA
}

func (s *Server) ensureChatThread(userA, userB string, createdAt time.Time) (*ChatThread, error) {
	userLow, userHigh := normalizeChatPair(userA, userB)
	var thread ChatThread
	var lastMessageID sql.NullInt64
	var lastMessageAt sql.NullTime
	err := s.db.QueryRow(
		`INSERT INTO chat_threads (user_low, user_high, created_at, last_message)
		 VALUES ($1, $2, $3, '')
		 ON CONFLICT (user_low, user_high)
		 DO UPDATE SET user_low = EXCLUDED.user_low
		 RETURNING id, user_low, user_high, created_at, last_message, last_message_id, last_message_at`,
		userLow,
		userHigh,
		createdAt,
	).Scan(
		&thread.ID,
		&thread.UserLow,
		&thread.UserHigh,
		&thread.CreatedAt,
		&thread.LastMessage,
		&lastMessageID,
		&lastMessageAt,
	)
	if err != nil {
		return nil, err
	}
	if lastMessageID.Valid {
		thread.LastMessageID = &lastMessageID.Int64
	}
	if lastMessageAt.Valid {
		thread.LastMessageAt = &lastMessageAt.Time
	}
	return &thread, nil
}

func (s *Server) isChatParticipant(threadID int64, userID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(
		`SELECT EXISTS(
		   SELECT 1 FROM chat_threads
		    WHERE id = $1 AND (user_low = $2 OR user_high = $2)
		 )`,
		threadID,
		userID,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *Server) listChatThreads(userID string, limit, offset int) ([]ChatSummary, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(
		`SELECT t.id,
		        CASE WHEN t.user_low = $1 THEN t.user_high ELSE t.user_low END AS other_id,
		        u.username,
		        u.icon_url,
		        u.is_online,
		        u.last_active_device_type,
		        u.last_seen_at,
		        t.last_message,
		        t.last_message_at,
		        t.created_at,
		        COALESCE(uc.unread_count, 0) AS unread_count
		   FROM chat_threads t
		   JOIN users u ON u.id = CASE WHEN t.user_low = $1 THEN t.user_high ELSE t.user_low END
		   LEFT JOIN chat_reads r ON r.thread_id = t.id AND r.user_id = $1
		   LEFT JOIN LATERAL (
		     SELECT COUNT(*) AS unread_count
		       FROM chat_messages m
		      WHERE m.thread_id = t.id
		        AND m.sender_id <> $1
		        AND m.deleted_at IS NULL
		        AND m.created_at > COALESCE(r.last_read_at, TIMESTAMPTZ '1970-01-01')
		   ) uc ON true
		  WHERE t.user_low = $1 OR t.user_high = $1
		  ORDER BY COALESCE(t.last_message_at, t.created_at) DESC
		  LIMIT $2 OFFSET $3`,
		userID,
		limit+1,
		offset,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	threads := make([]ChatSummary, 0, limit+1)
	for rows.Next() {
		var summary ChatSummary
		var otherUserLastSeenAt sql.NullTime
		var lastMessageAt sql.NullTime
		if err := rows.Scan(
			&summary.ID,
			&summary.OtherUserID,
			&summary.OtherUsername,
			&summary.OtherUserIcon,
			&summary.OtherUserOnline,
			&summary.OtherUserDeviceType,
			&otherUserLastSeenAt,
			&summary.LastMessage,
			&lastMessageAt,
			&summary.CreatedAt,
			&summary.UnreadCount,
		); err != nil {
			return nil, false, err
		}
		if lastMessageAt.Valid {
			summary.LastMessageAt = &lastMessageAt.Time
		}
		if otherUserLastSeenAt.Valid {
			summary.OtherUserLastSeenAt = &otherUserLastSeenAt.Time
		}
		threads = append(threads, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := false
	if len(threads) > limit {
		hasMore = true
		threads = threads[:limit]
	}
	for i := range threads {
		isImplicitFriend, replyRequired, replyRequiredMessage, err := s.getChatReplyState(threads[i].ID, userID)
		if err != nil {
			return nil, false, err
		}
		threads[i].IsImplicitFriend = isImplicitFriend
		threads[i].ReplyRequired = replyRequired
		threads[i].ReplyRequiredMessage = replyRequiredMessage
	}
	return threads, hasMore, nil
}

func (s *Server) getChatSummary(userID string, threadID int64) (*ChatSummary, error) {
	var summary ChatSummary
	var lastMessageAt sql.NullTime
	var otherUserLastSeenAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT t.id,
		        CASE WHEN t.user_low = $1 THEN t.user_high ELSE t.user_low END AS other_id,
		        u.username,
		        u.icon_url,
		        u.is_online,
		        u.last_active_device_type,
		        u.last_seen_at,
		        t.last_message,
		        t.last_message_at,
		        t.created_at,
		        COALESCE(uc.unread_count, 0) AS unread_count
		   FROM chat_threads t
		   JOIN users u ON u.id = CASE WHEN t.user_low = $1 THEN t.user_high ELSE t.user_low END
		   LEFT JOIN chat_reads r ON r.thread_id = t.id AND r.user_id = $1
		   LEFT JOIN LATERAL (
		     SELECT COUNT(*) AS unread_count
		       FROM chat_messages m
		      WHERE m.thread_id = t.id
		        AND m.sender_id <> $1
		        AND m.deleted_at IS NULL
		        AND m.created_at > COALESCE(r.last_read_at, TIMESTAMPTZ '1970-01-01')
		   ) uc ON true
		  WHERE t.id = $2 AND (t.user_low = $1 OR t.user_high = $1)`,
		userID,
		threadID,
	).Scan(
		&summary.ID,
		&summary.OtherUserID,
		&summary.OtherUsername,
		&summary.OtherUserIcon,
		&summary.OtherUserOnline,
		&summary.OtherUserDeviceType,
		&otherUserLastSeenAt,
		&summary.LastMessage,
		&lastMessageAt,
		&summary.CreatedAt,
		&summary.UnreadCount,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if lastMessageAt.Valid {
		summary.LastMessageAt = &lastMessageAt.Time
	}
	if otherUserLastSeenAt.Valid {
		summary.OtherUserLastSeenAt = &otherUserLastSeenAt.Time
	}
	isImplicitFriend, replyRequired, replyRequiredMessage, err := s.getChatReplyState(threadID, userID)
	if err != nil {
		return nil, err
	}
	summary.IsImplicitFriend = isImplicitFriend
	summary.ReplyRequired = replyRequired
	summary.ReplyRequiredMessage = replyRequiredMessage
	return &summary, nil
}

func (s *Server) listChatMessages(threadID int64, llmThreadID *int64, limit, offset int) ([]ChatMessage, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	query := `SELECT m.id, m.thread_id, m.llm_thread_id, m.sender_id, u.username, u.icon_url, m.message_type, m.failed, m.content, m.markdown_entry_id, m.markdown_title, m.latency_ms, m.streaming, m.seq, m.updated_at, m.created_at, m.deleted_at, m.deleted_by
		   FROM chat_messages m
		   JOIN users u ON u.id = m.sender_id
		  WHERE m.thread_id = $1`
	args := []any{threadID}
	if llmThreadID != nil {
		query += ` AND m.llm_thread_id = $2 ORDER BY m.created_at ASC LIMIT $3 OFFSET $4`
		args = append(args, *llmThreadID, limit+1, offset)
	} else {
		query += ` ORDER BY m.created_at ASC LIMIT $2 OFFSET $3`
		args = append(args, limit+1, offset)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	messages := make([]ChatMessage, 0, limit+1)
	for rows.Next() {
		var msg ChatMessage
		var llmThreadIDValue sql.NullInt64
		var markdownEntryID sql.NullInt64
		var latencyMs sql.NullInt64
		var updatedAt sql.NullTime
		var deletedAt sql.NullTime
		var deletedBy sql.NullString
		if err := rows.Scan(
			&msg.ID,
			&msg.ThreadID,
			&llmThreadIDValue,
			&msg.SenderID,
			&msg.SenderUsername,
			&msg.SenderIcon,
			&msg.MessageType,
			&msg.Failed,
			&msg.Content,
			&markdownEntryID,
			&msg.MarkdownTitle,
			&latencyMs,
			&msg.Streaming,
			&msg.Seq,
			&updatedAt,
			&msg.CreatedAt,
			&deletedAt,
			&deletedBy,
		); err != nil {
			return nil, false, err
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
			msg.Deleted = true
			msg.Content = ""
		}
		if llmThreadIDValue.Valid {
			msg.LLMThreadID = &llmThreadIDValue.Int64
		}
		if markdownEntryID.Valid {
			msg.MarkdownEntryID = &markdownEntryID.Int64
		}
		if latencyMs.Valid {
			msg.LatencyMs = &latencyMs.Int64
		}
		if updatedAt.Valid {
			msg.UpdatedAt = &updatedAt.Time
		}
		if deletedBy.Valid {
			msg.DeletedBy = deletedBy.String
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := false
	if len(messages) > limit {
		hasMore = true
		messages = messages[:limit]
	}
	if err := s.attachChatMessageAttachments(messages); err != nil {
		return nil, false, err
	}
	return messages, hasMore, nil
}

// attachChatMessageAttachments hydrates the .Attachment field on
// each message in the slice via a single bulk lookup against
// chat_message_attachments + attachments. The legacy
// ChatMessageAttachment struct only models one attachment per
// message, so we surface the first linked one (ord ASC, then
// created_at) for wire compatibility. Soft-deleted attachments
// are skipped.
func (s *Server) attachChatMessageAttachments(messages []ChatMessage) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(messages))
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	rows, err := s.db.Query(
		`SELECT cma.message_id, a.filename, a.mime_type, a.size_bytes, a.storage_kind, a.storage_ref, a.thumbnail_url, a.width, a.height, a.id
		   FROM chat_message_attachments cma
		   JOIN attachments a ON a.id = cma.attachment_id
		  WHERE cma.message_id = ANY($1) AND a.deleted_at IS NULL
		  ORDER BY cma.message_id, cma.ord ASC, a.created_at ASC`,
		pq.Array(ids),
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	first := map[int64]*ChatMessageAttachment{}
	for rows.Next() {
		var msgID int64
		var filename, mimeType, storageKind, storageRef string
		var thumb sql.NullString
		var width, height sql.NullInt64
		var size int64
		var attID string
		if err := rows.Scan(&msgID, &filename, &mimeType, &size, &storageKind, &storageRef, &thumb, &width, &height, &attID); err != nil {
			return err
		}
		if _, exists := first[msgID]; exists {
			continue
		}
		var url string
		if storageKind == AttachmentStorageRemote {
			url = storageRef
		} else if strings.HasPrefix(storageRef, "/uploads/") {
			// migrated chat row — ref is already an absolute path.
			url = storageRef
		} else {
			url = "/api/attachments/" + attID + "/download"
		}
		att := &ChatMessageAttachment{
			URL:      url,
			FileName: filename,
			Size:     size,
			MimeType: mimeType,
		}
		if thumb.Valid {
			att.ThumbnailURL = thumb.String
		}
		if width.Valid {
			att.Width = int(width.Int64)
		}
		if height.Valid {
			att.Height = int(height.Int64)
		}
		first[msgID] = att
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range messages {
		if a, ok := first[messages[i].ID]; ok {
			messages[i].Attachment = a
		}
	}
	return nil
}

func (s *Server) markChatRead(threadID int64, userID string, readAt time.Time, lastReadMessageID *int64) error {
	_, err := s.db.Exec(
		`INSERT INTO chat_reads (thread_id, user_id, last_read_at, last_read_message_id)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (thread_id, user_id)
		 DO UPDATE SET last_read_at = EXCLUDED.last_read_at,
		               last_read_message_id = CASE
		                   WHEN chat_reads.last_read_message_id IS NULL THEN EXCLUDED.last_read_message_id
		                   WHEN EXCLUDED.last_read_message_id IS NULL THEN chat_reads.last_read_message_id
		                   WHEN EXCLUDED.last_read_message_id > chat_reads.last_read_message_id THEN EXCLUDED.last_read_message_id
		                   ELSE chat_reads.last_read_message_id
		               END`,
		threadID,
		userID,
		readAt,
		lastReadMessageID,
	)
	return err
}

func (s *Server) upsertChatMemberStateViewed(threadID int64, userID string, openedAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO chat_member_state (thread_id, user_id, last_opened_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $3, $3)
		 ON CONFLICT (thread_id, user_id)
		 DO UPDATE SET last_opened_at = EXCLUDED.last_opened_at,
		               updated_at = EXCLUDED.updated_at`,
		threadID,
		userID,
		openedAt,
	)
	return err
}

func (s *Server) getChatParticipants(threadID int64) (string, string, error) {
	var userLow, userHigh string
	err := s.db.QueryRow(
		`SELECT user_low, user_high FROM chat_threads WHERE id = $1`,
		threadID,
	).Scan(&userLow, &userHigh)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", nil
		}
		return "", "", err
	}
	return userLow, userHigh, nil
}

func (s *Server) getChatCounterparty(threadID int64, userID string) (string, error) {
	userLow, userHigh, err := s.getChatParticipants(threadID)
	if err != nil {
		return "", err
	}
	switch userID {
	case userLow:
		return userHigh, nil
	case userHigh:
		return userLow, nil
	default:
		return "", nil
	}
}

func (s *Server) getChatImplicitFriendState(threadID int64) (bool, error) {
	var distinctSenders int
	err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT sender_id)
		   FROM chat_messages
		  WHERE thread_id = $1
		    AND deleted_at IS NULL`,
		threadID,
	).Scan(&distinctSenders)
	if err != nil {
		return false, err
	}
	return distinctSenders >= 2, nil
}

func (s *Server) getLastUndeletedChatMessageSender(threadID int64) (string, error) {
	var senderID string
	err := s.db.QueryRow(
		`SELECT sender_id
		   FROM chat_messages
		  WHERE thread_id = $1
		    AND deleted_at IS NULL
		  ORDER BY created_at DESC, id DESC
		  LIMIT 1`,
		threadID,
	).Scan(&senderID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return senderID, nil
}

func (s *Server) getChatReplyState(threadID int64, userID string) (bool, bool, string, error) {
	responderUserID, err := s.getAIResponderForChat(threadID, userID)
	if err != nil {
		return false, false, "", err
	}
	if responderUserID != "" {
		return true, false, "", nil
	}

	isImplicitFriend, err := s.getChatImplicitFriendState(threadID)
	if err != nil {
		return false, false, "", err
	}
	if isImplicitFriend {
		return true, false, "", nil
	}

	lastSenderID, err := s.getLastUndeletedChatMessageSender(threadID)
	if err != nil {
		return false, false, "", err
	}
	if lastSenderID == userID {
		return false, true, "你已发送首条消息，请等待对方回复后再继续发送", nil
	}
	return false, false, "", nil
}

func (s *Server) listLLMThreads(chatThreadID int64, ownerUserID string) ([]LLMThread, error) {
	rows, err := s.db.Query(
		`SELECT t.id, t.chat_thread_id, t.owner_user_id, t.bot_user_id, COALESCE(t.llm_config_id, b.llm_config_id), COALESCE(c.name, ''), COALESCE(c.model, ''), t.title, t.created_at, t.updated_at, t.last_message_at
		   FROM llm_threads t
		   LEFT JOIN bot_users b ON b.bot_user_id = t.bot_user_id
		   LEFT JOIN llm_configs c ON c.id = COALESCE(t.llm_config_id, b.llm_config_id)
		  WHERE t.chat_thread_id = $1 AND t.owner_user_id = $2
		  ORDER BY updated_at DESC, id DESC`,
		chatThreadID,
		ownerUserID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]LLMThread, 0)
	for rows.Next() {
		var item LLMThread
		var llmConfigID sql.NullInt64
		var lastMessageAt sql.NullTime
		if err := rows.Scan(&item.ID, &item.ChatThreadID, &item.OwnerUserID, &item.BotUserID, &llmConfigID, &item.ConfigName, &item.ConfigModel, &item.Title, &item.CreatedAt, &item.UpdatedAt, &lastMessageAt); err != nil {
			return nil, err
		}
		if llmConfigID.Valid {
			item.LLMConfigID = &llmConfigID.Int64
		}
		if lastMessageAt.Valid {
			item.LastMessageAt = &lastMessageAt.Time
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Server) getLLMThread(chatThreadID int64, ownerUserID string, llmThreadID int64) (*LLMThread, error) {
	var item LLMThread
	var llmConfigID sql.NullInt64
	var lastMessageAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT t.id, t.chat_thread_id, t.owner_user_id, t.bot_user_id, COALESCE(t.llm_config_id, b.llm_config_id), COALESCE(c.name, ''), COALESCE(c.model, ''), t.title, t.created_at, t.updated_at, t.last_message_at
		   FROM llm_threads t
		   LEFT JOIN bot_users b ON b.bot_user_id = t.bot_user_id
		   LEFT JOIN llm_configs c ON c.id = COALESCE(t.llm_config_id, b.llm_config_id)
		  WHERE t.id = $1 AND t.chat_thread_id = $2 AND t.owner_user_id = $3`,
		llmThreadID, chatThreadID, ownerUserID,
	).Scan(&item.ID, &item.ChatThreadID, &item.OwnerUserID, &item.BotUserID, &llmConfigID, &item.ConfigName, &item.ConfigModel, &item.Title, &item.CreatedAt, &item.UpdatedAt, &lastMessageAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if llmConfigID.Valid {
		item.LLMConfigID = &llmConfigID.Int64
	}
	if lastMessageAt.Valid {
		item.LastMessageAt = &lastMessageAt.Time
	}
	return &item, nil
}

func (s *Server) createLLMThread(chatThreadID int64, ownerUserID, botUserID, title string, now time.Time) (*LLMThread, error) {
	if strings.TrimSpace(title) == "" {
		title = "新话题"
	}
	botUser, err := s.getBotUserByUserID(botUserID)
	if err != nil {
		return nil, err
	}
	if botUser == nil {
		return nil, nil
	}
	// Passthrough bots have no LLM config; BotUser.LLMConfigID
	// scans 0 in that case. Inserting 0 into llm_threads.llm_config_id
	// triggers an FK violation (no such row). Write NULL instead so
	// the column matches the bot's nullable state.
	var llmConfigArg any
	if botUser.LLMConfigID > 0 {
		llmConfigArg = botUser.LLMConfigID
	} else {
		llmConfigArg = nil
	}
	var itemID int64
	err = s.db.QueryRow(
		`INSERT INTO llm_threads (chat_thread_id, owner_user_id, bot_user_id, llm_config_id, title, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $6)
		 RETURNING id`,
		chatThreadID, ownerUserID, botUserID, llmConfigArg, title, now,
	).Scan(&itemID)
	if err != nil {
		return nil, err
	}
	return s.getLLMThread(chatThreadID, ownerUserID, itemID)
}

func (s *Server) ensureDefaultLLMThread(chatThreadID int64, ownerUserID, botUserID string, now time.Time) (*LLMThread, error) {
	var item LLMThread
	var llmConfigID sql.NullInt64
	var lastMessageAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT t.id, t.chat_thread_id, t.owner_user_id, t.bot_user_id, COALESCE(t.llm_config_id, b.llm_config_id), COALESCE(c.name, ''), COALESCE(c.model, ''), t.title, t.created_at, t.updated_at, t.last_message_at
		   FROM llm_threads t
		   LEFT JOIN bot_users b ON b.bot_user_id = t.bot_user_id
		   LEFT JOIN llm_configs c ON c.id = COALESCE(t.llm_config_id, b.llm_config_id)
		  WHERE t.chat_thread_id = $1 AND t.owner_user_id = $2 AND t.bot_user_id = $3
		  ORDER BY updated_at DESC, id DESC
		  LIMIT 1`,
		chatThreadID, ownerUserID, botUserID,
	).Scan(&item.ID, &item.ChatThreadID, &item.OwnerUserID, &item.BotUserID, &llmConfigID, &item.ConfigName, &item.ConfigModel, &item.Title, &item.CreatedAt, &item.UpdatedAt, &lastMessageAt)
	if err == nil {
		if llmConfigID.Valid {
			item.LLMConfigID = &llmConfigID.Int64
		}
		if lastMessageAt.Valid {
			item.LastMessageAt = &lastMessageAt.Time
		}
		return &item, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return s.createLLMThread(chatThreadID, ownerUserID, botUserID, "新话题", now)
}

func (s *Server) updateLLMThreadTitle(chatThreadID int64, ownerUserID string, llmThreadID int64, title string, now time.Time) (*LLMThread, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "新话题"
	}
	var updatedID int64
	err := s.db.QueryRow(
		`UPDATE llm_threads
		    SET title = $4, updated_at = $5
		  WHERE id = $1 AND chat_thread_id = $2 AND owner_user_id = $3
		  RETURNING id`,
		llmThreadID, chatThreadID, ownerUserID, title, now,
	).Scan(&updatedID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return s.getLLMThread(chatThreadID, ownerUserID, updatedID)
}

func (s *Server) deleteLLMThread(chatThreadID int64, ownerUserID string, llmThreadID int64) (bool, error) {
	result, err := s.db.Exec(
		`DELETE FROM llm_threads
		  WHERE id = $1 AND chat_thread_id = $2 AND owner_user_id = $3`,
		llmThreadID,
		chatThreadID,
		ownerUserID,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *Server) updateLLMThreadConfig(chatThreadID int64, ownerUserID string, llmThreadID, llmConfigID int64, now time.Time) (*LLMThread, error) {
	var configName, configModel string
	if err := s.db.QueryRow(
		`SELECT name, model FROM llm_configs WHERE id = $1 AND (owner_user_id = $2 OR shared = TRUE)`,
		llmConfigID,
		ownerUserID,
	).Scan(&configName, &configModel); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	var updatedID int64
	err := s.db.QueryRow(
		`UPDATE llm_threads
		    SET llm_config_id = $4, updated_at = $5
		  WHERE id = $1 AND chat_thread_id = $2 AND owner_user_id = $3
		  RETURNING id`,
		llmThreadID,
		chatThreadID,
		ownerUserID,
		llmConfigID,
		now,
	).Scan(&updatedID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	item, err := s.getLLMThread(chatThreadID, ownerUserID, updatedID)
	if err != nil || item == nil {
		return item, err
	}
	if strings.TrimSpace(item.ConfigName) == "" {
		item.ConfigName = configName
	}
	if strings.TrimSpace(item.ConfigModel) == "" {
		item.ConfigModel = configModel
	}
	return item, nil
}

func buildLLMThreadTitle(content string, fallback string) string {
	replacer := strings.NewReplacer(
		"\r", " ",
		"\n", " ",
		"`", "",
		"#", "",
		"*", "",
		">", "",
		"[", "",
		"]", "",
		"(", "",
		")", "",
	)
	text := strings.TrimSpace(replacer.Replace(content))
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		if strings.TrimSpace(fallback) != "" {
			return strings.TrimSpace(fallback)
		}
		return "新话题"
	}
	runes := []rune(text)
	if len(runes) > 24 {
		return string(runes[:24]) + "..."
	}
	return text
}

func (s *Server) createChatMessage(threadID int64, llmThreadID *int64, senderID, content string, createdAt time.Time) (int64, error) {
	return s.createChatMessageWithOptions(threadID, llmThreadID, senderID, "text", false, content, nil, "", nil, false, createdAt)
}

func (s *Server) createChatMessageWithMetadata(threadID int64, llmThreadID *int64, senderID, messageType, content string, markdownEntryID *int64, markdownTitle string, latencyMs *int64, createdAt time.Time) (int64, error) {
	return s.createChatMessageWithOptions(threadID, llmThreadID, senderID, messageType, false, content, markdownEntryID, markdownTitle, latencyMs, false, createdAt)
}

func (s *Server) createAttachmentChatMessage(threadID int64, senderID, preview string, att ChatMessageAttachment, createdAt time.Time) (int64, error) {
	// Sender's personal team owns the attachment row. Chat
	// attachments don't pass through the auth-gated download
	// endpoint (their URL stays /uploads/<filename> for FE
	// compat); the workspace_id is purely an audit + data-locality
	// field here.
	workspaceID, err := s.personalWorkspaceForUser(senderID)
	if err != nil {
		return 0, err
	}
	attRow := &Attachment{
		WorkspaceID:    workspaceID,
		UploaderUserID: senderID,
		Filename:       att.FileName,
		MimeType:       att.MimeType,
		SizeBytes:      att.Size,
		StorageKind:    AttachmentStorageLocalFS,
		StorageRef:     att.URL, // /uploads/<filename> or remote URL
		Width:          att.Width,
		Height:         att.Height,
		ThumbnailURL:   att.ThumbnailURL,
	}
	if strings.HasPrefix(att.URL, "http://") || strings.HasPrefix(att.URL, "https://") {
		attRow.StorageKind = AttachmentStorageRemote
	}
	if err := s.insertAttachment(attRow); err != nil {
		return 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var id int64
	err = tx.QueryRow(
		`INSERT INTO chat_messages (thread_id, sender_id, message_type, failed, content, created_at)
		 VALUES ($1, $2, 'attachment', FALSE, $3, $4)
		 RETURNING id`,
		threadID,
		senderID,
		preview,
		createdAt,
	).Scan(&id)
	if err != nil {
		return 0, err
	}

	if _, err = tx.Exec(
		`INSERT INTO chat_message_attachments (message_id, attachment_id, ord) VALUES ($1, $2, 0)`,
		id, attRow.ID,
	); err != nil {
		return 0, err
	}

	if _, err = tx.Exec(
		`UPDATE chat_threads
		    SET last_message = $1, last_message_id = $2, last_message_at = $3
		  WHERE id = $4`,
		preview,
		id,
		createdAt,
		threadID,
	); err != nil {
		return 0, err
	}

	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// personalWorkspaceForUser returns the personal-team workspace id
// for a given user. Used by call paths that need a workspace_id
// for an attachment but don't have one threaded through (chat
// uploads are the obvious case — the sender's personal team is
// the natural owner).
func (s *Server) personalWorkspaceForUser(userID string) (string, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT id FROM teams WHERE owner_user_id = $1 AND kind = 'personal' LIMIT 1`,
		userID,
	).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Server) createChatMessageWithOptions(threadID int64, llmThreadID *int64, senderID, messageType string, failed bool, content string, markdownEntryID *int64, markdownTitle string, latencyMs *int64, streaming bool, createdAt time.Time) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var id int64
	err = tx.QueryRow(
		`INSERT INTO chat_messages (thread_id, llm_thread_id, sender_id, message_type, failed, content, markdown_entry_id, markdown_title, latency_ms, streaming, seq, updated_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0, $11, $11)
		 RETURNING id`,
		threadID,
		llmThreadID,
		senderID,
		messageType,
		failed,
		content,
		markdownEntryID,
		markdownTitle,
		latencyMs,
		streaming,
		createdAt,
	).Scan(&id)
	if err != nil {
		return 0, err
	}

	if _, err = tx.Exec(
		`UPDATE chat_threads
		    SET last_message = $1, last_message_id = $2, last_message_at = $3
		  WHERE id = $4`,
		content,
		id,
		createdAt,
		threadID,
	); err != nil {
		return 0, err
	}
	if llmThreadID != nil {
		var currentTitle string
		if err = tx.QueryRow(`SELECT title FROM llm_threads WHERE id = $1`, *llmThreadID).Scan(&currentTitle); err != nil {
			return 0, err
		}
		if strings.TrimSpace(currentTitle) == "" || strings.TrimSpace(currentTitle) == "新话题" {
			nextTitle := buildLLMThreadTitle(content, markdownTitle)
			if _, err = tx.Exec(
				`UPDATE llm_threads
				    SET title = $1, last_message_at = $2, updated_at = $2
				  WHERE id = $3`,
				nextTitle,
				createdAt,
				*llmThreadID,
			); err != nil {
				return 0, err
			}
		} else {
			if _, err = tx.Exec(
				`UPDATE llm_threads
			    SET last_message_at = $1, updated_at = $1
			  WHERE id = $2`,
				createdAt,
				*llmThreadID,
			); err != nil {
				return 0, err
			}
		}
	}

	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// updateChatMessageContent overwrites the content of a streaming chat
// message and bumps its seq + updated_at. The compound guard prevents
// stale chunks from clobbering a finalized row or one the user has
// revoked. Returns true if a row was updated.
func (s *Server) updateChatMessageContent(messageID int64, content string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE chat_messages
		    SET content = $1,
		        seq = seq + 1,
		        updated_at = NOW()
		  WHERE id = $2 AND streaming = TRUE AND deleted_at IS NULL`,
		content,
		messageID,
	)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// finalizeChatMessage flips streaming=false on a placeholder row and
// commits the final content + optional markdown link + latency. Idempotent.
func (s *Server) finalizeChatMessage(messageID int64, content string, markdownEntryID *int64, markdownTitle string, latencyMs *int64, failed bool) error {
	_, err := s.db.Exec(
		`UPDATE chat_messages
		    SET content = $1,
		        markdown_entry_id = $2,
		        markdown_title = $3,
		        latency_ms = $4,
		        failed = $5,
		        streaming = FALSE,
		        seq = seq + 1,
		        updated_at = NOW()
		  WHERE id = $6 AND deleted_at IS NULL`,
		content,
		markdownEntryID,
		markdownTitle,
		latencyMs,
		failed,
		messageID,
	)
	return err
}

func (s *Server) getChatMessageByID(messageID int64) (*ChatMessage, error) {
	var msg ChatMessage
	var llmThreadID sql.NullInt64
	var markdownEntryID sql.NullInt64
	var latencyMs sql.NullInt64
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime
	var deletedBy sql.NullString
	err := s.db.QueryRow(
		`SELECT m.id, m.thread_id, m.llm_thread_id, m.sender_id, u.username, u.icon_url, m.message_type, m.failed, m.content, m.markdown_entry_id, m.markdown_title, m.latency_ms, m.streaming, m.seq, m.updated_at, m.created_at, m.deleted_at, m.deleted_by
		   FROM chat_messages m
		   JOIN users u ON u.id = m.sender_id
		  WHERE m.id = $1`,
		messageID,
	).Scan(
		&msg.ID,
		&msg.ThreadID,
		&llmThreadID,
		&msg.SenderID,
		&msg.SenderUsername,
		&msg.SenderIcon,
		&msg.MessageType,
		&msg.Failed,
		&msg.Content,
		&markdownEntryID,
		&msg.MarkdownTitle,
		&latencyMs,
		&msg.Streaming,
		&msg.Seq,
		&updatedAt,
		&msg.CreatedAt,
		&deletedAt,
		&deletedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if deletedAt.Valid {
		msg.DeletedAt = &deletedAt.Time
		msg.Deleted = true
		msg.Content = ""
	}
	if llmThreadID.Valid {
		msg.LLMThreadID = &llmThreadID.Int64
	}
	if markdownEntryID.Valid {
		msg.MarkdownEntryID = &markdownEntryID.Int64
	}
	if latencyMs.Valid {
		msg.LatencyMs = &latencyMs.Int64
	}
	if updatedAt.Valid {
		msg.UpdatedAt = &updatedAt.Time
	}
	if deletedBy.Valid {
		msg.DeletedBy = deletedBy.String
	}
	one := []ChatMessage{msg}
	if err := s.attachChatMessageAttachments(one); err != nil {
		return nil, err
	}
	return &one[0], nil
}

func (s *Server) listRecentChatMessages(threadID int64, llmThreadID *int64, limit int) ([]ChatMessage, error) {
	if limit <= 0 {
		limit = 10
	}
	query := `SELECT m.id, m.thread_id, m.llm_thread_id, m.sender_id, u.username, u.icon_url, m.message_type, m.failed, m.content, m.markdown_entry_id, m.markdown_title, m.latency_ms, m.streaming, m.seq, m.updated_at, m.created_at, m.deleted_at, m.deleted_by
		   FROM chat_messages m
		   JOIN users u ON u.id = m.sender_id
		  WHERE m.thread_id = $1`
	args := []any{threadID}
	if llmThreadID != nil {
		query += ` AND m.llm_thread_id = $2 ORDER BY m.created_at DESC LIMIT $3`
		args = append(args, *llmThreadID, limit)
	} else {
		query += ` ORDER BY m.created_at DESC LIMIT $2`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]ChatMessage, 0, limit)
	for rows.Next() {
		var msg ChatMessage
		var llmThreadIDValue sql.NullInt64
		var markdownEntryID sql.NullInt64
		var latencyMs sql.NullInt64
		var updatedAt sql.NullTime
		var deletedAt sql.NullTime
		var deletedBy sql.NullString
		if err := rows.Scan(
			&msg.ID,
			&msg.ThreadID,
			&llmThreadIDValue,
			&msg.SenderID,
			&msg.SenderUsername,
			&msg.SenderIcon,
			&msg.MessageType,
			&msg.Failed,
			&msg.Content,
			&markdownEntryID,
			&msg.MarkdownTitle,
			&latencyMs,
			&msg.Streaming,
			&msg.Seq,
			&updatedAt,
			&msg.CreatedAt,
			&deletedAt,
			&deletedBy,
		); err != nil {
			return nil, err
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
			msg.Deleted = true
		}
		if llmThreadIDValue.Valid {
			msg.LLMThreadID = &llmThreadIDValue.Int64
		}
		if markdownEntryID.Valid {
			msg.MarkdownEntryID = &markdownEntryID.Int64
		}
		if latencyMs.Valid {
			msg.LatencyMs = &latencyMs.Int64
		}
		if updatedAt.Valid {
			msg.UpdatedAt = &updatedAt.Time
		}
		if deletedBy.Valid {
			msg.DeletedBy = deletedBy.String
		}
		items = append(items, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	if err := s.attachChatMessageAttachments(items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Server) findRetrySourceMessage(threadID int64, targetMessage *ChatMessage) (*ChatMessage, error) {
	if targetMessage == nil {
		return nil, nil
	}

	query := `SELECT m.id, m.thread_id, m.llm_thread_id, m.sender_id, u.username, u.icon_url, m.message_type, m.failed, m.content, m.markdown_entry_id, m.markdown_title, m.latency_ms, m.streaming, m.seq, m.updated_at, m.created_at, m.deleted_at, m.deleted_by
	   FROM chat_messages m
	   JOIN users u ON u.id = m.sender_id
	  WHERE m.thread_id = $1
	    AND m.sender_id <> $2
	    AND m.deleted_at IS NULL
	    AND (m.created_at < $3 OR (m.created_at = $3 AND m.id < $4))`
	args := []any{threadID, targetMessage.SenderID, targetMessage.CreatedAt, targetMessage.ID}
	if targetMessage.LLMThreadID != nil {
		query += ` AND m.llm_thread_id = $5`
		args = append(args, *targetMessage.LLMThreadID)
	} else {
		query += ` AND m.llm_thread_id IS NULL`
	}
	query += ` ORDER BY m.created_at DESC, m.id DESC LIMIT 1`

	row := s.db.QueryRow(query, args...)
	var msg ChatMessage
	var llmThreadID sql.NullInt64
	var markdownEntryID sql.NullInt64
	var latencyMs sql.NullInt64
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime
	var deletedBy sql.NullString
	err := row.Scan(
		&msg.ID,
		&msg.ThreadID,
		&llmThreadID,
		&msg.SenderID,
		&msg.SenderUsername,
		&msg.SenderIcon,
		&msg.MessageType,
		&msg.Failed,
		&msg.Content,
		&markdownEntryID,
		&msg.MarkdownTitle,
		&latencyMs,
		&msg.Streaming,
		&msg.Seq,
		&updatedAt,
		&msg.CreatedAt,
		&deletedAt,
		&deletedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if llmThreadID.Valid {
		msg.LLMThreadID = &llmThreadID.Int64
	}
	if markdownEntryID.Valid {
		msg.MarkdownEntryID = &markdownEntryID.Int64
	}
	if latencyMs.Valid {
		msg.LatencyMs = &latencyMs.Int64
	}
	if updatedAt.Valid {
		msg.UpdatedAt = &updatedAt.Time
	}
	if deletedAt.Valid {
		msg.DeletedAt = &deletedAt.Time
		msg.Deleted = true
	}
	if deletedBy.Valid {
		msg.DeletedBy = deletedBy.String
	}
	one := []ChatMessage{msg}
	if err := s.attachChatMessageAttachments(one); err != nil {
		return nil, err
	}
	return &one[0], nil
}

func (s *Server) markChatMessageFailedResolved(threadID, messageID int64, deletedAt time.Time) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE chat_messages
		    SET deleted_at = $1, deleted_by = 'retry'
		  WHERE id = $2 AND thread_id = $3 AND failed = TRUE AND deleted_at IS NULL`,
		deletedAt,
		messageID,
		threadID,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *Server) deleteChatMessage(threadID, messageID int64, userID string, deletedAt time.Time) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var senderID string
	var existingDeletedAt sql.NullTime
	err = tx.QueryRow(
		`SELECT sender_id, deleted_at
		   FROM chat_messages
		  WHERE id = $1 AND thread_id = $2`,
		messageID,
		threadID,
	).Scan(&senderID, &existingDeletedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if senderID != userID {
		return false, errNotMessageOwner
	}
	if existingDeletedAt.Valid {
		return false, nil
	}

	if _, err = tx.Exec(
		`UPDATE chat_messages
		    SET deleted_at = $1, deleted_by = $2
		  WHERE id = $3`,
		deletedAt,
		userID,
		messageID,
	); err != nil {
		return false, err
	}

	var latestID int64
	err = tx.QueryRow(
		`SELECT id FROM chat_messages
		  WHERE thread_id = $1
		  ORDER BY created_at DESC, id DESC
		  LIMIT 1`,
		threadID,
	).Scan(&latestID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if latestID == messageID {
		if _, err = tx.Exec(
			`UPDATE chat_threads
			    SET last_message = $1, last_message_at = $2
			  WHERE id = $3`,
			"消息已撤回",
			deletedAt,
			threadID,
		); err != nil {
			return false, err
		}
	}

	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Server) createPushDelivery(messageID int64, userID, deviceID, pushToken, status, errorMessage string, now time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO push_deliveries (message_id, user_id, device_id, push_token, provider, status, apns_id, error_message, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 'apns', $5, '', $6, $7, $7)`,
		messageID,
		userID,
		strings.TrimSpace(deviceID),
		sanitizePushToken(pushToken),
		strings.TrimSpace(status),
		strings.TrimSpace(errorMessage),
		now,
	)
	return err
}

func (s *Server) claimPendingPushDeliveries(limit int, now time.Time) ([]PushDelivery, error) {
	if limit <= 0 {
		limit = 20
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.Query(
		`SELECT id, message_id, user_id, device_id, push_token, provider, status, apns_id, error_message, created_at, updated_at
		   FROM push_deliveries
		  WHERE status = 'pending'
		  ORDER BY created_at ASC, id ASC
		  LIMIT $1
		  FOR UPDATE SKIP LOCKED`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]PushDelivery, 0, limit)
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var item PushDelivery
		if err := rows.Scan(
			&item.ID,
			&item.MessageID,
			&item.UserID,
			&item.DeviceID,
			&item.PushToken,
			&item.Provider,
			&item.Status,
			&item.APNSID,
			&item.ErrorMessage,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
		ids = append(ids, item.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		tx = nil
		return items, nil
	}

	if _, err := tx.Exec(
		`UPDATE push_deliveries
		    SET status = 'processing',
		        updated_at = $2
		  WHERE id = ANY($1)`,
		pq.Array(ids),
		now,
	); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	tx = nil
	for i := range items {
		items[i].Status = "processing"
		items[i].UpdatedAt = now
	}
	return items, nil
}

func (s *Server) updatePushDeliveryResult(id int64, status, apnsID, errorMessage string, now time.Time) error {
	_, err := s.db.Exec(
		`UPDATE push_deliveries
		    SET status = $2,
		        apns_id = $3,
		        error_message = $4,
		        updated_at = $5
		  WHERE id = $1`,
		id,
		strings.TrimSpace(status),
		strings.TrimSpace(apnsID),
		strings.TrimSpace(errorMessage),
		now,
	)
	return err
}
