package identifiers

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Moved from internal/chat/disclosure_test.go with the detector (ENG-917).
func TestFind_CardNeedsLuhnAndPhoneNeedsLength(t *testing.T) {
	for _, tc := range []struct {
		text string
		kind domain.IdentifierKind
		ok   bool
	}{
		{"4111 1111 1111 1111", domain.IdentifierCard, true},
		{"4111 1111 1111 1112", "", false},
		{"0212 555 12 34", domain.IdentifierPhone, true},
		{"555 12 34", "", false},
		{"123-45-6789", domain.IdentifierNationalID, true},
		{"10304949101", domain.IdentifierNationalID, true},
		{"GB82 WEST 1234 5698 7654 32", domain.IdentifierIBAN, true},
		{"GB82 WEST 1234 5698 7654 33", "", false},
		{"merve.gunes@example.com", domain.IdentifierEmail, true},
	} {
		ids := Find("redact " + tc.text + " now")
		switch {
		case !tc.ok && len(ids) != 0:
			t.Errorf("%q detected as %v", tc.text, ids)
		case tc.ok && (len(ids) != 1 || ids[0].Kind != tc.kind || ids[0].Value != tc.text):
			t.Errorf("%q: got %+v, want one %s", tc.text, ids, tc.kind)
		}
	}
}

func TestFind_ReferencesDatesAndAmountsAreNotIdentifiers(t *testing.T) {
	text := "Ref E-74183520-310.04-1113 of 24.09.2026, case HDC/CS/2025/00023, total £935,300 in 2024 2025 2026."
	if ids := Find(text); len(ids) != 0 {
		t.Fatalf("false positives: %+v", ids)
	}
}

func TestNormalise_SamePhoneWrittenTwoWays(t *testing.T) {
	if Normalise("0 (509) 735 17 39") != Normalise("05097351739") {
		t.Fatal("the same number must normalise the same")
	}
}

func TestDetector_ImplementsThePort(t *testing.T) {
	var d domain.IdentifierDetector = Detector{}
	if got := d.FindIdentifiers("call +90 532 123 45 67"); len(got) != 1 || got[0].Kind != domain.IdentifierPhone {
		t.Fatalf("got %+v", got)
	}
}
