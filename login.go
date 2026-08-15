package w9y

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	deviceCodeURL        = "https://github.com/login/device/code"
	tokenURL             = "https://github.com/login/oauth/access_token"
	tokenFile            = ".w9y/token"
	deviceScope          = "read:org"
	githubClientIDPublic = "Ov23liQtNkgFT24kpN4C"
)

func tokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, tokenFile)
}

func loadToken() string {
	data, err := os.ReadFile(tokenPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveToken(token string) error {
	p := tokenPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(token+"\n"), 0o600)
}

func removeToken() error {
	return os.Remove(tokenPath())
}

func newLoginCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authenticate with GitHub via device flow",
		Long: `Authenticate with GitHub using device flow.

Opens a browser for you to enter a code, then stores the token locally
at ~/.w9y/token for use with other CLI commands.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return runLogin()
		},
	}
}

func newLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove stored GitHub token",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if err := removeToken(); err != nil {
				return err
			}
			fmt.Println("logged out")
			return nil
		},
	}
}

func runLogin() error {
	// Step 1: Request device code
	req, _ := http.NewRequest("POST", deviceCodeURL, strings.NewReader(url.Values{
		"client_id": {githubClientIDPublic},
		"scope":     {deviceScope},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("request device code: %w", err)
	}
	defer resp.Body.Close()

	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return fmt.Errorf("parse device code: %w", err)
	}

	if dc.Interval == 0 {
		dc.Interval = 5
	}

	fmt.Printf("Go to: %s\n", dc.VerificationURI)
	fmt.Printf("Enter code: %s\n", dc.UserCode)
	fmt.Println()

	// Try to open browser
	fmt.Println("Waiting for authorization...")

	// Step 2: Poll for token
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(dc.Interval) * time.Second)

		tokenReq, _ := http.NewRequest("POST", tokenURL, strings.NewReader(url.Values{
			"client_id":   {githubClientIDPublic},
			"device_code": {dc.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}.Encode()))
		tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		tokenReq.Header.Set("Accept", "application/json")
		tokenResp, err := http.DefaultClient.Do(tokenReq)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(tokenResp.Body)
		tokenResp.Body.Close()

		var tr struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
			ErrorDesc   string `json:"error_description"`
		}
		if err := json.Unmarshal(body, &tr); err != nil {
			continue
		}

		switch tr.Error {
		case "":
			// Success — verify org membership
			if err := verifyOrgMembership(tr.AccessToken); err != nil {
				return err
			}
			if err := saveToken(tr.AccessToken); err != nil {
				return fmt.Errorf("save token: %w", err)
			}
			fmt.Println("Login successful!")
			return nil
		case "authorization_pending":
			continue
		case "slow_down":
			dc.Interval += 5
		case "expired_token":
			return fmt.Errorf("device code expired, try again")
		case "access_denied":
			return fmt.Errorf("authorization denied")
		default:
			return fmt.Errorf("unexpected error: %s - %s", tr.Error, tr.ErrorDesc)
		}
	}

	return fmt.Errorf("device code expired, try again")
}

func verifyOrgMembership(accessToken string) error {
	// First get username
	req, _ := http.NewRequest("GET", "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("check user: %w", err)
	}
	defer resp.Body.Close()

	var user struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return fmt.Errorf("parse user: %w", err)
	}

	// Check org membership
	memberURL := fmt.Sprintf("https://api.github.com/orgs/justwasm/members/%s", user.Login)
	memberReq, _ := http.NewRequest("GET", memberURL, nil)
	memberReq.Header.Set("Authorization", "Bearer "+accessToken)
	memberReq.Header.Set("Accept", "application/json")

	memberResp, err := http.DefaultClient.Do(memberReq)
	if err != nil {
		return fmt.Errorf("check membership: %w", err)
	}
	defer memberResp.Body.Close()
	io.Copy(io.Discard, memberResp.Body)

	if memberResp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("you are not a member of the justwasm organization")
	}

	return nil
}
