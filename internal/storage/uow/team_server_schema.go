package uow

import (
	"context"
	"fmt"
	"os"

	"github.com/steveyegge/beads/internal/storage/schema"
)

// checkTeamServerSchema verifies that a bts-managed database's schema version
// matches this binary's. In team-server mode bd never migrates, so a version
// mismatch must be refused here with actionable advice — otherwise commands
// fail later with cryptic SQL errors ("column X could not be found in any
// table in scope"). The connection must already have the database selected.
func checkTeamServerSchema(ctx context.Context, conn schema.DBConn, database string) error {
	current, err := schema.CurrentVersion(ctx, conn)
	if err != nil {
		return fmt.Errorf("uow: team-server schema check: %w", err)
	}
	latest := schema.LatestVersion()
	switch {
	case current == 0:
		return fmt.Errorf(
			"uow: database %q has no beads schema — the schema is managed by beads-team-server; ask your operator to run 'bts init' first",
			database)
	case current > latest:
		// Forward drift: the shared DB is newer than this binary. Reuse the
		// standard *SchemaSkewError, whose "your bd binary is stale,
		// rebuild/reinstall" advice is exactly right here; it also honors
		// BD_IGNORE_SCHEMA_SKEW=1.
		return schema.CheckForwardDrift(ctx, conn)
	case current < latest:
		if os.Getenv("BD_IGNORE_SCHEMA_SKEW") == "1" {
			fmt.Fprintf(os.Stderr,
				"Warning: schema skew ignored — database %q is at schema v%d, this bd expects v%d; queries touching newer schema may fail\n",
				database, current, latest)
			return nil
		}
		// Deliberately NOT schema.SchemaBehindError: its "run any bd write
		// command to migrate" advice is exactly wrong for a bts-owned schema.
		return fmt.Errorf(
			"uow: database %q is at schema v%d, this bd expects v%d; the schema is managed by beads-team-server — ask your operator to run 'bts migrate', or use a bd built against schema v%d (set BD_IGNORE_SCHEMA_SKEW=1 to proceed anyway)",
			database, current, latest, current)
	}
	return nil
}
