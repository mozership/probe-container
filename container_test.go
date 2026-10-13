package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/mozership/probe/actionref"
	"github.com/mozership/probe/actionrpc"
)

// answer is what a stand-in for the runtime answers one command with.
type answer struct {
	stdout string
	stderr string
	err    error
}

// runtime stands in for the command containers are run with. It records
// what it is run with, and answers a command by its first word.
type runtime struct {
	mu    sync.Mutex
	calls [][]string
	names []string
	// answers are tried in order for each call of a subcommand; the last
	// one answers every call after it.
	answers map[string][]answer
	// on is called with the arguments, and answers when it returns true.
	on func(args []string) (answer, bool)
}

func (r *runtime) exec(_ context.Context, name string, args ...string) (string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
	r.calls = append(r.calls, slices.Clone(args))
	if r.on != nil {
		if a, ok := r.on(args); ok {
			return a.stdout, a.stderr, a.err
		}
	}
	list := r.answers[args[0]]
	if len(list) == 0 {
		return "", "", nil
	}
	a := list[0]
	if len(list) > 1 {
		r.answers[args[0]] = list[1:]
	}
	return a.stdout, a.stderr, a.err
}

// ran returns the calls of a subcommand, each as one line.
func (r *runtime) ran(sub string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if c[0] == sub {
			out = append(out, strings.Join(c, " "))
		}
	}
	return out
}

func newAction(rt *runtime, env map[string]string) *Action {
	return &Action{
		exec:   rt.exec,
		getenv: func(k string) string { return env[k] },
		poll:   time.Millisecond,
	}
}

// step is a call as Probe makes it for a step of the run "run1", under a
// Probe that cleans up.
func step(with map[string]any) actionrpc.Call {
	return actionrpc.Call{
		With:     with,
		Step:     actionrpc.Step{RunID: "run1"},
		Cleanups: &actionrpc.Cleanups{},
	}
}

func started() map[string][]answer {
	return map[string][]answer{
		"run":     {{stdout: "abc123\n"}},
		"inspect": {{stdout: "/web\n"}},
		"port":    {{stdout: "80/tcp -> 127.0.0.1:32768\n"}},
	}
}

func TestRunStepStartsAContainerOfTheRun(t *testing.T) {
	rt := &runtime{answers: map[string][]answer{
		"run":     {{stdout: "Unable to find image\nabc123\n"}},
		"inspect": {{stdout: "/web\n"}},
		"port":    {{stdout: "80/tcp -> 0.0.0.0:32768\n80/tcp -> [::]:32768\n53/udp -> 127.0.0.1:32769\n"}},
	}}
	a := newAction(rt, nil)

	ret, state, err := a.RunStep(step(map[string]any{
		"image":     "nginx:1",
		"name":      "web",
		"command":   []any{"nginx", "-g", "daemon off;"},
		"env":       map[string]any{"B": "two words", "A": int64(1), "C": true},
		"ports":     []any{float64(80), "53/udp"},
		"volumes":   []any{"/src:/dst:ro"},
		"network":   "testnet",
		"pull":      "always",
		"labels":    map[string]any{"suite": "orders"},
		"add_hosts": []any{"host.docker.internal:host-gateway"},
		"init":      true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Errorf("state = %v, want none", state)
	}

	wantArgs := []string{
		"run", "--detach", "--label", "probe.run_id=run1",
		"--name", "web", "--pull", "always", "--network", "testnet",
		"--env", "A=1", "--env", "B=two words", "--env", "C=true",
		"--publish", "127.0.0.1::80", "--publish", "127.0.0.1::53/udp",
		"--volume", "/src:/dst:ro",
		"--label", "suite=orders",
		"--add-host", "host.docker.internal:host-gateway",
		"--init",
		"nginx:1", "nginx", "-g", "daemon off;",
	}
	if got := rt.calls[0]; !slices.Equal(got, wantArgs) {
		t.Errorf("run args =\n %q\nwant\n %q", got, wantArgs)
	}
	if rt.names[0] != "docker" {
		t.Errorf("ran %s, want docker", rt.names[0])
	}

	wantRes := map[string]any{
		"id":    "abc123",
		"name":  "web",
		"host":  "127.0.0.1",
		"port":  32768,
		"addr":  "127.0.0.1:32768",
		"ports": map[string]any{"80": 32768, "53/udp": 32769},
	}
	if got := ret["res"]; !reflect.DeepEqual(got, wantRes) {
		t.Errorf("res = %v, want %v", got, wantRes)
	}
	wantReq := map[string]any{
		"image":     "nginx:1",
		"name":      "web",
		"network":   "testnet",
		"pull":      "always",
		"keep":      false,
		"command":   []any{"nginx", "-g", "daemon off;"},
		"volumes":   []any{"/src:/dst:ro"},
		"env":       []any{"A", "B", "C"},
		"ports":     []any{"80", "53/udp"},
		"labels":    map[string]any{"suite": "orders"},
		"add_hosts": []any{"host.docker.internal:host-gateway"},
		"init":      true,
	}
	if got := ret["req"]; !reflect.DeepEqual(got, wantReq) {
		t.Errorf("req = %v, want %v", got, wantReq)
	}
	if ret["status"] != 0 {
		t.Errorf("status = %v, want 0", ret["status"])
	}
	if _, err := actionrpc.Sendable(ret); err != nil {
		t.Errorf("the result cannot be sent to Probe: %v", err)
	}
}

func TestRunStepWithoutPortsAsksForNone(t *testing.T) {
	rt := &runtime{answers: started()}
	ret, _, err := newAction(rt, nil).RunStep(step(map[string]any{"image": "alpine"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.ran("port"); len(got) != 0 {
		t.Errorf("asked for the ports: %v", got)
	}
	want := map[string]any{"id": "abc123", "name": "web", "ports": map[string]any{}}
	if got := ret["res"]; !reflect.DeepEqual(got, want) {
		t.Errorf("res = %v, want %v", got, want)
	}
}

func TestRunStepKeepsAContainer(t *testing.T) {
	tests := []struct {
		name     string
		with     map[string]any
		env      map[string]string
		cleanups *actionrpc.Cleanups
		want     bool
	}{
		{name: "by default it is removed", cleanups: &actionrpc.Cleanups{}},
		{name: "with.keep", with: map[string]any{"keep": true}, cleanups: &actionrpc.Cleanups{}, want: true},
		{name: "with.keep false", with: map[string]any{"keep": false}, cleanups: &actionrpc.Cleanups{}},
		{name: "the environment", env: map[string]string{envKeep: "1"}, cleanups: &actionrpc.Cleanups{}, want: true},
		{name: "the environment over with.keep false", with: map[string]any{"keep": false}, env: map[string]string{envKeep: "true"}, cleanups: &actionrpc.Cleanups{}, want: true},
		{name: "the environment set to false", env: map[string]string{envKeep: "0"}, cleanups: &actionrpc.Cleanups{}},
		{name: "the environment set to what is neither", env: map[string]string{envKeep: "yes please"}, cleanups: &actionrpc.Cleanups{}, want: true},
		{name: "with.keep under a Probe that does not clean up", with: map[string]any{"keep": true}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &runtime{answers: started()}
			with := map[string]any{"image": "alpine"}
			for k, v := range tt.with {
				with[k] = v
			}
			call := step(with)
			call.Cleanups = tt.cleanups
			ret, _, err := newAction(rt, tt.env).RunStep(call)
			if err != nil {
				t.Fatal(err)
			}
			labelled := strings.Contains(rt.ran("run")[0], "--label probe.keep=true")
			if labelled != tt.want {
				t.Errorf("labelled to be kept = %v, want %v", labelled, tt.want)
			}
			if got := ret["req"].(map[string]any)["keep"]; got != tt.want {
				t.Errorf("req.keep = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunStepRefusesToLeaveAContainerForGood(t *testing.T) {
	rt := &runtime{answers: started()}
	a := newAction(rt, nil)

	call := step(map[string]any{"image": "alpine"})
	call.Cleanups = nil
	_, _, err := a.RunStep(call)
	if err == nil || !strings.Contains(err.Error(), "does not clean up") {
		t.Errorf("under a Probe that does not clean up: err = %v", err)
	}

	call = step(map[string]any{"image": "alpine"})
	call.Step.RunID = ""
	_, _, err = a.RunStep(call)
	if err == nil || !strings.Contains(err.Error(), "ID of the run") {
		t.Errorf("without a run: err = %v", err)
	}

	if _, err := a.Run(map[string]any{"image": "alpine"}); err == nil {
		t.Error("Run started a container outside a step")
	}
	if len(rt.calls) != 0 {
		t.Errorf("the runtime was run: %v", rt.calls)
	}
}

func TestRunStepUsesTheRuntimeOfTheEnvironment(t *testing.T) {
	rt := &runtime{answers: started()}
	a := newAction(rt, map[string]string{envRuntime: "podman"})
	if _, _, err := a.RunStep(step(map[string]any{"image": "alpine", "ports": []any{80}})); err != nil {
		t.Fatal(err)
	}
	for i, n := range rt.names {
		if n != "podman" {
			t.Errorf("call %d ran %s, want podman", i, n)
		}
	}
}

func TestRunStepFailures(t *testing.T) {
	tests := []struct {
		name    string
		answers map[string][]answer
		with    map[string]any
		want    string
	}{
		{
			name:    "the runtime does not start it",
			answers: map[string][]answer{"run": {{err: errors.New("exit status 125: no such image")}}},
			want:    "starting alpine: exit status 125: no such image",
		},
		{
			name:    "the runtime tells no ID",
			answers: map[string][]answer{"run": {{stdout: "\n"}}},
			want:    "did not tell the ID",
		},
		{
			name:    "the name cannot be read",
			answers: map[string][]answer{"run": {{stdout: "abc"}}, "inspect": {{err: errors.New("gone")}}},
			want:    "reading the name of the container: gone",
		},
		{
			name:    "the ports cannot be read",
			answers: map[string][]answer{"run": {{stdout: "abc"}}, "port": {{err: errors.New("gone")}}},
			with:    map[string]any{"ports": []any{80}},
			want:    "reading the ports of the container: gone",
		},
		{
			name:    "a port is not published",
			answers: map[string][]answer{"run": {{stdout: "abc"}}, "port": {{stdout: "81/tcp -> 127.0.0.1:1\n"}}},
			with:    map[string]any{"ports": []any{80}},
			want:    "the port 80 of the container is not published",
		},
		{
			name: "a parameter is wrong before anything is run",
			with: map[string]any{"pull": "sometimes"},
			want: "with.pull must be one of",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &runtime{answers: tt.answers}
			with := map[string]any{"image": "alpine"}
			for k, v := range tt.with {
				with[k] = v
			}
			_, _, err := newAction(rt, nil).RunStep(step(with))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one with %q", err, tt.want)
			}
		})
	}
}

func TestRunStepWaits(t *testing.T) {
	running := answer{stdout: "true 0 \n"}
	tests := []struct {
		name    string
		wait    map[string]any
		inspect []answer
		logs    []answer
		want    string
		// polls is the least number of times the state is read.
		polls int
	}{
		{
			name:    "for the log to match, on stdout or stderr",
			wait:    map[string]any{"log": "ready to (accept|take)"},
			inspect: []answer{running},
			logs:    []answer{{stdout: "starting\n"}, {stdout: "starting\n", stderr: "ready to accept connections\n"}},
			polls:   2,
		},
		{
			name:    "for the log to match across lines",
			wait:    map[string]any{"log": "(?s)ready.*ready"},
			inspect: []answer{running},
			logs:    []answer{{stdout: "ready\n"}, {stdout: "ready\nrestarting\nready\n"}},
			polls:   2,
		},
		{
			name:    "for the health check to pass",
			wait:    map[string]any{"healthcheck": true},
			inspect: []answer{{stdout: "true 0 starting\n"}, {stdout: "true 0 starting\n"}, {stdout: "true 0 healthy\n"}},
			polls:   3,
		},
		{
			name:    "and fails for an image without a health check",
			wait:    map[string]any{"healthcheck": true},
			inspect: []answer{running},
			want:    "the image has no health check",
		},
		{
			name:    "and fails once the container stops, with the end of its log",
			wait:    map[string]any{"log": "ready"},
			inspect: []answer{running, {stdout: "false 3 \n"}},
			logs:    []answer{{stdout: "starting\n"}, {stdout: "starting\n", stderr: "out of memory\n"}},
			want:    "the container stopped with exit status 3 before it was ready; its log ends with: starting; out of memory",
		},
		{
			name:    "and fails when its state cannot be read",
			wait:    map[string]any{"log": "ready"},
			inspect: []answer{{err: errors.New("no such container")}},
			want:    "reading the state of the container: no such container",
		},
		{
			name:    "and fails when the log never matches",
			wait:    map[string]any{"log": "ready", "timeout": "30ms"},
			inspect: []answer{running},
			logs:    []answer{{stdout: "still starting\n"}},
			want:    `the container was not ready after 30ms: its log does not match "ready"; its log ends with: still starting`,
		},
		{
			name:    "and fails when the health check never passes",
			wait:    map[string]any{"healthcheck": true, "timeout": 0.03},
			inspect: []answer{{stdout: "true 0 unhealthy\n"}},
			want:    "the container was not ready after 30ms: its health check is unhealthy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answers := started()
			// The first inspect reads the name.
			answers["inspect"] = append(answers["inspect"], tt.inspect...)
			answers["logs"] = tt.logs
			rt := &runtime{answers: answers}
			_, _, err := newAction(rt, nil).RunStep(step(map[string]any{"image": "alpine", "wait": tt.wait}))
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("err = %v, want one with %q", err, tt.want)
			}
			if got := len(rt.ran("inspect")) - 1; got < tt.polls {
				t.Errorf("the state was read %d times, want at least %d", got, tt.polls)
			}
		})
	}
}

// listen returns a listener on the loopback address and its port.
func listen(t *testing.T) (net.Listener, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return l, port
}

// accept handles every connection to l with handle.
func accept(l net.Listener, handle func(net.Conn)) {
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
}

func TestRunStepWaitsForAPort(t *testing.T) {
	hold := func(c net.Conn) {
		_, _ = c.Read(make([]byte, 1))
		_ = c.Close()
	}
	tests := []struct {
		name   string
		handle func(net.Conn)
		want   string
	}{
		{name: "that keeps a connection open", handle: hold},
		{name: "that greets", handle: func(c net.Conn) {
			_, _ = c.Write([]byte("220 ready\r\n"))
			_ = c.Close()
		}},
		{
			// What a runtime does on a published port while nothing in the
			// container listens.
			name:   "and fails for one that closes every connection at once",
			handle: func(c net.Conn) { _ = c.Close() },
			want:   "the container was not ready after 400ms: nothing takes a connection on its port 80",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, port := listen(t)
			accept(l, tt.handle)
			answers := started()
			answers["inspect"] = append(answers["inspect"], answer{stdout: "true 0 \n"})
			answers["port"] = []answer{{stdout: "80/tcp -> 127.0.0.1:" + port + "\n"}}
			rt := &runtime{answers: answers}
			_, _, err := newAction(rt, nil).RunStep(step(map[string]any{
				"image": "alpine",
				"ports": []any{80},
				"wait":  map[string]any{"port": 80, "timeout": "400ms"},
			}))
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("err = %v, want one with %q", err, tt.want)
			}
		})
	}
}

func TestRunStepWaitsForAPortNothingListensOn(t *testing.T) {
	l, port := listen(t)
	_ = l.Close()
	answers := started()
	answers["inspect"] = append(answers["inspect"], answer{stdout: "true 0 \n"})
	answers["port"] = []answer{{stdout: "80/tcp -> 127.0.0.1:" + port + "\n"}}
	_, _, err := newAction(&runtime{answers: answers}, nil).RunStep(step(map[string]any{
		"image": "alpine",
		"ports": []any{80},
		"wait":  map[string]any{"port": "80/tcp", "timeout": "50ms"},
	}))
	if err == nil || !strings.Contains(err.Error(), "nothing takes a connection on its port 80") {
		t.Errorf("err = %v", err)
	}
}

func TestCleanup(t *testing.T) {
	const ofRun = "ps --all --quiet --no-trunc --filter label=probe.run_id=run1"
	const ofKept = ofRun + " --filter label=probe.keep=true"
	gone := errors.New("exit status 1: No such container: b")

	tests := []struct {
		name string
		env  map[string]string
		// lists answers ps for the containers of the run, and kept for
		// those of them that are kept, one for each time round.
		lists []string
		kept  []string
		rm    []answer
		// wantRm is what is removed each time round.
		wantRm  []string
		wantErr string
		wantLog []string
	}{
		{
			name:  "a run that left nothing",
			lists: []string{""},
			kept:  []string{""},
		},
		{
			name:   "removes the containers of the run",
			lists:  []string{"a\nb\n", ""},
			kept:   []string{"", ""},
			wantRm: []string{"rm --force --volumes a b"},
		},
		{
			name:    "leaves the ones that are kept, and tells of them",
			lists:   []string{"a\nb\nc\n", "b\n"},
			kept:    []string{"b\n", "b\n"},
			wantRm:  []string{"rm --force --volumes a c"},
			wantLog: []string{"kept containers of the run", "run_id=run1", "count=1", `remove="docker rm --force --volumes $(docker ps --all --quiet --filter label=probe.run_id=run1)"`, "name=web", "image=nginx:1", "ports=127.0.0.1:32768->80/tcp"},
		},
		{
			name:    "leaves every one under the environment",
			env:     map[string]string{envKeep: "1", envRuntime: "podman"},
			lists:   []string{"a\nb\n"},
			wantLog: []string{"count=2", `remove="podman rm --force --volumes $(podman ps --all --quiet --filter label=probe.run_id=run1)"`},
		},
		{
			name:   "removes one that a step still running started meanwhile",
			lists:  []string{"a\n", "b\n", ""},
			kept:   []string{"", "", ""},
			wantRm: []string{"rm --force --volumes a", "rm --force --volumes b"},
		},
		{
			name:   "does not fail for a container that is already gone",
			lists:  []string{"a\nb\n", ""},
			kept:   []string{"", ""},
			rm:     []answer{{err: gone}},
			wantRm: []string{"rm --force --volumes a b"},
		},
		{
			name:    "fails for a container that cannot be removed",
			lists:   []string{"a\n"},
			kept:    []string{""},
			rm:      []answer{{err: errors.New("exit status 1: permission denied")}},
			wantRm:  []string{"rm --force --volumes a", "rm --force --volumes a", "rm --force --volumes a"},
			wantErr: "1 containers of the run are still there: a: exit status 1: permission denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lists, kept := slices.Clone(tt.lists), slices.Clone(tt.kept)
			next := func(list *[]string) answer {
				a := answer{stdout: (*list)[0]}
				if len(*list) > 1 {
					*list = (*list)[1:]
				}
				return a
			}
			rt := &runtime{answers: map[string][]answer{"rm": tt.rm}}
			rt.on = func(args []string) (answer, bool) {
				switch line := strings.Join(args, " "); {
				case line == ofRun:
					return next(&lists), true
				case line == ofKept:
					return next(&kept), true
				case args[0] == "ps":
					return answer{stdout: "web\tnginx:1\t127.0.0.1:32768->80/tcp\n"}, true
				}
				return answer{}, false
			}
			var logged bytes.Buffer
			a := newAction(rt, tt.env)
			a.log = hclog.New(&hclog.LoggerOptions{Output: &logged, Level: hclog.Warn})

			err := a.Cleanup(actionrpc.CleanupCall{RunID: "run1"})
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want one with %q", err, tt.wantErr)
			}
			if got := rt.ran("rm"); !slices.Equal(got, tt.wantRm) {
				t.Errorf("removed %q, want %q", got, tt.wantRm)
			}
			for _, want := range tt.wantLog {
				if !strings.Contains(logged.String(), want) {
					t.Errorf("the log has no %q:\n%s", want, logged.String())
				}
			}
			if len(tt.wantLog) == 0 && logged.Len() > 0 {
				t.Errorf("logged a warning:\n%s", logged.String())
			}
		})
	}
}

func TestCleanupFailures(t *testing.T) {
	a := newAction(&runtime{}, nil)
	if err := a.Cleanup(actionrpc.CleanupCall{}); err == nil || !strings.Contains(err.Error(), "ID of the run") {
		t.Errorf("without a run: err = %v", err)
	}

	rt := &runtime{answers: map[string][]answer{"ps": {{err: errors.New("cannot connect to the daemon")}}}}
	err := newAction(rt, nil).Cleanup(actionrpc.CleanupCall{RunID: "run1"})
	if err == nil || !strings.Contains(err.Error(), "listing the containers of the run: cannot connect to the daemon") {
		t.Errorf("without a daemon: err = %v", err)
	}
}

func TestCleanupTellsOfAKeptContainerItCannotDescribe(t *testing.T) {
	rt := &runtime{}
	rt.on = func(args []string) (answer, bool) {
		if slices.Contains(args, "--quiet") {
			return answer{stdout: "abc\n"}, true
		}
		return answer{err: errors.New("gone")}, true
	}
	var logged bytes.Buffer
	a := newAction(rt, map[string]string{envKeep: "1"})
	a.log = hclog.New(&hclog.LoggerOptions{Output: &logged, Level: hclog.Warn})
	if err := a.Cleanup(actionrpc.CleanupCall{RunID: "run1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "kept container: id=abc") {
		t.Errorf("log:\n%s", logged.String())
	}
}

func TestCommand(t *testing.T) {
	out, _, err := command(context.Background(), "sh", "-c", "echo out; echo one >&2; echo two >&2")
	if err != nil || out != "out\n" {
		t.Errorf("out = %q, err = %v", out, err)
	}

	_, _, err = command(context.Background(), "sh", "-c", "echo broken >&2; echo beyond repair >&2; exit 4")
	if err == nil || err.Error() != "exit status 4: broken; beyond repair" {
		t.Errorf("a command that fails: err = %v", err)
	}

	_, _, err = command(context.Background(), "probe-container-no-such-runtime", "ps")
	if err == nil || !strings.Contains(err.Error(), envRuntime) {
		t.Errorf("a runtime that is not installed: err = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err = command(ctx, "sleep", "10")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a command that outlives its context: err = %v", err)
	}
}

func TestForLogHidesTheValuesOfTheEnvironment(t *testing.T) {
	with := map[string]any{"image": "postgres", "env": map[string]any{"POSTGRES_PASSWORD": "secret", "A": 1}}
	got := forLog(with)
	want := map[string]any{"image": "postgres", "env": []any{"A", "POSTGRES_PASSWORD"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forLog = %v, want %v", got, want)
	}
	if with["env"].(map[string]any)["POSTGRES_PASSWORD"] != "secret" {
		t.Error("forLog changed with")
	}
	plain := map[string]any{"image": "alpine"}
	if got := forLog(plain); !reflect.DeepEqual(got, plain) {
		t.Errorf("forLog without env = %v", got)
	}
}

// The manifests declare what the action takes and that it cleans up, which
// Probe goes by. It keeps to no guard: what a container does is not the
// action's to limit.
func TestManifests(t *testing.T) {
	for _, name := range []string{"action.yml", "e2e/action/action.yml"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		m, err := actionref.ParseManifest(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(m.Params, params) {
			t.Errorf("%s declares params %v, want %v", name, m.Params, params)
		}
		if !m.Cleanup {
			t.Errorf("%s does not set cleanup", name)
		}
		if len(m.Guard) != 0 {
			t.Errorf("%s declares guard %v, want none", name, m.Guard)
		}
	}
}

var _ actionrpc.Cleaner = (*Action)(nil)
