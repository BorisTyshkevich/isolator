package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// OwnershipChecker is the small interface the handler depends on, to make
// testing easy (the integration test fakes upstream).
type OwnershipChecker struct {
	UpstreamSocket string
	User           string
}

// CheckContainerOwned verifies the container with the given ID belongs to the
// proxy's user. Returns nil on owned, *CreateError otherwise.
//
// 1. subrequest GET /v<ver>/containers/<id>/json
// 2. 404 -> "container not found"
// 3. non-200 -> "container inspect failed"
// 4. parse JSON, look at Config.Labels[OwnerLabel], fallback to top-level Labels.
// 5. if missing or != user -> "container '<id>' is not owned by <user>"
func (o *OwnershipChecker) CheckContainerOwned(apiVersion, containerID string) error {
	path := fmt.Sprintf("/%s/containers/%s/json", apiVersion, containerID)
	status, body, err := subrequest(o.UpstreamSocket, path)
	if err != nil {
		return newCreateErr(403, "isolator: container inspect failed")
	}
	switch status {
	case 200:
		// fallthrough
	case 404:
		return newCreateErr(403, "isolator: container not found")
	default:
		return newCreateErr(403, "isolator: container inspect failed")
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return newCreateErr(403, "isolator: container inspect failed")
	}

	labelOwner := extractOwnerLabel(data)
	if labelOwner != o.User {
		return newCreateErr(403, "isolator: container '%s' is not owned by %s", containerID, o.User)
	}
	return nil
}

// CheckExecOwned verifies the exec instance is on a container owned by the
// proxy's user. Returns nil on owned, *CreateError otherwise.
//
// 1. subrequest GET /v<ver>/exec/<id>/json
// 2. 404 -> "exec not found"
// 3. non-200 -> "exec inspect failed"
// 4. parse, extract ContainerID; if empty -> "exec inspect missing ContainerID"
// 5. delegate to CheckContainerOwned.
func (o *OwnershipChecker) CheckExecOwned(apiVersion, execID string) error {
	path := fmt.Sprintf("/%s/exec/%s/json", apiVersion, execID)
	status, body, err := subrequest(o.UpstreamSocket, path)
	if err != nil {
		return newCreateErr(403, "isolator: exec inspect failed")
	}
	switch status {
	case 200:
		// fallthrough
	case 404:
		return newCreateErr(403, "isolator: exec not found")
	default:
		return newCreateErr(403, "isolator: exec inspect failed")
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return newCreateErr(403, "isolator: exec inspect failed")
	}
	cidRaw, _ := data["ContainerID"]
	cid, _ := cidRaw.(string)
	if cid == "" {
		return newCreateErr(403, "isolator: exec inspect missing ContainerID")
	}
	return o.CheckContainerOwned(apiVersion, cid)
}

// extractOwnerLabel pulls the owner label from either Config.Labels (full
// container inspect shape) or top-level Labels (list-item shape).
func extractOwnerLabel(data map[string]interface{}) string {
	if cfg, ok := data["Config"].(map[string]interface{}); ok {
		if labels, ok := cfg["Labels"].(map[string]interface{}); ok {
			if v, ok := labels[OwnerLabel].(string); ok {
				return v
			}
		}
	}
	if labels, ok := data["Labels"].(map[string]interface{}); ok {
		if v, ok := labels[OwnerLabel].(string); ok {
			return v
		}
	}
	return ""
}

// FilterContainerList rewrites the JSON response body for GET /containers/json
// to keep only items whose Labels[OwnerLabel] == user. If parsing fails, the
// original body is returned unchanged (per §11).
func FilterContainerList(body []byte, user string) []byte {
	var arr []map[string]interface{}
	if err := json.Unmarshal(body, &arr); err != nil {
		return body
	}
	kept := make([]map[string]interface{}, 0, len(arr))
	for _, item := range arr {
		if labels, ok := item["Labels"].(map[string]interface{}); ok {
			if v, ok := labels[OwnerLabel].(string); ok && v == user {
				kept = append(kept, item)
			}
		}
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return body
	}
	return out
}

// subrequest opens a one-shot HTTP/1.1 GET to the upstream Unix socket and
// returns (status, body, err). Always uses Connection: close. See §13.
func subrequest(upstreamSocket, httpPath string) (int, []byte, error) {
	conn, err := net.Dial("unix", upstreamSocket)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()

	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n", httpPath)
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

// PathSegments returns the path split on "/" with empties skipped.
// E.g. "/v1.51/containers/abc/start" -> ["v1.51","containers","abc","start"].
func PathSegments(p string) []string {
	out := make([]string, 0, 8)
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ExtractContainerTarget returns (apiVersion, containerID, true) when the path
// should trigger a container-ownership check, per §10. It returns false for
// /containers/create and /containers/json (those have their own handling) and
// for any path that doesn't fit the per-container shape.
func ExtractContainerTarget(path string) (string, string, bool) {
	segs := PathSegments(path)
	if len(segs) < 3 {
		return "", "", false
	}
	if segs[1] != "containers" {
		return "", "", false
	}
	id := segs[2]
	if id == "json" || id == "create" {
		return "", "", false
	}
	return segs[0], id, true
}

// ExtractExecTarget returns (apiVersion, execID, true) when the path is an
// exec action that should be ownership-checked.
func ExtractExecTarget(path string) (string, string, bool) {
	segs := PathSegments(path)
	if len(segs) < 3 {
		return "", "", false
	}
	if segs[1] != "exec" {
		return "", "", false
	}
	return segs[0], segs[2], true
}

// makeJSONErrorBody returns the canonical JSON body for proxy errors.
func makeJSONErrorBody(message string) []byte {
	type body struct {
		Message string `json:"message"`
	}
	out, _ := json.Marshal(body{Message: message})
	return out
}

// writeJSONError writes a proxy-format JSON error response per §16. It returns
// the body bytes for callers that need the length.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	body := makeJSONErrorBody(message)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = io.Copy(w, bytes.NewReader(body))
}
