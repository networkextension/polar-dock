package dock

// asset_insert.go — internal helper for dual-write paths that need
// to register a file as an asset without going through the HTTP
// /api/assets handler. Today: P8 post-image dual-write (called from
// post_handlers.go after the legacy /uploads/ write succeeds).
//
// Contract: caller already has the bytes on disk at srcPath and a
// known filename + mime. This helper computes sha256, then EITHER
// hands the file off to the existing uploadAssetToProvider (when
// PolarAssetsUseProvider is on) OR moves it into the
// content-addressed local-FS location (the P1 stub). It does NOT
// remove srcPath in either case — the caller controls that lifecycle
// (the post-image case keeps the legacy /uploads/<file> for the
// read path).
//
// Quota: enforced via checkAssetUploadAllowed; over-quota returns
// the appropriate error. Caller-side this surfaces as a log line +
// skipped dual-write — the legacy /uploads/ write already succeeded,
// so the user's action isn't blocked.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/lib/pq"
)

// AssetInsertInput is the dual-write call shape. Mirrors the subset
// of fields handleAssetUpload accepts, but as Go values (not a gin
// context) so background goroutines can call it.
type AssetInsertInput struct {
	WorkspaceID string                 // required; tenant-scoped quota + visibility
	Kind        string                 // e.g. "post-image", "media"
	Name        string                 // e.g. "posts/123/photo.jpg"
	Version     string                 // optional; defaults "v1"
	Visibility  string                 // private|workspace|link|public
	Mime        string
	SrcPath     string                 // file on disk with the bytes to register
	UploadedBy  string                 // user_id; required for the audit row
	Metadata    map[string]any         // arbitrary; serialized via json
	SourceURL   *string                // optional
}

// insertAssetFromFile registers the bytes at srcPath in the assets
// catalog. Returns the inserted *Asset on success. Does NOT remove
// srcPath — caller controls lifecycle.
//
// Behavior matches handleAssetUpload but without the gin / form
// validation layer:
//   - hashes the file
//   - checks workspace quota
//   - provider path (flag on): pickAnyHealthyProvider →
//     uploadAssetToProvider → markAssetLocationReady
//   - local-FS path (flag off): copy to assetBlobPath dst
//   - inserts the row + bumps usage
//
// Unique-violation on (workspace, kind, name, version) is returned
// as-is; caller may treat as "already inserted, no-op".
func (s *Server) insertAssetFromFile(in AssetInsertInput) (*Asset, error) {
	if in.WorkspaceID == "" {
		return nil, errors.New("insertAssetFromFile: WorkspaceID required")
	}
	if in.Kind == "" || in.Name == "" {
		return nil, errors.New("insertAssetFromFile: Kind and Name required")
	}
	if in.SrcPath == "" {
		return nil, errors.New("insertAssetFromFile: SrcPath required")
	}
	if in.Version == "" {
		in.Version = "v1"
	}
	if in.Visibility == "" {
		in.Visibility = "workspace"
	}
	sha, size, err := hashFileSHA256(in.SrcPath)
	if err != nil {
		return nil, fmt.Errorf("hash: %w", err)
	}
	// checkAssetUploadAllowed returns 200 on success (not 0) and a 4xx/5xx
	// on denial — gate on >=400 so a clean check doesn't read as an error.
	if code, msg := s.checkAssetUploadAllowed(in.WorkspaceID, size); code >= 400 {
		return nil, fmt.Errorf("quota %d: %s", code, msg)
	}

	var provider *AssetProvider
	if s.polarAssetsUseProvider {
		provider, err = s.pickAnyHealthyProvider("")
		if err != nil {
			return nil, fmt.Errorf("pick provider: %w", err)
		}
		if provider == nil {
			return nil, errors.New("no healthy asset provider registered")
		}
		filename := filepath.Base(in.Name)
		if err := s.uploadAssetToProvider(provider, sha, in.SrcPath, filename); err != nil {
			return nil, fmt.Errorf("upload to provider %s: %w", provider.Slug, err)
		}
	} else {
		dstPath := assetBlobPath(s.uploadDir, &in.WorkspaceID, sha)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
			return nil, fmt.Errorf("mkdir dst: %w", err)
		}
		if _, err := os.Stat(dstPath); errors.Is(err, os.ErrNotExist) {
			// Copy (not rename) so caller keeps srcPath for legacy
			// read paths that still serve from there.
			if err := copyFile(in.SrcPath, dstPath); err != nil {
				return nil, fmt.Errorf("copy to dst: %w", err)
			}
		}
		// else: dedup hit, existing canonical copy is identical
		// content-addressed.
	}

	var metaBytes []byte
	if len(in.Metadata) > 0 {
		mb, err := json.Marshal(in.Metadata)
		if err != nil {
			return nil, fmt.Errorf("marshal metadata: %w", err)
		}
		metaBytes = mb
	}

	ws := in.WorkspaceID
	asset := &Asset{
		WorkspaceID: &ws,
		Kind:        in.Kind,
		Name:        in.Name,
		Version:     in.Version,
		SHA256:      sha,
		SizeBytes:   size,
		Mime:        in.Mime,
		Visibility:  in.Visibility,
		SourceURL:   in.SourceURL,
		UploadedBy:  in.UploadedBy,
		Metadata:    metaBytes,
	}
	if _, err := s.insertAsset(asset); err != nil {
		// Re-surface unique-violation distinctly so the caller can
		// treat (workspace,kind,name,version) collisions as a no-op
		// instead of an error.
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return nil, fmt.Errorf("asset_exists: %w", err)
		}
		return nil, fmt.Errorf("insert asset: %w", err)
	}
	if err := s.bumpAssetUsage(in.WorkspaceID, size); err != nil {
		log.Printf("assets: bumpAssetUsage (+%d) workspace=%s failed: %v", size, in.WorkspaceID, err)
	}
	if provider != nil {
		if err := s.markAssetLocationReady(asset.ID, provider.ID); err != nil {
			log.Printf("assets: markAssetLocationReady asset=%d provider=%d failed: %v", asset.ID, provider.ID, err)
		}
	}
	return asset, nil
}

// dualWritePostImageToAssets is the P8 hook. Runs in its own goroutine
// from the post upload handler. Registers the post image as an asset
// (kind=post-image), records the resulting asset.id back on
// post_images.asset_id so a future read-path-flip PR can find what's
// already migrated. Never blocks; all failures log + return.
//
// The (post_id, sha256) tuple is the unique identity for a post
// image. We encode it in the asset name as `posts/<post_id>/<filename>`
// so the catalog browses cleanly.
//
// Quota over-cap returns a non-nil error from insertAssetFromFile;
// we log + skip. The legacy /uploads/ write already succeeded so the
// user's post stays fine.
func (s *Server) dualWritePostImageToAssets(postID int64, srcPath, origFilename, origURL, workspaceID, userID string) {
	asset, err := s.insertAssetFromFile(AssetInsertInput{
		WorkspaceID: workspaceID,
		Kind:        "post-image",
		Name:        fmt.Sprintf("posts/%d/%s", postID, origFilename),
		Version:     "v1",
		Visibility:  "workspace",
		Mime:        "image/*",
		SrcPath:     srcPath,
		UploadedBy:  userID,
		Metadata: map[string]any{
			"post_id":      postID,
			"variant":      "original",
			"original_url": origURL,
		},
	})
	if err != nil {
		log.Printf("assets: post-image dual-write skipped post=%d file=%s: %v", postID, origFilename, err)
		return
	}
	if _, err := s.db.Exec(
		`UPDATE post_images SET asset_id = $1 WHERE post_id = $2 AND file_url = $3`,
		asset.ID, postID, origURL,
	); err != nil {
		log.Printf("assets: post-image dual-write update post_images.asset_id failed post=%d asset=%d: %v", postID, asset.ID, err)
	}
}

// copyFile copies src to dst, creating dst with 0o600. Used by the
// local-FS dual-write path so the caller's srcPath survives.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

