package app

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-1123 (T-20261113-split-app-app-go-4-876-lines-bootstrapkernel-2): the
// characterization of the assembled object graph, pinned BEFORE app.go was split
// into per-seam files so the split could be shown to change nothing.
//
// Per representative config it boots the real kernel (bootstrapKernel over a
// scratch Postgres database, then startKernelServices) and records:
//   - every Kernel field, nil or its dynamic type (one level into the domain
//     stacks, the agent-plane server and its tool executor);
//   - the lifecycle names in registration order — the observable wiring order;
//   - the kernel tools in menu order, the event-bus subscriptions per event type,
//     the approval kinds with a registered handler;
//   - what a plugin is handed at Build (KernelServices, nil or set per field);
//   - the gRPC services mounted once startKernelServices served.
//
// The output is compared with testdata/boot_graph/<config>.golden. Regenerate
// only for an INTENDED wiring change (go test -run TestPg_BootGraph -update-boot-graph)
// and review the diff: a refactor that needs a new golden is not a refactor.
// Every golden change names its cause (testdata/boot_graph/README.md): the change
// that alters the graph updates the goldens itself (ENG-2138 repaired two that did not).

var updateBootGraph = flag.Bool("update-boot-graph", false, "rewrite testdata/boot_graph/*.golden from the current wiring")

// bootProbePlugin captures the KernelServices bundle plugins are handed at Build.
type bootProbePlugin struct{ got *KernelServices }

func (bootProbePlugin) Manifest() PluginManifest {
	return PluginManifest{ID: "bootprobe", DisplayName: "Boot probe", Version: "0"}
}
func (bootProbePlugin) Register(*Registry) error { return nil }
func (p bootProbePlugin) Build(svc KernelServices) error {
	*p.got = svc
	return nil
}

func TestPg_BootGraphCharacterization(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, cfg *config.Config)
	}{
		{"default", func(t *testing.T, cfg *config.Config) {
			cfg.Execution.Retrieval = config.DefaultConfig().Execution.Retrieval
		}},
		{"all_on", func(t *testing.T, cfg *config.Config) {
			// Retrieval lanes are all on already (retrievalScratchConfig).
			e := &cfg.Execution
			e.Erasure.Enabled, e.Erasure.AgentEraseEnabled = true, true
			e.Chat.ChatPoolSize = 1
			e.Ingestion.EvidenceCaptureEnabled = true
			e.Ingestion.KgExtractorEnabled = true
			e.Facts.ExtractEnabled, e.Facts.LaneEnabled = true, true
			e.Ledger.Enabled = true
			e.Skills.Curator.Enabled = true
			e.Tools.ToolsAutoApprove = true
			// Not t.TempDir: Kernel.Shutdown does not close the run-trace file, so on
			// Windows the directory cannot be removed while the test process lives.
			traceDir, err := os.MkdirTemp("", "boot-graph-trace-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(traceDir) }) // best effort, see above
			e.Trace.Enabled, e.Trace.Dir = true, traceDir
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := retrievalScratchConfig(t)
			tc.setup(t, cfg)
			got := bootGraph(t, cfg)
			golden := filepath.Join("testdata", "boot_graph", tc.name+".golden")
			if *updateBootGraph {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read %s (run with -update-boot-graph to create it): %v", golden, err)
			}
			if string(want) != got {
				d := lineDiff(string(want), got)
				if d == "" {
					d = "the same lines in a different ORDER (wiring order changed); got:\n" + got
				}
				t.Errorf("assembled object graph differs from %s:\n%s", golden, d)
			}
		})
	}
}

// bootGraph boots a kernel on cfg, starts its services, and renders the graph.
func bootGraph(t *testing.T, cfg *config.Config) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var svc KernelServices
	k, err := bootstrapKernel(ctx, cfg, lis, Options{Plugins: []Plugin{bootProbePlugin{got: &svc}}}, nil)
	if err != nil {
		_ = lis.Close()
		t.Fatalf("bootstrapKernel: %v", err)
	}
	var b strings.Builder
	section := func(title string) { fmt.Fprintf(&b, "\n## %s\n", title) }

	section("Kernel")
	dumpFields(&b, reflect.ValueOf(k).Elem())
	for _, s := range []struct {
		name string
		v    any
	}{
		{"Memory", k.Memory}, {"Awareness", k.Awareness}, {"Metabolism", k.Metabolism},
		{"Supervision", k.Supervision}, {"Server", k.Server}, {"Server.ToolExecutor", k.Server.ToolExecutor},
	} {
		section(s.name)
		rv := reflect.ValueOf(s.v)
		if rv.IsNil() {
			b.WriteString("nil\n")
			continue
		}
		dumpFields(&b, rv.Elem())
	}
	section("Lifecycles (registration order)")
	for _, lc := range k.Lifecycles {
		fmt.Fprintf(&b, "%s start=%t stop=%t\n", lc.Name, lc.Start != nil, lc.Stop != nil)
	}
	section("Kernel tools (menu order)")
	for _, kt := range k.Server.ToolExecutor.KernelTools {
		fmt.Fprintf(&b, "%s %T\n", kt.Spec().Name, kt)
	}
	section("Approval kinds with a handler")
	b.WriteString(strings.Join(mapKeys(reflect.ValueOf(k.Server.ApprovalHub).Elem().FieldByName("handlers")), "\n") + "\n")
	section("Event-bus subscriptions after bootstrapKernel")
	dumpSubs(&b, k)
	section("KernelServices handed to a plugin at Build")
	dumpFields(&b, reflect.ValueOf(&svc).Elem())

	// startKernelServices: serve, then stop the listener so Serve returns and
	// g.Wait orders every write the service goroutines made before the reads.
	g, gCtx := errgroup.WithContext(ctx)
	startKernelServices(g, gCtx, k)
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, derr := net.DialTimeout("tcp", lis.Addr().String(), 200*time.Millisecond)
		if derr == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gRPC listener never accepted: %v", derr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Serving is not the same as every service registered: Serve is the
	// goroutine's last call, so wait for the service map to settle.
	time.Sleep(500 * time.Millisecond)
	cancel()
	_ = lis.Close()
	_ = g.Wait()

	section("gRPC services after startKernelServices")
	if k.GRPC == nil {
		b.WriteString("nil\n")
	} else {
		var names []string
		for name := range k.GRPC.GetServiceInfo() {
			names = append(names, name)
		}
		sort.Strings(names)
		b.WriteString(strings.Join(names, "\n") + "\n")
	}
	section("Event-bus subscriptions after startKernelServices")
	dumpSubs(&b, k)

	shutdownWithin(t, k, 30*time.Second)
	return strings.TrimLeft(b.String(), "\n")
}

func dumpSubs(b *strings.Builder, k *Kernel) {
	subs := reflect.ValueOf(k.EventBus).Elem().FieldByName("subs")
	for _, key := range mapKeys(subs) {
		fmt.Fprintf(b, "%s %d\n", key, subs.MapIndex(reflect.ValueOf(key)).Len())
	}
}

func mapKeys(m reflect.Value) []string {
	var out []string
	for _, k := range m.MapKeys() {
		out = append(out, k.String())
	}
	sort.Strings(out)
	return out
}

// dumpFields writes one line per struct field: its name and describeValue.
func dumpFields(b *strings.Builder, v reflect.Value) {
	for i := 0; i < v.NumField(); i++ {
		fmt.Fprintf(b, "%s: %s\n", v.Type().Field(i).Name, describeValue(v.Field(i)))
	}
}

// describeValue is deterministic across runs: nil or the dynamic type for
// references, the value for bools and integers, set/empty for strings (paths
// differ per run), the type for structs.
func describeValue(f reflect.Value) string {
	switch f.Kind() {
	case reflect.Interface:
		if f.IsNil() {
			return "nil"
		}
		return f.Elem().Type().String()
	case reflect.Pointer, reflect.Func, reflect.Chan:
		if f.IsNil() {
			return "nil"
		}
		return f.Type().String()
	case reflect.Map, reflect.Slice:
		if f.IsNil() {
			return "nil"
		}
		return fmt.Sprintf("%s len=%d", f.Type(), f.Len())
	case reflect.Bool:
		return fmt.Sprintf("%t", f.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return fmt.Sprintf("%s=%d", f.Type(), f.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return fmt.Sprintf("%s=%d", f.Type(), f.Uint())
	case reflect.Float32, reflect.Float64:
		return fmt.Sprintf("%s=%g", f.Type(), f.Float())
	case reflect.String:
		if f.Len() == 0 {
			return "string(empty)"
		}
		return "string(set)"
	default:
		return f.Type().String()
	}
}

// lineDiff lists the lines only in want (-) and only in got (+), in order.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	inG := map[string]int{}
	for _, l := range g {
		inG[l]++
	}
	inW := map[string]int{}
	for _, l := range w {
		inW[l]++
	}
	var out strings.Builder
	for _, l := range w {
		if inG[l] == 0 {
			out.WriteString("- " + l + "\n")
		} else {
			inG[l]--
		}
	}
	for _, l := range g {
		if inW[l] == 0 {
			out.WriteString("+ " + l + "\n")
		} else {
			inW[l]--
		}
	}
	return out.String()
}
