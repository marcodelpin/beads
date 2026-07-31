package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/httpapi"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/contextinfo"
	"github.com/steveyegge/beads/internal/storage/domain"
)

var (
	serveAddr             string
	serveAllowNonLoopback bool
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the beads HTTP API over loopback",
	Long: `Serve the beads HTTP API — the same work surface the CLI answers, for
automation clients that would otherwise fork a bd subprocess per call.

The wire contract is described by an OpenAPI document (/v0); GET
/v0/beads/context reports which operations this build actually implements.

DEPLOYMENT

  Pass an explicit port. The default 127.0.0.1:0 takes an ephemeral one, which
  is right for ad-hoc and test use — where the bound address printed on stdout
  is read immediately — but carries no mutual exclusion: two serves against one
  workspace then run side by side on different ports with no way to enumerate
  them. On a fixed port the second one fails to bind, which is the intended
  behavior. Concurrent serves are data-safe either way; claims are arbitrated
  in the SQL server.

  Run it under a supervisor. bd shuts down gracefully on SIGHUP as well as
  SIGINT and SIGTERM, so closing the terminal of a foreground bd serve stops it.

PROBES

  GET /healthz is LIVENESS only: it answers from the process and never touches
  the database, so it stays green while the database is unreachable. For
  readiness use GET /v0/beads/ready?limit=1 — a real query, where 200 means
  ready and 503 means live but not ready.

  This build does not implement that endpoint yet: it answers 501, and
  ready.list is absent from the capabilities list in GET /v0/beads/context.
  Wire a readiness probe to it once that capability appears — until then a
  probe configured this way never reports ready, which is the safe direction
  but is not a readiness signal.

WHAT THIS DOES NOT DO

  No authentication and no TLS. The trust model is the loopback boundary, which
  is the same one the database behind it already relies on. --allow-non-loopback
  extends the surface to every network peer that can reach the address; nothing
  else about the server changes.

  Hooks do not fire. A hook is a user-controlled subprocess per mutation: in a
  concurrent server that is an unbounded latency multiplier and an orphaned
  child at shutdown, and its working-directory-derived hook lookup is
  meaningless in a server process. A CLI claim runs on_update; an HTTP claim
  does not.

  The per-command auto-commit machinery does not run. Durability is per request:
  a successful claim commits inside its own transaction, exactly as a proxied
  CLI claim does today.

  An actor on an HTTP request is caller-asserted provenance for the audit trail,
  not authenticated identity — the same thing it has always been on the CLI,
  where any local process can pass any --actor.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServe()
	},
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", "127.0.0.1:0",
		"Address to bind as IP:PORT; the host must be a numeric IP literal, and port 0 takes an ephemeral port")
	serveCmd.Flags().BoolVar(&serveAllowNonLoopback, "allow-non-loopback", false,
		"Permit a bind beyond loopback. bd serve has no authentication: every peer that can reach the address gets full read and claim access")
	rootCmd.AddCommand(serveCmd)
}

func runServe() error {
	// Flag validation first: it depends on nothing about the workspace, so the
	// refusal for a bad --addr is the same in every mode.
	if _, err := httpapi.ValidateBindAddr(serveAddr, serveAllowNonLoopback); err != nil {
		return HandleError("%v", err)
	}
	if err := serveModeGate(); err != nil {
		return HandleError("%v", err)
	}
	if uowProvider == nil {
		// Unreachable: proxied mode builds the provider in PersistentPreRunE,
		// and every other mode is refused above.
		return HandleError("bd serve: this workspace has no unit-of-work provider")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return HandleError("cannot resolve working directory: %v", err)
	}
	info, err := contextinfo.NewContextProvider(cwd, Version).ContextUseCase().GetContextInfo(rootCtx)
	if err != nil {
		return HandleError("cannot resolve workspace context: %v", err)
	}

	if serveAllowNonLoopback {
		fmt.Fprintf(os.Stderr,
			"bd serve: WARNING: --allow-non-loopback binds %s beyond loopback. "+
				"This API has no authentication and no TLS: any peer that can reach it can read every issue and claim work as any actor.\n",
			serveAddr)
	}

	srv, err := httpapi.Listen(httpapi.Config{
		Addr:             serveAddr,
		AllowNonLoopback: serveAllowNonLoopback,
		Provider:         uowProvider,
		Workspace:        info,
		SchemaVersion:    JSONSchemaVersion,
		Mode:             serveResolvedMode(info),
	})
	if err != nil {
		return HandleError("%v", err)
	}

	// Graceful shutdown rides the signal context the root command already sets
	// up (SIGINT/SIGTERM/SIGHUP). The provider is closed where every proxied
	// command closes it, in PersistentPostRunE — which in proxied mode does
	// nothing else, so none of the auto-commit, export or push maintenance can
	// fire behind a server.
	return srv.Serve(rootCtx)
}

// serveModeGate refuses the workspace modes bd serve cannot answer for.
//
// The two refusals are NOT the same kind of statement, and the messages say so.
// Embedded is permanent: there is no SQL server to open a unit of work against,
// and there never will be on that backend. Dolt server mode is staged: the
// provider construction it needs exists, it just is not wired into serve yet —
// so the text says it is not yet supported and names what IS supported today,
// rather than implying the mode is out of scope.
//
// The mode belongs in the message, not in ErrUnsupported.Backend: that field is
// documented as a BACKEND name and is the embryo of the pluggable-backend error
// taxonomy, so putting a topology string in it would hand every downstream
// errors.As a mixed backend/mode vocabulary.
func serveModeGate() error {
	if isEmbeddedMode() {
		return errServeEmbedded()
	}
	if !usesProxiedServer() {
		return errServeDoltServerMode()
	}
	return nil
}

// errServeEmbedded is the PERMANENT refusal. There is no unit-of-work provider
// for the embedded backend and there will not be one: the message says what the
// workspace is and what serve needs, and promises nothing further.
func errServeEmbedded() error {
	return fmt.Errorf("%w: bd serve requires a Dolt SQL server; this workspace uses embedded Dolt",
		&storage.ErrUnsupported{Op: "serve", Backend: "embedded-dolt"})
}

// errServeDoltServerMode is the STAGED refusal. The provider construction this
// mode needs already exists and is already tested; it is simply not wired into
// serve yet. So the text names what works today and says this does not — it
// must never read as "bd serve does not support this", which would send an
// operator off to change their topology for no reason.
func errServeDoltServerMode() error {
	return fmt.Errorf("%w: bd serve supports proxied-server workspaces (managed or external dolt) today; "+
		"dolt server mode is not yet supported by bd serve",
		&storage.ErrUnsupported{Op: "serve", Backend: "dolt"})
}

// serveResolvedMode labels the topology for the startup log line. Cosmetic —
// nothing dispatches on it — but the managed/external distinction is worth
// naming: an external dolt sql-server shares its max_connections budget with
// every other bd process pointed at it, and this server's pool is a claim on
// that budget.
func serveResolvedMode(info domain.ContextInfo) string {
	client, err := configfile.LoadProxiedServerClientInfo(info.BeadsDir)
	if err == nil && client != nil && client.External != nil {
		return info.DoltMode + " (external dolt)"
	}
	return info.DoltMode + " (managed dolt)"
}
