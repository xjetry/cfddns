package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"cfddns/internal/envfile"
)

const (
	DefaultPeriod = 5 * time.Minute
	// MinPeriod bounds how often a cycle may run. Every cycle costs one trace
	// request per family, and recheck cycles add Cloudflare API calls that
	// count against the token's 1200 requests / 5 min limit.
	MinPeriod      = 10 * time.Second
	DefaultEnvPath = "/etc/cfddns.env"
)

// Persisted environment keys. This table is the only place the key names
// live: ApplyEnvMap reads them, EnvPairs writes them, ApplyOSEnv enumerates
// them.
const (
	EnvV4      = "CFDDNS_V4"
	EnvV6      = "CFDDNS_V6"
	EnvV6Iface = "CFDDNS_V6_IFACE"
	EnvToken   = "CFDDNS_TOKEN"
	EnvPeriod  = "CFDDNS_PERIOD"
	EnvPort    = "CFDDNS_PORT"
	EnvPasswd  = "CFDDNS_PASSWD"
)

var envKeys = []string{EnvV4, EnvV6, EnvV6Iface, EnvToken, EnvPeriod, EnvPort, EnvPasswd}

type Config struct {
	DomainV4 string
	DomainV6 string
	// V6Iface names the interface whose stable global IPv6 address is used
	// for the AAAA record instead of the trace answer; empty means trace only.
	V6Iface    string
	Token      string
	Period     time.Duration
	Port       int // 0 = HTTP disabled
	Passwd     string
	StatusFile string // runtime-only; never persisted
	Once       bool
	Inspect    bool
	Install    bool
	// Init selects the init system for -install/-uninstall: "" (auto-detect),
	// systemd, openrc or procd. Never persisted.
	Init      string
	Uninstall bool
}

// ParseEnvFile reads a KEY=value file in the envfile grammar.
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return envfile.Parse(f)
}

// ApplyEnvMap sets fields from CFDDNS_* keys (only non-empty keys).
func (c *Config) ApplyEnvMap(m map[string]string) error {
	if v := m[EnvV4]; v != "" {
		c.DomainV4 = v
	}
	if v := m[EnvV6]; v != "" {
		c.DomainV6 = v
	}
	if v := m[EnvV6Iface]; v != "" {
		c.V6Iface = v
	}
	if v := m[EnvToken]; v != "" {
		c.Token = v
	}
	if v := m[EnvPeriod]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvPeriod, err)
		}
		c.Period = d
	}
	if v := m[EnvPort]; v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvPort, err)
		}
		c.Port = p
	}
	if v := m[EnvPasswd]; v != "" {
		c.Passwd = v
	}
	return nil
}

func (c *Config) ApplyOSEnv() error {
	m := map[string]string{}
	for _, k := range envKeys {
		if v := os.Getenv(k); v != "" {
			m[k] = v
		}
	}
	return c.ApplyEnvMap(m)
}

// EnvPairs lists the persisted keys in file order. Empty optional values are
// omitted; Period is always written so the installed service does not depend
// on the compiled-in default of whatever binary runs later.
func (c Config) EnvPairs() []envfile.Pair {
	var pairs []envfile.Pair
	add := func(k, v string) {
		if v != "" {
			pairs = append(pairs, envfile.Pair{Key: k, Value: v})
		}
	}
	add(EnvV4, c.DomainV4)
	add(EnvV6, c.DomainV6)
	add(EnvV6Iface, c.V6Iface)
	add(EnvToken, c.Token)
	add(EnvPeriod, c.Period.String())
	if c.Port > 0 {
		add(EnvPort, strconv.Itoa(c.Port))
	}
	add(EnvPasswd, c.Passwd)
	return pairs
}

// EnvFile renders the body written to DefaultEnvPath by -install.
func (c Config) EnvFile() (string, error) {
	return envfile.Encode(c.EnvPairs())
}

// Usage writes the -h help text (flags, requirements, log examples).
func Usage(w io.Writer) {
	fmt.Fprintf(w, `cfddns — Cloudflare DDNS (A/AAAA) for the host public IP

Usage:
  cfddns [flags]                 run as daemon (period updates)
  cfddns -once [flags]           run one update cycle and exit
  cfddns -inspect [flags]        read current Cloudflare records and exit
  cfddns -install [flags]        install binary + %[2]s + service (root)
  cfddns -uninstall              remove service, env, and binary (root)
  cfddns -h                      show this help

Flags:
  -4 string
        [optional*] IPv4 FQDN → Cloudflare A record (env %[3]s)
  -6 string
        [optional*] IPv6 FQDN → Cloudflare AAAA record (env %[4]s)
  -6-iface string
        [optional]  prefer the stable global IPv6 of this interface for -6;
                    falls back to trace when none is found (env %[5]s)
  -token string
        [required]  Cloudflare API Token; Zone:DNS:Edit + Zone:Zone:Read (env %[6]s)
  -period duration
        [optional]  update interval, Go duration, >= %[7]s (default %[1]s) (env %[8]s)
  -port int
        [optional]  HTTP listen port; omit/0 = no HTTP (env %[9]s)
  -passwd string
        [required if -port] Bearer secret for HTTP /update (env %[10]s)
  -once
        [optional]  one-shot update then exit (0 if all families ok, else 1)
  -inspect
        [optional]  read current Cloudflare A/AAAA records as JSON and exit
  -status-file string
        [optional]  write runtime status JSON to this path (daemon/-once only)
  -install
        [optional]  install as a service (needs same -4/-6/-token/... as run)
  -uninstall
        [optional]  stop and remove service, env, binary
  -init string
        [optional]  init system for -install/-uninstall: systemd, openrc, procd
                    (default: auto-detect)
  -h, -help
        show this help

* At least one of -4 or -6 is required for daemon, -once, -inspect, and -install.

Rules:
  - Config merge for daemon/-once: defaults → %[2]s → process env → CLI flags
  - -install persists exactly process env + CLI flags; it never merges an
    existing %[2]s, so re-running -install replaces the old configuration
  - Domains are lower-cased and must be RFC 1123 host names (use punycode for IDN)
  - Values in %[2]s are bare KEY=value: printable ASCII without whitespace,
    quotes, backslash, '#' or ';' (so -passwd/-token must fit that too)
  - Missing IPv4/IPv6 stack for a configured family: soft-fail (log, skip that family);
    daemon keeps running
  - -port requires -passwd; -passwd without -port is an error
  - Records are created DNS-only (proxied=false, ttl=auto); updates change only
    the content, other record fields are left as-is
  - Zone and record IDs are cached; a full re-read happens every 12 cycles

HTTP (when -port is set):
  curl -sS -H "Authorization: Bearer <passwd>" -X POST http://127.0.0.1:<port>/update
  (GET /update also works)

Service logs (after -install):
  systemd:  journalctl -u cfddns -f
            journalctl -u cfddns -n 100 --no-pager
            journalctl -u cfddns --since "1 hour ago"
  OpenRC:   tail -f /var/log/cfddns.log
  procd:    logread -e cfddns -f

Examples:
  cfddns -4 home.example.com -token "$CF_TOKEN"
  cfddns -4 home.example.com -6 home.example.com -6-iface eth0 -token "$CF_TOKEN" -period 2m
  sudo cfddns -install -4 home.example.com -token "$CF_TOKEN" -period 5m
  sudo cfddns -install -4 home.example.com -token "$CF_TOKEN" -port 8080 -passwd secret
  sudo cfddns -once
  sudo cfddns -uninstall

`, DefaultPeriod, DefaultEnvPath, EnvV4, EnvV6, EnvV6Iface, EnvToken, MinPeriod, EnvPeriod, EnvPort, EnvPasswd)
}

// Load is LoadFrom with the production env file path.
func Load(args []string) (Config, error) {
	return LoadFrom(DefaultEnvPath, args)
}

// LoadFrom merges configuration layers, higher wins:
// defaults → env file → process env → CLI flags that were explicitly set.
//
// The env file layer is skipped for -install and -uninstall. -install must
// persist exactly what the operator passes now, otherwise keys from a previous
// install (an old -6, -port, -passwd) would silently survive a reconfigure.
// -uninstall needs no configuration and must not fail on a corrupt file it is
// about to delete.
//
// Passing -h/-help returns flag.ErrHelp after printing Usage (caller exits 0).
func LoadFrom(envPath string, args []string) (Config, error) {
	c := Config{Period: DefaultPeriod}

	fs := flag.NewFlagSet("cfddns", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { Usage(fs.Output()) }

	var fl Config
	fs.StringVar(&fl.DomainV4, "4", "", "IPv4 FQDN → A record")
	fs.StringVar(&fl.DomainV6, "6", "", "IPv6 FQDN → AAAA record")
	fs.StringVar(&fl.V6Iface, "6-iface", "", "interface for stable IPv6")
	fs.StringVar(&fl.Token, "token", "", "Cloudflare API token")
	fs.DurationVar(&fl.Period, "period", DefaultPeriod, "update interval")
	fs.IntVar(&fl.Port, "port", 0, "HTTP port; 0=disabled")
	fs.StringVar(&fl.Passwd, "passwd", "", "Bearer secret")
	fs.StringVar(&fl.StatusFile, "status-file", "", "write runtime status JSON to path")
	fs.BoolVar(&fl.Once, "once", false, "one-shot update then exit")
	fs.BoolVar(&fl.Inspect, "inspect", false, "read current Cloudflare records and exit")
	fs.BoolVar(&fl.Install, "install", false, "install service (root)")
	fs.BoolVar(&fl.Uninstall, "uninstall", false, "uninstall service (root)")
	fs.StringVar(&fl.Init, "init", "", "init system: systemd|openrc|procd")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	c.Once, c.Inspect, c.Install, c.Uninstall, c.Init = fl.Once, fl.Inspect, fl.Install, fl.Uninstall, fl.Init
	c.StatusFile = fl.StatusFile

	if !c.Install && !c.Uninstall {
		// Missing or permission-denied (root 0600 while a non-root run passes
		// flags) is skipped; corrupt content still fails.
		m, err := ParseEnvFile(envPath)
		switch {
		case err == nil:
			if err := c.ApplyEnvMap(m); err != nil {
				return c, fmt.Errorf("%s: %w", envPath, err)
			}
		case os.IsNotExist(err), os.IsPermission(err):
		default:
			return c, fmt.Errorf("%s: %w", envPath, err)
		}
	}
	if err := c.ApplyOSEnv(); err != nil {
		return c, err
	}

	// Only flags the operator actually typed override lower layers, so an
	// untouched flag's default never masks an env value. An explicit -4 "" or
	// -port 0 is the way to clear an env-provided value for one run.
	if set["4"] {
		c.DomainV4 = fl.DomainV4
	}
	if set["6"] {
		c.DomainV6 = fl.DomainV6
	}
	if set["6-iface"] {
		c.V6Iface = fl.V6Iface
	}
	if set["token"] {
		c.Token = fl.Token
	}
	if set["period"] {
		c.Period = fl.Period
	}
	if set["port"] {
		c.Port = fl.Port
	}
	if set["passwd"] {
		c.Passwd = fl.Passwd
	}

	c.DomainV4 = NormalizeDomain(c.DomainV4)
	c.DomainV6 = NormalizeDomain(c.DomainV6)
	return c, nil
}

// NormalizeDomain lower-cases, trims whitespace and one trailing dot. The
// normalized string is what zone matching, record lookup and the persisted
// env file all use, so the same name can never be spelled two ways.
func NormalizeDomain(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".")
	return strings.ToLower(s)
}

// checkHostname enforces RFC 1123 host-name syntax on a normalized name: at
// most 253 characters, at least two labels, each 1..63 characters of
// [a-z0-9-] not starting or ending with '-'. Validation assumes the value went
// through NormalizeDomain; upper case is rejected on purpose so a Config built
// without Load cannot carry an unnormalized name into the API or env file.
func checkHostname(s string) error {
	if s == "" {
		return errors.New("empty")
	}
	if len(s) > 253 {
		return errors.New("longer than 253 characters")
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return errors.New("must have at least two labels, e.g. host.example.com")
	}
	for _, l := range labels {
		if l == "" {
			return errors.New("empty label")
		}
		if len(l) > 63 {
			return fmt.Errorf("label %q longer than 63 characters", l)
		}
		if l[0] == '-' || l[len(l)-1] == '-' {
			return fmt.Errorf("label %q starts or ends with '-'", l)
		}
		for i := 0; i < len(l); i++ {
			ch := l[i]
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
				continue
			}
			return fmt.Errorf("label %q contains %q; only a-z, 0-9 and '-' are allowed (use punycode for IDN)", l, ch)
		}
	}
	return nil
}

func checkIfaceName(s string) error {
	if strings.ContainsAny(s, " \t\n/") {
		return fmt.Errorf("invalid interface name %q", s)
	}
	return nil
}

func (c Config) ValidateRun() error {
	if c.Token == "" {
		return errors.New("-token is required")
	}
	if c.DomainV4 == "" && c.DomainV6 == "" {
		return errors.New("at least one of -4 or -6 is required")
	}
	if c.DomainV4 != "" {
		if err := checkHostname(c.DomainV4); err != nil {
			return fmt.Errorf("invalid -4 domain %q: %w", c.DomainV4, err)
		}
	}
	if c.DomainV6 != "" {
		if err := checkHostname(c.DomainV6); err != nil {
			return fmt.Errorf("invalid -6 domain %q: %w", c.DomainV6, err)
		}
	}
	if c.V6Iface != "" {
		if c.DomainV6 == "" {
			return errors.New("-6-iface requires -6")
		}
		if err := checkIfaceName(c.V6Iface); err != nil {
			return fmt.Errorf("-6-iface: %w", err)
		}
	}
	if c.Period < MinPeriod {
		return fmt.Errorf("-period must be >= %s", MinPeriod)
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

// ValidateInstall checks everything ValidateRun does plus that every value
// fits the env file grammar, so -install fails before touching the system
// rather than after copying the binary.
func (c Config) ValidateInstall() error {
	if err := c.ValidateRun(); err != nil {
		return err
	}
	if _, err := c.EnvFile(); err != nil {
		return fmt.Errorf("cannot persist to %s: %w", DefaultEnvPath, err)
	}
	return nil
}

func (c Config) ModeConflict() error {
	if c.Install && c.Uninstall {
		return errors.New("-install and -uninstall are mutually exclusive")
	}
	if (c.Install || c.Uninstall) && c.Once {
		return errors.New("cannot combine -once with -install/-uninstall")
	}
	if c.Init != "" && !c.Install && !c.Uninstall {
		return errors.New("-init only applies to -install/-uninstall")
	}
	if c.Inspect && (c.Once || c.Install || c.Uninstall) {
		return errors.New("cannot combine -inspect with -once/-install/-uninstall")
	}
	if c.StatusFile != "" && c.Inspect {
		return errors.New("cannot combine -status-file with -inspect")
	}
	if c.StatusFile != "" && (c.Install || c.Uninstall) {
		return errors.New("cannot combine -status-file with -install/-uninstall")
	}
	return nil
}
