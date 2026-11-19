package egressproxy

import (
	_ "embed"
	"strconv"
)

// ListenPort is where the forwarder listens inside the server's own network
// namespace (private to the launch, so a fixed port never collides).
const ListenPort = 3128

// PythonForwarder is the docker sidecar's forwarder (forwarder.py), run as
// `python3 -c PythonForwarder <port>` with the multiplex on its stdio.
//
//go:embed forwarder.py
var PythonForwarder string

// ProxyEnv is what a contained server is told about the proxy: the usual
// proxy variables (both spellings; curl, pip, uv, npm, Go and requests read
// them) and NODE_USE_ENV_PROXY for Node's built-in fetch (Node >= 24). A
// client that ignores them simply has no network: the namespace has nothing
// but loopback.
func ProxyEnv() []string {
	u := "http://127.0.0.1:" + strconv.Itoa(ListenPort)
	return []string{
		"HTTPS_PROXY=" + u, "https_proxy=" + u,
		"HTTP_PROXY=" + u, "http_proxy=" + u,
		"ALL_PROXY=" + u, "all_proxy=" + u,
		"NO_PROXY=", "no_proxy=",
		"npm_config_proxy=" + u, "npm_config_https_proxy=" + u,
		"NODE_USE_ENV_PROXY=1",
	}
}
