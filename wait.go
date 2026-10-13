package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultPoll is how long the action lets a container be between two
	// looks at whether it is ready.
	defaultPoll = 200 * time.Millisecond
	// dialTimeout bounds one attempt to connect to a port.
	dialTimeout = time.Second
	// settle is how long a connection has to stay open to count as taken.
	// A runtime listens on a published port itself, and takes a connection
	// there before anything in the container listens, only to close it at
	// once.
	settle = 250 * time.Millisecond
	// tailTimeout bounds the reading of a log for an error.
	tailTimeout = 5 * time.Second
)

// state is what a runtime tells of a container.
type state struct {
	running bool
	exit    int
	// health is the status of the health check, or empty for an image
	// without one.
	health string
}

// await returns once the container is what w asks for, and fails when it
// stops before that or is not there within the timeout of w.
func (a *Action) await(ctx context.Context, id string, w *wait, bound map[string]binding) error {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()

	poll := a.poll
	if poll <= 0 {
		poll = defaultPoll
	}
	pending := "its state was not read"
	for {
		st, err := a.state(ctx, id)
		switch {
		case err != nil && ctx.Err() == nil:
			return fmt.Errorf("reading the state of the container: %w", err)
		case err != nil:
		case !st.running:
			return fmt.Errorf("the container stopped with exit status %d before it was ready%s", st.exit, a.logTail(id))
		case w.healthy && st.health == "":
			return errors.New("with.wait.healthcheck is set, and the image has no health check to wait for")
		default:
			if pending = a.pending(ctx, id, w, bound, st); pending == "" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the container was not ready after %s: %s%s", w.timeout, pending, a.logTail(id))
		case <-time.After(poll):
		}
	}
}

// pending returns what the container is still waited for, or nothing once
// it is ready.
func (a *Action) pending(ctx context.Context, id string, w *wait, bound map[string]binding, st state) string {
	if w.healthy && st.health != "healthy" {
		return "its health check is " + st.health
	}
	if w.log != nil {
		stdout, stderr, err := a.run(ctx, "logs", id)
		if err != nil || !w.log.MatchString(stdout+stderr) {
			return fmt.Sprintf("its log does not match %q", w.log)
		}
	}
	if w.port != "" && !a.takes(ctx, bound[w.port].addr()) {
		return "nothing takes a connection on its port " + w.port
	}
	return ""
}

// takes reports whether a connection to addr is taken and kept: one that
// stays open, or that the other end writes to.
func (a *Action) takes(ctx context.Context, addr string) bool {
	dial := a.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(settle))
	n, err := conn.Read(make([]byte, 1))
	return n > 0 || errors.Is(err, os.ErrDeadlineExceeded)
}

// state reads whether the container runs, the status it exited with, and
// the status of its health check.
func (a *Action) state(ctx context.Context, id string) (state, error) {
	out, _, err := a.run(ctx, "inspect", "--format", "{{.State.Running}} {{.State.ExitCode}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", id)
	if err != nil {
		return state{}, err
	}
	f := strings.Fields(out)
	if len(f) < 2 {
		return state{}, fmt.Errorf("%s inspect answered %q", a.runtime(), strings.TrimSpace(out))
	}
	st := state{running: f[0] == "true"}
	st.exit, _ = strconv.Atoi(f[1])
	if len(f) > 2 {
		st.health = f[2]
	}
	return st, nil
}

// logTail returns the last lines the container wrote, to end an error with,
// or nothing when it wrote none.
func (a *Action) logTail(id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), tailTimeout)
	defer cancel()
	stdout, stderr, err := a.run(ctx, "logs", "--tail", strconv.Itoa(tailLines), id)
	if err != nil {
		return ""
	}
	said := tail(stdout+"\n"+stderr, tailLines)
	if said == "" {
		return ""
	}
	return "; its log ends with: " + said
}
