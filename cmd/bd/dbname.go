package main

import "strings"

// sanitizeDBName replaces hyphens and dots with underscores for
// SQL-idiomatic embedded Dolt database names (GH#2142, GH#3231).
//
// Located in a non-build-tagged file so it remains linkable when the
// CGO-gated embedded-Dolt code is excluded (GH#3402 workaround: callers
// such as init.go reference sanitizeDBName unconditionally).
func sanitizeDBName(name string) string {
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, ".", "_")
	return name
}
