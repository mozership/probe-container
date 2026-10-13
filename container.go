package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/mozership/probe/actionrpc"
)

const (
	// labelRun is the label every container the action starts carries, with
	// the ID of the run as its value. It is all Cleanup finds them by.
	labelRun = "probe.run_id"
	// labelKeep is the label of a container Cleanup leaves, with the value
	// true.
	labelKeep = "probe.keep"

	// envRuntime names the command the action runs containers with, for a
	// machine that has Podman or nerdctl in place of Docker. A step and the
	// cleanup of its run read it alike, so it is not a parameter of a step.
	envRuntime = "PROBE_CONTAINER_RUNTIME"
	// envKeep, when true, leaves every container of the run, to look into
	// after it.
	envKeep = "PROBE_CONTAINER_KEEP"

	defaultRuntime = "docker"

	// loopback is where a port is published when the step names no address,
	// and the host a step is told when the port is published on every one.
	loopback = "127.0.0.1"

	// cleanupTimeout bounds a cleanup. Probe gives one 30 seconds, the
	// start of the action included, and tells nothing of why one that ran
	// out of them failed.
	cleanupTimeout = 25 * time.Second
	// cleanupRounds is how many times a cleanup looks for containers. One
	// that follows a signal can run beside a step that is still starting a
	// container.
	cleanupRounds = 3

	// tailLines is how many lines of a container's log an error shows.
	tailLines = 10
)

// params are the keys the action takes in with. action.yml declares them,
// for probe check to report a key the action does not take.
var params = []string{"image", "name", "command", "env", "ports", "volumes", "network", "pull", "wait", "keep"}

// pulls are the values of with.pull, as the --pull of a runtime takes them.
var pulls = []string{"missing", "always", "never"}

// Action starts the container a step describes, waits until it is ready,
// and removes the containers of a run once the run is over.
type Action struct {
	log hclog.Logger
	// exec runs the command of the runtime with args, and returns what it
	// wrote. It defaults to running the command.
	exec func(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
	// dial opens the connection a wait.port is tried with. It defaults to
	// a net.Dialer.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// getenv reads the environment. It defaults to os.Getenv.
	getenv func(string) string
	// poll is how long a container is let be between two looks at whether
	// it is ready. It defaults to defaultPoll.
	poll time.Duration
}

// Run fails: a container outlives its step, and without a step there is no
// run to remove it after.
func (a *Action) Run(with map[string]any) (map[string]any, error) {
	ret, _, err := a.RunStep(actionrpc.Call{With: with})
	return ret, err
}

// RunStep starts the container call.With describes, labelled with the run,
// and returns once it is ready, with the ports it is reached at. A
// container that does not become ready is an error, and is left to the
// cleanup like any other.
func (a *Action) RunStep(call actionrpc.Call) (map[string]any, map[string]any, error) {
	if a.log == nil {
		a.log = hclog.NewNullLogger()
	}
	actionrpc.LogParams(a.log, "received request parameters", forLog(call.With))

	req, err := parseRequest(call.With)
	if err != nil {
		return nil, nil, err
	}
	if call.Step.RunID == "" {
		return nil, nil, errors.New("container action needs the ID of the run to label the container with, and was given none")
	}
	req.runID = call.Step.RunID
	req.keep = req.keep || a.keepAll()
	if call.Cleanups == nil && !req.keep {
		return nil, nil, errors.New("this Probe does not clean up once a run is over, so the container would stay: use a Probe that does, or set with.keep to leave the container on purpose")
	}

	ret, err := a.start(context.Background(), req)
	actionrpc.LogOutcome(a.log, "container", ret, err)
	return ret, nil, err
}

// forLog returns with as it is logged: the environment of the container by
// its names alone, as its values are often passwords.
func forLog(with map[string]any) map[string]any {
	env, ok := with["env"].(map[string]any)
	if !ok {
		return with
	}
	logged := make(map[string]any, len(with))
	for k, v := range with {
		logged[k] = v
	}
	logged["env"] = names(env)
	return logged
}

// names returns the keys of env in order, as a list a result can hold.
func names(env map[string]any) []any {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	list := make([]any, len(keys))
	for i, k := range keys {
		list[i] = k
	}
	return list
}

func (a *Action) start(ctx context.Context, r *request) (map[string]any, error) {
	began := time.Now()

	out, _, err := a.run(ctx, r.args()...)
	if err != nil {
		return nil, fmt.Errorf("starting %s: %w", r.image, err)
	}
	id := lastLine(out)
	if id == "" {
		return nil, fmt.Errorf("starting %s: %s did not tell the ID of the container", r.image, a.runtime())
	}

	name, _, err := a.run(ctx, "inspect", "--format", "{{.Name}}", id)
	if err != nil {
		return nil, fmt.Errorf("reading the name of the container: %w", err)
	}
	res := map[string]any{
		"id":    id,
		"name":  strings.TrimPrefix(strings.TrimSpace(name), "/"),
		"ports": map[string]any{},
	}

	bound := map[string]binding{}
	if len(r.ports) > 0 {
		out, _, err := a.run(ctx, "port", id)
		if err != nil {
			return nil, fmt.Errorf("reading the ports of the container: %w", err)
		}
		bound = parseBindings(out)
		ports := map[string]any{}
		for i, p := range r.ports {
			b, ok := bound[p.key]
			if !ok {
				return nil, fmt.Errorf("the port %s of the container is not published: %s port answered %q", p.key, a.runtime(), strings.TrimSpace(out))
			}
			ports[p.key] = b.port
			if i == 0 {
				res["host"] = b.host
				res["port"] = b.port
				res["addr"] = b.addr()
			}
		}
		res["ports"] = ports
	}

	if r.wait != nil {
		if err := a.await(ctx, id, r.wait, bound); err != nil {
			return nil, err
		}
	}

	return map[string]any{
		"req":    r.info(),
		"res":    res,
		"rt":     time.Since(began).String(),
		"status": 0,
	}, nil
}

// Cleanup removes the containers the steps of the run started, but for
// those that are kept, which it names in the log with how to remove them.
// It does not fail for a run that left none.
func (a *Action) Cleanup(call actionrpc.CleanupCall) error {
	if a.log == nil {
		a.log = hclog.NewNullLogger()
	}
	if call.RunID == "" {
		return errors.New("container action needs the ID of the run to find its containers by, and was given none")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	ofRun := "label=" + labelRun + "=" + call.RunID
	keepAll := a.keepAll()
	var kept []string
	var failure error
	for round := 0; ; round++ {
		all, err := a.list(ctx, ofRun)
		if err != nil {
			return err
		}
		kept = all
		if keepAll {
			break
		}
		if kept, err = a.list(ctx, ofRun, "label="+labelKeep+"=true"); err != nil {
			return err
		}
		doomed := slices.DeleteFunc(all, func(id string) bool { return slices.Contains(kept, id) })
		if len(doomed) == 0 {
			break
		}
		if round == cleanupRounds {
			return fmt.Errorf("%d containers of the run are still there: %s: %w", len(doomed), strings.Join(doomed, ", "), failure)
		}
		a.log.Debug("removing containers", "run_id", call.RunID, "containers", doomed)
		// A container that is gone by now fails the command, and is not
		// listed the next time round, which is what tells the two apart.
		if _, _, failure = a.run(ctx, append([]string{"rm", "--force", "--volumes"}, doomed...)...); failure != nil {
			a.log.Debug("removing containers failed", "error", failure)
			if ctx.Err() != nil {
				return fmt.Errorf("removing %d containers: %w", len(doomed), failure)
			}
		}
	}

	if len(kept) > 0 {
		a.tellKept(ctx, call.RunID, ofRun, kept)
	}
	return nil
}

// tellKept logs the containers the cleanup left and how to remove them, as
// warnings, which Probe shows without being asked to be verbose.
func (a *Action) tellKept(ctx context.Context, runID, ofRun string, kept []string) {
	rt := a.runtime()
	a.log.Warn("kept containers of the run", "run_id", runID, "count", len(kept),
		"remove", fmt.Sprintf("%s rm --force --volumes $(%s ps --all --quiet --filter %s)", rt, rt, ofRun))
	for _, id := range kept {
		out, _, err := a.run(ctx, "ps", "--all", "--filter", "id="+id, "--format", "{{.Names}}\t{{.Image}}\t{{.Ports}}")
		f := strings.SplitN(strings.TrimSpace(out), "\t", 3)
		if err != nil || f[0] == "" {
			a.log.Warn("kept container", "id", id)
			continue
		}
		f = append(f, "", "")
		a.log.Warn("kept container", "name", f[0], "image", f[1], "ports", f[2])
	}
}

// list returns the IDs of the containers that pass every filter, running
// or not.
func (a *Action) list(ctx context.Context, filters ...string) ([]string, error) {
	args := []string{"ps", "--all", "--quiet", "--no-trunc"}
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	out, _, err := a.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("listing the containers of the run: %w", err)
	}
	return strings.Fields(out), nil
}

// keepAll reports whether the environment asks for every container to be
// kept. A value that is neither true nor false keeps them: a container
// kept by mistake can still be removed.
func (a *Action) keepAll() bool {
	v := a.env(envKeep)
	if v == "" {
		return false
	}
	keep, err := strconv.ParseBool(v)
	return err != nil || keep
}

// runtime returns the command containers are run with.
func (a *Action) runtime() string {
	if v := a.env(envRuntime); v != "" {
		return v
	}
	return defaultRuntime
}

func (a *Action) env(name string) string {
	if a.getenv != nil {
		return a.getenv(name)
	}
	return os.Getenv(name)
}

// run runs the runtime with args.
func (a *Action) run(ctx context.Context, args ...string) (string, string, error) {
	run := a.exec
	if run == nil {
		run = command
	}
	return run(ctx, a.runtime(), args...)
}

// command runs name with args. Its error says what the command wrote to
// stderr, which is where a runtime explains itself.
func command(ctx context.Context, name string, args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if said := tail(stderr.String(), tailLines); said != "" {
			err = fmt.Errorf("%w: %s", err, said)
		}
		if errors.Is(err, exec.ErrNotFound) {
			err = fmt.Errorf("%w (set %s to the command containers are run with)", err, envRuntime)
		}
	}
	return stdout.String(), stderr.String(), err
}

// lastLine returns the last line of s that is not blank. A runtime that
// pulls an image may write of it before the ID of the container.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// tail returns the last n lines of s that are not blank, joined by "; ".
func tail(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}
