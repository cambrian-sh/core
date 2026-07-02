package network

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHotClientPool_Reuse(t *testing.T) {
	pool := NewHotClientPool()
	host := "example.com"

	client1 := pool.GetClient(host)
	client2 := pool.GetClient(host)

	if client1 == nil {
		t.Fatal("Expected client1 to be non-nil")
	}

	if client1 != client2 {
		t.Error("Expected GetClient to return the same client instance for the same host")
	}
}

func TestHotClientPool_Request(t *testing.T) {
	pool := NewHotClientPool()
	
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer server.Close()

	client := pool.GetClient(server.URL)
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}

func TestHotClientPool_Isolation(t *testing.T) {
	pool := NewHotClientPool()
	
	clientA := pool.GetClient("host-a.com")
	clientB := pool.GetClient("host-b.com")

	if clientA == clientB {
		t.Error("Expected different hosts to have different client instances")
	}
}
