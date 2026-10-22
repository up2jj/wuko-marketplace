// A test-only HTTP counter used to prove that traffic stops after owner loss.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

func main() {
	var mu sync.Mutex
	counts := map[string]int{}
	executionID := regexp.MustCompile(`it-k6-[0-9a-f]{6}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		mu.Lock()
		counts["otlp_batches"]++
		if bytes.Contains(body, []byte("execution_id")) {
			counts["otlp_execution_id"]++
			for _, id := range executionID.FindAll(body, -1) {
				counts["otlp_execution_id:"+string(id)]++
			}
		}
		if bytes.Contains(body, []byte("k6_")) {
			counts["otlp_k6_metrics"]++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(200)
	})
	mux.HandleFunc("/tick/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[strings.TrimPrefix(r.URL.Path, "/tick/")]++
		mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("/counts", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(counts)
	})
	s := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second}
	if err := s.ListenAndServe(); err != nil {
		panic(err)
	}
}
