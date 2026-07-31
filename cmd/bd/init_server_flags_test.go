package main

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
)

func newServerFlagsCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("server-host", "", "")
	c.Flags().Int("server-port", 0, "")
	c.Flags().String("server-user", "", "")
	return c
}

func TestPromoteExplicitServerConnFlags(t *testing.T) {
	t.Run("explicit flags override environment", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_HOST", "profile-host")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")
		t.Setenv("BEADS_DOLT_SERVER_USER", "profile-user")

		c := newServerFlagsCmd()
		if err := c.Flags().Set("server-host", "db.example.com"); err != nil {
			t.Fatal(err)
		}
		if err := c.Flags().Set("server-port", "3306"); err != nil {
			t.Fatal(err)
		}
		if err := c.Flags().Set("server-user", "app_rw"); err != nil {
			t.Fatal(err)
		}

		promoteExplicitServerConnFlags(c)

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

		promoteExplicitServerConnFlags(newServerFlagsCmd())

		if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "profile-host" {
			t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want profile-host", got)
		}
		if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "3307" {
			t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want 3307", got)
		}
	})

	t.Run("empty and zero flag values are not promoted", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_HOST", "profile-host")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")

		c := newServerFlagsCmd()
		if err := c.Flags().Set("server-host", ""); err != nil {
			t.Fatal(err)
		}
		if err := c.Flags().Set("server-port", "0"); err != nil {
			t.Fatal(err)
		}

		promoteExplicitServerConnFlags(c)

		if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "profile-host" {
			t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want profile-host", got)
		}
		if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "3307" {
			t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want 3307", got)
		}
	})
}
