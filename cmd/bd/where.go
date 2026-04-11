package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/utils"
)

// WhereResult contains information about the active beads location
type WhereResult struct {
	Path           string `json:"path"`                      // Active .beads directory path
	RedirectedFrom string `json:"redirected_from,omitempty"` // Original path if redirected
	Prefix         string `json:"prefix,omitempty"`          // Issue prefix (if detectable)
	DatabasePath   string `json:"database_path,omitempty"`   // Full path to database file
}

var whereCmd = &cobra.Command{
	Use:     "where",
	GroupID: "setup",
	Short:   "Show active beads location",
	Long: `Show the active beads database location, including redirect information.

	This command is useful for debugging when using redirects, to understand
	which beads workspace is actually being used.

Examples:
  bd where           # Show active beads location
  bd where --json    # Output in JSON format
`,
	Run: func(cmd *cobra.Command, args []string) {
		result := WhereResult{}

		if selected := selectedNoDBBeadsDir(); selected != "" {
			prepareSelectedNoDBContext(selected)
		}

		beadsDir := resolveWhereBeadsDir()
		if beadsDir == "" {
			if jsonOutput {
				outputJSON(map[string]string{
					"error":   "no_beads_directory",
					"message": activeWorkspaceNotFoundMessage(),
					"hint":    whereDiagHint(),
				})
			} else {
				fmt.Fprintln(os.Stderr, "Error: "+activeWorkspaceNotFoundMessage())
				fmt.Fprintln(os.Stderr, "Hint: "+whereDiagHint())
			}
			os.Exit(1)
		}

		result.Path = beadsDir

		// Check if we got here via redirect by looking for the original .beads directory
		// Walk up from cwd to find any .beads with a redirect file
		originalBeadsDir := findOriginalBeadsDir()
		if originalBeadsDir != "" && originalBeadsDir != beadsDir {
			result.RedirectedFrom = originalBeadsDir
		}

		// Find the database path
		dbPath := resolveWhereDatabasePath()
		if dbPath != "" {
			result.DatabasePath = dbPath
		}

		// Prefer YAML when available, otherwise do a scoped read-only reopen
		// using the already-resolved dbPath so we can preserve prefix output
		// without paying the old worktree-discovery cost.
		if prefix := config.GetString("issue-prefix"); prefix != "" {
			result.Prefix = prefix
		} else if dbPath != "" {
			_ = withStorage(getRootContext(), nil, dbPath, func(currentStore storage.DoltStorage) error {
				prefix, err := currentStore.GetConfig(getRootContext(), "issue_prefix")
				if err == nil && prefix != "" {
					result.Prefix = prefix
				}
				return nil
			})
		}

		// Output results
		if jsonOutput {
			outputJSON(result)
		} else {
			fmt.Println(result.Path)
			if result.RedirectedFrom != "" {
				fmt.Printf("  (via redirect from %s)\n", result.RedirectedFrom)
			}
			if result.Prefix != "" {
				fmt.Printf("  prefix: %s\n", result.Prefix)
			}
			if result.DatabasePath != "" {
				fmt.Printf("  database: %s\n", result.DatabasePath)
			}
		}
	},
}

func resolveWhereBeadsDir() string {
	if selected := selectedNoDBBeadsDir(); selected != "" {
		return selected
	}

	return beads.FindBeadsDir()
}

func resolveWhereDatabasePath() string {
	return beads.FindDatabasePath()
}

// findOriginalBeadsDir walks up from cwd looking for a .beads directory with a redirect file
// Returns the original .beads path if found, empty string otherwise
func findOriginalBeadsDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	// Canonicalize cwd to handle symlinks
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	// Check BEADS_DIR first
	if envDir := os.Getenv("BEADS_DIR"); envDir != "" {
		envDir = utils.CanonicalizePath(envDir)
		redirectFile := filepath.Join(envDir, beads.RedirectFileName)
		if _, err := os.Stat(redirectFile); err == nil {
			return envDir
		}
		return ""
	}

	// Walk up directory tree looking for .beads with redirect
	for dir := cwd; dir != "/" && dir != "."; {
		beadsDir := filepath.Join(dir, ".beads")
		if info, err := os.Stat(beadsDir); err == nil && info.IsDir() {
			redirectFile := filepath.Join(beadsDir, beads.RedirectFileName)
			if _, err := os.Stat(redirectFile); err == nil {
				return beadsDir
			}
			// Found .beads without redirect - this is the actual location
			return ""
		}

		// Move up one directory
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root (works on both Unix and Windows)
			// On Unix: filepath.Dir("/") returns "/"
			// On Windows: filepath.Dir("C:\\") returns "C:\\"
			break
		}
		dir = parent
	}

	return ""
}

func init() {
	rootCmd.AddCommand(whereCmd)
}
