// Package httpapi implements the HTTP surface described by
// internal/httpapi/spec/openapi.v0.yaml — the /v0 wire contract `bd serve`
// answers on.
//
// What is live: the process lifecycle (Listen and Serve), the bind policy, the
// route table and the request path in front of it — Host allowlist, connection
// cap, database semaphore, per-request deadline, structured request log — the
// two operations that answer from the process itself, GET /healthz and
// GET /v0/beads/context, the three reads — GET /v0/beads/ready,
// GET /v0/beads/issues and GET /v0/beads/issues/{id} — and the one write,
// POST /v0/beads/issues/{id}:claim. ContextResponse.capabilities is derived
// from the implemented handlers, so a release cut mid-slice never advertises
// an operation that does not work.
//
// The reads hold no query logic of their own. Each decodes its parameters and
// hands the whole request to issueops.Reader, obtained from the provider's own
// capability accessor — the same role, reached the same way, that `bd show
// --json` reaches on a store. Filter construction, the workspace config it
// depends on, the default limits and the wisp fallback all live inside that
// role. A handler CANNOT build a filter here, and that is machine-checked by
// two rules in .golangci.yml that cover the two ways it could be done:
// httpapi-transport-boundary (depguard) denies internal/workapi from this
// package's non-test files, so the builders are not callable; the forbidigo
// rule denies naming types.IssueFilter or types.WorkFilter here at all, so a
// hand-rolled filter is not writable either. The second is the one that
// matches the failure — the hosted viewer's bug was a bare IssueFilter, not a
// misused builder. What does stay here is transport — parameter decoding, the
// opaque cursor codec, the loopback-only refusal of an unlimited read, and the
// wire envelopes.
//
// The other front door is only PARTLY on the role. `bd ready` and `bd list`
// still build filters from the same request types through the same builders,
// because they consume the filter for modes the role does not express; see
// issueops.Reader's doc comment for the list and for why splitting those
// commands would be worse. So the accurate statement of the anti-drift
// property is: both surfaces answer from one library, and this surface can
// reach it only through the role.
//
// The claim is the only mutation this surface has, and claim.go states the two
// things a client must know before adopting it: the actor is caller-asserted
// provenance rather than authenticated identity, and hooks do not fire.
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
