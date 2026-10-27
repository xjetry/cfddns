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

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"CFDDNS_V4", "CFDDNS_V6", "CFDDNS_V6_IFACE", "CFDDNS_TOKEN", "CFDDNS_PERIOD", "CFDDNS_PORT", "CFDDNS_PASSWD"} {
		t.Setenv(k, "")
	}
}

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfddns.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

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

func TestValidate_PeriodLowerBound(t *testing.T) {
	c := config.Config{Token: "t", DomainV4: "a.example.com"}
	for _, p := range []time.Duration{0, -time.Second, config.MinPeriod - time.Millisecond} {
		c.Period = p
		if err := c.ValidateRun(); err == nil || !strings.Contains(err.Error(), "-period") {
			t.Errorf("period %v: err=%v, want -period error", p, err)
		}
	}
	c.Period = config.MinPeriod
	if err := c.ValidateRun(); err != nil {
		t.Fatalf("MinPeriod should be accepted: %v", err)
	}
}

func TestValidate_StrictHostname(t *testing.T) {
	c := config.Config{Token: "t", Period: time.Minute}
	for _, bad := range []string{
		"nodot",
		"has space.example.com",
		"under_score.example.com",
		"-lead.example.com",
		"trail-.example.com",
		"Upper.example.com", // validation expects normalized input
		"a..example.com",
		"trailingdot.example.com.",
		strings.Repeat("a", 64) + ".example.com",
		"ünïcode.example.com",
		"foo.bar/baz.com",
	} {
		c.DomainV4 = bad
		if err := c.ValidateRun(); err == nil {
			t.Errorf("%q accepted, want error", bad)
		}
	}
	for _, ok := range []string{"home.example.com", "a-b.c-d.example.co.uk", "xn--80ak6aa92e.com", "1.2.3.example.com"} {
		c.DomainV4 = ok
		if err := c.ValidateRun(); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
}

func TestValidate_V6IfaceRequiresV6(t *testing.T) {
	c := config.Config{Token: "t", DomainV4: "a.example.com", Period: time.Minute, V6Iface: "eth0"}
	if err := c.ValidateRun(); err == nil {
		t.Fatal("expected -6-iface requires -6")
	}
	c.DomainV6 = "a.example.com"
	if err := c.ValidateRun(); err != nil {
		t.Fatal(err)
	}
	c.V6Iface = "bad name"
	if err := c.ValidateRun(); err == nil {
		t.Fatal("expected invalid interface name")
	}
}

func TestNormalizeDomain(t *testing.T) {
	for in, want := range map[string]string{
		"  Home.Example.COM. ": "home.example.com",
		"home.example.com":     "home.example.com",
		"":                     "",
	} {
		if got := config.NormalizeDomain(in); got != want {
			t.Errorf("NormalizeDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	path := writeEnv(t, "CFDDNS_V4=home.example.com\nCFDDNS_TOKEN=abc\nCFDDNS_PERIOD=2m\n")
	m, err := config.ParseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if m["CFDDNS_V4"] != "home.example.com" || m["CFDDNS_TOKEN"] != "abc" {
		t.Fatalf("%v", m)
	}
}

func TestApplyEnvMap_PeriodPortIface(t *testing.T) {
	c := config.Config{Period: config.DefaultPeriod}
	if err := c.ApplyEnvMap(map[string]string{
		"CFDDNS_PERIOD":   "1m",
		"CFDDNS_PORT":     "8080",
		"CFDDNS_V6_IFACE": "eth0",
	}); err != nil {
		t.Fatal(err)
	}
	if c.Period != time.Minute {
		t.Fatalf("period: got %v want 1m", c.Period)
	}
	if c.Port != 8080 {
		t.Fatalf("port: got %d want 8080", c.Port)
	}
	if c.V6Iface != "eth0" {
		t.Fatalf("iface: got %q", c.V6Iface)
	}
}

func TestApplyEnvMap_Invalid(t *testing.T) {
	c := config.Config{Period: config.DefaultPeriod}
	if err := c.ApplyEnvMap(map[string]string{"CFDDNS_PERIOD": "not-a-duration"}); err == nil {
		t.Fatal("expected CFDDNS_PERIOD parse error")
	}
	if err := c.ApplyEnvMap(map[string]string{"CFDDNS_PORT": "xyz"}); err == nil {
		t.Fatal("expected CFDDNS_PORT parse error")
	}
}

func TestModeConflict(t *testing.T) {
	if err := (config.Config{Install: true, Uninstall: true}).ModeConflict(); err == nil {
		t.Fatal("expected install+uninstall conflict")
	}
	if err := (config.Config{Once: true, Install: true}).ModeConflict(); err == nil {
		t.Fatal("expected once+install conflict")
	}
	if err := (config.Config{Init: "systemd"}).ModeConflict(); err == nil {
		t.Fatal("expected -init without install/uninstall to be rejected")
	}
	if err := (config.Config{Init: "systemd", Install: true}).ModeConflict(); err != nil {
		t.Fatal(err)
	}
}

func TestModeConflict_StatusAndInspect(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Config
		wantErr bool
	}{
		{name: "inspect alone", cfg: config.Config{Inspect: true}},
		{name: "inspect with once", cfg: config.Config{Inspect: true, Once: true}, wantErr: true},
		{name: "inspect with install", cfg: config.Config{Inspect: true, Install: true}, wantErr: true},
		{name: "inspect with uninstall", cfg: config.Config{Inspect: true, Uninstall: true}, wantErr: true},
		{name: "status with daemon", cfg: config.Config{StatusFile: "/var/run/cfddns.status"}},
		{name: "status with once", cfg: config.Config{StatusFile: "/var/run/cfddns.status", Once: true}},
		{name: "status with inspect", cfg: config.Config{StatusFile: "/var/run/cfddns.status", Inspect: true}, wantErr: true},
		{name: "status with install", cfg: config.Config{StatusFile: "/var/run/cfddns.status", Install: true}, wantErr: true},
		{name: "status with uninstall", cfg: config.Config{StatusFile: "/var/run/cfddns.status", Uninstall: true}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.ModeConflict()
			if (err != nil) != tt.wantErr {
				t.Fatalf("ModeConflict() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_StatusFileFlag(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "missing.env")
	c, err := config.LoadFrom(path, []string{"-status-file", "/var/run/cfddns.status"})
	if err != nil {
		t.Fatal(err)
	}
	if c.StatusFile != "/var/run/cfddns.status" {
		t.Fatalf("StatusFile = %q, want /var/run/cfddns.status", c.StatusFile)
	}
}

func TestLoad_InspectFlag(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "missing.env")
	c, err := config.LoadFrom(path, []string{"-inspect"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Inspect {
		t.Fatal("Inspect = false, want true")
	}
}

func TestLoad_FlagsOverrideEnvFileAndOSEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("CFDDNS_TOKEN", "from-os-env")
	t.Setenv("CFDDNS_PORT", "9090")
	path := writeEnv(t, "CFDDNS_V4=file.example.com\nCFDDNS_TOKEN=from-file\nCFDDNS_PERIOD=2m\nCFDDNS_PORT=8080\nCFDDNS_PASSWD=filepw\n")

	c, err := config.LoadFrom(path, []string{"-period", "1m"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DomainV4 != "file.example.com" {
		t.Errorf("env file layer lost: V4=%q", c.DomainV4)
	}
	if c.Token != "from-os-env" {
		t.Errorf("OS env must beat env file: token=%q", c.Token)
	}
	if c.Port != 9090 {
		t.Errorf("OS env must beat env file: port=%d", c.Port)
	}
	if c.Period != time.Minute {
		t.Errorf("flag must beat env file: period=%v", c.Period)
	}
	if c.Passwd != "filepw" {
		t.Errorf("untouched key must survive from file: passwd=%q", c.Passwd)
	}
}

func TestLoad_ExplicitEmptyFlagClearsEnvValue(t *testing.T) {
	clearEnv(t)
	path := writeEnv(t, "CFDDNS_V4=a.example.com\nCFDDNS_V6=b.example.com\nCFDDNS_TOKEN=t\n")
	c, err := config.LoadFrom(path, []string{"-6", ""})
	if err != nil {
		t.Fatal(err)
	}
	if c.DomainV6 != "" {
		t.Fatalf("explicit -6 \"\" must clear: %q", c.DomainV6)
	}
	if c.DomainV4 != "a.example.com" {
		t.Fatalf("V4 lost: %q", c.DomainV4)
	}
}

func TestLoad_NormalizesDomains(t *testing.T) {
	clearEnv(t)
	c, err := config.LoadFrom(filepath.Join(t.TempDir(), "missing.env"), []string{"-4", "Home.Example.COM.", "-6", " V6.Example.com ", "-token", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DomainV4 != "home.example.com" || c.DomainV6 != "v6.example.com" {
		t.Fatalf("not normalized: %q %q", c.DomainV4, c.DomainV6)
	}
	if err := c.ValidateRun(); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_InstallIgnoresEnvFile(t *testing.T) {
	clearEnv(t)
	path := writeEnv(t, "CFDDNS_V4=old.example.com\nCFDDNS_V6=old6.example.com\nCFDDNS_TOKEN=old\nCFDDNS_PORT=8080\nCFDDNS_PASSWD=oldpw\n")
	c, err := config.LoadFrom(path, []string{"-install", "-4", "new.example.com", "-token", "new"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DomainV6 != "" || c.Port != 0 || c.Passwd != "" {
		t.Fatalf("install must not inherit old env file: %+v", c)
	}
	if c.DomainV4 != "new.example.com" || c.Token != "new" {
		t.Fatalf("flags lost: %+v", c)
	}
	if err := c.ValidateInstall(); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_UninstallIgnoresCorruptEnvFile(t *testing.T) {
	clearEnv(t)
	path := writeEnv(t, "CFDDNS_PASSWD=\"quoted is not allowed\"\n")
	if _, err := config.LoadFrom(path, nil); err == nil {
		t.Fatal("run mode must reject a corrupt env file")
	}
	c, err := config.LoadFrom(path, []string{"-uninstall"})
	if err != nil {
		t.Fatalf("uninstall must not read the env file: %v", err)
	}
	if !c.Uninstall {
		t.Fatal("Uninstall not set")
	}
}

func TestLoad_MissingOrUnreadableEnvFileIsSkipped(t *testing.T) {
	clearEnv(t)
	c, err := config.LoadFrom(filepath.Join(t.TempDir(), "missing.env"), []string{"-4", "a.example.com", "-token", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Period != config.DefaultPeriod {
		t.Fatalf("period default lost: %v", c.Period)
	}
}

func TestEnvFile_OmitsEmptyOptional(t *testing.T) {
	c := config.Config{
		DomainV4: "home.example.com",
		Token:    "tok123",
		Period:   5 * time.Minute,
	}
	got, err := c.EnvFile()
	if err != nil {
		t.Fatal(err)
	}
	want := "CFDDNS_V4=home.example.com\nCFDDNS_TOKEN=tok123\nCFDDNS_PERIOD=5m0s\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEnvFile_IncludesAllKeysInOrder(t *testing.T) {
	c := config.Config{
		DomainV4: "a.example.com",
		DomainV6: "aaaa.example.com",
		V6Iface:  "eth0",
		Token:    "t",
		Period:   time.Minute,
		Port:     8080,
		Passwd:   "secret",
	}
	got, err := c.EnvFile()
	if err != nil {
		t.Fatal(err)
	}
	want := "CFDDNS_V4=a.example.com\nCFDDNS_V6=aaaa.example.com\nCFDDNS_V6_IFACE=eth0\nCFDDNS_TOKEN=t\nCFDDNS_PERIOD=1m0s\nCFDDNS_PORT=8080\nCFDDNS_PASSWD=secret\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEnvPairs_DoesNotPersistStatusFile(t *testing.T) {
	const statusPath = "/var/run/cfddns.status"
	c := config.Config{Period: config.DefaultPeriod, StatusFile: statusPath}
	for _, pair := range c.EnvPairs() {
		if strings.Contains(strings.ToLower(pair.Key), "status") || pair.Value == statusPath {
			t.Errorf("StatusFile persisted as %+v", pair)
		}
	}
}

func TestEnvFile_RoundTripsThroughParseEnvFile(t *testing.T) {
	c := config.Config{DomainV4: "a.example.com", Token: "t", Period: time.Minute, Port: 8080, Passwd: "p@ss!w0rd$"}
	body, err := c.EnvFile()
	if err != nil {
		t.Fatal(err)
	}
	m, err := config.ParseEnvFile(writeEnv(t, body))
	if err != nil {
		t.Fatal(err)
	}
	var back config.Config
	if err := back.ApplyEnvMap(m); err != nil {
		t.Fatal(err)
	}
	if back != (config.Config{DomainV4: "a.example.com", Token: "t", Period: time.Minute, Port: 8080, Passwd: "p@ss!w0rd$"}) {
		t.Fatalf("round trip mismatch: %+v", back)
	}
}

func TestValidateInstall_RejectsValueOutsideEnvGrammar(t *testing.T) {
	c := config.Config{DomainV4: "a.example.com", Token: "t", Period: time.Minute, Port: 8080, Passwd: "has space"}
	if err := c.ValidateRun(); err != nil {
		t.Fatalf("run mode may use any passwd: %v", err)
	}
	err := c.ValidateInstall()
	if err == nil || !strings.Contains(err.Error(), "CFDDNS_PASSWD") {
		t.Fatalf("ValidateInstall = %v, want env grammar error naming CFDDNS_PASSWD", err)
	}
}

func TestUsage_ContainsFlagsAndLogs(t *testing.T) {
	var b strings.Builder
	config.Usage(&b)
	out := b.String()
	for _, want := range []string{
		"-4", "-6", "-6-iface", "-token", "-period", "-port", "-passwd",
		"-once", "-inspect", "-status-file", "-install", "-uninstall", "-init",
		"journalctl -u cfddns -f",
		"journalctl -u cfddns -n 100",
		"/var/log/cfddns.log",
		"logread -e cfddns",
		"[required]",
		"[optional",
		"CFDDNS_V6_IFACE",
		"Config merge for daemon/-once/-inspect:",
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
