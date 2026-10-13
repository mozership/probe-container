// Command probe-container serves a Probe action that starts a container
// and removes it once the run is over. Probe starts it for a step that uses
// github.com/mozership/probe-container@<commit>.
package main

import (
	"github.com/hashicorp/go-hclog"
	"github.com/mozership/probe/actionrpc"
)

// version is set at build time.
var version = "dev"

func main() {
	actionrpc.Serve(func(log hclog.Logger) actionrpc.Action {
		return &Action{log: log}
	})
}
