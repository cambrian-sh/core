// Command reference-fs-mcp is a reference READ-ONLY filesystem MCP server for Cambrian's
// Scout pre-plan discovery (ADR-0051 issue-007). It exposes two read-only tools —
// list_directory and read_file — root-jailed to a single directory, over stdio. It has NO
// write/mutating operations, so the operator can safely tag it `discovery-safe` (D6).
//
// Configure Cambrian's MCP connector to run it as a stdio command:
//
//	{ "id": "fs", "endpoint": "reference-fs-mcp", "args": ["--root", "/path/to/workspace"] }
//
// The Scout then discovers list_directory/read_file via find_tools (no hardcoded names) and
// the helicopter-class grounding works out of the box, without Cambrian shipping a System
// tool (System tools stay deferred; discovery is MCP-only — ADR-0051 D4).
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxReadBytes = 1 << 20 // 1 MiB read cap — discovery wants the shape, not huge blobs

type listIn struct {
	Path string `json:"path"`
}
type dirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}
type listOut struct {
	Path    string     `json:"path"`
	Entries []dirEntry `json:"entries"`
}

type readIn struct {
	Path string `json:"path"`
}
type readOut struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

// resolveJailed resolves p against root and rejects any path that escapes it (traversal
// containment). p is treated as relative to root; an absolute p is re-based onto root.
func resolveJailed(root, p string) (string, error) {
	p = strings.TrimPrefix(filepath.Clean("/"+p), "/") // strip leading / so Join can't escape via absolute
	full := filepath.Clean(filepath.Join(root, p))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the read-only root")
	}
	return full, nil
}

// doList lists a directory's entries (read-only), root-jailed and sorted for determinism.
func doList(root, p string) (listOut, error) {
	full, err := resolveJailed(root, p)
	if err != nil {
		return listOut{}, err
	}
	des, err := os.ReadDir(full)
	if err != nil {
		return listOut{}, err
	}
	out := listOut{Path: p}
	for _, de := range des {
		var size int64
		if info, e := de.Info(); e == nil {
			size = info.Size()
		}
		out.Entries = append(out.Entries, dirEntry{Name: de.Name(), IsDir: de.IsDir(), Size: size})
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	return out, nil
}

// doRead reads a file's contents (read-only), root-jailed and size-capped.
func doRead(root, p string) (readOut, error) {
	full, err := resolveJailed(root, p)
	if err != nil {
		return readOut{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return readOut{}, err
	}
	if info.IsDir() {
		return readOut{}, errors.New("path is a directory; use list_directory")
	}
	f, err := os.Open(full)
	if err != nil {
		return readOut{}, err
	}
	defer f.Close()
	buf := make([]byte, maxReadBytes)
	n, _ := f.Read(buf)
	return readOut{Path: p, Content: string(buf[:n]), Bytes: n, Truncated: info.Size() > int64(n)}, nil
}

func main() {
	root := flag.String("root", ".", "the read-only filesystem root the server is jailed to")
	flag.Parse()
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		log.Fatalf("reference-fs-mcp: bad root: %v", err)
	}

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "reference-fs", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "list_directory", Description: "List the entries of a directory under the read-only root. Args: {\"path\": \"relative/dir\"}."},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in listIn) (*mcpsdk.CallToolResult, listOut, error) {
			out, err := doList(absRoot, in.Path)
			return nil, out, err
		})
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "read_file", Description: "Read the contents of a text file under the read-only root. Args: {\"path\": \"relative/file\"}."},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in readIn) (*mcpsdk.CallToolResult, readOut, error) {
			out, err := doRead(absRoot, in.Path)
			return nil, out, err
		})

	if err := server.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		log.Fatalf("reference-fs-mcp: %v", err)
	}
}
