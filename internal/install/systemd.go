package install

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"cfddns/internal/config"
)

const (
	BinPath     = "/usr/local/bin/cfddns"
	EnvPath     = "/etc/cfddns.env"
	UnitPath    = "/etc/systemd/system/cfddns.service"
	ServiceName = "cfddns"
)

// EnvFileContent builds /etc/cfddns.env body. Only non-empty optional keys are
// included; Period is always written via Duration.String(). Values are unquoted.
func EnvFileContent(c config.Config) string {
	var b strings.Builder
	if c.DomainV4 != "" {
		fmt.Fprintf(&b, "CFDDNS_V4=%s\n", c.DomainV4)
	}
	if c.DomainV6 != "" {
		fmt.Fprintf(&b, "CFDDNS_V6=%s\n", c.DomainV6)
	}
	if c.Token != "" {
		fmt.Fprintf(&b, "CFDDNS_TOKEN=%s\n", c.Token)
	}
	fmt.Fprintf(&b, "CFDDNS_PERIOD=%s\n", c.Period.String())
	if c.Port > 0 {
		fmt.Fprintf(&b, "CFDDNS_PORT=%s\n", strconv.Itoa(c.Port))
	}
	if c.Passwd != "" {
		fmt.Fprintf(&b, "CFDDNS_PASSWD=%s\n", c.Passwd)
	}
	return b.String()
}

// UnitContent returns the fixed systemd unit for cfddns.
func UnitContent() string {
	return `[Unit]
Description=Cloudflare DDNS (cfddns)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/cfddns.env
ExecStart=/usr/local/bin/cfddns
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`
}

func requireRoot(action string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("root required for %s", action)
	}
	return nil
}

// Install copies the running binary, writes env + unit, and enables the service.
func Install(c config.Config, selfPath string) error {
	if err := requireRoot("install"); err != nil {
		return err
	}
	if err := copyFile(selfPath, BinPath, 0o755); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	if err := os.WriteFile(EnvPath, []byte(EnvFileContent(c)), 0o600); err != nil {
		return fmt.Errorf("write env: %w", err)
	}
	if err := os.WriteFile(UnitPath, []byte(UnitContent()), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", ServiceName); err != nil {
		return err
	}
	return nil
}

// Uninstall stops the service and removes unit, env, and binary.
func Uninstall() error {
	if err := requireRoot("uninstall"); err != nil {
		return err
	}
	_ = runSystemctl("stop", ServiceName)
	_ = runSystemctl("disable", ServiceName)
	for _, p := range []string{UnitPath, EnvPath, BinPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	return nil
}

func runSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// copyFile writes src to dst with mode via a temp file in the same directory
// when possible, so the binary replace is atomic on the same filesystem.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".cfddns-install-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}
