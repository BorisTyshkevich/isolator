package proxy

import (
	"encoding/json"
	"strings"
)

// CheckNetworkCreate validates and rewrites a /networks/create body for user.
// Returns rewritten bytes ready to forward upstream, or a *CreateError.
//
// Strategy: parse to map[string]interface{}, mutate in place (preserve unknown
// fields like IPAM, Internal, Attachable, EnableIPv6, Options). See §6.4.
func CheckNetworkCreate(body []byte, user string) ([]byte, error) {
	if len(body) > MaxBodySize {
		return nil, newCreateErr(413, "isolator: request body too large (%d bytes)", len(body))
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, newCreateErr(400, "isolator: invalid JSON body")
	}

	// 1. Driver allowlist.
	if drvRaw, present := raw["Driver"]; present && drvRaw != nil {
		drv, ok := drvRaw.(string)
		if !ok {
			return nil, newCreateErr(400, "isolator: invalid Driver type")
		}
		if drv != "" && drv != "bridge" {
			return nil, newCreateErr(403, "isolator: network driver '%s' not allowed (use bridge)", drv)
		}
	}

	// 2. Name policy. Required and non-empty.
	nameRaw, _ := raw["Name"]
	name, _ := nameRaw.(string)
	if name == "" {
		return nil, newCreateErr(403, "isolator: network name required")
	}
	if strings.HasPrefix(name, "iso-") {
		ownPrefix := "iso-" + user
		if name != ownPrefix && !strings.HasPrefix(name, ownPrefix+"-") {
			return nil, newCreateErr(403, "isolator: network name '%s' reserved for another user", name)
		}
	}

	// 3. Block ConfigFrom (non-nil/non-empty).
	if cfRaw, present := raw["ConfigFrom"]; present && cfRaw != nil {
		switch cf := cfRaw.(type) {
		case map[string]interface{}:
			if len(cf) > 0 {
				return nil, newCreateErr(403, "isolator: ConfigFrom is not allowed")
			}
		default:
			return nil, newCreateErr(403, "isolator: ConfigFrom is not allowed")
		}
	}

	// 4. Owner label injection.
	labels, ok := raw["Labels"].(map[string]interface{})
	if !ok || labels == nil {
		labels = map[string]interface{}{}
		raw["Labels"] = labels
	}
	labels[OwnerLabel] = user

	// 5. Re-serialize.
	newBody, err := json.Marshal(raw)
	if err != nil {
		return nil, newCreateErr(400, "isolator: failed to re-serialize body: %v", err)
	}
	return newBody, nil
}
