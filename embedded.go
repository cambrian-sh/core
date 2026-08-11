// Package core is the module root: it carries the embedded assets a
// self-installing kernel binary needs (ADR-0122). The `setup` subcommand
// (app.RunSetup) materializes these into the install prefix, so a single
// downloaded binary can bootstrap a working deployment with no source tree.
//
// The `all:` prefix matters: Python packages carry `__init__.py`, and the
// default embed rules exclude underscore-prefixed files. `all:` keeps them;
// build junk (`__pycache__`, *.pyc) is filtered out at unpack time instead.
package core

import "embed"

// AgentsFS carries the Python agent sources (agents/*.py + agents/system/…),
// including the PLAT-01 union lockfile (agents/requirements.lock).
//
//go:embed all:agents
var AgentsFS embed.FS

// ConfigTemplatesFS carries the config-bundle templates: config.example.json
// (the seed for configs/config.json) and tuning.json (tuning defaults and the
// ResolveBaseDir sentinel).
//
//go:embed configs/config.example.json configs/tuning.json
var ConfigTemplatesFS embed.FS
