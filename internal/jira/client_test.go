package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewClientDefaultsToV3(t *testing.T) {
	c := NewClient("https://example.atlassian.net", "user@example.com", "token123")
	if c.APIVersion != "3" {
		t.Errorf("NewClient APIVersion = %q, want %q", c.APIVersion, "3")
	}
}

func TestAPIBase(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"3", "https://jira.example.com/rest/api/3"},
		{"2", "https://jira.example.com/rest/api/2"},
		{"", "https://jira.example.com/rest/api/3"}, // empty defaults to 3
	}
	for _, tt := range tests {
		c := &Client{URL: "https://jira.example.com", APIVersion: tt.version}
		if got := c.apiBase(); got != tt.want {
			t.Errorf("apiBase() with version %q = %q, want %q", tt.version, got, tt.want)
		}
	}
}

// newTestClient creates a Client pointed at a test server with a fixed API token.
func newTestClient(serverURL, version string) *Client {
	c := NewClient(serverURL, "user", "token")
	c.APIVersion = version
	return c
}

func TestSearchIssuesURL(t *testing.T) {
	tests := []struct {
		version      string
		wantPath     string
		wantPathPart string // substring check on full path
	}{
		{"3", "/rest/api/3/search/jql", "search/jql"},
		{"2", "/rest/api/2/search", ""},
	}

	for _, tt := range tests {
		t.Run("v"+tt.version, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(SearchResult{Total: 0, Issues: []Issue{}})
			}))
			defer srv.Close()

			c := newTestClient(srv.URL, tt.version)
			_, err := c.SearchIssues(context.Background(), "project = TEST")
			if err != nil {
				t.Fatalf("SearchIssues error: %v", err)
			}
			if gotPath != tt.wantPath {
				t.Errorf("SearchIssues path = %q, want %q", gotPath, tt.wantPath)
			}
		})
	}
}

func TestFetchIssueTimestampURL(t *testing.T) {
	tests := []struct {
		version  string
		wantPath string
	}{
		{"3", "/rest/api/3/issue/PROJ-1"},
		{"2", "/rest/api/2/issue/PROJ-1"},
	}

	for _, tt := range tests {
		t.Run("v"+tt.version, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"fields": map[string]string{"updated": "2024-01-15T10:30:00.000+0000"},
				})
			}))
			defer srv.Close()

			c := newTestClient(srv.URL, tt.version)
			_, err := c.FetchIssueTimestamp(context.Background(), "PROJ-1")
			if err != nil {
				t.Fatalf("FetchIssueTimestamp error: %v", err)
			}
			if gotPath != tt.wantPath {
				t.Errorf("FetchIssueTimestamp path = %q, want %q", gotPath, tt.wantPath)
			}
		})
	}
}

func TestGetIssueURL(t *testing.T) {
	tests := []struct {
		version  string
		wantPath string
	}{
		{"3", "/rest/api/3/issue/PROJ-42"},
		{"2", "/rest/api/2/issue/PROJ-42"},
	}

	for _, tt := range tests {
		t.Run("v"+tt.version, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(Issue{ID: "1", Key: "PROJ-42"})
			}))
			defer srv.Close()

			c := newTestClient(srv.URL, tt.version)
			_, err := c.GetIssue(context.Background(), "PROJ-42")
			if err != nil {
				t.Fatalf("GetIssue error: %v", err)
			}
			if gotPath != tt.wantPath {
				t.Errorf("GetIssue path = %q, want %q", gotPath, tt.wantPath)
			}
		})
	}
}

func TestCreateIssueURL(t *testing.T) {
	tests := []struct {
		version        string
		wantCreatePath string
	}{
		{"3", "/rest/api/3/issue"},
		{"2", "/rest/api/2/issue"},
	}

	for _, tt := range tests {
		t.Run("v"+tt.version, func(t *testing.T) {
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					// Create response
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "1", "key": "PROJ-1"})
				} else {
					// GetIssue follow-up
					_ = json.NewEncoder(w).Encode(Issue{ID: "1", Key: "PROJ-1"})
				}
			}))
			defer srv.Close()

			c := newTestClient(srv.URL, tt.version)
			_, err := c.CreateIssue(context.Background(), map[string]interface{}{"summary": "test"})
			if err != nil {
				t.Fatalf("CreateIssue error: %v", err)
			}
			if len(paths) == 0 || paths[0] != tt.wantCreatePath {
				t.Errorf("CreateIssue POST path = %q, want %q", paths[0], tt.wantCreatePath)
			}
		})
	}
}

func TestUpdateIssueURL(t *testing.T) {
	tests := []struct {
		version  string
		wantPath string
	}{
		{"3", "/rest/api/3/issue/PROJ-7"},
		{"2", "/rest/api/2/issue/PROJ-7"},
	}

	for _, tt := range tests {
		t.Run("v"+tt.version, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			c := newTestClient(srv.URL, tt.version)
			err := c.UpdateIssue(context.Background(), "PROJ-7", map[string]interface{}{"summary": "updated"})
			if err != nil {
				t.Fatalf("UpdateIssue error: %v", err)
			}
			if gotPath != tt.wantPath {
				t.Errorf("UpdateIssue path = %q, want %q", gotPath, tt.wantPath)
			}
		})
	}
}

func TestSearchIssuesQueryParam(t *testing.T) {
	// Verify jql is passed as a query parameter for both versions.
	for _, version := range []string{"2", "3"} {
		t.Run("v"+version, func(t *testing.T) {
			var gotJQL string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotJQL = r.URL.Query().Get("jql")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(SearchResult{Total: 0, Issues: []Issue{}})
			}))
			defer srv.Close()

			c := newTestClient(srv.URL, version)
			_, err := c.SearchIssues(context.Background(), "project = TEST AND status = Open")
			if err != nil {
				t.Fatalf("SearchIssues error: %v", err)
			}
			if !strings.Contains(gotJQL, "project = TEST") {
				t.Errorf("jql query param = %q, want it to contain %q", gotJQL, "project = TEST")
			}
		})
	}
}
