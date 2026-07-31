// Package workapi holds the work-query contract shared by every bd frontend.
//
// Filter construction, defaults, and validation live here so the CLI and any
// other frontend answer the same question the same way instead of drifting
// through parallel copies. The package therefore depends on neither frontend:
// it must not import github.com/spf13/cobra or net/http, and it must not read
// process-local state (client cwd, environment) that is meaningless in a
// long-lived server. internal/config is available only for workspace-scoped
// reads such as GetCustomTypesFromYAML.
//
// The boundary is enforced mechanically, not by review: see the
// workapi-frontend-boundary depguard rule in .golangci.yml and the
// banned-accessor check in scripts/ci/pr-policy.sh.
package workapi
