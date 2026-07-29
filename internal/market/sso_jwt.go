package market

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
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

type ssoJWTClaims struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Scope  string `json:"scope"`
	jwt.RegisteredClaims
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
	claims := &ssoJWTClaims{}
	parsed, err := jwt.ParseWithClaims(
		token,
		claims,
		func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodRS256 {
				return nil, errors.New("unsupported JWT alg")
			}
			return v.publicKey, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(func() time.Time { return now }),
		jwt.WithLeeway(5*time.Second),
	)
	if err != nil || !parsed.Valid {
		if err == nil {
			err = errors.New("invalid JWT")
		}
		return ssoJWTPrincipal{}, err
	}
	if claims.IssuedAt == nil {
		return ssoJWTPrincipal{}, errors.New("missing iat")
	}
	if strings.TrimSpace(claims.UserID) == "" {
		return ssoJWTPrincipal{}, errors.New("missing user_id")
	}
	return ssoJWTPrincipal{
		UserID: strings.TrimSpace(claims.UserID),
		Email:  strings.TrimSpace(claims.Email),
		Role:   strings.ToLower(strings.TrimSpace(claims.Role)),
		Scope:  strings.TrimSpace(claims.Scope),
	}, nil
}
