package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fastClient returns a retryable HTTP client with the production retry policy but
// negligible backoff so tests run quickly.
func fastClient() *http.Client {
	c := newRetryableClient()
	c.RetryWaitMin = time.Millisecond
	c.RetryWaitMax = 5 * time.Millisecond
	return c.StandardClient()
}

func TestRetriesTransientServerErrors(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fail with 503 for the first two attempts, then succeed.
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	resp, err := fastClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("expected eventual success, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 requests (2 failures + 1 success), got %d", got)
	}
}

func TestExhaustsRetriesAndSurfacesError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	resp, err := fastClient().Get(srv.URL)
	if resp != nil {
		resp.Body.Close()
	}
	// After retries are exhausted the client surfaces either an error or the
	// final non-2xx response; in both cases the failure must reach the caller.
	if err == nil && (resp == nil || resp.StatusCode < 500) {
		t.Fatalf("expected failure to surface after exhausting retries, got resp=%v err=%v", resp, err)
	}

	// RetryMax=4 means 1 initial attempt + 4 retries = 5 total requests.
	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Fatalf("expected 5 requests (1 + 4 retries), got %d", got)
	}
}

func TestNoRetryOnSuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := fastClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 request for a successful response, got %d", got)
	}
}
