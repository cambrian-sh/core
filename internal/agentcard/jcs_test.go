package agentcard

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

// RFC 8785 Appendix B: IEEE-754 bit patterns and their canonical serialisation.
// An empty want means the value must be refused (NaN, Infinity).
var rfc8785Numbers = []struct{ bits, want string }{
	{"0000000000000000", "0"},
	{"8000000000000000", "0"},
	{"0000000000000001", "5e-324"},
	{"8000000000000001", "-5e-324"},
	{"7fefffffffffffff", "1.7976931348623157e+308"},
	{"ffefffffffffffff", "-1.7976931348623157e+308"},
	{"4340000000000000", "9007199254740992"},
	{"c340000000000000", "-9007199254740992"},
	{"4430000000000000", "295147905179352830000"},
	{"7fffffffffffffff", ""},
	{"7ff0000000000000", ""},
	{"44b52d02c7e14af5", "9.999999999999997e+22"},
	{"44b52d02c7e14af6", "1e+23"},
	{"44b52d02c7e14af7", "1.0000000000000001e+23"},
	{"444b1ae4d6e2ef4e", "999999999999999700000"},
	{"444b1ae4d6e2ef4f", "999999999999999900000"},
	{"444b1ae4d6e2ef50", "1e+21"},
	{"3eb0c6f7a0b5ed8c", "9.999999999999997e-7"},
	{"3eb0c6f7a0b5ed8d", "0.000001"},
	{"41b3de4355555553", "333333333.3333332"},
	{"41b3de4355555554", "333333333.33333325"},
	{"41b3de4355555555", "333333333.3333333"},
	{"41b3de4355555556", "333333333.3333334"},
	{"41b3de4355555557", "333333333.33333343"},
	{"becbf647612f3696", "-0.0000033333333333333333"},
	{"43143ff3c1cb0959", "1424953923781206.2"},
}

func TestFormatNumberRFC8785AppendixB(t *testing.T) {
	for _, tc := range rfc8785Numbers {
		raw, err := hex.DecodeString(tc.bits)
		if err != nil {
			t.Fatal(err)
		}
		f := math.Float64frombits(binary.BigEndian.Uint64(raw))
		got, err := FormatNumber(f)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s: want refusal, got %q", tc.bits, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: FormatNumber = %q, %v; want %q", tc.bits, got, err, tc.want)
		}
		// And through the parser: the JSON spelling of the same double must
		// canonicalise to the same text.
		js := []byte("[" + strings.TrimSpace(tc.want) + "]")
		c, err := Canonicalize(js)
		if err != nil || string(c) != "["+tc.want+"]" {
			t.Errorf("%s: Canonicalize(%s) = %s, %v", tc.bits, js, c, err)
		}
	}
}

// RFC 8785 §3.2.2 input and §3.2.3 canonical output.
func TestCanonicalizeRFC8785Example(t *testing.T) {
	in := `{
  "numbers": [333333333.33333329, 1E30, 4.50,
              2e-3, 0.000000000000000000000000001],
  "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
  "literals": [null, true, false]
}`
	want := `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`
	got, err := Canonicalize([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("Canonicalize:\n got %s\nwant %s", got, want)
	}
}

// RFC 8785 §3.2.3 sorting vector: names compared as UTF-16 code units.
func TestCanonicalizeRFC8785Sorting(t *testing.T) {
	in := `{
  "\u20ac": "Euro Sign",
  "\r": "Carriage Return",
  "\ufb33": "Hebrew Letter Dalet With Dagesh",
  "1": "One",
  "\ud83d\ude00": "Emoji: Grinning Face",
  "\u0080": "Control",
  "\u00f6": "Latin Small Letter O With Diaeresis"
}`
	got, err := Canonicalize([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	// Decode the canonical text in order and read the values back.
	dec := json.NewDecoder(strings.NewReader(string(got)))
	var order []string
	_, _ = dec.Token()
	for dec.More() {
		_, _ = dec.Token()
		v, _ := dec.Token()
		order = append(order, v.(string))
	}
	want := []string{"Carriage Return", "One", "Control", "Latin Small Letter O With Diaeresis",
		"Euro Sign", "Emoji: Grinning Face", "Hebrew Letter Dalet With Dagesh"}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("order = %v\nwant    %v", order, want)
	}
}

func TestCanonicalizeRefusals(t *testing.T) {
	for name, in := range map[string]string{
		"duplicate key":        `{"a":1,"a":2}`,
		"nested duplicate key": `{"x":{"b":true,"b":false}}`,
		"trailing data":        `{} {}`,
		"invalid utf-8":        "{\"a\":\"\xff\"}",
		"number out of range":  `[1e400]`,
		"not json":             `{"a":}`,
	} {
		if _, err := Canonicalize([]byte(in)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := Canonicalize([]byte(`{"a":1,"a":1}`)); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("duplicate key: err = %v, want ErrDuplicateKey", err)
	}
}

func TestCanonicalizeEscapesAndWhitespace(t *testing.T) {
	in := "{ \"b\" : [ ] , \"a\" : { } , \"c\" : \"<&>\\u2028\\u0001\\t\" , \"d\":-0.0, \"e\": 100 }"
	want := "{\"a\":{},\"b\":[],\"c\":\"<&> \\u0001\\t\",\"d\":0,\"e\":100}"
	got, err := Canonicalize([]byte(in))
	if err != nil || string(got) != want {
		t.Fatalf("got %s, %v\nwant %s", got, err, want)
	}
}
