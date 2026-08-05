package conformance

import (
	"context"
	"errors"
	"testing"

	publicops "github.com/steveyegge/beads/issueops"
)

// This file holds the contract every implementation of
// publicops.WorkspaceConfig must satisfy. Each case asserts what
// issueops/workspaceconfig.go PROMISES, cited by line, rather than what any one
// backend happens to do today; a backend that disagrees is parked at its own
// wiring site with skipKnownDivergence so the case still runs on the ones that
// agree.
//
// THIS CONTRACT HAS A DIFFERENT SHAPE FROM THE ISSUE CONTRACTS, and copying one
// of those wholesale would have produced the wrong test. The issue roles assert
// ISSUE STATE: seed rows, run the operation, read the rows back. Nothing here
// has rows to seed. What this plane promises is PRECEDENCE — which of two
// writes a later read sees, and whether a refusal leaves the earlier value
// standing — and SIDE EFFECTS: two of its keys are PROJECTED into normalized
// lookup tables that reads consult before the key itself, so "the write
// succeeded" and "the write took effect" are separate facts and the second one
// is the one that was broken. Every projection case therefore reads the TABLE
// through QueryScalar rather than reading the setting back through the role,
// because reading it back through the role is exactly the check that passed on
// a backend where nothing took effect.
//
// There are three wirings — the server-backed store, the embedded store and the
// unit-of-work provider — and only TWO independent bodies between them: dolt
// and embeddeddolt both hand back internal/workapi/storeworkspaceconfig and
// project through their own SetConfig, so they are one vote plus an engine
// check; the unit-of-work provider is the second, and it projects in the config
// repository its use case sits on. All three share the refusals, which come
// from workapi.ValidateSettingWrite, so what these cases can catch below that
// validator is the EXECUTION half.
//
// KEYS ARE NAMESPACED WITH THE FIXTURE PREFIX wherever they can be. Config keys
// are global to a workspace — there is no per-test plane — so an unscoped probe
// key would be an assertion about state a sibling case wrote. The two keys that
// CANNOT be namespaced are status.custom and types.custom: their whole point is
// that those exact names are projected, so the projection cases write the real
// keys and each one asserts the EXACT resulting table content rather than a
// delta. That is safe because a write rewrites the table outright, which is
// itself one of the promises under test.
//
// What is deliberately NOT here:
//   - which SOURCE owns a key (config.yaml, git config, this plane). That is
//     front-door routing over files on the client's machine and the role says
//     so; cmd/bd/config.go performs it and cmd/bd/config_test.go pins it.
//   - the multi-source views (`bd config show`, `drift`, `apply`, `validate`),
//     which are not on this role at all.
//   - the process-local caches the store backends drop on a write. They are one
//     backend's optimization, not a promise of the plane, and a second process
//     cannot observe them.

// WorkspaceConfigFixture supplies adapter-specific storage access for the
// settings assertions. Every field is named and typed exactly like the
// per-backend roleFixtureKit hook it is filled from, so a wiring is kit plus
// accessor plus prefix with no adapter in between.
type WorkspaceConfigFixture struct {
	// IssuePrefix namespaces the keys each assertion writes, so several of them
	// can share one database.
	IssuePrefix     string
	WorkspaceConfig publicops.WorkspaceConfig
	// SetConfig writes one workspace config key OUT OF BAND, past the role. It
	// is how the one case that removes the protected key puts it back, since
	// the role refuses to write it — the same seam a workspace's own
	// initialization uses.
	SetConfig func(context.Context, string, string) error
	// QueryScalar runs a single-row query and scans it, and RETURNS the error
	// rather than failing the test. It is how the projection cases read the
	// normalized tables, which the role deliberately gives no way to read.
	QueryScalar func(context.Context, string, []any, ...any) error
	// CountHistory reports how many history entries the fixture's branch has.
	// A nil hook means "this backend cannot observe history", and the case that
	// needs it SKIPS with that reason rather than passing quietly.
	CountHistory func(context.Context) (int, error)
}

// RunWorkspaceConfigStoresAValueVerbatim pins workspaceconfig.go:93-100: a
// successful write stores the value as given, and the result says so, so a
// caller may treat SetSetting as "what I sent is what is stored" without
// re-reading it.
//
// The value carries surrounding space and an inner comma on purpose. Both are
// characters this plane's other keys give meaning to — the comma separates
// entries in status.custom and types.custom — so a body that reached for a
// splitter or a trimmer on the general path would be caught here rather than on
// the one key where the behavior is correct.
func RunWorkspaceConfigStoresAValueVerbatim(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	key := workspaceConfigKey(fixture, "verbatim")
	const value = "  a, b  "

	result := setWorkspaceConfigSetting(t, ctx, fixture, key, value)
	if result.Key != key || result.Value != value {
		t.Fatalf("SetSetting result = %q=%q, want %q=%q", result.Key, result.Value, key, value)
	}
	assertWorkspaceConfigValue(t, ctx, fixture, key, value)
}

// RunWorkspaceConfigReplacesAnExistingValue pins workspaceconfig.go's "Set
// stores one setting, REPLACING any value already there": the plane holds one
// value per key, and a second write is not an append and not a refusal.
func RunWorkspaceConfigReplacesAnExistingValue(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	key := workspaceConfigKey(fixture, "replace")

	setWorkspaceConfigSetting(t, ctx, fixture, key, "first")
	setWorkspaceConfigSetting(t, ctx, fixture, key, "second")
	assertWorkspaceConfigValue(t, ctx, fixture, key, "second")
}

// RunWorkspaceConfigConflatesAnUnsetKeyWithAnEmptyValue pins
// workspaceconfig.go's SettingResult.Value: "" with a nil error is the answer
// for BOTH a key nothing ever wrote and a key written as the empty string, and
// there is no ErrNotFound on this role.
//
// It is asserted rather than left implicit because it is the promise a caller
// is most likely to assume the other way round, and because the conflation is
// what lets `bd config get` print "(not set)" for a key that was in fact set.
func RunWorkspaceConfigConflatesAnUnsetKeyWithAnEmptyValue(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	never := workspaceConfigKey(fixture, "never-written")
	emptied := workspaceConfigKey(fixture, "written-empty")

	assertWorkspaceConfigValue(t, ctx, fixture, never, "")
	setWorkspaceConfigSetting(t, ctx, fixture, emptied, "")
	assertWorkspaceConfigValue(t, ctx, fixture, emptied, "")

	// The two are the same ANSWER, but they are not the same row: the written
	// one is present in the enumeration and the never-written one is not. That
	// is the only way a caller can tell them apart on this role, so it is
	// pinned here beside the conflation it qualifies.
	settings := listWorkspaceConfigSettings(t, ctx, fixture)
	if _, ok := settings[emptied]; !ok {
		t.Fatalf("ListSettings omits %q, which was written as the empty string", emptied)
	}
	if _, ok := settings[never]; ok {
		t.Fatalf("ListSettings carries %q, which nothing wrote", never)
	}
}

// RunWorkspaceConfigListsEveryStoredSetting pins
// workspaceconfig.go's ListSettingsResult.Settings: every stored key with its
// value, and an empty map rather than nil.
func RunWorkspaceConfigListsEveryStoredSetting(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	first := workspaceConfigKey(fixture, "list-one")
	second := workspaceConfigKey(fixture, "list-two")
	setWorkspaceConfigSetting(t, ctx, fixture, first, "1")
	setWorkspaceConfigSetting(t, ctx, fixture, second, "2")

	settings := listWorkspaceConfigSettings(t, ctx, fixture)
	if settings == nil {
		t.Fatal("ListSettings returned a nil map; the contract promises an empty one")
	}
	for key, want := range map[string]string{first: "1", second: "2"} {
		if got, ok := settings[key]; !ok || got != want {
			t.Fatalf("ListSettings[%q] = %q (present=%v), want %q", key, got, ok, want)
		}
	}
}

// RunWorkspaceConfigUnsetRemovesTheSetting pins that a removed key is gone from
// BOTH answers the role gives — the single read and the enumeration — rather
// than from one of them.
func RunWorkspaceConfigUnsetRemovesTheSetting(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	key := workspaceConfigKey(fixture, "removable")
	setWorkspaceConfigSetting(t, ctx, fixture, key, "present")
	assertWorkspaceConfigValue(t, ctx, fixture, key, "present")

	unsetWorkspaceConfigSetting(t, ctx, fixture, key)
	assertWorkspaceConfigValue(t, ctx, fixture, key, "")
	if _, ok := listWorkspaceConfigSettings(t, ctx, fixture)[key]; ok {
		t.Fatalf("ListSettings still carries %q after UnsetSetting", key)
	}
}

// RunWorkspaceConfigUnsetOfAnAbsentKeySucceeds pins workspaceconfig.go's
// "Removing a key nothing set SUCCEEDS": UnsetSetting states an intended end
// state, so a caller clearing configuration it is not sure was ever written
// does not have to classify an error to learn it was already absent.
func RunWorkspaceConfigUnsetOfAnAbsentKeySucceeds(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	key := workspaceConfigKey(fixture, "absent")

	result, err := fixture.WorkspaceConfig.UnsetSetting(ctx, publicops.UnsetSettingRequest{Key: key})
	if err != nil {
		t.Fatalf("UnsetSetting(%q) on an absent key = %v, want success", key, err)
	}
	if result.Key != key {
		t.Fatalf("UnsetSetting result key = %q, want %q", result.Key, key)
	}
	// Twice, because idempotence is the claim: the second call is the one a
	// retrying caller actually makes.
	if _, err := fixture.WorkspaceConfig.UnsetSetting(ctx, publicops.UnsetSettingRequest{Key: key}); err != nil {
		t.Fatalf("second UnsetSetting(%q) = %v, want success", key, err)
	}
}

// RunWorkspaceConfigRefusesAnEmptyKey pins the empty-key refusal on all three
// verbs that take one, and pins it as ErrValidation rather than as any error:
// the front doors classify on that sentinel to tell a caller's mistake from a
// storage failure.
func RunWorkspaceConfigRefusesAnEmptyKey(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	for _, blank := range []string{"", "   "} {
		if _, err := fixture.WorkspaceConfig.GetSetting(ctx, publicops.GetSettingRequest{Key: blank}); !errors.Is(err, publicops.ErrValidation) {
			t.Fatalf("GetSetting(%q) error = %v, want ErrValidation", blank, err)
		}
		if _, err := fixture.WorkspaceConfig.SetSetting(ctx, publicops.SetSettingRequest{Key: blank, Value: "x"}); !errors.Is(err, publicops.ErrValidation) {
			t.Fatalf("SetSetting(%q) error = %v, want ErrValidation", blank, err)
		}
		if _, err := fixture.WorkspaceConfig.UnsetSetting(ctx, publicops.UnsetSettingRequest{Key: blank}); !errors.Is(err, publicops.ErrValidation) {
			t.Fatalf("UnsetSetting(%q) error = %v, want ErrValidation", blank, err)
		}
	}
}

// RunWorkspaceConfigRefusesTheProtectedKeyOnSet pins the one key this plane
// will not write, in BOTH spellings, and pins that the refusal leaves the
// stored prefix standing.
//
// The second half is the half that matters. A refusal that had already written
// would be worse than no refusal at all: the workspace would be re-prefixed AND
// the caller told it had not been. Every fixture is initialized with a prefix,
// so this reads the value back rather than asserting absence.
func RunWorkspaceConfigRefusesTheProtectedKeyOnSet(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	before := getWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyIssuePrefix)

	for _, test := range []struct{ key, want string }{
		// The underscored spelling keeps whatever the workspace was
		// initialized with; the dashed one is a key nothing has ever written,
		// so a write that slipped through would show up as a value where there
		// was none.
		{publicops.SettingKeyIssuePrefix, before},
		{"issue-prefix", ""},
	} {
		if _, err := fixture.WorkspaceConfig.SetSetting(ctx, publicops.SetSettingRequest{Key: test.key, Value: "hijack"}); !errors.Is(err, publicops.ErrValidation) {
			t.Fatalf("SetSetting(%q) error = %v, want ErrValidation", test.key, err)
		}
		assertWorkspaceConfigValue(t, ctx, fixture, test.key, test.want)
	}
}

// RunWorkspaceConfigUnsetDoesNotRefuseTheProtectedKey pins the asymmetry
// workspaceconfig.go's UnsetSetting doc records as bd-yby99.34: Set refuses the
// prefix and Unset does not.
//
// It is pinned rather than fixed because refusing it is a user-visible change
// this slice was not approved to make, and pinning is what keeps the asymmetry
// from being read as an accident — or from being closed on one backend and not
// the others while the owner decides. The prefix is restored afterwards, since
// every later case in the suite shares the workspace.
func RunWorkspaceConfigUnsetDoesNotRefuseTheProtectedKey(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	before := getWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyIssuePrefix)
	if before == "" {
		t.Fatalf("fixture has no %s set; this case needs one to remove and restore", publicops.SettingKeyIssuePrefix)
	}

	unsetWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyIssuePrefix)
	assertWorkspaceConfigValue(t, ctx, fixture, publicops.SettingKeyIssuePrefix, "")

	// Restored out of band, through the same seam a workspace's own
	// initialization uses, because the role refuses to write it back.
	if err := fixture.SetConfig(ctx, publicops.SettingKeyIssuePrefix, before); err != nil {
		t.Fatalf("restore %s to %q: %v", publicops.SettingKeyIssuePrefix, before, err)
	}
	assertWorkspaceConfigValue(t, ctx, fixture, publicops.SettingKeyIssuePrefix, before)
}

// RunWorkspaceConfigRefusesAnUnparseableCustomStatus pins the one value-shape
// refusal this plane makes, and pins that NOTHING is written when it fires.
//
// "Nothing" is two things here, and they are asserted separately because a body
// could get one right and the other wrong: the config row keeps its previous
// value, AND the custom_statuses table keeps its previous contents. The
// projection is what makes the second one possible to break — a body that wrote
// the row first and parsed while projecting would leave a stored status set
// that no read agrees with.
func RunWorkspaceConfigRefusesAnUnparseableCustomStatus(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	const good = "awaiting_review:active"
	setWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyStatusCustom, good)
	assertWorkspaceConfigTableCount(t, ctx, fixture, "custom_statuses", 1)

	// A built-in name: refused by the parser, and refused for a reason a caller
	// could plausibly hit rather than a syntactic accident.
	if _, err := fixture.WorkspaceConfig.SetSetting(ctx, publicops.SetSettingRequest{
		Key: publicops.SettingKeyStatusCustom, Value: "open",
	}); !errors.Is(err, publicops.ErrValidation) {
		t.Fatalf("SetSetting(status.custom, %q) error = %v, want ErrValidation", "open", err)
	}
	assertWorkspaceConfigValue(t, ctx, fixture, publicops.SettingKeyStatusCustom, good)
	assertWorkspaceConfigTableCount(t, ctx, fixture, "custom_statuses", 1)
}

// RunWorkspaceConfigProjectsCustomStatuses pins the side effect
// workspaceconfig.go's SetSetting doc describes for status.custom: the value is
// not merely stored, it REWRITES custom_statuses, which reads consult first.
func RunWorkspaceConfigProjectsCustomStatuses(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	setWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyStatusCustom, "awaiting_review:active,awaiting_docs:wip")
	assertWorkspaceConfigTableCount(t, ctx, fixture, "custom_statuses", 2)
	assertWorkspaceConfigTableHas(t, ctx, fixture, "custom_statuses", "awaiting_review")
	assertWorkspaceConfigTableHas(t, ctx, fixture, "custom_statuses", "awaiting_docs")
}

// RunWorkspaceConfigProjectsCustomTypes is the same pin for types.custom, and
// it is the case that FAILED on the unit-of-work backend before this role
// existed: that route wrote the string and left custom_types holding the
// previous set, so `bd config set types.custom` reported success and
// `bd create -t <the new type>` kept answering "invalid issue type" forever —
// with doctor re-verifying against the string and reporting all-OK.
//
// The three-stage sequence pins the REWRITE rather than an insert: a second
// write replaces the first set instead of adding to it, and the empty value
// clears the table. A body that appended would pass an assertion that only ever
// grew the set.
func RunWorkspaceConfigProjectsCustomTypes(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	setWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyTypesCustom, "research")
	assertWorkspaceConfigTableCount(t, ctx, fixture, "custom_types", 1)
	assertWorkspaceConfigTableHas(t, ctx, fixture, "custom_types", "research")

	setWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyTypesCustom, "session")
	assertWorkspaceConfigTableCount(t, ctx, fixture, "custom_types", 1)
	assertWorkspaceConfigTableHas(t, ctx, fixture, "custom_types", "session")

	setWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyTypesCustom, "")
	assertWorkspaceConfigTableCount(t, ctx, fixture, "custom_types", 0)
}

// RunWorkspaceConfigUnsetLeavesTheProjectionBehind pins the asymmetry
// workspaceconfig.go's UnsetSetting doc records as bd-yby99.33: removing the
// key that configured a projection does NOT undo the projection, so the custom
// types keep applying after the setting that named them is gone.
//
// All three implementations agree, so this is the plane's behavior rather than
// a divergence, and pinning it is what keeps it from being quietly fixed on one
// backend — which would leave a workspace's answer depending on which route
// removed the key.
func RunWorkspaceConfigUnsetLeavesTheProjectionBehind(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	setWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyTypesCustom, "leftover")
	assertWorkspaceConfigTableCount(t, ctx, fixture, "custom_types", 1)

	unsetWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyTypesCustom)
	assertWorkspaceConfigValue(t, ctx, fixture, publicops.SettingKeyTypesCustom, "")
	assertWorkspaceConfigTableCount(t, ctx, fixture, "custom_types", 1)

	// Left as this case found it, so a later case in the shared suite reads the
	// plane rather than this one's residue.
	setWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyTypesCustom, "")
	assertWorkspaceConfigTableCount(t, ctx, fixture, "custom_types", 0)
	unsetWorkspaceConfigSetting(t, ctx, fixture, publicops.SettingKeyTypesCustom)
}

// RunWorkspaceConfigARefusedWriteRecordsNoHistory pins the other half of "and
// NOTHING is written": a refusal does not reach storage at all, so it leaves no
// history entry behind either.
//
// The delta is taken around the refusal rather than read off the top of the
// log, for the reason the kit's CountHistory doc gives: two commits made inside
// one second tie on date and their order is not something to rely on.
func RunWorkspaceConfigARefusedWriteRecordsNoHistory(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) {
	t.Helper()
	if fixture.CountHistory == nil {
		t.Skip("this backend cannot observe history, so the no-write half of a refusal is unobservable here")
	}
	before, err := fixture.CountHistory(ctx)
	if err != nil {
		t.Fatalf("CountHistory before: %v", err)
	}

	for _, req := range []publicops.SetSettingRequest{
		{Key: "", Value: "x"},
		{Key: publicops.SettingKeyIssuePrefix, Value: "hijack"},
		{Key: publicops.SettingKeyStatusCustom, Value: "open"},
	} {
		if _, err := fixture.WorkspaceConfig.SetSetting(ctx, req); !errors.Is(err, publicops.ErrValidation) {
			t.Fatalf("SetSetting(%q) error = %v, want ErrValidation", req.Key, err)
		}
	}

	after, err := fixture.CountHistory(ctx)
	if err != nil {
		t.Fatalf("CountHistory after: %v", err)
	}
	if after != before {
		t.Fatalf("history entries went %d -> %d across three refused writes, want no change", before, after)
	}
}

// workspaceConfigKey namespaces a probe key under the fixture's prefix. Config
// keys are global to a workspace, so an unscoped one would be an assertion
// about whatever a sibling case last wrote.
func workspaceConfigKey(fixture WorkspaceConfigFixture, name string) string {
	return "custom." + fixture.IssuePrefix + "-" + name
}

func setWorkspaceConfigSetting(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture, key, value string) publicops.SetSettingResult {
	t.Helper()
	result, err := fixture.WorkspaceConfig.SetSetting(ctx, publicops.SetSettingRequest{Key: key, Value: value})
	if err != nil {
		t.Fatalf("SetSetting(%q, %q): %v", key, value, err)
	}
	return result
}

func unsetWorkspaceConfigSetting(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture, key string) {
	t.Helper()
	if _, err := fixture.WorkspaceConfig.UnsetSetting(ctx, publicops.UnsetSettingRequest{Key: key}); err != nil {
		t.Fatalf("UnsetSetting(%q): %v", key, err)
	}
}

func getWorkspaceConfigSetting(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture, key string) string {
	t.Helper()
	result, err := fixture.WorkspaceConfig.GetSetting(ctx, publicops.GetSettingRequest{Key: key})
	if err != nil {
		t.Fatalf("GetSetting(%q): %v", key, err)
	}
	if result.Key != key {
		t.Fatalf("GetSetting(%q) echoed key %q", key, result.Key)
	}
	return result.Value
}

func assertWorkspaceConfigValue(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture, key, want string) {
	t.Helper()
	if got := getWorkspaceConfigSetting(t, ctx, fixture, key); got != want {
		t.Fatalf("GetSetting(%q) = %q, want %q", key, got, want)
	}
}

func listWorkspaceConfigSettings(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture) map[string]string {
	t.Helper()
	result, err := fixture.WorkspaceConfig.ListSettings(ctx, publicops.ListSettingsRequest{})
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	return result.Settings
}

// assertWorkspaceConfigTableCount reads a normalized projection table directly.
// The role gives no way to read one, deliberately — it is a projection, not a
// setting — so this is the only assertion that can tell "the value was stored"
// from "the value took effect".
//
// The table name is interpolated because a table name cannot be a bind
// parameter; both call sites pass a literal from this file.
func assertWorkspaceConfigTableCount(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture, table string, want int) {
	t.Helper()
	var got int
	if err := fixture.QueryScalar(ctx, "SELECT COUNT(*) FROM "+table, nil, &got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s holds %d rows, want %d — the setting was stored without its projection", table, got, want)
	}
}

func assertWorkspaceConfigTableHas(t *testing.T, ctx context.Context, fixture WorkspaceConfigFixture, table, name string) {
	t.Helper()
	var got int
	if err := fixture.QueryScalar(ctx, "SELECT COUNT(*) FROM "+table+" WHERE name = ?", []any{name}, &got); err != nil {
		t.Fatalf("look up %q in %s: %v", name, table, err)
	}
	if got != 1 {
		t.Fatalf("%s holds %d rows named %q, want 1", table, got, name)
	}
}
