package linear

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCanonicalizeLinearExternalRef(t *testing.T) {
	tests := []struct {
		name        string
		externalRef string
		want        string
		ok          bool
	}{
		{
			name:        "slugged url",
			externalRef: "https://linear.app/crown-dev/issue/BEA-93/updated-title-for-beads",
			want:        "https://linear.app/crown-dev/issue/BEA-93",
			ok:          true,
		},
		{
			name:        "canonical url",
			externalRef: "https://linear.app/crown-dev/issue/BEA-93",
			want:        "https://linear.app/crown-dev/issue/BEA-93",
			ok:          true,
		},
		{
			name:        "not linear",
			externalRef: "https://example.com/issues/BEA-93",
			want:        "",
			ok:          false,
		},
	}

	for _, tt := range tests {
		got, ok := CanonicalizeLinearExternalRef(tt.externalRef)
		if ok != tt.ok {
			t.Fatalf("%s: ok=%v, want %v", tt.name, ok, tt.ok)
		}
		if got != tt.want {
			t.Fatalf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestNewClient(t *testing.T) {
	client := NewClient("test-api-key", "test-team-id")

	if client.APIKey != "test-api-key" {
		t.Errorf("APIKey = %q, want %q", client.APIKey, "test-api-key")
	}
	if client.TeamID != "test-team-id" {
		t.Errorf("TeamID = %q, want %q", client.TeamID, "test-team-id")
	}
	if client.Endpoint != DefaultAPIEndpoint {
		t.Errorf("Endpoint = %q, want %q", client.Endpoint, DefaultAPIEndpoint)
	}
	if client.HTTPClient == nil {
		t.Error("HTTPClient should not be nil")
	}
}

func TestWithEndpoint(t *testing.T) {
	client := NewClient("key", "team")
	customEndpoint := "https://custom.linear.app/graphql"

	newClient := client.WithEndpoint(customEndpoint)

	if newClient.Endpoint != customEndpoint {
		t.Errorf("Endpoint = %q, want %q", newClient.Endpoint, customEndpoint)
	}
	// Original should be unchanged
	if client.Endpoint != DefaultAPIEndpoint {
		t.Errorf("Original endpoint changed: %q", client.Endpoint)
	}
	// Other fields preserved
	if newClient.APIKey != "key" {
		t.Errorf("APIKey not preserved: %q", newClient.APIKey)
	}
}

func TestWithHTTPClient(t *testing.T) {
	client := NewClient("key", "team")
	customHTTPClient := &http.Client{Timeout: 60 * time.Second}

	newClient := client.WithHTTPClient(customHTTPClient)

	if newClient.HTTPClient != customHTTPClient {
		t.Error("HTTPClient not set correctly")
	}
	// Other fields preserved
	if newClient.APIKey != "key" {
		t.Errorf("APIKey not preserved: %q", newClient.APIKey)
	}
	if newClient.Endpoint != DefaultAPIEndpoint {
		t.Errorf("Endpoint not preserved: %q", newClient.Endpoint)
	}
}

func TestExtractLinearIdentifier(t *testing.T) {
	tests := []struct {
		name        string
		externalRef string
		want        string
	}{
		{
			name:        "standard URL",
			externalRef: "https://linear.app/team/issue/PROJ-123",
			want:        "PROJ-123",
		},
		{
			name:        "URL with slug",
			externalRef: "https://linear.app/team/issue/PROJ-456/some-title-here",
			want:        "PROJ-456",
		},
		{
			name:        "URL with trailing slash",
			externalRef: "https://linear.app/team/issue/ABC-789/",
			want:        "ABC-789",
		},
		{
			name:        "non-linear URL",
			externalRef: "https://jira.example.com/browse/PROJ-123",
			want:        "",
		},
		{
			name:        "empty string",
			externalRef: "",
			want:        "",
		},
		{
			name:        "malformed URL",
			externalRef: "not-a-url",
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractLinearIdentifier(tt.externalRef)
			if got != tt.want {
				t.Errorf("ExtractLinearIdentifier(%q) = %q, want %q", tt.externalRef, got, tt.want)
			}
		})
	}
}

func TestIsLinearExternalRef(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{"https://linear.app/team/issue/PROJ-123", true},
		{"https://linear.app/team/issue/PROJ-123/slug", true},
		{"https://jira.example.com/browse/PROJ-123", false},
		{"https://github.com/org/repo/issues/123", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got := IsLinearExternalRef(tt.ref)
			if got != tt.want {
				t.Errorf("IsLinearExternalRef(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"integer seconds", "30", 30 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"empty", "", 0},
		{"non-numeric", "abc", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRetryAfter(tt.value)
			if got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseRateLimitHeaders(t *testing.T) {
	t.Run("all headers present", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "45")
		h.Set("X-RateLimit-Requests-Remaining", "80")
		h.Set("X-RateLimit-Requests-Reset", "2026-01-01T00:00:00Z")

		info := parseRateLimitHeaders(h)
		if info.RetryAfter != 45*time.Second {
			t.Errorf("RetryAfter = %v, want 45s", info.RetryAfter)
		}
		if info.RequestsRemaining != 80 {
			t.Errorf("RequestsRemaining = %d, want 80", info.RequestsRemaining)
		}
		wantReset, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
		if !info.RequestsReset.Equal(wantReset) {
			t.Errorf("RequestsReset = %v, want %v", info.RequestsReset, wantReset)
		}
	})

	t.Run("no headers", func(t *testing.T) {
		info := parseRateLimitHeaders(http.Header{})
		if info.RetryAfter != 0 {
			t.Errorf("RetryAfter = %v, want 0", info.RetryAfter)
		}
		if info.RequestsRemaining != -1 {
			t.Errorf("RequestsRemaining = %d, want -1", info.RequestsRemaining)
		}
		if !info.RequestsReset.IsZero() {
			t.Errorf("RequestsReset should be zero, got %v", info.RequestsReset)
		}
	})
}

// mockServer returns an httptest.Server and a function to set the handler per request.
func mockServer(t *testing.T, handler http.HandlerFunc) *httpTestServer {
	t.Helper()
	s := &httpTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requestCount++
		handler(w, r)
	}))
	t.Cleanup(s.Server.Close)
	return s
}

type httpTestServer struct {
	Server       *httptest.Server
	requestCount int
}

func TestExecute_RetryAfterHeader(t *testing.T) {
	attempt := 0
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})

	client := NewClient("key", "team").WithEndpoint(srv.Server.URL)
	ctx := context.Background()
	data, err := client.Execute(ctx, &GraphQLRequest{Query: "{ viewer { id } }"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if data == nil {
		t.Fatal("Execute returned nil data")
	}
	if srv.requestCount != 2 {
		t.Errorf("expected 2 requests (1 retry), got %d", srv.requestCount)
	}
}

func TestExecute_NoRetryAfterFallsBackToExponential(t *testing.T) {
	attempt := 0
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})

	client := NewClient("key", "team").WithEndpoint(srv.Server.URL)
	ctx := context.Background()

	start := time.Now()
	data, err := client.Execute(ctx, &GraphQLRequest{Query: "{ viewer { id } }"})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if data == nil {
		t.Fatal("Execute returned nil data")
	}
	// First attempt exponential backoff: RetryDelay * 2^0 = 1s, plus up to 500ms jitter.
	// Should be at least ~1s but we allow some slack.
	if elapsed < 800*time.Millisecond {
		t.Errorf("expected exponential backoff of ~1s, got %v", elapsed)
	}
}

func TestExecute_CircuitBreakerTrips(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Requests-Remaining", "50")
		w.Header().Set("X-RateLimit-Requests-Reset", "2026-06-01T00:00:00Z")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})

	client := NewClient("key", "team").WithEndpoint(srv.Server.URL)
	ctx := context.Background()

	_, err := client.Execute(ctx, &GraphQLRequest{Query: "{ viewer { id } }"})
	if err == nil {
		t.Fatal("expected ErrRateLimitExhausted, got nil")
	}

	var rlErr *ErrRateLimitExhausted
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected ErrRateLimitExhausted, got %T: %v", err, err)
	}
	if rlErr.Remaining != 50 {
		t.Errorf("Remaining = %d, want 50", rlErr.Remaining)
	}
	if rlErr.Floor != DefaultRateLimitFloor {
		t.Errorf("Floor = %d, want %d", rlErr.Floor, DefaultRateLimitFloor)
	}
}

func TestExecute_CircuitBreakerAllowsAboveFloor(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Requests-Remaining", "200")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})

	client := NewClient("key", "team").WithEndpoint(srv.Server.URL)
	ctx := context.Background()

	data, err := client.Execute(ctx, &GraphQLRequest{Query: "{ viewer { id } }"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if data == nil {
		t.Fatal("Execute returned nil data")
	}
}

func TestExecute_CircuitBreakerCustomFloor(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Requests-Remaining", "80")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})

	client := NewClient("key", "team").WithEndpoint(srv.Server.URL).WithRateLimitFloor(50)
	ctx := context.Background()

	data, err := client.Execute(ctx, &GraphQLRequest{Query: "{ viewer { id } }"})
	if err != nil {
		t.Fatalf("Execute with custom floor returned error: %v", err)
	}
	if data == nil {
		t.Fatal("Execute returned nil data")
	}
}

func TestExecute_NoRateLimitHeaders(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})

	client := NewClient("key", "team").WithEndpoint(srv.Server.URL)
	ctx := context.Background()

	data, err := client.Execute(ctx, &GraphQLRequest{Query: "{ viewer { id } }"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if data == nil {
		t.Fatal("Execute returned nil data")
	}
}

func TestWithRateLimitFloor(t *testing.T) {
	client := NewClient("key", "team")
	c2 := client.WithRateLimitFloor(42)

	if c2.RateLimitFloor != 42 {
		t.Errorf("RateLimitFloor = %d, want 42", c2.RateLimitFloor)
	}
	if c2.APIKey != "key" {
		t.Errorf("APIKey not preserved: %q", c2.APIKey)
	}
	if client.RateLimitFloor != 0 {
		t.Errorf("original RateLimitFloor changed: %d", client.RateLimitFloor)
	}
}

// Note: BuildStateCache and FindStateForBeadsStatus require API calls
// and would need mocking to test. Skipping unit tests for those.
