package dock

import (
	"database/sql"
	"log"
)

// assetsSchemaPresent verifies that the 6 assets-module tables actually
// applied. The schema string is idempotent so this should always pass on
// a fresh boot — but a binary that pre-dates this section running against
// a newer DB (or vice-versa) would otherwise silently fail at first query
// time. Logging at boot makes the mismatch loud without blocking startup.
func assetsSchemaPresent(db *sql.DB) {
	required := []string{
		"assets",
		"asset_providers",
		"asset_locations",
		"asset_share_links",
		"asset_quotas",
		"asset_usage_events",
	}
	for _, t := range required {
		var ok bool
		err := db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'r')`,
			t,
		).Scan(&ok)
		if err != nil {
			log.Printf("assets: schema check %q: query error: %v", t, err)
			continue
		}
		if !ok {
			log.Printf("assets: schema check %q: MISSING — schema migration did not apply", t)
		}
	}
}
