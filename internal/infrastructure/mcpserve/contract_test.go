package mcpserve

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The tool contract golden (ADR-0126 Consequences: "a new public contract").
//
// tools/list is consumed by clients Cambrian does not ship. The moment this
// file exists, ANY drift in a name, description, annotation or input schema is
// a public-contract event: additive changes regenerate the golden in the same
// commit slice and get called out in review; a mutation or removal needs the
// tool-contract stability statement's process, not a casual edit.
//
// Regenerate with:  go test ./internal/infrastructure/mcpserve/ -run TestToolContract -update
var updateGolden = flag.Bool("update", false, "rewrite testdata/tools_list.golden.json from the live surface")

const goldenPath = "testdata/tools_list.golden.json"

// contractTool is the frozen view of one tool: exactly what an external client
// binds to, nothing internal.
type contractTool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	ReadOnly    bool            `json:"read_only_hint"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func TestToolContract_GoldenToolsList(t *testing.T) {
	// The declarations are the contract; the handlers never run here, so an
	// unbound backend set is exactly right.
	session := serve(t, Options{
		Surface:      CoreTools(NewCoreBackends()),
		Instructions: CoreInstructions,
	}, "token-aaaa")

	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	tools := make([]contractTool, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", tool.Name, err)
		}
		var readOnly bool
		if tool.Annotations != nil {
			readOnly = tool.Annotations.ReadOnlyHint
		}
		tools = append(tools, contractTool{
			Name:        tool.Name,
			Title:       tool.Title,
			Description: tool.Description,
			ReadOnly:    readOnly,
			InputSchema: schema,
		})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	got, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		t.Fatalf("marshal contract: %v", err)
	}
	got = append(got, '\n')

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden rewritten: %s — this is a public-contract event, say so in the report", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update once to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("tools/list drifted from the golden contract.\n"+
			"If this change is INTENDED, it is a public-contract event: regenerate with -update "+
			"and call it out in review.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// The phase-1 contract is READ-ONLY (owner ruling, 2026-08-14): exactly four
// tools, every one EffectRead + readOnlyHint, and none of them is `remember`.
func TestToolContract_Phase1IsReadOnly(t *testing.T) {
	surface := CoreTools(NewCoreBackends())
	if len(surface) != 4 {
		t.Fatalf("core tools = %d, want 4", len(surface))
	}
	for _, entry := range surface {
		if entry.Tool.Name == "remember" {
			t.Fatal("`remember` is deferred and must not be published in phase 1")
		}
		if !entry.Tool.ReadOnly {
			t.Errorf("%s: not marked read-only", entry.Tool.Name)
		}
		for _, e := range entry.Tool.Effects {
			if e != "read" {
				t.Errorf("%s declares effect %q; phase 1 is read-only", entry.Tool.Name, e)
			}
		}
	}
}

// D12: the instructions reach the client at initialize. They are what stands
// between a listed tool and a called one.
func TestServerInstructions_ReachTheClient(t *testing.T) {
	session := serve(t, Options{
		Surface:      CoreTools(NewCoreBackends()),
		Instructions: CoreInstructions,
	}, "token-aaaa")
	init := session.InitializeResult()
	if init == nil || !strings.Contains(init.Instructions, "search_memory") {
		t.Fatalf("initialize carried no usable instructions: %+v", init)
	}
}

// D8, listing side: a caller whose effects are denied is not SHOWN the tools —
// the menu is filtered by the same decision point that would refuse the call.
func TestToolsList_IsFilteredPerCallerEffects(t *testing.T) {
	session := serve(t, Options{
		Surface:    CoreTools(NewCoreBackends()),
		Authorizer: denyAll{},
	}, "token-aaaa")
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(listed.Tools) != 0 {
		t.Fatalf("a deny-all principal was shown %d tools; the menu must be filtered", len(listed.Tools))
	}
}
