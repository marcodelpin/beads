package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/netutil"

	"github.com/steveyegge/beads/internal/httpapi/apigen"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
)

// APIVersion is the path major this package serves, reported as
// ContextResponse.api_version. It changes only when /v1 is cut.
const APIVersion = "v0"

// The operating envelope. Every one of these is a bound on how much of the
// process a client can occupy, and the two that matter most are the ones with
// no natural limit: without semAcquireTimeout a queue behind a wedged database
// grows without end, and without requestDeadline a request that got a slot
// never gives it back. Both are deliberately generous — this is a loopback
// service for automation clients, not a public endpoint — and all of them can
// become operator flags later without touching the wire.
const (
	// maxInflight bounds handlers that touch the database. Every unit of work
	// pins one SQL connection, so this is also the steady-state connection
	// count.
	maxInflight = 16
	// maxConns bounds ACCEPTED connections. The semaphore does not: Go spawns
	// a goroutine per connection, and one parked on a full semaphore still
	// holds its goroutine, fd and buffers. Excess connections wait in the
	// kernel accept backlog instead of in Go memory.
	maxConns = 64
	// semAcquireTimeout bounds the queue in TIME as well as width. A timed-out
	// acquisition is the already-documented 503 busy, so shedding load
	// introduces no new status vocabulary.
	semAcquireTimeout = 10 * time.Second
	// requestDeadline is the whole-request backstop, needed because
	// WriteTimeout is 0 (below). It covers semaphore wait + unit of work +
	// query, and deliberately not the response write.
	requestDeadline = 60 * time.Second
	// saturationWarn is how long a semaphore wait has to last before it is
	// worth a log line of its own. This is the wedge-detection signal: /healthz
	// stays green while the database is hung, so saturation events are what
	// distinguish "wedged" from "no traffic".
	saturationWarn = time.Second
	// drainTimeout covers a claim inside its serialization-retry budget plus
	// the commit, so a graceful shutdown does not kill a connection whose write
	// may already have landed.
	drainTimeout = 20 * time.Second
	// uowCloseTimeout bounds the DETACHED close described on WithUOW.
	uowCloseTimeout = 5 * time.Second
)

// Pool limits for the provider's *sql.DB. The semaphore bounds handlers, not
// connections: a poisoned connection replaced after a failed ROLLBACK, each
// retry attempt of a committing transaction (a fresh unit of work is a fresh
// pinned connection), and any semaphore-exempt handler that later touches the
// database all escape it.
var servePoolLimits = uow.PoolLimits{
	MaxOpenConns:    maxInflight + 4,
	MaxIdleConns:    maxInflight,
	ConnMaxIdleTime: 5 * time.Minute,
	ConnMaxLifetime: time.Hour,
}

// HTTP-level timeouts. WriteTimeout is deliberately absent: `limit=0` means
// unlimited on both list operations, and a large body must not be truncated
// mid-write. Slowloris exposure is covered by the header and idle timeouts.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
)

// Config is everything the server needs to answer. It is assembled by the
// caller — the package resolves no workspace state of its own.
type Config struct {
	// Addr is the host:port to bind. The host must be a numeric IP literal;
	// see ValidateBindAddr.
	Addr string
	// AllowNonLoopback permits a bind beyond loopback. v0 has no
	// authentication and no TLS, so this is an operator decision that is never
	// taken by default.
	AllowNonLoopback bool
	// Provider is where every database-touching handler opens its one unit of
	// work per request.
	Provider uow.UnitOfWorkProvider
	// Workspace is the startup snapshot GET /v0/beads/context answers from.
	// Only the allowlisted fields are ever serialized — see contextResponse,
	// which names the whole set and the reasons for the exclusions.
	Workspace domain.ContextInfo
	// SchemaVersion is the CLI's stdout JSON envelope version, reported for
	// diagnostics. Clients are told not to branch on it.
	SchemaVersion int
	// Mode names the resolved storage topology ("proxied", "external") for the
	// startup log line. Cosmetic: nothing dispatches on it.
	Mode string
	// Stdout receives exactly one line, the bound address, so a caller that
	// asked for an ephemeral port can discover it. Stderr receives the
	// operational log. Both default to the process streams.
	Stdout io.Writer
	Stderr io.Writer
}

// Server is one bound listener and the routes behind it. Build it with Listen,
// which binds before returning so the caller can read Addr, then run Serve.
type Server struct {
	cfg      Config
	provider uow.UnitOfWorkProvider

	listener net.Listener
	http     *http.Server

	// sem bounds handlers that touch the database. Buffered channel rather
	// than sync.Semaphore so the acquisition can select on a timer.
	sem chan struct{}
	// semTimeout and semWarn default to the constants above. They are fields
	// rather than constants at the point of use so the queueing behavior can be
	// exercised in milliseconds instead of tens of seconds.
	semTimeout time.Duration
	semWarn    time.Duration

	log     *log.Logger
	stdout  io.Writer
	ctxBody apigen.ContextResponse

	// hosts is the Host-header allowlist, the DNS-rebinding defense. It is
	// derived from the bind address and there is no configuration that turns it
	// off; see newHostPolicy.
	hosts hostPolicy

	idPrefix  string
	idSeq     atomic.Uint64
	liveConns atomic.Int64
}

// ValidateBindAddr enforces the bind posture, following the policy the managed
// Dolt child already lives under (validateManagedServerConfigPolicy in
// cmd/bd/proxied_server.go): the host must be a NUMERIC IP literal.
//
// Hostnames are refused, "localhost" included. A name is not a listener
// specification — it resolves to whatever the host's resolver says today, so
// the operator cannot tell from the flag which interfaces they just opened.
// Unix sockets are not supported at all; they fail here because they do not
// parse as host:port.
func ValidateBindAddr(addr string, allowNonLoopback bool) (net.IP, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("--addr %q must be HOST:PORT with a numeric IP literal host (unix sockets are not supported): %w", addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return nil, fmt.Errorf("--addr %q: port must be a number from 0 to 65535 (0 picks an ephemeral port)", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("--addr %q: host must be a numeric IP literal, not a name — use 127.0.0.1 rather than localhost", addr)
	}
	if !ip.IsLoopback() && !allowNonLoopback {
		return nil, fmt.Errorf("--addr %q binds beyond loopback; bd serve has no authentication, so this requires --allow-non-loopback", addr)
	}
	return ip, nil
}

// Listen validates the configuration, binds the listener, and reports the
// bound address on stdout and the startup state on stderr. It does not accept
// anything until Serve runs.
//
// There is no lock file, pid file or discovery file: bd serve is
// operator-invoked and the TCP bind IS the mutual exclusion, so a second
// instance on the same fixed port fails here with the operating system's own
// address-in-use error. (Under the ephemeral default that exclusion does not
// exist — N instances simply run on N ports — which is why fixed ports are the
// deployment recommendation.)
func Listen(cfg Config) (*Server, error) {
	if cfg.Provider == nil {
		return nil, errors.New("httpapi: no unit-of-work provider")
	}
	ip, err := ValidateBindAddr(cfg.Addr, cfg.AllowNonLoopback)
	if err != nil {
		return nil, err
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}

	prefix, err := newIDPrefix()
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:        cfg,
		provider:   cfg.Provider,
		sem:        make(chan struct{}, maxInflight),
		semTimeout: semAcquireTimeout,
		semWarn:    saturationWarn,

		log:      log.New(cfg.Stderr, "bd serve: ", log.LstdFlags|log.LUTC),
		stdout:   cfg.Stdout,
		ctxBody:  contextResponse(cfg.Workspace, cfg.SchemaVersion, Capabilities()),
		hosts:    newHostPolicy(ip),
		idPrefix: prefix,
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w", cfg.Addr, err)
	}
	s.listener = netutil.LimitListener(ln, maxConns)

	s.http = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(cfg.Stderr, "bd serve: http: ", log.LstdFlags|log.LUTC),
		ConnState:         s.connState,
	}

	// Bound what a burst of requests can open on the database. The knob is
	// optional on the interface, so say so out loud when a provider does not
	// carry it rather than silently running unbounded.
	if tuner, ok := cfg.Provider.(uow.PoolTuner); ok {
		tuner.SetPoolLimits(servePoolLimits)
	} else {
		s.event("pool_limits_unavailable", "provider", fmt.Sprintf("%T", cfg.Provider))
	}

	fmt.Fprintf(s.stdout, "bd serve: listening on http://%s\n", s.Addr())
	s.logStartup()
	return s, nil
}

// Addr is the bound address, which is the only way to discover the port under
// the ephemeral default.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// Serve accepts requests until ctx is canceled, then drains. It returns nil
// on a clean shutdown; a listener failure is returned as-is.
//
// The drain budget covers a committing request that is mid-retry, because
// Shutdown does not cancel in-flight handler contexts: killing such a
// connection early would leave the client unable to tell whether its write
// landed.
func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.http.Serve(s.listener) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	s.event("shutdown_start", "drain_timeout", drainTimeout.String(), "conns", s.liveConns.Load())

	// Detached: ctx is already canceled, and the drain is the point.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()

	if err := s.http.Shutdown(drainCtx); err != nil {
		killed := s.liveConns.Load()
		_ = s.http.Close()
		s.event("shutdown_forced", "conns_killed", killed, "reason", err.Error())
	} else {
		s.event("shutdown_complete")
	}
	<-errCh
	return nil
}

func (s *Server) connState(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		s.liveConns.Add(1)
	case http.StateHijacked, http.StateClosed:
		s.liveConns.Add(-1)
	}
}

// WithUOW runs fn inside one unit of work and guarantees the rollback.
//
// The close context is DETACHED on purpose. Close sends ROLLBACK on the pinned
// connection, and the transaction layer POISONS that connection if the send
// fails (internal/storage/uow/doltserver_tx.go) — go-sql-driver's session reset
// does not clear an open transaction, so a session that may still be in one
// must never go back to the pool. Correctness is therefore safe either way, but
// closing with the request's own canceled context would fail the ROLLBACK
// immediately and burn one pinned session on every client disconnect. Reads
// never commit.
func (s *Server) WithUOW(ctx context.Context, rec *reqInfo, fn func(uow.UnitOfWork) error) error {
	start := time.Now()
	uw, err := s.provider.NewUOW(ctx)
	if rec != nil {
		rec.uowWait = time.Since(start)
	}
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uowCloseTimeout)
		defer cancel()
		uw.Close(closeCtx)
	}()
	return fn(uw)
}

// acquire takes a database slot, or gives up. A timed-out wait is ErrBusy, not
// a request parked for the full deadline and then answered with a
// non-retryable 500.
func (s *Server) acquire(ctx context.Context, rec *reqInfo) (release func(), err error) {
	start := time.Now()
	release = func() { <-s.sem }

	select {
	case s.sem <- struct{}{}:
		rec.semWait = time.Since(start)
		return release, nil
	default:
	}

	timer := time.NewTimer(orDefault(s.semTimeout, semAcquireTimeout))
	defer timer.Stop()
	select {
	case s.sem <- struct{}{}:
		rec.semWait = time.Since(start)
		s.noteSaturation(rec, "acquired")
		return release, nil
	case <-timer.C:
		rec.semWait = time.Since(start)
		s.event("semaphore_timeout", "request_id", rec.id, "wait_ms", millis(rec.semWait), "inflight", maxInflight)
		return nil, ErrBusy
	case <-ctx.Done():
		// The client hung up, or the request deadline expired, while queued.
		// Still a saturation datapoint: it is the same wedge, observed from a
		// request that did not live long enough to be shed.
		rec.semWait = time.Since(start)
		s.noteSaturation(rec, "abandoned")
		return nil, ctx.Err()
	}
}

// noteSaturation logs a wait that lasted long enough to matter. This is the
// signal that separates "wedged" from "no traffic" at 3 a.m., because /healthz
// stays green either way.
func (s *Server) noteSaturation(rec *reqInfo, outcome string) {
	if rec.semWait < orDefault(s.semWarn, saturationWarn) {
		return
	}
	s.event("semaphore_saturated",
		"request_id", rec.id, "wait_ms", millis(rec.semWait),
		"inflight", maxInflight, "outcome", outcome)
}

func orDefault(v, fallback time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return fallback
}

// handler builds the whole request path: the route table's registrations, the
// catch-all that keeps unrouted paths on the same error shape, and the
// middleware in front of both.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range routeTable {
		mux.Handle(rt.method+" "+rt.pattern, s.route(rt))
	}

	// Not an operation and deliberately not in the route table: it exists so
	// that an unrouted path still answers with problem+json rather than
	// net/http's text/plain default, which the document promises for EVERY
	// non-2xx byte. A method mismatch on a known path lands here too and
	// answers 404 rather than 405, because 405 is not in the v0 vocabulary.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, newResult(CodeNotFound, "no such route on this server"))
	}))

	return s.withRequestContext(s.checkHost(mux))
}

// reqInfo is the per-request record the log line is assembled from. Layers fill
// in what they know; the outermost middleware writes it.
type reqInfo struct {
	id      string
	op      string
	status  int
	code    Code
	semWait time.Duration
	uowWait time.Duration
}

type reqInfoKey struct{}

// requestInfo returns the record for the request in flight. It never returns
// nil: every request goes through withRequestContext, and handing back a
// detached record rather than nil means a mis-wired caller loses a log line
// instead of panicking mid-response.
func requestInfo(ctx context.Context) *reqInfo {
	if rec, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		return rec
	}
	return &reqInfo{}
}

// withRequestContext assigns the correlation id, applies response-wide
// headers, and writes the one log line per request. It is outermost so that a
// request refused by the Host check is logged like any other.
func (s *Server) withRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &reqInfo{id: s.nextID(), status: http.StatusOK}
		ctx := context.WithValue(r.Context(), reqInfoKey{}, rec)

		// No client or intermediary may cache an answer about live work.
		w.Header().Set("Cache-Control", "no-store")

		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r.WithContext(ctx))
		if sw.status != 0 {
			// Still zero means the handler returned without writing anything,
			// in which case net/http has sent the 200 rec already carries.
			rec.status = sw.status
		}

		s.event("request",
			"request_id", rec.id,
			"op", rec.op,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"code", string(rec.code),
			"duration_ms", millis(time.Since(start)),
			"sem_wait_ms", millis(rec.semWait),
			"uow_ms", millis(rec.uowWait),
		)
	})
}

// checkHost is the DNS-rebinding defense. An unauthenticated service on
// loopback is reachable from any browser on the host; a page that re-resolves
// its own name to 127.0.0.1 issues requests the browser treats as same-origin,
// so no CORS rule stops them. What the browser does preserve is the attacker's
// hostname in Host, which is what this rejects.
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hosts.allows(r.Host) {
			s.fail(w, r, InvalidArgument("Host", ReasonInvalidValue,
				"Host header is not one this server answers to"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// route wraps one operation with the limits that apply to it: the per-request
// deadline, and — unless the operation is exempt — a database slot.
func (s *Server) route(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := requestInfo(r.Context())
		rec.op = rt.op

		ctx, cancel := context.WithTimeout(r.Context(), requestDeadline)
		defer cancel()
		r = r.WithContext(ctx)

		if !rt.bypassSemaphore {
			release, err := s.acquire(ctx, rec)
			if err != nil {
				s.failErr(w, r, err)
				return
			}
			defer release()
		}

		rt.handler(s, w, r)
	})
}

// fail writes a problem response and records what it was for the log line.
// Every non-2xx byte this server emits goes through here or through
// handleNotImplemented.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, res Result) {
	rec := requestInfo(r.Context())
	rec.code = Code(res.Problem.Code)
	Write(w, res.WithRequestID(rec.id))
}

// failErr maps an error from the storage seam and logs the error text. On a 5xx
// that text goes to the log and NOWHERE else: driver and dial errors routinely
// embed the DSN, and the response detail is a fixed string per code. The
// request_id in both places is what reconnects them.
func (s *Server) failErr(w http.ResponseWriter, r *http.Request, err error) {
	res := ClassifyError(err)
	if res.Problem.Status >= 500 {
		s.event("request_error", "request_id", requestInfo(r.Context()).id, "error", err.Error())
	}
	s.fail(w, r, res)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// statusWriter records the status for the log line. It intentionally does not
// buffer the body: an unlimited read must stream.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap keeps http.ResponseController working through the wrapper, so a
// handler that needs to flush a large streamed page still can.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// hostPolicy is the set of Host header values this server answers to. It is
// data rather than a closure so the startup line can state the whole policy,
// and so the wildcard case below is a visible rule instead of an absence.
type hostPolicy struct {
	// ips are the numeric addresses allowed, matched with net.IP.Equal so every
	// spelling of one address matches: [0:0:0:0:0:0:0:1] and [::ffff:127.0.0.1]
	// are the same hosts as ::1 and 127.0.0.1, and a client that spells one of
	// them the long way is not an attacker.
	ips []net.IP
	// names are the allowed non-numeric Host values, lowercased. There is
	// exactly one, "localhost", and no mechanism to add another: a DNS name in
	// a Host header is precisely what the rebinding attack carries.
	names map[string]bool
	// anyIP additionally allows ANY numeric Host literal. Only a wildcard bind
	// sets it; see newHostPolicy for why that is still a rebinding defense.
	anyIP bool
}

// newHostPolicy returns the Host policy implied by a bind address.
//
// The loopback spellings are always allowed, and the bind's own address is too
// — including an alternate loopback bind like 127.0.0.2, whose clients dial
// exactly that address and would otherwise be refused by the defense meant to
// protect them.
//
// A WILDCARD bind (0.0.0.0, ::) has no single configured address to allow, so
// it allows any numeric IP literal instead — and still refuses foreign DNS
// names, which is the whole defense. A rebound page cannot produce an IP-literal
// Host: the browser sends the hostname from the attacker's URL, and fetching an
// IP URL directly is a direct connection, which is the exposure the operator
// accepted when they passed --allow-non-loopback. Disabling the check outright
// would instead surrender the defense on the serving host's own loopback
// interface, which is rebinding's canonical target, and on every LAN browser
// behind a firewall the attacker cannot otherwise reach.
func newHostPolicy(bind net.IP) hostPolicy {
	p := hostPolicy{
		ips:   []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		names: map[string]bool{"localhost": true},
		anyIP: bind.IsUnspecified(),
	}
	if !p.anyIP && !containsIP(p.ips, bind) {
		p.ips = append(p.ips, bind)
	}
	return p
}

// allows reports whether a Host header value is one this server answers to.
func (p hostPolicy) allows(host string) bool {
	h := hostOnly(host)
	if p.names[h] {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return p.anyIP || containsIP(p.ips, ip)
}

// label renders the policy for the startup line, so an operator can read what
// this server will answer to without deducing it from the bind address.
func (p hostPolicy) label() string {
	parts := make([]string, 0, len(p.ips)+len(p.names)+1)
	for _, ip := range p.ips {
		parts = append(parts, ip.String())
	}
	parts = append(parts, slices.Sorted(maps.Keys(p.names))...)
	if p.anyIP {
		parts = append(parts, "any-ip-literal (wildcard bind)")
	}
	return strings.Join(parts, ",")
}

func containsIP(ips []net.IP, want net.IP) bool {
	return slices.ContainsFunc(ips, want.Equal)
}

// hostOnly strips the port and any IPv6 brackets from a Host header value.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return strings.ToLower(host)
}

// newIDPrefix draws one random prefix per process so ids from two servers, or
// from two runs, never collide in a shared log.
func newIDPrefix() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("httpapi: request id seed: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Server) nextID() string {
	return fmt.Sprintf("%s-%06d", s.idPrefix, s.idSeq.Add(1))
}

func (s *Server) logStartup() {
	s.event("startup",
		"addr", s.Addr(),
		"mode", s.cfg.Mode,
		"workspace", s.cfg.Workspace.RepoRoot,
		"beads_dir", s.cfg.Workspace.BeadsDir,
		"database", s.cfg.Workspace.Database,
		"host_allowlist", s.hosts.label(),
		"capabilities", strings.Join(s.ctxBody.Capabilities, ","),
	)
	s.event("limits",
		"max_inflight", maxInflight,
		"max_conns", maxConns,
		"sem_wait", semAcquireTimeout.String(),
		"deadline", requestDeadline.String(),
		"pool_max_open", servePoolLimits.MaxOpenConns,
		"pool_max_idle", servePoolLimits.MaxIdleConns,
		"pool_idle_time", servePoolLimits.ConnMaxIdleTime.String(),
		"pool_lifetime", servePoolLimits.ConnMaxLifetime.String(),
	)
}

// event writes one structured stderr line. Values are quoted when they are not
// bare tokens, so a path or an error message can never inject a field — or a
// whole line — into the log.
func (s *Server) event(name string, kv ...any) {
	var b strings.Builder
	b.WriteString("event=")
	b.WriteString(name)
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		b.WriteByte(' ')
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(logValue(kv[i+1]))
	}
	s.log.Print(b.String())
}

func logValue(v any) string {
	str, ok := v.(string)
	if !ok {
		return fmt.Sprint(v)
	}
	if str == "" {
		return `""`
	}
	if strings.ContainsFunc(str, func(r rune) bool {
		return r <= ' ' || r == '"' || r == '=' || r == 0x7f
	}) {
		return strconv.Quote(str)
	}
	return str
}

func millis(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 3, 64)
}
