#!/usr/bin/env bash
# packtunnel-data.sh — copy packtunnel_* tables from ideamesh →
# polar_packtunnel. Same shape as wg-data.sh / iosdist-data.sh.
set -euo pipefail

SRC_DSN="${SRC_DSN:-postgres://ideamesh:test123456@127.0.0.1:5432/ideamesh}"
DST_DSN="${DST_DSN:-postgres://ideamesh:test123456@127.0.0.1:5432/polar_packtunnel}"
PSQL="${PSQL:-/Applications/Postgres.app/Contents/Versions/latest/bin/psql}"
PG_DUMP="${PG_DUMP:-/Applications/Postgres.app/Contents/Versions/latest/bin/pg_dump}"

APPLY=0
if [[ "${1:-}" == "--apply" ]]; then APPLY=1; fi

TABLES=(packtunnel_profiles packtunnel_rule_files)

echo "=== packtunnel-data.sh — $(if [[ $APPLY -eq 1 ]]; then echo APPLY; else echo DRY-RUN; fi) ==="
echo "source: $SRC_DSN"
echo "target: $DST_DSN"
echo
echo "--- source row counts ---"
for t in "${TABLES[@]}"; do
    n=$("$PSQL" "$SRC_DSN" -At -c "SELECT COUNT(*) FROM $t;" 2>/dev/null || echo "ERR")
    printf "  %-25s %s\n" "$t" "$n"
done
echo
if [[ $APPLY -eq 0 ]]; then
    echo "Dry run — pass --apply to perform the copy."
    exit 0
fi

TMPDIR=$(mktemp -d -t ptmigrate)
trap 'rm -rf "$TMPDIR"' EXIT
DUMP="$TMPDIR/packtunnel-data.sql"
"$PG_DUMP" "$SRC_DSN" --data-only --column-inserts --no-owner --no-privileges \
    $(printf -- '--table=%s ' "${TABLES[@]}") > "$DUMP"
echo "wrote $(wc -l < "$DUMP") lines to $DUMP"
{
    echo "BEGIN;"
    for t in "${TABLES[@]}"; do echo "TRUNCATE $t RESTART IDENTITY CASCADE;"; done
    cat "$DUMP"
    echo "COMMIT;"
} | "$PSQL" "$DST_DSN" -v ON_ERROR_STOP=1
echo
echo "--- target row counts (post-load) ---"
for t in "${TABLES[@]}"; do
    n=$("$PSQL" "$DST_DSN" -At -c "SELECT COUNT(*) FROM $t;")
    printf "  %-25s %s\n" "$t" "$n"
done
echo "Done."
