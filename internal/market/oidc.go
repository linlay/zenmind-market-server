package market

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const oidcStateCookie = "zenmind_market_oidc_state"
const oidcSessionCookie = "zenmind_market_session"

type oidcClient struct {
	verifier *oidc.IDTokenVerifier
	config   oauth2.Config
	cfg      Config
}

type oidcState struct {
	State, Nonce, Verifier string
	ExpiresAt              int64
}
type oidcSession struct {
	UserID    string
	ExpiresAt int64
}

func newOIDCClient(ctx context.Context, cfg Config) (*oidcClient, error) {
	if strings.TrimSpace(cfg.OIDCIssuer) == "" {
		return nil, nil
	}
	if strings.TrimSpace(cfg.OIDCClientID) == "" || strings.TrimSpace(cfg.OIDCClientSecret) == "" || strings.TrimSpace(cfg.OIDCSessionSecret) == "" {
		return nil, errors.New("MARKET_OIDC_CLIENT_ID, MARKET_OIDC_CLIENT_SECRET, and MARKET_OIDC_SESSION_SECRET are required when MARKET_OIDC_ISSUER is set")
	}
	redirectURL := strings.TrimSpace(cfg.OIDCRedirectURL)
	if redirectURL == "" {
		redirectURL = strings.TrimRight(cfg.PublicBaseURL, "/") + "/api/v1/auth/oidc/callback"
	}
	parsed, err := url.Parse(redirectURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("MARKET_OIDC_REDIRECT_URL must be an absolute URL")
	}
	provider, err := oidc.NewProvider(ctx, cfg.OIDCIssuer)
	if err != nil {
		return nil, err
	}
	scopes := strings.Fields(cfg.OIDCScopes)
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID}
	}
	if !containsString(scopes, oidc.ScopeOpenID) {
		scopes = append([]string{oidc.ScopeOpenID}, scopes...)
	}
	return &oidcClient{verifier: provider.Verifier(&oidc.Config{ClientID: cfg.OIDCClientID}), cfg: cfg, config: oauth2.Config{ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret, Endpoint: provider.Endpoint(), RedirectURL: redirectURL, Scopes: scopes}}, nil
}

func (a *App) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	client, err := a.ensureOIDCClient(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "oidc_unavailable", "OIDC login is temporarily unavailable")
		return
	}
	if client == nil {
		writeError(w, http.StatusNotFound, "oidc_disabled", "OIDC login is not configured")
		return
	}
	state, err := randomOIDCState()
	if err != nil {
		writeError(w, 500, "auth_error", err.Error())
		return
	}
	nonce, err := randomURLToken(32)
	if err != nil {
		writeError(w, 500, "auth_error", err.Error())
		return
	}
	verifier, err := randomURLToken(48)
	if err != nil {
		writeError(w, 500, "auth_error", err.Error())
		return
	}
	value, err := client.sign(oidcState{State: state, Nonce: nonce, Verifier: verifier, ExpiresAt: time.Now().Add(10 * time.Minute).Unix()})
	if err != nil {
		writeError(w, 500, "auth_error", err.Error())
		return
	}
	a.setOIDCCookie(w, oidcStateCookie, value, 600)
	http.Redirect(w, r, client.config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (a *App) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	client, err := a.ensureOIDCClient(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "oidc_unavailable", "OIDC login is temporarily unavailable")
		return
	}
	if client == nil {
		writeError(w, http.StatusNotFound, "oidc_disabled", "OIDC login is not configured")
		return
	}
	state, err := a.readOIDCState(client, r)
	if err != nil || !hmac.Equal([]byte(r.URL.Query().Get("state")), []byte(state.State)) || state.ExpiresAt <= time.Now().Unix() {
		writeError(w, 400, "invalid_oidc_state", "OIDC login state is invalid or expired")
		return
	}
	if code := r.URL.Query().Get("code"); code == "" {
		writeError(w, 400, "oidc_callback_error", "OIDC authorization code is missing")
		return
	} else {
		token, err := client.config.Exchange(r.Context(), code, oauth2.VerifierOption(state.Verifier))
		if err != nil {
			writeError(w, 401, "oidc_exchange_failed", "OIDC authorization-code exchange failed")
			return
		}
		rawIDToken, _ := token.Extra("id_token").(string)
		idToken, err := client.verifier.Verify(r.Context(), rawIDToken)
		if err != nil {
			writeError(w, 401, "invalid_oidc_token", "OIDC ID token verification failed")
			return
		}
		if idToken.Nonce != state.Nonce {
			writeError(w, 401, "invalid_oidc_nonce", "OIDC nonce mismatch")
			return
		}
		var claims map[string]any
		if err := idToken.Claims(&claims); err != nil {
			writeError(w, 401, "invalid_oidc_token", "OIDC ID token claims are invalid")
			return
		}
		if a.cfg.OIDCDebugClaims {
			if raw, err := json.Marshal(redactedOIDCClaims(claims)); err == nil {
				log.Printf("OIDC verified ID token claims: %s", raw)
			}
		}
		userID := strings.TrimSpace(stringClaim(claims, "sub"))
		if userID == "" {
			writeError(w, 401, "invalid_oidc_token", "OIDC subject is missing")
			return
		}
		username, name := oidcIdentityClaims(claims, userID)
		emailVerified, hasEmailVerified := optionalBoolClaim(claims, "email_verified")
		user, err := a.store.UpsertOIDCUser(r.Context(), oidcUserProfile{
			Issuer:           strings.TrimSpace(a.cfg.OIDCIssuer),
			Subject:          userID,
			Username:         username,
			DisplayName:      name,
			Email:            stringClaim(claims, "email"),
			EmailVerified:    emailVerified,
			HasEmailVerified: hasEmailVerified,
			ProviderAccount:  stringClaim(claims, "oaAccount"),
			ExternalUserID:   stringClaim(claims, "userId"),
			StaffNumber:      stringClaim(claims, "staffno"),
			IsAdmin:          oidcRole(claims, a.cfg) == "admin",
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		session, err := client.sign(oidcSession{
			UserID:    user.ID,
			ExpiresAt: idToken.Expiry.Unix(),
		})
		if err != nil {
			writeError(w, 500, "auth_error", err.Error())
			return
		}
		a.setOIDCCookie(w, oidcSessionCookie, session, int(time.Until(idToken.Expiry).Seconds()))
		a.setOIDCCookie(w, oidcStateCookie, "", -1)
		http.Redirect(w, r, safeOIDCRedirect(a.cfg.OIDCSuccessRedirect), http.StatusFound)
	}
}

func (a *App) handleOIDCLogout(w http.ResponseWriter, r *http.Request) {
	a.setOIDCCookie(w, oidcSessionCookie, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
func (a *App) oidcUserFromRequest(r *http.Request) (localUser, bool) {
	client := a.currentOIDCClient()
	if client == nil {
		return localUser{}, false
	}
	cookie, err := r.Cookie(oidcSessionCookie)
	if err != nil {
		return localUser{}, false
	}
	var session oidcSession
	if client.verify(cookie.Value, &session) != nil || session.ExpiresAt <= time.Now().Unix() || session.UserID == "" {
		return localUser{}, false
	}
	user, err := a.store.GetMarketUser(r.Context(), session.UserID)
	if err != nil || user.Status != "active" {
		return localUser{}, false
	}
	return localUser{ID: user.ID, Username: user.Username, Name: user.DisplayName, Email: user.Email, Role: user.Role}, true
}
func (a *App) readOIDCState(client *oidcClient, r *http.Request) (oidcState, error) {
	cookie, err := r.Cookie(oidcStateCookie)
	if err != nil {
		return oidcState{}, err
	}
	var value oidcState
	return value, client.verify(cookie.Value, &value)
}
func (a *App) setOIDCCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: strings.HasPrefix(strings.ToLower(a.cfg.PublicBaseURL), "https://"), SameSite: http.SameSiteLaxMode})
}
func (c *oidcClient) sign(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	h := hmac.New(sha256.New, []byte(c.cfg.OIDCSessionSecret))
	_, _ = h.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil)), nil
}
func (c *oidcClient) verify(value string, target any) error {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return errors.New("invalid signed value")
	}
	h := hmac.New(sha256.New, []byte(c.cfg.OIDCSessionSecret))
	_, _ = h.Write([]byte(parts[0]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(sig, h.Sum(nil)) {
		return errors.New("invalid signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}
func randomURLToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// The provider requires state to be opaque, random, and free of special characters.
func randomOIDCState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
func stringClaim(claims map[string]any, name string) string {
	value, _ := claims[name].(string)
	return strings.TrimSpace(value)
}

func optionalBoolClaim(claims map[string]any, name string) (bool, bool) {
	value, ok := claims[name].(bool)
	return value, ok
}

func oidcIdentityClaims(claims map[string]any, userID string) (username, name string) {
	username = stringClaim(claims, "oaAccount")
	if username == "" {
		username = stringClaim(claims, "staffoa")
	}
	if username == "" {
		username = stringClaim(claims, "preferred_username")
	}
	if username == "" {
		username = stringClaim(claims, "username")
	}
	name = stringClaim(claims, "name")
	if name == "" {
		name = stringClaim(claims, "nickname")
	}
	if name == "" {
		name = username
	}
	if username == "" {
		username = stringClaim(claims, "email")
	}
	if username == "" {
		username = userID
	}
	if name == "" {
		name = username
	}
	return username, name
}

func redactedOIDCClaims(claims map[string]any) map[string]any {
	result := make(map[string]any, len(claims))
	for key, value := range claims {
		lowerKey := strings.ToLower(key)
		if sensitiveOIDCClaim(lowerKey) {
			result[key] = "[redacted]"
			continue
		}
		result[key] = value
	}
	return result
}

func sensitiveOIDCClaim(key string) bool {
	if strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "assertion") {
		return true
	}
	if key == "email_verified" || key == "phone_number_verified" {
		return false
	}
	switch key {
	case "sub", "subjectid", "userid", "jti", "nonce", "name", "email", "staffemail", "phone_number", "oaaccount", "preferred_username", "staffoa", "staffno", "staffuserid", "gxj":
		return true
	default:
		return false
	}
}
func oidcRole(claims map[string]any, cfg Config) string {
	want := strings.TrimSpace(cfg.OIDCAdminRole)
	if want != "" {
		for _, value := range claimStrings(claims[cfg.OIDCRoleClaim]) {
			if value == want {
				return "admin"
			}
		}
	}
	return "creator"
}
func claimStrings(value any) []string {
	switch v := value.(type) {
	case string:
		return strings.Fields(v)
	case []any:
		result := make([]string, 0, len(v))
		for _, item := range v {
			if value, ok := item.(string); ok {
				result = append(result, value)
			}
		}
		return result
	}
	return nil
}
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func safeOIDCRedirect(value string) string {
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") {
		return value
	}
	return "/"
}
