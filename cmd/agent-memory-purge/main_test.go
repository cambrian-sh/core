package main

import (
	"strings"
	"testing"
)

// Every DELETE is filtered by the purge selection — never a bare table delete — and
// ingested document chunks (mnemonic_fact with no judge score and no ToolOutput
// marker) match no class.
func TestDeleteStatements_AreAllFiltered(t *testing.T) {
	stmts := deleteStatements()
	if len(stmts) != 4 {
		t.Fatalf("want 4 statements, got %d", len(stmts))
	}
	for _, s := range stmts {
		if !strings.Contains(s, " WHERE ") || !strings.Contains(s, selectedChunks()) {
			t.Errorf("unfiltered or mis-filtered statement: %s", s)
		}
	}
	if !strings.HasPrefix(stmts[len(stmts)-1], "DELETE FROM chunks") {
		t.Error("chunks must be deleted last, after their FK-less dependents")
	}
	for _, c := range purgeClasses {
		if strings.Contains(c.where, "mnemonic_fact") &&
			!strings.Contains(c.where, "scoring_prompt_version") {
			t.Errorf("a mnemonic_fact class without the raw-path marker would select ingested chunks: %s", c.where)
		}
	}
}
