package dock

// asset_warmup.go — P4c automatic cache-miss warm-pull.
//
// Triggered from redirectToProviderBlob when pickProviderForAsset
// signals needsWarmPull (caller region is known but no in-region
// provider has the asset in 'ready' state yet). We:
//
//   1. Look up a healthy provider in the caller's region that does
//      NOT yet have a 'ready' location for this asset.
//   2. Fire-and-forget a goroutine that calls the existing
//      replicateAssetToProvider orchestrator.
//   3. Dedup concurrent triggers with a sync.Map keyed by
//      (assetID, targetProviderID) so 1000 concurrent downloads in
//      region X don't kick off 1000 warm-pulls.
//
// The user's current request still 302s to the cross-region origin
// (returned by the picker); only future requests in the same region
// benefit from the warm-pull. That's by design — blocking the
// download to wait for replication would be much worse UX than a
// one-time cross-region hit.

import (
	"log"
)

// warmupKey is the dedup key for in-flight warm-pulls.
type warmupKey struct {
	AssetID    int64
	ProviderID int64
}

// triggerAssetWarmPullBackground kicks off a P4c warm-pull onto a
// same-region provider, if one is available. No-op when:
//   - callerRegion is empty (no region to warm-pull into)
//   - no healthy in-region provider lacks the asset (already cached
//     in this region, or no providers in this region at all)
//   - a warm-pull for the same (asset, target) is already in flight
//
// Always returns immediately; the actual replication runs in a
// goroutine and logs its outcome.
func (s *Server) triggerAssetWarmPullBackground(assetID int64, callerRegion string) {
	if callerRegion == "" {
		return
	}
	target, err := s.pickHealthyProviderInRegionWithoutAsset(callerRegion, assetID)
	if err != nil {
		log.Printf("assets: bg warm-pull pick failed asset=%d region=%s: %v", assetID, callerRegion, err)
		return
	}
	if target == nil {
		// Nothing healthy in this region that doesn't already have
		// the blob — either the region has no providers at all, or
		// it has providers and they all already hold this asset.
		// Both cases are no-ops.
		return
	}
	key := warmupKey{AssetID: assetID, ProviderID: target.ID}
	if _, busy := s.assetWarmupInFlight.LoadOrStore(key, struct{}{}); busy {
		// Another goroutine is already replicating this (asset,
		// target). Let it finish; this caller just took the
		// cross-region 302.
		return
	}
	go func() {
		defer s.assetWarmupInFlight.Delete(key)
		if _, err := s.replicateAssetToProvider(assetID, target.ID); err != nil {
			log.Printf("assets: bg warm-pull asset=%d target=%s failed: %v", assetID, target.Slug, err)
			return
		}
		log.Printf("assets: bg warm-pull asset=%d target=%s ready", assetID, target.Slug)
	}()
}
