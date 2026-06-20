package market

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	errSSOJWTNotConfigured = errors.New("SSO JWT verifier is not configured")
	errBearerTokenMissing  = errors.New("bearer token is missing")
)

type ssoJWTPrincipal struct {
	UserID string
	Email  string
	Role   string
	Scope  string
}

type ssoJWTVerifier struct {
	issuer    string
	audience  string
	publicKey *rsa.PublicKey
}

func newSSOJWTVerifier(cfg Config) (*ssoJWTVerifier, error) {
	issuer := strings.TrimSpace(cfg.SSOJWTIssuer)
	audience := strings.TrimSpace(cfg.SSOJWTAudience)
	if issuer == "" {
		return nil, nil
	}
	if audience == "" {
		return nil, errors.New("SSO_JWT_AUDIENCE is required")
	}
	publicKey, configured, err := loadSSOJWTPublicKey(cfg.SSOJWTPublicKeyFile, cfg.SSOJWTPublicKeyPEM)
	if err != nil {
		return nil, err
	}
	if !configured {
		return nil, errors.New("SSO_JWT_PUBLIC_KEY_FILE or SSO_JWT_PUBLIC_KEY_PEM is required")
	}
	return &ssoJWTVerifier{
		issuer:    issuer,
		audience:  audience,
		publicKey: publicKey,
	}, nil
}

func (p ssoJWTPrincipal) HasScope(scope string) bool {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return false
	}
	for _, item := range strings.Fields(p.Scope) {
		if item == scope {
			return true
		}
	}
	return false
}

func loadSSOJWTPublicKey(filePath, pemValue string) (*rsa.PublicKey, bool, error) {
	filePath = strings.TrimSpace(filePath)
	pemValue = strings.TrimSpace(pemValue)
	if filePath != "" {
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, true, err
		}
		key, err := parseSSOJWTPublicKeyPEM(string(content))
		return key, true, err
	}
	if pemValue == "" {
		return nil, false, nil
	}
	key, err := parseSSOJWTPublicKeyPEM(strings.ReplaceAll(pemValue, `\n`, "\n"))
	return key, true, err
}

func parseSSOJWTPublicKeyPEM(value string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(value)))
	if block == nil {
		return nil, errors.New("SSO JWT public key PEM is invalid")
	}
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if publicKey, ok := key.(*rsa.PublicKey); ok {
			return publicKey, nil
		}
		return nil, errors.New("SSO JWT public key must be RSA")
	}
	publicKey, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return publicKey, nil
}

func (v *ssoJWTVerifier) principalFromRequest(r *http.Request) (ssoJWTPrincipal, bool) {
	principal, err := v.verifyBearerHeader(r.Header.Get("Authorization"))
	return principal, err == nil
}

func (v *ssoJWTVerifier) verifyBearerHeader(header string) (ssoJWTPrincipal, error) {
	if v == nil {
		return ssoJWTPrincipal{}, errSSOJWTNotConfigured
	}
	token := bearerToken(header)
	if token == "" {
		return ssoJWTPrincipal{}, errBearerTokenMissing
	}
	return v.verify(token, time.Now())
}

func bearerToken(header string) string {
	fields := strings.Fields(strings.TrimSpace(header))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(fields[1])
}

func (v *ssoJWTVerifier) verify(token string, now time.Time) (ssoJWTPrincipal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ssoJWTPrincipal{}, errors.New("invalid JWT")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ssoJWTPrincipal{}, err
	}
	var header map[string]any
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return ssoJWTPrincipal{}, err
	}
	if header["alg"] != "RS256" {
		return ssoJWTPrincipal{}, errors.New("unsupported JWT alg")
	}
	signedValue := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ssoJWTPrincipal{}, err
	}
	digest := sha256.Sum256([]byte(signedValue))
	if err := rsa.VerifyPKCS1v15(v.publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return ssoJWTPrincipal{}, err
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ssoJWTPrincipal{}, err
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return ssoJWTPrincipal{}, err
	}
	if readStringClaim(claims, "iss") != v.issuer {
		return ssoJWTPrincipal{}, errors.New("issuer mismatch")
	}
	if !claimHasAudience(claims["aud"], v.audience) {
		return ssoJWTPrincipal{}, errors.New("audience mismatch")
	}
	exp := readNumberClaim(claims, "exp")
	if exp <= 0 || exp <= now.Unix() {
		return ssoJWTPrincipal{}, errors.New("token expired")
	}
	userID := readStringClaim(claims, "user_id")
	if userID == "" {
		return ssoJWTPrincipal{}, errors.New("missing user_id")
	}
	return ssoJWTPrincipal{
		UserID: userID,
		Email:  readStringClaim(claims, "email"),
		Role:   strings.ToLower(readStringClaim(claims, "role")),
		Scope:  readStringClaim(claims, "scope"),
	}, nil
}

func readStringClaim(claims map[string]any, name string) string {
	value, _ := claims[name].(string)
	return strings.TrimSpace(value)
}

func readNumberClaim(claims map[string]any, name string) int64 {
	switch value := claims[name].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	default:
		return 0
	}
}

func claimHasAudience(value any, audience string) bool {
	switch typed := value.(type) {
	case string:
		return typed == audience
	case []any:
		for _, item := range typed {
			if item == audience {
				return true
			}
		}
	}
	return false
}
