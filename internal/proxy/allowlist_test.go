package proxy

import "testing"

func TestIsEndpointAllowed(t *testing.T) {
	const user = "acm"
	cases := []struct {
		name   string
		method string
		path   string // includes raw query if relevant
		want   bool
	}{
		// 6.1 System
		{"head ping unversioned", "HEAD", "/_ping", true},
		{"get ping unversioned", "GET", "/_ping", true},
		{"post ping unversioned", "POST", "/_ping", false},
		{"get ping versioned", "GET", "/v1.51/_ping", true},
		{"head ping versioned blocked", "HEAD", "/v1.51/_ping", false},
		{"get version", "GET", "/v1.51/version", true},
		{"get info versioned", "GET", "/v1.51/info", true},
		{"get info unversioned", "GET", "/info", true},
		{"post info blocked", "POST", "/info", false},
		{"system df", "GET", "/v1.51/system/df", true},
		{"system events", "GET", "/v1.51/system/events", true},
		{"auth unversioned", "POST", "/auth", true},
		{"auth versioned", "POST", "/v1.51/auth", true},
		{"get auth blocked", "GET", "/auth", false},

		// 6.2 Containers
		{"containers list", "GET", "/v1.51/containers/json", true},
		{"containers create", "POST", "/v1.51/containers/create", true},
		{"container start", "POST", "/v1.51/containers/abc123/start", true},
		{"container stop", "POST", "/v1.51/containers/abc123/stop", true},
		{"container restart", "POST", "/v1.51/containers/abc123/restart", true},
		{"container kill", "POST", "/v1.51/containers/abc123/kill", true},
		{"container pause", "POST", "/v1.51/containers/abc123/pause", true},
		{"container unpause", "POST", "/v1.51/containers/abc123/unpause", true},
		{"container wait", "POST", "/v1.51/containers/abc123/wait", true},
		{"container attach", "POST", "/v1.51/containers/abc123/attach", true},
		{"container logs", "GET", "/v1.51/containers/abc123/logs", true},
		{"container json", "GET", "/v1.51/containers/abc123/json", true},
		{"container stats", "GET", "/v1.51/containers/abc123/stats", true},
		{"container top", "GET", "/v1.51/containers/abc123/top", true},
		{"container changes", "GET", "/v1.51/containers/abc123/changes", true},
		{"container export", "GET", "/v1.51/containers/abc123/export", true},
		{"container exec", "POST", "/v1.51/containers/abc123/exec", true},
		{"container delete", "DELETE", "/v1.51/containers/abc123", true},
		{"container update blocked", "POST", "/v1.51/containers/abc123/update", false},
		{"exec json", "GET", "/v1.51/exec/exec123/json", true},
		{"exec start", "POST", "/v1.51/exec/exec123/start", true},
		{"exec resize", "POST", "/v1.51/exec/exec123/resize", true},

		// 6.3 Images
		{"image inspect", "GET", "/v1.51/images/alpine/json", true},
		{"image inspect repo tag", "GET", "/v1.51/images/myrepo/myimage:latest/json", true},
		{"image list", "GET", "/v1.51/images/json", true},
		{"images create pull", "POST", "/v1.51/images/create?fromImage=alpine&tag=latest", true},
		{"images create no tag", "POST", "/v1.51/images/create?fromImage=alpine", true},
		{"images create platform", "POST", "/v1.51/images/create?fromImage=alpine&platform=linux/amd64", true},
		{"images create fromSrc blocked", "POST", "/v1.51/images/create?fromSrc=http://evil.com/rootkit.tar", false},
		{"images create both keys", "POST", "/v1.51/images/create?fromImage=alpine&fromSrc=x", false},
		{"images create no fromImage", "POST", "/v1.51/images/create", false},
		{"images create empty fromImage", "POST", "/v1.51/images/create?fromImage=", false},
		{"images create extra key", "POST", "/v1.51/images/create?fromImage=alpine&repo=evil", false},
		{"image push", "POST", "/v1.51/images/alpine/push", true},

		// 6.4 Networks
		{"networks list", "GET", "/v1.51/networks", true},
		{"networks list json", "GET", "/v1.51/networks/json", true},
		{"network inspect bridge", "GET", "/v1.51/networks/bridge", true},
		{"network inspect own", "GET", "/v1.51/networks/iso-acm", true},
		{"network connect own", "POST", "/v1.51/networks/iso-acm/connect", true},
		{"network delete own", "DELETE", "/v1.51/networks/iso-acm", true},
		{"network put own", "PUT", "/v1.51/networks/iso-acm", true},
		{"network connect other iso", "POST", "/v1.51/networks/iso-slot-0/connect", true},
		{"network delete other iso", "DELETE", "/v1.51/networks/iso-slot-0", true},
		{"network put other iso blocked", "PUT", "/v1.51/networks/iso-slot-0", false},
		{"network connect bridge", "POST", "/v1.51/networks/bridge/connect", true},
		{"network disconnect bridge", "POST", "/v1.51/networks/bridge/disconnect", true},
		{"networks create", "POST", "/v1.51/networks/create", true},

		// 6.5/6.6/6.7
		{"build", "POST", "/v1.51/build", true},
		{"images load", "POST", "/v1.51/images/load", true},
		{"volumes list", "GET", "/v1.51/volumes", true},
		{"volumes create", "POST", "/v1.51/volumes/create", true},
		{"volumes delete", "DELETE", "/v1.51/volumes/myvol", true},
		{"events", "GET", "/v1.51/events", true},

		// Blocked
		{"swarm init blocked", "POST", "/v1.51/swarm/init", false},
		{"swarm join blocked", "POST", "/v1.51/swarm/join", false},
		{"plugins pull blocked", "POST", "/v1.51/plugins/pull", false},
		{"grpc blocked", "POST", "/grpc", false},
		{"secrets blocked", "GET", "/v1.51/secrets", false},
		{"configs blocked", "GET", "/v1.51/configs", false},
		{"random path blocked", "GET", "/some/random/path", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsEndpointAllowed(tc.method, tc.path, user)
			if got != tc.want {
				t.Errorf("IsEndpointAllowed(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
			}
		})
	}
}
