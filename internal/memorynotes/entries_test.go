package memorynotes

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

const notesDoc = `Prefers short answers.

## Personal
Locker code: LOCKER-b681ce
Gym on Tuesdays

## Locker
LOCKER-zz99

## Work
` + "```" + `
## not a heading
` + "```" + `
Desk 4B
`

func digestSet(texts ...string) map[string]bool {
	out := map[string]bool{}
	for _, t := range texts {
		out[domain.MemoryNoteEntryDigest(t)[:16]] = true
	}
	return out
}

// ENG-2106: entries are the non-blank lines that are not headings or fence markers,
// each with its section; a "## " inside a fence is content.
func TestEntries(t *testing.T) {
	got := Entries(notesDoc)
	want := []Entry{
		{"Prefers short answers.", ""},
		{"Locker code: LOCKER-b681ce", "Personal"},
		{"Gym on Tuesdays", "Personal"},
		{"LOCKER-zz99", "Locker"},
		{"## not a heading", "Work"},
		{"Desk 4B", "Work"},
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Removing an entry leaves the rest byte-for-byte, drops a heading its section no longer
// has entries under, and leaves no doubled blank line behind.
func TestRemoveEntries(t *testing.T) {
	out, n := RemoveEntries(notesDoc, digestSet("Locker code: LOCKER-b681ce"))
	if n != 1 {
		t.Fatalf("removed %d", n)
	}
	want := "Prefers short answers.\n\n## Personal\nGym on Tuesdays\n\n## Locker\nLOCKER-zz99\n\n## Work\n```\n## not a heading\n```\nDesk 4B\n"
	if out != want {
		t.Fatalf("got:\n%q\nwant:\n%q", out, want)
	}
	out, n = RemoveEntries(notesDoc, digestSet("LOCKER-zz99"))
	if n != 1 || out != "Prefers short answers.\n\n## Personal\nLocker code: LOCKER-b681ce\nGym on Tuesdays\n\n## Work\n```\n## not a heading\n```\nDesk 4B\n" {
		t.Fatalf("emptied section not dropped cleanly:\n%q", out)
	}
	if out, n := RemoveEntries(notesDoc, digestSet("absent")); n != 0 || out != notesDoc {
		t.Fatal("no match must return the content unchanged")
	}
	if out, n := RemoveEntries("## Only\nsecret\n", digestSet("secret")); n != 1 || out != "" {
		t.Fatalf("a document left with nothing must be empty, got %q", out)
	}
}

// Matching requires EVERY query word (short words dropped), as a word or a word's
// start, over the line and its section heading.
func TestMatchEntries(t *testing.T) {
	es := Entries(notesDoc)
	texts := func(ms []Entry) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Text)
		}
		return out
	}
	for q, want := range map[string][]string{
		"locker code":    {"Locker code: LOCKER-b681ce"},
		"my locker code": {"Locker code: LOCKER-b681ce"},
		"locker":         {"Locker code: LOCKER-b681ce", "LOCKER-zz99"}, // heading "Locker" counts
		"gym":            {"Gym on Tuesdays"},
		"salary":         nil,
		"locker salary":  nil, // partial match never qualifies
		"tue":            {"Gym on Tuesdays"},
		"":               nil,
	} {
		got := texts(MatchEntries(es, q))
		if len(got) != len(want) {
			t.Errorf("%q → %v, want %v", q, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q → %v, want %v", q, got, want)
			}
		}
	}
	tr := Entries("Dolap şifresi: 4471\n")
	if len(MatchEntries(tr, "dolap şifre")) != 1 {
		t.Error("a query word must match the start of a longer word (Turkish suffixes)")
	}
}

func notesService(t *testing.T) *Service {
	t.Helper()
	svc, err := New(NewMemStore(), ScannerFunc(func(string) (bool, string) { return false, "" }),
		domain.AllowAllAuthorizer{}, Config{PersonalBudgetChars: 1000, OrgBudgetChars: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// MatchForErase reads only the owner's PERSONAL document and names entries by their
// erasure id; DropSnapshots makes a frozen run snapshot re-read.
func TestMatchForEraseAndDropSnapshots(t *testing.T) {
	svc := notesService(t)
	ctx := context.Background()
	alice := Principal{ID: "alice", AgentID: "chat"}
	for _, line := range []string{"Locker code: L-1", "Locker code: L-1"} {
		if r, err := svc.Propose(ctx, alice, Proposal{Op: OpAppend, Section: "Personal", Content: line}); err != nil || !r.Accepted {
			t.Fatalf("propose: %+v %v", r, err)
		}
	}
	ms, err := svc.MatchForErase(ctx, "alice", "locker code")
	if err != nil || len(ms) != 1 || ms[0].ID != domain.MemoryNoteEntryID("personal", "alice", "Locker code: L-1") || ms[0].Text != "Locker code: L-1" {
		t.Fatalf("match = %+v %v (duplicate lines are one entry)", ms, err)
	}
	if ms, _ := svc.MatchForErase(ctx, "bob", "locker code"); len(ms) != 0 {
		t.Fatal("another person's notes must not be searched")
	}
	if ms, _ := svc.MatchForErase(ctx, "", "locker code"); len(ms) != 0 {
		t.Fatal("no owner, no notes")
	}

	snap, err := svc.Snapshot(ctx, "run-1", alice)
	if err != nil || len(snap.Documents) != 1 {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}
	// Change the store under the frozen snapshot, as an erase does.
	d, _ := svc.store.Get(ctx, Key{Scope: ScopePersonal, OwnerID: "alice"})
	if _, err := svc.store.Put(ctx, d.Key, "", d.Revision, WriteMeta{Actor: "erase:x", Op: "erase", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if again, _ := svc.Snapshot(ctx, "run-1", alice); again.Version != snap.Version {
		t.Fatal("precondition: the snapshot is frozen for the run")
	}
	svc.DropSnapshots("bob")
	if again, _ := svc.Snapshot(ctx, "run-1", alice); again.Version != snap.Version {
		t.Fatal("dropping another owner's snapshots must keep alice's")
	}
	svc.DropSnapshots("alice")
	if again, _ := svc.Snapshot(ctx, "run-1", alice); len(again.Documents) != 0 {
		t.Fatalf("after DropSnapshots the run must re-read: %+v", again)
	}
	if got := ResourceTags(Key{Scope: ScopePersonal, OwnerID: "alice"}); len(got) != 3 || got[2] != domain.OwnerTag("alice") {
		t.Fatalf("personal resource tags = %v", got)
	}
}
