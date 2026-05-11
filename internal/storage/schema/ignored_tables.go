package schema

import (
	"fmt"
	"io/fs"
	"strings"
	"sync"
)

var ignoredMigrations = []int{
	29, // CREATE TABLE local_metadata
	11, // CREATE TABLE repo_mtimes
	20, // CREATE TABLE wisps
	21, // CREATE TABLE wisp_labels, wisp_dependencies, wisp_events, wisp_comments
	22, // CREATE INDEX on wisp_dependencies
	23, // ADD COLUMN no_history (issues + wisps; issues ALTER tolerated as duplicate)
	27, // ADD COLUMN started_at (issues + wisps; issues ALTER tolerated as duplicate)
	31, // CREATE INDEX idx_wisp_events_created_at
}

var (
	ignoredDDLOnce sync.Once
	ignoredDDLVal  []string
)

// IgnoredTableDDL returns the ordered list of SQL bodies needed to recreate
// all dolt-ignored tables from scratch. Each entry is the full contents of
// one .up.sql file. Derived at first call and cached thereafter.
func IgnoredTableDDL() []string {
	ignoredDDLOnce.Do(func() {
		ignoredDDLVal = buildIgnoredTableDDL()
	})
	return ignoredDDLVal
}

func buildIgnoredTableDDL() []string {
	result := make([]string, 0, len(ignoredMigrations))
	for _, v := range ignoredMigrations {
		result = append(result, ReadMigrationSQL(v))
	}
	return result
}

// ReadMigrationSQL reads the embedded .up.sql file for the given version number
// and returns its contents as a string. Panics if the migration is not found.
func ReadMigrationSQL(version int) string {
	entries, err := fs.ReadDir(upMigrations, "migrations")
	if err != nil {
		panic(fmt.Sprintf("schema: reading migrations dir: %v", err))
	}
	prefix := fmt.Sprintf("%04d_", version)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".up.sql") {
			data, err := upMigrations.ReadFile("migrations/" + e.Name())
			if err != nil {
				panic(fmt.Sprintf("schema: reading migration %s: %v", e.Name(), err))
			}
			return string(data)
		}
	}
	panic(fmt.Sprintf("schema: migration %04d not found", version))
}
