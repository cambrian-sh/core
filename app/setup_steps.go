// The setup steps (ADR-0122). Each is check-then-do and idempotent; failures
// mark the install degraded rather than aborting, so one re-run repairs a
// half-install. Order matters: config is written before migrate (RunMigrate
// loads the bundle via ResolveBaseDir), and migrate before start.
package app

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"time"

	"github.com/cambrian-sh/core/internal/config"
)

// stepInstall materializes the prefix layout and installs the running binary
// into <prefix>/bin, then chdirs to the prefix so ResolveBaseDir's CWD
// sentinel check resolves this bundle for migrate and the kernel boot.
func (s *setupState) stepInstall() bool {
	s.ui.section("1. Install")
	for _, d := range []string{"bin", "configs", "data", "agents", "logs", "db"} {
		if err := os.MkdirAll(filepath.Join(s.prefix, d), 0o755); err != nil {
			s.ui.line("fail", "cannot create "+filepath.Join(s.prefix, d), err.Error())
			return false
		}
	}
	s.ui.line("ok", "install prefix", s.prefix)

	dest := filepath.Join(s.prefix, "bin", exeName("cambrian-orchestrator"))
	s.binPath = dest
	exe, err := os.Executable()
	if err == nil {
		if r, e := filepath.EvalSymlinks(exe); e == nil {
			exe = r
		}
	}
	switch {
	case err != nil:
		s.warnf("cannot locate own executable", err.Error())
		s.binPath = ""
	case isSameFile(exe, dest):
		s.ui.line("ok", "kernel binary installed", dest)
	default:
		if cpErr := copyFile(exe, dest); cpErr != nil {
			if fileExists(dest) {
				// A running kernel holds the installed copy open (Windows); keep it.
				s.ui.line("warn", "kernel binary not refreshed", "installed copy is in use — stop the kernel and re-run setup to update it")
			} else {
				s.warnf("could not install binary into "+dest, cpErr.Error())
				s.binPath = exe
			}
		} else {
			s.ui.line("ok", "kernel binary installed", dest)
		}
	}

	if err := os.Chdir(s.prefix); err != nil {
		s.ui.line("fail", "cannot enter "+s.prefix, err.Error())
		return false
	}
	return true
}

// stepPreflight probes optional host tools. Nothing here degrades the install:
// docker only matters for the bundled local Postgres (the DB step degrades if
// the DB ends up unreachable), and a missing ollama only defers the embedder.
func (s *setupState) stepPreflight(ctx context.Context) {
	s.ui.section("\n2. Preflight")
	if v, ok := s.probe(ctx, "docker"); ok {
		s.dockerOK = true
		s.ui.line("ok", "docker", v)
	} else {
		s.ui.line("warn", "docker", "not found — needed only for the bundled local Postgres")
	}
	if v, ok := s.probe(ctx, "ollama"); ok {
		s.ollamaOK = true
		s.ui.line("ok", "ollama", v)
	} else {
		s.ui.line("warn", "ollama", "not found — the local embedder needs it; install it and run: ollama pull "+setupEmbedderModel)
	}
}

// stepDatabase choses where Postgres lives, brings it up if needed, and
// verifies pgvector availability (the migrations hard-require it).
func (s *setupState) stepDatabase(ctx context.Context) {
	s.ui.section("\n3. Database")
	where := s.ui.askChoice("Where should Cambrian's database live?", []setupChoice{
		{key: "1", label: "Local Docker Postgres — setup starts and manages it (recommended)"},
		{key: "2", label: "Existing / remote Postgres — you provide the connection (needs pgvector)"},
	}, "1")
	if where == "2" {
		s.db.host = s.ui.ask("Postgres host", s.db.host)
		s.db.port = s.ui.ask("Postgres port", s.db.port)
		s.db.dbname = s.ui.ask("database name", s.db.dbname)
		s.db.user = s.ui.ask("username", s.db.user)
		s.db.password = s.ui.ask("password (input is visible)", s.db.password)
	} else {
		s.db.user = s.ui.ask("Postgres user", s.db.user)
		s.db.password = s.ui.ask("Postgres password (input is visible)", s.db.password)
		s.db.dbname = s.ui.ask("database name", s.db.dbname)
	}

	// Highest-precedence koanf override: migrate and the spawned kernel read
	// these, so the DB connection works before the config bundle exists.
	os.Setenv("CAMBRIAN_DATABASE__HOST", s.db.host)
	os.Setenv("CAMBRIAN_DATABASE__PORT", s.db.port)
	os.Setenv("CAMBRIAN_DATABASE__USER", s.db.user)
	os.Setenv("CAMBRIAN_DATABASE__PASSWORD", s.db.password)
	os.Setenv("CAMBRIAN_DATABASE__DBNAME", s.db.dbname)

	if err := s.dbPing(ctx); err == nil {
		s.dbUp = true
		s.ui.line("ok", "Postgres reachable on "+s.db.host+":"+s.db.port, "")
	} else if where == "1" && s.dockerOK {
		compose := filepath.Join(s.prefix, "db", "docker-compose.yml")
		if werr := os.WriteFile(compose, []byte(setupComposeYML), 0o644); werr != nil {
			s.warnf("cannot write "+compose, werr.Error())
			return
		}
		os.Setenv("CAMBRIAN_DB_USER", s.db.user)
		os.Setenv("CAMBRIAN_DB_PASSWORD", s.db.password)
		os.Setenv("CAMBRIAN_DB_NAME", s.db.dbname)
		os.Setenv("CAMBRIAN_DB_PORT", s.db.port)
		fmt.Print("  starting Postgres via docker compose (cambrian-db)… ")
		out, ok := s.exec(ctx, 3*time.Minute, "docker", "compose", "-f", compose, "up", "-d", "cambrian-db")
		if ok {
			fmt.Println(s.ui.green("done"))
			// 60s: a FIRST install pulls the image and runs initdb + a config
			// restart before accepting connections — 30s was measured too short
			// on a real Fedora install (ADR-0122).
			s.dbUp = s.waitForDB(ctx, 60)
		} else {
			fmt.Println(s.ui.red("failed"))
			fmt.Println(s.ui.dim("     " + lastLines(out, 2)))
		}
		if s.dbUp {
			s.ui.line("ok", "Postgres up", "")
		} else {
			s.warnf("Postgres did not become ready", "inspect: docker logs cambrian-db")
		}
	} else {
		hint := "install docker, or choose an existing Postgres"
		if where == "2" {
			hint = "check the connection details"
		}
		s.warnf("Postgres not reachable at "+s.db.host+":"+s.db.port, hint)
	}

	if s.dbUp {
		ok, err := s.pgvectorAvailable(ctx)
		switch {
		case err != nil:
			s.warnf("could not check pgvector availability", err.Error())
		case ok:
			s.vectorOK = true
			s.ui.line("ok", "pgvector extension available", "")
		default:
			s.warnf("Postgres has no pgvector extension", "install pgvector (or use the pgvector/pgvector:pg16 image) — migrations need it")
		}
	}
	// The bundled Postgres runs in a container that carries a matching pg_dump;
	// the host often has none, or an older major that cannot dump pg16. Record
	// it so the config bundle routes the pre-migration snapshot (ENG-681) and
	// backups through it (backup.pg_container, ENG-1833).
	if where == "1" && s.dbUp && s.dockerOK {
		if out, ok := s.exec(ctx, 10*time.Second, "docker", "inspect", "-f", "{{.State.Running}}", setupDBContainer); ok && out == "true" {
			s.pgContainer = setupDBContainer
		}
	}
}

// stepPython provisions the agent runtime: agents ship inside the binary and
// are always materialized (the kernel scans exactly one metabolism.agents_dir);
// the Python environment half honors --skip-python. The environment is the
// kernel-managed one (ADR-0157 D7, ENG-2007): the same builder the kernel's
// boot check uses downloads the pinned standalone CPython and the pinned uv,
// verifies both against the embedded SHA-256 pin table, and installs the
// release's hashed base lock (agents/base.lock) wheels-only into an immutable
// generation under <prefix>/runtime/python. The host's Python is never used,
// so a machine with no Python at all works, and execution.agents.python in
// configs/config.json (mirror, index, wheelhouse, offline) applies here too.
func (s *setupState) stepPython(ctx context.Context) {
	s.ui.section("\n4. Python agent runtime")
	s.agentsDir = filepath.Join(s.prefix, "agents")
	if n, err := unpackAgents(s.agentsDir); err != nil {
		s.warnf("could not materialize agents", err.Error())
	} else {
		s.ui.line("ok", "agents materialized", fmt.Sprintf("%s  (%d files, embedded)", s.agentsDir, n))
	}
	promptsDir := filepath.Join(s.prefix, "prompts")
	if n, err := unpackPrompts(promptsDir); err != nil {
		s.warnf("could not materialize the prompt library", err.Error())
	} else {
		s.ui.line("ok", "prompt library materialized", fmt.Sprintf("%s  (%d files, embedded)", promptsDir, n))
	}

	configDir := filepath.Join(s.prefix, "configs")
	pc, err := config.ReadAgentPythonBlock(filepath.Join(configDir, "config.json"))
	if err != nil {
		s.warnf("execution.agents.python is invalid", err.Error())
		return
	}
	b, err := newPythonEnvBuilder(pc, s.prefix, configDir)
	if err != nil {
		s.warnf("sandbox Python env unavailable on this host", err.Error())
		return
	}
	if s.skipPython {
		if st, err := b.Check(ctx); err == nil && st.Usable() {
			s.envPy = st.Interpreter
		}
		s.ui.line("ok", "python runtime", "skipped (--skip-python)")
		return
	}
	b.Actor = "setup"
	if u, err := user.Current(); err == nil && u.Username != "" {
		b.Actor = "setup:" + u.Username
	}
	b.Progress = func(step string) { fmt.Println(s.ui.dim("     " + step)) }

	// A dev tree installs its local SDK as a wheel built from the tree (an
	// editable install would point agents at the repository, ADR-0157 D7.4.5);
	// a released machine installs the SDK the base lock pins, hash-checked.
	for _, cand := range []string{os.Getenv("CAMBRIAN_SDK_DIR"), filepath.Join(s.origCwd, "sdk"), filepath.Join(s.origCwd, "..", "sdk")} {
		if cand == "" || !fileExists(filepath.Join(cand, "pyproject.toml")) {
			continue
		}
		fmt.Println("  building the local agent SDK wheel (" + cand + ")…")
		wheel, err := b.BuildSDKWheel(ctx, cand)
		if err != nil {
			s.warnf("local SDK wheel not built", err.Error())
			return
		}
		b.SDKWheel = wheel
		s.ui.line("ok", "local agent SDK wheel", filepath.Base(wheel))
		break
	}

	fmt.Println("  building the sandbox Python env (pinned CPython " + b.Pins.Python.Version + ", hash-checked base lock)…")
	res, err := b.Build(ctx)
	if err != nil {
		s.warnf("sandbox Python env not built", err.Error()+" — fix and re-run: cambrian-orchestrator python-env build")
		return
	}
	s.envPy = res.Interpreter
	if res.Reused {
		s.ui.line("ok", "sandbox Python env present", res.Generation)
	} else {
		s.ui.line("ok", "sandbox Python env built", fmt.Sprintf("%s  (%d packages)", res.Generation, res.Packages))
	}

	// Per-agent import self-check (PLAT-01): name EXACTLY which agent is
	// missing which dist, not a vague "some import failed".
	out, _ := s.exec(ctx, time.Minute, s.envPy, "-c", setupSelfCheckPy, s.agentsDir)
	missing := parseSelfCheck(out)
	if len(missing) == 0 {
		s.ui.line("ok", "per-agent dependency self-check", "")
	} else {
		for _, m := range missing {
			s.ui.line("fail", "agent "+m.agent+" missing", m.mods)
		}
		s.degraded = true
	}
	s.ui.line("ok", "python runtime", s.envPy)
}

// stepJSRuntime provisions the JS agent runtime (ADR-0125 D7): probe node,
// probe/download bun (only downloaded when JS agent units actually exist under
// agents/ — a ~90MB fetch is not paid for a Python-only fleet), and run
// `bun install` for every agent package.json (agents root first — the union
// workspace). Node is never downloaded: probing is enough, bun covers the
// batteries-included path.
func (s *setupState) stepJSRuntime(ctx context.Context) {
	s.ui.section("\n5. JS agent runtime")
	if s.skipJS {
		s.ui.line("ok", "js runtime", "skipped (--skip-js)")
		return
	}

	if p, err := exec.LookPath("node"); err == nil {
		if v, ok := s.probe(ctx, "node"); ok {
			s.nodeBin = p
			s.ui.line("ok", "node", v)
		}
	}
	if s.nodeBin == "" {
		s.ui.line("warn", "node", "not found — needed only for runtime:\"node\" agents")
	}

	hasJS := hasJSAgents(s.agentsDir)
	bun := s.ensureBun(ctx, hasJS)
	switch {
	case bun != "":
		if v, ok := s.probe(ctx, bun); ok {
			s.ui.line("ok", "bun", v+"  ("+bun+")")
		} else {
			s.ui.line("ok", "bun", bun)
		}
	case hasJS:
		s.warnf("bun unavailable", "JS agents present but bun could not be provisioned — install bun and re-run setup")
		return
	default:
		s.ui.line("ok", "bun", "not provisioned (no JS agents)")
		return
	}

	for _, dir := range jsPackageDirs(s.agentsDir) {
		args := []string{"install"}
		if fileExists(filepath.Join(dir, "bun.lock")) || fileExists(filepath.Join(dir, "bun.lockb")) {
			args = append(args, "--frozen-lockfile")
		}
		fmt.Print("  bun install (" + dir + ")… ")
		out, ok := s.execIn(ctx, dir, 10*time.Minute, bun, args...)
		if ok {
			fmt.Println(s.ui.green("done"))
		} else {
			fmt.Println(s.ui.red("failed"))
			s.warnf("bun install failed in "+dir, lastLines(out, 2))
		}
	}
}

// stepModels pre-fetches the embedder/reranker/docling models so the first
// query isn't a multi-GB download mid-request. Everything here is non-fatal
// and never degrades the install — models download lazily on first use.
func (s *setupState) stepModels(ctx context.Context) {
	s.ui.section("\n6. Models")
	if s.skipModels {
		s.ui.line("ok", "model pre-fetch", "skipped (--skip-models)")
		return
	}
	if s.ollamaOK {
		out, _ := s.exec(ctx, 20*time.Second, "ollama", "list")
		if containsModel(out, setupEmbedderModel) {
			s.ui.line("ok", "embedder "+setupEmbedderModel+" present", "")
		} else {
			fmt.Print("  ollama pull " + setupEmbedderModel + "… ")
			if _, ok := s.exec(ctx, 15*time.Minute, "ollama", "pull", setupEmbedderModel); ok {
				fmt.Println(s.ui.green("done"))
			} else {
				fmt.Println(s.ui.yellow("skipped"))
				s.ui.line("warn", "could not pull "+setupEmbedderModel, "pull it later: ollama pull "+setupEmbedderModel)
			}
		}
	} else {
		s.ui.line("warn", "ollama missing — embedder model not pulled", "install ollama, then: ollama pull "+setupEmbedderModel)
	}
	if s.envPy != "" {
		// ADR-0157 S7: pre-fetch into the agents' shared model cache — the
		// directory the kernel points HF_HOME/TORCH_HOME/DOCLING_CACHE_DIR at
		// for every agent — not into this user's own cache, which a sandboxed
		// agent never sees.
		configPath := filepath.Join(s.prefix, "configs", "config.json")
		mcd, err := config.ReadModelCacheDir(configPath)
		if err != nil {
			s.ui.line("warn", "execution.agents.model_cache_dir unreadable; using the default", err.Error())
		}
		cache := config.AgentsConfig{ModelCacheDir: mcd}.ResolvedModelCacheDir(s.prefix)
		if err := os.MkdirAll(cache, 0o755); err != nil {
			s.ui.line("warn", "model cache not created; pre-fetch skipped", err.Error())
			return
		}
		env := append(os.Environ(), config.ModelCacheEnv(cache)...)
		rerank := os.Getenv("RERANK_MODEL")
		if rerank == "" {
			rerank = setupRerankModel
		}
		fmt.Print("  pre-fetching reranker (" + rerank + ")… ")
		if _, ok := s.execEnv(ctx, env, 15*time.Minute, s.envPy, "-c", "import sys; from huggingface_hub import snapshot_download as s; s(sys.argv[1])", rerank); ok {
			fmt.Println(s.ui.green("done"))
		} else {
			fmt.Println(s.ui.yellow("deferred"))
			s.ui.line("warn", "reranker pre-fetch deferred", "downloads on first use (a few hundred MB)")
		}
		if _, ok := s.execEnv(ctx, env, 15*time.Minute, s.envPy, "-m", "docling.cli.models", "download"); ok {
			s.ui.line("ok", "docling models pre-fetched", "")
		} else {
			s.ui.line("warn", "docling models deferred", "download on first document parse")
		}
	}
}

// stepConfig writes the kernel config bundle. LLM providers are intentionally
// left unconfigured — the kernel boots without them (ADR-0122 §deferral).
func (s *setupState) stepConfig() {
	s.ui.section("\n7. Config")
	if err := s.writeConfigBundle(); err != nil {
		s.warnf("could not write kernel config", err.Error())
		return
	}
	s.ui.line("ok", "kernel config bundle", filepath.Join(s.prefix, "configs")+"  (db + storage + embedder + interpreter)")
	s.ui.line("ok", "LLM providers", "not configured here — add keys from the operator console (or configs/providers.json + .env) whenever ready")
}

// stepMigrate applies DB migrations in-process via the ADR-0064 runner. The
// CWD is the prefix (stepInstall), so RunMigrate resolves this bundle —
// including backup.pg_container for the bundled Postgres (stepConfig), so a
// destructive migration on an upgrade is snapshotted with the container's
// pg_dump instead of refused on a host without one (ENG-1833).
func (s *setupState) stepMigrate(ctx context.Context) {
	if !s.dbUp {
		s.warnf("migrations skipped", "database not reachable")
		return
	}
	if !s.vectorOK {
		s.warnf("migrations skipped", "pgvector unavailable")
		return
	}
	fmt.Print("  applying DB migrations… ")
	if err := RunMigrate(ctx, []string{"up"}); err != nil {
		fmt.Println(s.ui.red("failed"))
		s.warnf("migrate up failed", err.Error())
	}
	// On success RunMigrate prints "migrations applied ✓" completing the line.
}

// stepOperator seeds the operator-console login (ADR-0047 D13). On a fresh
// install the first login is the env bootstrap operator,
// CAMBRIAN_OPERATOR_USER/PASSWORD, which the kernel re-asserts as an ADR-0142
// local account on every boot (buildAuthenticator); further persons are local
// accounts or an OIDC issuer, which a fresh install has neither of — so with the
// pair unset no login can succeed. Setup therefore writes them into
// <prefix>/.env, which app.Run loads from the resolved base dir before
// anything reads env. Secure-by-default is preserved: there is NO fixed
// default password — a non-interactive run generates a random one and prints
// it exactly once. Existing .env entries are never overwritten, so re-runs
// keep whatever the operator set (rotate by editing .env and restarting).
func (s *setupState) stepOperator() {
	s.ui.section("\n8. Operator account")
	envPath := filepath.Join(s.prefix, ".env")
	existing, err := readEnvFile(envPath)
	if err != nil {
		s.warnf("could not read "+envPath, err.Error())
		return
	}
	if existing["CAMBRIAN_OPERATOR_USER"] != "" && existing["CAMBRIAN_OPERATOR_PASSWORD"] != "" {
		s.ui.line("ok", "operator account present", "user "+existing["CAMBRIAN_OPERATOR_USER"]+"  ("+envPath+")")
		// Export for the kernel we are about to spawn, in case its .env load
		// is ever bypassed — same values, no behaviour change.
		os.Setenv("CAMBRIAN_OPERATOR_USER", existing["CAMBRIAN_OPERATOR_USER"])
		os.Setenv("CAMBRIAN_OPERATOR_PASSWORD", existing["CAMBRIAN_OPERATOR_PASSWORD"])
		return
	}

	user := os.Getenv("CAMBRIAN_OPERATOR_USER")
	if user == "" {
		user = "operator"
	}
	user = s.ui.ask("operator username", user)
	pass := os.Getenv("CAMBRIAN_OPERATOR_PASSWORD")
	generated := false
	if pass == "" {
		pass = s.ui.ask("operator password (input is visible; empty = generate)", "")
		if pass == "" {
			p, gerr := generatePassword()
			if gerr != nil {
				s.warnf("could not generate an operator password", gerr.Error())
				return
			}
			pass = p
			generated = true
		}
	}
	if err := upsertEnvFile(envPath, map[string]string{
		"CAMBRIAN_OPERATOR_USER":     user,
		"CAMBRIAN_OPERATOR_PASSWORD": pass,
	}); err != nil {
		s.warnf("could not write operator credentials", err.Error())
		return
	}
	os.Setenv("CAMBRIAN_OPERATOR_USER", user)
	os.Setenv("CAMBRIAN_OPERATOR_PASSWORD", pass)
	if generated {
		s.ui.line("ok", "operator account created", "user "+user)
		fmt.Println("  " + s.ui.bold("generated operator password: "+pass))
		fmt.Println(s.ui.dim("     saved in " + envPath + " — edit it and restart the kernel to rotate"))
	} else {
		s.ui.line("ok", "operator account created", "user "+user+"  → "+envPath)
	}
}

// stepStart launches the installed binary detached. By default it does NOT
// wait for the boot (owner decision 2026-08-11: setup must not block on a
// kernel restart) — it verifies only that the process survived spawning and
// leaves readiness to `status`. With --wait it polls the grpc.health.v1
// overall ("") status, which is DB-gated (ADR-0065) — SERVING proves the
// kernel booted against a working, migrated database.
func (s *setupState) stepStart(ctx context.Context) bool {
	s.ui.section("\n9. Kernel")
	if s.noStart {
		s.ui.line("ok", "not started (--no-start)", "start it later: "+s.binPath)
		return false
	}
	addr := net.JoinHostPort("localhost", s.serverPort)
	// "Already running" must mean OUR kernel: pid file first, then a health
	// probe. A bare open port proves nothing — it could be any process (or a
	// kernel from a different prefix), and reporting that as success would
	// claim an install is running when its kernel never started.
	if pid := readPidFile(s.prefix); pid != 0 && pidAlive(pid) {
		s.ui.line("ok", fmt.Sprintf("kernel already running (pid %d) on :%s", pid, s.serverPort), "left as-is; setup is idempotent")
		return true
	}
	if tcpOpen(addr, 1200*time.Millisecond) {
		if s.pollHealth(ctx, addr, 3*time.Second) {
			s.ui.line("ok", "a serving kernel already holds :"+s.serverPort, "not started from this prefix (no live pid file); left as-is")
			return true
		}
		s.warnf("port :"+s.serverPort+" is in use by another process", "stop it or change server.port in configs/config.json")
		return false
	}
	if s.degraded {
		// Don't start on a half-configured stack — it would just crash-loop.
		s.ui.line("warn", "not starting (setup had warnings)", "fix the items above, then re-run setup")
		return false
	}
	if s.binPath == "" || !fileExists(s.binPath) {
		s.warnf("kernel binary missing", "expected "+s.binPath)
		return false
	}
	pid, err := spawnDetached(s.binPath, s.prefix, filepath.Join(s.prefix, "logs"))
	if err != nil {
		s.warnf("could not start kernel", err.Error())
		return false
	}
	if werr := os.WriteFile(filepath.Join(s.prefix, "orchestrator.pid"), fmt.Appendf(nil, "%d\n", pid), 0o644); werr != nil {
		s.ui.line("warn", "could not write pid file", werr.Error())
	}
	if !s.waitReady {
		// Default: fire-and-forget. Setup's job ends at a successful spawn; the
		// boot proceeds in the background and `status` answers the readiness
		// question on demand. The one thing still checked is an INSTANT death —
		// a kernel that exits within moments (bad config, port stolen) must not
		// be reported as started. 3s: a config-validation refusal was measured
		// at ~2s and slipped past a 1.5s window.
		time.Sleep(3 * time.Second)
		if !pidAlive(pid) {
			s.warnf("kernel exited immediately after start",
				"inspect: "+filepath.Join(s.prefix, "logs", "orchestrator.err.log"))
			return false
		}
		s.ui.line("ok", fmt.Sprintf("kernel started (pid %d) on :%s", pid, s.serverPort),
			"booting in the background — verify: cambrian-orchestrator status  (--wait blocks until SERVING)")
		return true
	}
	fmt.Print("  waiting for readiness (grpc.health.v1)… ")
	if s.pollHealth(ctx, addr, 90*time.Second) {
		fmt.Println(s.ui.green("ready"))
		s.ui.line("ok", fmt.Sprintf("kernel running (pid %d) on :%s", pid, s.serverPort), "DB-gated readiness confirmed")
		return true
	}
	fmt.Println(s.ui.red("timeout"))
	s.warnf("kernel did not become ready in 90s", "logs: "+filepath.Join(s.prefix, "logs", "orchestrator.log"))
	return false
}
