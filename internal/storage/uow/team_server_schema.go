package uow

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/storage/schema"
)

// checkTeamServerSchema verifies that a bts-managed database's schema version
// matches this binary's. The connection must already have the database selected.
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
		return schema.CheckForwardDrift(ctx, conn)
	case current < latest:
		// No BD_IGNORE_SCHEMA_SKEW hatch here: it would let a newer bd write
		// against an older bts schema. Not SchemaBehindError either: its "run
		// any bd write command to migrate" advice is wrong for a bts-owned schema.
		return fmt.Errorf(
			"uow: database %q is at schema v%d, this bd expects v%d; the schema is managed by beads-team-server — ask your operator to run 'bts migrate', or use a bd built against schema v%d",
			database, current, latest, current)
	}
	return nil
}
