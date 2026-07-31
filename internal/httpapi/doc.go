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
// The Host allowlist is the DNS-rebinding defense, and it has no off switch.
// Every bind answers to the loopback spellings and to the bound address itself;
// a WILDCARD bind (0.0.0.0, ::) has no single configured address, so it answers
// to any numeric IP literal and still refuses foreign DNS names. That last rule
// is the one place this deviates from the design's letter, which enumerated
// "the configured bind address" without saying what a wildcard means: a rebound
// page cannot produce an IP-literal Host, because the browser sends the hostname
// from the attacker's URL, so allowing literals keeps --allow-non-loopback
// usable on every interface while the defense survives — including on the
// serving host's own loopback, which is rebinding's canonical target. Matching
// is on parsed addresses, so every spelling of an allowed address is allowed.
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
