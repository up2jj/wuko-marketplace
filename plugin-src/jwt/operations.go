package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type generateStepConfig struct {
	Claims     json.RawMessage `json:"claims"`
	Issuer     string          `json:"issuer"`
	Audience   stringList      `json:"audience"`
	Subject    string          `json:"subject"`
	TTL        string          `json:"ttl"`
	SigningJWK json.RawMessage `json:"signing_jwk"`
	Kid        string          `json:"kid"`
}

type jwksStepConfig struct {
	SigningJWK json.RawMessage `json:"signing_jwk"`
}

type verifyStepConfig struct {
	Token          string          `json:"token"`
	JWKS           json.RawMessage `json:"jwks"`
	JWKSURL        string          `json:"jwks_url"`
	Issuer         string          `json:"issuer"`
	Audience       stringList      `json:"audience"`
	Subject        string          `json:"subject"`
	Leeway         string          `json:"leeway"`
	RequiredClaims *[]string       `json:"required_claims"`
}

func (s *server) callHelper(ctx context.Context, params helperParams) (any, error) {
	switch params.Name {
	case "generate":
		if len(params.Args) > 2 {
			return nil, fmt.Errorf("jwt_generate accepts at most two arguments")
		}
		var claims json.RawMessage
		if len(params.Args) > 0 {
			claims = params.Args[0]
		}
		var options generateOptions
		if len(params.Args) > 1 {
			if err := decodeRawStrict(params.Args[1], &options); err != nil {
				return nil, fmt.Errorf("invalid jwt_generate options: %w", err)
			}
		}
		result, err := s.service.generate(ctx, claims, options)
		if err != nil {
			return nil, err
		}
		return result.Token, nil
	case "jwks":
		if len(params.Args) > 1 {
			return nil, fmt.Errorf("jwt_jwks accepts at most one argument")
		}
		var raw json.RawMessage
		if len(params.Args) == 1 {
			raw = params.Args[0]
		}
		return s.service.jwks(ctx, raw)
	case "verify", "is_valid":
		if len(params.Args) < 1 || len(params.Args) > 2 {
			return nil, fmt.Errorf("jwt_%s accepts a token and optional options", params.Name)
		}
		var token string
		if err := decodeStrict(params.Args[0], &token); err != nil || token == "" {
			return nil, fmt.Errorf("jwt_%s token must be a non-empty string", params.Name)
		}
		options, err := decodeVerifyOptions(argumentAt(params.Args, 1))
		if err != nil {
			return nil, fmt.Errorf("invalid jwt_%s options: %w", params.Name, err)
		}
		verified, err := s.service.verify(ctx, token, options)
		if params.Name == "is_valid" {
			if isTokenError(err) {
				return false, nil
			}
			return err == nil, err
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"header": verified.Header, "claims": verified.Claims}, nil
	default:
		return nil, fmt.Errorf("unknown helper %q", params.Name)
	}
}

func (s *server) callStep(ctx context.Context, stepType string, raw json.RawMessage, validateOnly bool) (map[string]any, error) {
	switch stepType {
	case "jwt.generate":
		var config generateStepConfig
		if err := decodeRawStrict(raw, &config); err != nil {
			return nil, fmt.Errorf("invalid jwt.generate configuration: %w", err)
		}
		if validateOnly {
			if !containsTemplate(config.TTL) && config.TTL != "" {
				if _, err := parsePositiveDuration(config.TTL); err != nil {
					return nil, err
				}
			}
			if len(config.Claims) > 0 && string(config.Claims) != "null" && !bytes.Contains(config.Claims, []byte("{{")) {
				if _, err := decodeObjectOrString(config.Claims); err != nil {
					return nil, fmt.Errorf("claims must be a JSON object: %w", err)
				}
			}
			if len(config.SigningJWK) > 0 && string(config.SigningJWK) != "null" && !bytes.Contains(config.SigningJWK, []byte("{{")) {
				if _, err := parseJWKInput(config.SigningJWK, jwkSign); err != nil {
					return nil, fmt.Errorf("invalid signing_jwk: %w", err)
				}
			}
			return nil, nil
		}
		result, err := s.service.generate(ctx, config.Claims, generateOptions{
			Issuer: config.Issuer, Audience: config.Audience, Subject: config.Subject, TTL: config.TTL,
			SigningJWK: config.SigningJWK, Kid: config.Kid,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"token": result.Token, "header": result.Header, "claims": result.Claims, "jwks": result.JWKS}, nil
	case "jwt.jwks":
		var config jwksStepConfig
		if err := decodeRawStrict(raw, &config); err != nil {
			return nil, fmt.Errorf("invalid jwt.jwks configuration: %w", err)
		}
		if validateOnly {
			if len(config.SigningJWK) > 0 && string(config.SigningJWK) != "null" && !bytes.Contains(config.SigningJWK, []byte("{{")) {
				if _, err := parseJWKInput(config.SigningJWK, jwkPublish); err != nil {
					return nil, fmt.Errorf("invalid signing_jwk: %w", err)
				}
			}
			return nil, nil
		}
		result, err := s.service.jwks(ctx, config.SigningJWK)
		if err != nil {
			return nil, err
		}
		return map[string]any{"jwks": result}, nil
	case "jwt.verify", "jwt.is_valid":
		var config verifyStepConfig
		if err := decodeRawStrict(raw, &config); err != nil {
			return nil, fmt.Errorf("invalid %s configuration: %w", stepType, err)
		}
		options := verifyOptions{
			JWKS: config.JWKS, JWKSURL: config.JWKSURL, Issuer: config.Issuer, Audience: config.Audience,
			Subject: config.Subject, Leeway: config.Leeway,
		}
		if config.RequiredClaims != nil {
			options.RequiredClaims, options.requiredSet = *config.RequiredClaims, true
		}
		if err := validateVerifyConfig(config.Token, options, validateOnly); err != nil {
			return nil, err
		}
		if validateOnly {
			return nil, nil
		}
		verified, err := s.service.verify(ctx, config.Token, options)
		if stepType == "jwt.is_valid" {
			if isTokenError(err) {
				return map[string]any{"valid": false}, nil
			}
			if err != nil {
				return nil, err
			}
			return map[string]any{"valid": true}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"header": verified.Header, "claims": verified.Claims}, nil
	default:
		return nil, fmt.Errorf("unsupported step type %q", stepType)
	}
}

func decodeVerifyOptions(raw json.RawMessage) (verifyOptions, error) {
	var wire struct {
		JWKS           json.RawMessage `json:"jwks"`
		JWKSURL        string          `json:"jwks_url"`
		Issuer         string          `json:"issuer"`
		Audience       stringList      `json:"audience"`
		Subject        string          `json:"subject"`
		Leeway         string          `json:"leeway"`
		RequiredClaims *[]string       `json:"required_claims"`
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := decodeRawStrict(raw, &wire); err != nil {
		return verifyOptions{}, err
	}
	options := verifyOptions{
		JWKS: wire.JWKS, JWKSURL: wire.JWKSURL, Issuer: wire.Issuer, Audience: wire.Audience,
		Subject: wire.Subject, Leeway: wire.Leeway,
	}
	if wire.RequiredClaims != nil {
		options.RequiredClaims, options.requiredSet = *wire.RequiredClaims, true
	}
	return options, validateVerifyOptions(options, false)
}

func validateVerifyConfig(token string, options verifyOptions, allowTemplates bool) error {
	if strings.TrimSpace(token) == "" && !(allowTemplates && containsTemplate(token)) {
		return fmt.Errorf("token is required")
	}
	return validateVerifyOptions(options, allowTemplates)
}

func validateVerifyOptions(options verifyOptions, allowTemplates bool) error {
	if len(options.JWKS) > 0 && string(options.JWKS) != "null" && options.JWKSURL != "" {
		return fmt.Errorf("jwks and jwks_url are mutually exclusive")
	}
	if options.Leeway != "" && !(allowTemplates && containsTemplate(options.Leeway)) {
		if _, err := parseNonnegativeDuration("leeway", options.Leeway); err != nil {
			return err
		}
	}
	if options.JWKSURL != "" && !(allowTemplates && containsTemplate(options.JWKSURL)) {
		target, err := url.Parse(options.JWKSURL)
		if err != nil || target.String() != options.JWKSURL {
			return fmt.Errorf("jwks_url is invalid")
		}
		if err := validateJWKSURL(target); err != nil {
			return err
		}
	}
	if len(options.JWKS) > 0 && string(options.JWKS) != "null" && !(allowTemplates && bytes.Contains(options.JWKS, []byte("{{"))) {
		if _, err := parseJWKInput(options.JWKS, jwkVerify); err != nil {
			return fmt.Errorf("invalid jwks: %w", err)
		}
	}
	if err := validateAudience(options.Audience); err != nil {
		return err
	}
	seenClaims := make(map[string]struct{}, len(options.RequiredClaims))
	for _, name := range options.RequiredClaims {
		if strings.TrimSpace(name) == "" && !(allowTemplates && containsTemplate(name)) {
			return fmt.Errorf("required_claims must not contain empty names")
		}
		if _, exists := seenClaims[name]; exists {
			return fmt.Errorf("required_claims must not contain duplicates")
		}
		seenClaims[name] = struct{}{}
	}
	return nil
}

func validateAudience(audience stringList) error {
	seen := make(map[string]struct{}, len(audience))
	for _, value := range audience {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("audience must not contain empty values")
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("audience must not contain duplicates")
		}
		seen[value] = struct{}{}
	}
	return nil
}

func parsePositiveDuration(value string) (int64, error) {
	duration, err := parseNonnegativeDuration("ttl", value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("ttl must be a positive duration")
	}
	return int64(duration), nil
}

func containsTemplate(value string) bool { return strings.Contains(value, "{{") }

func argumentAt(arguments []json.RawMessage, index int) json.RawMessage {
	if index >= len(arguments) {
		return nil
	}
	return arguments[index]
}
