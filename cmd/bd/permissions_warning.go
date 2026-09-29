package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/config"
)

// The .beads permissions warning is emitted once per invocation, and only
// after the output mode has settled. The context-binding paths
// (prepareSelectedCommandContext, loadEnvironment) run before
// refreshBoundCommandConfig reads the target's config json:true, and several
// of them run more than once per command, so they only record the directory;
// flushBeadsDirPermissionsWarning emits at the end of PersistentPreRunE.
// A binding made after that point (a RunE rebinding, e.g. `bd where`) emits
// directly, because the output mode is final by then (edt-donk).
var (
	beadsDirPermissionsPending  string
	beadsDirPermissionsSettled  bool
	beadsDirPermissionsReported bool
	beadsDirPermissionsJSON     bool
)

// resetBeadsDirPermissionsWarning clears the per-invocation state. Called at
// the start of PersistentPreRunE.
func resetBeadsDirPermissionsWarning() {
	beadsDirPermissionsPending = ""
	beadsDirPermissionsSettled = false
	beadsDirPermissionsReported = false
	beadsDirPermissionsJSON = false
}

// noteBeadsDirPermissions records beadsDir for the permissions check, or
// checks it now when the output mode has already settled.
func noteBeadsDirPermissions(beadsDir string) {
	if beadsDir == "" {
		return
	}
	beadsDirPermissionsPending = beadsDir
	if beadsDirPermissionsSettled {
		emitBeadsDirPermissionsWarning()
	}
}

// flushBeadsDirPermissionsWarning marks the output mode as settled for cmd
// and emits the warning for the recorded directory.
func flushBeadsDirPermissionsWarning(cmd *cobra.Command) {
	beadsDirPermissionsSettled = true
	beadsDirPermissionsJSON = commandFormatIsJSON(cmd)
	emitBeadsDirPermissionsWarning()
}

// commandFormatIsJSON reports whether the command's --format flag (root, or a
// local one that shadows it, e.g. `bd list --format json`) asks for JSON. A
// local --format is resolved into jsonOutput only inside RunE, after the
// warning is emitted.
func commandFormatIsJSON(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	f := cmd.Flags().Lookup("format")
	return f != nil && f.Changed && strings.EqualFold(f.Value.String(), "json")
}

// emitBeadsDirPermissionsWarning emits the warning at most once per invocation
// and never when JSON output was requested (root or local --json, --format
// json, config json:true).
func emitBeadsDirPermissionsWarning() {
	if beadsDirPermissionsReported || beadsDirPermissionsPending == "" || jsonOutput || beadsDirPermissionsJSON {
		return
	}
	beadsDirPermissionsReported = true
	config.CheckBeadsDirPermissions(beadsDirPermissionsPending)
}
