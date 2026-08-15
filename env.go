package w9y

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newEnvCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "env",
		Short: "List environment variables used by w9y",
		Long:  `Show all environment variables, their current values, and descriptions.`,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			type envVar struct {
				Name        string
				Value       string
				Default     string
				Description string
			}

			vars := []envVar{
				{"W9Y", getenv("W9Y", ""), "https://w9y.io/", "Remote w9y server URL for CLI commands (upload, backup, restore, delete, get, list)"},
				{"DATA_DIR", getenv("DATA_DIR", ""), "data", "Data directory for local blob storage"},
				{"PORT", getenv("PORT", ""), "8080", "Server listen port"},
				{"GITHUB_CLIENT_ID", mask(getenv("GITHUB_CLIENT_ID", "")), "", "GitHub OAuth App client ID (enables web login)"},
				{"GITHUB_CLIENT_SECRET", mask(getenv("GITHUB_CLIENT_SECRET", "")), "", "GitHub OAuth App client secret (enables web login)"},
				{"W9Y_SESSION_SECRET", mask(getenv("W9Y_SESSION_SECRET", "")), "", "HMAC signing key for web session cookies (enables web login)"},
				{"HOST", getenv("HOST", ""), "", "Public host of this server. Used to build the OAuth redirect_uri. If empty, auto-built from X-Forwarded-Host (or Host) on each request."},
			}

			for _, v := range vars {
				val := v.Value
				if val == "" {
					val = "(not set)"
				}
				def := v.Default
				if def == "" {
					def = "-"
				}
				fmt.Printf("%-26s = %-20s  (default: %s)\n  %s\n\n", v.Name, val, def, v.Description)
			}
			return nil
		},
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}
