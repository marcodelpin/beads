// Package httpapi implements the HTTP surface described by
// internal/httpapi/spec/openapi.v0.yaml — the /v0 wire contract `bd serve`
// answers on.
//
// What is live: the process lifecycle (Listen and Serve), the bind policy, the
// route table and the request path in front of it — Host allowlist, connection
// cap, database semaphore, per-request deadline, structured request log — plus
// the two operations that answer from the process itself, GET /healthz and
// GET /v0/beads/context. The read and claim operations are registered as
// transitional 501 stubs so the route table can be checked against the document
// all at once; ContextResponse.capabilities is derived from the implemented
// handlers only, so it never advertises one of them.
//
// Around that sit the inert pieces the contract needs regardless: the
// problem+json error mapping, the shared limit defaults, and the compile-time
// pins that keep the generated types welded to the canonical structs.
//
// Two rules govern everything added here:
//
// The spec is the source of truth. Types come from it (`make api-gen`), never
// the other way round, and `make api-check` fails a change that edits one
// without the other.
//
// There is no wire struct. Responses marshal internal/types values directly,
// so the CLI's --json output and these bodies are one compatibility domain —
// which also means a serialized field on types.Issue cannot be renamed or
// removed without breaking the HTTP contract, not just the CLI's.
package httpapi
