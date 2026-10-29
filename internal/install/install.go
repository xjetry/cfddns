// Package install puts cfddns under the host's init system. Three init
// systems are supported; each is described by a System value and the
// install/uninstall sequences are shared. Configuration is never injected by
// the init system: every service definition starts the bare binary, which
// reads /etc/cfddns.env itself, so cfddns is the only parser of that file.
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cfddns/internal/config"
)

const ServiceName = "cfddns"

// Runner executes one host command; Exec is the production implementation.
type Runner func(name string, args ...string) error

// ExecTimeout bounds one init-system command. systemd itself gives a unit
// 90s to stop (DefaultTimeoutStopSec) and a restart is a stop plus a start,
// so the bound sits above that: it exists for a control command that hangs
// outright (a wedged init, a blocked D-Bus), not for a slow but working one.
const ExecTimeout = 3 * time.Minute

// Exec runs the command within ExecTimeout and folds its output into the
// error on failure.
func Exec(name string, args ...string) error {
	return runWithin(ExecTimeout, name, args...)
}

func runWithin(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Init commands fork helpers (rc-service → openrc-run.sh →
	// supervise-daemon) that inherit the output pipe, and the wait only ends
	// when every holder of that pipe is gone. The command therefore gets its
	// own process group, which the deadline kills as a whole, and WaitDelay
	// closes the pipe should a helper have escaped the group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("%s %s: timed out after %s: %s", name, strings.Join(args, " "), timeout, strings.TrimSpace(string(out)))
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// System describes how one init system hosts the service.
type System struct {
	Name        string
	BinPath     string
	EnvPath     string
	ServicePath string
	ServiceMode os.FileMode
	// LogHint is the command an operator runs to follow the service log.
	LogHint string
	service func(binPath string) string
	// activate runs after all files are written: register at boot and
	// (re)start. It must restart, not merely start: a plain start is a no-op
	// for an already running service, which would leave a reinstall running
	// the old binary with the old configuration.
	activate [][]string
	// deactivate runs before files are removed: stop, then unregister from
	// boot. A failure aborts the uninstall while the service definition is
	// still installed (see Uninstall).
	deactivate [][]string
	// cleanup runs after files are removed.
	cleanup [][]string
}

// Service renders the service definition for this system.
func (s *System) Service() string { return s.service(s.BinPath) }

var Systemd = &System{
	Name:        "systemd",
	BinPath:     "/usr/local/bin/cfddns",
	EnvPath:     config.DefaultEnvPath,
	ServicePath: "/etc/systemd/system/cfddns.service",
	ServiceMode: 0o644,
	LogHint:     "journalctl -u cfddns -f",
	service: func(bin string) string {
		return `[Unit]
Description=Cloudflare DDNS (cfddns)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# Configuration is read by cfddns itself from ` + config.DefaultEnvPath + `.
ExecStart=` + bin + `
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`
	},
	activate: [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", ServiceName},
		{"systemctl", "restart", ServiceName},
	},
	deactivate: [][]string{
		{"systemctl", "stop", ServiceName},
		{"systemctl", "disable", ServiceName},
	},
	cleanup: [][]string{{"systemctl", "daemon-reload"}},
}

var OpenRC = &System{
	Name:        "openrc",
	BinPath:     "/usr/local/bin/cfddns",
	EnvPath:     config.DefaultEnvPath,
	ServicePath: "/etc/init.d/cfddns",
	ServiceMode: 0o755,
	LogHint:     "tail -f /var/log/cfddns.log",
	service: func(bin string) string {
		return `#!/sbin/openrc-run
description="Cloudflare DDNS (cfddns)"
# Configuration is read by cfddns itself from ` + config.DefaultEnvPath + `.
supervisor=supervise-daemon
command=` + bin + `
respawn_delay=5
respawn_max=0
output_log=/var/log/cfddns.log
error_log=/var/log/cfddns.log

depend() {
	need net
	after firewall
}
`
	},
	activate: [][]string{
		{"rc-update", "add", ServiceName, "default"},
		{"rc-service", ServiceName, "restart"},
	},
	deactivate: [][]string{
		{"rc-service", ServiceName, "stop"},
		{"rc-update", "del", ServiceName, "default"},
	},
}

// Procd is OpenWrt. /usr/local/bin is not on OpenWrt's default PATH, so the
// binary goes to /usr/bin to keep "cfddns -once" and "-uninstall" callable.
var Procd = &System{
	Name:        "procd",
	BinPath:     "/usr/bin/cfddns",
	EnvPath:     config.DefaultEnvPath,
	ServicePath: "/etc/init.d/cfddns",
	ServiceMode: 0o755,
	LogHint:     "logread -e cfddns -f",
	service: func(bin string) string {
		return `#!/bin/sh /etc/rc.common
# Cloudflare DDNS (cfddns). Configuration is read by cfddns itself from ` + config.DefaultEnvPath + `.
USE_PROCD=1
START=95
STOP=10

start_service() {
	procd_open_instance
	procd_set_param command ` + bin + `
	procd_set_param respawn
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_close_instance
}
`
	},
	activate: [][]string{
		{"/etc/init.d/cfddns", "enable"},
		{"/etc/init.d/cfddns", "restart"},
	},
	deactivate: [][]string{
		{"/etc/init.d/cfddns", "stop"},
		{"/etc/init.d/cfddns", "disable"},
	},
}

var systems = map[string]*System{
	Systemd.Name: Systemd,
	OpenRC.Name:  OpenRC,
	Procd.Name:   Procd,
}

// Probe abstracts the host checks Detect relies on.
type Probe struct {
	Exists   func(path string) bool
	LookPath func(file string) (string, error)
}

func HostProbe() Probe {
	return Probe{
		Exists: func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		LookPath: exec.LookPath,
	}
}

func (p Probe) has(file string) bool {
	_, err := p.LookPath(file)
	return err == nil
}

// Detect picks the init system that booted this host. /run/systemd/system is
// systemd's own "booted with systemd" marker (sd_booted), checked first so a
// host that merely has openrc-run installed next to systemd is not misread.
func Detect(p Probe) (*System, error) {
	switch {
	case p.Exists("/run/systemd/system") && p.has("systemctl"):
		return Systemd, nil
	case p.Exists("/etc/openwrt_release") || p.Exists("/sbin/procd"):
		return Procd, nil
	case p.Exists("/sbin/openrc-run") || p.has("openrc-run"):
		return OpenRC, nil
	}
	return nil, errors.New("no supported init system found (systemd, OpenRC or procd); pass -init to force one")
}

// Select returns the named system, or detects one when name is empty.
func Select(name string, p Probe) (*System, error) {
	if name == "" {
		return Detect(p)
	}
	s, ok := systems[name]
	if !ok {
		return nil, fmt.Errorf("unknown -init %q (systemd, openrc, procd)", name)
	}
	return s, nil
}

// RequireRoot reports an error unless the process can write system paths.
func RequireRoot(action string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("root required for %s", action)
	}
	return nil
}

// Install copies the running binary, writes env + service, registers the
// service at boot and restarts it. The env file is rendered before anything
// is touched so a value outside the file grammar cannot leave a half install.
func Install(sys *System, c config.Config, selfPath string, run Runner) error {
	env, err := c.EnvFile()
	if err != nil {
		return fmt.Errorf("cannot persist to %s: %w", sys.EnvPath, err)
	}
	if err := copyFile(selfPath, sys.BinPath, 0o755); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	// The env file holds the API token: 0600 and root-owned on every
	// install, whatever a previous install or a hand edit left behind.
	if err := replaceFile(sys.EnvPath, 0o600, writeString(env)); err != nil {
		return fmt.Errorf("write env: %w", err)
	}
	if err := replaceFile(sys.ServicePath, sys.ServiceMode, writeString(sys.Service())); err != nil {
		return fmt.Errorf("write service: %w", err)
	}
	for _, cmd := range sys.activate {
		if err := run(cmd[0], cmd[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// Uninstall stops and unregisters the service, then removes service, env and
// binary. While the service definition is installed the init system knows
// the service, so a stop or disable that fails aborts before any file goes:
// removing the files under a service that could not be stopped would leave
// a process no init system manages, reported as "uninstalled". Once the
// definition is gone (an earlier uninstall that got that far) stop and
// disable can only answer "no such service", and the leftovers are removed
// regardless.
func Uninstall(sys *System, run Runner) error {
	_, err := os.Stat(sys.ServicePath)
	installed := err == nil
	for _, cmd := range sys.deactivate {
		if err := run(cmd[0], cmd[1:]...); err != nil && installed {
			return err
		}
	}
	for _, p := range []string{sys.ServicePath, sys.EnvPath, sys.BinPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	for _, cmd := range sys.cleanup {
		if err := run(cmd[0], cmd[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// copyFile writes src to dst with mode through replaceFile.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return replaceFile(dst, mode, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

func writeString(s string) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	}
}

// replaceFile writes dst atomically: content fills a temp file in the same
// directory, which is given mode and renamed over dst. The result is a new
// inode carrying mode and this process's ownership whatever dst was before,
// so a reinstall over a loosened or foreign-owned file never inherits it; a
// crash mid-write leaves the old file intact; and a running binary keeps its
// old inode until the init system restarts it.
func replaceFile(dst string, mode os.FileMode, content func(io.Writer) error) error {
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

	if err := content(tmp); err != nil {
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
