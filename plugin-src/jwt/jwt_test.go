package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestGenerateVerifyDefaultsAndOverrides(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	s := newService()
	s.now = func() time.Time { return clock }
	result, err := s.generate(t.Context(), json.RawMessage(`{"sub":"claim-subject","role":"admin","request_id":9007199254740993}`), generateOptions{
		Issuer: "issuer", Subject: "option-subject", Audience: stringList{"api", "other"}, TTL: "10m",
	})
	if err != nil {
		t.Fatalf("generate() error = %v", err)
	}
	if result.Claims["sub"] != "claim-subject" || result.Claims["role"] != "admin" {
		t.Fatalf("claims = %#v", result.Claims)
	}
	if result.Claims["request_id"] != json.Number("9007199254740993") {
		t.Fatalf("large claim = %#v", result.Claims["request_id"])
	}
	if result.Claims["iat"] != clock.Unix() || result.Claims["exp"] != clock.Add(10*time.Minute).Unix() {
		t.Fatalf("registered claims = %#v", result.Claims)
	}
	verified, err := s.verify(t.Context(), result.Token, verifyOptions{
		Issuer: "issuer", Subject: "claim-subject", Audience: stringList{"missing", "api"},
	})
	if err != nil {
		t.Fatalf("verify() error = %v", err)
	}
	if verified.Header["alg"] != "RS256" || verified.Claims["role"] != "admin" {
		t.Fatalf("verified = %#v", verified)
	}
}

func TestCommandKeyIsStableAndJWTIDChanges(t *testing.T) {
	s := newService()
	one, err := s.generate(t.Context(), nil, generateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.generate(t.Context(), nil, generateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if one.Header["kid"] != two.Header["kid"] {
		t.Fatalf("kids differ: %q != %q", one.Header["kid"], two.Header["kid"])
	}
	if one.Claims["jti"] == two.Claims["jti"] {
		t.Fatal("generated JWT IDs are equal")
	}
	if _, err := s.verify(t.Context(), one.Token, verifyOptions{}); err != nil {
		t.Fatalf("verify first token: %v", err)
	}
}

func TestCommandKeyCustomKidStillVerifiesLocally(t *testing.T) {
	s := newService()
	result, err := s.generate(t.Context(), nil, generateOptions{Kid: "local-key"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Header["kid"] != "local-key" {
		t.Fatalf("kid = %#v", result.Header["kid"])
	}
	if _, err := s.verify(t.Context(), result.Token, verifyOptions{}); err != nil {
		t.Fatalf("verify() error = %v", err)
	}
}

func TestCustomSigningJWKAndPublicProjection(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw := privateJWK(t, private, "custom")
	s := newService()
	result, err := s.generate(t.Context(), nil, generateOptions{SigningJWK: raw, Kid: "custom"})
	if err != nil {
		t.Fatalf("generate() error = %v", err)
	}
	projected, err := s.jwks(t.Context(), raw)
	if err != nil {
		t.Fatalf("jwks() error = %v", err)
	}
	encoded, _ := json.Marshal(projected)
	for _, privateName := range []string{`"d"`, `"p"`, `"q"`, `"dp"`, `"dq"`, `"qi"`} {
		if strings.Contains(string(encoded), privateName) {
			t.Fatalf("public JWKS contains private member %s: %s", privateName, encoded)
		}
	}
	verificationJSON, _ := json.Marshal(projected)
	if _, err := s.verify(t.Context(), result.Token, verifyOptions{JWKS: verificationJSON}); err != nil {
		t.Fatalf("verify() with projected JWKS error = %v", err)
	}
}

func TestVerifyRejectsInvalidTokensAndClaims(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	s := newService()
	s.now = func() time.Time { return clock }
	expired, err := s.generate(t.Context(), json.RawMessage(`{"exp":1799999999}`), generateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.verify(t.Context(), expired.Token, verifyOptions{}); !isTokenError(err) {
		t.Fatalf("expired error = %T %v", err, err)
	}
	missingExp, err := s.generate(t.Context(), json.RawMessage(`{"exp":null}`), generateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.verify(t.Context(), missingExp.Token, verifyOptions{}); !isTokenError(err) {
		t.Fatalf("invalid exp error = %T %v", err, err)
	}
	parts := strings.Split(expired.Token, ".")
	parts[2] = strings.Repeat("A", len(parts[2]))
	if _, err := s.verify(t.Context(), strings.Join(parts, "."), verifyOptions{}); !isTokenError(err) {
		t.Fatalf("signature error = %T %v", err, err)
	}
	if _, err := s.verify(t.Context(), "not-a-jwt", verifyOptions{}); !isTokenError(err) {
		t.Fatalf("malformed error = %T %v", err, err)
	}
}

func TestRequiredClaimsCanBeExplicitlyEmpty(t *testing.T) {
	s := newService()
	result, err := s.generate(t.Context(), json.RawMessage(`{"exp":null}`), generateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	options := verifyOptions{RequiredClaims: []string{}, requiredSet: true}
	if _, err := s.verify(t.Context(), result.Token, options); !isTokenError(err) {
		// exp is present but invalid, so disabling required claims must not disable validation
		// of a registered claim that is actually present.
		t.Fatalf("invalid present exp error = %T %v", err, err)
	}
	deleteClaims := json.RawMessage(`{"scope":"read"}`)
	key, kid, err := s.commandKey(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	token := signClaims(t, key, kid, deleteClaims)
	if _, err := s.verify(t.Context(), token, options); err != nil {
		t.Fatalf("verify without exp and empty required claims: %v", err)
	}
}

func TestDuplicateJSONAndWeakKeysAreRejected(t *testing.T) {
	if _, err := decodeObject([]byte(`{"a":1,"a":2}`)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate JSON error = %v", err)
	}
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseJWKInput(privateJWK(t, weak, "weak"), jwkSign); err == nil || !strings.Contains(err.Error(), "2048") {
		t.Fatalf("weak key error = %v", err)
	}
}

func TestGenerateRejectsOversizedCompactToken(t *testing.T) {
	s := newService()
	claims, err := json.Marshal(map[string]any{"data": strings.Repeat("x", 800_000)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.generate(t.Context(), claims, generateOptions{}); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("oversized token error = %v", err)
	}
}

func TestNumericDateFraction(t *testing.T) {
	nanos, err := numericDateNanos("exp", json.Number("1800000000.125"))
	if err != nil {
		t.Fatal(err)
	}
	want := big.NewInt(1_800_000_000_125_000_000)
	if nanos.Cmp(want) != 0 {
		t.Fatalf("nanos = %s, want %s", nanos, want)
	}
}

func TestNumericDateBeforeUnixEpochIsValidSyntax(t *testing.T) {
	nanos, err := numericDateNanos("iat", json.Number("-0.5"))
	if err != nil {
		t.Fatal(err)
	}
	if nanos.Cmp(big.NewInt(-500_000_000)) != 0 {
		t.Fatalf("nanos = %s", nanos)
	}
}

func privateJWK(t *testing.T, key *rsa.PrivateKey, kid string) json.RawMessage {
	t.Helper()
	key.Precompute()
	value := map[string]any{
		"kty": "RSA", "n": encodeBigInt(key.N), "e": encodeBigInt(big.NewInt(int64(key.E))),
		"d": encodeBigInt(key.D), "p": encodeBigInt(key.Primes[0]), "q": encodeBigInt(key.Primes[1]),
		"dp": encodeBigInt(key.Precomputed.Dp), "dq": encodeBigInt(key.Precomputed.Dq),
		"qi": encodeBigInt(key.Precomputed.Qinv), "kid": kid, "alg": "RS256", "use": "sig",
		"extension": "ignored",
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func signClaims(t *testing.T, key *rsa.PrivateKey, kid string, claims json.RawMessage) string {
	t.Helper()
	// generate supplies defaults, so sign the desired raw claim set directly.
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": kid})
	unsigned := rawURL.EncodeToString(header) + "." + rawURL.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + rawURL.EncodeToString(signature)
}

func TestSignOnlyJWKCanBeProjectedButNotUsedForVerification(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(privateJWK(t, private, "signer"), &value); err != nil {
		t.Fatal(err)
	}
	value["key_ops"] = []string{"sign"}
	raw, _ := json.Marshal(value)
	s := newService()
	result, err := s.generate(t.Context(), nil, generateOptions{SigningJWK: raw})
	if err != nil {
		t.Fatalf("generate() error = %v", err)
	}
	projected, err := s.jwks(t.Context(), raw)
	if err != nil {
		t.Fatalf("jwks() error = %v", err)
	}
	encoded, _ := json.Marshal(projected)
	if _, err := s.verify(t.Context(), result.Token, verifyOptions{JWKS: encoded}); err != nil {
		t.Fatalf("verify() with projected JWKS error = %v", err)
	}
	if _, err := s.verify(t.Context(), result.Token, verifyOptions{JWKS: raw}); err == nil || !strings.Contains(err.Error(), "verify") {
		t.Fatalf("sign-only verification key error = %v", err)
	}
}

func TestGenerateAcceptsClaimsAsJSONString(t *testing.T) {
	s := newService()
	result, err := s.generate(t.Context(), json.RawMessage(`"{\"role\":\"admin\"}"`), generateOptions{})
	if err != nil {
		t.Fatalf("generate() error = %v", err)
	}
	if result.Claims["role"] != "admin" {
		t.Fatalf("claims = %#v", result.Claims)
	}
	if _, err := s.generate(t.Context(), json.RawMessage(`"[1]"`), generateOptions{}); err == nil {
		t.Fatal("non-object JSON string claims were accepted")
	}
}

func TestVerifyRejectsCriticalHeader(t *testing.T) {
	s := newService()
	key, kid, err := s.commandKey(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": kid, "crit": []string{"exp"}, "exp": 1})
	unsigned := rawURL.EncodeToString(header) + "." + rawURL.EncodeToString([]byte(`{"exp":99999999999}`))
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	token := unsigned + "." + rawURL.EncodeToString(signature)
	if _, err := s.verify(t.Context(), token, verifyOptions{}); !isTokenError(err) {
		t.Fatalf("crit header error = %T %v", err, err)
	}
}
