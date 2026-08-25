package db

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/steveyegge/beads/internal/labelns"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// TestLabelSQLRepositoryExclusiveNamespaces pins the proxied/UOW write
// path's enforcement of labels.exclusive-prefixes (bd-7u5ki): before this
// fix, labelSQLRepositoryImpl.Insert did a raw INSERT IGNORE that never
// called issueops.EnforceExclusiveLabelInTx, so a proxied 'bd label add'
// silently accepted a second label in a configured-exclusive namespace even
// though the embedded/classic route (AddLabelInTx) already rejected it.
func (s *testSuite) TestLabelSQLRepositoryExclusiveNamespaces() {
	s.Run("RejectsSecondLabelInConfiguredNamespace", s.labelInsertExclusiveRejects)
	s.Run("AllowsSecondLabelWhenUnconfigured", s.labelInsertExclusiveUnconfiguredAllows)
	s.Run("AllowsSecondLabelInDifferentNamespace", s.labelInsertExclusiveDifferentNamespace)
	s.Run("IdempotentReAddOfSameLabelStillAllowed", s.labelInsertExclusiveIdempotentSameLabel)
}

func (s *testSuite) setExclusivePrefixes(raw string) {
	s.T().Helper()
	_, err := s.Runner().ExecContext(s.Ctx(),
		"REPLACE INTO config (`key`, value) VALUES (?, ?)", labelns.ConfigKey, raw)
	s.Require().NoError(err)
}

// clearExclusivePrefixes undoes setExclusivePrefixes. SetupTest resets the
// database once per exported TestXxx method, not once per s.Run subtest, so
// a subtest that configures labels.exclusive-prefixes leaks it into every
// later subtest in the SAME method unless it cleans up after itself.
func (s *testSuite) clearExclusivePrefixes() {
	s.T().Helper()
	_, err := s.Runner().ExecContext(s.Ctx(), "DELETE FROM config WHERE `key` = ?", labelns.ConfigKey)
	s.Require().NoError(err)
}

func (s *testSuite) labelInsertExclusiveRejects() {
	defer s.clearExclusivePrefixes()
	s.setExclusivePrefixes("tier:,review:")
	s.seedIssueRow("bd-lbl-excl-1")
	r := s.labelRepo()
	s.Require().NoError(r.Insert(s.Ctx(), "bd-lbl-excl-1", "tier:fable", "tester", domain.LabelOpts{}))

	err := r.Insert(s.Ctx(), "bd-lbl-excl-1", "tier:opus", "tester", domain.LabelOpts{})
	s.Require().Error(err, "a second label in a configured-exclusive namespace must be rejected by the proxied write path")
	s.Contains(err.Error(), "exclusive")
	s.Contains(err.Error(), "tier:fable", "the error should name the existing label")

	out, lerr := r.List(s.Ctx(), "bd-lbl-excl-1", domain.LabelOpts{})
	s.Require().NoError(lerr)
	s.Equal([]string{"tier:fable"}, out, "the rejected add must not have landed")
}

func (s *testSuite) labelInsertExclusiveUnconfiguredAllows() {
	// No labels.exclusive-prefixes set: both labels coexist, matching
	// pre-#4757 behavior and the direct route's own unconfigured case.
	s.seedIssueRow("bd-lbl-excl-2")
	r := s.labelRepo()
	s.Require().NoError(r.Insert(s.Ctx(), "bd-lbl-excl-2", "tier:fable", "tester", domain.LabelOpts{}))
	s.Require().NoError(r.Insert(s.Ctx(), "bd-lbl-excl-2", "tier:opus", "tester", domain.LabelOpts{}))

	out, err := r.List(s.Ctx(), "bd-lbl-excl-2", domain.LabelOpts{})
	s.Require().NoError(err)
	s.ElementsMatch([]string{"tier:fable", "tier:opus"}, out)
}

func (s *testSuite) labelInsertExclusiveDifferentNamespace() {
	defer s.clearExclusivePrefixes()
	// tier: is exclusive; topic: is not configured, so two topic: labels
	// coexist on the same issue even with tier: enforcement active.
	s.setExclusivePrefixes("tier:")
	s.seedIssueRow("bd-lbl-excl-3")
	r := s.labelRepo()
	s.Require().NoError(r.Insert(s.Ctx(), "bd-lbl-excl-3", "tier:fable", "tester", domain.LabelOpts{}))
	s.Require().NoError(r.Insert(s.Ctx(), "bd-lbl-excl-3", "topic:storage", "tester", domain.LabelOpts{}))
	s.Require().NoError(r.Insert(s.Ctx(), "bd-lbl-excl-3", "topic:cli", "tester", domain.LabelOpts{}))

	out, err := r.List(s.Ctx(), "bd-lbl-excl-3", domain.LabelOpts{})
	s.Require().NoError(err)
	s.ElementsMatch([]string{"tier:fable", "topic:storage", "topic:cli"}, out)
}

func (s *testSuite) labelInsertExclusiveIdempotentSameLabel() {
	defer s.clearExclusivePrefixes()
	s.setExclusivePrefixes("tier:")
	s.seedIssueRow("bd-lbl-excl-4")
	r := s.labelRepo()
	s.Require().NoError(r.Insert(s.Ctx(), "bd-lbl-excl-4", "tier:fable", "tester", domain.LabelOpts{}))
	// Re-adding the SAME label is not a namespace violation (it is the label
	// already on record, not a second distinct one) - must stay a no-op
	// success, not an "exclusive" rejection.
	err := r.Insert(s.Ctx(), "bd-lbl-excl-4", "tier:fable", "tester", domain.LabelOpts{})
	s.Require().NoError(err, "re-adding the existing label must stay idempotent")
	s.False(strings.Contains(errString(err), "exclusive"))

	out, lerr := r.List(s.Ctx(), "bd-lbl-excl-4", domain.LabelOpts{})
	s.Require().NoError(lerr)
	s.Equal([]string{"tier:fable"}, out)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// isSerializationError reports a MySQL/Dolt error that guarantees the
// transaction was rolled back and is safe to redo whole - the same class
// internal/storage/uow.IsSerializationError checks, duplicated narrowly here
// because uow imports this package (db), not the other way around.
func isSerializationError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && (mysqlErr.Number == 1213 || mysqlErr.Number == 1205) {
		return true
	}
	var stateErr interface{ SQLState() string }
	if errors.As(err, &stateErr) {
		return stateErr.SQLState() == "40001" || stateErr.SQLState() == "40P01"
	}
	return false
}

// addLabelInTxWithRetry runs issueops.AddLabelInTx in its OWN fresh
// transaction, redoing the WHOLE read-check-write on a serialization
// conflict - the same discipline internal/storage/uow.RunTxResultWithin and
// internal/storage/dolt.DoltStore.withRetryTx apply in production, inlined
// here because this test drives *sql.Tx directly instead of through either
// production retry wrapper.
// ready/proceed implement the two-phase rendezvous bda-5vzi added: on the
// FIRST attempt each writer opens its transaction, PINS ITS SNAPSHOT with a
// read of the labels table (REPEATABLE READ fixes the read view at the first
// read), signals ready, and only calls AddLabelInTx once EVERY writer has
// pinned. That removes the scheduler dependence the cross-model review
// found: without it, nothing stopped one transaction committing before the
// other had even read, which produces the expected one-winner outcome with
// no lock involved - a green that proves nothing. With the pin, neither
// first-attempt check can see the other's commit, so on UNFIXED code both
// writers pass the check and both commit (red by construction, not by
// scheduling luck). Retries skip the rendezvous: a retried transaction
// legitimately reads fresh state and sees the winner.
func (s *testSuite) addLabelInTxWithRetry(issueID, label string, ready chan<- struct{}, proceed <-chan struct{}) error {
	first := true
	for attempt := 0; attempt < 20; attempt++ {
		tx, err := s.db.BeginTx(s.Ctx(), nil)
		if err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		if first {
			var pinned int
			if scanErr := tx.QueryRowContext(s.Ctx(),
				"SELECT COUNT(*) FROM labels WHERE issue_id = ?", issueID,
			).Scan(&pinned); scanErr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("pin snapshot: %w", scanErr)
			}
			ready <- struct{}{}
			<-proceed
			first = false
		}
		addErr := issueops.AddLabelInTx(s.Ctx(), tx, "labels", "events", issueID, label, "tester")
		if addErr != nil {
			_ = tx.Rollback()
			if isSerializationError(addErr) {
				continue
			}
			return addErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			if isSerializationError(commitErr) {
				continue
			}
			return fmt.Errorf("commit: %w", commitErr)
		}
		return nil
	}
	return fmt.Errorf("addLabelInTxWithRetry: exhausted retries for %s/%s", issueID, label)
}

// TestLabelExclusiveConcurrentAddsSerializeToOneWinner is the review-demanded
// two-writer regression for the check-then-insert race the review of
// upstream PR #4757 (bd-7u5ki) found: two writers on SEPARATE connections
// (two independent *sql.Tx from the SAME *sql.DB, which carries no
// MaxOpenConns limit at this layer - see the note in cmd/bd's
// label_exclusive_test.go for why that test cannot exercise this) adding
// DIFFERENT labels into the SAME exclusive namespace on the SAME issue must
// end with exactly ONE tier: label, deterministically.
//
// Before the per-(issue, namespace) lock (acquireLabelNamespaceLockInTx,
// migration 0066), both writers' check would read "no violation" (neither
// had committed yet), and Dolt's cell-level merge accepts two INSERTs at
// different primary keys (issue_id, "tier:a") / (issue_id, "tier:b") as
// independent, non-conflicting changes - so BOTH commits would land, leaving
// the issue with two tier: labels. The lock forces both transactions to
// rewrite the SAME lock-row cell, so one always collides at commit and the
// retry above redoes the whole transaction against a fresh read that now
// sees the winner's label.
//
// Run 5x per the review's own bar for a race-prone fix
// (determinism-gate-before-green): a single green run is not proof.
func (s *testSuite) TestLabelExclusiveConcurrentAddsSerializeToOneWinner() {
	s.setExclusivePrefixes("tier:,review:")
	defer s.clearExclusivePrefixes()

	for rep := 0; rep < 5; rep++ {
		s.Run(fmt.Sprintf("rep%d", rep), func() {
			issueID := fmt.Sprintf("bd-lbl-race-%d", rep)
			s.seedIssueRow(issueID)

			labelsToAdd := []string{"tier:fable", "tier:opus"}
			errs := make([]error, len(labelsToAdd))
			ready := make(chan struct{}, len(labelsToAdd))
			proceed := make(chan struct{})
			var wg sync.WaitGroup
			for i, label := range labelsToAdd {
				wg.Add(1)
				go func(i int, label string) {
					defer wg.Done()
					errs[i] = s.addLabelInTxWithRetry(issueID, label, ready, proceed)
				}(i, label)
			}
			// Two-phase barrier: wait until EVERY writer has opened its
			// transaction and pinned its read view, then release them all.
			for range labelsToAdd {
				<-ready
			}
			close(proceed)
			wg.Wait()

			successCount := 0
			var loserErr error
			for _, err := range errs {
				if err == nil {
					successCount++
				} else {
					loserErr = err
				}
			}
			s.Equal(1, successCount, "expected exactly 1 of 2 concurrent adds to succeed, errs=%v", errs)
			s.Require().Error(loserErr, "expected the losing writer to fail")
			s.Contains(loserErr.Error(), "exclusive", "the loser must fail with the exclusive-namespace rejection, not some other error")

			labels, err := s.labelRepo().List(s.Ctx(), issueID, domain.LabelOpts{})
			s.Require().NoError(err)
			tierLabels := 0
			for _, l := range labels {
				if strings.HasPrefix(l, "tier:") {
					tierLabels++
				}
			}
			s.Equal(1, tierLabels, "expected exactly 1 tier: label after the race, got %v", labels)
		})
	}
}

// persistLabelsInTxWithRetry runs issueops.PersistLabels (the create/import
// chokepoint) in its OWN fresh transaction with the same whole-transaction
// serialization-retry + two-phase snapshot-pin rendezvous discipline as
// addLabelInTxWithRetry above. Returns the warn-callback count from the
// attempt that COMMITTED - callbacks fired by rolled-back attempts are
// discarded, mirroring the "callers should dedup" contract on
// storage.BatchCreateOptions.OnExclusiveLabelConflict.
func (s *testSuite) persistLabelsInTxWithRetry(issueID string, labels []string, warn bool, ready chan<- struct{}, proceed <-chan struct{}) (int, error) {
	first := true
	for attempt := 0; attempt < 20; attempt++ {
		tx, err := s.db.BeginTx(s.Ctx(), nil)
		if err != nil {
			return 0, fmt.Errorf("begin: %w", err)
		}
		if first {
			var pinned int
			if scanErr := tx.QueryRowContext(s.Ctx(),
				"SELECT COUNT(*) FROM labels WHERE issue_id = ?", issueID,
			).Scan(&pinned); scanErr != nil {
				_ = tx.Rollback()
				return 0, fmt.Errorf("pin snapshot: %w", scanErr)
			}
			ready <- struct{}{}
			<-proceed
			first = false
		}
		warned := 0
		bc := &issueops.BatchContext{
			ExclusiveLabelPrefixes: labelns.ParsePrefixes("tier:,review:"),
			Opts: storage.BatchCreateOptions{
				ExclusiveLabelConflictWarn: warn,
				OnExclusiveLabelConflict:   func(string, string, []string) { warned++ },
			},
		}
		issue := &types.Issue{ID: issueID, Labels: labels}
		if _, persistErr := issueops.PersistLabels(s.Ctx(), tx, bc, issue, "tester", "events"); persistErr != nil {
			_ = tx.Rollback()
			if isSerializationError(persistErr) {
				continue
			}
			return 0, persistErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			if isSerializationError(commitErr) {
				continue
			}
			return 0, fmt.Errorf("commit: %w", commitErr)
		}
		return warned, nil
	}
	return 0, fmt.Errorf("persistLabelsInTxWithRetry: exhausted retries for %s", issueID)
}

// assertOneTierWinner is the shared postcondition for the enforce-mode
// two-writer scenarios: exactly one writer succeeded, the loser failed with
// the exclusive-namespace rejection, and exactly one tier: label survives.
func (s *testSuite) assertOneTierWinner(issueID string, errs []error) {
	s.T().Helper()
	successCount := 0
	var loserErr error
	for _, err := range errs {
		if err == nil {
			successCount++
		} else {
			loserErr = err
		}
	}
	s.Equal(1, successCount, "expected exactly 1 of 2 concurrent writers to succeed, errs=%v", errs)
	s.Require().Error(loserErr, "expected the losing writer to fail")
	s.Contains(loserErr.Error(), "exclusive", "the loser must fail with the exclusive-namespace rejection, not some other error")
	s.Equal(1, s.countTierLabels(issueID), "expected exactly 1 tier: label after the race")
}

func (s *testSuite) countTierLabels(issueID string) int {
	s.T().Helper()
	labels, err := s.labelRepo().List(s.Ctx(), issueID, domain.LabelOpts{})
	s.Require().NoError(err)
	tierLabels := 0
	for _, l := range labels {
		if strings.HasPrefix(l, "tier:") {
			tierLabels++
		}
	}
	return tierLabels
}

// TestLabelExclusivePersistPathSerializesWithConcurrentWriters is the
// two-writer regression for the SECOND half of the #4757 review finding
// (bda-e5b1): the label_namespace_locks row only serializes writers who BOTH
// rewrite it, and checkExclusiveLabelsForPersist - on the PersistLabels
// chokepoint EVERY bd create and EVERY import row goes through - never
// touched it. A create/import upsert therefore could not collide with a
// concurrent AddLabelInTx ("mixed", the exact interleaving the cross-model
// review constructed) or with another upsert ("upsert-upsert"), and both
// commits landed two labels into one exclusive namespace with neither writer
// seeing a violation. Same two-phase snapshot-pin rendezvous as
// TestLabelExclusiveConcurrentAddsSerializeToOneWinner, so the pre-fix
// outcome is red by construction, not by scheduling luck; same 5x repetition
// per determinism-gate-before-green.
//
// "warn-vs-enforce" pins the WARN-MODE half of the fix: an import-mode
// (warn) upsert must take the lock too, else an ENFORCING peer both lands
// its label and records nothing - the invariant is "no silent violation":
// either one tier: label survives (the peer was refused) or two survive AND
// the warn writer reported the conflict through its callback.
func (s *testSuite) TestLabelExclusivePersistPathSerializesWithConcurrentWriters() {
	s.setExclusivePrefixes("tier:,review:")
	defer s.clearExclusivePrefixes()

	for rep := 0; rep < 5; rep++ {
		s.Run(fmt.Sprintf("mixed-rep%d", rep), func() {
			issueID := fmt.Sprintf("bd-lbl-prace-m%d", rep)
			s.seedIssueRow(issueID)
			var persistErr, addErr error
			ready := make(chan struct{}, 2)
			proceed := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, persistErr = s.persistLabelsInTxWithRetry(issueID, []string{"tier:fable"}, false, ready, proceed)
			}()
			go func() {
				defer wg.Done()
				addErr = s.addLabelInTxWithRetry(issueID, "tier:opus", ready, proceed)
			}()
			<-ready
			<-ready
			close(proceed)
			wg.Wait()
			s.assertOneTierWinner(issueID, []error{persistErr, addErr})
		})

		s.Run(fmt.Sprintf("upsert-upsert-rep%d", rep), func() {
			issueID := fmt.Sprintf("bd-lbl-prace-u%d", rep)
			s.seedIssueRow(issueID)
			labelsToAdd := []string{"tier:fable", "tier:opus"}
			errs := make([]error, len(labelsToAdd))
			ready := make(chan struct{}, len(labelsToAdd))
			proceed := make(chan struct{})
			var wg sync.WaitGroup
			for i, label := range labelsToAdd {
				wg.Add(1)
				go func(i int, label string) {
					defer wg.Done()
					_, errs[i] = s.persistLabelsInTxWithRetry(issueID, []string{label}, false, ready, proceed)
				}(i, label)
			}
			for range labelsToAdd {
				<-ready
			}
			close(proceed)
			wg.Wait()
			s.assertOneTierWinner(issueID, errs)
		})

		s.Run(fmt.Sprintf("warn-vs-enforce-rep%d", rep), func() {
			issueID := fmt.Sprintf("bd-lbl-prace-w%d", rep)
			s.seedIssueRow(issueID)
			var warned int
			var persistErr, addErr error
			ready := make(chan struct{}, 2)
			proceed := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				warned, persistErr = s.persistLabelsInTxWithRetry(issueID, []string{"tier:fable"}, true, ready, proceed)
			}()
			go func() {
				defer wg.Done()
				addErr = s.addLabelInTxWithRetry(issueID, "tier:opus", ready, proceed)
			}()
			<-ready
			<-ready
			close(proceed)
			wg.Wait()

			// The warn-mode writer never hard-fails on a conflict.
			s.Require().NoError(persistErr, "warn-mode upsert must not hard-fail")
			switch s.countTierLabels(issueID) {
			case 1:
				// The enforcing peer lost the lock-cell collision, retried
				// against a fresh read and was correctly refused.
				s.Require().Error(addErr, "with 1 surviving tier: label the enforcing add must have been refused")
				s.Contains(addErr.Error(), "exclusive")
			case 2:
				// The warn writer lost, retried, saw the peer's label and kept
				// its own AS WRITTEN - permitted, but only WITH the violation
				// reported. Two labels with warned==0 is the pre-fix silent
				// double-land this scenario exists to catch.
				s.Require().NoError(addErr)
				s.GreaterOrEqual(warned, 1, "two tier: labels may only coexist when the warn-mode writer REPORTED the violation")
			default:
				s.Failf("unexpected tier: label count", "issue %s", issueID)
			}
		})
	}
}

// proxiedInsertWithRetry is addLabelInTxWithRetry's sibling through the
// PROXIED repository path (bda-5vzi half 2): the writer drives
// labelSQLRepositoryImpl.Insert bound to its own *sql.Tx - the exact
// entry point a proxied 'bd label add' reaches through the UOW - instead
// of calling the embedded issueops.AddLabelInTx directly. Same two-phase
// snapshot-pin rendezvous, same whole-transaction serialization retry;
// only the guarded path under test differs, which is the point: the two
// write stacks share issueops.EnforceExclusiveLabelInTx, and this test is
// what notices if the proxied stack ever stops routing through it.
func (s *testSuite) proxiedInsertWithRetry(issueID, label string, ready chan<- struct{}, proceed <-chan struct{}) error {
	first := true
	for attempt := 0; attempt < 20; attempt++ {
		tx, err := s.db.BeginTx(s.Ctx(), nil)
		if err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		if first {
			var pinned int
			if scanErr := tx.QueryRowContext(s.Ctx(),
				"SELECT COUNT(*) FROM labels WHERE issue_id = ?", issueID,
			).Scan(&pinned); scanErr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("pin snapshot: %w", scanErr)
			}
			ready <- struct{}{}
			<-proceed
			first = false
		}
		addErr := NewLabelSQLRepository(tx).Insert(s.Ctx(), issueID, label, "tester", domain.LabelOpts{})
		if addErr != nil {
			_ = tx.Rollback()
			if isSerializationError(addErr) {
				continue
			}
			return addErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			if isSerializationError(commitErr) {
				continue
			}
			return fmt.Errorf("commit: %w", commitErr)
		}
		return nil
	}
	return fmt.Errorf("proxiedInsertWithRetry: exhausted retries for %s/%s", issueID, label)
}

// TestLabelExclusiveConcurrentProxiedInsertsSerializeToOneWinner is the
// proxied-route twin of TestLabelExclusiveConcurrentAddsSerializeToOneWinner.
// The direct-route test proves the lock serializes issueops.AddLabelInTx;
// this one proves the SAME property for the repository Insert the proxied
// server uses. Run 5x per determinism-gate-before-green, same as the twin.
func (s *testSuite) TestLabelExclusiveConcurrentProxiedInsertsSerializeToOneWinner() {
	s.setExclusivePrefixes("tier:,review:")
	defer s.clearExclusivePrefixes()

	for rep := 0; rep < 5; rep++ {
		s.Run(fmt.Sprintf("rep%d", rep), func() {
			issueID := fmt.Sprintf("bd-lbl-proxrace-%d", rep)
			s.seedIssueRow(issueID)

			labelsToAdd := []string{"tier:fable", "tier:opus"}
			errs := make([]error, len(labelsToAdd))
			ready := make(chan struct{}, len(labelsToAdd))
			proceed := make(chan struct{})
			var wg sync.WaitGroup
			for i, label := range labelsToAdd {
				wg.Add(1)
				go func(i int, label string) {
					defer wg.Done()
					errs[i] = s.proxiedInsertWithRetry(issueID, label, ready, proceed)
				}(i, label)
			}
			for range labelsToAdd {
				<-ready
			}
			close(proceed)
			wg.Wait()
			s.assertOneTierWinner(issueID, errs)
		})
	}
}
