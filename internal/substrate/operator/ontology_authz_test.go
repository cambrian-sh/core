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
