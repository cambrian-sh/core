package network

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// HotClientPool manages a pool of persistent HTTP clients to minimize TLS handshake overhead.
type HotClientPool struct {
	mu      sync.RWMutex
	clients map[string]*http.Client
}

// NewHotClientPool creates a new HotClientPool.
func NewHotClientPool() *HotClientPool {
	return &HotClientPool{
		clients: make(map[string]*http.Client),
	}
}

// GetClient returns an http.Client for the given host.
// It creates a new client if one doesn't exist for the host.
func (p *HotClientPool) GetClient(host string) *http.Client {
	p.mu.RLock()
	client, ok := p.clients[host]
	p.mu.RUnlock()

	if ok {
		return client
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after lock
	if client, ok := p.clients[host]; ok {
		return client
	}

	// Create a new optimized client
	client = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConnsPerHost:   10, // Optimized for agent-to-specific-service calls
		},
		Timeout: 60 * time.Second,
	}

	p.clients[host] = client
	return client
}
