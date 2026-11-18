package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentPythonDefaults(t *testing.T) {
	pc := DefaultConfig().Execution.Agents.Python
	if !pc.BuildOnBoot || pc.Offline || pc.Root != "" || len(pc.Wheelhouse) != 0 {
		t.Fatalf("defaults: %+v (build_on_boot true per owner A1-Q12)", pc)
	}
	if got := pc.ResolvedRoot(filepath.FromSlash("/k")); got != filepath.Join(filepath.FromSlash("/k"), "runtime", "python") {
		t.Fatalf("default root %s", got)
	}
}

func TestAgentPythonValidate(t *testing.T) {
	abs, _ := filepath.Abs(t.TempDir())
	for name, c := range map[string]struct {
		cfg  AgentPythonConfig
		want string
	}{
		"http mirror":       {AgentPythonConfig{PythonMirrorURL: "http://mirror/"}, "python_mirror_url"},
		"bare host mirror":  {AgentPythonConfig{InstallerMirrorURL: "mirror.local/uv"}, "installer_mirror_url"},
		"ftp index":         {AgentPythonConfig{IndexURL: "ftp://x/simple"}, "index_url"},
		"offline no wheels": {AgentPythonConfig{Offline: true}, "requires execution.agents.python.wheelhouse"},
		"escaping relative": {AgentPythonConfig{Wheelhouse: []string{"../wheels"}}, "leaves the config directory"},
		"url wheelhouse":    {AgentPythonConfig{Wheelhouse: []string{"https://x/wheels"}}, "file:// URLs"},
		"empty wheelhouse":  {AgentPythonConfig{Wheelhouse: []string{" "}}, "empty path"},
	} {
		errs := c.cfg.validate()
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, ";"), c.want) {
			t.Errorf("%s: want an error containing %q, got %v", name, c.want, errs)
		}
	}
	for name, c := range map[string]AgentPythonConfig{
		"defaults":         {BuildOnBoot: true},
		"https mirrors":    {PythonMirrorURL: "https://nexus.local/py/", InstallerMirrorURL: "https://nexus.local/uv/", IndexURL: "https://nexus.local/simple"},
		"file mirror":      {PythonMirrorURL: "file:///srv/bundle/"},
		"offline absolute": {Offline: true, Wheelhouse: []string{abs}},
		"offline relative": {Offline: true, Wheelhouse: []string{"wheels"}},
		"offline file url": {Offline: true, Wheelhouse: []string{"file:///srv/wheels"}},
	} {
		if errs := c.validate(); len(errs) != 0 {
			t.Errorf("%s: unexpected errors %v", name, errs)
		}
	}
}

func TestAgentPythonBlockReachesFullValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Execution.Agents.Python.Offline = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "execution.agents.python.offline") {
		t.Fatalf("full config validation must include the block, got %v", err)
	}
}

func TestReadAgentPythonBlock(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	got, err := ReadAgentPythonBlock(p)
	if err != nil || !got.BuildOnBoot {
		t.Fatalf("missing file must yield defaults: %+v %v", got, err)
	}
	body := "\xEF\xBB\xBF" + `{"database":{"host":"x"},"execution":{"agents":{"python":{"offline":true,"wheelhouse":["wheels"],"build_on_boot":false}}}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = ReadAgentPythonBlock(p)
	if err != nil || !got.Offline || got.BuildOnBoot || got.ResolvedWheelhouse(dir)[0] != filepath.Join(dir, "wheels") {
		t.Fatalf("got %+v %v", got, err)
	}
	if err := os.WriteFile(p, []byte(`{"execution":{"agents":{"python":{"offline":true}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAgentPythonBlock(p); err == nil {
		t.Fatal("an invalid block must be refused")
	}
}
