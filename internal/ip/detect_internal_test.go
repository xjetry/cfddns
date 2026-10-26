package ip

import (
	"net/http"
	"testing"
)

// The leak fix depends on one Transport per family living as long as the
// Detector; a regression to per-call construction shows up here as distinct
// pointers.
func TestClient_ReusedPerFamily(t *testing.T) {
	d := &Detector{}
	v4a, v4b := d.client(IPv4), d.client(IPv4)
	v6a, v6b := d.client(IPv6), d.client(IPv6)
	if v4a != v4b || v6a != v6b {
		t.Fatal("client must return the same *http.Client for a family on every call")
	}
	if v4a == v6a {
		t.Fatal("families must not share a client: their dialers force different networks")
	}
	for _, c := range []*http.Client{v4a, v6a} {
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport type %T", c.Transport)
		}
		if tr.Proxy != nil {
			t.Fatal("detection must dial directly; a proxy would report the proxy's address")
		}
		if tr.IdleConnTimeout == 0 {
			t.Fatal("idle connections must expire")
		}
	}
}

func TestCloseIdleConnections_BeforeAndAfterUse(t *testing.T) {
	d := &Detector{}
	d.CloseIdleConnections() // no clients yet
	_ = d.client(IPv4)
	d.CloseIdleConnections()
	if d.client(IPv4) == nil {
		t.Fatal("client must survive CloseIdleConnections")
	}
}
