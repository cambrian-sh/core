package agentmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// captureForward runs forwardPipe over `input` and returns the records the
// kernel logger produced.
func captureForward(t *testing.T, input string, isErr bool) []map[string]any {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	forwardPipe(context.Background(), strings.NewReader(input), "telegram_ingress_agent", isErr)

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("kernel emitted a non-JSON record: %q", line)
		}
		out = append(out, rec)
	}
	return out
}

// The bug this fixes. Python's `logging` writes every level to STDERR, so keying
// the level off the stream recorded agent INFO as a kernel ERROR — and a stream
// where almost everything is an error is one where nothing is.
func TestForwardPipe_PythonInfoOnStderrIsNotAnError(t *testing.T) {
	recs := captureForward(t, "INFO:__main__:telegram ingress: polling from update_id 0\n", true)

	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if got := recs[0]["level"]; got != "INFO" {
		t.Fatalf("an agent INFO line was recorded as %v", got)
	}
	// The logger becomes an ATTRIBUTE, not punctuation inside a string no field
	// query can reach.
	if got := recs[0]["logger"]; got != "__main__" {
		t.Fatalf("logger not lifted out: %v", got)
	}
	if got, _ := recs[0]["msg"].(string); got != "telegram ingress: polling from update_id 0" {
		t.Fatalf("message still carries its prefix: %q", got)
	}
}

// A structured line that declares its level must be believed regardless of the
// stream. The old guard ignored `level` entirely on stderr.
func TestForwardPipe_DeclaredLevelWinsOverTheStream(t *testing.T) {
	recs := captureForward(t, `{"level":"info","msg":"agent pool started","size":2}`+"\n", true)

	if got := recs[0]["level"]; got != "INFO" {
		t.Fatalf("declared info on stderr recorded as %v", got)
	}
	// Typed attributes survive — the log console's filter depends on them.
	if got := recs[0]["size"]; got != float64(2) {
		t.Fatalf("attribute lost: %v", got)
	}
}

// The SDK emits JSON whose `msg` still carries the prefix. Both facts have to be
// used: the level, and the logger name.
func TestForwardPipe_PrefixInsideAJSONMessageIsStillHonoured(t *testing.T) {
	recs := captureForward(t,
		`{"msg":"INFO:cambrian.runtime:Agent serving on unix:sock","agent_id":"x"}`+"\n", true)

	if got := recs[0]["level"]; got != "INFO" {
		t.Fatalf("prefix level ignored inside JSON: %v", got)
	}
	if got := recs[0]["logger"]; got != "cambrian.runtime" {
		t.Fatalf("logger not extracted: %v", got)
	}
}

// The fallback must stay loud. A traceback or a panic says no level, and stderr
// is then the only evidence there is.
func TestForwardPipe_UndeclaredStderrIsStillAnError(t *testing.T) {
	recs := captureForward(t, "Traceback (most recent call last):\n", true)

	if got := recs[0]["level"]; got != "ERROR" {
		t.Fatalf("a bare stderr line should stay an error, got %v", got)
	}
}

// Real levels are not flattened either way: a Python ERROR on stderr stays an
// error, and a WARNING is a warning rather than being rounded up.
func TestForwardPipe_RealLevelsSurvive(t *testing.T) {
	recs := captureForward(t,
		"ERROR:daemon:getUpdates failed\nWARNING:daemon:retrying in 8s\nDEBUG:daemon:offset=12\n", true)

	want := []string{"ERROR", "WARN", "DEBUG"}
	if len(recs) != len(want) {
		t.Fatalf("want %d records, got %d", len(want), len(recs))
	}
	for i, w := range want {
		if got := recs[i]["level"]; got != w {
			t.Fatalf("record %d: want %s, got %v", i, w, got)
		}
	}
}

// stdout keeps its existing behaviour: undeclared means info.
func TestForwardPipe_UndeclaredStdoutIsInfo(t *testing.T) {
	recs := captureForward(t, "plain progress line\n", false)

	if got := recs[0]["level"]; got != "INFO" {
		t.Fatalf("want INFO, got %v", got)
	}
	if got := recs[0]["agent_id"]; got != "telegram_ingress_agent" {
		t.Fatalf("agent attribution lost: %v", got)
	}
}
