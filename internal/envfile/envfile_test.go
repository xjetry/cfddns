package envfile_test

import (
	"strings"
	"testing"

	"cfddns/internal/envfile"
)

func TestEncodeParse_RoundTrip(t *testing.T) {
	pairs := []envfile.Pair{
		{Key: "CFDDNS_V4", Value: "home.example.com"},
		{Key: "CFDDNS_TOKEN", Value: "abc-DEF_123"},
		{Key: "CFDDNS_PASSWD", Value: "p@ss!w0rd$%^&*()[]{}<>?,./|~`+=-"},
		{Key: "CFDDNS_PERIOD", Value: "5m0s"},
	}
	text, err := envfile.Encode(pairs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, `"`) {
		t.Fatalf("values must be bare: %q", text)
	}
	got, err := envfile.Parse(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pairs {
		if got[p.Key] != p.Value {
			t.Errorf("%s: got %q want %q", p.Key, got[p.Key], p.Value)
		}
	}
}

func TestCheckValue_RejectsAmbiguousCharacters(t *testing.T) {
	for _, bad := range []string{"a b", "a\tb", `a"b`, "a'b", `a\b`, "a#b", "a;b", "é", "a\nb"} {
		if err := envfile.CheckValue(bad); err == nil {
			t.Errorf("CheckValue(%q) = nil, want error", bad)
		}
	}
	for _, ok := range []string{"", "abc", "p@ss!w0rd$", "a=b", "5m0s", "home.example.com"} {
		if err := envfile.CheckValue(ok); err != nil {
			t.Errorf("CheckValue(%q) = %v, want nil", ok, err)
		}
	}
}

func TestEncode_RejectsBadValueWithKey(t *testing.T) {
	_, err := envfile.Encode([]envfile.Pair{{Key: "CFDDNS_PASSWD", Value: "has space"}})
	if err == nil || !strings.Contains(err.Error(), "CFDDNS_PASSWD") {
		t.Fatalf("err = %v, want error naming the key", err)
	}
	_, err = envfile.Encode([]envfile.Pair{{Key: "bad-key", Value: "x"}})
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
}

func TestParse_IgnoresCommentsAndBlank(t *testing.T) {
	in := "# comment\n\n; also comment\n  CFDDNS_V4=home.example.com  \nCFDDNS_PORT=\n"
	got, err := envfile.Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if got["CFDDNS_V4"] != "home.example.com" {
		t.Errorf("V4 = %q", got["CFDDNS_V4"])
	}
	if v, ok := got["CFDDNS_PORT"]; !ok || v != "" {
		t.Errorf("PORT: present=%v value=%q, want present and empty", ok, v)
	}
}

func TestParse_RejectsQuotedAndMalformed(t *testing.T) {
	for _, in := range []string{
		`CFDDNS_PASSWD="quoted"`,
		"CFDDNS_PASSWD='q'",
		`CFDDNS_PASSWD=a\b`,
		"CFDDNS_PASSWD= leading",
		"CFDDNS_PASSWD=a b",
		"no-equals-sign",
		"lower=case",
	} {
		if _, err := envfile.Parse(strings.NewReader(in)); err == nil {
			t.Errorf("Parse(%q) = nil error, want error", in)
		}
	}
}
