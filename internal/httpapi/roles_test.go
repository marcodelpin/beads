package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// These tests cover the OTHER database source: a backend whose facade is a
// store rather than a unit-of-work provider, served by handing Listen the roles
// directly.
// store rather than a unit-of-work provider, served by handing Listen the issue
// roles directly.
//
// The fakes below implement issueops.Reader, issueops.Claimer,
// issueops.WorkspaceConfig and issueops.StatsReporter and NOTHING else —
// deliberately not uow.UnitOfWorkProvider, because "a backend that cannot
// produce a unit of work is still servable" is the property this whole seam
// exists for. If any of them ever grows a NewUOW method these tests stop
// proving it.
// The fakes below implement issueops.Reader, issueops.Claimer and
// issueops.EdgeReader and NOTHING else — deliberately not
// uow.UnitOfWorkProvider, because "a backend that cannot produce a unit of work
// is still servable" is the property this whole seam exists for. If any of them
// ever grows a NewUOW method these tests stop proving it.
// The fakes below implement one issueops role each and NOTHING else —
// deliberately not uow.UnitOfWorkProvider, because "a backend that cannot
// produce a unit of work is still servable" is the property this whole seam
// exists for. If any of them ever grows a NewUOW method these tests stop
// proving it.

type roleReader struct {
	page    issueops.IssuePage
	details *issueops.IssueDetails
	err     error

	mu    sync.Mutex
	ready []issueops.ReadyRequest
	list  []issueops.ListRequest
	get   []issueops.GetRequest
}

func (r *roleReader) Ready(_ context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
	r.mu.Lock()
	r.ready = append(r.ready, req)
	r.mu.Unlock()
	if r.err != nil {
		return issueops.IssuePage{}, r.err
	}
	return r.page, nil
}

func (r *roleReader) List(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
	r.mu.Lock()
	r.list = append(r.list, req)
	r.mu.Unlock()
	if r.err != nil {
		return issueops.IssuePage{}, r.err
	}
	return r.page, nil
}

func (r *roleReader) Get(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
	r.mu.Lock()
	r.get = append(r.get, req)
	r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	return r.details, nil
}

func (r *roleReader) readyRequests() []issueops.ReadyRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]issueops.ReadyRequest(nil), r.ready...)
}

func (r *roleReader) getRequests() []issueops.GetRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]issueops.GetRequest(nil), r.get...)
}

type roleEdgeReader struct {
	result issueops.EdgeReadResult
	err    error

	mu    sync.Mutex
	reads []issueops.EdgeReadRequest
}

func (e *roleEdgeReader) ReadEdges(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
	e.mu.Lock()
	e.reads = append(e.reads, req)
	e.mu.Unlock()
	if e.err != nil {
		return issueops.EdgeReadResult{}, e.err
	}
	return e.result, nil
}

func (e *roleEdgeReader) edgeRequests() []issueops.EdgeReadRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]issueops.EdgeReadRequest(nil), e.reads...)
}

// roleReadyCounter is the third role a store-shaped source must supply. It is
// its own fake rather than a method on roleReader for the reason the roles are
// separate interfaces: a backend hands out one surface per question, and a test
// double that answered two of them would be the shape this seam exists to rule
// out.
type roleReadyCounter struct {
	total int64
	err   error

	mu     sync.Mutex
	counts []issueops.ReadyRequest
}

func (c *roleReadyCounter) CountReady(_ context.Context, req issueops.ReadyRequest) (issueops.ReadyCountResult, error) {
	c.mu.Lock()
	c.counts = append(c.counts, req)
	c.mu.Unlock()
	if c.err != nil {
		return issueops.ReadyCountResult{}, c.err
	}
	return issueops.ReadyCountResult{Total: c.total}, nil
}

func (c *roleReadyCounter) countRequests() []issueops.ReadyRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]issueops.ReadyRequest(nil), c.counts...)
}

// roleQuerier is the boolean-query role's double. It records the whole request
// because the property this surface owes is that the EXPRESSION reaches the
// role untouched: a handler that parsed it here would be the drift the role
// exists to remove, and only the recorded sentence can show it did not.
type roleQuerier struct {
	page issueops.IssuePage
	err  error

	mu      sync.Mutex
	queries []issueops.QueryRequest
}

func (q *roleQuerier) Query(_ context.Context, req issueops.QueryRequest) (issueops.IssuePage, error) {
	q.mu.Lock()
	q.queries = append(q.queries, req)
	q.mu.Unlock()
	if q.err != nil {
		return issueops.IssuePage{}, q.err
	}
	return q.page, nil
}

func (q *roleQuerier) queryRequests() []issueops.QueryRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]issueops.QueryRequest(nil), q.queries...)
}

type roleClaimer struct {
	result issueops.ClaimResult
	err    error

	mu     sync.Mutex
	claims []issueops.ClaimRequest
}

func (c *roleClaimer) Claim(_ context.Context, req issueops.ClaimRequest) (issueops.ClaimResult, error) {
	c.mu.Lock()
	c.claims = append(c.claims, req)
	c.mu.Unlock()
	if c.err != nil {
		return issueops.ClaimResult{}, c.err
	}
	return c.result, nil
}

func (c *roleClaimer) claimRequests() []issueops.ClaimRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]issueops.ClaimRequest(nil), c.claims...)
}

type roleSettings struct {
	value    string
	settings map[string]string
	err      error

	mu   sync.Mutex
	gets []issueops.GetSettingRequest
}

func (c *roleSettings) GetSetting(_ context.Context, req issueops.GetSettingRequest) (issueops.SettingResult, error) {
	c.mu.Lock()
	c.gets = append(c.gets, req)
	c.mu.Unlock()
	if c.err != nil {
		return issueops.SettingResult{}, c.err
	}
	return issueops.SettingResult{Key: req.Key, Value: c.value}, nil
}

func (c *roleSettings) ListSettings(context.Context, issueops.ListSettingsRequest) (issueops.ListSettingsResult, error) {
	if c.err != nil {
		return issueops.ListSettingsResult{}, c.err
	}
	return issueops.ListSettingsResult{Settings: c.settings}, nil
}

func (c *roleSettings) SetSetting(context.Context, issueops.SetSettingRequest) (issueops.SetSettingResult, error) {
	return issueops.SetSettingResult{}, errors.New("this surface publishes no settings write")
}

func (c *roleSettings) UnsetSetting(context.Context, issueops.UnsetSettingRequest) (issueops.UnsetSettingResult, error) {
	return issueops.UnsetSettingResult{}, errors.New("this surface publishes no settings write")
}

func (c *roleSettings) getRequests() []issueops.GetSettingRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]issueops.GetSettingRequest(nil), c.gets...)
}

type roleStats struct {
	summary types.Statistics
	err     error

	mu        sync.Mutex
	stats     []issueops.StatsRequest
	assignees []issueops.AssigneeStatsRequest
}

func (s *roleStats) Stats(_ context.Context, req issueops.StatsRequest) (issueops.StatsResult, error) {
	s.mu.Lock()
	s.stats = append(s.stats, req)
	s.mu.Unlock()
	if s.err != nil {
		return issueops.StatsResult{}, s.err
	}
	return issueops.StatsResult{Summary: s.summary}, nil
}

func (s *roleStats) AssigneeStats(_ context.Context, req issueops.AssigneeStatsRequest) (issueops.StatsResult, error) {
	s.mu.Lock()
	s.assignees = append(s.assignees, req)
	s.mu.Unlock()
	if s.err != nil {
		return issueops.StatsResult{}, s.err
	}
	return issueops.StatsResult{Summary: s.summary}, nil
}

func (s *roleStats) statsRequests() []issueops.StatsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]issueops.StatsRequest(nil), s.stats...)
}

func (s *roleStats) assigneeRequests() []issueops.AssigneeStatsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]issueops.AssigneeStatsRequest(nil), s.assignees...)
}

// rolesConfig completes a store-shaped database source.
//
// Listen requires the WHOLE role set together (checkDatabaseSource), so a test
// that exercises one operation still has to hand over the roles the other
// operations reach. This fills the ones the caller left nil with inert fakes
// and keeps the ones it set, so a test names only the role it is about.
//
// Not for TestListenRequiresExactlyOneDatabaseSource: that test's whole subject
// is the PARTIAL set, so it builds its Configs by hand.
func rolesConfig(cfg Config) Config {
	if cfg.Reader == nil {
		cfg.Reader = &roleReader{}
	}
	if cfg.Claimer == nil {
		cfg.Claimer = &roleClaimer{}
	}
	if cfg.Settings == nil {
		cfg.Settings = &roleSettings{}
	}
	if cfg.Stats == nil {
		cfg.Stats = &roleStats{}
	}
	if cfg.CycleDetector == nil {
		cfg.CycleDetector = &roleCycleDetector{}
	}
	if cfg.EdgeReader == nil {
		cfg.EdgeReader = &roleEdgeReader{}
	}
	if cfg.ReadyCounter == nil {
		cfg.ReadyCounter = &roleReadyCounter{}
	}
	if cfg.Querier == nil {
		cfg.Querier = &roleQuerier{}
	}
	return cfg
}

// rolesConfigWithout is a complete source with exactly one role removed, which
// is the shape every half-set case in TestListenRequiresExactlyOneDatabaseSource
// wants. Naming the role to DROP rather than the roles to keep is what stops
// those cases from having to be edited every time the set grows.
func rolesConfigWithout(drop func(*Config)) Config {
	cfg := rolesConfig(Config{})
	drop(&cfg)
	return cfg
}

// roleCycleDetector is the third role of the store-shaped source. It is the
// same shape as the two above so that every configured-roles case can hand
// Listen a complete source without deciding what its cycle answer should be.
type roleCycleDetector struct {
	report issueops.CycleReport
	err    error

	mu    sync.Mutex
	calls []issueops.DetectCyclesRequest
}

func (d *roleCycleDetector) DetectCycles(_ context.Context, req issueops.DetectCyclesRequest) (issueops.CycleReport, error) {
	d.mu.Lock()
	d.calls = append(d.calls, req)
	d.mu.Unlock()
	if d.err != nil {
		return issueops.CycleReport{}, d.err
	}
	return d.report, nil
}

// countedPage is the fixture both database sources answer with, so a body
// difference between them can only come from the construction.
func countedPage() []*types.IssueWithCounts {
	return []*types.IssueWithCounts{
		{Issue: seededIssue("bd-1", "alice", types.StatusOpen), DependencyCount: 1, DependentCount: 2, CommentCount: 3},
		{Issue: seededIssue("bd-2", "", types.StatusOpen)},
	}
}

// TestListenRequiresExactlyOneDatabaseSource pins the precondition that
// replaced the old nil-provider check.
//
// The two refusals are different mistakes and must stay distinguishable. A
// HALF-SET set is the dangerous one: a Config carrying a reader and no claimer
// would bind, answer every read, and fail the one issue write on this surface
// with a nil dereference — at claim time, in a handler, on a live server.
// PARTIALLY-SET role set is the dangerous one: a Config carrying a reader and
// no claimer would bind, answer every read, and fail the one write on this
// surface with a nil dereference — at claim time, in a handler, on a live
// server. Each role an operation reaches has a row here for the same reason.
// PARTIAL set is the dangerous one: a Config carrying a reader and no claimer
// would bind, answer every read, and fail the one write on this surface with a
// nil dereference — at claim time, in a handler, on a live server. The set
// grows with the surface, so the case that matters most is the newest role
// missing: that is what a caller written against the previous release passes.
func TestListenRequiresExactlyOneDatabaseSource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "neither source",
			cfg:     Config{},
			wantErr: "no database source",
		},
		{
			name: "a provider alone is a complete source",
			cfg:  Config{Provider: &fakeProvider{}},
		},
		{
			name: "every role together is a complete source",
			cfg:  rolesConfig(Config{}),
		},
		// One case per role, each dropping exactly that role from an otherwise
		// complete source. The set is all-or-nothing rather than a required
		// pair plus optional extras: a Config missing any one role binds,
		// advertises that operation's capability, and then faults inside a
		// handler on a live server. A new role adds one line here and one to
		// rolesConfig, and nothing else in this test.
		{
			name:    "no reader",
			cfg:     rolesConfigWithout(func(c *Config) { c.Reader = nil }),
			wantErr: "no database source",
		},
		{
			name:    "no claimer",
			cfg:     rolesConfigWithout(func(c *Config) { c.Claimer = nil }),
			wantErr: "no database source",
		},
		{
			name:    "no settings role",
			cfg:     rolesConfigWithout(func(c *Config) { c.Settings = nil }),
			wantErr: "no database source",
		},
		{
			name:    "no summary role",
			cfg:     rolesConfigWithout(func(c *Config) { c.Stats = nil }),
			wantErr: "no database source",
		},
		{
			name:    "no cycle detector",
			cfg:     rolesConfigWithout(func(c *Config) { c.CycleDetector = nil }),
			wantErr: "no database source",
		},
		{
			name:    "no edge reader",
			cfg:     rolesConfigWithout(func(c *Config) { c.EdgeReader = nil }),
			wantErr: "no database source",
		},
		{
			name:    "no ready counter",
			cfg:     rolesConfigWithout(func(c *Config) { c.ReadyCounter = nil }),
			wantErr: "no database source",
		},
		{
			// The newest role, and the one a caller written against the
			// PREVIOUS release leaves out: every set that was complete before
			// this commit is incomplete now, and the failure a bind would defer
			// is a nil dereference on the first issues:query request.
			name:    "no querier",
			cfg:     rolesConfigWithout(func(c *Config) { c.Querier = nil }),
			wantErr: "no database source",
		},
		{
			name:    "a cycle detector alone",
			cfg:     Config{CycleDetector: &roleCycleDetector{}},
			wantErr: "no database source",
		},
		{
			name:    "a querier alone",
			cfg:     Config{Querier: &roleQuerier{}},
			wantErr: "no database source",
		},
		{
			name:    "a provider and a querier",
			cfg:     Config{Provider: &fakeProvider{}, Querier: &roleQuerier{}},
			wantErr: "exactly one database source",
		},
		{
			// The newest role, and the one a caller written against the
			// previous release would leave out: the pair alone used to be a
			// complete source, and a Config that stayed that way would bind and
			// then nil-dereference on the first ready:count request.
			name:    "a reader and a claimer without a ready counter",
			cfg:     Config{Reader: &roleReader{}, Claimer: &roleClaimer{}},
			wantErr: "no database source",
		},
		{
			name:    "a ready counter alone",
			cfg:     Config{ReadyCounter: &roleReadyCounter{}},
			wantErr: "no database source",
		},
		{
			name:    "a provider and a ready counter",
			cfg:     Config{Provider: &fakeProvider{}, ReadyCounter: &roleReadyCounter{}},
			wantErr: "exactly one database source",
		},
		{
			name:    "a provider and a reader",
			cfg:     Config{Provider: &fakeProvider{}, Reader: &roleReader{}},
			wantErr: "exactly one database source",
		},
		{
			name:    "a provider and a claimer",
			cfg:     Config{Provider: &fakeProvider{}, Claimer: &roleClaimer{}},
			wantErr: "exactly one database source",
		},
		{
			name:    "a provider and a cycle detector",
			cfg:     Config{Provider: &fakeProvider{}, CycleDetector: &roleCycleDetector{}},
			wantErr: "exactly one database source",
		},
		{
			name: "a provider and every role",
			cfg: func() Config {
				cfg := rolesConfig(Config{})
				cfg.Provider = &fakeProvider{}
				return cfg
			}(),
			wantErr: "exactly one database source",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Addr = "127.0.0.1:0"
			cfg.Stdout = io.Discard
			cfg.Stderr = io.Discard

			srv, err := Listen(cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Listen: %v, want a bound server", err)
				}
				t.Cleanup(func() { _ = srv.http.Close() })
				return
			}
			if err == nil {
				t.Fatalf("Listen bound a server for %s; want a refusal mentioning %q", tc.name, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestConfiguredRolesServeTheSameReadyBytesAsAProvider is the "construction
// only" proof: two servers built from the two database sources, over fakes that
// answer with the same page, produce the same response byte for byte.
func TestConfiguredRolesServeTheSameReadyBytesAsAProvider(t *testing.T) {
	items := countedPage()

	viaProvider := newTestServer(t, Config{Provider: &fakeProvider{
		issues:     &fakeIssues{},
		readIssues: &recordingIssues{items: items},
		readConfig: emptyConfig{},
	}})
	viaRoles := newTestServer(t, rolesConfig(Config{
		Reader: &roleReader{page: issueops.IssuePage{Items: items}},
	}))

	for _, path := range []string{"/v0/beads/ready", "/v0/beads/issues"} {
		fromProvider := viaProvider.get(t, path)
		fromRoles := viaRoles.get(t, path)

		if fromProvider.StatusCode != http.StatusOK || fromRoles.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: provider status %d, roles status %d, want 200 from both",
				path, fromProvider.StatusCode, fromRoles.StatusCode)
		}
		if got, want := fromRoles.Header.Get("Content-Type"), fromProvider.Header.Get("Content-Type"); got != want {
			t.Errorf("GET %s: roles Content-Type %q, provider %q", path, got, want)
		}
		if got, want := readAll(t, fromRoles), readAll(t, fromProvider); got != want {
			t.Errorf("GET %s: the two database sources answer differently\nroles:    %s\nprovider: %s", path, got, want)
		}
	}
}

// TestConfiguredRolesAnswerEveryDatabaseRoute drives all six
// TestConfiguredRolesAnswerEveryDatabaseRoute drives all five
// database-touching operations against a store-shaped source, which is the
// whole point: none of them can reach a unit of work here, because there is no
// provider to open one.
// TestConfiguredRolesAnswerEveryDatabaseRoute drives every database-touching
// operation against a store-shaped source, which is the whole point: none of
// them can reach a unit of work here, because there is no provider to open one.
func TestConfiguredRolesAnswerEveryDatabaseRoute(t *testing.T) {
	details := &issueops.IssueDetails{Issue: *seededIssue("bd-1", "alice", types.StatusOpen)}
	reader := &roleReader{page: issueops.IssuePage{Items: countedPage(), HasMore: true}, details: details}
	claimer := &roleClaimer{result: issueops.ClaimResult{
		Issue:   seededIssue("bd-1", "alice", types.StatusInProgress),
		Changed: true,
	}}
	settings := &roleSettings{
		value:    "awaiting_review:active",
		settings: map[string]string{"status.custom": "awaiting_review:active", "notion.token": "secret-value"},
	}
	blocked := 2
	reporter := &roleStats{summary: types.Statistics{TotalIssues: 7, OpenIssues: 5, BlockedIssues: &blocked}}
	edges := &roleEdgeReader{result: issueops.EdgeReadResult{Anchors: []issueops.AnchorEdges{
		{ID: "bd-1", Edges: []*types.Dependency{{IssueID: "bd-1", DependsOnID: "bd-2", Type: types.DepBlocks}}},
		{ID: "bd-9", Missing: true},
	}}}
	counter := &roleReadyCounter{total: 41}
	querier := &roleQuerier{page: issueops.IssuePage{Items: countedPage()}}
	ts := newTestServer(t, rolesConfig(Config{
		Reader: reader, Claimer: claimer, Settings: settings, Stats: reporter,
		EdgeReader: edges, ReadyCounter: counter, Querier: querier,
	}))

	t.Run("ready", func(t *testing.T) {
		resp := ts.get(t, "/v0/beads/ready?sort=oldest")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
		}
		body := decodeBody(t, resp)
		if body["has_more"] != true {
			t.Errorf("has_more = %v, want the value the role reported", body["has_more"])
		}
		if got, ok := body["items"].([]any); !ok || len(got) != 2 {
			t.Errorf("items = %v, want the role's two rows", body["items"])
		}
		// The role is handed the request the wire named, not a rewritten one.
		reqs := reader.readyRequests()
		if len(reqs) != 1 || reqs[0].Sort != "oldest" {
			t.Errorf("ready requests = %+v, want one carrying sort=oldest", reqs)
		}
	})

	t.Run("count ready", func(t *testing.T) {
		resp := ts.get(t, "/v0/beads/ready:count?label=api")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
		}
		if body := decodeBody(t, resp); body["total"] != float64(41) {
			t.Errorf("total = %v, want the 41 the role reported", body["total"])
		}
		// The role is handed the listing's request with the page taken off:
		// the filter the wire named, no Limit, no Offset, and the sort this
		// server sends on both ready operations.
		reqs := counter.countRequests()
		if len(reqs) != 1 {
			t.Fatalf("count requests = %+v, want exactly one", reqs)
		}
		if got := reqs[0]; got.Limit != nil || got.Offset != 0 {
			t.Errorf("count request carries a page (limit=%v offset=%d); the role refuses one", got.Limit, got.Offset)
		}
		if got := reqs[0]; len(got.Labels) != 1 || got.Labels[0] != "api" || got.Sort != readySortDefault {
			t.Errorf("count request = %+v, want the wire filter under sort %q", got, readySortDefault)
		}
	})

	t.Run("query", func(t *testing.T) {
		resp := ts.get(t, "/v0/beads/issues:query?q=type%3Dbug+OR+label%3Durgent&sort=priority")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
		}
		body := decodeBody(t, resp)
		if got, ok := body["items"].([]any); !ok || len(got) != 2 {
			t.Errorf("items = %v, want the role's two rows", body["items"])
		}
		if _, present := body["next_cursor"]; present {
			t.Errorf("query page carries a next_cursor: %v", body)
		}
		// The EXPRESSION reaches the role verbatim. A handler that parsed,
		// normalised or re-quoted it would be the second implementation of the
		// query language this role exists to prevent.
		reqs := querier.queryRequests()
		if len(reqs) != 1 {
			t.Fatalf("query requests = %+v, want exactly one", reqs)
		}
		if got := reqs[0].Expression; got != "type=bug OR label=urgent" {
			t.Errorf("expression = %q, want the sentence the wire named", got)
		}
		if got := reqs[0]; got.SortBy != "priority" || got.Offset != 0 {
			t.Errorf("query request = %+v, want sort=priority and no offset", got)
		}
	})

	t.Run("list", func(t *testing.T) {
		resp := ts.get(t, "/v0/beads/issues")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
		}
		body := decodeBody(t, resp)
		// has_more and next_cursor are a biconditional on this surface, and the
		// cursor is minted from the page the role returned.
		if body["has_more"] != true {
			t.Errorf("has_more = %v, want true", body["has_more"])
		}
		if cursor, _ := body["next_cursor"].(string); cursor == "" {
			t.Errorf("no next_cursor beside has_more: %v", body)
		}
	})

	t.Run("get", func(t *testing.T) {
		resp := ts.get(t, "/v0/beads/issues/bd-1")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
		}
		if body := decodeBody(t, resp); body["id"] != "bd-1" {
			t.Errorf("body = %v, want the detail view the role returned", body)
		}
		if reqs := reader.getRequests(); len(reqs) != 1 || reqs[0].ID != "bd-1" {
			t.Errorf("get requests = %+v, want one for bd-1", reqs)
		}
	})

	t.Run("claim", func(t *testing.T) {
		resp := ts.claim(t, claimPath, `{"actor":"alice"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
		}
		body := decodeBody(t, resp)
		if body["already_claimed"] != false {
			t.Errorf("already_claimed = %v, want false for a claim that changed the row", body["already_claimed"])
		}
		issue, _ := body["issue"].(map[string]any)
		if issue["id"] != "bd-1" || issue["status"] != string(types.StatusInProgress) {
			t.Errorf("issue = %v, want the row the role reported", body["issue"])
		}
		if reqs := claimer.claimRequests(); len(reqs) != 1 || reqs[0] != (issueops.ClaimRequest{IssueID: "bd-1", Actor: "alice"}) {
			t.Errorf("claim requests = %+v, want one for bd-1 by alice", reqs)
		}
	})

	t.Run("config list", func(t *testing.T) {
		resp := ts.get(t, "/v0/beads/config")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
		}
		items, _ := decodeBody(t, resp)["items"].([]any)
		if len(items) != 2 {
			t.Fatalf("items = %v, want the role's two settings", items)
		}
		// Ordered by key, which is what makes the page envelope's cursor
		// expressible later: "notion.token" sorts before "status.custom".
		first, _ := items[0].(map[string]any)
		second, _ := items[1].(map[string]any)
		if first["key"] != "notion.token" || second["key"] != "status.custom" {
			t.Fatalf("items are not ordered by key: %v", items)
		}
		// The credential-bearing key is present and its value is ABSENT rather
		// than masked, so a client cannot mistake a placeholder for
		// configuration.
		if first["redacted"] != true {
			t.Errorf("notion.token redacted = %v, want true", first["redacted"])
		}
		if _, present := first["value"]; present {
			t.Errorf("notion.token carries a value on the wire: %v", first)
		}
		if second["redacted"] != false || second["value"] != "awaiting_review:active" {
			t.Errorf("status.custom = %v, want its stored value unredacted", second)
		}
	})

	t.Run("config get", func(t *testing.T) {
		resp := ts.get(t, "/v0/beads/config/status.custom")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
		}
		body := decodeBody(t, resp)
		if body["key"] != "status.custom" || body["value"] != "awaiting_review:active" {
			t.Errorf("body = %v, want the setting the role returned", body)
		}
		// The key reaches the role verbatim, dots and all: it is one path
		// segment, not a namespace this surface walks.
		if reqs := settings.getRequests(); len(reqs) != 1 || reqs[0].Key != "status.custom" {
			t.Errorf("get requests = %+v, want one for status.custom", reqs)
		}
	})

	t.Run("stats", func(t *testing.T) {
		resp := ts.get(t, "/v0/beads/stats?skip_blocked=true")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
		}
		body := decodeBody(t, resp)
		summary, _ := body["summary"].(map[string]any)
		if summary["total_issues"] != float64(7) {
			t.Errorf("summary = %v, want the counts the role reported", summary)
		}
		// The role answered with a populated blocked count, so the hint was not
		// taken and the flag says so — derived from the answer, not echoed from
		// the request that asked for the fast path.
		if body["blocked_count_skipped"] != false {
			t.Errorf("blocked_count_skipped = %v, want false beside a populated blocked_issues", body["blocked_count_skipped"])
		}
		if reqs := reporter.statsRequests(); len(reqs) != 1 || !reqs[0].SkipBlocked {
			t.Errorf("stats requests = %+v, want one carrying SkipBlocked", reqs)
		}
	})

	t.Run("stats for one assignee", func(t *testing.T) {
		resp := ts.get(t, "/v0/beads/stats?assignee=alice&skip_blocked=true")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
		}
		// The assignee-scoped method, with the hint dropped rather than
		// forwarded: that request type has no such field.
		reqs := reporter.assigneeRequests()
		if len(reqs) != 1 || reqs[0].Assignee != "alice" {
			t.Errorf("assignee requests = %+v, want one for alice", reqs)
		}
		if got := reporter.statsRequests(); len(got) != 1 {
			t.Errorf("the workspace-wide method was called %d times, want the 1 from the subtest above: an assignee asks the other question", len(got))
		}
	})

	t.Run("dependencies", func(t *testing.T) {
		resp := ts.get(t, "/v0/beads/dependencies?issue_id=bd-1&issue_id=bd-9&type=blocks")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
		}
		body := decodeBody(t, resp)
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items = %v, want the one edge the role returned", body["items"])
		}
		if edge, _ := items[0].(map[string]any); edge["issue_id"] != "bd-1" || edge["depends_on_id"] != "bd-2" {
			t.Errorf("edge = %v, want the row the role returned", items[0])
		}
		// The ghost anchor is on `missing`, not a 404 and not an item.
		missing, _ := body["missing"].([]any)
		if len(missing) != 1 || missing[0] != "bd-9" {
			t.Errorf("missing = %v, want the one anchor the role reported absent", body["missing"])
		}
		// The role is handed the request the wire named, not a rewritten one.
		reqs := edges.edgeRequests()
		if len(reqs) != 1 || len(reqs[0].IDs) != 2 || reqs[0].IDs[0] != "bd-1" || reqs[0].IDs[1] != "bd-9" {
			t.Fatalf("edge requests = %+v, want one carrying both ids in order", reqs)
		}
		if len(reqs[0].Types) != 1 || reqs[0].Types[0] != types.DepBlocks {
			t.Errorf("edge request types = %v, want the one the wire named", reqs[0].Types)
		}
	})
}

// TestConfiguredRolesKeepTheDocumentedRefusals: the error vocabulary belongs to
// the handlers, so it cannot depend on which database source produced the role.
// Every case here is the roles-path twin of a provider-path test in this
// package.
func TestConfiguredRolesKeepTheDocumentedRefusals(t *testing.T) {
	t.Run("a missing issue is 404", func(t *testing.T) {
		ts := newTestServer(t, Config{
			Reader:        &roleReader{err: fmt.Errorf("get bd-404: %w", storage.ErrNotFound)},
			Claimer:       &roleClaimer{},
			Settings:      &roleSettings{},
			Stats:         &roleStats{},
			CycleDetector: &roleCycleDetector{},
			EdgeReader:    &roleEdgeReader{},
			ReadyCounter:  &roleReadyCounter{},
			Querier:       &roleQuerier{},
		})
		resp := ts.get(t, "/v0/beads/issues/bd-404")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", resp.StatusCode, readAll(t, resp))
		}
		if body := decodeBody(t, resp); body["code"] != string(CodeNotFound) {
			t.Errorf("code = %v, want %s", body["code"], CodeNotFound)
		}
	})

	t.Run("a filter refusal is the documented 400", func(t *testing.T) {
		// The builders run INSIDE the role on this path, so their refusal
		// arrives at the handler exactly as it does from a unit-of-work reader —
		// and must still be mapped to its parameter rather than to a 500.
		ts := newTestServer(t, Config{
			Reader:        &roleReader{err: errors.New("invalid status bogus")},
			Claimer:       &roleClaimer{},
			Settings:      &roleSettings{},
			Stats:         &roleStats{},
			CycleDetector: &roleCycleDetector{},
			EdgeReader:    &roleEdgeReader{},
			ReadyCounter:  &roleReadyCounter{},
			Querier:       &roleQuerier{},
		})
		resp := ts.get(t, "/v0/beads/issues?status=bogus")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
		}
		body := decodeBody(t, resp)
		if body["code"] != string(CodeInvalidArgument) || body["param"] != "status" {
			t.Errorf("body = %v, want invalid_argument on param status", body)
		}
	})

	t.Run("a foreign holder is 409 with its state", func(t *testing.T) {
		ts := newTestServer(t, Config{
			Reader:        &roleReader{},
			Stats:         &roleStats{},
			CycleDetector: &roleCycleDetector{},
			EdgeReader:    &roleEdgeReader{},
			Claimer: &roleClaimer{err: &issueops.ClaimConflictError{
				IssueID:  "bd-1",
				Assignee: "bob",
				Status:   types.StatusInProgress,
				Err:      fmt.Errorf("claim bd-1: %w", storage.ErrAlreadyClaimed),
			}},
			Settings:     &roleSettings{},
			ReadyCounter: &roleReadyCounter{},
			Querier:      &roleQuerier{},
		})
		resp := ts.claim(t, claimPath, `{"actor":"alice"}`)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", resp.StatusCode, readAll(t, resp))
		}
		body := decodeBody(t, resp)
		if body["code"] != string(CodeAlreadyClaimed) {
			t.Errorf("code = %v, want %s", body["code"], CodeAlreadyClaimed)
		}
		if body["assignee"] != "bob" || body["issue_status"] != string(types.StatusInProgress) {
			t.Errorf("body = %v, want the holder and status the role reported", body)
		}
	})

	t.Run("a role failure is the generic 500", func(t *testing.T) {
		ts := newTestServer(t, Config{
			Reader:        &roleReader{err: errors.New("backend is unreachable")},
			Claimer:       &roleClaimer{},
			Settings:      &roleSettings{},
			Stats:         &roleStats{},
			CycleDetector: &roleCycleDetector{},
			EdgeReader:    &roleEdgeReader{},
			ReadyCounter:  &roleReadyCounter{},
			Querier:       &roleQuerier{},
		})
		resp := ts.get(t, "/v0/beads/ready")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", resp.StatusCode, readAll(t, resp))
		}
		if body := readAll(t, resp); strings.Contains(body, "backend is unreachable") {
			t.Errorf("the 5xx body republished the backend's error text: %s", body)
		}
	})
}

// TestStartupNamesTheDatabaseSource: uow_ms is 0.000 for every request a
// roles-backed server answers, because it opens no units of work. That is the
// true value, and the startup line is what makes it attributable instead of
// looking like lost instrumentation.
func TestStartupNamesTheDatabaseSource(t *testing.T) {
	t.Run("provider", func(t *testing.T) {
		ts := newTestServer(t, Config{Provider: &tunableProvider{}})
		startup := findLogLine(t, ts.stderr.String(), "event=startup")
		if !strings.Contains(startup, "db=provider") {
			t.Errorf("startup line does not name the database source:\n%s", startup)
		}
		limits := findLogLine(t, ts.stderr.String(), "event=limits")
		if !strings.Contains(limits, "pool_max_open=") {
			t.Errorf("limits line omits the pool bounds a provider-backed server applies:\n%s", limits)
		}
	})

	t.Run("roles", func(t *testing.T) {
		ts := newTestServer(t, rolesConfig(Config{}))
		startup := findLogLine(t, ts.stderr.String(), "event=startup")
		if !strings.Contains(startup, "db=roles") {
			t.Errorf("startup line does not name the database source:\n%s", startup)
		}
		// The pool belongs to whatever the backend is; this server neither owns
		// it nor tuned it, so publishing bounds it did not set would be a lie.
		limits := findLogLine(t, ts.stderr.String(), "event=limits")
		if strings.Contains(limits, "pool_") {
			t.Errorf("limits line publishes pool bounds this server never applied:\n%s", limits)
		}
		if strings.Contains(ts.stderr.String(), "event=pool_limits_unavailable") {
			t.Errorf("a roles-backed server announced a missing provider knob:\n%s", ts.stderr.String())
		}
	})
}

// TestARolesRequestReportsNoUnitOfWorkTime states the other half out loud: the
// number really is zero, and it is zero because nothing on this path opens a
// unit of work — not because the timing wrapper was dropped.
func TestARolesRequestReportsNoUnitOfWorkTime(t *testing.T) {
	ts := newTestServer(t, rolesConfig(Config{}))

	if resp := ts.get(t, "/v0/beads/ready"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	line := findLogLine(t, ts.stderr.String(), "op="+OpListReadyWork)
	if !strings.Contains(line, "uow_ms=0.000") {
		t.Errorf("a roles-backed request reported unit-of-work time it cannot have spent:\n%s", line)
	}
}

// TestWithUOWRefusesWithoutAProvider: the helper's whole job is to open a unit
// of work, and a roles-backed server has nothing to open one from. An error is
// the answer; a nil dereference inside a handler is not.
func TestWithUOWRefusesWithoutAProvider(t *testing.T) {
	ts := newTestServer(t, rolesConfig(Config{}))

	ran := false
	err := ts.WithUOW(context.Background(), &reqInfo{}, func(uow.UnitOfWork) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("WithUOW succeeded on a server with no unit-of-work provider")
	}
	if ran {
		t.Error("the callback ran without a unit of work")
	}
}

// TestARoleThatAnswersWithNothingIsNotDereferenced covers the one guarantee a
// configured role cannot be asked for: that a call which reports no error
// carries the value the handler is about to dereference.
//
// It used to hold BY CONSTRUCTION. s.reader() could only return
// uow.NewIssueReader(...), whose Get routes through workapi.GetIssueOrWisp —
// a function whose whole reason to exist is folding both miss shapes into
// ErrNotFound so that no caller can write `if err != nil || issue == nil` and
// report a dropped connection as "not found". A caller-supplied role is
// ordinary code and carries no such guarantee, and both handlers that hold a
// pointer from a role dereference it unconditionally.
func TestARoleThatAnswersWithNothingIsNotDereferenced(t *testing.T) {
	// The answer is the SAME 404 a real miss produces, byte for byte: the
	// document states one not-found body, and a client must not be able to
	// tell a broken role from an absent issue.
	t.Run("a reader with no detail view is the documented miss", func(t *testing.T) {
		silent := newTestServer(t, rolesConfig(Config{}))
		missed := newTestServer(t, Config{
			Reader:        &roleReader{err: fmt.Errorf("get bd-1: %w", storage.ErrNotFound)},
			Claimer:       &roleClaimer{},
			Settings:      &roleSettings{},
			Stats:         &roleStats{},
			CycleDetector: &roleCycleDetector{},
			EdgeReader:    &roleEdgeReader{},
			ReadyCounter:  &roleReadyCounter{},
			Querier:       &roleQuerier{},
		})

		got := silent.get(t, "/v0/beads/issues/bd-1")
		want := missed.get(t, "/v0/beads/issues/bd-1")
		if got.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", got.StatusCode, readAll(t, got))
		}
		gotBody, wantBody := decodeBody(t, got), decodeBody(t, want)
		// request_id is per request and is the only member that may differ.
		delete(gotBody, "request_id")
		delete(wantBody, "request_id")
		if !reflect.DeepEqual(gotBody, wantBody) {
			t.Errorf("body = %v, want the body a real miss produces: %v", gotBody, wantBody)
		}
		assertNoPanic(t, silent)
	})

	// A claim that reports success without a row is not a documented outcome —
	// there is no wire code for it — so it is the generic 500. What it must not
	// be is a panic: the response is recovered into the same status, but the
	// fault reaches the log as a stack trace instead of as an error, and the
	// panic path writes no request_error line for an operator to alert on.
	t.Run("a claimer with no issue is the generic failure", func(t *testing.T) {
		ts := newTestServer(t, Config{
			Reader:        &roleReader{},
			Claimer:       &roleClaimer{result: issueops.ClaimResult{Changed: true}},
			Settings:      &roleSettings{},
			Stats:         &roleStats{},
			CycleDetector: &roleCycleDetector{},
			EdgeReader:    &roleEdgeReader{},
			ReadyCounter:  &roleReadyCounter{},
			Querier:       &roleQuerier{},
		})

		resp := ts.claim(t, claimPath, `{"actor":"alice"}`)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", resp.StatusCode, readAll(t, resp))
		}
		if body := decodeBody(t, resp); body["code"] != string(CodeInternal) {
			t.Errorf("code = %v, want %s", body["code"], CodeInternal)
		}
		assertNoPanic(t, ts)
		if line := findLogLine(t, ts.stderr.String(), "event=request_error"); !strings.Contains(line, "claim") {
			t.Errorf("the 500 is logged without naming the operation that produced it:\n%s", line)
		}
	})
}

// assertNoPanic fails when the server recovered a panic. Every case in this
// file is one a handler could reach by trusting a role's return value, so the
// status alone does not distinguish a refusal from a recovered dereference.
func assertNoPanic(t *testing.T, ts *testServer) {
	t.Helper()
	if log := ts.stderr.String(); strings.Contains(log, "event=panic") {
		t.Errorf("a handler dereferenced what the role did not return:\n%s", log)
	}
}

// hookableStore is the smallest thing storage.NewHookFiringStore will decorate:
// the DoltStorage surface is embedded nil because IssueClaimer is the only
// method this test ever reaches through it.
type hookableStore struct {
	storage.DoltStorage
	claimer issueops.Claimer
}

func (s hookableStore) IssueClaimer() (issueops.Claimer, error) { return s.claimer, nil }

// TestListenRefusesARoleThatFiresTheWorkspaceHooks.
//
// `bd serve` documents that hooks do not fire, and until roles became
// configuration nothing could make them: the provider seam builds its claimer
// from a unit of work, which carries no hook layer at all. A store is the
// opposite — its accessors hand out its decorators, deliberately, so that a CLI
// claim keeps its on_update — and bd's own chain is
// caller -> HookFiringStore -> InstrumentedStorage -> raw. So the one line a
// caller with a store would obviously write, store.IssueClaimer(), returns
// exactly the claimer this server may not serve.
//
// The refusal is at Listen because the alternative is silent: a server built
// that way answers every request correctly and runs a user's subprocess per
// landed claim for as long as it is up.
func TestListenRefusesARoleThatFiresTheWorkspaceHooks(t *testing.T) {
	// A nil runner, which is what a HookFiringStore built without one carries.
	// The refusal must not depend on that: the type's job is to fire hooks, and
	// a server that admitted this one would be a config change away from
	// breaking its own contract.
	hooked := storage.NewHookFiringStore(hookableStore{claimer: &roleClaimer{}}, nil)

	fromTheStore, err := hooked.IssueClaimer()
	if err != nil {
		t.Fatalf("IssueClaimer: %v", err)
	}
	if !storage.RoleFiresHooks(fromTheStore) {
		t.Fatal("the store's own accessor no longer returns a hook-firing claimer; this test proves nothing")
	}

	listen := func(cl issueops.Claimer) (*Server, error) {
		cfg := rolesConfig(Config{Claimer: cl})
		cfg.Addr = "127.0.0.1:0"
		cfg.Stdout = io.Discard
		cfg.Stderr = io.Discard
		return Listen(cfg)
	}

	if _, err := listen(fromTheStore); err == nil {
		t.Error("Listen bound a server whose claim route runs the workspace's hook scripts")
	} else if !strings.Contains(err.Error(), "hooks") {
		t.Errorf("refusal %q does not say what is wrong with the role", err)
	}

	// And the store BENEATH the decorator is the value the doc sends a caller
	// to, so it has to be servable. Without this the guard could be a blanket
	// refusal of every store-backed claimer and still pass.
	fromBeneath, err := hooked.Unwrap().IssueClaimer()
	if err != nil {
		t.Fatalf("IssueClaimer on the undecorated store: %v", err)
	}
	srv, err := listen(fromBeneath)
	if err != nil {
		t.Fatalf("Listen: %v, want a bound server for the claimer beneath the hook layer", err)
	}
	t.Cleanup(func() { _ = srv.http.Close() })
}
