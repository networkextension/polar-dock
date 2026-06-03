package dock

// Site-icon → central assets migration.
//
// The platform logo was stored as a local /uploads/site_icon_*.png and its
// RELATIVE url rendered in the shared header on EVERY subdomain — so the
// browser resolved it against the current page's origin and it 404'd
// anywhere off the dock's own domain (e.g. hosts.4950.store). We
// single-write it to the central assets catalog and store a stable,
// ABSOLUTE public share url (publicBaseURL + /s/<code>) that resolves the
// same from any origin. Mirrors the blob→assets migration done for the
// plugin modules (doc/arch/blob-storage-to-assets-migration.md).

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func siteIconExtForMime(mime string) string {
	switch {
	case strings.Contains(mime, "jpeg"), strings.Contains(mime, "jpg"):
		return ".jpg"
	case strings.Contains(mime, "svg"):
		return ".svg"
	case strings.Contains(mime, "webp"):
		return ".webp"
	default:
		return ".png"
	}
}

// ensureSiteIconAssetColumn adds the column tracking the icon's catalog
// asset id. Idempotent; called at boot.
func (s *Server) ensureSiteIconAssetColumn() {
	if _, err := s.db.Exec(`ALTER TABLE site_settings ADD COLUMN IF NOT EXISTS icon_asset_id BIGINT`); err != nil {
		log.Printf("site-icon: ensure icon_asset_id column: %v", err)
	}
}

// migrateSiteIconFileToAsset registers the bytes at srcPath in the central
// assets catalog (platform-owned, public) and mints a permanent public
// share link. Returns the absolute share url + the asset id. The staged
// srcPath is the caller's to remove (single-write — no local copy kept).
func (s *Server) migrateSiteIconFileToAsset(srcPath, mime, uploadedBy string) (string, int64, error) {
	ws, err := s.systemWorkspaceID()
	if err != nil || ws == "" {
		return "", 0, fmt.Errorf("site icon asset: system workspace unavailable: %w", err)
	}
	if mime == "" {
		mime = "image/png"
	}
	if uploadedBy == "" {
		uploadedBy = "system"
	}
	asset, err := s.insertAssetFromFile(AssetInsertInput{
		WorkspaceID: ws,
		Kind:        "media",
		Name:        "site/icon/" + randHex(6) + siteIconExtForMime(mime),
		Version:     "v1",
		Visibility:  "public",
		Mime:        mime,
		SrcPath:     srcPath,
		UploadedBy:  uploadedBy,
		Metadata:    map[string]any{"purpose": "site_icon"},
	})
	if err != nil {
		return "", 0, err
	}
	// nil ExpiresAt + nil MaxDownloads = permanent, unlimited.
	link := &AssetShareLink{AssetID: asset.ID, CreatedBy: uploadedBy}
	if _, err := s.insertAssetShareLink(link); err != nil {
		return "", 0, err
	}
	publicURL := strings.TrimRight(s.publicBaseURL, "/") + "/s/" + link.ShortCode
	return publicURL, asset.ID, nil
}

// setSiteIconAsset records the icon's catalog asset id (the icon_url write
// goes through updateSiteIcon).
func (s *Server) setSiteIconAsset(assetID int64) error {
	_, err := s.db.Exec(`UPDATE site_settings SET icon_asset_id = $1 WHERE id = 1`, assetID)
	return err
}

// backfillSiteIconAssetOnce migrates a legacy local /uploads site icon into
// the catalog on boot (idempotent goroutine). No-op once icon_url is an
// absolute share url (or empty).
func (s *Server) backfillSiteIconAssetOnce() {
	settings, err := s.getSiteSettings()
	if err != nil || settings == nil {
		return
	}
	icon := settings.IconURL
	if icon == "" || !strings.HasPrefix(icon, "/uploads/") {
		return // already migrated or unset
	}
	if s.uploadDir == "" {
		return
	}
	local := filepath.Join(s.uploadDir, strings.TrimPrefix(icon, "/uploads/"))
	if _, statErr := os.Stat(local); statErr != nil {
		log.Printf("site-icon: backfill skip (local file gone): %s", local)
		return
	}
	mime := "image/png"
	low := strings.ToLower(local)
	if strings.HasSuffix(low, ".jpg") || strings.HasSuffix(low, ".jpeg") {
		mime = "image/jpeg"
	} else if strings.HasSuffix(low, ".svg") {
		mime = "image/svg+xml"
	} else if strings.HasSuffix(low, ".webp") {
		mime = "image/webp"
	}
	publicURL, assetID, mErr := s.migrateSiteIconFileToAsset(local, mime, "system")
	if mErr != nil {
		log.Printf("site-icon: backfill migrate failed: %v", mErr)
		return
	}
	if err := s.updateSiteIcon(publicURL); err != nil {
		log.Printf("site-icon: backfill updateSiteIcon failed: %v", err)
		return
	}
	_ = s.setSiteIconAsset(assetID)
	log.Printf("site-icon: backfilled local icon → asset:%d (%s)", assetID, publicURL)
}
