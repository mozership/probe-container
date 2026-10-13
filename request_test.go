package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseRequest(t *testing.T) {
	r, err := parseRequest(map[string]any{
		"image":   "postgres:17",
		"name":    "db",
		"command": []any{"postgres", "-c", "max_connections=200", int64(1), 2.5, true},
		"env":     map[string]any{"POSTGRES_PASSWORD": "secret", "PGPORT": 5432, "DEBUG": false, "EMPTY": ""},
		"ports":   []any{5432, int64(5433), float64(5434), "5435", "53/udp", "8080:80", "0.0.0.0:9090:90/tcp", "[::1]:7070:70"},
		"volumes": []any{"/data:/var/lib/postgresql/data", "cache:/cache:ro"},
		"network": "testnet",
		"pull":    "never",
		"keep":    true,
		"wait": map[string]any{
			"port":        "5432",
			"log":         "ready",
			"healthcheck": true,
			"timeout":     "90s",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if r.image != "postgres:17" || r.name != "db" || r.network != "testnet" || r.pull != "never" || !r.keep {
		t.Errorf("request = %+v", r)
	}
	if want := []string{"postgres", "-c", "max_connections=200", "1", "2.5", "true"}; !reflect.DeepEqual(r.command, want) {
		t.Errorf("command = %q, want %q", r.command, want)
	}
	if want := []string{"DEBUG=false", "EMPTY=", "PGPORT=5432", "POSTGRES_PASSWORD=secret"}; !reflect.DeepEqual(r.env, want) {
		t.Errorf("env = %q, want %q", r.env, want)
	}
	wantPorts := []port{
		{spec: "127.0.0.1::5432", key: "5432"},
		{spec: "127.0.0.1::5433", key: "5433"},
		{spec: "127.0.0.1::5434", key: "5434"},
		{spec: "127.0.0.1::5435", key: "5435"},
		{spec: "127.0.0.1::53/udp", key: "53/udp"},
		{spec: "8080:80", key: "80"},
		{spec: "0.0.0.0:9090:90/tcp", key: "90"},
		{spec: "[::1]:7070:70", key: "70"},
	}
	if !reflect.DeepEqual(r.ports, wantPorts) {
		t.Errorf("ports = %+v, want %+v", r.ports, wantPorts)
	}
	if want := []string{"/data:/var/lib/postgresql/data", "cache:/cache:ro"}; !reflect.DeepEqual(r.volumes, want) {
		t.Errorf("volumes = %q, want %q", r.volumes, want)
	}
	if r.wait == nil || r.wait.port != "5432" || r.wait.log.String() != "ready" || !r.wait.healthy || r.wait.timeout != 90*time.Second {
		t.Errorf("wait = %+v", r.wait)
	}
}

func TestParseRequestDefaults(t *testing.T) {
	r, err := parseRequest(map[string]any{
		"image": "alpine", "name": nil, "command": nil, "env": nil, "ports": nil,
		"volumes": nil, "network": nil, "pull": nil, "wait": nil, "keep": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (&request{image: "alpine"}); !reflect.DeepEqual(r, want) {
		t.Errorf("request = %+v, want %+v", r, want)
	}

	r, err = parseRequest(map[string]any{"image": "alpine", "wait": map[string]any{"log": "up"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.wait.timeout != DefaultWaitTimeout || r.wait.port != "" || r.wait.healthy {
		t.Errorf("wait = %+v", r.wait)
	}
}

func TestParseRequestWaitTimeout(t *testing.T) {
	tests := []struct {
		in   any
		want time.Duration
	}{
		{"1m30s", 90 * time.Second},
		{5, 5 * time.Second},
		{int64(7), 7 * time.Second},
		{0.5, 500 * time.Millisecond},
	}
	for _, tt := range tests {
		r, err := parseRequest(map[string]any{"image": "alpine", "wait": map[string]any{"log": "up", "timeout": tt.in}})
		if err != nil {
			t.Errorf("timeout %v: %v", tt.in, err)
			continue
		}
		if r.wait.timeout != tt.want {
			t.Errorf("timeout %v = %s, want %s", tt.in, r.wait.timeout, tt.want)
		}
	}
}

func TestParseRequestErrors(t *testing.T) {
	tests := []struct {
		name string
		with map[string]any
		want string
	}{
		{"no image", map[string]any{}, "container action requires with.image"},
		{"an empty image", map[string]any{"image": ""}, "container action requires with.image"},
		{"an image that is not a string", map[string]any{"image": 3}, "with.image must be a string"},
		{"an image that is an option", map[string]any{"image": "--privileged"}, "would be read as an option"},
		{"a key it does not take", map[string]any{"image": "a", "runtime": "podman"}, "container action takes image, name, command, env, ports, volumes, network, pull, wait, keep, not runtime"},
		{"a name that is not a string", map[string]any{"image": "a", "name": []any{"x"}}, "with.name must be a string"},
		{"a network that is not a string", map[string]any{"image": "a", "network": true}, "with.network must be a string"},
		{"a pull that is not a string", map[string]any{"image": "a", "pull": true}, "with.pull must be a string"},
		{"a pull it does not know", map[string]any{"image": "a", "pull": "if-not-present"}, "with.pull must be one of missing, always, never, not if-not-present"},
		{"a command that is a string", map[string]any{"image": "a", "command": "sleep 1"}, "with.command must be a list, with one entry for each word"},
		{"a command with a list in it", map[string]any{"image": "a", "command": []any{"sh", []any{"-c"}}}, "with.command[1] must be a string or a number"},
		{"volumes that are a map", map[string]any{"image": "a", "volumes": map[string]any{"/a": "/b"}}, "with.volumes must be a list"},
		{"a volume that is nothing", map[string]any{"image": "a", "volumes": []any{nil}}, "with.volumes[0] must be a string or a number"},
		{"an env that is a list", map[string]any{"image": "a", "env": []any{"A=1"}}, "with.env must be a map of names to values"},
		{"an env with a name that has an equals sign", map[string]any{"image": "a", "env": map[string]any{"A=B": "c"}}, `with.env has the name "A=B"`},
		{"an env with no name", map[string]any{"image": "a", "env": map[string]any{"": "c"}}, `with.env has the name ""`},
		{"an env with a list for a value", map[string]any{"image": "a", "env": map[string]any{"A": []any{1}}}, "with.env.A must be a string, a number or a boolean"},
		{"an env with nothing for a value", map[string]any{"image": "a", "env": map[string]any{"A": nil}}, "with.env.A must be a string, a number or a boolean"},
		{"ports that are a number", map[string]any{"image": "a", "ports": 80}, "with.ports must be a list"},
		{"a port that is a boolean", map[string]any{"image": "a", "ports": []any{true}}, "with.ports[0] must be a port of the container"},
		{"a port with a fraction", map[string]any{"image": "a", "ports": []any{80.5}}, "with.ports[0] must be a port of the container"},
		{"a port that is no number", map[string]any{"image": "a", "ports": []any{"http"}}, `with.ports[0]: "http" does not name a port of the container`},
		{"a port of zero", map[string]any{"image": "a", "ports": []any{0}}, "does not name a port of the container"},
		{"a port beyond the last", map[string]any{"image": "a", "ports": []any{65536}}, "does not name a port of the container"},
		{"a published port without one of the container", map[string]any{"image": "a", "ports": []any{"8080:"}}, `with.ports[0]: "8080:" does not name a port of the container`},
		{"a port with a protocol it does not know", map[string]any{"image": "a", "ports": []any{"80/http"}}, "has the protocol http, which is not one of tcp, udp, sctp"},
		{"a port listed twice", map[string]any{"image": "a", "ports": []any{80, "8080:80/tcp"}}, "with.ports[1]: the port 80 of the container is listed twice"},
		{"a keep that is a string", map[string]any{"image": "a", "keep": "yes"}, "with.keep must be true or false"},
		{"a wait that is a string", map[string]any{"image": "a", "wait": "ready"}, "with.wait must be a map with any of port, log, healthcheck, timeout"},
		{"a wait with a key it does not take", map[string]any{"image": "a", "wait": map[string]any{"http": "/"}}, "with.wait takes port, log, healthcheck, timeout, not http"},
		{"a wait for nothing", map[string]any{"image": "a", "wait": map[string]any{"timeout": "5s"}}, "with.wait names nothing to wait for"},
		{"a wait for no health check", map[string]any{"image": "a", "wait": map[string]any{"healthcheck": false}}, "with.wait names nothing to wait for"},
		{"a wait for a port that is not published", map[string]any{"image": "a", "ports": []any{80}, "wait": map[string]any{"port": 81}}, "with.wait.port 81 is not a port of the container that with.ports publishes"},
		{"a wait for a port that is not TCP", map[string]any{"image": "a", "ports": []any{"53/udp"}, "wait": map[string]any{"port": "53/udp"}}, "with.wait.port 53/udp is not a TCP port"},
		{"a wait for a port that is a list", map[string]any{"image": "a", "wait": map[string]any{"port": []any{80}}}, "with.wait.port must be one of with.ports"},
		{"a wait for a log that is no expression", map[string]any{"image": "a", "wait": map[string]any{"log": "("}}, "with.wait.log: error parsing regexp"},
		{"a wait for an empty log", map[string]any{"image": "a", "wait": map[string]any{"log": ""}}, "with.wait.log must be a regular expression"},
		{"a wait for a log that is a number", map[string]any{"image": "a", "wait": map[string]any{"log": 5}}, "with.wait.log must be a regular expression"},
		{"a wait for a health check that is a string", map[string]any{"image": "a", "wait": map[string]any{"healthcheck": "yes"}}, "with.wait.healthcheck must be true or false"},
		{"a wait with a timeout that is no duration", map[string]any{"image": "a", "wait": map[string]any{"log": "x", "timeout": "soon"}}, "with.wait.timeout:"},
		{"a wait with a timeout that is a boolean", map[string]any{"image": "a", "wait": map[string]any{"log": "x", "timeout": true}}, "with.wait.timeout must be a duration or a number of seconds"},
		{"a wait with no time", map[string]any{"image": "a", "wait": map[string]any{"log": "x", "timeout": 0}}, "with.wait.timeout must be more than nothing"},
		{"a wait with less than no time", map[string]any{"image": "a", "wait": map[string]any{"log": "x", "timeout": "-1s"}}, "with.wait.timeout must be more than nothing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRequest(tt.with)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one with %q", err, tt.want)
			}
		})
	}
}

func TestParseBindings(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want map[string]binding
	}{
		{"nothing published", "", map[string]binding{}},
		{"a port on the loopback address", "5432/tcp -> 127.0.0.1:51110\n", map[string]binding{"5432": {"127.0.0.1", 51110}}},
		{"a port on every address is reached at the loopback one", "80/tcp -> 0.0.0.0:8080\n", map[string]binding{"80": {"127.0.0.1", 8080}}},
		{"IPv4 before IPv6", "80/tcp -> 0.0.0.0:8080\n80/tcp -> [::]:8081\n", map[string]binding{"80": {"127.0.0.1", 8080}}},
		{"IPv6 before IPv4", "80/tcp -> [::1]:8081\n80/tcp -> 192.0.2.1:8080\n", map[string]binding{"80": {"192.0.2.1", 8080}}},
		{"IPv6 alone", "80/tcp -> [::1]:8081\n", map[string]binding{"80": {"::1", 8081}}},
		{"every IPv6 address alone", "80/tcp -> [::]:8081\n", map[string]binding{"80": {"127.0.0.1", 8081}}},
		{"another protocol keeps its name", "53/udp -> 127.0.0.1:5353\n53/tcp -> 127.0.0.1:5354\n", map[string]binding{"53/udp": {"127.0.0.1", 5353}, "53": {"127.0.0.1", 5354}}},
		{"lines it cannot read are passed over", "warning\n80/tcp -> nowhere\n80/tcp -> 127.0.0.1:http\n\n  81/tcp -> 127.0.0.1:1  \n", map[string]binding{"81": {"127.0.0.1", 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseBindings(tt.out); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseBindings = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBindingAddr(t *testing.T) {
	if got := (binding{"127.0.0.1", 80}).addr(); got != "127.0.0.1:80" {
		t.Errorf("addr = %s", got)
	}
	if got := (binding{"::1", 80}).addr(); got != "[::1]:80" {
		t.Errorf("addr = %s", got)
	}
}

func TestTailAndLastLine(t *testing.T) {
	if got := tail("a\n\n b \nc\n", 2); got != "b; c" {
		t.Errorf("tail = %q", got)
	}
	if got := tail("\n \n", 3); got != "" {
		t.Errorf("tail of nothing = %q", got)
	}
	if got := lastLine("pulling\nabc123\n\n"); got != "abc123" {
		t.Errorf("lastLine = %q", got)
	}
	if got := lastLine(""); got != "" {
		t.Errorf("lastLine of nothing = %q", got)
	}
}
