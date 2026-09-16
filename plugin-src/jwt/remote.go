package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	remoteTimeout    = 5 * time.Second
	defaultCacheTTL  = 5 * time.Minute
	minimumCacheTTL  = time.Minute
	maximumCacheTTL  = time.Hour
	maxCacheEntries  = 100
	maxRedirectCount = 3
)

type cacheEntry struct {
	keys         []rsaJWK
	expires      time.Time
	etag         string
	lastModified string
	lastUsed     time.Time
}

type fetchResult struct {
	keys []rsaJWK
	err  error
}

type remoteCache struct {
	mu       sync.Mutex
	entries  map[string]cacheEntry
	inflight map[string][]chan fetchResult
	client   *http.Client
	now      func() time.Time
}

func newRemoteCache() *remoteCache {
	cache := &remoteCache{
		entries: make(map[string]cacheEntry), inflight: make(map[string][]chan fetchResult), now: time.Now,
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = cache.dialContext
	cache.client = &http.Client{
		Transport: transport,
		Timeout:   remoteTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > maxRedirectCount {
				return fmt.Errorf("too many redirects")
			}
			if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && request.URL.Scheme != "https" {
				return fmt.Errorf("HTTPS downgrade is not allowed")
			}
			return validateJWKSURL(request.URL)
		},
	}
	return cache
}

func (c *remoteCache) fetch(ctx context.Context, rawURL string, force bool) ([]rsaJWK, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.String() != rawURL {
		return nil, fmt.Errorf("jwks_url is invalid")
	}
	if err := validateJWKSURL(parsed); err != nil {
		return nil, err
	}
	now := c.now()
	c.mu.Lock()
	entry, exists := c.entries[rawURL]
	if exists && !force && now.Before(entry.expires) {
		entry.lastUsed = now
		c.entries[rawURL] = entry
		keys := cloneKeys(entry.keys)
		c.mu.Unlock()
		return keys, nil
	}
	if waiters, fetching := c.inflight[rawURL]; fetching {
		waiting := make(chan fetchResult, 1)
		c.inflight[rawURL] = append(waiters, waiting)
		c.mu.Unlock()
		select {
		case result := <-waiting:
			// The leader's cancellation says nothing about this caller; fetch again instead.
			if result.err != nil && ctx.Err() == nil && isContextError(result.err) {
				return c.fetch(ctx, rawURL, force)
			}
			return result.keys, result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c.inflight[rawURL] = nil
	c.mu.Unlock()

	keys, replacement, err := c.fetchNow(ctx, parsed, entry, exists)
	result := fetchResult{keys: cloneKeys(keys), err: err}
	c.mu.Lock()
	if err == nil {
		if replacement != nil {
			c.entries[rawURL] = *replacement
		} else {
			delete(c.entries, rawURL)
		}
		c.evictLocked()
	}
	waiters := c.inflight[rawURL]
	delete(c.inflight, rawURL)
	c.mu.Unlock()
	for _, waiting := range waiters {
		waiting <- result
	}
	return result.keys, result.err
}

func (c *remoteCache) fetchNow(ctx context.Context, target *url.URL, previous cacheEntry, hasPrevious bool) ([]rsaJWK, *cacheEntry, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("creating remote JWKS request")
	}
	request.Header.Set("Accept", "application/json")
	if hasPrevious {
		if previous.etag != "" {
			request.Header.Set("If-None-Match", previous.etag)
		}
		if previous.lastModified != "" {
			request.Header.Set("If-Modified-Since", previous.lastModified)
		}
	}
	response, err := c.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, nil, fmt.Errorf("fetching remote JWKS failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified && hasPrevious {
		ttl, cacheable := cacheLifetime(response.Header)
		if !cacheable {
			return cloneKeys(previous.keys), nil, nil
		}
		previous.expires = c.now().Add(ttl)
		previous.lastUsed = c.now()
		return cloneKeys(previous.keys), &previous, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("remote JWKS returned HTTP %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, maxDocumentSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, nil, fmt.Errorf("reading remote JWKS failed")
	}
	if len(data) > maxDocumentSize {
		return nil, nil, fmt.Errorf("remote JWKS exceeds 1 MiB")
	}
	keys, err := parseJWKInput(json.RawMessage(data), jwkVerify)
	if err != nil {
		return nil, nil, fmt.Errorf("remote JWKS is invalid: %w", err)
	}
	ttl, cacheable := cacheLifetime(response.Header)
	if !cacheable {
		return keys, nil, nil
	}
	now := c.now()
	entry := &cacheEntry{
		keys: cloneKeys(keys), expires: now.Add(ttl), etag: response.Header.Get("ETag"),
		lastModified: response.Header.Get("Last-Modified"), lastUsed: now,
	}
	return keys, entry, nil
}

func validateJWKSURL(target *url.URL) error {
	if target.User != nil || target.Host == "" || target.Fragment != "" {
		return fmt.Errorf("jwks_url must not contain credentials or a fragment")
	}
	if target.Scheme == "https" {
		return nil
	}
	if target.Scheme != "http" {
		return fmt.Errorf("jwks_url must use HTTPS or loopback HTTP")
	}
	host := target.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	address := net.ParseIP(host)
	if address == nil || !address.IsLoopback() {
		return fmt.Errorf("plain HTTP jwks_url must use a loopback host")
	}
	return nil
}

func (c *remoteCache) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("invalid remote JWKS port")
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, fmt.Errorf("resolving remote JWKS host failed")
	}
	// HTTP is admitted only for loopback hosts. Reject mixed DNS answers so a hostname
	// cannot pass validation and then be rebound to a non-loopback address.
	parsedIP := net.ParseIP(host)
	loopbackOnly := strings.EqualFold(host, "localhost") || parsedIP != nil && parsedIP.IsLoopback()
	if loopbackOnly {
		for _, candidate := range addresses {
			if !candidate.IsLoopback() {
				return nil, fmt.Errorf("plain HTTP remote JWKS resolved outside loopback")
			}
		}
	}
	dialer := net.Dialer{Timeout: remoteTimeout}
	var lastErr error
	for _, candidate := range addresses {
		connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.String(), port))
		if err == nil {
			return connection, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func cacheLifetime(header http.Header) (time.Duration, bool) {
	for _, directive := range strings.Split(header.Get("Cache-Control"), ",") {
		trimmed := strings.TrimSpace(strings.ToLower(directive))
		if trimmed == "no-store" {
			return 0, false
		}
		if after, ok := strings.CutPrefix(trimmed, "max-age="); ok {
			seconds, err := strconv.ParseInt(strings.Trim(after, `"`), 10, 64)
			if err == nil {
				if seconds <= int64(minimumCacheTTL/time.Second) {
					return minimumCacheTTL, true
				}
				if seconds >= int64(maximumCacheTTL/time.Second) {
					return maximumCacheTTL, true
				}
				return time.Duration(seconds) * time.Second, true
			}
		}
	}
	return defaultCacheTTL, true
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func cloneKeys(keys []rsaJWK) []rsaJWK {
	return append([]rsaJWK(nil), keys...)
}

func (c *remoteCache) evictLocked() {
	for len(c.entries) > maxCacheEntries {
		var oldestURL string
		var oldest time.Time
		for rawURL, entry := range c.entries {
			if oldestURL == "" || entry.lastUsed.Before(oldest) {
				oldestURL, oldest = rawURL, entry.lastUsed
			}
		}
		delete(c.entries, oldestURL)
	}
}
