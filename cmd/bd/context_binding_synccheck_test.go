//go:build cgo

package main

import (
	"debug/buildinfo"
	"strings"
	"testing"
)

// requireFullCGOBuild fails the test unless the bd binary at path was built
// the way the fork's context-binding coverage needs it: with cgo enabled and
// WITHOUT the gms_pure_go tag (the default/full cgo build). The shared
// subprocess helper builds with -tags gms_pure_go, so a test that switched to
// it would still pass while no longer exercising the full-cgo configuration
// (bda-tfok.4.5, codex round 1 F10). The build settings Go stamps into the
// binary are the authority; the helper that produced it is not.
func requireFullCGOBuild(t *testing.T, path string) {
	t.Helper()
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		t.Fatalf("read build info of %s: %v", path, err)
	}
	var tags, cgo string
	for _, s := range info.Settings {
		switch s.Key {
		case "-tags":
			tags = s.Value
		case "CGO_ENABLED":
			cgo = s.Value
		}
	}
	for _, tag := range strings.Split(tags, ",") {
		if strings.TrimSpace(tag) == "gms_pure_go" {
			t.Fatalf("bd binary %s was built with -tags %q: want the full cgo build, without gms_pure_go", path, tags)
		}
	}
	if cgo != "1" {
		t.Fatalf("bd binary %s was built with CGO_ENABLED=%q: want the full cgo build (CGO_ENABLED=1)", path, cgo)
	}
}
