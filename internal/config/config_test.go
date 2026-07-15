package config_test

import (
	"os"
	"path/filepath"
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
