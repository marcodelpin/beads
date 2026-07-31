package httpapi

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/beads/internal/httpapi/spec"
)

// These tests are the spec drift gates. They are pure — no database, no
// server, no build tag — because they must run in the required PR job. With
// the wire structs pinned to canonical Go types via x-go-type, the compiler
// cannot see the spec at all, so if these move to a conditional CI tier the
// contract stops being enforced.

var httpVerbs = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true, "patch": true,
	"head": true, "options": true, "trace": true,
}

type specOp struct {
	path   string
	method string
	op     map[string]any
}

func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(spec.OpenAPIV0(), &doc); err != nil {
		t.Fatalf("parse embedded openapi document: %v", err)
	}
	return doc
}

func mapAt(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := parent[key]
	if !ok {
		t.Fatalf("missing key %q", key)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("key %q is %T, want a mapping", key, v)
	}
	return m
}

// resolveRef follows a local $ref one level, which is all this document uses.
func resolveRef(t *testing.T, doc map[string]any, node map[string]any) map[string]any {
	t.Helper()
	ref, ok := node["$ref"].(string)
	if !ok {
		return node
	}
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		t.Fatalf("only local $refs are supported, got %q", ref)
	}
	cur := doc
	parts := strings.Split(rest, "/")
	for i, part := range parts {
		next, ok := cur[part]
		if !ok {
			t.Fatalf("$ref %q: no such node %q", ref, strings.Join(parts[:i+1], "/"))
		}
		m, ok := next.(map[string]any)
		if !ok {
			t.Fatalf("$ref %q: node %q is %T, want a mapping", ref, part, next)
		}
		cur = m
	}
	return cur
}

func specOps(t *testing.T, doc map[string]any) map[string]specOp {
	t.Helper()
	out := map[string]specOp{}
	for path, item := range mapAt(t, doc, "paths") {
		methods, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("path %q is %T, want a mapping", path, item)
		}
		for method, raw := range methods {
			if !httpVerbs[strings.ToLower(method)] {
				continue
			}
			op, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("%s %s is %T, want a mapping", method, path, raw)
			}
			id, _ := op["operationId"].(string)
			if id == "" {
				t.Fatalf("%s %s has no operationId", strings.ToUpper(method), path)
			}
			if prev, dup := out[id]; dup {
				t.Fatalf("operationId %q used twice: %s %s and %s %s",
					id, prev.method, prev.path, strings.ToUpper(method), path)
			}
			out[id] = specOp{path: path, method: strings.ToUpper(method), op: op}
		}
	}
	return out
}

func sortedCodes(codes []Code) []string {
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		out = append(out, string(c))
	}
	sort.Strings(out)
	return out
}

// TestSpecGovernance pins the document-level invariants that make the rest of
// the contract legible: one OpenAPI version, one error shape everywhere, a
// machine-readable code list on every documented failure, and no vendor
// vocabulary.
func TestSpecGovernance(t *testing.T) {
	doc := loadSpec(t)

	if got, _ := doc["openapi"].(string); got != "3.0.3" {
		t.Errorf("openapi = %q, want 3.0.3", got)
	}
	if got, _ := doc["x-bd-source"].(string); got != "spec-first" {
		t.Errorf("x-bd-source = %q, want spec-first (the document is hand-written and generates the Go types)", got)
	}
	if got, _ := mapAt(t, doc, "info")["version"].(string); got == "" {
		t.Error("info.version is empty")
	}

	// The OSS surface stays vendor-neutral: no product names, no hosted-only
	// extensions.
	if raw := string(spec.OpenAPIV0()); strings.Contains(raw, "x-gc-") {
		t.Error("document carries x-gc-* extensions; the OSS spec must stay vendor-neutral")
	}

	for id, so := range specOps(t, doc) {
		if _, ok := so.op["summary"].(string); !ok {
			t.Errorf("%s: no summary", id)
		}
		if _, ok := so.op["description"].(string); !ok {
			t.Errorf("%s: no description", id)
		}
		for status, raw := range mapAt(t, so.op, "responses") {
			node, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("%s %s response is %T, want a mapping", id, status, raw)
			}
			resp := resolveRef(t, doc, node)
			content := mapAt(t, resp, "content")
			if len(content) != 1 {
				t.Errorf("%s %s: %d media types, want exactly 1", id, status, len(content))
			}
			if strings.HasPrefix(status, "2") {
				if _, ok := content["application/json"]; !ok {
					t.Errorf("%s %s: success bodies are application/json", id, status)
				}
				continue
			}
			body, ok := content["application/problem+json"]
			if !ok {
				t.Errorf("%s %s: every non-2xx body is application/problem+json", id, status)
				continue
			}
			bodyMap, ok := body.(map[string]any)
			if !ok {
				t.Fatalf("%s %s: content is %T, want a mapping", id, status, body)
			}
			schema := mapAt(t, bodyMap, "schema")
			if got, _ := schema["$ref"].(string); got != "#/components/schemas/Problem" {
				t.Errorf("%s %s: schema %q, want the one Problem envelope", id, status, got)
			}
			if _, ok := resp["x-bd-codes"]; !ok {
				t.Errorf("%s %s: no x-bd-codes; every documented failure names its machine codes", id, status)
			}
		}
	}
}

// TestSpecDefaultsMatchSharedConstants keeps the document honest about the two
// limit defaults and about limit=0. The whole point of sharing constants
// between the CLI flag and the HTTP parameter is that the two surfaces cannot
// diverge; a spec that states a different number would reintroduce the
// divergence in the one place clients actually read.
func TestSpecDefaultsMatchSharedConstants(t *testing.T) {
	doc := loadSpec(t)
	ops := specOps(t, doc)

	for _, tc := range []struct {
		opID string
		want int
	}{
		{OpListIssues, DefaultListLimit},
		{OpListReadyWork, DefaultReadyLimit},
	} {
		so, ok := ops[tc.opID]
		if !ok {
			t.Fatalf("operation %q missing from the spec", tc.opID)
		}
		limit := specParam(t, so, "limit")
		schema := mapAt(t, limit, "schema")

		got, ok := schema["default"].(int)
		if !ok {
			t.Fatalf("%s: limit has no integer default (%T)", tc.opID, schema["default"])
		}
		if got != tc.want {
			t.Errorf("%s: spec documents limit default %d, shared constant is %d", tc.opID, got, tc.want)
		}
		if lo, ok := schema["minimum"].(int); !ok || lo != 0 {
			t.Errorf("%s: limit minimum = %v, want 0 (limit=0 is a legal, meaningful value)", tc.opID, schema["minimum"])
		}

		// limit=0 means unlimited on both surfaces. If this phrasing ever
		// changes, change it deliberately — clients depend on the behavior
		// and this is the only place the wire documents it.
		desc, _ := limit["description"].(string)
		if !strings.Contains(desc, "`0` means unlimited") {
			t.Errorf("%s: limit description does not document that `0` means unlimited", tc.opID)
		}
		if !strings.Contains(desc, "--allow-non-loopback") {
			t.Errorf("%s: limit description does not document the non-loopback refusal of limit=0", tc.opID)
		}
	}
}

func specParam(t *testing.T, so specOp, name string) map[string]any {
	t.Helper()
	raw, ok := so.op["parameters"].([]any)
	if !ok {
		t.Fatalf("%s %s has no parameters", so.method, so.path)
	}
	for _, p := range raw {
		param, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("%s %s: parameter is %T, want a mapping", so.method, so.path, p)
		}
		if got, _ := param["name"].(string); got == name {
			return param
		}
	}
	t.Fatalf("%s %s: no %q parameter", so.method, so.path, name)
	return nil
}

// TestSpecStatusCodesMatchHandlerTable is the rule that keeps the error
// vocabulary from growing by accident: every documented status+code pair is
// permanent wire surface, so the spec may document exactly what the mapping in
// problem.go can produce, and no more.
//
// The Host-header middleware's 400 invalid_argument is reachable on every
// route and is documented once at the document level rather than per
// operation, so it is absent from both sides here by construction.
func TestSpecStatusCodesMatchHandlerTable(t *testing.T) {
	doc := loadSpec(t)
	ops := specOps(t, doc)

	if len(ops) != len(operationCodes) {
		t.Errorf("spec documents %d operations, the handler table declares %d", len(ops), len(operationCodes))
	}
	for id := range ops {
		if _, ok := operationCodes[id]; !ok {
			t.Errorf("spec operation %q has no entry in the handler table", id)
		}
	}

	for id, codes := range operationCodes {
		so, ok := ops[id]
		if !ok {
			t.Errorf("handler table operation %q is not in the spec", id)
			continue
		}

		wantByStatus := map[int][]Code{}
		for _, c := range codes {
			status := c.Status()
			if status == 0 {
				t.Errorf("%s: code %q has no frozen status", id, c)
				continue
			}
			wantByStatus[status] = append(wantByStatus[status], c)
		}

		gotStatuses := map[int]bool{}
		for status, raw := range mapAt(t, so.op, "responses") {
			code, err := strconv.Atoi(status)
			if err != nil {
				t.Fatalf("%s: response key %q is not a status code", id, status)
			}
			if code >= 200 && code < 300 {
				continue
			}
			gotStatuses[code] = true

			node, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("%s %d: response is %T, want a mapping", id, code, raw)
			}
			resp := resolveRef(t, doc, node)
			var documented []string
			for _, c := range toStrings(t, resp["x-bd-codes"]) {
				documented = append(documented, c)
				if Code(c).Status() == 0 {
					t.Errorf("%s %d: code %q is not in the frozen vocabulary", id, code, c)
					continue
				}
				if got := Code(c).Status(); got != code {
					t.Errorf("%s: code %q is documented under %d but is frozen to %d", id, c, code, got)
				}
			}
			sort.Strings(documented)
			if want := sortedCodes(wantByStatus[code]); !equalStrings(documented, want) {
				t.Errorf("%s %d: spec documents codes %v, the handler table can emit %v", id, code, documented, want)
			}
		}

		for status := range wantByStatus {
			if !gotStatuses[status] {
				t.Errorf("%s: handler table can emit %d, the spec does not document it", id, status)
			}
		}
		for status := range gotStatuses {
			if _, ok := wantByStatus[status]; !ok {
				t.Errorf("%s: spec documents %d, no mapping row can produce it for this operation", id, status)
			}
		}
	}
}

// TestDefaultsMatchCLIFlags guards every default this document repeats from a
// cobra flag registration. A client swapping a `bd` subprocess for an HTTP call
// gets the same answer only if the two surfaces default the same way, and a
// default is the one piece of the contract nobody passes explicitly — so a
// divergence is invisible until the result sets differ.
//
// The limits are the interim duplication described in defaults.go: until the
// shared constants move into internal/workapi, the flag registration is the
// other copy of those two numbers. `sort` has no constant to share at all; the
// flag string IS the source of truth.
//
// If a flag registration is reworded this fails loudly — re-point the regex,
// and check the values still agree while you are there.
func TestDefaultsMatchCLIFlags(t *testing.T) {
	limitFlag := regexp.MustCompile(`IntP\("limit",\s*"n",\s*(-?\d+)`)

	for _, tc := range []struct {
		file string
		want int
	}{
		{"../../cmd/bd/list.go", DefaultListLimit},
		{"../../cmd/bd/ready.go", DefaultReadyLimit},
	} {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		m := limitFlag.FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s: no --limit flag registration found; re-point this guard at the CLI's default", tc.file)
		}
		got, err := strconv.Atoi(string(m[1]))
		if err != nil {
			t.Fatalf("%s: limit default %q is not a number", tc.file, m[1])
		}
		if got != tc.want {
			t.Errorf("%s registers --limit default %d, the shared constant is %d", tc.file, got, tc.want)
		}
	}

	// The ready sort policy. Getting this wrong changes the item SET, not just
	// the order, as soon as the limit truncates: `hybrid` demotes older
	// high-priority work that `priority` surfaces first.
	//
	// Note for anyone tempted to "correct" the spec back to hybrid: the
	// storage layer maps an EMPTY policy to hybrid
	// (internal/storage/sqlbuild/ready.go), but the CLI never sends empty —
	// the flag registers a concrete default — so that fallback is not `bd
	// ready`'s behavior and must not be this parameter's default. A handler
	// that forwards an absent `sort` as "" reintroduces the divergence with
	// the spec still saying the right thing.
	sortFlag := regexp.MustCompile(`StringP\("sort",\s*"s",\s*"([a-z]+)"`)
	src, err := os.ReadFile("../../cmd/bd/ready.go")
	if err != nil {
		t.Fatalf("read cmd/bd/ready.go: %v", err)
	}
	m := sortFlag.FindSubmatch(src)
	if m == nil {
		t.Fatalf("cmd/bd/ready.go: no --sort flag registration found; re-point this guard at the CLI's default")
	}
	cliSort := string(m[1])

	doc := loadSpec(t)
	so, ok := specOps(t, doc)[OpListReadyWork]
	if !ok {
		t.Fatalf("operation %q missing from the spec", OpListReadyWork)
	}
	schema := mapAt(t, specParam(t, so, "sort"), "schema")
	specSort, _ := schema["default"].(string)
	if specSort != cliSort {
		t.Errorf("spec documents sort default %q, `bd ready --sort` registers %q", specSort, cliSort)
	}
	if !slices.Contains(toStrings(t, schema["enum"]), specSort) {
		t.Errorf("sort default %q is not in the documented enum %v", specSort, schema["enum"])
	}
}

func toStrings(t *testing.T, v any) []string {
	t.Helper()
	if v == nil {
		return nil
	}
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("value is %T, want a sequence", v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("sequence item is %T, want a string", item)
		}
		out = append(out, s)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
