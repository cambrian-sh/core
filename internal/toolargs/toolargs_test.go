package toolargs

import (
	"strings"
	"testing"
)

const mailDraft = `{"type":"object","additionalProperties":false,"required":["to","subject"],"properties":{
 "to":{"type":"array","items":{"type":"string"}},
 "subject":{"type":"string"},
 "priority":{"type":"integer"},
 "urgent":{"type":"boolean"},
 "meta":{"type":"object","properties":{"cc":{"type":"array","items":{"type":"string"}}}}}}`

// T-20261108-assistant-passes-mail-draft-to-as-a-json: the live o3x run sent
// {"to": "[\"jarek…\"]"} and the world stored the literal string. The kernel now
// coerces the JSON-encoded array before dispatch.
func TestValidateArgs_CoercesTheUnambiguousMistakes(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"array as a JSON string":   {`{"to":"[\"jarek@x.example\"]","subject":"s"}`, `{"subject":"s","to":["jarek@x.example"]}`},
		"single value for a list":  {`{"to":"a@x","subject":"s"}`, `{"subject":"s","to":["a@x"]}`},
		"numeric and bool strings": {`{"to":["a"],"subject":"s","priority":"2","urgent":"true"}`, `{"priority":2,"subject":"s","to":["a"],"urgent":true}`},
		"number where a string":    {`{"to":["a"],"subject":42}`, `{"subject":"42","to":["a"]}`},
		"nested object as string":  {`{"to":["a"],"subject":"s","meta":"{\"cc\":\"b@x\"}"}`, `{"meta":{"cc":["b@x"]},"subject":"s","to":["a"]}`},
		"already valid":            {`{"to":["a"],"subject":"s"}`, `{"to":["a"],"subject":"s"}`},
	}
	for name, c := range cases {
		got, err := Validator{}.ValidateArgs("mail_draft", []byte(mailDraft), []byte(c.in))
		if err != nil || string(got) != c.want {
			t.Errorf("%s: got %s err %v, want %s", name, got, err, c.want)
		}
	}
}

func TestValidateArgs_RefusesWhatCannotBeFixedWithAnActionableError(t *testing.T) {
	for name, in := range map[string]string{
		"missing required": `{"to":["a"]}`,
		"unknown argument": `{"to":["a"],"subject":"s","bcc":"x"}`,
		"wrong item type":  `{"to":[{"addr":"a"}],"subject":"s"}`,
		"not json":         `{to: a}`,
	} {
		_, err := Validator{}.ValidateArgs("mail_draft", []byte(mailDraft), []byte(in))
		if err == nil || !strings.Contains(err.Error(), "mail_draft") {
			t.Errorf("%s: want an error naming the tool, got %v", name, err)
		}
	}
	// A schema the kernel cannot read never blocks the call.
	if got, err := (Validator{}).ValidateArgs("t", []byte(`not a schema`), []byte(`{"a":1}`)); err != nil || string(got) != `{"a":1}` {
		t.Fatalf("got %s %v", got, err)
	}
}
