package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var key = []byte("0123456789abcdef0123456789abcdef")

func signer(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(key, "https://auth.dxps.local", "dxps-api")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func enc(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }

// TC-AUTH-001: A token issued by DxPS verifies and carries tenant, subject and scopes.
func TestIssueVerifyRoundTrip(t *testing.T) {
	s := signer(t)
	tok, err := s.Issue("crm-app", "mvno-alpha", []string{ScopeOrderWrite, ScopeOrderRead}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tenant != "mvno-alpha" || c.Subject != "crm-app" || !c.HasScope(ScopeOrderWrite) || c.HasScope(ScopeTenantAny) || c.ID == "" {
		t.Fatalf("unexpected claims %+v", c)
	}
}

// TC-AUTH-002: Signing keys shorter than 256 bits are rejected.
func TestShortKeyRejected(t *testing.T) {
	if _, err := NewSigner([]byte("short"), "i", "a"); !errors.Is(err, ErrKey) {
		t.Fatalf("got %v", err)
	}
}

// TC-AUTH-003: Tokens cannot be issued for an invalid tenant identifier (SQL/charset injection).
func TestIssueInvalidTenant(t *testing.T) {
	for _, ten := range []string{"", "a b", "x'; DROP TABLE t;--", strings.Repeat("a", 41)} {
		if _, err := signer(t).Issue("s", ten, nil, time.Minute); !errors.Is(err, ErrTenant) {
			t.Fatalf("%q: got %v", ten, err)
		}
	}
}

// TC-AUTH-004: Algorithm confusion is blocked (alg=none, RS256, HS512, wrong typ).
func TestAlgorithmConfusion(t *testing.T) {
	s := signer(t)
	tok, _ := s.Issue("s", "host-mno", nil, time.Minute)
	p := strings.Split(tok, ".")
	for _, h := range []map[string]string{{"alg": "none", "typ": "JWT"}, {"alg": "RS256", "typ": "JWT"}, {"alg": "HS512"}, {"alg": "HS256", "typ": "JWE"}} {
		forged := enc(h) + "." + p[1] + "." + p[2]
		if _, err := s.Verify(forged); !errors.Is(err, ErrAlg) {
			t.Fatalf("%v: got %v", h, err)
		}
	}
	if _, err := s.Verify(enc(map[string]string{"alg": "none"}) + "." + p[1] + "."); !errors.Is(err, ErrMalformed) {
		t.Fatalf("unsigned token: got %v", err)
	}
}

// TC-AUTH-005: Any modification of the claims (e.g. tenant swap) invalidates the signature.
func TestTamperedClaims(t *testing.T) {
	s := signer(t)
	tok, _ := s.Issue("s", "mvno-beta", []string{ScopeOrderRead}, time.Minute)
	p := strings.Split(tok, ".")
	b, _ := base64.RawURLEncoding.DecodeString(p[1])
	var c map[string]any
	_ = json.Unmarshal(b, &c)
	c["tenant"] = "host-mno"
	if _, err := s.Verify(p[0] + "." + enc(c) + "." + p[2]); !errors.Is(err, ErrSignature) {
		t.Fatalf("got %v", err)
	}
	other, _ := NewSigner([]byte("ffffffffffffffffffffffffffffffff"), "https://auth.dxps.local", "dxps-api")
	foreign, _ := other.Issue("s", "mvno-beta", nil, time.Minute)
	if _, err := s.Verify(foreign); !errors.Is(err, ErrSignature) {
		t.Fatalf("foreign key: got %v", err)
	}
}

// TC-AUTH-006: Expiry and not-before are enforced with a 30 s leeway.
func TestTimeClaims(t *testing.T) {
	s := signer(t)
	base := time.Unix(1_800_000_000, 0)
	s.SetClock(func() time.Time { return base })
	tok, _ := s.Issue("s", "host-mno", nil, time.Minute)
	for _, tc := range []struct {
		at   time.Duration
		want error
	}{{0, nil}, {89 * time.Second, nil}, {91 * time.Second, ErrExpired}, {-29 * time.Second, nil}, {-31 * time.Second, ErrNotYet}} {
		s.SetClock(func() time.Time { return base.Add(tc.at) })
		if _, err := s.Verify(tok); !errors.Is(err, tc.want) {
			t.Fatalf("at %v: got %v want %v", tc.at, err, tc.want)
		}
	}
}

// TC-AUTH-007: Issuer, audience, missing exp and invalid tenant claims are rejected.
func TestClaimValidation(t *testing.T) {
	s := signer(t)
	now := time.Now().Unix()
	good := Claims{Issuer: "https://auth.dxps.local", Audience: "dxps-api", ExpiresAt: now + 60, Tenant: "host-mno"}
	cases := map[string]struct {
		mut  func(*Claims)
		want error
	}{
		"issuer":   {func(c *Claims) { c.Issuer = "https://evil" }, ErrIssuer},
		"audience": {func(c *Claims) { c.Audience = "other" }, ErrAudience},
		"no exp":   {func(c *Claims) { c.ExpiresAt = 0 }, ErrExpired},
		"tenant":   {func(c *Claims) { c.Tenant = "bad tenant" }, ErrTenant},
		"ok":       {func(*Claims) {}, nil},
	}
	for name, tc := range cases {
		c := good
		tc.mut(&c)
		tok, _ := s.sign(header{Alg: "HS256", Typ: "JWT"}, c)
		if _, err := s.Verify(tok); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

// TC-AUTH-008: Malformed tokens (empty, oversized, wrong segment count, bad base64/JSON) are rejected.
func TestMalformed(t *testing.T) {
	s := signer(t)
	tok, _ := s.Issue("s", "host-mno", nil, time.Minute)
	p := strings.Split(tok, ".")
	for _, bad := range []string{"", strings.Repeat("a", MaxTokenLen+1), "a.b", "a.b.c.d", "!!!." + p[1] + "." + p[2],
		enc("x")[:3] + "." + p[1] + "." + p[2], p[0] + "." + p[1] + ".***", p[0] + ".!!!." + p[2]} {
		if _, err := s.Verify(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	hb := base64.RawURLEncoding.EncodeToString([]byte("{not json"))
	if _, err := s.Verify(hb + "." + p[1] + "." + p[2]); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad header json: %v", err)
	}
	sig := s.mac(p[0] + ".bm90IGpzb24")
	if _, err := s.Verify(p[0] + ".bm90IGpzb24." + base64.RawURLEncoding.EncodeToString(sig)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad claims json: %v", err)
	}
	sig = s.mac(p[0] + ".!!")
	if _, err := s.Verify(p[0] + ".!!." + base64.RawURLEncoding.EncodeToString(sig)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad claims b64: %v", err)
	}
}

// TC-AUTH-009: Bearer extraction is case-insensitive on the scheme and rejects other schemes.
func TestBearerToken(t *testing.T) {
	for in, want := range map[string]string{"Bearer abc": "abc", "bearer  abc ": "abc", "Basic abc": "", "Bearer ": "", "": ""} {
		got, ok := BearerToken(in)
		if got != want || ok != (want != "") {
			t.Fatalf("%q: got %q %v", in, got, ok)
		}
	}
}
