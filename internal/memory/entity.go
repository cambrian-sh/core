package memory

import (
	"encoding/json"
	"net/url"
	"path"
	"strings"
)

// ADR-0049 D8 — canonical entity identity.
//
// An entity is a real, persistent THING the agent engaged (a file, a directory, an
// API). Promoting things to first-class records only pays off if the SAME real thing
// always resolves to the SAME id — otherwise the store fragments ("a.md", "./a.md",
// "A.MD" each minting a separate record). So identity flows through one aggressive,
// deterministic canonicalizer; nothing else mints ids.

// canonicalEntity is a thing's deterministic identity plus the optional attribute that
// a single touch observed (the specific endpoint of an api, say) — the attribute is
// descriptive metadata, NOT part of the identity, so it never fragments the entity.
type canonicalEntity struct {
	Kind     string // file | dir | api
	ID       string // canonicalized identifier within the kind
	Endpoint string // api only: the specific endpoint path observed (an attribute)
}

// Key is the store id: "kind:id". Deterministic, so the upsert dedups by real thing.
func (e canonicalEntity) Key() string { return e.Kind + ":" + e.ID }

// canonicalPath normalizes a filesystem path so equivalent spellings collapse: Windows
// backslashes → forward slashes, `.`/`..`/`//` cleaned away, no trailing slash, and
// lower-cased (the runtime targets Windows, whose filesystem is case-insensitive — two
// casings name one real file and must share one entity). Pure/deterministic.
func canonicalPath(raw string) string {
	p := strings.ReplaceAll(raw, "\\", "/")
	p = path.Clean(p)
	return strings.ToLower(p)
}

// defaultPorts maps a scheme to the port that carries no information (so :443 on https
// is stripped — same host either way).
var defaultPorts = map[string]string{"http": "80", "https": "443"}

// canonicalAPI reduces a URL/host ref to its api identity: scheme://host (host
// lower-cased, default port dropped), returning the path as the observed endpoint
// attribute. All endpoints under one host therefore collapse to ONE api entity
// (ADR-0049 D8: endpoints are attributes, not entities). ok=false on an unparseable
// value with no host. Pure/deterministic.
func canonicalAPI(raw string) (id, endpoint string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	// A bare host ("example.com[:port]") has no scheme — give url.Parse one so it lands
	// in Host rather than Path, then drop the synthetic scheme from the id.
	synthetic := false
	parseable := raw
	if !strings.Contains(raw, "://") {
		parseable = "//" + raw
		synthetic = true
	}
	u, err := url.Parse(parseable)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	host := strings.ToLower(u.Host)
	scheme := strings.ToLower(u.Scheme)
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		if defaultPorts[scheme] == host[i+1:] {
			host = host[:i] // strip the redundant default port
		}
	}
	endpoint = strings.TrimRight(u.Path, "/")
	if synthetic || scheme == "" {
		return host, endpoint, true
	}
	return scheme + "://" + host, endpoint, true
}

// entityArgKinds maps a resource arg key to the entity KIND it names. Only keys that
// denote a persistent thing appear; `command` is intentionally absent (a command is an
// action, not a thing). file-like keys yield a file (its parent dir is added by the
// caller); url-like keys yield an api.
var entityArgKinds = map[string]string{
	"path":      "file",
	"file":      "file",
	"filename":  "file",
	"dir":       "dir",
	"directory": "dir",
	"url":       "api",
	"uri":       "api",
	"endpoint":  "api",
	"host":      "api",
}

// isDegeneratePath reports whether a canonicalized path is a non-identifying location —
// the cwd ("."), parent (".."), or root ("/"/"") — which must NOT become an entity (a
// relative "." is whatever directory the agent happened to run in; it identifies nothing).
func isDegeneratePath(p string) bool {
	switch p {
	case "", ".", "..", "/":
		return true
	default:
		return false
	}
}

// pathArgKind resolves the AMBIGUOUS `path` arg to a file or a directory using the TOOL
// (ADR-0049 D8 — semantics are per-tool, never per-server): `list_directory`/`mkdir`/etc.
// name a directory, while `read_file`/`write_file`/etc. name a file. Deterministic.
func pathArgKind(toolName string) string {
	n := strings.ToLower(toolName)
	if containsAny(n, "directory", "dir", "folder", "readdir", "list", "mkdir", "ls", "tree") {
		return "dir"
	}
	return "file" // the common case for a path-taking tool
}

// engagedEntities derives the canonical entities a tool call touched, from its tool name
// and args (ADR-0049 D8). A file touch also yields its parent directory. Degenerate paths
// (the cwd, root) are skipped — they identify nothing. Deduplicated by Key. Pure.
//
// EVERY engagement counts — read OR write. Reading a file/url is DISCOVERY: it tells us
// the thing exists and what its content is, which is exactly the world-model knowledge
// worth keeping. The caller derives the field observation (a delete clears exists; a read
// or write records exists + the observed content baseline).
func engagedEntities(toolName string, argsJSON []byte) []canonicalEntity {
	if len(argsJSON) == 0 {
		return nil
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(argsJSON, &args) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []canonicalEntity
	add := func(e canonicalEntity) {
		if e.ID == "" || seen[e.Key()] {
			return
		}
		seen[e.Key()] = true
		out = append(out, e)
	}
	addFile := func(cp string) {
		if isDegeneratePath(cp) {
			return
		}
		add(canonicalEntity{Kind: "file", ID: cp})
		if d := path.Dir(cp); !isDegeneratePath(d) {
			add(canonicalEntity{Kind: "dir", ID: d})
		}
	}
	addDir := func(cp string) {
		if !isDegeneratePath(cp) {
			add(canonicalEntity{Kind: "dir", ID: cp})
		}
	}
	for key, kind := range entityArgKinds {
		raw, ok := args[key]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil || v == "" {
			continue
		}
		switch kind {
		case "file":
			// `path` is ambiguous — the TOOL decides whether it names a file or a dir.
			// `file`/`filename` are unambiguously files.
			if key == "path" && pathArgKind(toolName) == "dir" {
				addDir(canonicalPath(v))
			} else {
				addFile(canonicalPath(v))
			}
		case "dir":
			addDir(canonicalPath(v))
		case "api":
			if id, endpoint, ok := canonicalAPI(v); ok {
				add(canonicalEntity{Kind: "api", ID: id, Endpoint: endpoint})
			}
		}
	}
	return out
}
