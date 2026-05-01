package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mockUpstream is a tiny test fixture that listens on a Unix socket and
// dispatches incoming GETs to a per-path scripted response. Used by the
// ownership tests to fake `docker inspect` results without a real daemon.
type mockUpstream struct {
	t        *testing.T
	listener net.Listener
	socket   string
	mu       sync.Mutex
	// route: path -> (status, jsonBody)
	routes map[string]mockResponse
}

type mockResponse struct {
	status int
	body   string
}

func newMockUpstream(t *testing.T) *mockUpstream {
	t.Helper()
	// Unix socket paths max out around 104 chars on macOS, so use a short path
	// under /tmp rather than the (long) t.TempDir under /var/folders/...
	dir, err := mkShortTempDir("iso-mock-")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "u.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = removeAll(dir)
	})
	m := &mockUpstream{
		t:        t,
		listener: l,
		socket:   sock,
		routes:   map[string]mockResponse{},
	}
	go m.serve()
	t.Cleanup(func() {
		_ = l.Close()
	})
	return m
}

func (m *mockUpstream) addRoute(path string, status int, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[path] = mockResponse{status: status, body: body}
}

func (m *mockUpstream) serve() {
	for {
		c, err := m.listener.Accept()
		if err != nil {
			return
		}
		go m.handle(c)
	}
}

func (m *mockUpstream) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	// Drain the request body before responding. Otherwise the proxy
	// (writing client → upstream) gets "broken pipe" when we close before
	// it finishes flushing, and the round-trip surfaces as 502.
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	m.mu.Lock()
	r, ok := m.routes[req.URL.Path]
	m.mu.Unlock()
	if !ok {
		// Default 404
		r = mockResponse{status: 404, body: `{"message":"not found"}`}
	}
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		r.status, http.StatusText(r.status), len(r.body), r.body)
	_, _ = c.Write([]byte(resp))
}

func TestContainerOwned(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantBlocked bool
		wantStatus  int // CreateError.Status when blocked
		errSub      string
	}{
		{"container owned", 200, `{"Config":{"Labels":{"dev.boris.isolator.user":"acm"}}}`, false, 0, ""},
		{"container wrong owner", 200, `{"Config":{"Labels":{"dev.boris.isolator.user":"other"}}}`, true, 403, "is not owned by acm"},
		{"container no label", 200, `{"Config":{"Labels":{}}}`, true, 403, "is not owned by acm"},
		// 404: pass-through with Docker's standard phrasing — buildx/compose
		// rely on this to detect "container does not exist yet" before
		// creating it. See ownership.go:CheckContainerOwned doc comment.
		{"container not found", 404, `{"message":"not found"}`, true, 404, "No such container"},
		{"container inspect 500", 500, `{"message":"error"}`, true, 403, "container inspect failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockUpstream(t)
			m.addRoute("/v1.51/containers/abc/json", tc.status, tc.body)
			o := &OwnershipChecker{UpstreamSocket: m.socket, User: "acm"}
			err := o.CheckContainerOwned("v1.51", "abc")
			if tc.wantBlocked {
				if err == nil {
					t.Fatal("expected blocked")
				}
				var ce *CreateError
				if !errors.As(err, &ce) || ce.Status != tc.wantStatus {
					t.Errorf("want status=%d CreateError, got %+v", tc.wantStatus, err)
				}
				if !strings.Contains(err.Error(), tc.errSub) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestContainerOwnedFallbackTopLevel covers the §10 fallback to top-level
// Labels when Config.Labels is absent.
func TestContainerOwnedFallbackTopLevel(t *testing.T) {
	m := newMockUpstream(t)
	m.addRoute("/v1.51/containers/abc/json", 200, `{"Labels":{"dev.boris.isolator.user":"acm"}}`)
	o := &OwnershipChecker{UpstreamSocket: m.socket, User: "acm"}
	if err := o.CheckContainerOwned("v1.51", "abc"); err != nil {
		t.Errorf("unexpected: %v", err)
	}
}

func TestExecOwned(t *testing.T) {
	cases := []struct {
		name        string
		execStatus  int
		execBody    string
		ctnrStatus  int
		ctnrBody    string
		wantBlocked bool
		errSub      string
	}{
		{
			name:       "exec owned",
			execStatus: 200, execBody: `{"ContainerID":"abc"}`,
			ctnrStatus: 200, ctnrBody: `{"Config":{"Labels":{"dev.boris.isolator.user":"acm"}}}`,
			wantBlocked: false,
		},
		{
			name:       "exec wrong owner",
			execStatus: 200, execBody: `{"ContainerID":"abc"}`,
			ctnrStatus: 200, ctnrBody: `{"Config":{"Labels":{"dev.boris.isolator.user":"other"}}}`,
			wantBlocked: true, errSub: "is not owned by acm",
		},
		{
			// 404 passes through unchanged so probe-then-create flows
			// see Docker's standard "no such instance" rather than 403.
			name:       "exec not found",
			execStatus: 404, execBody: `{"message":"not found"}`,
			wantBlocked: true, errSub: "No such exec instance",
		},
		{
			name:       "exec missing ContainerID",
			execStatus: 200, execBody: `{}`,
			wantBlocked: true, errSub: "exec inspect missing ContainerID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockUpstream(t)
			m.addRoute("/v1.51/exec/xyz/json", tc.execStatus, tc.execBody)
			if tc.ctnrStatus != 0 {
				m.addRoute("/v1.51/containers/abc/json", tc.ctnrStatus, tc.ctnrBody)
			}
			o := &OwnershipChecker{UpstreamSocket: m.socket, User: "acm"}
			err := o.CheckExecOwned("v1.51", "xyz")
			if tc.wantBlocked {
				if err == nil {
					t.Fatal("expected blocked")
				}
				if !strings.Contains(err.Error(), tc.errSub) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestFilterContainerList exercises §11 list filtering: only items whose
// Labels[OwnerLabel] matches user are kept.
func TestFilterContainerList(t *testing.T) {
	body := `[
		{"Id":"a","Labels":{"dev.boris.isolator.user":"acm"}},
		{"Id":"b","Labels":{"dev.boris.isolator.user":"other"}},
		{"Id":"c","Labels":{}},
		{"Id":"d","Labels":{"dev.boris.isolator.user":"acm","x":"y"}}
	]`
	out := FilterContainerList([]byte(body), "acm")
	// Output must contain exactly Id "a" and "d".
	s := string(out)
	if !strings.Contains(s, `"Id":"a"`) || !strings.Contains(s, `"Id":"d"`) {
		t.Errorf("wanted a and d, got %s", s)
	}
	if strings.Contains(s, `"Id":"b"`) || strings.Contains(s, `"Id":"c"`) {
		t.Errorf("unexpected items kept: %s", s)
	}
}

// TestFilterContainerListInvalidJSON ensures an unparseable body is returned
// unchanged so ReverseProxy can stream it.
func TestFilterContainerListInvalidJSON(t *testing.T) {
	in := []byte("not json")
	out := FilterContainerList(in, "acm")
	if string(out) != string(in) {
		t.Errorf("invalid JSON body was modified: %q", out)
	}
}
