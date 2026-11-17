package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
)

func TestParseEgressModesPrivateEntriesAndTrust(t *testing.T) {
	for line, want := range map[string]command{
		"egress allow box db.corp.example --private":           {name: "egress allow", vm: "box", hosts: []string{"db.corp.example"}, private: true},
		"vm configure box --egress open":                       {name: "vm configure", vm: "box", egress: "open", create: api.CreateParams{VM: "box"}},
		"vm configure box --egress=open --allow-plain-secrets": {name: "vm configure", vm: "box", egress: "open", allowPlain: true, create: api.CreateParams{VM: "box"}},
		"vm configure box --egress restricted":                 {name: "vm configure", vm: "box", egress: "restricted", create: api.CreateParams{VM: "box"}},
		"vm create box --isolated --egress open":               {name: "vm create", vm: "box", egress: "open", create: api.CreateParams{VM: "box", Isolated: true, Open: true}},
		"vm create box --isolated --egress restricted":         {name: "vm create", vm: "box", egress: "restricted", create: api.CreateParams{VM: "box", Isolated: true}},
		"vm create box --egress open":                          {name: "vm create", vm: "box", egress: "open", create: api.CreateParams{VM: "box", Open: true}},
		"secret grant box raw --allow-plain-secrets":           {name: "secret grant", vm: "box", names: []string{"raw"}, allowPlain: true},
		"vm trust list box":                                    {name: "trust list", vm: "box"},
		"vm trust ls box":                                      {name: "trust list", vm: "box"},
		`vm trust add box C:\certs\corp-root.pem`:              {name: "trust add", vm: "box", file: `C:\certs\corp-root.pem`, names: []string{"corp-root"}},
		"vm trust add box ca.pem --name corp":                  {name: "trust add", vm: "box", file: "ca.pem", names: []string{"corp"}},
		"vm trust rm box corp":                                 {name: "trust remove", vm: "box", names: []string{"corp"}},
	} {
		got, err := parse(strings.Fields(line))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parse(%q) = %+v, %v; want %+v", line, got, err, want)
		}
	}
	for line, want := range map[string]string{
		"vm configure box --egress open --cpus 2":                    "pass --egress on its own",
		"vm configure box --egress wide":                             "want open or restricted",
		"vm configure box --egress restricted --allow-plain-secrets": "--allow-plain-secrets goes with --egress open",
		"vm configure box --cpus 2 --allow-plain-secrets":            "--allow-plain-secrets goes with --egress open",
		"egress remove box a.com --private":                          "",
		"secret revoke box raw --allow-plain-secrets":                "",
		"trust list box":          "unknown command",
		"vm trust":                "",
		"vm trust add box":        "",
		"vm trust remove box":     "",
		"vm trust list box extra": "",
	} {
		_, err := parse(strings.Fields(line))
		if _, ok := errors.AsType[usageError](err); !ok || !strings.Contains(err.Error(), want) {
			t.Errorf("parse(%q) err = %v, want a usage error with %q", line, err, want)
		}
	}
}

func TestEgressOutputMarksPrivateEntriesAndTheOpenMode(t *testing.T) {
	e := api.Egress{VM: "box", Allow: []string{"api.example.com", "db.corp.example"}, Private: []string{"db.corp.example"}}
	if got := allowlistLines(e); got != "api.example.com\ndb.corp.example (private)" {
		t.Errorf("restricted list %q", got)
	}
	if got := describeEgressLine(e); got != "  egress: restricted (2 allowed)" {
		t.Errorf("status line %q", got)
	}
	e.Open = true
	open := "box: egress open: any public host, through the egress proxy; the allowlist applies again after glov vm configure box --egress restricted"
	if got := allowlistLines(e); got != open+"\napi.example.com\ndb.corp.example (private)" {
		t.Errorf("open list %q", got)
	}
	if got := describeAllowlist(e); got != open+"\nallowlist: api.example.com, db.corp.example (private)" {
		t.Errorf("open allowlist %q", got)
	}
	if got := describeEgressLine(e); got != "  egress: open (any public host, through the proxy)" {
		t.Errorf("open status line %q", got)
	}
	if got := describeEgressMode(api.Egress{VM: "box"}); got != "box: egress restricted: only the allowlist" {
		t.Errorf("restricted mode %q", got)
	}
}

func TestDescribeTrust(t *testing.T) {
	until := time.Date(2036, 1, 2, 3, 4, 5, 0, time.UTC)
	r := api.Trust{VM: "box", Certificates: []api.TrustedCertificate{
		{Name: "corp", Subject: "CN=Corp Root", NotAfter: &until, SHA256: strings.Repeat("ab", 32), Authority: true},
		{Name: "srv", Subject: "CN=srv", SHA256: strings.Repeat("cd", 32)},
	}}
	if got := describeTrust(r); got != "corp  CN=Corp Root until 2036-01-02  sha256 abababababababab\nsrv  CN=srv  sha256 cdcdcdcdcdcdcdcd  (not a CA)\n" {
		t.Errorf("describeTrust = %q", got)
	}
	if got := describeTrust(api.Trust{VM: "box"}); got != "box: no trusted certificates\n" {
		t.Errorf("empty = %q", got)
	}
}

func TestPlainSecretsAreConfirmedOnAConsoleOnly(t *testing.T) {
	refusal := proto.Errorf(api.CodePlainSecrets, "VM box would hold raw ...")
	var sent []bool
	send := func(allow bool) error {
		sent = append(sent, allow)
		if allow {
			return nil
		}
		return refusal
	}
	var asked string
	yes := func(w string) string { asked = w; return " Y\r\n" }
	if err := confirmingPlainSecrets(false, true, yes, send); err != nil || !reflect.DeepEqual(sent, []bool{false, true}) || asked != refusal.Message {
		t.Errorf("console, yes: %v, sent %v, asked %q", err, sent, asked)
	}
	sent = nil
	if err := confirmingPlainSecrets(false, true, func(string) string { return "\n" }, send); err == nil || err.Error() != refusal.Message || len(sent) != 1 {
		t.Errorf("console, no: %v, sent %v", err, sent)
	}
	sent = nil
	if err := confirmingPlainSecrets(false, false, func(string) string { t.Error("asked without a console"); return "y" }, send); err == nil || len(sent) != 1 {
		t.Errorf("no console: %v, sent %v", err, sent)
	}
	sent = nil
	if err := confirmingPlainSecrets(true, false, nil, send); err != nil || !reflect.DeepEqual(sent, []bool{true}) {
		t.Errorf("--allow-plain-secrets: %v, sent %v", err, sent)
	}
	other := proto.Errorf(1, "VM box is not isolated")
	if err := confirmingPlainSecrets(false, true, yes, func(bool) error { return other }); err == nil || err.Error() != "VM box is not isolated" {
		t.Errorf("another error: %v", err)
	}
}
