package proxy

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCheckNetworkCreate(t *testing.T) {
	const user = "acm"

	cases := []struct {
		name        string
		body        string
		wantBlocked bool
		errSub      string
	}{
		{"clean bridge", `{"Name":"my-net","Driver":"bridge"}`, false, ""},
		{"missing driver", `{"Name":"my-net"}`, false, ""},
		{"empty driver", `{"Name":"my-net","Driver":""}`, false, ""},
		{"host driver", `{"Name":"my-net","Driver":"host"}`, true, "network driver 'host' not allowed"},
		{"overlay driver", `{"Name":"my-net","Driver":"overlay"}`, true, "network driver 'overlay' not allowed"},
		{"macvlan driver", `{"Name":"my-net","Driver":"macvlan"}`, true, "network driver 'macvlan' not allowed"},
		{"ipvlan driver", `{"Name":"my-net","Driver":"ipvlan"}`, true, "network driver 'ipvlan' not allowed"},
		{"missing name", `{"Driver":"bridge"}`, true, "name required"},
		{"empty name", `{"Name":"","Driver":"bridge"}`, true, "name required"},
		{"iso-acm name", `{"Name":"iso-acm","Driver":"bridge"}`, false, ""},
		{"iso-acm-suffix", `{"Name":"iso-acm-test","Driver":"bridge"}`, false, ""},
		{"iso-other name", `{"Name":"iso-other","Driver":"bridge"}`, true, "reserved for another user"},
		{"iso-acmevil prefix", `{"Name":"iso-acmevil","Driver":"bridge"}`, true, "reserved for another user"},
		{"testcontainers random", `{"Name":"tc-abc123","Driver":"bridge"}`, false, ""},
		{"ConfigFrom set", `{"Name":"my-net","ConfigFrom":{"Network":"other-net"}}`, true, "ConfigFrom is not allowed"},
		{"invalid JSON", `not json`, true, "invalid JSON body"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := CheckNetworkCreate([]byte(tc.body), user)
			if tc.wantBlocked {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil; out=%s", tc.errSub, out)
				}
				if !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
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

func TestCheckNetworkCreateOwnerMerge(t *testing.T) {
	body := `{"Name":"my-net","Labels":{"env":"test"}}`
	out, err := CheckNetworkCreate([]byte(body), "acm")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	_ = json.Unmarshal(out, &parsed)
	labels := parsed["Labels"].(map[string]interface{})
	if labels["env"] != "test" || labels[OwnerLabel] != "acm" {
		t.Errorf("merge failed: %v", labels)
	}
}

// TestCheckNetworkCreatePassthrough verifies that IPAM, Internal, Attachable,
// and unknown future fields survive the round-trip exactly.
func TestCheckNetworkCreatePassthrough(t *testing.T) {
	body := `{
		"Name":"my-net",
		"Driver":"bridge",
		"IPAM":{"Config":[{"Subnet":"10.99.0.0/24"}]},
		"Internal":true,
		"Attachable":true,
		"FutureField":{"x":1}
	}`
	out, err := CheckNetworkCreate([]byte(body), "acm")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	wantIPAM := map[string]interface{}{
		"Config": []interface{}{
			map[string]interface{}{"Subnet": "10.99.0.0/24"},
		},
	}
	if !reflect.DeepEqual(parsed["IPAM"], wantIPAM) {
		t.Errorf("IPAM not preserved: %v", parsed["IPAM"])
	}
	if parsed["Internal"] != true {
		t.Errorf("Internal not preserved")
	}
	if parsed["Attachable"] != true {
		t.Errorf("Attachable not preserved")
	}
	wantFuture := map[string]interface{}{"x": float64(1)}
	if !reflect.DeepEqual(parsed["FutureField"], wantFuture) {
		t.Errorf("FutureField not preserved: %v", parsed["FutureField"])
	}
}

// TestCheckNetworkCreateBodyTooLarge verifies the >16 MB rejection path.
func TestCheckNetworkCreateBodyTooLarge(t *testing.T) {
	big := make([]byte, MaxBodySize+1)
	for i := range big {
		big[i] = 'a'
	}
	_, err := CheckNetworkCreate(big, "acm")
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CreateError
	if !errors.As(err, &ce) || ce.Status != 413 {
		t.Errorf("expected 413, got %+v", err)
	}
}
