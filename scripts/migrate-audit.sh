#!/usr/bin/env bash
# migrate-audit.sh — Audit and batch-migrate beads boards from SQLite to Dolt
#
# Usage:
#   ./scripts/migrate-audit.sh                        # Audit only (dry run)
#   ./scripts/migrate-audit.sh --migrate              # Audit + migrate all eligible boards
#   ./scripts/migrate-audit.sh --migrate --port 3307  # Migrate using specific Dolt server port
#
# This script:
# 1. Finds all .beads/ directories under ~/github (configurable via BEADS_SEARCH_ROOT)
# 2. Reports the state of each board (backend, issue count, schema version)
# 3. Optionally migrates all eligible SQLite boards to Dolt
#
# Safety: always creates backups, never destroys data, reports failures clearly.
#
# Requires: bd, sqlite3, jq
# Fixes GH#2016, GH#2086.

set -euo pipefail

SEARCH_ROOT="${BEADS_SEARCH_ROOT:-$HOME/github}"
MAX_DEPTH="${BEADS_SEARCH_DEPTH:-4}"
DOLT_PORT="${BEADS_DOLT_PORT:-3307}"
DOLT_HOST="${BEADS_DOLT_HOST:-127.0.0.1}"
DO_MIGRATE=false
DRY_RUN=false
EXCLUDE_PATTERN=""

# Parse arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        --migrate) DO_MIGRATE=true; shift ;;
        --dry-run) DO_MIGRATE=true; DRY_RUN=true; shift ;;
        --port) DOLT_PORT="$2"; shift 2 ;;
        --host) DOLT_HOST="$2"; shift 2 ;;
        --root) SEARCH_ROOT="$2"; shift 2 ;;
        --depth) MAX_DEPTH="$2"; shift 2 ;;
        --exclude) EXCLUDE_PATTERN="$2"; shift 2 ;;
        --help|-h)
            echo "Usage: $0 [--migrate] [--dry-run] [--port PORT] [--host HOST] [--root DIR] [--depth N] [--exclude PATTERN]"
            echo ""
            echo "Options:"
            echo "  --migrate       Actually perform migrations (default: audit only)"
            echo "  --dry-run       Show what --migrate would do without making changes"
            echo "  --port PORT     Dolt server port (default: 3307)"
            echo "  --host HOST     Dolt server host (default: 127.0.0.1)"
            echo "  --root DIR      Search root directory (default: ~/github)"
            echo "  --depth N       Max directory depth to search (default: 4)"
            echo "  --exclude PAT   Skip projects matching pattern (e.g., 'backup|test')"
            exit 0
            ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# Check dependencies
for cmd in bd sqlite3 jq; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Error: $cmd is required but not found"
        exit 1
    fi
done

# Colors (if terminal supports them)
if [[ -t 1 ]]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[0;33m'
    BLUE='\033[0;34m'
    NC='\033[0m'
else
    RED='' GREEN='' YELLOW='' BLUE='' NC=''
fi

# Helper: read a JSON field from metadata.json
read_metadata() {
    local file="$1" field="$2"
    jq -r ".${field} // empty" "$file" 2>/dev/null || echo ""
}

# Helper: update metadata.json with server config for migration
write_server_config() {
    local file="$1"
    local tmp="${file}.tmp"
    jq --argjson port "$DOLT_PORT" --arg host "$DOLT_HOST" \
        '. + {dolt_server_port: $port, dolt_server_host: $host, dolt_mode: "server"}' \
        "$file" > "$tmp" && mv "$tmp" "$file"
}

echo "╔══════════════════════════════════════════════════════════════╗"
echo "║            Beads Board Migration Audit                      ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""
echo "Search root: $SEARCH_ROOT"
echo "Max depth:   $MAX_DEPTH"
echo "Dolt server: $DOLT_HOST:$DOLT_PORT"
if $DRY_RUN; then
    echo "Mode:        DRY RUN (showing what --migrate would do)"
elif $DO_MIGRATE; then
    echo "Mode:        MIGRATE"
else
    echo "Mode:        AUDIT ONLY (use --migrate to convert, --dry-run to preview)"
fi
if [[ -n "$EXCLUDE_PATTERN" ]]; then
    echo "Exclude:     $EXCLUDE_PATTERN"
fi
echo ""

# Counters
total=0
sqlite_boards=0
dolt_boards=0
redirect_boards=0
empty_boards=0
migrate_success=0
migrate_fail=0
migrate_skip=0

# Find all .beads directories
while IFS= read -r beads_dir; do
    total=$((total + 1))
    project_dir=$(dirname "$beads_dir")
    project=$(basename "$project_dir")

    # Skip excluded projects
    if [[ -n "$EXCLUDE_PATTERN" ]] && echo "$project" | grep -qE "$EXCLUDE_PATTERN"; then
        continue
    fi

    # Skip redirects
    if [[ -f "$beads_dir/redirect" ]]; then
        target=$(cat "$beads_dir/redirect")
        printf "${BLUE}↪${NC}  %-35s redirect → %s\n" "$project" "$target"
        redirect_boards=$((redirect_boards + 1))
        continue
    fi

    # Skip if no metadata
    if [[ ! -f "$beads_dir/metadata.json" ]]; then
        printf "${YELLOW}?${NC}  %-35s no metadata.json\n" "$project"
        empty_boards=$((empty_boards + 1))
        continue
    fi

    # Read backend
    backend=$(read_metadata "$beads_dir/metadata.json" "backend")

    # Already on Dolt
    if [[ "$backend" == "dolt" ]]; then
        issue_count="?"
        if command -v bd &>/dev/null; then
            issue_count=$(cd "$project_dir" && bd count 2>/dev/null || echo "?")
        fi
        printf "${GREEN}✓${NC}  %-35s dolt (issues: %s)\n" "$project" "$issue_count"
        dolt_boards=$((dolt_boards + 1))
        continue
    fi

    # SQLite backend (empty or explicit "sqlite")
    if [[ -f "$beads_dir/beads.db" ]]; then
        # Check if beads.db is a valid SQLite database (not empty/corrupt)
        file_size=$(stat -f%z "$beads_dir/beads.db" 2>/dev/null || stat -c%s "$beads_dir/beads.db" 2>/dev/null || echo "0")
        if [[ "$file_size" == "0" ]]; then
            printf "${YELLOW}?${NC}  %-35s empty beads.db (0 bytes)\n" "$project"
            empty_boards=$((empty_boards + 1))
            continue
        fi

        issue_count=$(sqlite3 "$beads_dir/beads.db" "SELECT COUNT(*) FROM issues;" 2>/dev/null || echo "err")
        if [[ "$issue_count" == "err" ]]; then
            printf "${YELLOW}?${NC}  %-35s corrupt or unreadable beads.db\n" "$project"
            empty_boards=$((empty_boards + 1))
            continue
        fi

        has_owner=$(sqlite3 "$beads_dir/beads.db" "PRAGMA table_info(issues);" 2>/dev/null | grep -c "owner" || echo "0")

        schema_status="current"
        if [[ "$has_owner" == "0" ]]; then
            schema_status="OLD-SCHEMA"
        fi

        if $DO_MIGRATE; then
            if [[ "$schema_status" == "OLD-SCHEMA" ]]; then
                printf "${YELLOW}⚠${NC}  %-35s sqlite (%s issues, %s) — SKIPPED\n" "$project" "$issue_count" "$schema_status"
                migrate_skip=$((migrate_skip + 1))
            else
                if $DRY_RUN; then
                    printf "${BLUE}→${NC}  %-35s sqlite (%s issues) — WOULD migrate to %s:%s\n" "$project" "$issue_count" "$DOLT_HOST" "$DOLT_PORT"
                    migrate_success=$((migrate_success + 1))
                    sqlite_boards=$((sqlite_boards + 1))
                    continue
                fi

                printf "${BLUE}→${NC}  %-35s sqlite (%s issues) — migrating..." "$project" "$issue_count"

                # Backup metadata.json before modifying
                cp "$beads_dir/metadata.json" "$beads_dir/metadata.json.pre-migrate-audit"

                # Add server config to metadata.json
                write_server_config "$beads_dir/metadata.json"

                # Run bd list to trigger auto-migration
                migrate_output=$(cd "$project_dir" && bd list --json 2>&1) || true

                # Check result
                new_backend=$(read_metadata "$beads_dir/metadata.json" "backend")

                if [[ "$new_backend" == "dolt" ]]; then
                    printf " ${GREEN}✓${NC}\n"
                    # Clean up metadata backup on success
                    rm -f "$beads_dir/metadata.json.pre-migrate-audit"
                    migrate_success=$((migrate_success + 1))
                else
                    # Restore original metadata.json on failure
                    mv "$beads_dir/metadata.json.pre-migrate-audit" "$beads_dir/metadata.json"
                    error_reason=$(echo "$migrate_output" | grep -o "Warning:.*" | head -1)
                    printf " ${RED}✗${NC} %s\n" "${error_reason:-unknown error}"
                    migrate_fail=$((migrate_fail + 1))
                fi
            fi
        else
            printf "${YELLOW}○${NC}  %-35s sqlite (%s issues, schema: %s)\n" "$project" "$issue_count" "$schema_status"
        fi
        sqlite_boards=$((sqlite_boards + 1))
    else
        printf "${YELLOW}?${NC}  %-35s backend=%s but no beads.db\n" "$project" "${backend:-empty}"
        empty_boards=$((empty_boards + 1))
    fi

done < <(find "$SEARCH_ROOT" -name ".beads" -type d -maxdepth "$MAX_DEPTH" 2>/dev/null | sort)

echo ""
echo "════════════════════════════════════════════════════════════════"
echo "Summary:"
echo "  Total boards:   $total"
echo "  Dolt:           $dolt_boards"
echo "  SQLite:         $sqlite_boards"
echo "  Redirect:       $redirect_boards"
echo "  Empty/Other:    $empty_boards"

if $DO_MIGRATE; then
    echo ""
    echo "Migration results:"
    echo "  Succeeded:      $migrate_success"
    echo "  Failed:         $migrate_fail"
    echo "  Skipped:        $migrate_skip"
fi

echo ""
