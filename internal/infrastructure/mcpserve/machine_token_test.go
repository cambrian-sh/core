package mcpserve

import "testing"

// ADR-0127 CL-5: the broker's one credential. VerifyMachineToken is what
// premium's machine_token_v1 ingress verifier reaches through
// KernelServices.VerifyMachineToken; every absence fails closed.
func TestVerifyMachineToken(t *testing.T) {
	t.Parallel()
	store := staticSecrets(map[string]string{
		ClientSecretName("laptop"):      "tok-laptop",
		WorkerOwnerSecretName("laptop"): "owner-a",
		ClientSecretName("ci-bot"):      "tok-ci", // an MCP client, not a worker
		ClientSecretName("blank"):       "",
		WorkerOwnerSecretName("blank"):  "owner-b",
	})
	cases := []struct {
		name, machine, token string
		wantOwner            string
		wantOK               bool
	}{
		{"valid worker token", "laptop", "tok-laptop", "owner-a", true},
		{"wrong token", "laptop", "tok-ci", "", false},
		{"another machine's token", "ci-bot", "tok-laptop", "", false},
		{"non-worker client cannot ingest as a machine", "ci-bot", "tok-ci", "", false},
		{"never issued / revoked", "desktop", "tok-laptop", "", false},
		{"empty stored secret never matches an empty token", "blank", "", "", false},
		{"empty presented token", "laptop", "", "", false},
		{"empty machine", "", "tok-laptop", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner, ok := VerifyMachineToken(store, c.machine, c.token)
			if ok != c.wantOK || owner != c.wantOwner {
				t.Errorf("VerifyMachineToken(%q, %q) = (%q, %v), want (%q, %v)",
					c.machine, c.token, owner, ok, c.wantOwner, c.wantOK)
			}
		})
	}
	if _, ok := VerifyMachineToken(nil, "laptop", "tok-laptop"); ok {
		t.Error("a nil store must refuse, never accept")
	}
}
