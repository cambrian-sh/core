package memory

import "testing"

// Cycle 1: BLOCKED: prefix → ErrorKindBlocked (tracer bullet)
func TestIsErrorOutput_Blocked(t *testing.T) {
	ok, kind := IsErrorOutput("BLOCKED: 'write' is not in ALLOWED_COMMANDS")
	if !ok {
		t.Fatal("expected IsErrorOutput to return true for BLOCKED: prefix")
	}
	if kind != ErrorKindBlocked {
		t.Fatalf("expected ErrorKindBlocked, got %q", kind)
	}
}

// Cycle 2–9: full signal matrix via table-driven test.
func TestIsErrorOutput_AllSignals(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantErr  bool
		wantKind ErrorKind
	}{
		// clean inputs
		{"clean text", "The Sieve of Eratosthenes runs in O(n log log n).", false, ""},
		{"empty string", "", false, ""},

		// BLOCKED prefix
		{"BLOCKED prefix", "BLOCKED: 'rm' is not in ALLOWED_COMMANDS", true, ErrorKindBlocked},

		// FAILURE / ERROR generic prefixes
		{"FAILURE prefix", "FAILURE: agent returned nil payload", true, ErrorKindGeneric},
		{"ERROR prefix", "ERROR: connection refused to substrate", true, ErrorKindGeneric},

		// JSON exit_code non-zero
		{"exit_code non-zero", `{"stdout":"","stderr":"","exit_code":1}`, true, ErrorKindNonZeroExit},
		{"exit_code 127", `{"stdout":"","stderr":"","exit_code":127}`, true, ErrorKindNonZeroExit},
		{"exit_code zero is clean", `{"stdout":"ok","stderr":"","exit_code":0}`, false, ""},

		// JSON stderr non-empty
		{"stderr non-empty", `{"stdout":"","stderr":"some warning","exit_code":0}`, true, ErrorKindStderr},
		{"stderr empty is clean", `{"stdout":"result","stderr":"","exit_code":0}`, false, ""},

		// exception substrings
		{"SyntaxError", "SyntaxError: invalid syntax on line 3", true, ErrorKindException},
		{"ModuleNotFoundError", "ModuleNotFoundError: No module named 'numpy'", true, ErrorKindException},
		{"Traceback", "Traceback (most recent call last):\n  File ...", true, ErrorKindException},

		// runtime/infra failure narrations laundered into an agent answer — these must
		// NOT be consolidated as durable knowledge (the web_search-poisoning gap).
		{"grpc deadline", "system tool 'web_search' failed: DEADLINE_EXCEEDED", true, ErrorKindRuntime},
		{"inactive rpc", "<_InactiveRpcError of RPC that terminated with status=...>", true, ErrorKindRuntime},
		{"errno not found", "can't open file 'web_tool.py': [Errno 2] No such file or directory", true, ErrorKindRuntime},
		{"tool jail path", "file paths (e.g., 'C:\\\\...\\\\cambrian-tool-3789890053\\\\tools\\\\web_tool.py')", true, ErrorKindRuntime},
		// a legitimate fact that merely mentions a tool name stays clean (no false positive).
		{"clean tool mention", "The web_search tool returns the top results for a query.", false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, kind := IsErrorOutput(tc.text)
			if ok != tc.wantErr {
				t.Errorf("IsErrorOutput(%q) ok=%v, want %v", tc.text, ok, tc.wantErr)
			}
			if kind != tc.wantKind {
				t.Errorf("IsErrorOutput(%q) kind=%q, want %q", tc.text, kind, tc.wantKind)
			}
		})
	}
}
