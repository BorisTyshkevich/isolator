package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HostAllowlist is a hot-reloadable set of allowed hostnames. Lines are
// matched case-insensitively. A line of the form "*.example.com" matches any
// subdomain of example.com (but not example.com itself); a bare hostname
// matches exactly. Comment lines (starting with "#") and blank lines are
// ignored.
type HostAllowlist struct {
	path     string
	mu       sync.RWMutex
	hosts    map[string]struct{}
	suffixes []string
}

// LoadHostAllowlist reads the file at path and returns a hot-reloadable
// allowlist. Subsequent Reload() calls re-read the same path.
func LoadHostAllowlist(path string) (*HostAllowlist, error) {
	a := &HostAllowlist{path: path}
	if err := a.Reload(); err != nil {
		return nil, err
	}
	return a, nil
}

// Reload re-parses the on-disk file and atomically swaps in the new set.
func (a *HostAllowlist) Reload() error {
	f, err := os.Open(a.path)
	if err != nil {
		return err
	}
	defer f.Close()

	hosts := make(map[string]struct{})
	var suffixes []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.ToLower(line)
		if strings.HasPrefix(line, "*.") {
			suffixes = append(suffixes, line[1:])
		} else {
			hosts[line] = struct{}{}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}

	a.mu.Lock()
	a.hosts = hosts
	a.suffixes = suffixes
	a.mu.Unlock()
	return nil
}

// Contains reports whether host is allowed under the current snapshot.
func (a *HostAllowlist) Contains(host string) bool {
	host = strings.ToLower(host)
	a.mu.RLock()
	defer a.mu.RUnlock()
	if _, ok := a.hosts[host]; ok {
		return true
	}
	for _, suf := range a.suffixes {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	return false
}

// Size reports the number of exact and suffix entries currently loaded
// (mainly for startup logging).
func (a *HostAllowlist) Size() (exact, suffix int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.hosts), len(a.suffixes)
}

// ConnectProxy is an HTTP CONNECT-only forward proxy that enforces a host
// allowlist. Only TCP/443 and TCP/80 are forwarded — non-HTTPS protocols
// (ssh, ClickHouse-native, k8s API on 6443, etc.) are expected to bypass
// HTTPS_PROXY entirely (raw TCP clients ignore it; HTTP-aware tools should
// have their target host listed in NO_PROXY so they go direct via pf).
// Each accepted connection runs in its own goroutine.
type ConnectProxy struct {
	User      string
	Allowlist *HostAllowlist
	Logger    *log.Logger
	// DialTimeout bounds the upstream dial. Zero means 10s.
	DialTimeout time.Duration

	connID atomic.Uint64
}

// NewConnectProxy constructs a proxy bound to the given allowlist.
func NewConnectProxy(user string, allowlist *HostAllowlist, logger *log.Logger) *ConnectProxy {
	if logger == nil {
		logger = log.Default()
	}
	return &ConnectProxy{User: user, Allowlist: allowlist, Logger: logger}
}

// Serve accepts connections from l and handles each as a CONNECT request.
// Returns when l.Accept returns an error (typically when the listener is
// closed).
func (p *ConnectProxy) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go p.handle(c)
	}
}

func (p *ConnectProxy) handle(client net.Conn) {
	defer client.Close()
	id := p.connID.Add(1)

	// Bounded read deadline for the request line + headers so a slow client
	// can't tie up a goroutine indefinitely.
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))

	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		p.Logger.Printf("[%s] connect[%d] read request: %v", p.User, id, err)
		return
	}

	// Clear the read deadline once we're past parsing — relay phase has its
	// own lifetime tied to data flow.
	_ = client.SetReadDeadline(time.Time{})

	if req.Method != http.MethodConnect {
		p.deny(client, id, req.Method+" "+req.RequestURI, http.StatusMethodNotAllowed,
			"only CONNECT is supported")
		return
	}

	target := req.RequestURI // "host:port"
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		p.deny(client, id, target, http.StatusBadRequest, "bad host:port")
		return
	}
	// Host check first: a missing hostname is the more common /
	// more-actionable failure mode (operator adds it to config.toml or
	// to no_proxy if the host should bypass the proxy).
	if !p.Allowlist.Contains(host) {
		p.deny(client, id, target, http.StatusForbidden,
			"host "+host+" not in allowlist")
		return
	}
	if port != "443" && port != "80" {
		p.deny(client, id, target, http.StatusForbidden,
			"port "+port+" not permitted (proxy handles 443/80; add host to no_proxy for direct access)")
		return
	}

	timeout := p.DialTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	upstream, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		p.deny(client, id, target, http.StatusBadGateway, "dial: "+err.Error())
		return
	}

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = upstream.Close()
		p.Logger.Printf("[%s] connect[%d] write 200: %v", p.User, id, err)
		return
	}
	p.Logger.Printf("[%s] connect[%d] accept %s", p.User, id, target)

	// Drain anything the client buffered before the relay.
	if buffered := br.Buffered(); buffered > 0 {
		if _, err := io.CopyN(upstream, br, int64(buffered)); err != nil {
			_ = upstream.Close()
			p.Logger.Printf("[%s] connect[%d] flush prelude: %v", p.User, id, err)
			return
		}
	}

	Relay(client, upstream)
	p.Logger.Printf("[%s] connect[%d] close  %s", p.User, id, target)
}

func (p *ConnectProxy) deny(client net.Conn, id uint64, target string, status int, reason string) {
	body := reason + "\r\n"
	resp := fmt.Sprintf(
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body,
	)
	_, _ = io.WriteString(client, resp)
	p.Logger.Printf("[%s] connect[%d] deny  %s status=%d reason=%q", p.User, id, target, status, reason)
}
