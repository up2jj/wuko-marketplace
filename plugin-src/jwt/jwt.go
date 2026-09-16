package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

const (
	defaultTTL  = 5 * time.Minute
	maxTokenLen = 1 << 20
)

type tokenError struct{ message string }

func (e tokenError) Error() string { return e.message }

func tokenErrorf(format string, args ...any) error {
	return tokenError{message: fmt.Sprintf(format, args...)}
}

func isTokenError(err error) bool {
	var target tokenError
	return errors.As(err, &target)
}

type stringList []string

func (s *stringList) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*s = stringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("must be a string or array of strings")
	}
	*s = many
	return nil
}

type generateOptions struct {
	Issuer     string          `json:"issuer"`
	Audience   stringList      `json:"audience"`
	Subject    string          `json:"subject"`
	TTL        string          `json:"ttl"`
	SigningJWK json.RawMessage `json:"signing_jwk"`
	Kid        string          `json:"kid"`
}

type verifyOptions struct {
	JWKS           json.RawMessage `json:"jwks"`
	JWKSURL        string          `json:"jwks_url"`
	Issuer         string          `json:"issuer"`
	Audience       stringList      `json:"audience"`
	Subject        string          `json:"subject"`
	Leeway         string          `json:"leeway"`
	RequiredClaims []string        `json:"required_claims"`
	requiredSet    bool
}

type generatedToken struct {
	Token  string
	Header map[string]any
	Claims map[string]any
	JWKS   map[string]any
}

type verifiedToken struct {
	Header map[string]any
	Claims map[string]any
}

type service struct {
	now func() time.Time

	keyMu sync.Mutex
	key   *rsa.PrivateKey
	kid   string

	remote *remoteCache
}

func newService() *service {
	return &service{now: time.Now, remote: newRemoteCache()}
}

func (s *service) commandKey(ctx context.Context) (*rsa.PrivateKey, string, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.key != nil {
		return s.key, s.kid, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", fmt.Errorf("generating RSA signing key: %w", err)
	}
	kid, err := thumbprint(&key.PublicKey)
	if err != nil {
		return nil, "", fmt.Errorf("deriving signing key identifier: %w", err)
	}
	s.key, s.kid = key, kid
	return key, kid, nil
}

func (s *service) generate(ctx context.Context, claimData json.RawMessage, options generateOptions) (generatedToken, error) {
	if err := validateAudience(options.Audience); err != nil {
		return generatedToken{}, err
	}
	ttl := defaultTTL
	if options.TTL != "" {
		parsed, err := time.ParseDuration(options.TTL)
		if err != nil || parsed <= 0 {
			return generatedToken{}, fmt.Errorf("ttl must be a positive duration")
		}
		ttl = parsed
	}
	now := s.now().UTC()
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return generatedToken{}, fmt.Errorf("generating JWT ID: %w", err)
	}
	claims := map[string]any{
		"iat": now.Unix(), "exp": now.Add(ttl).Unix(), "jti": rawURL.EncodeToString(jtiBytes),
	}
	if options.Issuer != "" {
		claims["iss"] = options.Issuer
	}
	if options.Subject != "" {
		claims["sub"] = options.Subject
	}
	if len(options.Audience) == 1 {
		claims["aud"] = options.Audience[0]
	} else if len(options.Audience) > 1 {
		claims["aud"] = []string(options.Audience)
	}
	if len(claimData) > 0 && string(claimData) != "null" {
		custom, err := decodeObjectOrString(claimData)
		if err != nil {
			return generatedToken{}, fmt.Errorf("claims must be a JSON object: %w", err)
		}
		for name, value := range custom {
			claims[name] = value
		}
	}
	key, kid, err := s.signingKey(ctx, options.SigningJWK, options.Kid)
	if err != nil {
		return generatedToken{}, err
	}
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return generatedToken{}, fmt.Errorf("encoding JWT header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return generatedToken{}, fmt.Errorf("encoding JWT claims: %w", err)
	}
	unsigned := rawURL.EncodeToString(headerJSON) + "." + rawURL.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return generatedToken{}, fmt.Errorf("signing JWT: %w", err)
	}
	token := unsigned + "." + rawURL.EncodeToString(signature)
	if len(token) > maxTokenLen {
		return generatedToken{}, fmt.Errorf("generated JWT exceeds 1 MiB")
	}
	return generatedToken{
		Token: token, Header: header, Claims: claims,
		JWKS: map[string]any{"keys": []any{publicJWK(&key.PublicKey, kid)}},
	}, nil
}

func (s *service) signingKey(ctx context.Context, raw json.RawMessage, requestedKid string) (*rsa.PrivateKey, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		key, kid, err := s.commandKey(ctx)
		if err != nil {
			return nil, "", err
		}
		if requestedKid != "" {
			kid = requestedKid
		}
		return key, kid, nil
	}
	keys, err := parseJWKInput(raw, jwkSign)
	if err != nil {
		return nil, "", fmt.Errorf("invalid signing_jwk: %w", err)
	}
	selected, err := selectSigningJWK(keys, requestedKid)
	if err != nil {
		return nil, "", err
	}
	kid := selected.kid
	if kid == "" {
		kid, err = thumbprint(selected.public)
		if err != nil {
			return nil, "", fmt.Errorf("deriving signing key identifier: %w", err)
		}
	}
	return selected.private, kid, nil
}

func selectSigningJWK(keys []rsaJWK, requestedKid string) (rsaJWK, error) {
	if requestedKid != "" {
		for _, key := range keys {
			if key.kid == requestedKid {
				return key, nil
			}
		}
		if len(keys) == 1 && keys[0].kid == "" {
			keys[0].kid = requestedKid
			return keys[0], nil
		}
		return rsaJWK{}, fmt.Errorf("no signing JWK matches kid")
	}
	if len(keys) != 1 {
		return rsaJWK{}, fmt.Errorf("kid is required when signing_jwk contains multiple keys")
	}
	return keys[0], nil
}

func (s *service) jwks(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		key, kid, err := s.commandKey(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"keys": []any{publicJWK(&key.PublicKey, kid)}}, nil
	}
	keys, err := parseJWKInput(raw, jwkPublish)
	if err != nil {
		return nil, fmt.Errorf("invalid signing_jwk: %w", err)
	}
	public := make([]any, 0, len(keys))
	for _, key := range keys {
		kid := key.kid
		if kid == "" {
			kid, err = thumbprint(key.public)
			if err != nil {
				return nil, fmt.Errorf("deriving signing key identifier: %w", err)
			}
		}
		public = append(public, publicJWK(key.public, kid))
	}
	return map[string]any{"keys": public}, nil
}

func (s *service) verify(ctx context.Context, token string, options verifyOptions) (verifiedToken, error) {
	if len(token) == 0 || len(token) > maxTokenLen {
		return verifiedToken{}, tokenErrorf("JWT must be non-empty and no larger than 1 MiB")
	}
	if len(options.JWKS) > 0 && string(options.JWKS) != "null" && options.JWKSURL != "" {
		return verifiedToken{}, fmt.Errorf("jwks and jwks_url are mutually exclusive")
	}
	leeway, err := parseNonnegativeDuration("leeway", options.Leeway)
	if err != nil {
		return verifiedToken{}, err
	}
	header, claims, unsigned, signature, err := parseToken(token)
	if err != nil {
		return verifiedToken{}, err
	}
	alg, ok := header["alg"].(string)
	if !ok || alg != "RS256" {
		return verifiedToken{}, tokenErrorf("JWT alg must be RS256")
	}
	// RFC 7515 section 4.1.11: no critical extensions are understood, so any crit must be rejected.
	if _, exists := header["crit"]; exists {
		return verifiedToken{}, tokenErrorf("JWT crit header parameters are not supported")
	}
	kid, _ := header["kid"].(string)
	if value, exists := header["kid"]; exists {
		if _, ok := value.(string); !ok || kid == "" {
			return verifiedToken{}, tokenErrorf("JWT kid must be a non-empty string when present")
		}
	}
	keys, remote, err := s.verificationKeys(ctx, options)
	if err != nil {
		return verifiedToken{}, err
	}
	usesCommandKey := options.JWKSURL == "" && (len(options.JWKS) == 0 || string(options.JWKS) == "null")
	var selected rsaJWK
	if usesCommandKey {
		selected = keys[0]
	} else {
		selected, err = selectJWK(keys, kid, false)
	}
	if err != nil && remote {
		keys, fetchErr := s.remote.fetch(ctx, options.JWKSURL, true)
		if fetchErr != nil {
			return verifiedToken{}, fetchErr
		}
		selected, err = selectJWK(keys, kid, false)
	}
	if err != nil {
		return verifiedToken{}, err
	}
	digest := sha256.Sum256([]byte(unsigned))
	if err := rsa.VerifyPKCS1v15(selected.public, crypto.SHA256, digest[:], signature); err != nil {
		return verifiedToken{}, tokenErrorf("JWT signature is invalid")
	}
	if err := validateClaims(claims, options, s.now(), leeway); err != nil {
		return verifiedToken{}, err
	}
	return verifiedToken{Header: header, Claims: claims}, nil
}

func (s *service) verificationKeys(ctx context.Context, options verifyOptions) ([]rsaJWK, bool, error) {
	if options.JWKSURL != "" {
		keys, err := s.remote.fetch(ctx, options.JWKSURL, false)
		return keys, true, err
	}
	if len(options.JWKS) > 0 && string(options.JWKS) != "null" {
		keys, err := parseJWKInput(options.JWKS, jwkVerify)
		if err != nil {
			return nil, false, fmt.Errorf("invalid jwks: %w", err)
		}
		return keys, false, nil
	}
	key, kid, err := s.commandKey(ctx)
	if err != nil {
		return nil, false, err
	}
	return []rsaJWK{{public: &key.PublicKey, kid: kid}}, false, nil
}

func parseToken(token string) (map[string]any, map[string]any, string, []byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, nil, "", nil, tokenErrorf("JWT must contain three non-empty segments")
	}
	decoded := make([][]byte, 3)
	for index, part := range parts {
		value, err := rawURL.DecodeString(part)
		if err != nil || rawURL.EncodeToString(value) != part {
			return nil, nil, "", nil, tokenErrorf("JWT contains invalid base64url")
		}
		decoded[index] = value
	}
	header, err := decodeObject(decoded[0])
	if err != nil {
		return nil, nil, "", nil, tokenErrorf("JWT header is invalid: %v", err)
	}
	claims, err := decodeObject(decoded[1])
	if err != nil {
		return nil, nil, "", nil, tokenErrorf("JWT claims are invalid: %v", err)
	}
	return header, claims, parts[0] + "." + parts[1], decoded[2], nil
}

func parseNonnegativeDuration(name, value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration", name)
	}
	return parsed, nil
}

func validateClaims(claims map[string]any, options verifyOptions, now time.Time, leeway time.Duration) error {
	required := options.RequiredClaims
	if !options.requiredSet {
		required = []string{"exp"}
	}
	for _, name := range required {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("required_claims must not contain empty names")
		}
		if _, exists := claims[name]; !exists {
			return tokenErrorf("JWT is missing required claim %q", name)
		}
	}
	nowNanos := big.NewInt(now.UnixNano())
	leewayNanos := big.NewInt(leeway.Nanoseconds())
	if value, exists := claims["exp"]; exists {
		exp, err := numericDateNanos("exp", value)
		if err != nil {
			return err
		}
		threshold := new(big.Int).Sub(nowNanos, leewayNanos)
		if exp.Cmp(threshold) <= 0 {
			return tokenErrorf("JWT has expired")
		}
	}
	if value, exists := claims["nbf"]; exists {
		nbf, err := numericDateNanos("nbf", value)
		if err != nil {
			return err
		}
		threshold := new(big.Int).Add(nowNanos, leewayNanos)
		if nbf.Cmp(threshold) > 0 {
			return tokenErrorf("JWT is not valid yet")
		}
	}
	if value, exists := claims["iat"]; exists {
		iat, err := numericDateNanos("iat", value)
		if err != nil {
			return err
		}
		threshold := new(big.Int).Add(nowNanos, leewayNanos)
		if iat.Cmp(threshold) > 0 {
			return tokenErrorf("JWT was issued in the future")
		}
	}
	if options.Issuer != "" && claims["iss"] != options.Issuer {
		return tokenErrorf("JWT issuer does not match")
	}
	if options.Subject != "" && claims["sub"] != options.Subject {
		return tokenErrorf("JWT subject does not match")
	}
	if len(options.Audience) > 0 && !audienceMatches(claims["aud"], options.Audience) {
		return tokenErrorf("JWT audience does not match")
	}
	return nil
}

func numericDateNanos(name string, value any) (*big.Int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return nil, tokenErrorf("JWT claim %q must be a number", name)
	}
	rat := new(big.Rat)
	if _, ok := rat.SetString(number.String()); !ok {
		return nil, tokenErrorf("JWT claim %q is not a valid NumericDate", name)
	}
	rat.Mul(rat, big.NewRat(int64(time.Second), 1))
	if !rat.IsInt() {
		return nil, tokenErrorf("JWT claim %q has sub-nanosecond precision", name)
	}
	return rat.Num(), nil
}

func audienceMatches(value any, wanted []string) bool {
	available := make(map[string]struct{})
	switch current := value.(type) {
	case string:
		available[current] = struct{}{}
	case []any:
		for _, item := range current {
			text, ok := item.(string)
			if !ok {
				return false
			}
			available[text] = struct{}{}
		}
	default:
		return false
	}
	for _, candidate := range wanted {
		if _, exists := available[candidate]; exists {
			return true
		}
	}
	return false
}
