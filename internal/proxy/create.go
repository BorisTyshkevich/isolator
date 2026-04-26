package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CreateError carries an HTTP status code alongside the error message so the
// handler can map directly to the response without a string-match.
type CreateError struct {
	Status  int
	Message string
}

func (e *CreateError) Error() string { return e.Message }

func newCreateErr(status int, format string, args ...interface{}) *CreateError {
	return &CreateError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// dangerousHostConfigFields lists every HostConfig field that is rejected when
// "truthy" per §9.1. Order matters — this is the order in which the prototype
// reports which field tripped the check, and tests rely on stable messages.
var dangerousHostConfigFields = []string{
	"Privileged",
	"VolumesFrom",
	"Devices",
	"DeviceCgroupRules",
	"DeviceRequests",
	"CapAdd",
	"CapDrop",
	"SecurityOpt",
	"PidMode",
	"IpcMode",
	"UTSMode",
	"UsernsMode",
	"CgroupnsMode",
	"CgroupParent",
	"Cgroup",
	"Runtime",
	"Sysctls",
	"Ulimits",
	"OomScoreAdj",
	"OomKillDisable",
	"DNS",
	"DNSOptions",
	"DNSSearch",
	"Links",
}

// isTruthy implements §9.1: bool true; non-empty string; non-empty array;
// non-empty map; *bool that points to true; non-zero int (incl. floats from
// json.Unmarshal). nil is not truthy.
func isTruthy(v interface{}) bool {
	if v == nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != ""
	case float64: // JSON numbers
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case []interface{}:
		return len(t) > 0
	case map[string]interface{}:
		return len(t) > 0
	default:
		// Anything else (e.g. typed objects) — treat presence as truthy.
		return true
	}
}

// CheckCreate validates and rewrites a container-create body for user.
// Returns the new (re-serialized) body bytes ready to forward upstream, or a
// *CreateError describing the policy/format violation.
//
// Implementation strategy: parse to map[string]interface{} at top level,
// mutate fields in place, marshal back. This preserves any field the proxy
// does not explicitly inspect — see §9.9 strategy A.
func CheckCreate(body []byte, user string) ([]byte, error) {
	if len(body) > MaxBodySize {
		return nil, newCreateErr(413, "isolator: request body too large (%d bytes)", len(body))
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, newCreateErr(400, "isolator: invalid JSON body")
	}

	// 9.7: container User field — must be string if present; uid=0 forbidden.
	if userVal, present := raw["User"]; present && userVal != nil {
		s, ok := userVal.(string)
		if !ok {
			return nil, newCreateErr(400, "isolator: invalid User field type")
		}
		trimmed := strings.TrimSpace(s)
		if trimmed == "root" || trimmed == "0" || strings.HasPrefix(trimmed, "0:") {
			return nil, newCreateErr(403, "isolator: container user '%s' not allowed", trimmed)
		}
	}

	// HostConfig validation (9.1, 9.2, 9.3, 9.5, 9.6).
	var hc map[string]interface{}
	if hcRaw, ok := raw["HostConfig"]; ok && hcRaw != nil {
		hcMap, isMap := hcRaw.(map[string]interface{})
		if !isMap {
			return nil, newCreateErr(400, "isolator: invalid HostConfig type")
		}
		hc = hcMap

		// 9.1 dangerous fields
		for _, field := range dangerousHostConfigFields {
			val, present := hc[field]
			if !present {
				continue
			}
			// *bool semantics: §9.1 says non-nil and *val == true. JSON
			// can't deliver a *bool literal — `false`/`true`/null map to
			// `bool`/nil here. The Python reference checks "if val:" so a
			// JSON `false` must be falsy. isTruthy already does this.
			if isTruthy(val) {
				return nil, newCreateErr(403, "isolator: %s is not allowed", field)
			}
		}

		// 9.2 ExtraHosts
		if eh, present := hc["ExtraHosts"]; present && eh != nil {
			ehArr, ok := eh.([]interface{})
			if !ok {
				return nil, newCreateErr(400, "isolator: invalid ExtraHosts type")
			}
			for _, entry := range ehArr {
				entryStr, _ := entry.(string)
				if strings.TrimSpace(entryStr) != "host.docker.internal:host-gateway" {
					return nil, newCreateErr(403, "isolator: ExtraHosts entry '%s' not allowed", entryStr)
				}
			}
		}

		// 9.3 NetworkMode
		if nm, present := hc["NetworkMode"]; present && nm != nil {
			nmStr, ok := nm.(string)
			if !ok {
				return nil, newCreateErr(400, "isolator: invalid NetworkMode type")
			}
			if !networkAllowedForUser(nmStr, user) {
				return nil, newCreateErr(403, "isolator: NetworkMode '%s' not allowed (use iso-%s)", nmStr, user)
			}
		}

		// 9.5 Binds
		if bindsRaw, present := hc["Binds"]; present && bindsRaw != nil {
			bindsArr, ok := bindsRaw.([]interface{})
			if !ok {
				return nil, newCreateErr(400, "isolator: invalid Binds type")
			}
			rewritten := make([]interface{}, 0, len(bindsArr))
			for _, b := range bindsArr {
				bs, ok := b.(string)
				if !ok {
					return nil, newCreateErr(400, "isolator: invalid Bind entry")
				}
				newBind, err := validateAndRewriteBind(bs, user)
				if err != nil {
					return nil, err
				}
				rewritten = append(rewritten, newBind)
			}
			hc["Binds"] = rewritten
		}

		// 9.6 Mounts
		if mountsRaw, present := hc["Mounts"]; present && mountsRaw != nil {
			mountsArr, ok := mountsRaw.([]interface{})
			if !ok {
				return nil, newCreateErr(400, "isolator: invalid Mounts type")
			}
			for _, m := range mountsArr {
				mMap, ok := m.(map[string]interface{})
				if !ok {
					return nil, newCreateErr(400, "isolator: invalid Mount entry")
				}
				if err := validateAndRewriteMount(mMap, user); err != nil {
					return nil, err
				}
			}
			// Mounts mutated in place inside mountsArr; rebind to be explicit.
			hc["Mounts"] = mountsArr
		}
	}

	// 9.4 NetworkingConfig.EndpointsConfig
	if ncRaw, ok := raw["NetworkingConfig"]; ok && ncRaw != nil {
		ncMap, isMap := ncRaw.(map[string]interface{})
		if !isMap {
			return nil, newCreateErr(400, "isolator: invalid NetworkingConfig type")
		}
		if epRaw, present := ncMap["EndpointsConfig"]; present && epRaw != nil {
			epMap, ok := epRaw.(map[string]interface{})
			if !ok {
				return nil, newCreateErr(400, "isolator: invalid EndpointsConfig type")
			}
			for key := range epMap {
				if !networkAllowedForUser(key, user) {
					return nil, newCreateErr(403, "isolator: network '%s' not allowed (use iso-%s)", key, user)
				}
			}
		}
	}
	// silence lint about hc unused
	_ = hc

	// 9.8 Owner label injection — mutate (or create) Labels in place.
	labels, ok := raw["Labels"].(map[string]interface{})
	if !ok || labels == nil {
		labels = map[string]interface{}{}
		raw["Labels"] = labels
	}
	labels[OwnerLabel] = user

	// 9.9 Re-serialize.
	newBody, err := json.Marshal(raw)
	if err != nil {
		return nil, newCreateErr(400, "isolator: failed to re-serialize body: %v", err)
	}
	return newBody, nil
}

// networkAllowedForUser implements the §9.3/9.4 allow-set check for both
// NetworkMode and EndpointsConfig keys: {"", "default", "bridge", "iso-<user>"}.
func networkAllowedForUser(name, user string) bool {
	switch name {
	case "", "default", "bridge":
		return true
	}
	return name == "iso-"+user
}

// validateAndRewriteBind parses one bind string ("source[:dest[:options]]"),
// validates source per §9.5, and returns the bind string with the source
// replaced by the resolved path.
func validateAndRewriteBind(bind, user string) (string, error) {
	// Split on ":" to extract source. The bind format keeps ":" as field separator
	// (Docker doesn't support ":" inside the host source on macOS/linux paths in this API).
	parts := strings.Split(bind, ":")
	if len(parts) == 0 {
		return "", newCreateErr(400, "isolator: empty bind")
	}
	source := parts[0]
	if !strings.HasPrefix(source, "/") {
		return "", newCreateErr(403, "isolator: named volume '%s' not allowed", source)
	}
	resolved := ResolvePath(source)
	if !IsPathAllowed(resolved, user) {
		return "", newCreateErr(403, "isolator: bind mount not allowed: %s", source)
	}
	if strings.Contains(resolved, "docker.sock") {
		return "", newCreateErr(403, "isolator: mounting Docker socket is not allowed")
	}
	parts[0] = resolved
	return strings.Join(parts, ":"), nil
}

// validateAndRewriteMount handles one entry under HostConfig.Mounts per §9.6.
// Mutates the map in place when the type is "bind".
func validateAndRewriteMount(m map[string]interface{}, user string) error {
	tRaw, _ := m["Type"]
	t, _ := tRaw.(string)
	switch t {
	case "bind":
		srcRaw, _ := m["Source"]
		src, _ := srcRaw.(string)
		if !strings.HasPrefix(src, "/") {
			return newCreateErr(403, "isolator: bind mount not allowed: %s", src)
		}
		resolved := ResolvePath(src)
		if !IsPathAllowed(resolved, user) {
			return newCreateErr(403, "isolator: bind mount not allowed: %s", src)
		}
		if strings.Contains(resolved, "docker.sock") {
			return newCreateErr(403, "isolator: mounting Docker socket is not allowed")
		}
		m["Source"] = resolved
		return nil
	case "volume":
		return newCreateErr(403, "isolator: named volumes are not allowed")
	case "tmpfs":
		return nil
	default:
		return newCreateErr(403, "isolator: mount type '%s' not allowed", t)
	}
}

