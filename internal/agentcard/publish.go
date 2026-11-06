package agentcard

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// Publishing (ADR-0139 D4: cards are "published over A2A").
//
// A2A discovery puts ONE card at <base URL>/.well-known/agent-card.json, and a
// kernel hosts several agents, so each agent gets its own base URL under the
// card listener:
//
//	GET /a2a/{agent_id}/.well-known/agent-card.json   the agent's signed card
//	GET /a2a/agents                                   index: ids and card URLs (Cambrian, not A2A)
//	GET /.well-known/jwks.json                        the kernel's card-signing public key
//
// Cards are built from the registry on every request and signed then: the
// registry is the source of truth (an agent registered or changed since boot
// is served as it is now), and an Ed25519 signature costs microseconds.

// Publisher serves the kernel's cards.
type Publisher struct {
	Source        domain.AgentDeclarationSource
	Signer        *Signer
	Options       BuildOptions
	IncludeSystem bool
	// BaseURL, optional, is the listener's public base URL; it makes the index
	// return absolute card URLs.
	BaseURL string
}

// Cards returns every eligible agent's signed card, keyed by agent id.
func (p *Publisher) Cards(ctx context.Context) (map[string]Card, error) {
	defs, err := p.Source.GetAllAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("agentcard: list agents: %w", err)
	}
	out := make(map[string]Card, len(defs))
	for _, def := range defs {
		c, ok, err := p.card(ctx, def)
		if err != nil {
			return nil, err
		}
		if ok {
			out[def.ID] = c
		}
	}
	return out, nil
}

func (p *Publisher) card(ctx context.Context, def domain.AgentDefinition) (Card, bool, error) {
	if ok, _ := Eligible(def, p.IncludeSystem); !ok {
		return Card{}, false, nil
	}
	man, err := p.Source.GetManifest(ctx, def.ID)
	if err != nil {
		return Card{}, false, fmt.Errorf("agentcard: manifest of %s: %w", def.ID, err)
	}
	signed, err := p.Signer.Sign(Build(def, man, p.Options))
	if err != nil {
		return Card{}, false, fmt.Errorf("agentcard: sign card of %s: %w", def.ID, err)
	}
	return signed, true, nil
}

// Handler is the card listener's HTTP handler. Read-only, GET/HEAD only.
func (p *Publisher) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		if !readOnly(w, r) {
			return
		}
		writeJSON(w, "application/jwk-set+json", p.Signer.JWKS())
	})
	mux.HandleFunc("/a2a/agents", func(w http.ResponseWriter, r *http.Request) {
		if !readOnly(w, r) {
			return
		}
		defs, err := p.Source.GetAllAgents(r.Context())
		if err != nil {
			slog.Error("ADR-0139 D4: card index: list agents failed", "err", err)
			http.Error(w, "agent registry unavailable", http.StatusServiceUnavailable)
			return
		}
		type entry struct {
			ID      string `json:"id"`
			CardURL string `json:"card_url"`
		}
		idx := []entry{}
		for _, def := range defs {
			if ok, _ := Eligible(def, p.IncludeSystem); ok {
				idx = append(idx, entry{ID: def.ID, CardURL: strings.TrimRight(p.BaseURL, "/") + "/a2a/" + def.ID + WellKnownPath})
			}
		}
		sort.Slice(idx, func(i, j int) bool { return idx[i].ID < idx[j].ID })
		writeJSON(w, "application/json", map[string]any{"agents": idx})
	})
	mux.HandleFunc("/a2a/", func(w http.ResponseWriter, r *http.Request) {
		if !readOnly(w, r) {
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/a2a/")
		id, ok := strings.CutSuffix(rest, WellKnownPath)
		if !ok || id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		def, err := p.find(r.Context(), id)
		if err != nil {
			slog.Error("ADR-0139 D4: card lookup failed", "agent", id, "err", err)
			http.Error(w, "agent registry unavailable", http.StatusServiceUnavailable)
			return
		}
		if def == nil {
			http.NotFound(w, r)
			return
		}
		c, ok, err := p.card(r.Context(), *def)
		if err != nil {
			slog.Error("ADR-0139 D4: card build failed", "agent", id, "err", err)
			http.Error(w, "card unavailable", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, "application/json", c)
	})
	return mux
}

// find resolves an agent by exact id (never by display name: a card URL names
// one agent, and a name match could serve another's card under that URL).
func (p *Publisher) find(ctx context.Context, id string) (*domain.AgentDefinition, error) {
	defs, err := p.Source.GetAllAgents(ctx)
	if err != nil {
		return nil, err
	}
	for i := range defs {
		if defs[i].ID == id {
			return &defs[i], nil
		}
	}
	return nil, nil
}

func readOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, contentType string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
