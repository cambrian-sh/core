package network

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWASIInterceptor_Interception(t *testing.T) {
	// 1. Setup Mock Server
	receivedRequest := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedRequest = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Intercepted!"))
	}))
	defer server.Close()

	// 2. Setup HotClientPool and Interceptor
	pool := NewHotClientPool()
	interceptor := NewWASIInterceptor(pool)

	// 3. Test RoundTripper directly (Host-side validation)
	req, _ := http.NewRequest("GET", server.URL, nil)
	resp, err := interceptor.RoundTrip(req)
	if err != nil {
		t.Fatalf("Interceptor failed: %v", err)
	}
	defer resp.Body.Close()

	if !receivedRequest {
		t.Error("Expected request to reach mock server via interceptor")
	}

	if resp.StatusCode != 200 {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// 4. Verify pooling (second request to same host)
	receivedRequest = false
	resp2, err := interceptor.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	
	if !receivedRequest {
		t.Error("Expected pooled request to reach mock server")
	}
}
