package dock

// skill_updates_handlers.go — P4: "skill updates available" detector.
//
// Wire shape:
//   GET /api/skill-updates
//   → { items: [{host_id, host_name, publisher, kind,
//                installed_version, latest_version, catalog_id}] }
//
// Algorithm:
//   1. Pull the operator's workspace's host_skills (source='bundle')
//      from polar-hosts via /api/hosts + /api/hosts/:id/skills.
//   2. Pull the workspace-visible catalog from polar-hosts via
//      /api/skill-catalog (already workspace-filtered server-side).
//   3. For each installed bundle, find the highest catalog version
//      with matching (publisher, skill_kind). If higher than installed,
//      emit an "update available" row.
//
// Version compare is lexical (semver canonicalisation is out of scope
// here). Catalog rows are listed publisher ASC, version DESC by
// polar-hosts, so the first match per (publisher, kind) is the
// highest known.
//
// Cost: O(hosts * bundles_per_host) per request, plus 1 + N HTTP
// hops to polar-hosts. Acceptable for a UI-triggered call; if we
// ever need a sidebar badge that polls every 30s, move to a
// background goroutine + cached snapshot.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

type skillUpdateItem struct {
	HostID           string `json:"host_id"`
	HostName         string `json:"host_name,omitempty"`
	Publisher        string `json:"publisher"`
	SkillKind        string `json:"skill_kind"`
	InstalledVersion string `json:"installed_version,omitempty"`
	LatestVersion    string `json:"latest_version"`
	CatalogID        string `json:"catalog_id"`
}

func (s *Server) handleSkillUpdatesList(c *gin.Context) {
	workspaceID := strings.TrimSpace(c.GetString("workspace_id"))
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "active workspace required"})
		return
	}
	origin := s.polarHostsOrigin()
	if origin == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "POLAR_HOSTS_TOUCH_URL not configured"})
		return
	}

	// (1) catalog visible to workspace (already filtered by P3 logic).
	catalog, err := fetchCatalogForUpdates(origin, workspaceID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "catalog fetch failed: " + err.Error()})
		return
	}
	// Compute (publisher, kind) → highest catalog row. Catalog API
	// orders version DESC per (pub,kind) so we keep the first match.
	type catKey struct{ pub, kind string }
	latest := map[catKey]catalogEntry{}
	for _, e := range catalog {
		k := catKey{e.Publisher, e.SkillKind}
		if _, seen := latest[k]; !seen {
			latest[k] = e
		}
	}

	// (2) installed bundles across all hosts in workspace.
	hosts, err := fetchWorkspaceHosts(origin, workspaceID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "hosts fetch failed: " + err.Error()})
		return
	}
	out := []skillUpdateItem{}
	for _, h := range hosts {
		skills, err := fetchHostSkills(origin, workspaceID, h.ID)
		if err != nil {
			// One bad host shouldn't poison the whole response — skip.
			continue
		}
		for _, sk := range skills {
			if sk.Source != "bundle" || sk.Publisher == "" {
				continue
			}
			latestEntry, ok := latest[catKey{sk.Publisher, sk.Kind}]
			if !ok {
				continue
			}
			if compareVersions(latestEntry.Version, sk.InstalledVersion) <= 0 {
				continue
			}
			out = append(out, skillUpdateItem{
				HostID:           h.ID,
				HostName:         h.Name,
				Publisher:        sk.Publisher,
				SkillKind:        sk.Kind,
				InstalledVersion: sk.InstalledVersion,
				LatestVersion:    latestEntry.Version,
				CatalogID:        latestEntry.ID,
			})
		}
	}

	// Stable order: by host then publisher then kind.
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostID != out[j].HostID {
			return out[i].HostID < out[j].HostID
		}
		if out[i].Publisher != out[j].Publisher {
			return out[i].Publisher < out[j].Publisher
		}
		return out[i].SkillKind < out[j].SkillKind
	})

	c.JSON(http.StatusOK, gin.H{"items": out, "count": len(out)})
}

// --- helpers ---

// polarHostsOrigin extracts http://host:port from the configured
// POLAR_HOSTS_TOUCH_URL (which always ends in /internal/v1/hosts/touch).
func (s *Server) polarHostsOrigin() string {
	base := strings.TrimSpace(s.polarHostsTouchURL)
	if base == "" {
		return ""
	}
	if idx := strings.Index(base, "/internal/"); idx > 0 {
		return base[:idx]
	}
	return base
}

type wsHost struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func fetchWorkspaceHosts(origin, workspaceID string) ([]wsHost, error) {
	url := fmt.Sprintf("%s/internal/v1/hosts?workspace_id=%s", origin, workspaceID)
	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Endpoint not yet shipped on polar-hosts — return empty rather
		// than fail the whole skill_updates call. Future polar-hosts
		// PR can add /internal/v1/hosts; until then UI shows 0 updates.
		return []wsHost{}, nil
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("hosts GET %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var env struct {
		Items []wsHost `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

type installedSkill struct {
	Kind             string `json:"kind"`
	Publisher        string `json:"publisher"`
	InstalledVersion string `json:"installed_version"`
	Source           string `json:"source"`
}

func fetchHostSkills(origin, workspaceID, hostID string) ([]installedSkill, error) {
	url := fmt.Sprintf("%s/internal/v1/hosts/%s/skills?workspace_id=%s", origin, hostID, workspaceID)
	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return []installedSkill{}, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("host_skills GET HTTP %d", resp.StatusCode)
	}
	var env struct {
		Items []installedSkill `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

func fetchCatalogForUpdates(origin, workspaceID string) ([]catalogEntry, error) {
	url := fmt.Sprintf("%s/internal/v1/skill-catalog?workspace_id=%s", origin, workspaceID)
	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return []catalogEntry{}, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("catalog list HTTP %d", resp.StatusCode)
	}
	var env struct {
		Items []catalogEntry `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

// compareVersions does a dotted-segment compare with numeric segments
// treated numerically. Returns -1 / 0 / 1 like strcmp.
//
// Examples:
//   "0.1.0"  < "0.1.1"
//   "0.2.0"  > "0.1.99"
//   "1.0.0"  > "0.9.99"
//   "1.0.0-rc1" < "1.0.0" (trailing -X treated as pre-release)
//
// Not a full semver impl — that's overkill when the alternative is
// "operator visually inspects the version column". When two versions
// differ but compare equal here, the update simply doesn't show up,
// which is the safe failure mode.
func compareVersions(a, b string) int {
	a, aPre, _ := strings.Cut(a, "-")
	b, bPre, _ := strings.Cut(b, "-")
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		ap := ""
		bp := ""
		if i < len(as) {
			ap = as[i]
		}
		if i < len(bs) {
			bp = bs[i]
		}
		c := compareSegment(ap, bp)
		if c != 0 {
			return c
		}
	}
	// Equal up to here: pre-release loses to plain.
	if aPre == "" && bPre != "" {
		return 1
	}
	if aPre != "" && bPre == "" {
		return -1
	}
	if aPre < bPre {
		return -1
	}
	if aPre > bPre {
		return 1
	}
	return 0
}

func compareSegment(a, b string) int {
	if a == b {
		return 0
	}
	ai, aOK := parseUint(a)
	bi, bOK := parseUint(b)
	if aOK && bOK {
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
		return 0
	}
	if a < b {
		return -1
	}
	return 1
}

func parseUint(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var v uint64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		v = v*10 + uint64(r-'0')
	}
	return v, true
}
