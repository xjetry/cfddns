package config

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultPeriod = 5 * time.Minute
const DefaultEnvPath = "/etc/cfddns.env"

type Config struct {
	DomainV4  string
	DomainV6  string
	Token     string
	Period    time.Duration
	Port      int // 0 = HTTP disabled
	Passwd    string
	Once      bool
	Install   bool
	Uninstall bool
}

func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, sc.Err()
}

// ApplyEnvMap sets fields from CFDDNS_* keys (only non-empty keys).
func (c *Config) ApplyEnvMap(m map[string]string) error {
	if v := m["CFDDNS_V4"]; v != "" {
		c.DomainV4 = v
	}
	if v := m["CFDDNS_V6"]; v != "" {
		c.DomainV6 = v
	}
	if v := m["CFDDNS_TOKEN"]; v != "" {
		c.Token = v
	}
	if v := m["CFDDNS_PERIOD"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("CFDDNS_PERIOD: %w", err)
		}
		c.Period = d
	}
	if v := m["CFDDNS_PORT"]; v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("CFDDNS_PORT: %w", err)
		}
		c.Port = p
	}
	if v := m["CFDDNS_PASSWD"]; v != "" {
		c.Passwd = v
	}
	return nil
}

func (c *Config) ApplyOSEnv() error {
	m := map[string]string{}
	for _, k := range []string{"CFDDNS_V4", "CFDDNS_V6", "CFDDNS_TOKEN", "CFDDNS_PERIOD", "CFDDNS_PORT", "CFDDNS_PASSWD"} {
		if v := os.Getenv(k); v != "" {
			m[k] = v
		}
	}
	return c.ApplyEnvMap(m)
}

// Load merges defaults, optional /etc/cfddns.env, OS env, then CLI flags.
// Higher layers win: defaults → env file → OS env → flags.
func Load(args []string) (Config, error) {
	c := Config{Period: DefaultPeriod}
	// Load env file only when readable. Missing or permission-denied (e.g. root
	// 0600 while non-root uses CLI flags) is skipped; corrupt content still fails.
	if m, err := ParseEnvFile(DefaultEnvPath); err == nil {
		if err := c.ApplyEnvMap(m); err != nil {
			return c, err
		}
	} else if err != nil && !os.IsNotExist(err) && !os.IsPermission(err) {
		return c, fmt.Errorf("%s: %w", DefaultEnvPath, err)
	}
	if err := c.ApplyOSEnv(); err != nil {
		return c, err
	}

	fs := flag.NewFlagSet("cfddns", flag.ContinueOnError)
	var periodStr string
	fs.StringVar(&c.DomainV4, "4", c.DomainV4, "IPv4 FQDN (A record)")
	fs.StringVar(&c.DomainV6, "6", c.DomainV6, "IPv6 FQDN (AAAA record)")
	fs.StringVar(&c.Token, "token", c.Token, "Cloudflare API token")
	fs.StringVar(&periodStr, "period", c.Period.String(), "update period")
	fs.IntVar(&c.Port, "port", c.Port, "HTTP port (0=disabled)")
	fs.StringVar(&c.Passwd, "passwd", c.Passwd, "HTTP Bearer password")
	fs.BoolVar(&c.Once, "once", false, "run one update and exit")
	fs.BoolVar(&c.Install, "install", false, "install systemd service")
	fs.BoolVar(&c.Uninstall, "uninstall", false, "uninstall systemd service")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if periodStr != "" {
		d, err := time.ParseDuration(periodStr)
		if err != nil {
			return c, fmt.Errorf("-period: %w", err)
		}
		c.Period = d
	}
	return c, nil
}

func validDomain(s string) bool {
	return s != "" && strings.Contains(s, ".") && !strings.Contains(s, " ")
}

func (c Config) ValidateRun() error {
	if c.Token == "" {
		return errors.New("-token is required")
	}
	if c.DomainV4 == "" && c.DomainV6 == "" {
		return errors.New("at least one of -4 or -6 is required")
	}
	if c.DomainV4 != "" && !validDomain(c.DomainV4) {
		return fmt.Errorf("invalid -4 domain: %q", c.DomainV4)
	}
	if c.DomainV6 != "" && !validDomain(c.DomainV6) {
		return fmt.Errorf("invalid -6 domain: %q", c.DomainV6)
	}
	if c.Period <= 0 {
		return errors.New("-period must be > 0")
	}
	if c.Port < 0 || c.Port > 65535 {
		return errors.New("-port out of range")
	}
	if c.Port > 0 && c.Passwd == "" {
		return errors.New("-passwd is required when -port is set")
	}
	if c.Port == 0 && c.Passwd != "" {
		return errors.New("-passwd set but -port is not; HTTP disabled")
	}
	return nil
}

func (c Config) ValidateInstall() error {
	// Persist a working unit: same domain/token/period/port rules as run.
	return c.ValidateRun()
}

func (c Config) ModeConflict() error {
	n := 0
	if c.Install {
		n++
	}
	if c.Uninstall {
		n++
	}
	if n > 1 {
		return errors.New("-install and -uninstall are mutually exclusive")
	}
	if (c.Install || c.Uninstall) && c.Once {
		return errors.New("cannot combine -once with -install/-uninstall")
	}
	return nil
}
