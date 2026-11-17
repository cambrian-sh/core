package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// agentOutToolName is the ledger tool name of a file collected from an
// instance's scratch out/ folder.
const agentOutToolName = "agent_out"

var _ domain.AgentOutputCollector = (*Server)(nil)

// CollectAgentOutputs records the regular files under an exited instance's
// out/ folder as kind=artifact run-ledger entries (ADR-0157 D5.1), before the
// sandbox removes the scratch. Each entry is:
//   - written by the agent principal and attributed to the instance — both kernel
//     facts (the kernel launched it), never anything the agent wrote;
//   - placed in the run, session, step and conversation of the last lease the
//     instance presented (instanceWork), so it inherits that step's (or turn's)
//     tool results as DerivedFrom and is never easier to read than they are;
//   - classified at write by the ledger writer (ClassifyArtifactWrite, the
//     conversation owner stamped) and capped: one file by
//     execution.ledger.artifacts.max_file_bytes, one collection by
//     max_collect_bytes and max_collect_files.
//
// Symlinks and anything not a regular file are skipped (a link could point
// outside the scratch). Files past a cap are left to be deleted with the scratch
// and named in the returned error; the others are still recorded.
func (s *Server) CollectAgentOutputs(ctx context.Context, out domain.AgentOutputs) (int, error) {
	defer s.instanceWork().forget(out.InstanceID)
	if !s.ledgerOn() || out.Dir == "" || out.AgentID == "" {
		return 0, nil
	}
	files, err := listOutFiles(out.Dir)
	if err != nil || len(files) == 0 {
		return 0, err
	}
	caps := s.ExecCfg.Ledger.Artifacts
	prov := domain.LedgerProvenance{StepIndex: -1}
	if b, ok := s.instanceWork().lastFor(out.InstanceID); ok {
		prov = s.provenanceOfBinding(ctx, b)
	}
	writer := domain.AgentPrincipal(out.AgentID)
	ctx = domain.WithPrincipal(ctx, writer)
	if prov.Owner != "" {
		ctx = domain.WithConversationOwner(ctx, prov.Owner)
	}
	var (
		recorded int
		total    int64
		problems []string
	)
	for _, f := range files {
		switch {
		case recorded >= caps.CollectFilesCap():
			problems = append(problems, fmt.Sprintf("%s: over max_collect_files (%d)", f.rel, caps.CollectFilesCap()))
			continue
		case f.size > caps.FileCap():
			problems = append(problems, fmt.Sprintf("%s: %d bytes over max_file_bytes (%d)", f.rel, f.size, caps.FileCap()))
			continue
		case total+f.size > caps.CollectBytesCap():
			problems = append(problems, fmt.Sprintf("%s: over max_collect_bytes (%d)", f.rel, caps.CollectBytesCap()))
			continue
		}
		body, rerr := os.ReadFile(f.abs)
		if rerr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f.rel, rerr))
			continue
		}
		args, _ := json.Marshal(map[string]string{"path": "out/" + f.rel})
		if _, werr := s.Ledger.Writer.Record(ctx, domain.LedgerRecord{
			Kind: domain.LedgerArtifact, Provenance: prov, ToolName: agentOutToolName, ArgsJSON: args,
			Writer: writer, InstanceID: out.InstanceID, ContentType: outContentType(f.rel), Body: body,
		}); werr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f.rel, werr))
			continue
		}
		recorded++
		total += int64(len(body))
	}
	if len(problems) > 0 {
		return recorded, errors.New("not collected: " + strings.Join(problems, "; "))
	}
	return recorded, nil
}

type outFile struct {
	rel, abs string
	size     int64
}

// listOutFiles lists the regular files under dir in path order; a missing dir
// is an empty list.
func listOutFiles(dir string) ([]outFile, error) {
	var files []outFile
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil // directories are walked; links, devices and pipes are not artifacts
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return nil
		}
		files = append(files, outFile{rel: filepath.ToSlash(rel), abs: path, size: info.Size()})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, err
}

// outContentType infers a MIME type from the file extension; unknown ⇒ octet-stream.
func outContentType(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}
