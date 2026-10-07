// Package auth issues and verifies HS256 OAuth2 access tokens (JWT, RFC 7519) carrying the DxPS tenant claim.
//
// Verification is deliberately strict: only the exact header {"alg":"HS256","typ":"JWT"} family is
// accepted (no "none", no RS/HS confusion), iss/aud/exp/nbf are mandatory and the tenant claim must be
// a valid dxps_tenant value.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"dxps/internal/tenant"
)

const (
	ScopeOrderWrite = "dxps:order:write"
	ScopeOrderRead  = "dxps:order:read"
	ScopeHubWrite   = "dxps:hub:write"
	ScopeTenantAny  = "dxps:tenant:any" // host operator acting for another tenant (audited)
	ScopeAdmin      = "dxps:admin"

	MaxTokenLen = 4096
	leeway      = 30 * time.Second
)

var (
	ErrMalformed = errors.New("malformed token")
	ErrAlg       = errors.New("unsupported token algorithm")
	ErrSignature = errors.New("invalid token signature")
	ErrExpired   = errors.New("token expired")
	ErrNotYet    = errors.New("token not yet valid")
	ErrIssuer    = errors.New("invalid token issuer")
	ErrAudience  = errors.New("invalid token audience")
	ErrTenant    = errors.New("invalid tenant claim")
	ErrKey       = errors.New("signing key must be at least 32 bytes")
)

type Claims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Audience  string `json:"aud"`
	ExpiresAt int64  `json:"exp"`
	NotBefore int64  `json:"nbf"`
	IssuedAt  int64  `json:"iat"`
	ID        string `json:"jti"`
	Tenant    string `json:"tenant"`
	Scope     string `json:"scope"`
}

func (c *Claims) HasScope(s string) bool {
	for _, x := range strings.Fields(c.Scope) {
		if x == s {
			return true
		}
	}
	return false
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type Signer struct {
	key      []byte
	issuer   string
	audience string
	now      func() time.Time
}

func NewSigner(key []byte, issuer, audience string) (*Signer, error) {
	if len(key) < 32 {
		return nil, ErrKey
	}
	return &Signer{key: append([]byte(nil), key...), issuer: issuer, audience: audience, now: time.Now}, nil
}

// SetClock overrides the time source (tests).
func (s *Signer) SetClock(now func() time.Time) { s.now = now }

var b64 = base64.RawURLEncoding

func (s *Signer) Issue(subject, tenantID string, scopes []string, ttl time.Duration) (string, error) {
	if !tenant.Valid(tenantID) {
		return "", ErrTenant
	}
	jti := make([]byte, 12)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	now := s.now()
	c := Claims{
		Issuer: s.issuer, Subject: subject, Audience: s.audience,
		IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: now.Add(ttl).Unix(),
		ID: b64.EncodeToString(jti), Tenant: tenantID, Scope: strings.Join(scopes, " "),
	}
	return s.sign(header{Alg: "HS256", Typ: "JWT"}, c)
}

func (s *Signer) sign(h header, c Claims) (string, error) {
	hb, _ := json.Marshal(h)
	cb, _ := json.Marshal(c)
	signing := b64.EncodeToString(hb) + "." + b64.EncodeToString(cb)
	return signing + "." + b64.EncodeToString(s.mac(signing)), nil
}

func (s *Signer) mac(signing string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(signing))
	return m.Sum(nil)
}

func (s *Signer) Verify(tok string) (*Claims, error) {
	if len(tok) == 0 || len(tok) > MaxTokenLen {
		return nil, ErrMalformed
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[2] == "" {
		return nil, ErrMalformed
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var h header
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, ErrMalformed
	}
	if h.Alg != "HS256" || (h.Typ != "" && h.Typ != "JWT") {
		return nil, ErrAlg
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformed
	}
	if !hmac.Equal(sig, s.mac(parts[0]+"."+parts[1])) {
		return nil, ErrSignature
	}
	cb, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(cb, &c); err != nil {
		return nil, ErrMalformed
	}
	now := s.now()
	switch {
	case c.Issuer != s.issuer:
		return nil, ErrIssuer
	case c.Audience != s.audience:
		return nil, ErrAudience
	case c.ExpiresAt == 0 || now.After(time.Unix(c.ExpiresAt, 0).Add(leeway)):
		return nil, ErrExpired
	case c.NotBefore != 0 && now.Add(leeway).Before(time.Unix(c.NotBefore, 0)):
		return nil, ErrNotYet
	case !tenant.Valid(c.Tenant):
		return nil, ErrTenant
	}
	return &c, nil
}

// BearerToken extracts the token from an Authorization header value.
func BearerToken(h string) (string, bool) {
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	return strings.TrimSpace(h[len(p):]), true
}
