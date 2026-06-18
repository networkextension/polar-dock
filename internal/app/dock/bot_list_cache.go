package dock

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// botListCache caches the result of the paginated/summary bots list — the
// hot read path once a workspace holds hundreds of bots. The list query is
// cheap on its own, but the page is hit on every nav and the cache lets us
// serve the common keys (first page, no filter) without touching Postgres.
//
// Invalidation is O(1) via a per-workspace version counter bumped on any bot
// create/update/delete: the version is part of every cache key, so a bump
// makes all of that workspace's stale entries unreachable. Stale entries are
// also TTL-bounded and the whole map is cleared if it grows past a cap, so it
// never leaks unboundedly across many distinct (q, group, offset) keys.
const (
	botListCacheTTL        = 60 * time.Second
	botListCacheMaxEntries = 512
)

type botListCacheValue struct {
	bots      []BotUser
	total     int
	groups    []GroupCount
	storeTime time.Time
}

type botListCache struct {
	mu       sync.Mutex
	entries  map[string]botListCacheValue
	versions map[string]uint64
}

func newBotListCache() *botListCache {
	return &botListCache{
		entries:  map[string]botListCacheValue{},
		versions: map[string]uint64{},
	}
}

// botListCacheKey builds the cache key for one list request. The workspace
// version is embedded so a bump invalidates every prior key for that
// workspace at once.
func botListCacheKey(workspaceID string, version uint64, q, group string, limit, offset int) string {
	var b strings.Builder
	b.WriteString(workspaceID)
	b.WriteByte('|')
	b.WriteString(strconv.FormatUint(version, 10))
	b.WriteString("|q=")
	b.WriteString(q)
	b.WriteString("|g=")
	b.WriteString(group)
	b.WriteString("|l=")
	b.WriteString(strconv.Itoa(limit))
	b.WriteString("|o=")
	b.WriteString(strconv.Itoa(offset))
	return b.String()
}

// version returns the current cache version for a workspace (0 if never bumped).
func (c *botListCache) version(workspaceID string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.versions[workspaceID]
}

// get returns a cached value if present and still within TTL.
func (c *botListCache) get(key string) (botListCacheValue, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	if !ok || time.Since(v.storeTime) >= botListCacheTTL {
		return botListCacheValue{}, false
	}
	return v, true
}

// set stores a value, clearing the map first if it has grown past the cap.
func (c *botListCache) set(key string, v botListCacheValue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= botListCacheMaxEntries {
		c.entries = map[string]botListCacheValue{}
	}
	v.storeTime = time.Now()
	c.entries[key] = v
}

// bump increments a workspace's version, making all of its cached pages
// unreachable on the next lookup. Called after any bot mutation.
func (c *botListCache) bump(workspaceID string) {
	if workspaceID == "" {
		return
	}
	c.mu.Lock()
	c.versions[workspaceID]++
	c.mu.Unlock()
}

// bumpBotListCache is the Server-level helper called from create/update/delete.
// Best-effort: no-op if the cache isn't initialised (e.g. in some tests).
func (s *Server) bumpBotListCache(workspaceID string) {
	if s.botListCache == nil {
		return
	}
	s.botListCache.bump(workspaceID)
}
