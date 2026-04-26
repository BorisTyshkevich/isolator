package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
)

// Handler implements http.Handler for the per-user docker-proxy. Construct via
// NewHandler.
type Handler struct {
	User           string
	UpstreamSocket string

	transport    *http.Transport
	reverseProxy *httputil.ReverseProxy

	// ownership pulls upstream container/exec metadata and decides if the
	// request is allowed to proceed.
	ownership *OwnershipChecker
}

// NewHandler builds the http.Handler with all routing and the upstream
// transport wired to dial the daemon's Unix socket.
func NewHandler(user, upstreamSocket string) *Handler {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", upstreamSocket)
		},
		// Disable HTTP/2 — Docker speaks HTTP/1.1 over Unix.
		ForceAttemptHTTP2: false,
	}

	h := &Handler{
		User:           user,
		UpstreamSocket: upstreamSocket,
		transport:      transport,
		ownership:      &OwnershipChecker{UpstreamSocket: upstreamSocket, User: user},
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "docker"
		},
		Transport:      transport,
		ModifyResponse: h.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			h.logf("ERROR: upstream proxy error for %s %s: %v", r.Method, r.URL.Path, err)
			writeJSONError(w, http.StatusBadGateway, "isolator: upstream error")
		},
	}
	h.reverseProxy = rp
	return h
}

func (h *Handler) logf(format string, args ...interface{}) {
	log.Printf("[%s] "+format, append([]interface{}{h.User}, args...)...)
}

// ServeHTTP is the entry point per §14.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Anti-smuggling §7: explicit non-chunked Transfer-Encoding rejection.
	// net/http already rejects unknown encodings, but defense-in-depth.
	if tes := r.TransferEncoding; len(tes) > 0 {
		for _, te := range tes {
			if te != "chunked" {
				h.logf("BLOCKED: Transfer-Encoding %q", te)
				writeJSONError(w, 400, "isolator: Transfer-Encoding not allowed")
				return
			}
		}
	}

	rawURI := r.URL.RequestURI()

	if !IsEndpointAllowed(r.Method, rawURI, h.User) {
		h.logf("BLOCKED endpoint: %s %s", r.Method, r.URL.Path)
		writeJSONError(w, 403, fmt.Sprintf("isolator: endpoint not allowed: %s %s", r.Method, r.URL.Path))
		return
	}

	// 2. Large-body passthrough — §8.
	if isLargeBodyPath(r.Method, r.URL.Path) {
		h.handleLargeBody(w, r)
		return
	}

	// 3. Container create — §9.
	if isContainerCreatePath(r.Method, r.URL.Path) {
		h.handleContainerCreate(w, r)
		return
	}

	// 4. Network create — §6.4.
	if isNetworkCreatePath(r.Method, r.URL.Path) {
		h.handleNetworkCreate(w, r)
		return
	}

	// 5. Ownership check — §10. Runs BEFORE any hijack on streaming endpoints
	// so unauthorized clients never enter relay mode.
	if err := h.maybeCheckOwnership(r); err != nil {
		var ce *CreateError
		if errors.As(err, &ce) {
			h.logf("BLOCKED: %s", ce.Message)
			writeJSONError(w, ce.Status, ce.Message)
			return
		}
		h.logf("ERROR: ownership check: %v", err)
		writeJSONError(w, 403, "isolator: ownership check failed")
		return
	}

	// 6. Streaming hijack — §12.
	if isStreamingPath(r.URL.Path) {
		h.handleStreaming(w, r)
		return
	}

	// 7. Default: reverse proxy. Container list filtering is implemented in
	// modifyResponse below.
	h.reverseProxy.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Route predicates
// ---------------------------------------------------------------------------

func isLargeBodyPath(method, path string) bool {
	if method != "POST" {
		return false
	}
	if reBuild.MatchString(path) {
		return true
	}
	if reImagesLoad.MatchString(path) {
		return true
	}
	return false
}

func isContainerCreatePath(method, path string) bool {
	return method == "POST" && reContainersCreate.MatchString(path)
}

func isNetworkCreatePath(method, path string) bool {
	return method == "POST" && reNetworksCreate.MatchString(path)
}

// isStreamingPath uses substring matching per §12. The spec acknowledges this
// over-matches /exec/{id}/resize and /exec/{id}/json — that is intentional.
func isStreamingPath(path string) bool {
	for _, sub := range []string{"/attach", "/exec", "/wait", "/logs", "/events"} {
		if strings.Contains(path, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Container create handler
// ---------------------------------------------------------------------------

func (h *Handler) handleContainerCreate(w http.ResponseWriter, r *http.Request) {
	body, err := readBoundedBody(r.Body, MaxBodySize)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeJSONError(w, 413, fmt.Sprintf("isolator: request body too large (>%d bytes)", MaxBodySize))
			return
		}
		writeJSONError(w, 400, "isolator: failed to read body")
		return
	}

	newBody, err := CheckCreate(body, h.User)
	if err != nil {
		var ce *CreateError
		if errors.As(err, &ce) {
			h.logf("BLOCKED: %s", ce.Message)
			writeJSONError(w, ce.Status, ce.Message)
			return
		}
		writeJSONError(w, 400, "isolator: bad request")
		return
	}

	rewriteRequestBody(r, newBody)
	h.reverseProxy.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Network create handler
// ---------------------------------------------------------------------------

func (h *Handler) handleNetworkCreate(w http.ResponseWriter, r *http.Request) {
	body, err := readBoundedBody(r.Body, MaxBodySize)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeJSONError(w, 413, fmt.Sprintf("isolator: request body too large (>%d bytes)", MaxBodySize))
			return
		}
		writeJSONError(w, 400, "isolator: failed to read body")
		return
	}
	newBody, err := CheckNetworkCreate(body, h.User)
	if err != nil {
		var ce *CreateError
		if errors.As(err, &ce) {
			h.logf("BLOCKED: %s", ce.Message)
			writeJSONError(w, ce.Status, ce.Message)
			return
		}
		writeJSONError(w, 400, "isolator: bad request")
		return
	}
	rewriteRequestBody(r, newBody)
	h.reverseProxy.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Ownership check
// ---------------------------------------------------------------------------

// maybeCheckOwnership runs container/exec ownership when the request is a
// per-resource action. Returns nil for paths that don't need it.
func (h *Handler) maybeCheckOwnership(r *http.Request) error {
	path := r.URL.Path

	// Exec actions: /v<ver>/exec/<id>/{start,resize,json}
	if reExecAction.MatchString(path) {
		ver, id, ok := ExtractExecTarget(path)
		if !ok {
			return nil
		}
		return h.ownership.CheckExecOwned(ver, id)
	}

	// Container actions and container delete
	if reContainerAction.MatchString(path) || reContainerDelete.MatchString(path) {
		ver, id, ok := ExtractContainerTarget(path)
		if !ok {
			return nil
		}
		return h.ownership.CheckContainerOwned(ver, id)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Streaming handler
// ---------------------------------------------------------------------------

// handleStreaming hijacks the client connection, dials upstream, replays the
// request line + headers + any already-buffered body bytes, then enters
// bidirectional relay until either side closes (§12).
func (h *Handler) handleStreaming(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeJSONError(w, 500, "isolator: hijacking not supported")
		return
	}

	upstream, err := net.Dial("unix", h.UpstreamSocket)
	if err != nil {
		writeJSONError(w, 502, "isolator: upstream dial failed")
		return
	}

	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}

	// Build the upstream request: same method + URI + headers from the client.
	if err := writeRequestLine(upstream, r); err != nil {
		_ = clientConn.Close()
		_ = upstream.Close()
		return
	}

	// Forward any bytes we have already buffered from the body before relay.
	// clientBuf is a *bufio.ReadWriter; the read side may have leftover bytes
	// from header parsing.
	if clientBuf != nil && clientBuf.Reader != nil {
		if n := clientBuf.Reader.Buffered(); n > 0 {
			peeked, _ := clientBuf.Reader.Peek(n)
			if _, err := upstream.Write(peeked); err != nil {
				_ = clientConn.Close()
				_ = upstream.Close()
				return
			}
			_, _ = clientBuf.Reader.Discard(n)
		}
	}

	Relay(clientConn, upstream)
}

func writeRequestLine(w io.Writer, r *http.Request) error {
	// Build request line: METHOD URI HTTP/1.1.
	uri := r.URL.RequestURI()
	if uri == "" {
		uri = r.URL.Path
	}
	header := &bytes.Buffer{}
	fmt.Fprintf(header, "%s %s HTTP/1.1\r\n", r.Method, uri)
	// Forward all headers as-is. Host comes from r.Host (Go strips it from
	// r.Header).
	host := r.Host
	if host == "" {
		host = "docker"
	}
	fmt.Fprintf(header, "Host: %s\r\n", host)
	for k, vs := range r.Header {
		for _, v := range vs {
			fmt.Fprintf(header, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprint(header, "\r\n")
	_, err := w.Write(header.Bytes())
	return err
}

// ---------------------------------------------------------------------------
// Large-body passthrough (§8)
// ---------------------------------------------------------------------------

// handleLargeBody streams /build and /images/load through the reverse proxy.
// We rely on httputil.ReverseProxy to forward the body without buffering. The
// 16 MB limit and ownership checks are skipped here.
func (h *Handler) handleLargeBody(w http.ResponseWriter, r *http.Request) {
	// We must NOT touch r.Body — let ReverseProxy stream it through. The
	// request body has unknown length when chunked; ReverseProxy sets the
	// outbound Transfer-Encoding accordingly.
	h.reverseProxy.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Response modification (container list filtering, §11)
// ---------------------------------------------------------------------------

func (h *Handler) modifyResponse(resp *http.Response) error {
	// Match GET /v<ver>/containers/json (no query handling needed for routing
	// — the path is what matters).
	if resp.Request == nil {
		return nil
	}
	if resp.Request.Method != "GET" {
		return nil
	}
	if !reContainersList.MatchString(resp.Request.URL.Path) {
		return nil
	}
	// The list response is JSON. If status != 200, leave it alone.
	if resp.StatusCode != 200 {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	filtered := FilterContainerList(body, h.User)
	resp.Body = io.NopCloser(bytes.NewReader(filtered))
	resp.ContentLength = int64(len(filtered))
	resp.Header.Set("Content-Length", strconv.Itoa(len(filtered)))
	resp.Header.Del("Transfer-Encoding")
	return nil
}

// ---------------------------------------------------------------------------
// Body utilities
// ---------------------------------------------------------------------------

var errBodyTooLarge = errors.New("body too large")

// readBoundedBody reads up to limit+1 bytes; if body exceeds limit returns
// errBodyTooLarge.
func readBoundedBody(r io.Reader, limit int) ([]byte, error) {
	lr := io.LimitReader(r, int64(limit)+1)
	body, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// rewriteRequestBody substitutes a known-length body on the request and fixes
// up framing per §9.9 step 3-5: ContentLength struct field, Content-Length
// header, drop Transfer-Encoding.
func rewriteRequestBody(r *http.Request, newBody []byte) {
	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))
	r.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	r.Header.Del("Transfer-Encoding")
	r.TransferEncoding = nil
}

