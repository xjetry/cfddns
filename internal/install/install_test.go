package install_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cfddns/internal/config"
	"cfddns/internal/install"
)

func TestService_SystemdPathsAndNoEnvironmentFile(t *testing.T) {
	got := install.Systemd.Service()
	for _, want := range []string{"ExecStart=/usr/local/bin/cfddns\n", "WantedBy=multi-user.target", "Restart=on-failure"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	// cfddns must stay the only parser of /etc/cfddns.env.
	if strings.Contains(got, "EnvironmentFile") {
		t.Errorf("unit must not inject the env file: %q", got)
	}
}

func TestService_OpenRC(t *testing.T) {
	got := install.OpenRC.Service()
	for _, want := range []string{"#!/sbin/openrc-run\n", "supervisor=supervise-daemon", "command=/usr/local/bin/cfddns\n", "output_log=/var/log/cfddns.log", "need net"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if install.OpenRC.ServiceMode != 0o755 {
		t.Errorf("init script must be executable, mode %o", install.OpenRC.ServiceMode)
	}
}

func TestService_Procd(t *testing.T) {
	got := install.Procd.Service()
	for _, want := range []string{"#!/bin/sh /etc/rc.common\n", "USE_PROCD=1", "procd_set_param command /usr/bin/cfddns\n", "procd_set_param respawn", "procd_set_param stdout 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if install.Procd.BinPath != "/usr/bin/cfddns" {
		t.Errorf("OpenWrt PATH lacks /usr/local/bin; BinPath=%q", install.Procd.BinPath)
	}
	if install.Procd.ServiceMode != 0o755 {
		t.Errorf("init script must be executable, mode %o", install.Procd.ServiceMode)
	}
}

func probe(paths []string, bins []string) install.Probe {
	return install.Probe{
		Exists: func(p string) bool {
			for _, x := range paths {
				if x == p {
					return true
				}
			}
			return false
		},
		LookPath: func(f string) (string, error) {
			for _, x := range bins {
				if x == f {
					return "/bin/" + f, nil
				}
			}
			return "", errors.New("not found")
		},
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		bins  []string
		want  string
	}{
		{"systemd booted", []string{"/run/systemd/system"}, []string{"systemctl"}, "systemd"},
		{"systemd beats openrc binary", []string{"/run/systemd/system", "/sbin/openrc-run"}, []string{"systemctl", "openrc-run"}, "systemd"},
		{"systemctl present but not booted with systemd", []string{"/sbin/openrc-run"}, []string{"systemctl"}, "openrc"},
		{"openwrt", []string{"/etc/openwrt_release", "/sbin/procd"}, nil, "procd"},
		{"alpine", []string{"/sbin/openrc-run"}, []string{"rc-service"}, "openrc"},
		{"nothing", nil, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sys, err := install.Detect(probe(c.paths, c.bins))
			if c.want == "" {
				if err == nil {
					t.Fatalf("expected error, got %s", sys.Name)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sys.Name != c.want {
				t.Fatalf("got %s want %s", sys.Name, c.want)
			}
		})
	}
}

func TestSelect(t *testing.T) {
	p := probe([]string{"/etc/openwrt_release"}, nil)
	if sys, err := install.Select("", p); err != nil || sys.Name != "procd" {
		t.Fatalf("auto: %v %v", sys, err)
	}
	if sys, err := install.Select("openrc", p); err != nil || sys.Name != "openrc" {
		t.Fatalf("forced: %v %v", sys, err)
	}
	if _, err := install.Select("launchd", p); err == nil {
		t.Fatal("unknown init must be rejected")
	}
}

type recorder struct {
	calls []string
	fail  map[string]error
}

func (r *recorder) run(name string, args ...string) error {
	line := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, line)
	if err, ok := r.fail[line]; ok {
		return err
	}
	return nil
}

// sandbox returns a copy of sys with every path under a temp dir.
func sandbox(t *testing.T, sys *install.System) (*install.System, string) {
	t.Helper()
	root := t.TempDir()
	cp := *sys
	cp.BinPath = filepath.Join(root, sys.BinPath)
	cp.EnvPath = filepath.Join(root, sys.EnvPath)
	cp.ServicePath = filepath.Join(root, sys.ServicePath)
	for _, p := range []string{cp.EnvPath, cp.ServicePath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	self := filepath.Join(root, "self")
	if err := os.WriteFile(self, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &cp, self
}

func TestInstall_SequenceAndFiles(t *testing.T) {
	for _, sys := range []*install.System{install.Systemd, install.OpenRC, install.Procd} {
		t.Run(sys.Name, func(t *testing.T) {
			sb, self := sandbox(t, sys)
			c := config.Config{DomainV4: "home.example.com", Token: "tok", Period: time.Minute, Port: 8080, Passwd: "secret"}
			rec := &recorder{}
			if err := install.Install(sb, c, self, rec.run); err != nil {
				t.Fatal(err)
			}

			bin, err := os.Stat(sb.BinPath)
			if err != nil || bin.Mode().Perm() != 0o755 {
				t.Fatalf("binary: %v mode=%v", err, bin.Mode())
			}
			env, err := os.Stat(sb.EnvPath)
			if err != nil || env.Mode().Perm() != 0o600 {
				t.Fatalf("env: %v mode=%v", err, env.Mode())
			}
			want, _ := c.EnvFile()
			if got, _ := os.ReadFile(sb.EnvPath); string(got) != want {
				t.Fatalf("env content %q want %q", got, want)
			}
			svc, err := os.Stat(sb.ServicePath)
			if err != nil || svc.Mode().Perm() != sys.ServiceMode {
				t.Fatalf("service: %v mode=%v want %v", err, svc.Mode(), sys.ServiceMode)
			}
			if got, _ := os.ReadFile(sb.ServicePath); string(got) != sb.Service() {
				t.Fatalf("service content mismatch")
			}

			var wantCalls []string
			switch sys.Name {
			case "systemd":
				wantCalls = []string{"systemctl daemon-reload", "systemctl enable cfddns", "systemctl restart cfddns"}
			case "openrc":
				wantCalls = []string{"rc-update add cfddns default", "rc-service cfddns restart"}
			case "procd":
				wantCalls = []string{"/etc/init.d/cfddns enable", "/etc/init.d/cfddns restart"}
			}
			if strings.Join(rec.calls, "\n") != strings.Join(wantCalls, "\n") {
				t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(rec.calls, "\n"), strings.Join(wantCalls, "\n"))
			}
		})
	}
}

func TestInstall_ReinstallRestartsAndKeepsScriptExecutable(t *testing.T) {
	sb, self := sandbox(t, install.OpenRC)
	c := config.Config{DomainV4: "home.example.com", Token: "tok", Period: time.Minute}
	if err := install.Install(sb, c, self, (&recorder{}).run); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sb.ServicePath, 0o644); err != nil {
		t.Fatal(err)
	}
	c.DomainV4 = "new.example.com"
	rec := &recorder{}
	if err := install.Install(sb, c, self, rec.run); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(sb.EnvPath); !strings.Contains(string(got), "new.example.com") {
		t.Fatalf("reinstall must replace env: %q", got)
	}
	if st, _ := os.Stat(sb.ServicePath); st.Mode().Perm() != 0o755 {
		t.Fatalf("reinstall must restore script mode, got %v", st.Mode())
	}
	if len(rec.calls) == 0 || !strings.HasSuffix(rec.calls[len(rec.calls)-1], "restart") {
		t.Fatalf("reinstall must end with a restart: %v", rec.calls)
	}
}

func TestInstall_RejectsBadEnvBeforeTouchingSystem(t *testing.T) {
	sb, self := sandbox(t, install.Systemd)
	c := config.Config{DomainV4: "home.example.com", Token: "tok", Period: time.Minute, Port: 8080, Passwd: "has space"}
	rec := &recorder{}
	err := install.Install(sb, c, self, rec.run)
	if err == nil || !strings.Contains(err.Error(), "CFDDNS_PASSWD") {
		t.Fatalf("err = %v", err)
	}
	for _, p := range []string{sb.BinPath, sb.EnvPath, sb.ServicePath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s must not exist after a rejected install", p)
		}
	}
	if len(rec.calls) != 0 {
		t.Errorf("no commands must run: %v", rec.calls)
	}
}

func TestInstall_ActivateFailurePropagates(t *testing.T) {
	sb, self := sandbox(t, install.Systemd)
	c := config.Config{DomainV4: "home.example.com", Token: "tok", Period: time.Minute}
	rec := &recorder{fail: map[string]error{"systemctl restart cfddns": errors.New("unit failed")}}
	err := install.Install(sb, c, self, rec.run)
	if err == nil || !strings.Contains(err.Error(), "unit failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestUninstall_SequenceAndRemoval(t *testing.T) {
	sb, self := sandbox(t, install.Systemd)
	c := config.Config{DomainV4: "home.example.com", Token: "tok", Period: time.Minute}
	if err := install.Install(sb, c, self, (&recorder{}).run); err != nil {
		t.Fatal(err)
	}
	// stop failing (service not running) must not abort the uninstall.
	rec := &recorder{fail: map[string]error{"systemctl stop cfddns": errors.New("not loaded")}}
	if err := install.Uninstall(sb, rec.run); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{sb.BinPath, sb.EnvPath, sb.ServicePath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still present", p)
		}
	}
	want := []string{"systemctl stop cfddns", "systemctl disable cfddns", "systemctl daemon-reload"}
	if strings.Join(rec.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls %v want %v", rec.calls, want)
	}
	// Second uninstall with nothing left is still clean.
	if err := install.Uninstall(sb, (&recorder{}).run); err != nil {
		t.Fatal(err)
	}
}

func TestRequireRoot_NonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	err := install.RequireRoot("install")
	if err == nil || !strings.Contains(err.Error(), "root required") {
		t.Fatalf("err = %v", err)
	}
}
