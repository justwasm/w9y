package w9y

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	sessionCookie = "w9y_session"
	sessionMaxAge = 7 * 24 * time.Hour
)

var (
	githubClientID     = os.Getenv("GITHUB_CLIENT_ID")
	githubClientSecret = os.Getenv("GITHUB_CLIENT_SECRET")
	sessionSecret      = os.Getenv("W9Y_SESSION_SECRET")
	githubOrg          = "justwasm"
)

func authEnabled() bool {
	return githubClientID != "" && githubClientSecret != "" && sessionSecret != ""
}

func signSession(username string) string {
	mac := hmac.New(sha256.New, []byte(sessionSecret))
	mac.Write([]byte(username))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return username + "." + sig
}

func verifySession(cookie string) (string, bool) {
	parts := strings.SplitN(cookie, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	username, sigB64 := parts[0], parts[1]

	mac := hmac.New(sha256.New, []byte(sessionSecret))
	mac.Write([]byte(username))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sigB64), []byte(expected)) {
		return "", false
	}
	return username, true
}

func getUsername(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	username, ok := verifySession(c.Value)
	if !ok {
		return ""
	}
	return username
}

func handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !authEnabled() {
		http.Error(w, "auth not configured", http.StatusServiceUnavailable)
		return
	}
	callbackURL := "/api/auth/callback"
	if r.TLS == nil && !strings.Contains(r.Host, "localhost") {
		callbackURL = "https://" + r.Host + "/api/auth/callback"
	}
 redirectTo := fmt.Sprintf(
		"https://github.com/login/oauth/authorize?client_id=%s&scope=read:org&redirect_uri=%s",
		githubClientID, callbackURL,
	)
	http.Redirect(w, r, redirectTo, http.StatusFound)
}

func handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !authEnabled() {
		http.Error(w, "auth not configured", http.StatusServiceUnavailable)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	accessToken, err := exchangeCode(code)
	if err != nil {
		slog.Error("github token exchange failed", "error", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}

	username, err := fetchUsername(accessToken)
	if err != nil {
		slog.Error("github user fetch failed", "error", err)
		http.Error(w, "failed to fetch user info", http.StatusBadGateway)
		return
	}

	isMember, err := checkOrgMembership(accessToken, username)
	if err != nil {
		slog.Error("org membership check failed", "error", err, "user", username)
		http.Error(w, "failed to verify org membership", http.StatusBadGateway)
		return
	}
	if !isMember {
		http.Error(w, "you are not a member of the "+githubOrg+" organization", http.StatusForbidden)
		return
	}

	signed := signSession(username)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    signed,
		Path:     "/",
		MaxAge:   int(sessionMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, "/", http.StatusFound)
}

func handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func handleAuthUser(w http.ResponseWriter, r *http.Request) {
	username := getUsername(r)
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authenticated":false}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"authenticated": true,
		"username":      username,
	})
}

func exchangeCode(code string) (string, error) {
	data := fmt.Sprintf(`{"client_id":"%s","client_secret":"%s","code":"%s"}`,
		githubClientID, githubClientSecret, code)

	resp, err := http.Post("https://github.com/login/oauth/access_token", "application/json",
		strings.NewReader(data))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Error != "" {
		return "", fmt.Errorf("github oauth error: %s", result.Error)
	}
	return result.AccessToken, nil
}

func fetchUsername(accessToken string) (string, error) {
	req, _ := http.NewRequest("GET", "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var user struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return "", err
	}
	if user.Login == "" {
		return "", fmt.Errorf("empty username")
	}
	return user.Login, nil
}

func checkOrgMembership(accessToken, username string) (bool, error) {
	url := fmt.Sprintf("https://api.github.com/orgs/%s/members/%s", githubOrg, username)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// 204 = member, 404 = not member
	return resp.StatusCode == http.StatusNoContent, nil
}
