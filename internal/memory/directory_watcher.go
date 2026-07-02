package memory

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"

	"github.com/fsnotify/fsnotify"
)

var supportedExtensions = map[string]bool{
	"md":   true,
	"txt":  true,
	"json": true,
}

// DirectoryWatcher watches a directory for new files.
//
// When SignalReceiver is set (ADR-0031), file events are delivered as
// domain.Signal to the ReactiveEngine; the enqueue path is bypassed.
//
// When SignalReceiver is nil, file events are fed directly into the ingestion
// pipeline via enqueue (ADR-0028 legacy path, preserved for backward compat).
type DirectoryWatcher struct {
	dir     string
	enqueue func(domain.ExternalDocument) bool
	// SignalReceiver routes file events to the ReactiveEngine (ADR-0031).
	// When nil, the legacy enqueue path is used.
	SignalReceiver domain.SignalReceiver
}

// NewDirectoryWatcher constructs a DirectoryWatcher. enqueue may be nil
// (useful in tests that only call Poll).
func NewDirectoryWatcher(dir string, enqueue func(domain.ExternalDocument) bool) *DirectoryWatcher {
	return &DirectoryWatcher{dir: dir, enqueue: enqueue}
}

// Name implements domain.IngestionAdapter.
func (dw *DirectoryWatcher) Name() string { return "directory_watcher" }

// Poll returns all supported files in the directory newer than since.
func (dw *DirectoryWatcher) Poll(_ context.Context, since time.Time) ([]domain.ExternalDocument, error) {
	entries, err := os.ReadDir(dw.dir)
	if err != nil {
		return nil, err
	}
	var docs []domain.ExternalDocument
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.TrimPrefix(filepath.Ext(entry.Name()), ".")
		if !supportedExtensions[ext] {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().After(since) {
			continue
		}
		fullPath := filepath.Join(dw.dir, entry.Name())
		content, err := os.ReadFile(fullPath)
		if err != nil {
			slog.Warn("DirectoryWatcher: failed to read file", "path", fullPath, "err", err)
			continue
		}
		docs = append(docs, domain.ExternalDocument{
			SourceURI:  fullPath,
			SourceType: ext,
			Title:      entry.Name(),
			Body:       string(content),
			Timestamp:  info.ModTime(),
		})
	}
	return docs, nil
}

// Stream implements domain.IngestionAdapter via fsnotify.
func (dw *DirectoryWatcher) Stream(ctx context.Context) (<-chan domain.ExternalDocument, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := watcher.Add(dw.dir); err != nil {
		watcher.Close()
		return nil, err
	}

	out := make(chan domain.ExternalDocument, 64)
	go func() {
		defer close(out)
		defer watcher.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&(fsnotify.Create|fsnotify.Write) == 0 {
					continue
				}
				ext := strings.TrimPrefix(filepath.Ext(event.Name), ".")
				if !supportedExtensions[ext] {
					continue
				}
				content, err := os.ReadFile(event.Name)
				if err != nil {
					slog.Warn("DirectoryWatcher: failed to read", "path", event.Name, "err", err)
					continue
				}
				doc := domain.ExternalDocument{
					SourceURI:  event.Name,
					SourceType: ext,
					Title:      filepath.Base(event.Name),
					Body:       string(content),
					Timestamp:  time.Now(),
				}
				select {
				case out <- doc:
				case <-ctx.Done():
					return
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Warn("DirectoryWatcher: fsnotify error", "err", err)
			}
		}
	}()
	return out, nil
}

// Start begins streaming files from the watched directory.
//
// When SignalReceiver is set, each file event is delivered as a domain.Signal
// to the ReactiveEngine (ADR-0031 path). When SignalReceiver is nil, files are
// delivered directly to the enqueue function (ADR-0028 legacy path).
func (dw *DirectoryWatcher) Start(ctx context.Context) {
	if dw.SignalReceiver == nil && dw.enqueue == nil {
		return
	}
	ch, err := dw.Stream(ctx)
	if err != nil {
		slog.Error("DirectoryWatcher: failed to start fsnotify watcher", "err", err)
		return
	}
	go func() {
		for doc := range ch {
			if dw.SignalReceiver != nil {
				sig := domain.Signal{
					StreamID:  dw.dir,
					FromAgent: "directory_watcher",
					Payload: map[string]any{
						"path":      doc.SourceURI,
						"extension": doc.SourceType,
						"mime_type": mimeForExt(doc.SourceType),
					},
					RawText:   doc.Title,
					Timestamp: doc.Timestamp,
				}
				if err := dw.SignalReceiver.OnSignal(ctx, sig); err != nil {
					slog.Warn("DirectoryWatcher: SignalReceiver error", "path", doc.SourceURI, "err", err)
				}
			} else if dw.enqueue != nil {
				if !dw.enqueue(doc) {
					slog.Warn("DirectoryWatcher: ingestion queue full, dropping file", "path", doc.SourceURI)
				}
			}
		}
	}()
}

// mimeForExt returns a basic MIME type for common file extensions.
func mimeForExt(ext string) string {
	switch ext {
	case "md", "txt":
		return "text/plain"
	case "json":
		return "application/json"
	case "html", "htm":
		return "text/html"
	case "pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}
