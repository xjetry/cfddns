package ip

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
)

// DefaultProcIfInet6 is the Linux kernel's IPv6 address table. Each line is
// "<32 hex addr> <ifindex> <prefixlen> <scope> <flags> <ifname>", with scope
// and the low byte of the address flags in hex (net/ipv6/addrconf.c).
const DefaultProcIfInet6 = "/proc/net/if_inet6"

// Address flag bits as printed by if_inet6 (include/uapi/linux/if_addr.h).
const (
	ifaTemporary  = 0x01
	ifaDADFailed  = 0x08
	ifaDeprecated = 0x20
	ifaTentative  = 0x40
	ifaPermanent  = 0x80

	scopeGlobal = 0x00
)

type ifInet6Entry struct {
	addr  netip.Addr
	scope uint8
	flags uint8
	iface string
}

func parseIfInet6(r io.Reader) ([]ifInet6Entry, error) {
	var out []ifInet6Entry
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 6 {
			return nil, fmt.Errorf("line %d: expected 6 fields, got %d", n, len(fields))
		}
		raw, err := hex.DecodeString(fields[0])
		if err != nil || len(raw) != 16 {
			return nil, fmt.Errorf("line %d: bad address %q", n, fields[0])
		}
		scope, err := strconv.ParseUint(fields[3], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("line %d: bad scope %q", n, fields[3])
		}
		flags, err := strconv.ParseUint(fields[4], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("line %d: bad flags %q", n, fields[4])
		}
		out = append(out, ifInet6Entry{
			addr:  netip.AddrFrom16([16]byte(raw)),
			scope: uint8(scope),
			flags: uint8(flags),
			iface: fields[5],
		})
	}
	return out, sc.Err()
}

// stableCandidates filters entries of iface down to addresses suitable for a
// long-lived AAAA record: global scope, globally routable (no ULA, link-local
// or loopback), and not temporary (RFC 4941 privacy addresses rotate daily),
// deprecated, tentative or DAD-failed. Candidates are ordered so the choice
// is the same across cycles and reboots regardless of kernel list order:
// permanent (manually configured) addresses first, then by address value.
func stableCandidates(entries []ifInet6Entry, iface string) (cands []netip.Addr, seenIface bool) {
	for _, e := range entries {
		if e.iface != iface {
			continue
		}
		seenIface = true
		if e.scope != scopeGlobal {
			continue
		}
		if e.flags&(ifaTemporary|ifaDeprecated|ifaTentative|ifaDADFailed) != 0 {
			continue
		}
		if !e.addr.IsGlobalUnicast() || e.addr.IsPrivate() {
			continue
		}
		cands = append(cands, e.addr)
	}
	perm := map[netip.Addr]bool{}
	for _, e := range entries {
		if e.iface == iface && e.flags&ifaPermanent != 0 {
			perm[e.addr] = true
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if perm[cands[i]] != perm[cands[j]] {
			return perm[cands[i]]
		}
		return cands[i].Less(cands[j])
	})
	return cands, seenIface
}

// StableIPv6 returns the preferred stable global IPv6 address of iface by
// reading the kernel table at path. Errors describe why nothing was chosen so
// callers can fall back and report the reason.
func StableIPv6(path, iface string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	entries, err := parseIfInet6(f)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	cands, seen := stableCandidates(entries, iface)
	if !seen {
		return "", fmt.Errorf("interface %q has no IPv6 addresses", iface)
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("interface %q has no stable global IPv6 address", iface)
	}
	return cands[0].String(), nil
}
