package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoteJWKSCacheAndRefresh(t *testing.T) {
	signer := newService()
	issued, err := signer.generate(t.Context(), nil, generateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(issued.JWKS)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Cache-Control", "max-age=600")
		writer.Header().Set("ETag", `"one"`)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(body)
	}))
	defer server.Close()

	verifier := newService()
	options := verifyOptions{JWKSURL: server.URL}
	if _, err := verifier.verify(t.Context(), issued.Token, options); err != nil {
		t.Fatalf("first verify error = %v", err)
	}
	if _, err := verifier.verify(t.Context(), issued.Token, options); err != nil {
		t.Fatalf("cached verify error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}

	claimsJSON, _ := json.Marshal(issued.Claims)
	unknownKid := signClaims(t, signer.key, "missing", claimsJSON)
	if _, err := verifier.verify(t.Context(), unknownKid, options); !isTokenError(err) {
		t.Fatalf("unknown kid error = %T %v", err, err)
	}
	if requests.Load() != 2 {
		t.Fatalf("refresh requests = %d, want 2", requests.Load())
	}
}

func TestRemoteJWKSNoStoreAndLimits(t *testing.T) {
	signer := newService()
	issued, err := signer.generate(t.Context(), nil, generateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(issued.JWKS)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Cache-Control", "no-store")
		_, _ = writer.Write(body)
	}))
	defer server.Close()

	verifier := newService()
	for range 2 {
		if _, err := verifier.verify(t.Context(), issued.Token, verifyOptions{JWKSURL: server.URL}); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
	if _, err := verifier.verify(t.Context(), issued.Token, verifyOptions{JWKSURL: "http://example.com/keys"}); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback HTTP error = %v", err)
	}
}

func TestRemoteJWKSOperationalFailureIsNotTokenInvalid(t *testing.T) {
	signer := newService()
	issued, err := signer.generate(t.Context(), nil, generateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	serverURL := server.URL
	server.Close()

	_, err = newService().verify(t.Context(), issued.Token, verifyOptions{JWKSURL: serverURL})
	if err == nil || isTokenError(err) {
		t.Fatalf("network error = %T %v", err, err)
	}
}

func TestRemoteJWKSCanceledLeaderDoesNotFailWaiters(t *testing.T) {
	signer := newService()
	issued, err := signer.generate(t.Context(), nil, generateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(issued.JWKS)
	release := make(chan struct{})
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			select {
			case <-release:
			case <-request.Context().Done():
			}
			return
		}
		_, _ = writer.Write(body)
	}))
	defer server.Close()

	verifier := newService()
	options := verifyOptions{JWKSURL: server.URL}
	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := verifier.verify(leaderCtx, issued.Token, options)
		leaderErr <- err
	}()
	for requests.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	waiterErr := make(chan error, 1)
	go func() {
		_, err := verifier.verify(t.Context(), issued.Token, options)
		waiterErr <- err
	}()
	for {
		verifier.remote.mu.Lock()
		waiting := len(verifier.remote.inflight[server.URL])
		verifier.remote.mu.Unlock()
		if waiting > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %T %v", err, err)
	}
	close(release)
	if err := <-waiterErr; err != nil {
		t.Fatalf("waiter error = %v", err)
	}
}
