package dock

// asset_quota_recompute.go — periodic sweeper that re-derives each
// workspace's asset usage_bytes from the canonical SUM(assets.size_bytes).
// Catches drift from any path that forgot to bumpAssetUsage (defensive)
// and refreshes the over_quota flag on cap changes the admin may
// have made via PATCH without immediately recomputing.

import (
	"context"
	"log"
	"time"
)

const assetQuotaRecomputeInterval = 6 * time.Hour

func (s *Server) runAssetQuotaRecompute(ctx context.Context) {
	t := time.NewTicker(assetQuotaRecomputeInterval)
	defer t.Stop()
	// Run once immediately so a freshly-restarted dock doesn't wait
	// 6h before producing accurate numbers in the admin UI.
	s.assetQuotaRecomputeOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.assetQuotaRecomputeOnce()
		}
	}
}

func (s *Server) assetQuotaRecomputeOnce() {
	wsIDs, err := s.listAssetQuotaWorkspaces()
	if err != nil {
		log.Printf("asset quota recompute: list workspaces failed: %v", err)
		return
	}
	var refreshed, failed int
	for _, ws := range wsIDs {
		if err := s.recomputeAssetUsage(ws); err != nil {
			log.Printf("asset quota recompute: workspace=%s err=%v", ws, err)
			failed++
			continue
		}
		refreshed++
	}
	log.Printf("asset quota recompute: workspaces=%d refreshed=%d failed=%d", len(wsIDs), refreshed, failed)
}
