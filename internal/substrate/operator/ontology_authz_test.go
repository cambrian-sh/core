package operator

import "testing"

// ADR-0149 D2: changing what every write is validated against is an Operator
// command; reading the declarations and the violations report is a read.
func TestOntologyRPCs_OperatorOnlyApply(t *testing.T) {
	const lane = "/cambrian.premium.substrate.SubstrateLane/"
	if !isOperatorOnly(lane + "ApplyOntologyChange") {
		t.Fatal("ApplyOntologyChange must be Operator-only")
	}
	for _, m := range []string{"GetOntology", "ListOntologyViolations"} {
		if isOperatorOnly(lane + m) {
			t.Fatalf("%s is a read, open to Operator and Viewer", m)
		}
	}
}

// The full agent draft is read by the person deciding its ontology_change
// approval — an Operator — so it is Operator-only even though it is named like
// a read (T-20261113-approvers-of-an-ontology-change-see-only-a-200).
func TestOntologyRPCs_GetOntologyDraftIsOperatorOnly(t *testing.T) {
	if !isOperatorOnly("/cambrian.premium.substrate.SubstrateLane/GetOntologyDraft") {
		t.Fatal("GetOntologyDraft must be Operator-only")
	}
	// Matched on the full method: an OperatorConsole read of the same name is not.
	if isOperatorOnly(operatorMethodPrefix + "GetOntologyDraft") {
		t.Fatal("the exception must not leak to another service's method of the same name")
	}
}
