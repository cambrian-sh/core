package config

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// FileLayer is one JSON file layer of the config pipeline: its provenance label
// and the path it is read from.
type FileLayer struct {
	Source string
	Path   string
}

// FileLayers returns the file layers LoadConfigWithStore merges, LOWEST priority
// first, for the primary config path (configs/config.json in production).
//
// This list is the single statement of file precedence. LoadConfigWithStore
// merges these layers in this order, and the ADR-0101 D8 write-time resolver
// (ResolveFileSetting) walks the same list backwards — so "which file supplies
// this key" cannot be answered by a second, drifting copy of the precedence (the
// argument D4 makes against reconstructing provenance separately).
func FileLayers(path string) []FileLayer {
	dir := filepath.Dir(path)
	return []FileLayer{
		{SourceTuning, filepath.Join(dir, "tuning.json")},
		{SourceTuningLocal, filepath.Join(dir, "tuning.local.json")},
		{SourceConfig, path},
		{SourceConfigLocal, strings.TrimSuffix(path, ".json") + ".local.json"},
		{SourceEmbedder, filepath.Join(dir, "embedder.json")},
		{SourceEmbedderLocal, filepath.Join(dir, "embedder.local.json")},
		{SourceProviders, filepath.Join(dir, "providers.json")},
		{SourceProvidersLocal, filepath.Join(dir, "providers.local.json")},
		{SourceMCP, filepath.Join(dir, "mcp.json")},
	}
}

// committedSources are the file layers that are tracked by git and must never be
// rewritten by the kernel (ADR-0101 D8). `git ls-files configs/` lists exactly one
// layer file: tuning.json, the curated power-user starter. Every other layer is
// gitignored per machine (`.gitignore`, enforced by TestCommittedSourcesMatchGitignore).
//
// A static set rather than a `git ls-files` call at runtime: a container or an
// installed binary has no .git to ask, and a write path that behaves differently
// depending on whether a repository happens to surround the binary is not one an
// operator can reason about.
var committedSources = map[string]bool{
	SourceTuning: true,
}

// FileOrigin answers "which file layer currently supplies this key" for the D8
// write path and the boot override report.
type FileOrigin struct {
	// Source is the layer label ("providers.json"), Path the file on disk.
	Source string
	Path   string
	// Value is what that file states for the key, as the merge sees it (after the
	// v1→v2 migration). For a generator lookup it is that generator's object.
	Value any
	// Writable reports whether D8 may write the value into this file. Reason says
	// why not when it is false ("committed to git", "read-only").
	Writable bool
	Reason   string
}

// ResolveFileSetting returns the HIGHEST file layer that states key, reading the
// files as they are NOW. ok is false when no file layer sets it.
//
// Boot provenance cannot answer this: once the store holds the key, provenance
// attributes it to "store" and the file underneath is invisible — which is the
// silent shadowing D8 exists to end. Reading at call time also means an operator's
// edit to providers.json since boot is honoured rather than overwritten blind.
//
// A legacy flat execution key ("execution.ewma_alpha") is looked up under its v2
// path, because each layer is migrated before it is inspected, exactly as the
// merge migrates it.
func ResolveFileSetting(configPath, key string) (FileOrigin, bool) {
	canon := CanonicalKey(key)
	layers := FileLayers(configPath)
	for i := len(layers) - 1; i >= 0; i-- {
		l := layers[i]
		sk := scratchFile(l.Path)
		if sk == nil || !sk.Exists(canon) {
			continue
		}
		o := FileOrigin{Source: l.Source, Path: l.Path, Value: sk.Get(canon)}
		o.Writable, o.Reason = fileWritable(l)
		return o, true
	}
	return FileOrigin{}, false
}

// GeneratorsKey is the config path of the generator list in the file layers.
const GeneratorsKey = "llm_provider.generators"

// FileGenerators returns the generator list the file layers supply, and the
// origin of that list. Koanf replaces a list WHOLESALE, so the list in effect is
// the one in the highest file layer that states `llm_provider.generators` at all
// — a lower file's list is invisible even for ids the higher one omits.
func FileGenerators(configPath string) ([]GeneratorConfig, FileOrigin, bool) {
	o, ok := ResolveFileSetting(configPath, GeneratorsKey)
	if !ok {
		return nil, FileOrigin{}, false
	}
	var list []GeneratorConfig
	if b, err := stdjson.Marshal(o.Value); err == nil {
		_ = stdjson.Unmarshal(b, &list)
	}
	return list, o, true
}

// ResolveGeneratorFile returns the file layer that supplies generator id: the
// layer whose (winning) generator list contains it. Value is that file's entry.
func ResolveGeneratorFile(configPath, id string) (FileOrigin, bool) {
	list, o, ok := FileGenerators(configPath)
	if !ok {
		return FileOrigin{}, false
	}
	for _, g := range list {
		if g.ID == id {
			o.Value = g
			return o, true
		}
	}
	return FileOrigin{}, false
}

// fileWritable decides whether D8 may write into a layer's file.
func fileWritable(l FileLayer) (bool, string) {
	if committedSources[l.Source] {
		return false, l.Source + " is committed to git and is never rewritten by the kernel"
	}
	fi, err := os.Stat(l.Path)
	if err != nil {
		return false, err.Error()
	}
	if fi.Mode().Perm()&0o200 == 0 {
		return false, l.Source + " is read-only"
	}
	return true, ""
}

// IsSecretKey reports whether a config key's VALUE must be treated as a secret:
// never written into a file by D8 (D5), and never printed by the boot override
// report or the CLI. Variable NAMES (`api_key_env`, `token_env`) are not secrets.
func IsSecretKey(key string) bool {
	leaf := strings.ToLower(key)
	if i := strings.LastIndex(leaf, "."); i >= 0 {
		leaf = leaf[i+1:]
	}
	if strings.HasSuffix(leaf, "_env") {
		return false
	}
	for _, s := range []string{"password", "secret", "token", "api_key", "apikey", "credential"} {
		if strings.Contains(leaf, s) {
			return true
		}
	}
	return false
}

// WriteFileSetting writes value for key into the file o names, in place
// (ADR-0101 D8). Every other byte of the file — key order, comments-as-keys,
// indentation, line endings — is left as it was; only the value at the key's
// path is replaced. The file is replaced atomically (temp file + rename), so a
// crash mid-write leaves the old file, never a truncated one.
//
// The key must already be present in the file (that is how o was resolved). For
// a legacy flat execution key, the spelling the file actually uses — nested v2 or
// flat v1 — is the one rewritten, so a v1 file is not silently given a second,
// conflicting copy of the key.
func WriteFileSetting(o FileOrigin, key string, value any) error {
	if IsSecretKey(key) {
		return fmt.Errorf("refusing to write secret-shaped key %s into %s (ADR-0101 D5)", key, o.Source)
	}
	return patchFile(o.Path, func(doc []byte) ([]byte, error) {
		path, ok := locateKey(doc, key)
		if !ok {
			return nil, fmt.Errorf("%s no longer sets %s", o.Source, key)
		}
		return setRaw(doc, path, value)
	})
}

// WriteFileGenerator replaces (remove=false) or deletes (remove=true) the
// generator with this id inside the file's generator list, leaving the other
// generators byte-for-byte as they were (ADR-0101 D8 + D9).
func WriteFileGenerator(o FileOrigin, id string, entry any, remove bool) error {
	return patchFile(o.Path, func(doc []byte) ([]byte, error) {
		listPath, ok := locateKey(doc, GeneratorsKey)
		if !ok {
			return nil, fmt.Errorf("%s no longer sets %s", o.Source, GeneratorsKey)
		}
		idx := -1
		for i, g := range gjson.GetBytes(doc, listPath).Array() {
			if g.Get("id").String() == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, fmt.Errorf("%s no longer declares generator %q", o.Source, id)
		}
		elem := fmt.Sprintf("%s.%d", listPath, idx)
		if remove {
			return sjson.DeleteBytes(doc, elem)
		}
		return setRaw(doc, elem, entry)
	})
}

// patchFile reads path, applies edit to the document (BOM stripped), checks the
// result is still valid JSON, and atomically replaces the file.
func patchFile(path string, edit func(doc []byte) ([]byte, error)) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	bom := []byte{0xEF, 0xBB, 0xBF}
	hasBOM := bytes.HasPrefix(raw, bom)
	doc := bytes.TrimPrefix(raw, bom)
	if !gjson.ValidBytes(doc) {
		return fmt.Errorf("%s is not valid JSON; not rewriting it", path)
	}
	out, err := edit(doc)
	if err != nil {
		return err
	}
	if !gjson.ValidBytes(out) {
		return fmt.Errorf("patched %s would not be valid JSON; not rewriting it", path)
	}
	if hasBOM {
		out = append(append([]byte{}, bom...), out...)
	}
	return writeFileAtomic(path, out)
}

// writeFileAtomic replaces path with data via a temp file in the same directory
// and a rename, keeping the original file mode. The same directory matters: a
// rename across filesystems is a copy, which is not atomic.
func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	_ = os.Chmod(tmpName, mode) // best-effort: Windows ignores most mode bits
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// locateKey finds the spelling of key the RAW (unmigrated) document uses and
// returns it as a gjson/sjson path. Candidates: the v2 nested path, the key as
// given, and the v1 flat path for a nested execution key.
func locateKey(doc []byte, key string) (string, bool) {
	cands := []string{CanonicalKey(key), key}
	if rest, ok := strings.CutPrefix(key, "execution."); ok {
		if block, name, found := strings.Cut(rest, "."); found && legacyExecutionKey[name] == block {
			cands = append(cands, "execution."+name)
		}
	}
	for _, c := range cands {
		p := jsonPath(c)
		if gjson.GetBytes(doc, p).Exists() {
			return p, true
		}
	}
	return "", false
}

// jsonPath converts a dotted config key into a gjson/sjson path, escaping the
// characters those libraries treat as syntax inside a segment.
func jsonPath(key string) string {
	parts := strings.Split(key, ".")
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `|`, `\|`, `#`, `\#`, `@`, `\@`)
	for i, p := range parts {
		parts[i] = r.Replace(p)
	}
	return strings.Join(parts, ".")
}

// setRaw replaces the value at path with value, rendered to sit naturally in the
// surrounding document: an object or list is indented to the depth of the line it
// starts on, with the file's own indent unit and line endings.
func setRaw(doc []byte, path string, value any) ([]byte, error) {
	prefix, unit := indentAt(doc, gjson.GetBytes(doc, path).Index)
	var buf bytes.Buffer
	enc := stdjson.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent(prefix, unit)
	if err := enc.Encode(value); err != nil {
		return nil, fmt.Errorf("encode value: %w", err)
	}
	rendered := bytes.TrimRight(buf.Bytes(), "\n")
	if bytes.Contains(doc, []byte("\r\n")) {
		rendered = bytes.ReplaceAll(rendered, []byte("\n"), []byte("\r\n"))
	}
	return sjson.SetRawBytes(doc, path, rendered)
}

// indentAt returns the leading whitespace of the line containing offset, and the
// document's indent unit (the leading whitespace of its first indented line,
// defaulting to two spaces).
func indentAt(doc []byte, offset int) (prefix, unit string) {
	unit = "  "
	for _, line := range bytes.Split(doc, []byte("\n")) {
		trimmed := bytes.TrimLeft(line, " \t")
		if len(trimmed) > 0 && len(trimmed) < len(line) {
			unit = string(line[:len(line)-len(trimmed)])
			break
		}
	}
	if offset <= 0 || offset > len(doc) {
		return "", unit
	}
	start := bytes.LastIndexByte(doc[:offset], '\n') + 1
	end := start
	for end < len(doc) && (doc[end] == ' ' || doc[end] == '\t') {
		end++
	}
	return string(doc[start:end]), unit
}

// SameValue reports whether two config values are equal once normalised through
// JSON (so 1 and 1.0, or a typed struct and its decoded map, compare equal).
func SameValue(a, b any) bool {
	na, errA := normalise(a)
	nb, errB := normalise(b)
	if errA != nil || errB != nil {
		return false
	}
	return reflect.DeepEqual(na, nb)
}

func normalise(v any) (any, error) {
	b, err := stdjson.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = stdjson.Unmarshal(b, &out)
	return out, err
}
