package config_test

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cfddns/internal/config"
)

func TestValidate_RequiresTokenAndAtLeastOneDomain(t *testing.T) {
	c := config.Config{Period: 5 * time.Minute}
	if err := c.ValidateRun(); err == nil {
		t.Fatal("expected error without token/domains")
	}
	c.Token = "tok"
	if err := c.ValidateRun(); err == nil {
		t.Fatal("expected error without domains")
	}
	c.DomainV4 = "home.example.com"
	if err := c.ValidateRun(); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidate_PortRequiresPasswd(t *testing.T) {
	c := config.Config{
		Token: "t", DomainV4: "a.example.com", Period: time.Minute, Port: 8080,
	}
	if err := c.ValidateRun(); err == nil {
		t.Fatal("expected passwd required with port")
	}
	c.Passwd = "secret"
	if err := c.ValidateRun(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_PasswdWithoutPort(t *testing.T) {
	c := config.Config{
		Token: "t", DomainV4: "a.example.com", Period: time.Minute, Passwd: "x",
	}
	if err := c.ValidateRun(); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidate_PeriodPositive(t *testing.T) {
	c := config.Config{Token: "t", DomainV4: "a.example.com", Period: 0}
	if err := c.ValidateRun(); err == nil {
		t.Fatal("expected period error")
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfddns.env")
	content := "CFDDNS_V4=home.example.com\nCFDDNS_TOKEN=abc\nCFDDNS_PERIOD=2m\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := config.ParseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if m["CFDDNS_V4"] != "home.example.com" || m["CFDDNS_TOKEN"] != "abc" {
		t.Fatalf("%v", m)
	}
}

func TestApplyEnvMap_PeriodAndPort(t *testing.T) {
	c := config.Config{Period: config.DefaultPeriod}
	if err := c.ApplyEnvMap(map[string]string{
		"CFDDNS_PERIOD": "1m",
		"CFDDNS_PORT":   "8080",
	}); err != nil {
		t.Fatal(err)
	}
	if c.Period != time.Minute {
		t.Fatalf("period: got %v want 1m", c.Period)
	}
	if c.Port != 8080 {
		t.Fatalf("port: got %d want 8080", c.Port)
	}
}

func TestApplyEnvMap_PeriodInvalid(t *testing.T) {
	c := config.Config{Period: config.DefaultPeriod}
	if err := c.ApplyEnvMap(map[string]string{"CFDDNS_PERIOD": "not-a-duration"}); err == nil {
		t.Fatal("expected CFDDNS_PERIOD parse error")
	}
}

func TestApplyEnvMap_PortInvalid(t *testing.T) {
	c := config.Config{}
	if err := c.ApplyEnvMap(map[string]string{"CFDDNS_PORT": "xyz"}); err == nil {
		t.Fatal("expected CFDDNS_PORT parse error")
	}
}

func TestModeConflict_InstallUninstall(t *testing.T) {
	c := config.Config{Install: true, Uninstall: true}
	if err := c.ModeConflict(); err == nil {
		t.Fatal("expected install+uninstall conflict")
	}
}

func TestModeConflict_OnceInstall(t *testing.T) {
	c := config.Config{Once: true, Install: true}
	if err := c.ModeConflict(); err == nil {
		t.Fatal("expected once+install conflict")
	}
}

func TestValidate_InvalidDomain(t *testing.T) {
	c := config.Config{Token: "t", DomainV4: "nodot", Period: time.Minute}
	if err := c.ValidateRun(); err == nil {
		t.Fatal("expected invalid domain error")
	}
	c.DomainV4 = "has space.example.com"
	if err := c.ValidateRun(); err == nil {
		t.Fatal("expected invalid domain error for space")
	}
}

func TestLoad_FlagsOverrideDefaults(t *testing.T) {
	// Avoid inheriting ambient CFDDNS_* from the process environment.
	for _, k := range []string{"CFDDNS_V4", "CFDDNS_V6", "CFDDNS_TOKEN", "CFDDNS_PERIOD", "CFDDNS_PORT", "CFDDNS_PASSWD"} {
		t.Setenv(k, "")
	}
	c, err := config.Load([]string{"-4", "home.example.com", "-token", "x", "-period", "1m"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DomainV4 != "home.example.com" {
		t.Fatalf("domain: got %q", c.DomainV4)
	}
	if c.Token != "x" {
		t.Fatalf("token: got %q", c.Token)
	}
	if c.Period != time.Minute {
		t.Fatalf("period: got %v want 1m", c.Period)
	}
}

func TestUsage_ContainsFlagsAndJournalctl(t *testing.T) {
	var b strings.Builder
	config.Usage(&b)
	out := b.String()
	for _, want := range []string{
		"-4", "-6", "-token", "-period", "-port", "-passwd",
		"-once", "-install", "-uninstall",
		"journalctl -u cfddns -f",
		"journalctl -u cfddns -n 100",
		"[required]",
		"[optional",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Usage missing %q", want)
		}
	}
}

func TestLoad_HelpReturnsErrHelp(t *testing.T) {
	_, err := config.Load([]string{"-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("got %v, want flag.ErrHelp", err)
	}
}
