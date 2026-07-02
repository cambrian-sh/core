package config_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
)

func TestModelConfig_Unmarshal(t *testing.T) {
	raw := `{"provider":"openai","model":"gpt-4o","endpoint":"https://api.openai.com/v1","api_key_env":"OPENAI_API_KEY","cost_per_1m_input":5.0,"cost_per_1m_output":15.0,"timeout_ms":30000,"capabilities":["planning","text-generation"]}`
	var mc config.ModelConfig
	if err := json.Unmarshal([]byte(raw), &mc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mc.Provider != "openai" {
		t.Errorf("Provider: want openai, got %q", mc.Provider)
	}
	if mc.Model != "gpt-4o" {
		t.Errorf("Model: want gpt-4o, got %q", mc.Model)
	}
	if mc.CostPer1MInput != 5.0 {
		t.Errorf("CostPer1MInput: want 5.0, got %v", mc.CostPer1MInput)
	}
	if mc.CostPer1MOutput != 15.0 {
		t.Errorf("CostPer1MOutput: want 15.0, got %v", mc.CostPer1MOutput)
	}
	if mc.TimeoutMs != 30000 {
		t.Errorf("TimeoutMs: want 30000, got %d", mc.TimeoutMs)
	}
	if len(mc.Capabilities) != 2 {
		t.Errorf("Capabilities: want 2, got %d", len(mc.Capabilities))
	}
}

func TestModelConfig_DefaultsApplied(t *testing.T) {
	raw := `{"provider":"ollama","model":"llama3"}`
	var mc config.ModelConfig
	if err := json.Unmarshal([]byte(raw), &mc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mc.TimeoutMs != 0 {
		t.Errorf("TimeoutMs: want 0 (zero value before LoadConfig), got %d", mc.TimeoutMs)
	}
	if mc.CostPer1MInput != 0 {
		t.Errorf("CostPer1MInput: want 0, got %v", mc.CostPer1MInput)
	}
}

// NOTE: ADR-0042 removed the top-level llm{} block and models[] array. Generator
// parsing + timeout/health defaults are now covered by llm_provider_config_test.go
// (TestLoadConfig_LLMProvider_RoundTripAndDefaults). ModelConfig struct round-trip
// is still covered by TestModelConfig_* above (the streaming path uses it).

func TestConfig_LoadConfig_EnvVarOverridesFileValue(t *testing.T) {
	// New convention (ADR-0024): env vars use CAMBRIAN_ prefix with __ as hierarchy separator.
	// No ${VAR} placeholders in files — env.Provider injects values directly.
	t.Setenv("CAMBRIAN_METABOLISM__PYTHON_EXECUTABLE", "/ci/python")
	t.Setenv("CAMBRIAN_METABOLISM__AGENTS_DIR", "/ci/agents/")
	t.Setenv("CAMBRIAN_STORAGE__DATA_DIR", "/ci/data")

	jsonStr := `{
		"llm": {"endpoint":"http://localhost:11434","model":"llama3"},
		"metabolism": {"python_executable":"/default/python","agents_dir":"/default/agents"},
		"storage": {"data_dir":"/default/data","db_name":"test.db"},
		"database": {"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},
		"server": {"port":"50051"}
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(jsonStr), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Metabolism.PythonExecutable != "/ci/python" {
		t.Errorf("PythonExecutable: want /ci/python, got %q", cfg.Metabolism.PythonExecutable)
	}
	if cfg.Metabolism.AgentsDir != "/ci/agents/" {
		t.Errorf("AgentsDir: want /ci/agents/, got %q", cfg.Metabolism.AgentsDir)
	}
	if cfg.Storage.DataDir != "/ci/data" {
		t.Errorf("DataDir: want /ci/data, got %q", cfg.Storage.DataDir)
	}
}

func TestLoadConfig_CapabilityCluster_DefaultsApplied(t *testing.T) {
	jsonStr := `{
		"llm": {"endpoint":"http://localhost:11434","model":"llama3"},
		"metabolism": {"python_executable":"python","agents_dir":"agents"},
		"storage": {"data_dir":"data","db_name":"test.db"},
		"database": {"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},
		"server": {"port":"50051"},
		"execution": {}
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(jsonStr), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	ex := cfg.Execution
	if ex.CapabilityClusterThreshold != 0.80 {
		t.Errorf("CapabilityClusterThreshold: want 0.80, got %v", ex.CapabilityClusterThreshold)
	}
	if ex.CapabilityClusterEpsilon != 0.02 {
		t.Errorf("CapabilityClusterEpsilon: want 0.02, got %v", ex.CapabilityClusterEpsilon)
	}
	if ex.CapabilityClusterMinAgents != 3 {
		t.Errorf("CapabilityClusterMinAgents: want 3, got %v", ex.CapabilityClusterMinAgents)
	}
	if ex.CapabilityClusterIntervalSeconds != 3600 {
		t.Errorf("CapabilityClusterIntervalSeconds: want 3600, got %v", ex.CapabilityClusterIntervalSeconds)
	}
}

func TestExecutionConfig_Validate_ModelConfigs(t *testing.T) {
	cfg := config.ExecutionConfig{
		StepTimeoutMultiplier:       2.0,
		PlanTimeoutMs:               120000,
		EWMAAlpha:                   0.5,
		GatekeeperW1:                0.4,
		GatekeeperW2:                0.4,
		GatekeeperW3:                0.2,
		TrustScoreCalWeight:         0.6,
		TrustScoreAbsWeight:         0.4,
		MinAuctionConfidence:        0.3,
		MaxReplanAttempts:           2,
		MaxPartialContextBytes:      51200,
		FallbackConfidenceThreshold: 0.4,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config should not error: %v", err)
	}
}

func TestExecutionConfig_ADR0018Defaults(t *testing.T) {
	cfg := config.ExecutionConfig{}
	if cfg.LLMGatewayMaxConcurrency != 0 {
		t.Error("LLMGatewayMaxConcurrency zero value should be 0 before LoadConfig")
	}
	if cfg.LLMGatewayRetryBackoffMs != 0 {
		t.Error("LLMGatewayRetryBackoffMs zero value should be 0 before LoadConfig")
	}
	if cfg.SessionTokenSweepIntervalSeconds != 0 {
		t.Error("SessionTokenSweepIntervalSeconds zero value should be 0 before LoadConfig")
	}
	if cfg.SessionTokenTTLMultiplier != 0 {
		t.Error("SessionTokenTTLMultiplier zero value should be 0 before LoadConfig")
	}
	if cfg.BudgetExhaustionAlarmRate != 0 {
		t.Error("BudgetExhaustionAlarmRate zero value should be 0 before LoadConfig")
	}
	if cfg.MinStepEnergy != 0 {
		t.Error("MinStepEnergy zero value should be 0 before LoadConfig")
	}
	if cfg.MaxStepEnergy != 0 {
		t.Error("MaxStepEnergy zero value should be 0 before LoadConfig")
	}
	if cfg.HistogramMinSamples != 0 {
		t.Error("HistogramMinSamples zero value should be 0 before LoadConfig")
	}
	if cfg.HistogramAlpha != 0 {
		t.Error("HistogramAlpha zero value should be 0 before LoadConfig")
	}
}

func TestTelemetryConfig_Unmarshal(t *testing.T) {
	raw := `{"telemetry":{"otlp_endpoint":"collector:4317","trace_sampling_rate":0.1,"metrics_export_interval_seconds":15,"enable_stdout_exporter":true,"prometheus_port":8080}}`
	var cfg struct {
		Telemetry config.TelemetryConfig `json:"telemetry"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Telemetry.OTLPEndpoint != "collector:4317" {
		t.Errorf("OTLPEndpoint: want collector:4317, got %q", cfg.Telemetry.OTLPEndpoint)
	}
	if cfg.Telemetry.TraceSamplingRate != 0.1 {
		t.Errorf("TraceSamplingRate: want 0.1, got %v", cfg.Telemetry.TraceSamplingRate)
	}
	if cfg.Telemetry.MetricsExportIntervalSeconds != 15 {
		t.Errorf("MetricsExportIntervalSeconds: want 15, got %d", cfg.Telemetry.MetricsExportIntervalSeconds)
	}
	if !cfg.Telemetry.EnableStdoutExporter {
		t.Error("EnableStdoutExporter: want true")
	}
	if cfg.Telemetry.PrometheusPort != 8080 {
		t.Errorf("PrometheusPort: want 8080, got %d", cfg.Telemetry.PrometheusPort)
	}
}

func TestConfigError_DetectableViaErrorsAs(t *testing.T) {
	invalid := config.ExecutionConfig{
		PlanTimeoutMs:       0, // below minimum of 1000 — triggers validation error
		EWMAAlpha:           0.5,
		TrustScoreCalWeight: 0.6,
		TrustScoreAbsWeight: 0.4,
	}
	err := invalid.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	var cfgErr *config.ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *config.ConfigError via errors.As, got %T", err)
	}
	if cfgErr.Field == "" {
		t.Error("ConfigError.Field should be non-empty")
	}
	if cfgErr.Message == "" {
		t.Error("ConfigError.Message should be non-empty")
	}
}

func TestConfigError_MultipleFailuresJoined(t *testing.T) {
	invalid := config.ExecutionConfig{
		PlanTimeoutMs:       0,   // < 1000
		EWMAAlpha:           2.0, // > 1
		TrustScoreCalWeight: 0.6,
		TrustScoreAbsWeight: 0.4,
	}
	err := invalid.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	var cfgErr *config.ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *config.ConfigError, got %T", err)
	}
	// Both violations must appear in a single error message.
	if !strings.Contains(cfgErr.Message, "plan_timeout_ms") {
		t.Errorf("message missing plan_timeout_ms violation: %q", cfgErr.Message)
	}
	if !strings.Contains(cfgErr.Message, "ewma_alpha") {
		t.Errorf("message missing ewma_alpha violation: %q", cfgErr.Message)
	}
}

func TestLoadConfig_LocalOverride_WinsOverBaseFile(t *testing.T) {
	base := `{"embedder":{"endpoint":"http://base:11434","model":"base-model"},"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	local := `{"embedder":{"model":"local-model"}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	localPath := filepath.Join(dir, "config.local.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(localPath, []byte(local), 0644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Embedder.Model != "local-model" {
		t.Errorf("Embedder.Model: want local-model, got %q", cfg.Embedder.Model)
	}
	// Base value must still be present for non-overridden fields.
	if cfg.Embedder.Endpoint != "http://base:11434" {
		t.Errorf("Embedder.Endpoint: want http://base:11434, got %q", cfg.Embedder.Endpoint)
	}
}

func TestLoadConfig_MissingLocalFile_NotAnError(t *testing.T) {
	base := `{"llm":{"endpoint":"http://localhost:11434"},"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// No config.local.json written — must not error.
	if _, err := config.LoadConfig(path); err != nil {
		t.Errorf("LoadConfig with no local file should not error: %v", err)
	}
}

func TestLoadConfig_EnvVar_WinsOverLocalFile(t *testing.T) {
	t.Setenv("CAMBRIAN_EMBEDDER__MODEL", "env-model")
	base := `{"embedder":{"endpoint":"http://localhost:11434","model":"base-model"},"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	local := `{"embedder":{"model":"local-model"}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.local.json"), []byte(local), 0644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Embedder.Model != "env-model" {
		t.Errorf("Embedder.Model: want env-model, got %q", cfg.Embedder.Model)
	}
}

func TestLoadConfig_EnvVar_SetsSecretField(t *testing.T) {
	t.Setenv("CAMBRIAN_DATABASE__PASSWORD", "injected-secret")
	jsonStr := `{"llm":{"endpoint":"http://localhost:11434"},"database":{"host":"h","user":"u","password":"","dbname":"d"},"server":{"port":"50051"}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(jsonStr), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Database.Password != "injected-secret" {
		t.Errorf("Database.Password: want injected-secret, got %q", cfg.Database.Password)
	}
}

func TestLoadConfig_TypeMismatch_WrapsAsConfigError(t *testing.T) {
	// An object where a scalar is expected causes a JSON parse error at the
	// Koanf file provider layer, which is wrapped as *ConfigError{Field:"unmarshal"}.
	jsonStr := `{"llm":{"endpoint":"http://localhost:11434"},"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":{"nested":"bad"}}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(jsonStr), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := config.LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for type mismatch, got nil")
	}
	var cfgErr *config.ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *config.ConfigError, got %T: %v", err, err)
	}
	if cfgErr.Field != "unmarshal" {
		t.Errorf("ConfigError.Field: want unmarshal, got %q", cfgErr.Field)
	}
}

func TestLoadConfig_MinimalFile_DefaultsPreserved(t *testing.T) {
	// Only llm.endpoint set — all execution fields must come from DefaultConfig.
	jsonStr := `{"llm":{"endpoint":"http://localhost:11434"},"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(jsonStr), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	def := config.DefaultConfig()
	if cfg.Execution.EWMAAlpha != def.Execution.EWMAAlpha {
		t.Errorf("EWMAAlpha: want %v, got %v", def.Execution.EWMAAlpha, cfg.Execution.EWMAAlpha)
	}
	if cfg.Execution.PlanTimeoutMs != def.Execution.PlanTimeoutMs {
		t.Errorf("PlanTimeoutMs: want %v, got %v", def.Execution.PlanTimeoutMs, cfg.Execution.PlanTimeoutMs)
	}
	if cfg.Execution.FallbackEnabled != def.Execution.FallbackEnabled {
		t.Errorf("FallbackEnabled: want %v, got %v", def.Execution.FallbackEnabled, cfg.Execution.FallbackEnabled)
	}
}

func TestLoadConfig_MissingPassword_ReturnsConfigError(t *testing.T) {
	// No password in file, no env var — validator must fire.
	jsonStr := `{"llm":{"endpoint":"http://localhost:11434"},"database":{"host":"localhost","user":"u","password":"","dbname":"d"},"server":{"port":"50051"}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(jsonStr), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := config.LoadConfig(path)
	if err == nil {
		t.Fatal("expected ConfigError for missing database.password, got nil")
	}
	var cfgErr *config.ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *config.ConfigError, got %T", err)
	}
	if !strings.Contains(cfgErr.Message, "database.password") {
		t.Errorf("error message should mention database.password, got: %q", cfgErr.Message)
	}
}

// TestLoadConfig_Langfuse_ConditionalValidation removed — Langfuse is a premium
// feature (ADR-0057) and is no longer part of the OSS config schema.

func TestLoadConfig_FallbackEnabled_CanBeSetFalse(t *testing.T) {
	// Regression guard: the old if !cfg.Execution.FallbackEnabled guard made
	// it impossible to explicitly set fallback_enabled: false in config.
	jsonStr := `{"llm":{"endpoint":"http://localhost:11434"},"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"},"execution":{"fallback_enabled":false}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(jsonStr), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Execution.FallbackEnabled {
		t.Error("FallbackEnabled: explicit false in config must not be overridden to true")
	}
}

func TestDefaultConfig_AgentPool_DefaultTimeout(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.AgentPool.DefaultAgentTimeoutMs != 30000 {
		t.Errorf("AgentPool.DefaultAgentTimeoutMs: want 30000, got %d", cfg.AgentPool.DefaultAgentTimeoutMs)
	}
}

func TestLoadConfig_AgentPool_EnvOverride(t *testing.T) {
	t.Setenv("CAMBRIAN_AGENT_POOL__DEFAULT_AGENT_TIMEOUT_MS", "5000")
	jsonStr := `{"llm":{"endpoint":"http://localhost:11434"},"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(jsonStr), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AgentPool.DefaultAgentTimeoutMs != 5000 {
		t.Errorf("AgentPool.DefaultAgentTimeoutMs: want 5000, got %d", cfg.AgentPool.DefaultAgentTimeoutMs)
	}
}

func TestDefaultConfig_KnownValues(t *testing.T) {
	cfg := config.DefaultConfig()
	ex := cfg.Execution
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"EWMAAlpha", ex.EWMAAlpha, 0.5},
		{"StepTimeoutMultiplier", ex.StepTimeoutMultiplier, 2.0},
		{"PlanTimeoutMs", ex.PlanTimeoutMs, 120000},
		{"FallbackEnabled", ex.FallbackEnabled, true},
		{"GatekeeperW1", ex.GatekeeperW1, 0.4},
		{"GatekeeperW4", ex.GatekeeperW4, 0.15},
		{"CrossVerifyRate", ex.CrossVerifyRate, 0.05},
		{"Graph.DecayFactor", ex.Graph.DecayFactor, 0.75},
		{"Telemetry.TraceSamplingRate", cfg.Telemetry.TraceSamplingRate, 1.0},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: want %v, got %v", tc.name, tc.want, tc.got)
		}
	}
}

// ADR-0037 / 0037-01: the ResourceSelector flag defaults to the auction
// (status quo control) at 0% EFE traffic for safe rollout.
func TestDefaultConfig_ADR0037SelectorDefaults(t *testing.T) {
	ex := config.DefaultConfig().Execution
	if ex.ResourceSelector != "auction" {
		t.Errorf("ResourceSelector = %q, want auction", ex.ResourceSelector)
	}
	if ex.EFETrafficPercent != 0 {
		t.Errorf("EFETrafficPercent = %v, want 0 (safe rollout)", ex.EFETrafficPercent)
	}
	if ex.EFEExplorationBonus <= 0 {
		t.Errorf("EFEExplorationBonus = %v, want > 0", ex.EFEExplorationBonus)
	}
}

func TestExecutionConfig_Validate_RejectsUnknownSelector(t *testing.T) {
	ex := config.DefaultConfig().Execution
	ex.ResourceSelector = "bogus"
	if err := ex.Validate(); err == nil {
		t.Error("Validate() should reject an unknown ResourceSelector value")
	}
	for _, ok := range []string{"auction", "efe", "auto"} {
		ex.ResourceSelector = ok
		if err := ex.Validate(); err != nil {
			t.Errorf("Validate() rejected valid selector %q: %v", ok, err)
		}
	}
}

func TestDefaultConfig_PassesValidation(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg == nil {
		t.Fatal("DefaultConfig() returned nil")
	}
	if err := cfg.Execution.Validate(); err != nil {
		t.Errorf("DefaultConfig() should produce a valid config, got: %v", err)
	}
}

// ADR-0057 (three-tier config): the former TestDefaultConfig_ReactiveEngineDefaults
// moved out — ReactiveEngine tuning is premium config and is tested in the premium repo.

func TestTelemetryConfig_ZeroValueDisabled(t *testing.T) {
	var tc config.TelemetryConfig
	if tc.OTLPEndpoint != "" {
		t.Error("OTLPEndpoint zero value should be empty")
	}
	if tc.TraceSamplingRate != 0 {
		t.Errorf("TraceSamplingRate zero value should be 0, got %v", tc.TraceSamplingRate)
	}
	if tc.MetricsExportIntervalSeconds != 0 {
		t.Error("MetricsExportIntervalSeconds zero value should be 0")
	}
	if tc.EnableStdoutExporter {
		t.Error("EnableStdoutExporter zero value should be false")
	}
	if tc.PrometheusPort != 0 {
		t.Error("PrometheusPort zero value should be 0")
	}
}

// ----------------------------------------------------------------------------
// Config split: 7-layer pipeline tests (.omo/plans/config-split.md)
// ----------------------------------------------------------------------------
//
// Each test uses t.TempDir() and places every applicable layer file inside
// the same temp dir. The primary `path` argument is always <dir>/config.json;
// secondary paths (tuning.json, tuning.local.json, mcp.json, config.local.json)
// are derived from filepath.Dir(path) — NOT from the process CWD.

// TestLoadConfig_TuningOverridesDefaults verifies that a single field in
// configs/tuning.json (layer 2) wins over the Go default from DefaultConfig().
func TestLoadConfig_TuningOverridesDefaults(t *testing.T) {
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	tuning := `{"execution":{"ewma_alpha":0.99}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tuning.json"), []byte(tuning), 0644); err != nil {
		t.Fatalf("write tuning: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Execution.EWMAAlpha != 0.99 {
		t.Errorf("EWMAAlpha: want 0.99 (from tuning.json), got %v", cfg.Execution.EWMAAlpha)
	}
	// Untuned fields must fall through to DefaultConfig().
	def := config.DefaultConfig()
	if cfg.Execution.PlanTimeoutMs != def.Execution.PlanTimeoutMs {
		t.Errorf("PlanTimeoutMs: want %v (default), got %v", def.Execution.PlanTimeoutMs, cfg.Execution.PlanTimeoutMs)
	}
}

// TestLoadConfig_TuningLocalOverridesTuning verifies that configs/tuning.local.json
// (layer 3) wins over configs/tuning.json (layer 2) when both are present.
func TestLoadConfig_TuningLocalOverridesTuning(t *testing.T) {
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	tuning := `{"execution":{"ewma_alpha":0.7}}`
	local := `{"execution":{"ewma_alpha":0.3}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tuning.json"), []byte(tuning), 0644); err != nil {
		t.Fatalf("write tuning: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tuning.local.json"), []byte(local), 0644); err != nil {
		t.Fatalf("write tuning.local: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Execution.EWMAAlpha != 0.3 {
		t.Errorf("EWMAAlpha: want 0.3 (from tuning.local.json, wins over tuning.json), got %v", cfg.Execution.EWMAAlpha)
	}
}

// TestLoadConfig_TuningAbsent_DoesNotAffectDefaults verifies that when neither
// tuning.json nor tuning.local.json exist, all execution fields still come
// from DefaultConfig() (no zero values, no errors).
func TestLoadConfig_TuningAbsent_DoesNotAffectDefaults(t *testing.T) {
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	// Deliberately write NO tuning.json / tuning.local.json.
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	def := config.DefaultConfig()
	if cfg.Execution.EWMAAlpha != def.Execution.EWMAAlpha {
		t.Errorf("EWMAAlpha: want %v (default), got %v", def.Execution.EWMAAlpha, cfg.Execution.EWMAAlpha)
	}
	if cfg.Execution.PlanTimeoutMs != def.Execution.PlanTimeoutMs {
		t.Errorf("PlanTimeoutMs: want %v (default), got %v", def.Execution.PlanTimeoutMs, cfg.Execution.PlanTimeoutMs)
	}
	if cfg.Execution.KG2RAGEnabled != def.Execution.KG2RAGEnabled {
		t.Errorf("KG2RAGEnabled: want %v (default), got %v", def.Execution.KG2RAGEnabled, cfg.Execution.KG2RAGEnabled)
	}
}

// TestLoadConfig_MCPFromSeparateFile verifies that configs/mcp.json (layer 6)
// populates cfg.MCP.Servers when present.
func TestLoadConfig_MCPFromSeparateFile(t *testing.T) {
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	mcp := `{"mcp":{"servers":[{"id":"fs","transport":"stdio","endpoint":"./bin/fs-mcp","auth":{"type":"none"}}]}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(mcp), 0644); err != nil {
		t.Fatalf("write mcp: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.MCP.Servers) != 1 {
		t.Fatalf("MCP.Servers: want 1, got %d", len(cfg.MCP.Servers))
	}
	if cfg.MCP.Servers[0].ID != "fs" {
		t.Errorf("MCP.Servers[0].ID: want fs, got %q", cfg.MCP.Servers[0].ID)
	}
	if cfg.MCP.Servers[0].Endpoint != "./bin/fs-mcp" {
		t.Errorf("MCP.Servers[0].Endpoint: want ./bin/fs-mcp, got %q", cfg.MCP.Servers[0].Endpoint)
	}
}

// TestLoadConfig_MCPInConfigJSONWhenNoMCPFile verifies the backward-compatibility
// case: with no mcp.json, the `mcp` block inside config.json still loads.
func TestLoadConfig_MCPInConfigJSONWhenNoMCPFile(t *testing.T) {
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"},"mcp":{"servers":[{"id":"from-config","transport":"stdio","endpoint":"./bin/x","auth":{"type":"none"}}]}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.MCP.Servers) != 1 {
		t.Fatalf("MCP.Servers: want 1, got %d", len(cfg.MCP.Servers))
	}
	if cfg.MCP.Servers[0].ID != "from-config" {
		t.Errorf("MCP.Servers[0].ID: want from-config, got %q", cfg.MCP.Servers[0].ID)
	}
}

// TestLoadConfig_MCPFileOverridesConfigJSON verifies the documented precedence:
// when both config.json and mcp.json define an `mcp` block, mcp.json (layer 6)
// wins over config.json (layer 4).
func TestLoadConfig_MCPFileOverridesConfigJSON(t *testing.T) {
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"},"mcp":{"servers":[{"id":"from-config","transport":"stdio","endpoint":"./bin/a","auth":{"type":"none"}}]}}`
	mcp := `{"mcp":{"servers":[{"id":"from-mcp-file","transport":"stdio","endpoint":"./bin/b","auth":{"type":"none"}}]}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(mcp), 0644); err != nil {
		t.Fatalf("write mcp: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.MCP.Servers) != 1 {
		t.Fatalf("MCP.Servers: want 1 (mcp.json wins), got %d", len(cfg.MCP.Servers))
	}
	if cfg.MCP.Servers[0].ID != "from-mcp-file" {
		t.Errorf("MCP.Servers[0].ID: want from-mcp-file (mcp.json wins), got %q", cfg.MCP.Servers[0].ID)
	}
}

// TestLoadConfig_AllLayers writes one field per layer and asserts the 7-layer
// precedence: env (7) > mcp.json (6) > config.local.json (5) > config.json (4)
// > tuning.local.json (3) > tuning.json (2) > Go defaults (1).
func TestLoadConfig_AllLayers(t *testing.T) {
	t.Setenv("CAMBRIAN_EXECUTION__EWMA_ALPHA", "0.11")
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"},"execution":{"ewma_alpha":0.22}}`
	tuning := `{"execution":{"ewma_alpha":0.33}}`
	tuningLocal := `{"execution":{"ewma_alpha":0.44}}`
	localCfg := `{"execution":{"ewma_alpha":0.55}}`
	mcp := `{"mcp":{"default_session_budget":7.0,"servers":[]}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	localPath := filepath.Join(dir, "config.local.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tuning.json"), []byte(tuning), 0644); err != nil {
		t.Fatalf("write tuning: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tuning.local.json"), []byte(tuningLocal), 0644); err != nil {
		t.Fatalf("write tuning.local: %v", err)
	}
	if err := os.WriteFile(localPath, []byte(localCfg), 0644); err != nil {
		t.Fatalf("write config.local: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(mcp), 0644); err != nil {
		t.Fatalf("write mcp: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Layer 7 (env) wins: 0.11.
	if cfg.Execution.EWMAAlpha != 0.11 {
		t.Errorf("EWMAAlpha: want 0.11 (env wins), got %v", cfg.Execution.EWMAAlpha)
	}
	// MCP layer (6) populates cfg.MCP.DefaultSessionBudget.
	if cfg.MCP.DefaultSessionBudget != 7.0 {
		t.Errorf("MCP.DefaultSessionBudget: want 7.0 (from mcp.json), got %v", cfg.MCP.DefaultSessionBudget)
	}
}

// TestLoadConfig_PathDerivation_NotCWD is the regression test for Momus Critical #1.
// It verifies that tuning.json / mcp.json are derived from filepath.Dir(path),
// NOT from the process CWD. The test changes the process CWD into a junk dir
// that does NOT contain a tuning.json, then asserts that a tuning.json next to
// the primary path argument is still loaded. If derivation accidentally fell
// back to CWD, the tuning would be missing and the test would fail.
func TestLoadConfig_PathDerivation_NotCWD(t *testing.T) {
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	tuning := `{"execution":{"ewma_alpha":0.77}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tuning.json"), []byte(tuning), 0644); err != nil {
		t.Fatalf("write tuning: %v", err)
	}
	// Save the current CWD, chdir into a junk dir that does NOT contain a
	// tuning.json, restore on exit. If LoadConfig incorrectly used CWD for
	// path derivation, the tuning.json at `<dir>/tuning.json` would be
	// invisible and the test would observe the default EWMAAlpha.
	origCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	junk := t.TempDir()
	if err := os.Chdir(junk); err != nil {
		t.Fatalf("chdir junk: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCWD) })

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Execution.EWMAAlpha != 0.77 {
		t.Errorf("EWMAAlpha: want 0.77 (from <dir>/tuning.json, path-derived not CWD), got %v", cfg.Execution.EWMAAlpha)
	}
}

// TestLoadConfig_EmbedderFromSeparateFile verifies that configs/embedder.json
// (layer 6) populates cfg.Embedder when present.
func TestLoadConfig_EmbedderFromSeparateFile(t *testing.T) {
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"},"embedder":{"provider":"openai","model":"text-embedding-3-small","endpoint":"https://api.openai.com/v1","dimensions":1536}}`
	embedder := `{"embedder":{"provider":"ollama","model":"nomic-embed-text","endpoint":"http://localhost:11434","dimensions":768}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "embedder.json"), []byte(embedder), 0644); err != nil {
		t.Fatalf("write embedder: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Embedder.Provider != "ollama" {
		t.Errorf("Embedder.Provider: want ollama (from embedder.json), got %q", cfg.Embedder.Provider)
	}
	if cfg.Embedder.Model != "nomic-embed-text" {
		t.Errorf("Embedder.Model: want nomic-embed-text (from embedder.json), got %q", cfg.Embedder.Model)
	}
	if cfg.Embedder.Dimensions != 768 {
		t.Errorf("Embedder.Dimensions: want 768 (from embedder.json), got %d", cfg.Embedder.Dimensions)
	}
}

// TestLoadConfig_EmbedderLocalOverridesEmbedder verifies that configs/embedder.local.json
// (layer 7) wins over configs/embedder.json (layer 6) when both are present.
func TestLoadConfig_EmbedderLocalOverridesEmbedder(t *testing.T) {
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	embedder := `{"embedder":{"provider":"ollama","model":"bge-large","endpoint":"http://localhost:11434","dimensions":1024}}`
	local := `{"embedder":{"model":"nomic-embed-text","dimensions":768}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "embedder.json"), []byte(embedder), 0644); err != nil {
		t.Fatalf("write embedder: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "embedder.local.json"), []byte(local), 0644); err != nil {
		t.Fatalf("write embedder.local: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// model + dimensions come from local; provider + endpoint come from base.
	if cfg.Embedder.Model != "nomic-embed-text" {
		t.Errorf("Embedder.Model: want nomic-embed-text (from embedder.local.json), got %q", cfg.Embedder.Model)
	}
	if cfg.Embedder.Dimensions != 768 {
		t.Errorf("Embedder.Dimensions: want 768 (from embedder.local.json), got %d", cfg.Embedder.Dimensions)
	}
	if cfg.Embedder.Provider != "ollama" {
		t.Errorf("Embedder.Provider: want ollama (from embedder.json, not overridden by local), got %q", cfg.Embedder.Provider)
	}
}

// TestLoadConfig_ProvidersFromSeparateFile verifies that configs/providers.json
// (layer 8) populates cfg.LLMProvider when present.
func TestLoadConfig_ProvidersFromSeparateFile(t *testing.T) {
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`
	embedder := `{"embedder":{"provider":"ollama","model":"nomic-embed-text","endpoint":"http://localhost:11434","dimensions":768}}`
	providers := `{"llm_provider":{
		"default": "qwen-local",
		"generators": [
			{"id":"qwen-local","provider":"ollama","model":"qwen3:8b","endpoint":"http://localhost:11434"},
			{"id":"deepseek","provider":"openai","model":"deepseek-v4-flash","endpoint":"https://x/v1","api_key_env":"OPENCODE_API_KEY"}
		],
		"roles": {"router":"qwen-local","planner":"deepseek"}
	}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "embedder.json"), []byte(embedder), 0644); err != nil {
		t.Fatalf("write embedder: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "providers.json"), []byte(providers), 0644); err != nil {
		t.Fatalf("write providers: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.LLMProvider.Default != "qwen-local" {
		t.Errorf("LLMProvider.Default: want qwen-local (from providers.json), got %q", cfg.LLMProvider.Default)
	}
	if len(cfg.LLMProvider.Generators) != 2 {
		t.Fatalf("LLMProvider.Generators: want 2, got %d", len(cfg.LLMProvider.Generators))
	}
	if cfg.LLMProvider.Generators[0].ID != "qwen-local" {
		t.Errorf("Generators[0].ID: want qwen-local, got %q", cfg.LLMProvider.Generators[0].ID)
	}
	if cfg.LLMProvider.Roles["planner"] != "deepseek" {
		t.Errorf("Roles[planner]: want deepseek, got %q", cfg.LLMProvider.Roles["planner"])
	}
}

// TestLoadConfig_ProvidersFileOverridesConfigJSON verifies the documented
// precedence: when both config.json and providers.json define an
// `llm_provider` block, providers.json (layer 8) wins over config.json (layer 4).
func TestLoadConfig_ProvidersFileOverridesConfigJSON(t *testing.T) {
	base := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"},"llm_provider":{"default":"from-config","generators":[{"id":"from-config","provider":"ollama","model":"x","endpoint":"http://x"}]}}`
	embedder := `{"embedder":{"provider":"ollama","model":"nomic-embed-text","endpoint":"http://localhost:11434","dimensions":768}}`
	providers := `{"llm_provider":{
		"default": "from-file",
		"generators": [
			{"id":"from-file","provider":"ollama","model":"y","endpoint":"http://y"}
		]
	}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(base), 0644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "embedder.json"), []byte(embedder), 0644); err != nil {
		t.Fatalf("write embedder: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "providers.json"), []byte(providers), 0644); err != nil {
		t.Fatalf("write providers: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.LLMProvider.Default != "from-file" {
		t.Errorf("LLMProvider.Default: want from-file (providers.json wins), got %q", cfg.LLMProvider.Default)
	}
	if len(cfg.LLMProvider.Generators) != 1 || cfg.LLMProvider.Generators[0].ID != "from-file" {
		t.Errorf("Generators[0].ID: want from-file, got %+v", cfg.LLMProvider.Generators)
	}
}
