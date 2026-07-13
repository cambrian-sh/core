// pagerank-recompute is the always-up worker that maintains the chunk_pagerank
// table (ADR-0054 D2). It runs in its OWN container so the recompute schedule is
// decoupled from the (intermittent, edge-device) kernel: the kernel only READS
// chunk_pagerank, this worker WRITES it.
//
// It recomputes on three triggers:
//   - boot (self-heals after a long-off period),
//   - a periodic ticker (delta/age-gated via ShouldRecompute),
//   - an on-demand HTTP trigger the kernel calls (POST /recompute, force).
//
// All three funnel through a single-flight guard so overlapping triggers never
// stack two graph builds. RUN_ONCE=1 computes once and exits (cron/timer driver).
//
// Config is env-driven (container-native); the PG connection reuses the kernel's
// config file so the DSN stays in one place.
//
//	PAGERANK_INTERVAL_SEC   tick period (default 3600)
//	PAGERANK_MAX_AGE_HOURS  force recompute if older (default 24)
//	PAGERANK_DELTA_PCT      recompute if chunk-count moved >= this (default 5)
//	PAGERANK_DAMPING        damping factor (default 0.85)
//	PAGERANK_ITERATIONS     power-iteration rounds (default 50)
//	PAGERANK_DF_CAP         drop entities in > N chunks (default 2000; 0 = no cap)
//	PAGERANK_HTTP_ADDR      trigger endpoint listen addr (default ":8088"; "" disables)
//	PAGERANK_TRIGGER_TOKEN  if set, POST /recompute requires header X-Trigger-Token
//	RUN_ONCE                "1" ⇒ compute once and exit
//	CAMBRIAN_CONFIG         kernel config path (default configs/config.json)
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory"
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// worker owns the recompute, with a single-flight guard so the ticker, the boot
// pass, and the HTTP trigger can never run two graph builds concurrently.
type worker struct {
	pg       *postgres.PgVectorAdapter
	computer memory.PageRankComputer
	params   memory.PageRankParams
	deltaPct float64
	maxAge   time.Duration

	running atomic.Bool
	mu      sync.Mutex   // serializes the (rare) compute itself
	last    atomic.Value // stores lastRun
}

type lastRun struct {
	At       time.Time `json:"at"`
	Scored   int       `json:"scored_chunks"`
	Duration string    `json:"duration"`
	Err      string    `json:"error,omitempty"`
}

// recompute runs the PageRank pass. force=true skips the ShouldRecompute gate
// (an explicit kernel trigger). Returns ran=false if a recompute was already in
// flight (single-flight) or the gate decided it was unnecessary.
func (w *worker) recompute(ctx context.Context, force bool) (ran bool, n int, err error) {
	if !w.running.CompareAndSwap(false, true) {
		return false, 0, nil // already in flight
	}
	defer w.running.Store(false)
	w.mu.Lock()
	defer w.mu.Unlock()

	if !force {
		prevAt, prevChunks, _, merr := w.pg.PageRankMeta(ctx)
		if merr != nil {
			slog.Warn("pagerank: meta read failed; proceeding", "err", merr)
		}
		curChunks, _, serr := w.pg.CorpusStats(ctx)
		if serr != nil {
			return false, 0, serr
		}
		if !memory.ShouldRecompute(prevAt, prevChunks, curChunks, w.deltaPct, w.maxAge) {
			slog.Info("pagerank: corpus unchanged, skipping", "chunks", curChunks)
			return false, 0, nil
		}
	}

	t0 := time.Now()
	n, err = memory.RecomputeAndStore(ctx, w.pg, w.pg, w.computer, w.params)
	rec := lastRun{At: time.Now(), Scored: n, Duration: time.Since(t0).String()}
	if err != nil {
		rec.Err = err.Error()
		w.last.Store(rec)
		slog.Error("pagerank: recompute failed", "err", err)
		return true, 0, err
	}
	w.last.Store(rec)
	slog.Info("pagerank: done", "scored_chunks", n, "elapsed", rec.Duration, "force", force, "df_cap", w.params.DFCap)
	return true, n, nil
}

func (w *worker) serveHTTP(ctx context.Context, addr, token string) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("ok"))
	})

	mux.HandleFunc("/status", func(rw http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{"running": w.running.Load()}
		if v, ok := w.last.Load().(lastRun); ok {
			resp["last"] = v
		}
		at, chunks, triplets, _ := w.pg.PageRankMeta(ctx)
		resp["table"] = map[string]any{"computed_at": at, "chunk_count": chunks, "triplet_count": triplets}
		writeJSON(rw, http.StatusOK, resp)
	})

	// POST /recompute[?wait=true] — the on-demand trigger the kernel calls.
	// Async (202) by default; ?wait=true blocks and returns the result (200).
	mux.HandleFunc("/recompute", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(rw, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		if token != "" && r.Header.Get("X-Trigger-Token") != token {
			writeJSON(rw, http.StatusUnauthorized, map[string]string{"error": "bad token"})
			return
		}
		if w.running.Load() {
			writeJSON(rw, http.StatusAccepted, map[string]string{"status": "already_running"})
			return
		}
		if r.URL.Query().Get("wait") == "true" {
			ran, n, err := w.recompute(ctx, true)
			if err != nil {
				writeJSON(rw, http.StatusInternalServerError, map[string]string{"status": "error", "error": err.Error()})
				return
			}
			writeJSON(rw, http.StatusOK, map[string]any{"status": "done", "ran": ran, "scored_chunks": n})
			return
		}
		// Fire-and-forget: detach from the request context so the recompute
		// outlives the HTTP call.
		go func() { _, _, _ = w.recompute(context.WithoutCancel(ctx), true) }()
		writeJSON(rw, http.StatusAccepted, map[string]string{"status": "started"})
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("pagerank: trigger endpoint up", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("pagerank: http server error", "err", err)
		}
	}()
	return srv
}

func writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(v)
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	_ = config.LoadDotEnv(".env") // best-effort; container may inject env directly

	cfgPath := os.Getenv("CAMBRIAN_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/config.json"
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		slog.Error("pagerank-recompute: config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pg, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		slog.Error("pagerank-recompute: pg connect failed", "err", err)
		os.Exit(1)
	}
	defer pg.Close()

	w := &worker{
		pg:       pg,
		computer: memory.GoPageRank{},
		params: memory.PageRankParams{
			Damping:    envFloat("PAGERANK_DAMPING", 0.85),
			Iterations: envInt("PAGERANK_ITERATIONS", 50),
			DFCap:      envInt("PAGERANK_DF_CAP", 2000),
		},
		deltaPct: envFloat("PAGERANK_DELTA_PCT", 5),
		maxAge:   time.Duration(envInt("PAGERANK_MAX_AGE_HOURS", 24)) * time.Hour,
	}
	runOnce := os.Getenv("RUN_ONCE") == "1"

	// Boot pass (force: self-heal after a long-off period).
	if _, _, err := w.recompute(ctx, true); err != nil {
		slog.Warn("pagerank: boot recompute failed", "err", err)
	}
	if runOnce {
		return
	}

	// On-demand trigger endpoint (the thing the kernel calls).
	var srv *http.Server
	if addr := envStr("PAGERANK_HTTP_ADDR", ":8088"); addr != "" {
		srv = w.serveHTTP(ctx, addr, os.Getenv("PAGERANK_TRIGGER_TOKEN"))
	}

	intervalSec := envInt("PAGERANK_INTERVAL_SEC", 3600)
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()
	slog.Info("pagerank-recompute: worker up", "interval_sec", intervalSec)
	for {
		select {
		case <-ctx.Done():
			slog.Info("pagerank-recompute: shutting down")
			if srv != nil {
				shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = srv.Shutdown(shutCtx)
				cancel()
			}
			return
		case <-ticker.C:
			_, _, _ = w.recompute(ctx, false) // gated by ShouldRecompute
		}
	}
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}
