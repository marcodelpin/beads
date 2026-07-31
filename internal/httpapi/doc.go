// Package httpapi implements the HTTP surface described by
// internal/httpapi/spec/openapi.v0.yaml — the /v0 wire contract `bd serve`
// answers on.
//
// The contract lands before the server does. What lives here today is the
// inert half: the problem+json error mapping, the shared limit defaults, and
// the compile-time pins that keep the generated types welded to the canonical
// structs. Nothing in this package is routed or reachable yet.
//
// Two rules govern everything added here later:
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
