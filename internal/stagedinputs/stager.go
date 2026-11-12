// Package stagedinputs is the kernel side of the code sandbox's per-run input
// folder (owner decision 2026-11-10, option C;
// cambrian-knowledge/decisions/code-sandbox-files-per-run-mount.md; the port is
// domain.ToolInputStager, the executor half domain/staged_inputs.go).
//
// For one call of a tool that declares staged_inputs, the Stager resolves every
// requested input under the CALLER'S access and writes exactly those files into a
// fresh folder:
//
//   - a ledger id (led_…) is read through the enforced ledger reader with the
//     caller's predicate on ctx (authz.EnforcingLedger: forbidden = absent), so an
//     entry the caller could not read refuses the call;
//   - any other input is a path, read by the configured file-reading tool through
//     the ToolExecutor as the same caller (grant, resource policy, scope, the
//     person's own per-user file server, a ledger record of the read).
//
// Any input that does not resolve refuses the WHOLE call and leaves nothing on
// disk. The folder (inputs/ and an empty outputs/) reaches the tool server in the
// request's _meta; the server mounts inputs/ read-only and outputs/ writable for
// that one execution. After the call the executor collects outputs/ (Outputs) and
// deletes the folder (Cleanup). Nothing persists between calls or people.
package stagedinputs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// Defaults for every zero Config field.
const (
	DefaultArgument       = domain.DefaultStagedInputsArgument
	DefaultMaxFiles       = 20
	DefaultMaxTotalBytes  = 64 << 20
	DefaultMaxOutputFiles = 20
	DefaultMaxOutputBytes = 32 << 20
	DefaultRootName       = "cambrian-staged"
	DefaultTextReader     = "mcp:filesystem/read_text_file"
	DefaultBinaryReader   = "mcp:filesystem/read_media_file"
	DefaultReaderArgument = "path"
)

// DefaultBinaryExtensions are read with the binary reader (base64 content).
var DefaultBinaryExtensions = []string{
	".xlsx", ".xls", ".xlsm", ".ods", ".parquet", ".feather", ".zip", ".gz",
	".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".docx", ".pptx", ".sqlite", ".db",
}

// Reader names a file-reading tool and its path argument.
type Reader struct {
	Tool     string
	Argument string
}

// Config is execution.tools.staged_inputs resolved (zero fields ⇒ defaults).
type Config struct {
	Root             string
	Argument         string
	MaxFiles         int
	MaxTotalBytes    int64
	MaxOutputFiles   int
	MaxOutputBytes   int64
	TextReader       Reader
	BinaryReader     Reader
	BinaryExtensions []string
}

func (c Config) withDefaults() Config {
	if c.Root == "" {
		c.Root = filepath.Join(os.TempDir(), DefaultRootName)
	}
	if c.Argument == "" {
		c.Argument = DefaultArgument
	}
	if c.MaxFiles <= 0 {
		c.MaxFiles = DefaultMaxFiles
	}
	if c.MaxTotalBytes <= 0 {
		c.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if c.MaxOutputFiles <= 0 {
		c.MaxOutputFiles = DefaultMaxOutputFiles
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if c.TextReader.Tool == "" {
		c.TextReader.Tool = DefaultTextReader
	}
	if c.TextReader.Argument == "" {
		c.TextReader.Argument = DefaultReaderArgument
	}
	if c.BinaryReader.Tool == "" {
		c.BinaryReader.Tool = DefaultBinaryReader
	}
	if c.BinaryReader.Argument == "" {
		c.BinaryReader.Argument = DefaultReaderArgument
	}
	if c.BinaryExtensions == nil {
		c.BinaryExtensions = DefaultBinaryExtensions
	}
	return c
}

// Stager implements domain.ToolInputStager.
type Stager struct {
	cfg Config
	// ledger is the ENFORCED reader (authz.EnforcingLedger); nil ⇒ the run ledger
	// is off and ledger-id inputs are refused.
	ledger domain.LedgerReader
}

var _ domain.ToolInputStager = (*Stager)(nil)

// New builds a stager. ledger must be the enforced reader (never the raw store).
func New(cfg Config, ledger domain.LedgerReader) *Stager {
	return &Stager{cfg: cfg.withDefaults(), ledger: ledger}
}

// Argument implements domain.ToolInputStager.
func (s *Stager) Argument() string { return s.cfg.Argument }

// Root is the directory per-run folders are created in.
func (s *Stager) Root() string { return s.cfg.Root }

// resolved is one input ready to write.
type resolved struct {
	ref    string
	name   string
	data   []byte
	source string // the ledger id it came from, "" when unknown
}

// Stage implements domain.ToolInputStager.
func (s *Stager) Stage(ctx context.Context, in domain.StageRequest) (domain.StagedCall, error) {
	if len(in.Inputs) > s.cfg.MaxFiles {
		return nil, fmt.Errorf("%d inputs requested; at most %d per call", len(in.Inputs), s.cfg.MaxFiles)
	}
	var (
		items []resolved
		total int64
	)
	seen := map[string]bool{}
	for _, ref := range in.Inputs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		var (
			r   resolved
			err error
		)
		if domain.IsLedgerID(ref) {
			r, err = s.fromLedger(ctx, ref)
		} else {
			r, err = s.fromPath(ctx, in, ref)
		}
		if err != nil {
			return nil, err
		}
		total += int64(len(r.data))
		if total > s.cfg.MaxTotalBytes {
			return nil, fmt.Errorf("inputs exceed %d bytes", s.cfg.MaxTotalBytes)
		}
		items = append(items, r)
	}
	return s.write(items)
}

// fromLedger reads a ledger entry under the caller's predicate (on ctx).
func (s *Stager) fromLedger(ctx context.Context, id string) (resolved, error) {
	if s.ledger == nil {
		return resolved{}, fmt.Errorf("%s: ledger ids cannot be staged, the run ledger is off", id)
	}
	ent, err := s.ledger.Get(ctx, id)
	if err != nil {
		return resolved{}, fmt.Errorf("%s: %w", id, err)
	}
	if ent == nil {
		// Absent and forbidden are the same answer by design (EnforcingLedger).
		return resolved{}, fmt.Errorf("%s: no such ledger entry readable by this caller", id)
	}
	if ent.Truncated {
		return resolved{}, fmt.Errorf("%s: the ledger kept only %d of its %d bytes; stage the file by its path instead", id, len(ent.Body), ent.SizeBytes)
	}
	data, ext := ent.Body, ""
	if ent.Kind != domain.LedgerArtifact {
		data, ext = decodeContent(ent.Body)
	}
	name := nameFromArgs(ent.Args)
	if name == "" {
		if ext == "" {
			ext = extFor(ent.ContentType, data)
		}
		name = ent.ID + ext
	}
	return resolved{ref: id, name: name, data: data, source: ent.ID}, nil
}

// fromPath reads a path through the configured reader tool, as the caller.
func (s *Stager) fromPath(ctx context.Context, in domain.StageRequest, p string) (resolved, error) {
	if in.Read == nil {
		return resolved{}, fmt.Errorf("%s: paths cannot be staged here", p)
	}
	reader, binary := s.cfg.TextReader, false
	ext := strings.ToLower(path.Ext(filepath.ToSlash(p)))
	for _, b := range s.cfg.BinaryExtensions {
		if strings.EqualFold(ext, b) {
			reader, binary = s.cfg.BinaryReader, true
			break
		}
	}
	args, _ := json.Marshal(map[string]string{reader.Argument: p})
	resp := in.Read(ctx, domain.ToolCallRequest{
		AgentID: in.Call.AgentID, ToolName: reader.Tool, ArgsJSON: args,
		SessionTokenID: in.Call.SessionTokenID, TaskID: in.Call.TaskID, System: in.Call.System,
	})
	switch {
	case resp.Denied:
		return resolved{}, fmt.Errorf("%s: reading it through %s was refused: %s", p, reader.Tool, resp.DenyReason)
	case resp.Error != "":
		return resolved{}, fmt.Errorf("%s: reading it through %s failed: %s", p, reader.Tool, resp.Error)
	}
	body := resp.ResultJSON
	if body == nil && domain.IsLedgerID(resp.ResultCID) && s.ledger != nil {
		ent, err := s.ledger.Get(ctx, resp.ResultCID)
		if err != nil || ent == nil || ent.Truncated {
			return resolved{}, fmt.Errorf("%s: the read result could not be fetched back from the ledger", p)
		}
		body = ent.Body
	}
	var data []byte
	if binary {
		d, ok := decodeBinary(body)
		if !ok {
			return resolved{}, fmt.Errorf("%s: %s returned no binary content", p, reader.Tool)
		}
		data = d
	} else {
		data, _ = decodeContent(body)
	}
	src := ""
	if domain.IsLedgerID(resp.ResultCID) {
		src = resp.ResultCID
	}
	return resolved{ref: p, name: safeName(path.Base(filepath.ToSlash(p))), data: data, source: src}, nil
}

// write creates the per-run folder with exactly the resolved inputs.
func (s *Stager) write(items []resolved) (domain.StagedCall, error) {
	if err := os.MkdirAll(s.cfg.Root, 0o700); err != nil {
		return nil, fmt.Errorf("staging root: %w", err)
	}
	dir, err := os.MkdirTemp(s.cfg.Root, "run-")
	if err != nil {
		return nil, fmt.Errorf("staging folder: %w", err)
	}
	c := &call{dir: dir, inputs: filepath.Join(dir, "inputs"), outputs: filepath.Join(dir, "outputs"),
		files: map[string]string{}, maxOut: s.cfg.MaxOutputFiles, maxOutBytes: s.cfg.MaxOutputBytes}
	fail := func(err error) (domain.StagedCall, error) {
		c.Cleanup()
		return nil, err
	}
	for _, d := range []string{c.inputs, c.outputs} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return fail(fmt.Errorf("staging folder: %w", err))
		}
	}
	used := map[string]bool{}
	for _, it := range items {
		name := uniqueName(it.name, used)
		if err := os.WriteFile(filepath.Join(c.inputs, name), it.data, 0o600); err != nil {
			return fail(fmt.Errorf("staging %s: %w", it.ref, err))
		}
		c.files[it.ref] = name
		if it.source != "" {
			c.sources = append(c.sources, it.source)
		}
	}
	return c, nil
}

// call is one staged call's folders.
type call struct {
	dir, inputs, outputs string
	files                map[string]string
	sources              []string
	maxOut               int
	maxOutBytes          int64
	once                 sync.Once
}

func (c *call) Meta() map[string]any {
	files := make(map[string]any, len(c.files))
	for k, v := range c.files {
		files[k] = v
	}
	return map[string]any{
		domain.StagedInputsMetaDir:     filepath.ToSlash(c.inputs),
		domain.StagedInputsMetaOutputs: filepath.ToSlash(c.outputs),
		domain.StagedInputsMetaFiles:   files,
	}
}

func (c *call) Sources() []string { return append([]string(nil), c.sources...) }

// Outputs reads every regular file under outputs/ (symlinks are never followed),
// in name order, capped by count and per-file size.
func (c *call) Outputs() []domain.StagedOutput {
	var out []domain.StagedOutput
	kept := 0
	_ = filepath.WalkDir(c.outputs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best effort: an unreadable entry is skipped
		}
		rel, rerr := filepath.Rel(c.outputs, p)
		if rerr != nil {
			return nil //nolint:nilerr
		}
		name := filepath.ToSlash(rel)
		if !d.Type().IsRegular() {
			out = append(out, domain.StagedOutput{Name: name, Skipped: "not a regular file"})
			return nil
		}
		if kept >= c.maxOut {
			out = append(out, domain.StagedOutput{Name: name, Skipped: fmt.Sprintf("more than %d output files", c.maxOut)})
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			out = append(out, domain.StagedOutput{Name: name, Skipped: ierr.Error()})
			return nil
		}
		if info.Size() > c.maxOutBytes {
			out = append(out, domain.StagedOutput{Name: name, Skipped: fmt.Sprintf("%d bytes, over the %d-byte cap", info.Size(), c.maxOutBytes)})
			return nil
		}
		body, rerr2 := os.ReadFile(p)
		if rerr2 != nil {
			out = append(out, domain.StagedOutput{Name: name, Skipped: rerr2.Error()})
			return nil
		}
		kept++
		out = append(out, domain.StagedOutput{Name: name, ContentType: contentType(name, body), Body: body})
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (c *call) Cleanup() {
	c.once.Do(func() { _ = os.RemoveAll(c.dir) }) // best effort; the root is the OS temp dir
}

// decodeContent turns a tool result body into file bytes: an MCP content array
// (or {"content": …}) gives its text blocks joined, or a single data block
// decoded; anything else is the body itself. ext is ".json" / ".txt" when the
// result was not a content array, "" otherwise.
func decodeContent(body []byte) ([]byte, string) {
	if blocks, ok := contentBlocks(body); ok {
		var text strings.Builder
		var data []byte
		for _, b := range blocks {
			if t, ok := b["text"].(string); ok {
				text.WriteString(t)
				continue
			}
			if d, ok := blockData(b); ok && data == nil {
				data = d
			}
		}
		if text.Len() > 0 {
			return []byte(text.String()), ""
		}
		if data != nil {
			return data, ""
		}
	}
	if json.Valid(body) {
		var str string
		if json.Unmarshal(body, &str) == nil {
			return []byte(str), ".txt"
		}
		return body, ".json"
	}
	return body, ".txt"
}

// decodeBinary returns the first base64 data block of a content result.
func decodeBinary(body []byte) ([]byte, bool) {
	blocks, ok := contentBlocks(body)
	if !ok {
		return nil, false
	}
	for _, b := range blocks {
		if d, ok := blockData(b); ok {
			return d, true
		}
	}
	return nil, false
}

func contentBlocks(body []byte) ([]map[string]any, bool) {
	var blocks []map[string]any
	if json.Unmarshal(body, &blocks) == nil && len(blocks) > 0 {
		return blocks, true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return nil, false
	}
	if raw, ok := obj["content"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []map[string]any{{"type": "text", "text": s}}, true
		}
		if json.Unmarshal(raw, &blocks) == nil && len(blocks) > 0 {
			return blocks, true
		}
	}
	return nil, false
}

// blockData decodes a block's base64 payload (image/audio/blob "data", or an
// embedded resource's "blob").
func blockData(b map[string]any) ([]byte, bool) {
	enc, ok := b["data"].(string)
	if !ok {
		if res, isMap := b["resource"].(map[string]any); isMap {
			enc, ok = res["blob"].(string)
		}
	}
	if !ok {
		return nil, false
	}
	d, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, false
	}
	return d, true
}

// nameFromArgs is the file name a ledger entry's call named (path, file_path or
// filename argument), sanitised; "" when none.
func nameFromArgs(args json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	for _, k := range []string{"path", "file_path", "filename", "name"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			if n := safeName(path.Base(filepath.ToSlash(v))); n != "input" {
				return n
			}
		}
	}
	return ""
}

// safeName keeps a file name inside its folder: no separators, no dot names, no
// control characters.
func safeName(n string) string {
	n = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == '/', r == '\\', r == ':', r == '*', r == '?', r == '"', r == '<', r == '>', r == '|':
			return '_'
		}
		return r
	}, strings.TrimSpace(n))
	n = strings.Trim(n, ". ")
	if n == "" {
		return "input"
	}
	return n
}

func uniqueName(n string, used map[string]bool) string {
	key := strings.ToLower(n)
	if !used[key] {
		used[key] = true
		return n
	}
	ext := path.Ext(n)
	stem := strings.TrimSuffix(n, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if !used[strings.ToLower(cand)] {
			used[strings.ToLower(cand)] = true
			return cand
		}
	}
}

func extFor(contentType string, data []byte) string {
	switch {
	case strings.Contains(contentType, "json"), json.Valid(data):
		return ".json"
	case strings.HasPrefix(contentType, "text/csv"):
		return ".csv"
	}
	return ".txt"
}

func contentType(name string, body []byte) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".json":
		return "application/json"
	case ".md", ".txt", ".log":
		return "text/plain; charset=utf-8"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	if json.Valid(body) {
		return "application/json"
	}
	return "application/octet-stream"
}
