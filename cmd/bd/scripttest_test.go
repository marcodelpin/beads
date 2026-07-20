//go:build scripttests
// +build scripttests

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"rsc.io/script"
	"rsc.io/script/scripttest"
)

func TestScripts(t *testing.T) {
	// Skip on Windows - test scripts use sh -c which requires Unix shell
	if runtime.GOOS == "windows" {
		t.Skip("scripttest uses Unix shell commands (sh -c), skipping on Windows")
	}

	// Use the shared bd binary (built once, reused across cmd/bd tests; see
	// buildBDForInitTests in test_helpers_pure_test.go, bda-9l1).
	exe := buildBDForInitTests(t)
	binDir := filepath.Dir(exe)

	// Create minimal engine with default commands plus bd
	timeout := 2 * time.Second
	engine := script.NewEngine()
	engine.Cmds["bd"] = script.Program(exe, nil, timeout)

	// Add binDir to PATH so 'sh -c bd ...' works in test scripts
	currentPath := os.Getenv("PATH")
	env := []string{"PATH=" + binDir + ":" + currentPath}

	// Run all tests
	scripttest.Test(t, context.Background(), engine, env, "testdata/*.txt")
}
