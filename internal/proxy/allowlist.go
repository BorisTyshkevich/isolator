package proxy

import (
	"net/url"
	"regexp"
	"strings"
)

// Endpoint allowlist regexes. All endpoint matching uses path component only
// (the caller strips the query string before matching).

var (
	// 6.1 System
	reSystemVersioned = regexp.MustCompile(`^/v[\d.]+/(_ping|version|info|system/df|system/events)$`)
	reAuth            = regexp.MustCompile(`^(/v[\d.]+)?/auth$`)

	// 6.2 Containers
	reContainersList   = regexp.MustCompile(`^/v[\d.]+/containers/json$`)
	reContainersCreate = regexp.MustCompile(`^/v[\d.]+/containers/create$`)
	reContainerAction  = regexp.MustCompile(`^/v[\d.]+/containers/[a-zA-Z0-9_.-]+/(start|stop|restart|kill|pause|unpause|wait|attach|logs|inspect|stats|top|changes|export|exec|json)(/.*)?$`)
	reContainerDelete  = regexp.MustCompile(`^/v[\d.]+/containers/[a-zA-Z0-9_.-]+$`)
	reExecAction       = regexp.MustCompile(`^/v[\d.]+/exec/[a-zA-Z0-9_.-]+/(start|resize|json)$`)

	// 6.3 Images
	reImagesList   = regexp.MustCompile(`^/v[\d.]+/images/json$`)
	reImagesCreate = regexp.MustCompile(`^/v[\d.]+/images/create$`)
	reImageInspect = regexp.MustCompile(`^/v[\d.]+/images/[a-zA-Z0-9_./:@%-]+/json$`)
	reImagePush    = regexp.MustCompile(`^/v[\d.]+/images/[a-zA-Z0-9_./:@%-]+/push$`)

	// 6.4 Networks
	reNetworksList    = regexp.MustCompile(`^/v[\d.]+/networks(/json)?$`)
	reNetworksCreate  = regexp.MustCompile(`^/v[\d.]+/networks/create$`)
	reNetworkInspect  = regexp.MustCompile(`^/v[\d.]+/networks/[a-zA-Z0-9_.-]+$`)
	reNetworkConnect  = regexp.MustCompile(`^/v[\d.]+/networks/[a-zA-Z0-9_.-]+/(connect|disconnect)$`)

	// 6.5 Build & Load
	reBuild      = regexp.MustCompile(`^/v[\d.]+/build$`)
	reImagesLoad = regexp.MustCompile(`^/v[\d.]+/images/load$`)

	// 6.6 Volumes
	reVolumes = regexp.MustCompile(`^/v[\d.]+/volumes(/.*)?$`)

	// 6.7 Events
	reEvents = regexp.MustCompile(`^/v[\d.]+/events$`)
)

// userIsoNetworkRegex returns the regex for the user's iso-<user> network
// and any sub-paths under it.
func userIsoNetworkRegex(user string) *regexp.Regexp {
	return regexp.MustCompile(`^/v[\d.]+/networks/iso-` + regexp.QuoteMeta(user) + `(/.*)?$`)
}

// IsEndpointAllowed reports whether (method, rawPathWithQuery) is in the
// allowlist for user. rawPathWithQuery is the request URI as received from
// the client; this function strips the query string before matching paths,
// but inspects the query for /images/create.
func IsEndpointAllowed(method, rawPathWithQuery, user string) bool {
	// Split off query string for path matching.
	path := rawPathWithQuery
	rawQuery := ""
	if i := strings.Index(rawPathWithQuery, "?"); i >= 0 {
		path = rawPathWithQuery[:i]
		rawQuery = rawPathWithQuery[i+1:]
	}

	// 6.1 System: unversioned _ping (HEAD or GET)
	if path == "/_ping" {
		return method == "HEAD" || method == "GET"
	}
	// 6.1 unversioned info
	if path == "/info" {
		return method == "GET"
	}
	// 6.1 auth (versioned or unversioned)
	if reAuth.MatchString(path) {
		return method == "POST"
	}
	// 6.1 system versioned: _ping, version, info, system/df, system/events
	if reSystemVersioned.MatchString(path) {
		return method == "GET"
	}

	// 6.2 Containers list
	if reContainersList.MatchString(path) {
		return method == "GET"
	}
	// 6.2 Containers create
	if reContainersCreate.MatchString(path) {
		return method == "POST"
	}
	// 6.2 Container action: start/stop/.../json
	if reContainerAction.MatchString(path) {
		// Method per Docker API: GET for inspect/logs/stats/top/changes/export/json,
		// POST for the rest. The Python prototype accepts any method that matches
		// the regex; we follow the same liberal stance.
		return method == "GET" || method == "POST"
	}
	// 6.2 Container delete
	if reContainerDelete.MatchString(path) {
		return method == "DELETE"
	}
	// 6.2 Exec action
	if reExecAction.MatchString(path) {
		return method == "GET" || method == "POST"
	}

	// 6.3 Images
	if reImagesList.MatchString(path) {
		return method == "GET"
	}
	if reImageInspect.MatchString(path) {
		return method == "GET"
	}
	if reImagesCreate.MatchString(path) {
		if method != "POST" {
			return false
		}
		return imagesCreateQueryAllowed(rawQuery)
	}
	if reImagePush.MatchString(path) {
		return method == "POST"
	}

	// 6.4 Networks
	if reNetworksCreate.MatchString(path) {
		return method == "POST"
	}
	if reNetworksList.MatchString(path) {
		return method == "GET"
	}
	if reNetworkConnect.MatchString(path) {
		return method == "POST"
	}
	// User's iso network: ALL methods allowed on /networks/iso-<user>[/...]
	if userIsoNetworkRegex(user).MatchString(path) {
		return true
	}
	if reNetworkInspect.MatchString(path) {
		// GET inspect, DELETE delete (any non-system network)
		return method == "GET" || method == "DELETE"
	}

	// 6.5 Build & Load
	if reBuild.MatchString(path) {
		return method == "POST"
	}
	if reImagesLoad.MatchString(path) {
		return method == "POST"
	}

	// 6.6 Volumes (full CRUD)
	if reVolumes.MatchString(path) {
		return true
	}

	// 6.7 Events
	if reEvents.MatchString(path) {
		return method == "GET"
	}

	return false
}

// imagesCreateQueryAllowed validates the query string for POST /images/create.
// 1. fromSrc -> blocked
// 2. unknown keys -> blocked
// 3. fromImage required and non-empty
func imagesCreateQueryAllowed(rawQuery string) bool {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return false
	}
	if _, ok := values["fromSrc"]; ok {
		return false
	}
	allowed := map[string]bool{"fromImage": true, "tag": true, "platform": true}
	for k := range values {
		if !allowed[k] {
			return false
		}
	}
	fromImage := values.Get("fromImage")
	if fromImage == "" {
		return false
	}
	return true
}
