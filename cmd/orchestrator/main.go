package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/cambrian-sh/cambrian-runtime/app"
	"github.com/cambrian-sh/cambrian-runtime/internal/config"
)

// version is set at build time: -ldflags "-X main.version=$(git describe --tags --always)".
// Defaults to "dev" for un-tagged local builds.
var version = "dev"

// main is a thin shell over app.Run — the composition root lives in the importable
// `app` package so a downstream (premium) binary can reuse the same bootstrap and
// inject proprietary components via app.Options. ADR-0057 (Model C).
func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version" || os.Args[1] == "version") {
		fmt.Println("cambrian-runtime", version)
		return
	}
	if err := app.Run(context.Background(), app.DefaultOptions()); err != nil {
		var cfgErr *config.ConfigError
		if errors.As(err, &cfgErr) {
			slog.Error("❌ Configuration error — fix the problem and restart", "field", cfgErr.Field, "detail", cfgErr.Message)
		} else {
			slog.Error("❌ Kernel Panic", "err", err)
		}
		os.Exit(1)
	}
}
