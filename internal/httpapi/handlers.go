package httpapi

import (
	"maps"
	"net/http"
	"slices"

	"github.com/steveyegge/beads/internal/httpapi/apigen"
	"github.com/steveyegge/beads/internal/storage/domain"
)

// handleHealth answers from the process and touches nothing that can fail.
//
// This is LIVENESS, not readiness: it stays 200 while the database is
// unreachable, wedged, or an hour dead. The documented readiness probe is
// GET /v0/beads/ready?limit=1, which goes through the semaphore and a real
// transaction.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.requireNoQuery(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, apigen.Health{Status: apigen.Ok})
}

// handleContext answers the identity handshake from the startup snapshot.
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	if !s.requireNoQuery(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.ctxBody)
}

// handleNotImplemented is the transitional stub for an operation whose handler
// has not landed. It is NOT wire surface: 501 appears nowhere in the document,
// and `not_implemented` is deliberately absent from the frozen code vocabulary
// in problem.go so it cannot leak into the spec parity check.
//
// The stubs exist so route/spec parity is provable now rather than accumulating
// a per-slice exemption. They are enumerated in the route table (implemented:
// false) and in the exemption list that
// TestSpecStatusCodesMatchHandlerTable carries; when the last one becomes a
// real handler, that list must be deleted and the test fails if it is not.
func (s *Server) handleNotImplemented(w http.ResponseWriter, r *http.Request) {
	rec := requestInfo(r.Context())
	rec.code = Code("not_implemented")
	detail := "this operation is not implemented by this build; check `capabilities` in GET /v0/beads/context before calling"
	Write(w, Result{Problem: apigen.Problem{
		Status: http.StatusNotImplemented,
		Title:  http.StatusText(http.StatusNotImplemented),
		Code:   string(rec.code),
		Detail: &detail,
	}}.WithRequestID(rec.id))
}

// requireNoQuery enforces the document's unknown-parameter rule for the
// operations that take no parameters at all. It reports whether the request may
// proceed.
//
// Silently ignoring an unrecognized parameter is the failure this exists to
// prevent: on a filtering operation it WIDENS the result set, so a client one
// version ahead receives rows it believes it filtered out. Rejecting is also a
// client's only per-parameter capability probe, since `capabilities` is
// operation-level.
func (s *Server) requireNoQuery(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if len(q) == 0 {
		return true
	}
	// Name one key, and always the same one for the same request: `param` is
	// what the client dispatches on, so it must not depend on map order.
	offender := slices.Min(slices.Collect(maps.Keys(q)))
	res := InvalidArgument(offender, ReasonUnknownParameter,
		"this operation takes no query parameters")
	if offender == "" {
		// The degenerate `?=1` spelling parses to a parameter whose name is the
		// empty string. InvalidArgument reads "" as "this input has no nameable
		// part" and omits `param` — true of an unparseable body, not of this —
		// and the document promises `param` on every other 400. Name it.
		res.Problem.Param = &offender
	}
	requestInfo(r.Context()).refuse(offender)
	s.fail(w, r, res)
	return false
}

// contextResponse projects the workspace snapshot onto the wire.
//
// This is an ALLOWLIST, written out field by field, and that is the whole
// point: the source struct is the server's own configuration, which is exactly
// the kind of struct that grows a member nobody meant to publish. Marshaling
// it directly — or copying it field-for-field "to keep them in sync" — would
// turn every future addition into a silent disclosure.
//
// Three exclusions are load-bearing:
//
// SyncRemote, above all. It is populated unconditionally from the workspace's
// sync.remote config, and remote URLs routinely embed credentials
// (https://x-access-token:TOKEN@host/...). It is excluded in this and every
// future version.
//
// ServerHost/ServerPort, the database bind endpoint: advertising it invites
// clients to bypass this API and dial the database directly, on a server whose
// trust model is "root with an empty password on loopback".
//
// DataDir, ProxiedDir and CWDRepoRoot, absolute host paths that identify
// nothing a client needs. BeadsDir and RepoRoot ARE published, deliberately:
// they are the single-workspace server's only workspace-identity handshake, and
// the document says so.
func contextResponse(info domain.ContextInfo, schemaVersion int, capabilities []string) apigen.ContextResponse {
	if capabilities == nil {
		// The document types this as an array; a client must never have to
		// tell null from empty to learn that nothing is implemented.
		capabilities = []string{}
	}
	return apigen.ContextResponse{
		ApiVersion:    APIVersion,
		BdVersion:     info.BdVersion,
		SchemaVersion: schemaVersion,
		Backend:       info.Backend,
		DoltMode:      info.DoltMode,
		Database:      info.Database,
		BeadsDir:      info.BeadsDir,
		RepoRoot:      info.RepoRoot,
		ProjectId:     info.ProjectID,
		Capabilities:  capabilities,
	}
}
