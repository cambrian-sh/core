package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The reply that leaked in run 20261107T191751Z_usecases_full (g-tr-records-request-1 #2):
// the working summary listed every redaction by its value.
const leakedTurkishReply = `BE-2026/0001 BİLGİ EDİNME BAŞVURUSU CEVAP TASLAĞI

Başvuru Bilgileri:
- Başvuru Tarihi: 03.09.2026
- CEVAP SÜRESİNİN SON GÜNÜ: 24.09.2026 (Başvuru tarihinden itibaren 21 gün)

VERİLEN BELGELER (Tam Olarak):
1. Teknik Rapor - 1. Tespit Tutanağı (E-74183520-754.01-1110, 17.01.2026)

KİŞİSEL VERILER KARARTILARAK VERİLEN BELGELER:
1. Vatandaş Yazışması (E-74183520-622.01-1112, 13.02.2026)
   - Karartılacak Veriler: Merve Güneş'in T.C. kimlik numarası (10304949101), adresi (Esentepe Mahallesi, Lale Caddesi No:10), telefon numarası (0 (509) 803 65 52)
2. Polis Tespit Tutanağı (E-74183520-310.04-1113, 06.04.2026)
   - Karartılacak Veriler: Yusuf Bulut'un T.C. kimlik numarası (55879350776), telefon numarası (0 (509) 735 17 39)`

func TestGuard_WithholdingParagraphNeverStatesTheValuesItWithholds(t *testing.T) {
	out, held := guardDisclosure(leakedTurkishReply, domain.ProfileEmployee, nil, "tr")
	for _, v := range []string{"10304949101", "55879350776", "0 (509) 803 65 52", "0 (509) 735 17 39"} {
		if strings.Contains(out, v) {
			t.Errorf("guarded reply still states %q:\n%s", v, out)
		}
	}
	if held[idNationalID] != 2 || held[idPhone] != 2 || held.total() != 4 {
		t.Errorf("held back %v, want 2 national IDs and 2 phone numbers", held)
	}
	// What the officer needs survives: references, dates, the deadline, the categories.
	for _, keep := range []string{"E-74183520-310.04-1113", "E-74183520-754.01-1110", "24.09.2026",
		"03.09.2026", "T.C. kimlik numarası ([kimlik numarası gizlendi])", "[telefon numarası gizlendi]"} {
		if !strings.Contains(out, keep) {
			t.Errorf("guarded reply lost %q:\n%s", keep, out)
		}
	}
	if !strings.HasSuffix(out, "Bu yanıtta gösterilmeyen kişisel veriler: kimlik numarası ×2, telefon numarası ×2. "+
		"Görmeye yetkili olanlar için kaynak kayıtlarda yer alıyor.") {
		t.Errorf("held-back line missing or not in Turkish:\n%s", out)
	}
}

func TestGuard_EnglishWithholdingParagraph(t *testing.T) {
	reply := "Released with personal data redacted (s.40(2)):\n" +
		"- HDC/CS/2025/00023 - redact Owen Cooper's NI number QQ 81 28 80 C, phone 07700 900767 and owen@example.org\n" +
		"- IBAN on the invoice: GB82 WEST 1234 5698 7654 32\n\n" +
		"Statutory response date: 16 October 2026."
	out, held := guardDisclosure(reply, domain.ProfileEmployee, nil, "en")
	for _, v := range []string{"QQ 81 28 80 C", "07700 900767", "owen@example.org", "GB82 WEST 1234 5698 7654 32"} {
		if strings.Contains(out, v) {
			t.Errorf("still states %q:\n%s", v, out)
		}
	}
	if held.total() != 4 {
		t.Errorf("held = %v", held)
	}
	if !strings.Contains(out, "HDC/CS/2025/00023") || !strings.Contains(out, "16 October 2026") {
		t.Errorf("lost the reference or the date:\n%s", out)
	}
	if !strings.Contains(out, "Personal data left out of this reply: national ID number, phone number, e-mail address, bank account number.") {
		t.Errorf("held-back line:\n%s", out)
	}
}

// An entitled officer asking for a contact detail gets it: the guard is not a blanket filter.
func TestGuard_InternalReplyWithoutWithholdingIsUntouched(t *testing.T) {
	reply := "The applicant, Cengiz Erdem, can be reached on 0 (509) 214 55 10 or cengiz.erdem@posta.example."
	out, held := guardDisclosure(reply, domain.ProfileEmployee, nil, "en")
	if out != reply || held != nil {
		t.Fatalf("an internal reply with no withholding context was changed:\n%s", out)
	}
}

func TestGuard_CustomerReplyMasksStrongIdentifiersButNotWhatTheUserGave(t *testing.T) {
	reply := "Your application is registered under your ID 12345678901. The applicant next door is 55879350776. " +
		"Call the zoning office on +90 212 555 12 34."
	out, held := guardDisclosure(reply, domain.ProfileCustomer, []string{"Benim kimlik numaram 12345678901"}, "tr")
	if !strings.Contains(out, "12345678901") {
		t.Errorf("the user's own number, which they wrote, was masked:\n%s", out)
	}
	if strings.Contains(out, "55879350776") {
		t.Errorf("a third party's national ID reached a customer:\n%s", out)
	}
	if !strings.Contains(out, "+90 212 555 12 34") {
		t.Errorf("a contact number outside any withholding paragraph was masked for a customer:\n%s", out)
	}
	if held[idNationalID] != 1 || held.total() != 1 {
		t.Errorf("held = %v", held)
	}
}

func TestGuard_UserSuppliedValueInAWithholdingParagraphIsKept(t *testing.T) {
	reply := "Done: I redacted 0555 123 45 67 from the letter."
	out, _ := guardDisclosure(reply, domain.ProfileEmployee, []string{"please redact 05551234567 from it"}, "en")
	if out != reply {
		t.Fatalf("a value the user wrote was masked:\n%s", out)
	}
}

// Reference numbers, dates, amounts and years must never read as identifiers, even inside
// a withholding paragraph.
func TestGuard_NoFalsePositivesInAWithholdingParagraph(t *testing.T) {
	reply := "Withheld under s.43: contract HDC/PR/2026/00026 (£935,300), record E-74183520-640.99-1114 of 23.03.2026, " +
		"deadline 24.09.2026 (21 gün), 1.234.567.890 TL, items 2024 2025 2026, ratio 0.512, page 12-14, " +
		"ref 2026/0054 and YRB-2026/0054, case 4982."
	out, held := guardDisclosure(reply, domain.ProfileEmployee, nil, "en")
	if out != reply || held != nil {
		t.Fatalf("false positive:\n%s\nheld=%v", out, held)
	}
}

func TestGuard_CardNeedsLuhnAndPhoneNeedsLength(t *testing.T) {
	for _, tc := range []struct {
		text string
		kind identifierKind
		ok   bool
	}{
		{"4111 1111 1111 1111", idCard, true},
		{"4111 1111 1111 1112", "", false},
		{"0212 555 12 34", idPhone, true},
		{"555 12 34", "", false},
		{"123-45-6789", idNationalID, true},
	} {
		ids := findIdentifiers("redact " + tc.text + " now")
		switch {
		case !tc.ok && len(ids) != 0:
			t.Errorf("%q detected as %v", tc.text, ids)
		case tc.ok && (len(ids) != 1 || ids[0].kind != tc.kind || ids[0].value != tc.text):
			t.Errorf("%q: got %+v, want one %s", tc.text, ids, tc.kind)
		}
	}
}

func TestDisclosureCatalogue_EveryDetectableLanguageIsComplete(t *testing.T) {
	for _, p := range languageProfiles {
		c, ok := disclosureCatalogue[p.code]
		if !ok {
			t.Fatalf("language %q is detectable but has no disclosure catalogue", p.code)
		}
		if _, ok := withholdingCues[p.code]; !ok {
			t.Errorf("language %q has no withholding cues", p.code)
		}
		for _, k := range identifierOrder {
			if c.label[k] == "" || c.noun[k] == "" {
				t.Errorf("catalogue %q lacks %s", p.code, k)
			}
		}
		if strings.Count(c.line, "%s") != 1 {
			t.Errorf("catalogue %q line must hold exactly one %%s", p.code)
		}
	}
}

// The guard runs on the turn path: what is stored and returned is the guarded reply.
func TestTurn_StoredReplyIsGuarded(t *testing.T) {
	svc, store, pool := setup(t, domain.ProfileEmployee)
	pool.reply = "To be redacted: the complainant's phone 0 (509) 735 17 39."
	got, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "draft the FOI response"})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if strings.Contains(got.Content, "735 17 39") {
		t.Fatalf("returned reply still states the number: %q", got.Content)
	}
	msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
	if strings.Contains(msgs[len(msgs)-1].Content, "735 17 39") {
		t.Fatalf("stored reply still states the number: %q", msgs[len(msgs)-1].Content)
	}
}
