//go:build cgo

package embeddeddolt_test

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// bd-pkim8: RejectStaleUpserts is the transactional half of the import stale
// guard. cmd/bd's filterStaleImportIssues reads local updated_at before the
// batch write, so a local update committing in between would be silently
// overwritten; with the option set, the upsert itself keeps the stored row
// when it is strictly newer than the incoming one.
func TestCreateIssuesRejectStaleUpserts(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	seed := func(t *testing.T, te *testEnv, ctx context.Context, id string) {
		t.Helper()
		err := te.store.CreateIssuesWithFullOptions(ctx, []*types.Issue{{
			ID: id, Title: "local title", Status: types.StatusOpen,
			Priority: 2, IssueType: types.TypeTask,
			CreatedAt: base, UpdatedAt: base.Add(time.Hour),
		}}, "tester", storage.BatchCreateOptions{SkipPrefixValidation: true})
		if err != nil {
			t.Fatalf("seed issue: %v", err)
		}
	}

	upsert := func(t *testing.T, te *testEnv, ctx context.Context, id, title string, updatedAt time.Time, rejectStale bool) {
		t.Helper()
		err := te.store.CreateIssuesWithFullOptions(ctx, []*types.Issue{{
			ID: id, Title: title, Status: types.StatusOpen,
			Priority: 2, IssueType: types.TypeTask,
			CreatedAt: base, UpdatedAt: updatedAt,
		}}, "tester", storage.BatchCreateOptions{
			SkipPrefixValidation: true,
			RejectStaleUpserts:   rejectStale,
		})
		if err != nil {
			t.Fatalf("upsert issue: %v", err)
		}
	}

	title := func(t *testing.T, te *testEnv, ctx context.Context, id string) string {
		t.Helper()
		var got string
		te.queryScalar(t, ctx, "SELECT title FROM issues WHERE id = ?", []any{id}, &got)
		return got
	}

	t.Run("stale_incoming_keeps_local_row", func(t *testing.T) {
		te := newTestEnv(t, "rsa")
		ctx := t.Context()
		seed(t, te, ctx, "rsa-1")

		upsert(t, te, ctx, "rsa-1", "stale snapshot title", base, true)

		if got := title(t, te, ctx, "rsa-1"); got != "local title" {
			t.Fatalf("title = %q, want local row preserved", got)
		}
		var gotUpdated time.Time
		te.queryScalar(t, ctx, "SELECT updated_at FROM issues WHERE id = ?", []any{"rsa-1"}, &gotUpdated)
		if !gotUpdated.UTC().Equal(base.Add(time.Hour)) {
			t.Fatalf("updated_at = %v, want local %v preserved", gotUpdated.UTC(), base.Add(time.Hour))
		}
	})

	t.Run("equal_timestamp_applies", func(t *testing.T) {
		// Equal timestamps win so re-importing the same snapshot stays
		// idempotent, matching the pre-filter's strictly-older semantics.
		te := newTestEnv(t, "rsb")
		ctx := t.Context()
		seed(t, te, ctx, "rsb-1")

		upsert(t, te, ctx, "rsb-1", "equal-time title", base.Add(time.Hour), true)

		if got := title(t, te, ctx, "rsb-1"); got != "equal-time title" {
			t.Fatalf("title = %q, want equal-timestamp upsert applied", got)
		}
	})

	t.Run("newer_incoming_applies", func(t *testing.T) {
		te := newTestEnv(t, "rsc")
		ctx := t.Context()
		seed(t, te, ctx, "rsc-1")

		upsert(t, te, ctx, "rsc-1", "newer title", base.Add(2*time.Hour), true)

		if got := title(t, te, ctx, "rsc-1"); got != "newer title" {
			t.Fatalf("title = %q, want newer upsert applied", got)
		}
		var gotUpdated time.Time
		te.queryScalar(t, ctx, "SELECT updated_at FROM issues WHERE id = ?", []any{"rsc-1"}, &gotUpdated)
		if !gotUpdated.UTC().Equal(base.Add(2 * time.Hour)) {
			t.Fatalf("updated_at = %v, want incoming %v", gotUpdated.UTC(), base.Add(2*time.Hour))
		}
	})

	t.Run("without_flag_stale_overwrites", func(t *testing.T) {
		// --allow-stale path: plain UPSERT semantics, older snapshot wins.
		te := newTestEnv(t, "rsd")
		ctx := t.Context()
		seed(t, te, ctx, "rsd-1")

		upsert(t, te, ctx, "rsd-1", "stale snapshot title", base, false)

		if got := title(t, te, ctx, "rsd-1"); got != "stale snapshot title" {
			t.Fatalf("title = %q, want unguarded upsert to overwrite", got)
		}
	})
}
