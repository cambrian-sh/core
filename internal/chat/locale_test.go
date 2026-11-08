package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261107-chat-failure-texts-are-english-only-in-non: the kernel's fixed failure
// sentences follow the conversation's language, from a catalogue, never a model.

func TestDetectLanguage(t *testing.T) {
	cases := []struct{ text, want string }{
		{"Komşumun ruhsat başvurusunu görebilir miyim?", "tr"},
		{"Merhaba, bugün nöbetçi eczane hangisi?", "tr"},
		{"merhaba nasil basvuru yaparim lutfen", "tr"}, // Turkish typed without Turkish letters
		{"İmar durumu belgesi için ne gerekiyor", "tr"},
		{"Can you show me my open invoices?", "en"},
		{"What is the status of the permit?", "en"},
		{"ok", ""},
		{"12345", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := DetectLanguage(tc.text); got != tc.want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// Adding a language is data: a detectable language must have every message.
func TestCatalogue_EveryDetectableLanguageIsComplete(t *testing.T) {
	for _, p := range languageProfiles {
		m, ok := messageCatalogue[p.code]
		if !ok {
			t.Fatalf("language %q is detectable but has no catalogue", p.code)
		}
		for _, id := range allMessageIDs {
			if m[id] == "" {
				t.Errorf("catalogue %q lacks message %q", p.code, id)
			}
		}
	}
	for lang := range messageCatalogue {
		if len(messageCatalogue[lang]) != len(allMessageIDs) {
			t.Errorf("catalogue %q has %d messages, want %d", lang, len(messageCatalogue[lang]), len(allMessageIDs))
		}
	}
}

func TestHumanFailure_TurkishOnEveryPath(t *testing.T) {
	errs := []error{
		ErrTurnStalled, ErrEmptyReply, context.DeadlineExceeded, context.Canceled,
		errors.New("agentpool: ErrPoolBusy"), errors.New("session not found: lease-1"),
		errors.New("upstream timeout"), errors.New("boom"),
	}
	seen := map[string]bool{}
	for _, err := range errs {
		tr, en := humanFailure(err, "tr"), humanFailure(err, "en")
		if tr == "" || tr == en {
			t.Errorf("%v: Turkish text %q must exist and differ from English %q", err, tr, en)
		}
		if tr != messageCatalogue["tr"][failureMessage(err)] {
			t.Errorf("%v: %q is not the Turkish catalogue entry", err, tr)
		}
		seen[tr] = true
	}
	if len(seen) != len(allMessageIDs) {
		t.Errorf("expected every message id to be reachable, saw %d distinct texts", len(seen))
	}
	// A regional tag uses its primary language; an unknown language falls back to English.
	if got := humanFailure(ErrEmptyReply, "tr-TR"); got != messageCatalogue["tr"][MsgEmptyReply] {
		t.Errorf("tr-TR = %q", got)
	}
	if got := humanFailure(ErrEmptyReply, "xx"); got != messageCatalogue["en"][MsgEmptyReply] {
		t.Errorf("unknown language must fall back to English, got %q", got)
	}
}

func finalNote(t *testing.T, sink *capturingSink) string {
	t.Helper()
	for i := len(sink.got) - 1; i >= 0; i-- {
		if sink.got[i].Final {
			return sink.got[i].Note
		}
	}
	t.Fatal("no final progress update")
	return ""
}

func TestTurn_FailureLineFollowsTheConversationLanguage(t *testing.T) {
	ctx := context.Background()

	// A Turkish turn gets the Turkish failure line.
	svc, _, pool := setup(t, domain.ProfileEmployee)
	sink := &capturingSink{}
	svc.SetProgressSink(sink)
	pool.reply = "   "
	if _, err := svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "Ruhsat başvurum ne durumda?"}); err == nil {
		t.Fatal("an empty reply must fail the turn")
	}
	if got, want := finalNote(t, sink), messageCatalogue["tr"][MsgEmptyReply]; got != want {
		t.Errorf("Turkish turn failure line = %q, want %q", got, want)
	}

	// An undetectable message in a Turkish conversation stays Turkish (from the history).
	pool.reply = "Tamam."
	if _, err := svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "Merhaba, belediye nerede?"}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	pool.err = errors.New("agentpool: ErrPoolBusy")
	sink.got = nil
	_, _ = svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "ok"})
	if got, want := finalNote(t, sink), messageCatalogue["tr"][MsgAtCapacity]; got != want {
		t.Errorf("follow-up failure line = %q, want %q", got, want)
	}

	// English is unchanged.
	svc2, _, pool2 := setup(t, domain.ProfileEmployee)
	sink2 := &capturingSink{}
	svc2.SetProgressSink(sink2)
	pool2.reply = ""
	_, _ = svc2.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "What is my permit status?"})
	if got := finalNote(t, sink2); got != "I wasn't able to produce an answer for that one. Try rephrasing it?" {
		t.Errorf("English failure line changed: %q", got)
	}
}

func TestTurn_DefaultLanguageIsConfigurable(t *testing.T) {
	svc, _, pool := setup(t, domain.ProfileEmployee)
	svc.DefaultLanguage = "tr"
	sink := &capturingSink{}
	svc.SetProgressSink(sink)
	pool.reply = ""
	_, _ = svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "12345"})
	if got, want := finalNote(t, sink), messageCatalogue["tr"][MsgEmptyReply]; got != want {
		t.Errorf("undetectable first message with default tr = %q, want %q", got, want)
	}
}

func TestHandoff_CarriesTheTurnLanguage(t *testing.T) {
	svc, _, pool := setup(t, domain.ProfileEmployee)
	ctx := context.Background()
	if _, err := svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "Nöbetçi eczane hangisi?"}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if _, err := svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "Show me my invoices please"}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	hs := pool.all()
	if got := hs[0].Payload.Metadata["language"]; got != "tr" {
		t.Errorf("Turkish turn handoff language = %q, want tr", got)
	}
	if got := hs[1].Payload.Metadata["language"]; got != "en" {
		t.Errorf("English turn handoff language = %q, want en", got)
	}
}

// The status-line phases (domain progress catalogue) and the failure sentences cover the
// same languages, so a detectable language is never half-translated.
func TestCatalogue_ProgressPhasesCoverTheSameLanguages(t *testing.T) {
	langs := domain.ProgressLanguages()
	if len(langs) != len(messageCatalogue) {
		t.Fatalf("progress languages %v vs failure catalogue %d languages", langs, len(messageCatalogue))
	}
	for _, l := range langs {
		if _, ok := messageCatalogue[l]; !ok {
			t.Errorf("progress language %q has no failure catalogue", l)
		}
	}
}

func TestTurn_StatusLineFollowsTheConversationLanguage(t *testing.T) {
	svc, _, _ := setup(t, domain.ProfileEmployee)
	sink := &capturingSink{}
	svc.SetProgressSink(sink)
	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "Ruhsat başvurum ne durumda?"}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	var texts []string
	for _, u := range sink.got {
		if u.Language != "tr" {
			t.Errorf("update %+v does not carry the turn language", u)
		}
		texts = append(texts, u.Text())
	}
	want := []string{"istek anlaşılıyor", "üzerinde çalışılıyor", "yanıt yazılıyor", ""}
	if len(texts) != len(want) {
		t.Fatalf("status lines = %q, want %q", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Errorf("status line %d = %q, want %q", i, texts[i], want[i])
		}
	}

	svc2, _, _ := setup(t, domain.ProfileEmployee)
	sink2 := &capturingSink{}
	svc2.SetProgressSink(sink2)
	_, _ = svc2.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "What is my permit status?"})
	if got := sink2.got[0].Text(); got != "understanding the request" {
		t.Errorf("English status line changed: %q", got)
	}
}
