package proxy

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCheckCreate(t *testing.T) {
	const user = "acm"

	cases := []struct {
		name        string
		body        string
		wantBlocked bool
		wantErrSub  string // substring expected in error message when blocked
	}{
		{"clean create", `{"Image":"alpine"}`, false, ""},
		{"privileged true", `{"HostConfig":{"Privileged":true}}`, true, "Privileged is not allowed"},
		{"cap add", `{"HostConfig":{"CapAdd":["NET_ADMIN"]}}`, true, "CapAdd is not allowed"},
		{"cap drop", `{"HostConfig":{"CapDrop":["ALL"]}}`, true, "CapDrop is not allowed"},
		{"devices", `{"HostConfig":{"Devices":[{"PathOnHost":"/dev/sda"}]}}`, true, "Devices is not allowed"},
		{"dns", `{"HostConfig":{"DNS":["8.8.8.8"]}}`, true, "DNS is not allowed"},
		{"dns options", `{"HostConfig":{"DNSOptions":["ndots:5"]}}`, true, "DNSOptions is not allowed"},
		{"dns search", `{"HostConfig":{"DNSSearch":["evil.com"]}}`, true, "DNSSearch is not allowed"},
		{"pid mode host", `{"HostConfig":{"PidMode":"host"}}`, true, "PidMode is not allowed"},
		{"ipc mode host", `{"HostConfig":{"IpcMode":"host"}}`, true, "IpcMode is not allowed"},
		{"uts mode host", `{"HostConfig":{"UTSMode":"host"}}`, true, "UTSMode is not allowed"},
		{"userns mode host", `{"HostConfig":{"UsernsMode":"host"}}`, true, "UsernsMode is not allowed"},
		{"cgroupns mode host", `{"HostConfig":{"CgroupnsMode":"host"}}`, true, "CgroupnsMode is not allowed"},
		{"security opt", `{"HostConfig":{"SecurityOpt":["seccomp=unconfined"]}}`, true, "SecurityOpt is not allowed"},
		{"sysctls", `{"HostConfig":{"Sysctls":{"net.ipv4.ip_forward":"1"}}}`, true, "Sysctls is not allowed"},
		{"ulimits", `{"HostConfig":{"Ulimits":[{"Name":"nofile","Soft":1024}]}}`, true, "Ulimits is not allowed"},
		{"runtime", `{"HostConfig":{"Runtime":"nvidia"}}`, true, "Runtime is not allowed"},
		{"oom score adj nonzero", `{"HostConfig":{"OomScoreAdj":-500}}`, true, "OomScoreAdj is not allowed"},
		{"oom score adj zero", `{"HostConfig":{"OomScoreAdj":0}}`, false, ""},
		{"oom kill disable true", `{"HostConfig":{"OomKillDisable":true}}`, true, "OomKillDisable is not allowed"},
		{"oom kill disable false", `{"HostConfig":{"OomKillDisable":false}}`, false, ""},
		{"privileged false", `{"HostConfig":{"Privileged":false}}`, false, ""},
		{"volumes from", `{"HostConfig":{"VolumesFrom":["other_container"]}}`, true, "VolumesFrom is not allowed"},
		{"device cgroup rules", `{"HostConfig":{"DeviceCgroupRules":["c 1:3 rmw"]}}`, true, "DeviceCgroupRules is not allowed"},
		{"device requests", `{"HostConfig":{"DeviceRequests":[{"Count":-1}]}}`, true, "DeviceRequests is not allowed"},
		{"cgroup parent", `{"HostConfig":{"CgroupParent":"/system.slice"}}`, true, "CgroupParent is not allowed"},
		{"links", `{"HostConfig":{"Links":["db:db"]}}`, true, "Links is not allowed"},

		{"extra hosts valid", `{"HostConfig":{"ExtraHosts":["host.docker.internal:host-gateway"]}}`, false, ""},
		{"extra hosts evil", `{"HostConfig":{"ExtraHosts":["evil:1.2.3.4"]}}`, true, "ExtraHosts entry"},
		{"extra hosts mixed", `{"HostConfig":{"ExtraHosts":["host.docker.internal:host-gateway","evil:1.2.3.4"]}}`, true, "ExtraHosts entry"},

		{"net mode iso-acm", `{"HostConfig":{"NetworkMode":"iso-acm"}}`, false, ""},
		{"net mode iso-other", `{"HostConfig":{"NetworkMode":"iso-slot-0"}}`, true, "NetworkMode"},
		{"net mode host", `{"HostConfig":{"NetworkMode":"host"}}`, true, "NetworkMode"},
		{"net mode empty", `{"HostConfig":{"NetworkMode":""}}`, false, ""},
		{"net mode default", `{"HostConfig":{"NetworkMode":"default"}}`, false, ""},
		{"net mode bridge", `{"HostConfig":{"NetworkMode":"bridge"}}`, false, ""},

		{"endpoints iso-acm", `{"NetworkingConfig":{"EndpointsConfig":{"iso-acm":{}}}}`, false, ""},
		{"endpoints other", `{"NetworkingConfig":{"EndpointsConfig":{"iso-other":{}}}}`, true, "network 'iso-other' not allowed"},

		{"bind valid ws", `{"HostConfig":{"Binds":["/Users/Workspaces/acm/project:/work"]}}`, false, ""},
		{"bind etc passwd", `{"HostConfig":{"Binds":["/etc/passwd:/etc/passwd"]}}`, true, "bind mount not allowed"},
		{"bind other user", `{"HostConfig":{"Binds":["/Users/Workspaces/other/x:/x"]}}`, true, "bind mount not allowed"},
		{"bind shared tmp", `{"HostConfig":{"Binds":["/tmp/x:/x"]}}`, true, "bind mount not allowed"},
		{"bind named volume", `{"HostConfig":{"Binds":["named-vol:/x"]}}`, true, "named volume"},
		{"bind /var/run/docker.sock", `{"HostConfig":{"Binds":["/var/run/docker.sock:/var/run/docker.sock"]}}`, true, "bind mount not allowed"},
		{"bind docker.sock inside ws", `{"HostConfig":{"Binds":["/Users/Workspaces/acm/docker.sock:/x"]}}`, true, "Docker socket"},

		{"mount bind ws", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/Users/Workspaces/acm/project","Target":"/work"}]}}`, false, ""},
		{"mount bind etc", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/etc","Target":"/etc"}]}}`, true, "bind mount not allowed"},
		{"mount bind docker.sock", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/Users/Workspaces/acm/docker.sock","Target":"/x"}]}}`, true, "Docker socket"},
		{"mount volume", `{"HostConfig":{"Mounts":[{"Type":"volume","Source":"myvol","Target":"/data"}]}}`, true, "named volumes are not allowed"},
		{"mount tmpfs", `{"HostConfig":{"Mounts":[{"Type":"tmpfs","Target":"/tmp"}]}}`, false, ""},
		{"mount unknown", `{"HostConfig":{"Mounts":[{"Type":"npipe","Source":"x","Target":"/x"}]}}`, true, "mount type"},

		{"user root", `{"User":"root"}`, true, "container user"},
		{"user 0", `{"User":"0"}`, true, "container user"},
		{"user 0:0", `{"User":"0:0"}`, true, "container user"},
		{"user 0:1000", `{"User":"0:1000"}`, true, "container user"},
		{"user 1000", `{"User":"1000"}`, false, ""},
		{"user 1000:1000", `{"User":"1000:1000"}`, false, ""},
		{"user empty", `{"User":""}`, false, ""},
		{"user wrong type", `{"User":0}`, true, "invalid User field type"},

		{"multi binds one bad", `{"HostConfig":{"Binds":["/Users/Workspaces/acm/ok:/a","/etc/bad:/b"]}}`, true, "bind mount not allowed"},
		{"invalid json", `not json at all`, true, "invalid JSON body"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := CheckCreate([]byte(tc.body), user)
			if tc.wantBlocked {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil; out=%s", tc.wantErrSub, out)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Ensure owner label is injected on success.
			var parsed map[string]interface{}
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatalf("output not valid JSON: %v", err)
			}
			labels, _ := parsed["Labels"].(map[string]interface{})
			if got, _ := labels[OwnerLabel].(string); got != user {
				t.Errorf("owner label = %q, want %q", got, user)
			}
		})
	}
}

// TestCheckCreateOwnerLabelMerge verifies that pre-existing labels are
// preserved when the owner label is injected.
func TestCheckCreateOwnerLabelMerge(t *testing.T) {
	body := `{"Image":"alpine","Labels":{"env":"test"}}`
	out, err := CheckCreate([]byte(body), "acm")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	labels, _ := parsed["Labels"].(map[string]interface{})
	if labels["env"] != "test" {
		t.Errorf("env label dropped: %v", labels)
	}
	if labels[OwnerLabel] != "acm" {
		t.Errorf("owner label missing: %v", labels)
	}
}

// TestCheckCreateBindRewrite confirms the resolved (Clean) source is written
// back into the bind string.
func TestCheckCreateBindRewrite(t *testing.T) {
	// Use a path with redundant ./ that Clean will normalize. The path
	// doesn't need to exist — EvalSymlinks falls back to Clean output.
	body := `{"HostConfig":{"Binds":["/Users/Workspaces/acm/./project:/work"]}}`
	out, err := CheckCreate([]byte(body), "acm")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	hc := parsed["HostConfig"].(map[string]interface{})
	binds := hc["Binds"].([]interface{})
	if got := binds[0].(string); !strings.HasPrefix(got, "/Users/Workspaces/acm/project:") {
		t.Errorf("bind not rewritten to clean path: %q", got)
	}
}

// TestCheckCreateFidelityTopLevel verifies that unknown top-level fields
// survive the round-trip exactly. See §9.9.
func TestCheckCreateFidelityTopLevel(t *testing.T) {
	body := `{
		"Image":"alpine",
		"Healthcheck":{"Test":["CMD","true"]},
		"StopSignal":"SIGUSR1",
		"FutureField":{"x":1}
	}`
	out, err := CheckCreate([]byte(body), "acm")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	want := map[string]interface{}{
		"Healthcheck": map[string]interface{}{"Test": []interface{}{"CMD", "true"}},
		"StopSignal":  "SIGUSR1",
		"FutureField": map[string]interface{}{"x": float64(1)},
	}
	for k, v := range want {
		if !reflect.DeepEqual(parsed[k], v) {
			t.Errorf("field %s: got %v, want %v", k, parsed[k], v)
		}
	}
}

// TestCheckCreateFidelityHostConfig verifies that unknown HostConfig fields
// (e.g. Tmpfs, FutureHC) are preserved.
func TestCheckCreateFidelityHostConfig(t *testing.T) {
	body := `{"Image":"alpine","HostConfig":{"Tmpfs":{"/run":""},"FutureHC":42}}`
	out, err := CheckCreate([]byte(body), "acm")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	hc := parsed["HostConfig"].(map[string]interface{})
	if !reflect.DeepEqual(hc["Tmpfs"], map[string]interface{}{"/run": ""}) {
		t.Errorf("Tmpfs not preserved: %v", hc["Tmpfs"])
	}
	if hc["FutureHC"] != float64(42) {
		t.Errorf("FutureHC not preserved: %v", hc["FutureHC"])
	}
}

// TestCheckCreateBindSymlinkOutsideWorkspace creates a real symlink within a
// fake workspace pointing outside, then confirms CheckCreate rejects it.
func TestCheckCreateBindSymlinkOutsideWorkspace(t *testing.T) {
	tmp := t.TempDir()
	prevWS, prevHome := WorkspacesDir, UsersHomePrefix
	WorkspacesDir = tmp + "/Workspaces"
	UsersHomePrefix = tmp + "/users"
	t.Cleanup(func() {
		WorkspacesDir = prevWS
		UsersHomePrefix = prevHome
	})

	wsUser := WorkspacesDir + "/acm"
	if err := makeAllDirs(wsUser); err != nil {
		t.Fatal(err)
	}
	link := wsUser + "/escape"
	if err := makeSymlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	body := `{"HostConfig":{"Binds":["` + link + `:/x"]}}`
	_, err := CheckCreate([]byte(body), "acm")
	if err == nil {
		t.Fatal("expected blocked, got nil")
	}
	var ce *CreateError
	if !errors.As(err, &ce) || ce.Status != 403 {
		t.Fatalf("expected 403, got %+v", err)
	}
	if !strings.Contains(err.Error(), "bind mount not allowed") {
		t.Errorf("wrong message: %v", err)
	}
}
