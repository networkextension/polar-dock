package dock

// plugin_releases_store.go — Track 3 (module OTA self-update). Store layer
// for published module binaries and the heartbeat-time resolution that
// decides whether a running module should roll to a new version.
//
// The binary file itself lives under POLAR_PLUGIN_DIST_DIR/<name>/<rel>;
// these rows are its manifest (sha256 + channel + publish time). See
// plugin_binary_handler.go for serving and internal_api_handlers.go for the
// directive the heartbeat hands back.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"
)

// PluginRelease is one row in plugin_releases.
type PluginRelease struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	SHA256      string    `json:"sha256"`
	DistRelPath string    `json:"dist_rel_path"`
	Channel     string    `json:"channel"`
	Notes       string    `json:"notes,omitempty"`
	PublishedAt time.Time `json:"published_at"`
}

// PluginUpdateDirective is the heartbeat-response payload telling a module
// to self-update. URL is a dock-relative path (e.g. /api/plugin/buildings/
// binary?...); the SDK resolves it against its DockBase before fetching, so
// the module pulls over the same channel it heartbeats on.
type PluginUpdateDirective struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// insertPluginRelease records a published binary. Upsert on the PK so
// re-publishing the same (name, version, os, arch) refreshes sha + path +
// publish time (idempotent republish).
func (s *Server) insertPluginRelease(r PluginRelease) error {
	r.Name = strings.TrimSpace(r.Name)
	r.Version = strings.TrimSpace(r.Version)
	r.OS = strings.TrimSpace(strings.ToLower(r.OS))
	r.Arch = strings.TrimSpace(strings.ToLower(r.Arch))
	r.SHA256 = strings.TrimSpace(strings.ToLower(r.SHA256))
	r.DistRelPath = strings.TrimSpace(r.DistRelPath)
	if r.Channel = strings.TrimSpace(r.Channel); r.Channel == "" {
		r.Channel = "stable"
	}
	if r.Name == "" || r.Version == "" || r.OS == "" || r.Arch == "" {
		return errors.New("plugin release: name, version, os, arch required")
	}
	if r.SHA256 == "" || r.DistRelPath == "" {
		return errors.New("plugin release: sha256 and dist_rel_path required")
	}
	now := time.Now().UTC()
	_, err := s.db.Exec(`
		INSERT INTO plugin_releases (name, version, os, arch, sha256,
		    dist_rel_path, channel, notes, published_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (name, version, os, arch) DO UPDATE SET
		    sha256        = EXCLUDED.sha256,
		    dist_rel_path = EXCLUDED.dist_rel_path,
		    channel       = EXCLUDED.channel,
		    notes         = EXCLUDED.notes,
		    published_at  = EXCLUDED.published_at`,
		r.Name, r.Version, r.OS, r.Arch, r.SHA256, r.DistRelPath, r.Channel, r.Notes, now)
	return err
}

// getPluginRelease returns one release row, or (nil, nil) if absent. Used
// by the binary handler (sha + path) and update resolution.
func (s *Server) getPluginRelease(name, version, osName, arch string) (*PluginRelease, error) {
	var r PluginRelease
	err := s.db.QueryRow(`
		SELECT name, version, os, arch, sha256, dist_rel_path, channel, notes, published_at
		  FROM plugin_releases
		 WHERE name=$1 AND version=$2 AND os=$3 AND arch=$4`,
		strings.TrimSpace(name), strings.TrimSpace(version),
		strings.ToLower(strings.TrimSpace(osName)), strings.ToLower(strings.TrimSpace(arch))).
		Scan(&r.Name, &r.Version, &r.OS, &r.Arch, &r.SHA256, &r.DistRelPath, &r.Channel, &r.Notes, &r.PublishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// latestPluginReleaseVersion returns the newest-published stable version for
// a module across any platform, for the admin-list "a newer build exists"
// signal. Empty string if nothing is published. Ordering is by publish time
// (not semver) — matches the "publish periodically" model and dodges semver
// parsing.
func (s *Server) latestPluginReleaseVersion(name string) (string, error) {
	var v sql.NullString
	err := s.db.QueryRow(`
		SELECT version FROM plugin_releases
		 WHERE name=$1 AND channel='stable'
		 ORDER BY published_at DESC LIMIT 1`, strings.TrimSpace(name)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v.String, nil
}

// setPluginTargetVersion pins (or clears, with "") the version a module
// should roll to. Returns false if the module row doesn't exist.
func (s *Server) setPluginTargetVersion(name, version string) (bool, error) {
	res, err := s.db.Exec(`UPDATE plugin_modules SET target_version=$2 WHERE name=$1`,
		strings.TrimSpace(name), strings.TrimSpace(version))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// resolvePluginUpdate decides whether a module reporting runningVer on
// (osName, arch) should self-update, returning the directive or nil.
//
// Resolution: the module's target_version pin if set, else the newest
// published stable release for this exact platform. If the resolved target
// equals the running version, or no binary exists for that target+platform,
// returns nil (nothing to do). The returned URL is dock-relative.
func (s *Server) resolvePluginUpdate(name, runningVer, osName, arch string) (*PluginUpdateDirective, error) {
	name = strings.TrimSpace(name)
	osName = strings.ToLower(strings.TrimSpace(osName))
	arch = strings.ToLower(strings.TrimSpace(arch))
	if name == "" || osName == "" || arch == "" {
		return nil, nil // module didn't report a platform — can't match a binary
	}

	// Pinned target, if any.
	var target sql.NullString
	err := s.db.QueryRow(`SELECT target_version FROM plugin_modules WHERE name=$1`, name).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // unknown module
	}
	if err != nil {
		return nil, err
	}
	resolved := strings.TrimSpace(target.String)

	if resolved == "" {
		// No pin → prefer the polar-release publish system when armed
		// (POLAR_RELEASE_BASE). Its directive URL is the signed provider
		// GET, so the binary bytes bypass dock entirely. Any miss/error
		// falls through to the legacy plugin_releases path below, so a
		// release outage can never block a working module's update.
		if dir, rerr := s.resolvePluginUpdateViaRelease(name, runningVer, osName, arch); rerr != nil {
			log.Printf("dock: release resolve(%s %s-%s) failed, falling back to plugin_releases: %v", name, osName, arch, rerr)
		} else if dir != nil {
			return dir, nil
		}

		// No pin: follow the newest stable release for this platform.
		var v sql.NullString
		err := s.db.QueryRow(`
			SELECT version FROM plugin_releases
			 WHERE name=$1 AND os=$2 AND arch=$3 AND channel='stable'
			 ORDER BY published_at DESC LIMIT 1`, name, osName, arch).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		resolved = strings.TrimSpace(v.String)
	}
	if resolved == "" || resolved == strings.TrimSpace(runningVer) {
		return nil, nil // nothing to do
	}

	// Need an actual binary for the resolved target on this platform.
	rel, err := s.getPluginRelease(name, resolved, osName, arch)
	if err != nil || rel == nil {
		return nil, err
	}
	return &PluginUpdateDirective{
		Version: rel.Version,
		URL: fmt.Sprintf("/api/plugin/%s/binary?version=%s&os=%s&arch=%s",
			url.PathEscape(name), url.QueryEscape(rel.Version), osName, arch),
		SHA256: rel.SHA256,
	}, nil
}
