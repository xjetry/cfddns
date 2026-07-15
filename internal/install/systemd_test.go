package install_test

import (
	"strings"
	"testing"
	"time"

	"cfddns/internal/config"
	"cfddns/internal/install"
)

func TestEnvFileContent_OmitsEmptyOptional(t *testing.T) {
	c := config.Config{
		DomainV4: "home.example.com",
		Token:    "tok123",
		Period:   5 * time.Minute,
	}
	got := install.EnvFileContent(c)

	if !strings.Contains(got, "CFDDNS_V4=home.example.com") {
		t.Errorf("missing V4: %q", got)
	}
	if !strings.Contains(got, "CFDDNS_TOKEN=tok123") {
		t.Errorf("missing token: %q", got)
	}
	if !strings.Contains(got, "CFDDNS_PERIOD=5m0s") {
		t.Errorf("missing period: %q", got)
	}
	if strings.Contains(got, "CFDDNS_V6=") {
		t.Errorf("should omit empty V6: %q", got)
	}
	if strings.Contains(got, "CFDDNS_PORT=") {
		t.Errorf("should omit PORT when 0: %q", got)
	}
	if strings.Contains(got, "CFDDNS_PASSWD=") {
		t.Errorf("should omit empty PASSWD: %q", got)
	}
	if strings.Contains(got, `"`) {
		t.Errorf("should not quote values: %q", got)
	}
}

func TestEnvFileContent_IncludesPortAndPasswdAndV6(t *testing.T) {
	c := config.Config{
		DomainV4: "a.example.com",
		DomainV6: "aaaa.example.com",
		Token:    "t",
		Period:   time.Minute,
		Port:     8080,
		Passwd:   "secret",
	}
	got := install.EnvFileContent(c)

	for _, want := range []string{
		"CFDDNS_V4=a.example.com",
		"CFDDNS_V6=aaaa.example.com",
		"CFDDNS_TOKEN=t",
		"CFDDNS_PERIOD=1m0s",
		"CFDDNS_PORT=8080",
		"CFDDNS_PASSWD=secret",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestUnitContent_Paths(t *testing.T) {
	got := install.UnitContent()
	if !strings.Contains(got, "EnvironmentFile=/etc/cfddns.env") {
		t.Errorf("missing EnvironmentFile: %q", got)
	}
	if !strings.Contains(got, "ExecStart=/usr/local/bin/cfddns") {
		t.Errorf("missing ExecStart: %q", got)
	}
	if !strings.Contains(got, "WantedBy=multi-user.target") {
		t.Errorf("missing WantedBy: %q", got)
	}
}

func TestInstall_NonRootReturnsError(t *testing.T) {
	// Unit tests run as non-root; Install must fail early without touching system paths.
	err := install.Install(config.Config{}, "/nonexistent/cfddns")
	if err == nil {
		t.Fatal("expected error when not root")
	}
	if !strings.Contains(err.Error(), "root required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUninstall_NonRootReturnsError(t *testing.T) {
	err := install.Uninstall()
	if err == nil {
		t.Fatal("expected error when not root")
	}
	if !strings.Contains(err.Error(), "root required") {
		t.Errorf("unexpected error: %v", err)
	}
}
