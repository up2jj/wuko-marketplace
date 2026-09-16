package main

import (
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
)

const maxJWKSetKeys = 100

var rawURL = base64.RawURLEncoding

type jwk struct {
	Kty    string          `json:"kty"`
	N      string          `json:"n"`
	E      string          `json:"e"`
	D      string          `json:"d,omitempty"`
	P      string          `json:"p,omitempty"`
	Q      string          `json:"q,omitempty"`
	DP     string          `json:"dp,omitempty"`
	DQ     string          `json:"dq,omitempty"`
	QI     string          `json:"qi,omitempty"`
	Oth    json.RawMessage `json:"oth,omitempty"`
	Kid    string          `json:"kid,omitempty"`
	Use    string          `json:"use,omitempty"`
	Alg    string          `json:"alg,omitempty"`
	KeyOps []string        `json:"key_ops,omitempty"`
}

type jwkSet struct {
	Keys []json.RawMessage `json:"keys"`
}

// jwkPurpose describes how parsed key material will be used, which determines the
// required parameters and the key_ops value that must be permitted.
type jwkPurpose int

const (
	// jwkVerify accepts public or private keys whose key_ops permit verify.
	jwkVerify jwkPurpose = iota
	// jwkSign requires private keys whose key_ops permit sign.
	jwkSign
	// jwkPublish accepts public or private keys whose key_ops permit sign or verify,
	// because it only projects the public half of caller-owned signing material.
	jwkPublish
)

type rsaJWK struct {
	public  *rsa.PublicKey
	private *rsa.PrivateKey
	kid     string
}

func parseJWKInput(raw json.RawMessage, purpose jwkPurpose) ([]rsaJWK, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if len(raw) > maxDocumentSize {
		return nil, fmt.Errorf("JWK document exceeds 1 MiB")
	}
	var encoded string
	if raw[0] == '"' {
		if err := decodeStrict(raw, &encoded); err != nil {
			return nil, fmt.Errorf("invalid JWK JSON string: %w", err)
		}
		raw = json.RawMessage(encoded)
	}
	object, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid JWK document: %w", err)
	}
	var entries []json.RawMessage
	if _, isSet := object["keys"]; isSet {
		var set jwkSet
		if err := decodeJSON(raw, &set); err != nil {
			return nil, fmt.Errorf("invalid JWKS: %w", err)
		}
		entries = set.Keys
	} else {
		entries = []json.RawMessage{raw}
	}
	if len(entries) == 0 || len(entries) > maxJWKSetKeys {
		return nil, fmt.Errorf("JWK set must contain between 1 and %d keys", maxJWKSetKeys)
	}
	result := make([]rsaJWK, 0, len(entries))
	seenKids := make(map[string]struct{})
	for index, entry := range entries {
		parsed, err := parseRSAJWK(entry, purpose)
		if err != nil {
			return nil, fmt.Errorf("invalid JWK at index %d: %w", index, err)
		}
		if parsed.kid != "" {
			if _, exists := seenKids[parsed.kid]; exists {
				return nil, fmt.Errorf("duplicate JWK kid %q", parsed.kid)
			}
			seenKids[parsed.kid] = struct{}{}
		}
		result = append(result, parsed)
	}
	return result, nil
}

func parseRSAJWK(raw json.RawMessage, purpose jwkPurpose) (rsaJWK, error) {
	requirePrivate := purpose == jwkSign
	var value jwk
	if err := decodeJSON(raw, &value); err != nil {
		return rsaJWK{}, err
	}
	if value.Kty != "RSA" {
		return rsaJWK{}, fmt.Errorf("kty must be RSA")
	}
	if value.Alg != "" && value.Alg != "RS256" {
		return rsaJWK{}, fmt.Errorf("alg must be RS256 when present")
	}
	if value.Use != "" && value.Use != "sig" {
		return rsaJWK{}, fmt.Errorf("use must be sig when present")
	}
	if len(value.KeyOps) > 0 {
		seen := make(map[string]struct{}, len(value.KeyOps))
		for _, operation := range value.KeyOps {
			if _, exists := seen[operation]; exists {
				return rsaJWK{}, fmt.Errorf("key_ops contains duplicate value %q", operation)
			}
			seen[operation] = struct{}{}
		}
		switch purpose {
		case jwkSign:
			if !slices.Contains(value.KeyOps, "sign") {
				return rsaJWK{}, fmt.Errorf("key_ops does not permit sign")
			}
		case jwkPublish:
			if !slices.Contains(value.KeyOps, "sign") && !slices.Contains(value.KeyOps, "verify") {
				return rsaJWK{}, fmt.Errorf("key_ops does not permit sign or verify")
			}
		default:
			if !slices.Contains(value.KeyOps, "verify") {
				return rsaJWK{}, fmt.Errorf("key_ops does not permit verify")
			}
		}
	}
	n, err := decodeBigInt("n", value.N)
	if err != nil {
		return rsaJWK{}, err
	}
	if n.BitLen() < 2048 {
		return rsaJWK{}, fmt.Errorf("RSA modulus must be at least 2048 bits")
	}
	eBig, err := decodeBigInt("e", value.E)
	if err != nil {
		return rsaJWK{}, err
	}
	if !eBig.IsInt64() || eBig.Int64() < 3 || eBig.Int64()%2 == 0 || eBig.Int64() > int64(^uint(0)>>1) {
		return rsaJWK{}, fmt.Errorf("RSA exponent is invalid")
	}
	public := &rsa.PublicKey{N: n, E: int(eBig.Int64())}
	result := rsaJWK{public: public, kid: value.Kid}
	privatePresent := value.D != "" || value.P != "" || value.Q != "" || value.DP != "" || value.DQ != "" || value.QI != "" || len(value.Oth) > 0
	if !privatePresent {
		if requirePrivate {
			return rsaJWK{}, fmt.Errorf("private RSA parameters are required")
		}
		return result, nil
	}
	if len(value.Oth) > 0 && string(value.Oth) != "null" {
		return rsaJWK{}, fmt.Errorf("multi-prime RSA keys are not supported")
	}
	d, err := decodeBigInt("d", value.D)
	if err != nil {
		return rsaJWK{}, err
	}
	p, err := decodeBigInt("p", value.P)
	if err != nil {
		return rsaJWK{}, err
	}
	q, err := decodeBigInt("q", value.Q)
	if err != nil {
		return rsaJWK{}, err
	}
	private := &rsa.PrivateKey{PublicKey: *public, D: d, Primes: []*big.Int{p, q}}
	if err := private.Validate(); err != nil {
		return rsaJWK{}, fmt.Errorf("RSA private key is inconsistent")
	}
	private.Precompute()
	for name, supplied := range map[string]string{"dp": value.DP, "dq": value.DQ, "qi": value.QI} {
		if supplied == "" {
			continue
		}
		actual := map[string]*big.Int{"dp": private.Precomputed.Dp, "dq": private.Precomputed.Dq, "qi": private.Precomputed.Qinv}[name]
		decoded, err := decodeBigInt(name, supplied)
		if err != nil {
			return rsaJWK{}, err
		}
		if decoded.Cmp(actual) != 0 {
			return rsaJWK{}, fmt.Errorf("RSA parameter %s is inconsistent", name)
		}
	}
	result.private = private
	return result, nil
}

func decodeBigInt(name, value string) (*big.Int, error) {
	if value == "" {
		return nil, fmt.Errorf("RSA parameter %s is required", name)
	}
	decoded, err := rawURL.DecodeString(value)
	if err != nil || len(decoded) == 0 || rawURL.EncodeToString(decoded) != value || decoded[0] == 0 {
		return nil, fmt.Errorf("RSA parameter %s is not canonical base64url", name)
	}
	return new(big.Int).SetBytes(decoded), nil
}

func encodeBigInt(value *big.Int) string {
	return rawURL.EncodeToString(value.Bytes())
}

func publicJWK(key *rsa.PublicKey, kid string) map[string]any {
	return map[string]any{
		"kty": "RSA", "n": encodeBigInt(key.N), "e": encodeBigInt(big.NewInt(int64(key.E))),
		"kid": kid, "use": "sig", "alg": "RS256",
	}
}

func thumbprint(key *rsa.PublicKey) (string, error) {
	canonical, err := json.Marshal(struct {
		E   string `json:"e"`
		Kty string `json:"kty"`
		N   string `json:"n"`
	}{E: encodeBigInt(big.NewInt(int64(key.E))), Kty: "RSA", N: encodeBigInt(key.N)})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return rawURL.EncodeToString(digest[:]), nil
}

func selectJWK(keys []rsaJWK, kid string, requirePrivate bool) (rsaJWK, error) {
	if kid != "" {
		for _, key := range keys {
			if key.kid == kid {
				if requirePrivate && key.private == nil {
					return rsaJWK{}, fmt.Errorf("selected JWK has no private key")
				}
				return key, nil
			}
		}
		return rsaJWK{}, tokenErrorf("no JWK matches token kid")
	}
	if len(keys) != 1 {
		return rsaJWK{}, tokenErrorf("token has no kid and JWKS is ambiguous")
	}
	if requirePrivate && keys[0].private == nil {
		return rsaJWK{}, fmt.Errorf("selected JWK has no private key")
	}
	return keys[0], nil
}
