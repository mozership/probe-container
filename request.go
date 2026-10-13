package main

import (
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultWaitTimeout bounds a with.wait that sets no timeout.
const DefaultWaitTimeout = 60 * time.Second

// waitParams are the keys of with.wait.
var waitParams = []string{"port", "log", "healthcheck", "timeout"}

// protocols are the protocols a port can be published for.
var protocols = []string{"tcp", "udp", "sctp"}

// kind is how the value of an option is written in a step.
type kind int

const (
	// word is one value, a string or a number.
	word kind = iota
	// wordList is a list of them, each given to the runtime with the flag.
	wordList
	// toggle is true or false, and the flag is given when it is true.
	toggle
)

// option is a parameter that is given to the runtime as one of its own
// options, with the value the step wrote.
type option struct {
	key  string
	flag string
	kind kind
}

// options are given to the runtime in this order.
var options = []option{
	{"platform", "--platform", word},
	{"entrypoint", "--entrypoint", word},
	{"user", "--user", word},
	{"workdir", "--workdir", word},
	{"hostname", "--hostname", word},
	{"network_aliases", "--network-alias", wordList},
	{"add_hosts", "--add-host", wordList},
	{"dns", "--dns", wordList},
	{"env_files", "--env-file", wordList},
	{"tmpfs", "--tmpfs", wordList},
	{"cap_add", "--cap-add", wordList},
	{"cap_drop", "--cap-drop", wordList},
	{"ulimits", "--ulimit", wordList},
	{"memory", "--memory", word},
	{"cpus", "--cpus", word},
	{"shm_size", "--shm-size", word},
	{"privileged", "--privileged", toggle},
	{"read_only", "--read-only", toggle},
	{"init", "--init", toggle},
}

// optionKeys returns the keys of options, in their order.
func optionKeys() []string {
	keys := make([]string, len(options))
	for i, o := range options {
		keys[i] = o.key
	}
	return keys
}

// chosen is an option a step sets. A toggle that is set has no values.
type chosen struct {
	option
	values []string
}

type request struct {
	image   string
	name    string
	command []string
	// env is the environment of the container, as NAME=value in the order
	// of the names.
	env     []string
	ports   []port
	volumes []string
	// labels are the labels the step gives the container, as name=value in
	// the order of the names.
	labels  []string
	network string
	pull    string
	// options are the options of the runtime the step sets.
	options []chosen
	wait    *wait
	keep    bool
	// runID is the run the container is labelled with.
	runID string
}

// port is a port of the container to publish.
type port struct {
	// spec is what the runtime is given to publish it.
	spec string
	// key names the port in a result: its number in the container, and its
	// protocol unless it is TCP, as 53/udp.
	key string
}

// wait is what a container has to do before its step is over.
type wait struct {
	// port is the key of the port that has to take a connection, or empty.
	port string
	// log has to match what the container wrote, unless it is nil.
	log *regexp.Regexp
	// healthy has the health check of the image pass.
	healthy bool
	timeout time.Duration
}

func parseRequest(with map[string]any) (*request, error) {
	r := &request{}

	for k := range with {
		if !slices.Contains(params, k) {
			return nil, fmt.Errorf("container action takes %s, not %s", strings.Join(params, ", "), k)
		}
	}

	var err error
	if r.image, err = text(with, "image"); err != nil {
		return nil, err
	}
	if r.image == "" {
		return nil, errors.New("container action requires with.image")
	}
	if strings.HasPrefix(r.image, "-") {
		return nil, fmt.Errorf("with.image %q is not an image: it would be read as an option", r.image)
	}
	if r.name, err = text(with, "name"); err != nil {
		return nil, err
	}
	if r.network, err = text(with, "network"); err != nil {
		return nil, err
	}
	if r.pull, err = text(with, "pull"); err != nil {
		return nil, err
	}
	if r.pull != "" && !slices.Contains(pulls, r.pull) {
		return nil, fmt.Errorf("with.pull must be one of %s, not %s", strings.Join(pulls, ", "), r.pull)
	}
	if r.command, err = words(with, "command"); err != nil {
		return nil, err
	}
	if r.volumes, err = words(with, "volumes"); err != nil {
		return nil, err
	}
	if r.env, err = pairs(with, "env"); err != nil {
		return nil, err
	}
	if r.labels, err = pairs(with, "labels"); err != nil {
		return nil, err
	}
	for _, l := range r.labels {
		if name, _, _ := strings.Cut(l, "="); name == labelRun || name == labelKeep {
			return nil, fmt.Errorf("with.labels has the label %s, which the action sets itself", name)
		}
	}
	if r.options, err = parseOptions(with); err != nil {
		return nil, err
	}
	if r.ports, err = parsePorts(with["ports"]); err != nil {
		return nil, err
	}
	if r.wait, err = parseWait(with["wait"], r.ports); err != nil {
		return nil, err
	}
	if v, ok := with["keep"]; ok && v != nil {
		if r.keep, ok = v.(bool); !ok {
			return nil, fmt.Errorf("with.keep must be true or false, not %v", v)
		}
	}
	return r, nil
}

// text returns with[key], which is a string when it is set.
func text(with map[string]any, key string) (string, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("with.%s must be a string, not %v", key, v)
	}
	return s, nil
}

// words returns with[key], a list of plain values, as strings.
func words(with map[string]any, key string) ([]string, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("with.%s must be a list, with one entry for each word, not %v", key, v)
	}
	out := make([]string, len(list))
	for i, e := range list {
		s, ok := plain(e)
		if !ok {
			return nil, fmt.Errorf("with.%s[%d] must be a string or a number, not %v", key, i, e)
		}
		out[i] = s
	}
	return out, nil
}

// plain returns a string, a number or a boolean as the string it is
// written as.
func plain(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	default:
		return "", false
	}
}

// pairs returns with[key], a map of names to plain values, as name=value in
// the order of the names.
func pairs(with map[string]any, key string) ([]string, error) {
	v := with[key]
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("with.%s must be a map of names to values, not %v", key, v)
	}
	if len(m) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(m))
	for k, e := range m {
		if k == "" || strings.Contains(k, "=") {
			return nil, fmt.Errorf("with.%s has the name %q, and a name is not empty and has no equals sign", key, k)
		}
		s, ok := plain(e)
		if !ok {
			return nil, fmt.Errorf("with.%s.%s must be a string, a number or a boolean", key, k)
		}
		out = append(out, k+"="+s)
	}
	slices.Sort(out)
	return out, nil
}

// parseOptions returns the options with sets, in the order of options. One
// that is empty or false is the same as one that is left out.
func parseOptions(with map[string]any) ([]chosen, error) {
	var set []chosen
	for _, o := range options {
		v, ok := with[o.key]
		if !ok || v == nil {
			continue
		}
		switch o.kind {
		case word:
			s, ok := plain(v)
			if _, isBool := v.(bool); !ok || isBool {
				return nil, fmt.Errorf("with.%s must be a string or a number, not %v", o.key, v)
			}
			if s != "" {
				set = append(set, chosen{o, []string{s}})
			}
		case wordList:
			list, err := words(with, o.key)
			if err != nil {
				return nil, err
			}
			if len(list) > 0 {
				set = append(set, chosen{o, list})
			}
		case toggle:
			on, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("with.%s must be true or false, not %v", o.key, v)
			}
			if on {
				set = append(set, chosen{option: o})
			}
		}
	}
	return set, nil
}

func parsePorts(v any) ([]port, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("with.ports must be a list, not %v", v)
	}
	ports := make([]port, 0, len(list))
	for i, e := range list {
		s, ok := e.(string)
		if n, isNumber := whole(e); isNumber {
			s, ok = strconv.Itoa(n), true
		}
		if !ok {
			return nil, fmt.Errorf("with.ports[%d] must be a port of the container, such as 5432, or what the runtime publishes one by, such as 8080:80, not %v", i, e)
		}
		p, err := parsePort(s)
		if err != nil {
			return nil, fmt.Errorf("with.ports[%d]: %w", i, err)
		}
		if slices.ContainsFunc(ports, func(q port) bool { return q.key == p.key }) {
			return nil, fmt.Errorf("with.ports[%d]: the port %s of the container is listed twice", i, p.key)
		}
		ports = append(ports, p)
	}
	return ports, nil
}

// parsePort reads one entry of with.ports. A port of the container alone is
// published on a free port of the loopback address; an entry that says
// where to publish it is given to the runtime as it is.
func parsePort(s string) (port, error) {
	inside := s
	spec := loopback + "::" + s
	if i := strings.LastIndex(s, ":"); i >= 0 {
		inside = s[i+1:]
		spec = s
	}
	number, proto, hasProto := strings.Cut(inside, "/")
	if n, err := strconv.Atoi(number); err != nil || n < 1 || n > math.MaxUint16 {
		return port{}, fmt.Errorf("%q does not name a port of the container, a number from 1 to 65535", s)
	}
	if hasProto && !slices.Contains(protocols, proto) {
		return port{}, fmt.Errorf("%q has the protocol %s, which is not one of %s", s, proto, strings.Join(protocols, ", "))
	}
	key := number
	if hasProto && proto != "tcp" {
		key += "/" + proto
	}
	return port{spec: spec, key: key}, nil
}

func parseWait(v any, ports []port) (*wait, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("with.wait must be a map with any of %s, not %v", strings.Join(waitParams, ", "), v)
	}
	for k := range m {
		if !slices.Contains(waitParams, k) {
			return nil, fmt.Errorf("with.wait takes %s, not %s", strings.Join(waitParams, ", "), k)
		}
	}
	w := &wait{timeout: DefaultWaitTimeout}

	if p, ok := m["port"]; ok && p != nil {
		s, isString := p.(string)
		if n, isNumber := whole(p); isNumber {
			s, isString = strconv.Itoa(n), true
		}
		if !isString {
			return nil, fmt.Errorf("with.wait.port must be one of with.ports, not %v", p)
		}
		w.port = strings.TrimSuffix(s, "/tcp")
		if strings.Contains(w.port, "/") {
			return nil, fmt.Errorf("with.wait.port %s is not a TCP port, and only one of those can be connected to", s)
		}
		if !slices.ContainsFunc(ports, func(q port) bool { return q.key == w.port }) {
			return nil, fmt.Errorf("with.wait.port %s is not a port of the container that with.ports publishes", s)
		}
	}

	if l, ok := m["log"]; ok && l != nil {
		s, isString := l.(string)
		if !isString || s == "" {
			return nil, fmt.Errorf("with.wait.log must be a regular expression, not %v", l)
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("with.wait.log: %w", err)
		}
		w.log = re
	}

	if h, ok := m["healthcheck"]; ok && h != nil {
		if w.healthy, ok = h.(bool); !ok {
			return nil, fmt.Errorf("with.wait.healthcheck must be true or false, not %v", h)
		}
	}

	if t, ok := m["timeout"]; ok && t != nil {
		var err error
		if w.timeout, err = parseTimeout(t); err != nil {
			return nil, err
		}
	}

	if w.port == "" && w.log == nil && !w.healthy {
		return nil, errors.New("with.wait names nothing to wait for: give it port, log or healthcheck")
	}
	return w, nil
}

// parseTimeout accepts a duration string such as "10s" or a number of
// seconds, as the http action does.
func parseTimeout(v any) (time.Duration, error) {
	var d time.Duration
	switch t := v.(type) {
	case string:
		var err error
		if d, err = time.ParseDuration(t); err != nil {
			return 0, fmt.Errorf("with.wait.timeout: %w", err)
		}
	case int:
		d = time.Duration(t) * time.Second
	case int64:
		d = time.Duration(t) * time.Second
	case float64:
		d = time.Duration(t * float64(time.Second))
	default:
		return 0, fmt.Errorf("with.wait.timeout must be a duration or a number of seconds, not %v", v)
	}
	if d <= 0 {
		return 0, fmt.Errorf("with.wait.timeout must be more than nothing, not %v", v)
	}
	return d, nil
}

// whole returns v as an int when it is a number without a fraction, which
// is how a port arrives from a workflow.
func whole(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		if t == math.Trunc(t) && math.Abs(t) <= math.MaxInt32 {
			return int(t), true
		}
	}
	return 0, false
}

// args returns what the runtime is given to start the container in the
// background. Everything after the image is the command of the container,
// so nothing the step sets can be read as an option of the runtime there.
func (r *request) args() []string {
	args := []string{"run", "--detach", "--label", labelRun + "=" + r.runID}
	if r.keep {
		args = append(args, "--label", labelKeep+"=true")
	}
	if r.name != "" {
		args = append(args, "--name", r.name)
	}
	if r.pull != "" {
		args = append(args, "--pull", r.pull)
	}
	if r.network != "" {
		args = append(args, "--network", r.network)
	}
	for _, e := range r.env {
		args = append(args, "--env", e)
	}
	for _, p := range r.ports {
		args = append(args, "--publish", p.spec)
	}
	for _, v := range r.volumes {
		args = append(args, "--volume", v)
	}
	for _, l := range r.labels {
		args = append(args, "--label", l)
	}
	for _, o := range r.options {
		if o.kind == toggle {
			args = append(args, o.flag)
		}
		for _, v := range o.values {
			args = append(args, o.flag, v)
		}
	}
	args = append(args, r.image)
	return append(args, r.command...)
}

// info returns the request as a result has it, with the environment by its
// names alone.
func (r *request) info() map[string]any {
	info := map[string]any{"image": r.image, "keep": r.keep}
	if r.name != "" {
		info["name"] = r.name
	}
	if r.network != "" {
		info["network"] = r.network
	}
	if r.pull != "" {
		info["pull"] = r.pull
	}
	if len(r.command) > 0 {
		info["command"] = list(r.command)
	}
	if len(r.volumes) > 0 {
		info["volumes"] = list(r.volumes)
	}
	if len(r.env) > 0 {
		env := make([]any, len(r.env))
		for i, e := range r.env {
			env[i], _, _ = strings.Cut(e, "=")
		}
		info["env"] = env
	}
	if len(r.ports) > 0 {
		ports := make([]any, len(r.ports))
		for i, p := range r.ports {
			ports[i] = p.key
		}
		info["ports"] = ports
	}
	if len(r.labels) > 0 {
		labels := make(map[string]any, len(r.labels))
		for _, l := range r.labels {
			name, value, _ := strings.Cut(l, "=")
			labels[name] = value
		}
		info["labels"] = labels
	}
	for _, o := range r.options {
		switch o.kind {
		case word:
			info[o.key] = o.values[0]
		case wordList:
			info[o.key] = list(o.values)
		case toggle:
			info[o.key] = true
		}
	}
	return info
}

func list(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// binding is where a port of a container is reached from the machine.
type binding struct {
	host string
	port int
}

func (b binding) addr() string {
	return net.JoinHostPort(b.host, strconv.Itoa(b.port))
}

// parseBindings reads what the port command of a runtime answers, a line
// such as "5432/tcp -> 127.0.0.1:51110" for each address a port is
// published on, keyed as port.key is. A port published on IPv4 and IPv6
// keeps its IPv4 address, and one published on every address is reached at
// the loopback one.
func parseBindings(out string) map[string]binding {
	bound := map[string]binding{}
	for _, line := range strings.Split(out, "\n") {
		inside, outside, ok := strings.Cut(strings.TrimSpace(line), " -> ")
		if !ok {
			continue
		}
		host, p, err := net.SplitHostPort(outside)
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		key := strings.TrimSuffix(inside, "/tcp")
		ip := net.ParseIP(host)
		v4 := ip != nil && ip.To4() != nil
		if _, seen := bound[key]; seen && !v4 {
			continue
		}
		if ip == nil || ip.IsUnspecified() {
			host = loopback
		}
		bound[key] = binding{host: host, port: n}
	}
	return bound
}
