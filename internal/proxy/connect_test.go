package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAllowlist(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	return path
}

func TestHostAllowlistMatch(t *testing.T) {
	path := writeAllowlist(t, `
# comment line
github.com
proxy.golang.org
*.googleapis.com
   chatgpt.com
`)
	a, err := LoadHostAllowlist(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	cases := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"GitHub.com", true},
		{"proxy.golang.org", true},
		{"chatgpt.com", true},
		{"sum.golang.org", false},
		{"foo.googleapis.com", true},
		{"deep.nested.googleapis.com", true},
		{"googleapis.com", false}, // *.googleapis.com matches subs only
		{"evil.com", false},
	}
	for _, c := range cases {
		if got := a.Contains(c.host); got != c.want {
			t.Errorf("Contains(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestHostAllowlistReload(t *testing.T) {
	path := writeAllowlist(t, "github.com\n")
	a, err := LoadHostAllowlist(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !a.Contains("github.com") || a.Contains("evil.com") {
		t.Fatalf("initial state wrong")
	}
	if err := os.WriteFile(path, []byte("evil.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if a.Contains("github.com") || !a.Contains("evil.com") {
		t.Fatalf("reload did not swap state")
	}
}

func startConnectProxy(t *testing.T, allowlist string) (string, func()) {
	t.Helper()
	path := writeAllowlist(t, allowlist)
	a, err := LoadHostAllowlist(path)
	if err != nil {
		t.Fatalf("load allowlist: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := NewConnectProxy("test", a, nil)
	go func() { _ = p.Serve(l) }()
	return l.Addr().String(), func() { _ = l.Close() }
}

// sendCONNECT issues a CONNECT request to the proxy and returns the response.
func sendCONNECT(t *testing.T, proxyAddr, target string) *http.Response {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_, _ = io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp
}

func TestConnectProxyDeniedByPort(t *testing.T) {
	addr, stop := startConnectProxy(t, "github.com\n")
	defer stop()

	resp := sendCONNECT(t, addr, "github.com:22")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "no_proxy") {
		t.Errorf("body = %q, want hint to add to no_proxy", body)
	}
}

func TestConnectProxyDeniedByHost(t *testing.T) {
	addr, stop := startConnectProxy(t, "github.com\n")
	defer stop()

	resp := sendCONNECT(t, addr, "evil.com:443")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "evil.com") {
		t.Errorf("body = %q, want mention of denied host", body)
	}
}

func TestConnectProxyMethodNotAllowed(t *testing.T) {
	addr, stop := startConnectProxy(t, "github.com\n")
	defer stop()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// Allowlist denial fires before port check: "missing hostname" is the more
// actionable error — operator either adds the host to allowlist or moves it
// to no_proxy.
func TestConnectProxyHostCheckBeforePort(t *testing.T) {
	addr, stop := startConnectProxy(t, "github.com\n")
	defer stop()

	// evil.com:6443 — both checks would fail. Expect host-not-in-allowlist.
	resp := sendCONNECT(t, addr, "evil.com:6443")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not in allowlist") {
		t.Errorf("body = %q, want allowlist message", body)
	}
}

