package packtunnel

// store.go — typed CRUD for packtunnel_profiles + packtunnel_rule_files
// tables in the polar_packtunnel database. Lifted from dock's
// internal/app/dock/store.go (lines ~3816-4076) and the type
// definitions (~368-439). Receivers + DB field rewritten for the
// *Plugin receiver pattern.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ── Types (lifted verbatim from dock/store.go) ──────────────────────

type PackTunnelProfile struct {
	ID          string                    `json:"id"`
	UserID      string                    `json:"user_id"`
	WorkspaceID string                    `json:"workspace_id"`
	Name        string                    `json:"name"`
	Type        string                    `json:"type"`
	Server      PackTunnelServerEndpoint  `json:"server"`
	Auth        PackTunnelAuth            `json:"auth"`
	Options     PackTunnelOptions         `json:"options"`
	Transport   *PackTunnelTransport      `json:"transport,omitempty"`
	Metadata    PackTunnelProfileMetadata `json:"metadata"`
	CreatedAt   time.Time                 `json:"created_at"`
	UpdatedAt   time.Time                 `json:"updated_at"`
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

// ── Profiles ────────────────────────────────────────────────────────

func (p *Plugin) listPackTunnelProfiles(workspaceID string) ([]PackTunnelProfile, error) {
	rows, err := p.DB.Query(
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

func (p *Plugin) getPackTunnelProfile(workspaceID, id string) (*PackTunnelProfile, error) {
	item, err := scanPackTunnelProfile(p.DB.QueryRow(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), name, profile_type, server_config, auth_config, options_config, transport_config,
		        priority, enabled, editable, source, country_code, country_flag, is_active, created_at, updated_at
		   FROM packtunnel_profiles
		  WHERE workspace_id = $1 AND id = $2`,
		workspaceID, id,
	).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return item, nil
}

func (p *Plugin) getActivePackTunnelProfile(workspaceID string) (*PackTunnelProfile, error) {
	item, err := scanPackTunnelProfile(p.DB.QueryRow(
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

func (p *Plugin) createPackTunnelProfile(workspaceID, ownerUserID string, item PackTunnelProfile, now time.Time) (*PackTunnelProfile, error) {
	if strings.TrimSpace(item.ID) == "" {
		item.ID = generateResourceID()
	}
	serverJSON, authJSON, optionsJSON, transportJSON, err := marshalPackTunnelProfile(item)
	if err != nil {
		return nil, err
	}
	if item.Metadata.IsActive {
		if _, err := p.DB.Exec(`UPDATE packtunnel_profiles SET is_active = FALSE, updated_at = $2 WHERE workspace_id = $1 AND is_active = TRUE`, workspaceID, now); err != nil {
			return nil, err
		}
	}
	created, err := scanPackTunnelProfile(p.DB.QueryRow(
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

func (p *Plugin) updatePackTunnelProfile(workspaceID, id string, item PackTunnelProfile, now time.Time) (*PackTunnelProfile, error) {
	serverJSON, authJSON, optionsJSON, transportJSON, err := marshalPackTunnelProfile(item)
	if err != nil {
		return nil, err
	}
	tx, err := p.DB.Begin()
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
		    SET name = $3, profile_type = $4, server_config = $5, auth_config = $6,
		        options_config = $7, transport_config = $8, priority = $9, enabled = $10,
		        editable = $11, source = $12, country_code = $13, country_flag = $14,
		        is_active = $15, updated_at = $16
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

func (p *Plugin) deletePackTunnelProfile(workspaceID, id string) (bool, error) {
	result, err := p.DB.Exec(`DELETE FROM packtunnel_profiles WHERE workspace_id = $1 AND id = $2`, workspaceID, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (p *Plugin) setActivePackTunnelProfile(workspaceID, id string, now time.Time) (*PackTunnelProfile, error) {
	tx, err := p.DB.Begin()
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
		workspaceID, id, now,
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

// ── Rule files (one per workspace_id) ───────────────────────────────

func (p *Plugin) upsertPackTunnelRuleFile(item PackTunnelRuleFile) error {
	_, err := p.DB.Exec(
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
		item.UserID, item.WorkspaceID, item.FileName, item.StoredName, item.FilePath,
		item.Size, item.ContentType, item.UploadedAt,
	)
	return err
}

func (p *Plugin) getPackTunnelRuleFile(workspaceID string) (*PackTunnelRuleFile, error) {
	var item PackTunnelRuleFile
	err := p.DB.QueryRow(
		`SELECT owner_user_id, COALESCE(workspace_id, ''), file_name, stored_name, file_path, file_size, content_type, uploaded_at
		   FROM packtunnel_rule_files
		  WHERE workspace_id = $1`,
		workspaceID,
	).Scan(
		&item.UserID, &item.WorkspaceID, &item.FileName, &item.StoredName,
		&item.FilePath, &item.Size, &item.ContentType, &item.UploadedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (p *Plugin) deletePackTunnelRuleFile(workspaceID string) (bool, error) {
	result, err := p.DB.Exec(`DELETE FROM packtunnel_rule_files WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// ── Marshal/scan + ID helpers ───────────────────────────────────────

func scanPackTunnelProfile(scan func(dest ...any) error) (*PackTunnelProfile, error) {
	var (
		item          PackTunnelProfile
		serverJSON    []byte
		authJSON      []byte
		optionsJSON   []byte
		transportJSON []byte
	)
	err := scan(
		&item.ID, &item.UserID, &item.WorkspaceID, &item.Name, &item.Type,
		&serverJSON, &authJSON, &optionsJSON, &transportJSON,
		&item.Metadata.Priority, &item.Metadata.Enabled, &item.Metadata.Editable,
		&item.Metadata.Source, &item.Metadata.CountryCode, &item.Metadata.CountryFlag,
		&item.Metadata.IsActive, &item.CreatedAt, &item.UpdatedAt,
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
	}
	return string(serverJSON), string(authJSON), string(optionsJSON), transportJSON, nil
}

func decodePackTunnelJSON(raw []byte, dest any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, dest)
}

// generateResourceID — local copy of dock's helper (same shape: 16
// random bytes → 32-char hex). Used as the default PK on createProfile.
func generateResourceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
