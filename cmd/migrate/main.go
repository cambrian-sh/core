package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/internal/infrastructure/postgres"
	"github.com/cambrian-sh/cambrian-runtime/internal/version"
)

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		slog.Error("load .env", "err", err)
		os.Exit(1)
	}
	configPath := os.Getenv("CAMBRIAN_CONFIG")
	if configPath == "" {
		configPath = "configs/config.json"
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	ctx := context.Background()
	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		slog.Error("connect to postgres", "err", err)
		os.Exit(1)
	}
	defer vec.Close()
	ver := version.Version()
	if err := vec.RecordSchemaVersion(ctx, ver); err != nil {
		slog.Error("record schema version", "err", err)
		os.Exit(1)
	}
	fmt.Println("Schema version recorded:", ver)
}
