package main

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newServerFlagsCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("server-host", "", "")
	c.Flags().Int("server-port", 0, "")
	c.Flags().String("server-socket", "", "")
	c.Flags().String("server-user", "", "")
	c.Flags().Bool("server-tls", false, "")
	return c
}

func mustSetFlag(t *testing.T, c *cobra.Command, name, value string) {
	t.Helper()
	if err := c.Flags().Set(name, value); err != nil {
		t.Fatal(err)
	}
}

func mustPromote(t *testing.T, c *cobra.Command) {
	t.Helper()
	if err := promoteExplicitServerConnFlags(c); err != nil {
		t.Fatalf("promoteExplicitServerConnFlags: %v", err)
	}
}

func TestPromoteExplicitServerConnFlags(t *testing.T) {
	t.Run("explicit flags override environment", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_HOST", "profile-host")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")
		t.Setenv("BEADS_DOLT_SERVER_USER", "profile-user")

		c := newServerFlagsCmd()
		mustSetFlag(t, c, "server-host", "db.example.com")
		mustSetFlag(t, c, "server-port", "3306")
		mustSetFlag(t, c, "server-user", "app_rw")

		mustPromote(t, c)

		if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "db.example.com" {
			t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want db.example.com", got)
		}
		if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "3306" {
			t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want 3306", got)
		}
		if got := os.Getenv("BEADS_DOLT_SERVER_USER"); got != "app_rw" {
			t.Errorf("BEADS_DOLT_SERVER_USER = %q, want app_rw", got)
		}
	})

	t.Run("unset flags leave environment untouched", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_HOST", "profile-host")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")
		t.Setenv("BEADS_DOLT_SERVER_SOCKET", "/tmp/profile.sock")

		mustPromote(t, newServerFlagsCmd())

		if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "profile-host" {
			t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want profile-host", got)
		}
		if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "3307" {
			t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want 3307", got)
		}
		if got := os.Getenv("BEADS_DOLT_SERVER_SOCKET"); got != "/tmp/profile.sock" {
			t.Errorf("BEADS_DOLT_SERVER_SOCKET = %q, want /tmp/profile.sock (untouched)", got)
		}
	})

	t.Run("server-tls flag promotes both directions", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_TLS", "1")

		c := newServerFlagsCmd()
		mustSetFlag(t, c, "server-tls", "false")
		mustPromote(t, c)
		if got := os.Getenv("BEADS_DOLT_SERVER_TLS"); got != "0" {
			t.Errorf("BEADS_DOLT_SERVER_TLS = %q, want 0 (explicit --server-tls=false)", got)
		}

		c = newServerFlagsCmd()
		mustSetFlag(t, c, "server-tls", "true")
		mustPromote(t, c)
		if got := os.Getenv("BEADS_DOLT_SERVER_TLS"); got != "1" {
			t.Errorf("BEADS_DOLT_SERVER_TLS = %q, want 1 (explicit --server-tls)", got)
		}
	})

	t.Run("unset server-tls leaves environment untouched", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_TLS", "true")
		mustPromote(t, newServerFlagsCmd())
		if got := os.Getenv("BEADS_DOLT_SERVER_TLS"); got != "true" {
			t.Errorf("BEADS_DOLT_SERVER_TLS = %q, want true (untouched)", got)
		}
	})

	t.Run("socket flag overrides environment", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_SOCKET", "/tmp/stale.sock")

		c := newServerFlagsCmd()
		mustSetFlag(t, c, "server-socket", "/tmp/fresh.sock")
		mustPromote(t, c)

		if got := os.Getenv("BEADS_DOLT_SERVER_SOCKET"); got != "/tmp/fresh.sock" {
			t.Errorf("BEADS_DOLT_SERVER_SOCKET = %q, want /tmp/fresh.sock", got)
		}
	})

	t.Run("explicit empty socket selects TCP over ambient socket", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_SOCKET", "/tmp/stale.sock")

		c := newServerFlagsCmd()
		mustSetFlag(t, c, "server-socket", "")
		mustPromote(t, c)

		if got, ok := os.LookupEnv("BEADS_DOLT_SERVER_SOCKET"); ok {
			t.Errorf("BEADS_DOLT_SERVER_SOCKET = %q, want unset (empty means TCP)", got)
		}
	})

	t.Run("explicit TCP flags clear ambient socket", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_SOCKET", "/tmp/stale.sock")

		c := newServerFlagsCmd()
		mustSetFlag(t, c, "server-host", "db.example.com")
		mustPromote(t, c)

		if got, ok := os.LookupEnv("BEADS_DOLT_SERVER_SOCKET"); ok {
			t.Errorf("BEADS_DOLT_SERVER_SOCKET = %q, want unset (explicit TCP selected)", got)
		}

		t.Setenv("BEADS_DOLT_SERVER_SOCKET", "/tmp/stale.sock")
		c = newServerFlagsCmd()
		mustSetFlag(t, c, "server-port", "3306")
		mustPromote(t, c)

		if got, ok := os.LookupEnv("BEADS_DOLT_SERVER_SOCKET"); ok {
			t.Errorf("BEADS_DOLT_SERVER_SOCKET = %q, want unset (explicit TCP selected)", got)
		}
	})

	t.Run("explicit socket wins over explicit TCP flags", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_SOCKET", "/tmp/stale.sock")

		c := newServerFlagsCmd()
		mustSetFlag(t, c, "server-host", "db.example.com")
		mustSetFlag(t, c, "server-socket", "/tmp/fresh.sock")
		mustPromote(t, c)

		if got := os.Getenv("BEADS_DOLT_SERVER_SOCKET"); got != "/tmp/fresh.sock" {
			t.Errorf("BEADS_DOLT_SERVER_SOCKET = %q, want /tmp/fresh.sock (socket flag documented to override host/port)", got)
		}
	})

	t.Run("non-TCP flags do not clear ambient socket", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_SOCKET", "/tmp/profile.sock")

		c := newServerFlagsCmd()
		mustSetFlag(t, c, "server-user", "app_rw")
		mustPromote(t, c)

		if got := os.Getenv("BEADS_DOLT_SERVER_SOCKET"); got != "/tmp/profile.sock" {
			t.Errorf("BEADS_DOLT_SERVER_SOCKET = %q, want /tmp/profile.sock (untouched)", got)
		}
	})

	t.Run("changed empty and out-of-range values fail explicitly", func(t *testing.T) {
		cases := []struct {
			name    string
			flag    string
			value   string
			wantSub string
		}{
			{"empty host", "server-host", "", "--server-host cannot be empty"},
			{"zero port", "server-port", "0", "--server-port must be between 1 and 65535"},
			{"negative port", "server-port", "-1", "--server-port must be between 1 and 65535"},
			{"oversized port", "server-port", "65536", "--server-port must be between 1 and 65535"},
			{"empty user", "server-user", "", "--server-user cannot be empty"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("BEADS_DOLT_SERVER_HOST", "profile-host")
				t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")
				t.Setenv("BEADS_DOLT_SERVER_USER", "profile-user")

				c := newServerFlagsCmd()
				mustSetFlag(t, c, tc.flag, tc.value)

				err := promoteExplicitServerConnFlags(c)
				if err == nil {
					t.Fatalf("promoteExplicitServerConnFlags = nil, want error containing %q", tc.wantSub)
				}
				if !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("error = %q, want substring %q", err, tc.wantSub)
				}
				if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "profile-host" {
					t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want profile-host (untouched on error)", got)
				}
				if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "3307" {
					t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want 3307 (untouched on error)", got)
				}
				if got := os.Getenv("BEADS_DOLT_SERVER_USER"); got != "profile-user" {
					t.Errorf("BEADS_DOLT_SERVER_USER = %q, want profile-user (untouched on error)", got)
				}
			})
		}
	})
}
