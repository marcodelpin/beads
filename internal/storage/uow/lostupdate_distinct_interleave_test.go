package uow

// Deterministic distinct-issue interleave (bda-oxpy measurement, 2026-08-17).
// Writer A opens a transaction and applies a change to issue lud-a; writer B
// then runs a COMPLETE write (open, apply, commit) on issue lud-b; A commits
// last. Under the stale-BEGIN-snapshot hypothesis (the storage/dolt #5740 /
// NEXUS#92 shape) A's commit would revert B's committed row.
//
// MEASURED VERDICT: the hypothesis does NOT hold for the uow shape. On
// dolt-sql-server 2.2.0 (this repo's pinned test image) AND 2.3.0 (the
// deployed fleet server), with the pre-fix single-statement in-tx
// DOLT_COMMIT('-Am'), B's write SURVIVES in 5/5 deterministic interleaves and
// 3/3 four-writer barrier races, with zero serialization retries. The
// difference from the storage/dolt bug: there DOLT_ADD pre-staged the stale
// transaction root as a SEPARATE in-tx statement and DOLT_COMMIT('-m') then
// committed that stale STAGED root; '-Am' stages inside the same statement
// that commits the transaction, so it resolves against the post-merge root.
// This test is kept as a regression guard for that property: it must stay
// green under both the single-statement and the two-phase Commit shapes.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/dbproxy/proxy"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/testutil"
	"github.com/steveyegge/beads/internal/types"
)

func TestUOW_DeterministicDistinctInterleave_NoLostUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a Dolt container and a dbproxy subprocess; skipped in -short")
	}
	port := testutil.StartIsolatedDoltContainer(t)
	portInt, err := strconv.Atoi(port)
	require.NoError(t, err)

	bdBin := buildBDBinary(t)
	prev := proxy.ResolveExecutable
	proxy.ResolveExecutable = func() (string, error) { return bdBin, nil }
	t.Cleanup(func() { proxy.ResolveExecutable = prev })

	t.Setenv("HOME", t.TempDir())

	storeRootDir := t.TempDir()
	shutdownOnInterrupt(t, storeRootDir)
	t.Cleanup(func() {
		if err := proxy.Shutdown(storeRootDir); err != nil {
			t.Logf("proxy.Shutdown(%s): %v", storeRootDir, err)
		}
	})
	logPath := filepath.Join(t.TempDir(), "server.log")

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provider, err := NewExternalDoltServerUOWProvider(
		ctx,
		storeRootDir,
		"beads_lostupdate_det_test",
		logPath,
		configfile.ExternalDoltConfig{Host: "127.0.0.1", Port: portInt},
		"root",
		"",
		0,
		0,
		false,
		"",
	)
	require.NoError(t, err)
	require.NotNil(t, provider)
	t.Cleanup(func() { _ = provider.Close(context.Background()) })

	seed := func(id string) {
		uw, err := provider.NewUOW(ctx)
		require.NoError(t, err)
		defer uw.Close(ctx)
		_, err = uw.IssueUseCase().CreateIssue(ctx, domain.CreateIssueParams{
			Issue: &types.Issue{
				ID:        id,
				Title:     "det target " + id,
				Status:    types.StatusOpen,
				Priority:  2,
				IssueType: types.TypeTask,
				Metadata:  json.RawMessage(`{"seed":"yes"}`),
			},
			ExplicitID: id,
		}, "seeder")
		require.NoError(t, err)
		require.NoError(t, uw.Commit(ctx, "seed "+id))
	}
	seed("lud-a")
	seed("lud-b")

	const iterations = 5
	for it := 0; it < iterations; it++ {
		keyA := fmt.Sprintf("wa%d", it)
		keyB := fmt.Sprintf("wb%d", it)

		// A: open a transaction and apply, but do NOT commit yet.
		uwA, err := provider.NewUOW(ctx)
		require.NoError(t, err)
		_, err = uwA.IssueUseCase().ApplyUpdate(ctx, "lud-a",
			domain.UpdateSpec{Fields: map[string]any{issueops.OpSetMetadata: []string{keyA + "=1"}}}, "writer-a")
		require.NoError(t, err)

		// B: complete write on a DIFFERENT issue while A's tx is open.
		func() {
			uwB, err := provider.NewUOW(ctx)
			require.NoError(t, err)
			defer uwB.Close(ctx)
			_, err = uwB.IssueUseCase().ApplyUpdate(ctx, "lud-b",
				domain.UpdateSpec{Fields: map[string]any{issueops.OpSetMetadata: []string{keyB + "=1"}}}, "writer-b")
			require.NoError(t, err)
			require.NoError(t, uwB.Commit(ctx, "write "+keyB))
		}()

		// A commits last. On a serialization loss, redo the whole unit of work
		// (the RunTx contract); B's landed write must survive either way.
		errA := uwA.Commit(ctx, "write "+keyA)
		uwA.Close(ctx)
		if errA != nil {
			t.Logf("iter %d: A commit returned %v; redoing whole UOW", it, errA)
			require.Truef(t, IsSerializationError(errA), "iter %d: unexpected A commit error class: %v", it, errA)
			uwA2, err := provider.NewUOW(ctx)
			require.NoError(t, err)
			_, err = uwA2.IssueUseCase().ApplyUpdate(ctx, "lud-a",
				domain.UpdateSpec{Fields: map[string]any{issueops.OpSetMetadata: []string{keyA + "=1"}}}, "writer-a")
			require.NoError(t, err)
			require.NoError(t, uwA2.Commit(ctx, "write "+keyA+" redo"))
			uwA2.Close(ctx)
		}

		// Read back BOTH issues on a fresh unit of work.
		uw, err := provider.NewUOW(ctx)
		require.NoError(t, err)
		checkHas := func(id, key string) {
			final, err := uw.IssueUseCase().GetIssue(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, final)
			got := map[string]any{}
			require.NoError(t, json.Unmarshal(final.Metadata, &got))
			_, ok := got[key]
			require.Truef(t, ok, "iter %d: issue %s lost successful write %s (metadata now %v)", it, id, key, got)
		}
		checkHas("lud-b", keyB)
		checkHas("lud-a", keyA)
		uw.Close(ctx)
	}
}
