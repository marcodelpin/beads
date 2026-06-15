package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type exitError struct {
	Code int
}

func (e *exitError) Error() string {
	return fmt.Sprintf("exit code %d", e.Code)
}

func exitCodeFromError(err error) (int, bool) {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.Code, true
	}
	return 0, false
}

func activeWorkspaceNotFoundError() string {
	return "no active beads workspace found"
}

func activeWorkspaceNotFoundMessage() string {
	return "No active beads workspace found."
}

func diagHint() string {
	return workspaceDiagHint(true)
}

func whereDiagHint() string {
	return workspaceDiagHint(false)
}

func workspaceDiagHint(includeWhere bool) string {
	if includeWhere {
		if !usesSQLServer() {
			return "run 'bd where' to inspect the resolved workspace, or 'bd init' to create a new database"
		}
		return "run 'bd where' to inspect the resolved workspace, run 'bd doctor' to diagnose, or 'bd init' to create a new database"
	}
	if !usesSQLServer() {
		return "check BEADS_DIR/worktree setup, or run 'bd init' to create a new database"
	}
	return "check BEADS_DIR/worktree setup, run 'bd doctor' to diagnose, or run 'bd init' to create a new database"
}

func buildJSONError(message, hint string) interface{} {
	inner := map[string]interface{}{
		"error": message,
	}
	if hint != "" {
		inner["hint"] = hint
	}
	if jsonEnvelopeEnabled() {
		return map[string]interface{}{
			"schema_version": JSONSchemaVersion,
			"data":           inner,
		}
	}
	inner["schema_version"] = JSONSchemaVersion
	return inner
}

func jsonStderrError(message, hint string) {
	encoder := json.NewEncoder(os.Stderr)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(buildJSONError(message, hint))
}

func jsonStdoutError(message, hint string) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(buildJSONError(message, hint))
}

func HandleError(format string, args ...interface{}) error {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	return &exitError{Code: 1}
}

func HandleErrorRespectJSON(format string, args ...interface{}) error {
	if jsonOutput {
		jsonStdoutError(fmt.Sprintf(format, args...), "")
		return &exitError{Code: 1}
	}
	return HandleError(format, args...)
}

func HandleErrorWithHint(message, hint string) error {
	if jsonOutput {
		jsonStderrError(message, hint)
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", message)
		fmt.Fprintf(os.Stderr, "Hint: %s\n", hint)
	}
	return &exitError{Code: 1}
}

func HandleErrorWithHintRespectJSON(message, hint string) error {
	if jsonOutput {
		jsonStdoutError(message, hint)
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", message)
		fmt.Fprintf(os.Stderr, "Hint: %s\n", hint)
	}
	return &exitError{Code: 1}
}

func SilentExit() error {
	return &exitError{Code: 1}
}

// FatalError writes an error message to stderr (structured JSON when --json is
// set) and exits with code 1. Retained for the proxied-server code paths that
// run outside cobra's RunE error-return convention; the RunE-converted commands
// use HandleError and friends instead.
func FatalError(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if jsonOutput {
		jsonStderrError(msg, "")
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
	}
	os.Exit(1)
}

// FatalErrorRespectJSON writes an error message and exits with code 1. If
// --json is set, outputs structured JSON to stdout; otherwise plain text to
// stderr.
func FatalErrorRespectJSON(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if jsonOutput {
		jsonStdoutError(msg, "")
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
	}
	os.Exit(1)
}

// FatalErrorWithHintRespectJSON writes an error message with a hint and exits.
// If --json is set, emits structured JSON to stdout so callers can parse it.
func FatalErrorWithHintRespectJSON(message, hint string) {
	if jsonOutput {
		jsonStdoutError(message, hint)
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", message)
		fmt.Fprintf(os.Stderr, "Hint: %s\n", hint)
	}
	os.Exit(1)
}

// FatalErrorWithHint writes an error message with a hint to stderr and exits.
func FatalErrorWithHint(message, hint string) {
	if jsonOutput {
		jsonStderrError(message, hint)
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", message)
		fmt.Fprintf(os.Stderr, "Hint: %s\n", hint)
	}
	os.Exit(1)
}

func WarnError(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "Warning: "+format+"\n", args...)
}

func CheckReadonly(operation string) {
	if readonlyMode {
		fmt.Fprintf(os.Stderr, "Error: operation '%s' is not allowed in read-only mode\n", operation)
		os.Exit(1)
	}
}
