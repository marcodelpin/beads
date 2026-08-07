//go:build cgo

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// End-to-end for the dependency-graph writes, against real Dolt through a real
// `bd serve` subprocess. The pure tests in internal/httpapi cover the wire edge
// against a fake role; what only this level can prove is what the STORAGE
// TRANSACTION did — that a removal really removed, and that a refused batch
// left the graph exactly as it found it.

// postJSON posts body to path with the documented media type and returns the
// status and decoded body.
func (sp *serveProcess) postJSON(t *testing.T, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, sp.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sp.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v\nstderr:\n%s", path, err, sp.stderr.String())
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode %s body %q: %v", path, raw, err)
		}
	}
	return resp.StatusCode, m
}

// storedEdges reads the edges leaving id back out of the database through the
// documented stored-edge read. It is the read-back every assertion below is
// made against: what the graph holds after a write, not what the write said.
func (sp *serveProcess) storedEdges(t *testing.T, id string) []map[string]any {
	t.Helper()
	status, body, _ := sp.get(t, "/v0/beads/dependencies?issue_id="+id)
	if status != http.StatusOK {
		t.Fatalf("GET the stored edges of %s: status = %d: %v", id, status, body)
	}
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("stored edges of %s: items = %#v, want an array", id, body["items"])
	}
	edges := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		edge, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("stored edges of %s: item = %#v, want an object", id, item)
		}
		// The read is per-source, so an edge that names another source is a
		// read this test cannot reason about.
		if edge["issue_id"] == id {
			edges = append(edges, edge)
		}
	}
	return edges
}

func (sp *serveProcess) removeDependency(t *testing.T, issueID, dependsOnID, actor string) (int, map[string]any) {
	t.Helper()
	return sp.postJSON(t, "/v0/beads/dependencies:remove",
		fmt.Sprintf(`{"actor":%q,"issue_id":%q,"depends_on_id":%q}`, actor, issueID, dependsOnID))
}

func TestProxiedServerServeRemoveDependency(t *testing.T) {
	requireSharedProxiedServer(t)
	t.Parallel()
	bd := buildEmbeddedBD(t)
	p := newSharedProxiedProject(t, bd, "srvdeprm")
	sp := startServe(t, bd, p.dir, bdProxiedEnv(p.dir))

	// The retained proof for this operation: the idempotent re-remove, read
	// back out of the database between the calls. A fake role can report
	// `removed: false` for any reason at all; only a real store proves that the
	// second call found nothing because the first call had already taken it.
	t.Run("the second removal finds nothing and changes nothing", func(t *testing.T) {
		source := bdProxiedCreate(t, bd, p.dir, "removal source", "-p", "1")
		target := bdProxiedCreate(t, bd, p.dir, "removal target", "-p", "1")
		bdProxiedDep(t, bd, p.dir, "add", source.ID, target.ID)

		if edges := sp.storedEdges(t, source.ID); len(edges) != 1 {
			t.Fatalf("the CLI-written edge is not in the graph: %v", edges)
		}

		status, body := sp.removeDependency(t, source.ID, target.ID, "http-agent")
		if status != http.StatusOK {
			t.Fatalf("first removal: status = %d, want 200: %v", status, body)
		}
		if body["removed"] != true {
			t.Errorf("first removal: removed = %v, want true", body["removed"])
		}
		if edges := sp.storedEdges(t, source.ID); len(edges) != 0 {
			t.Fatalf("the edge survived a removal that reported success: %v", edges)
		}
		// And the CLI reads the same graph through its own path.
		if out := bdProxiedDep(t, bd, p.dir, "list", source.ID); strings.Contains(out, target.ID) {
			t.Errorf("`bd dep list` still shows the removed edge:\n%s", out)
		}

		status, body = sp.removeDependency(t, source.ID, target.ID, "http-agent")
		if status != http.StatusOK {
			t.Fatalf("re-removal: status = %d, want 200 — a missing edge is not a refusal: %v", status, body)
		}
		if body["removed"] != false {
			t.Errorf("re-removal: removed = %v, want false", body["removed"])
		}
		if edges := sp.storedEdges(t, source.ID); len(edges) != 0 {
			t.Errorf("the re-removal changed the graph: %v", edges)
		}
	})

	// An endpoint id that names nothing is a 200 with `removed: false`, not a
	// 404. This is the operation's absent-code row proved against a real
	// database rather than against a fake that could not have looked.
	t.Run("an endpoint that names nothing is not a 404", func(t *testing.T) {
		source := bdProxiedCreate(t, bd, p.dir, "no edges at all", "-p", "2")

		status, body := sp.removeDependency(t, source.ID, "bd-nosuchissue", "http-agent")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %v", status, body)
		}
		if body["removed"] != false {
			t.Errorf("removed = %v, want false", body["removed"])
		}

		status, body = sp.removeDependency(t, "bd-nosuchissue", source.ID, "http-agent")
		if status != http.StatusOK {
			t.Fatalf("ghost source: status = %d, want 200: %v", status, body)
		}
		if body["removed"] != false {
			t.Errorf("ghost source: removed = %v, want false", body["removed"])
		}
	})

	t.Run("a refused request writes nothing", func(t *testing.T) {
		source := bdProxiedCreate(t, bd, p.dir, "refusals keep the edge", "-p", "2")
		target := bdProxiedCreate(t, bd, p.dir, "refusals keep the target", "-p", "2")
		bdProxiedDep(t, bd, p.dir, "add", source.ID, target.ID)

		for _, body := range []string{
			fmt.Sprintf(`{"actor":"   ","issue_id":%q,"depends_on_id":%q}`, source.ID, target.ID),
			fmt.Sprintf(`{"actor":"agent\nbd serve: forged","issue_id":%q,"depends_on_id":%q}`, source.ID, target.ID),
			fmt.Sprintf(`{"actor":"agent","issue_id":%q}`, source.ID),
			fmt.Sprintf(`{"actor":"agent","issue_id":%q,"depends_on_id":%q,"force":true}`, source.ID, target.ID),
		} {
			status, problem := sp.postJSON(t, "/v0/beads/dependencies:remove", body)
			if status != http.StatusBadRequest {
				t.Fatalf("body %.50q: status = %d, want 400: %v", body, status, problem)
			}
			if problem["code"] != "invalid_argument" {
				t.Errorf("body %.50q: code = %v, want invalid_argument", body, problem["code"])
			}
		}

		if edges := sp.storedEdges(t, source.ID); len(edges) != 1 {
			t.Errorf("a refused removal changed the graph: %v", edges)
		}
	})

	sp.shutdown(t)
}
