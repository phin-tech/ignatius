// Package standard imports the event sinks that live in this module and that a stock build
// wants, the way Caddy's modules/standard does. Importing it for its side effect registers them:
//
//	import _ "github.com/phin-tech/ignatius/go/standard"
//
// The stock command (cmd/ignatius) and the plugin builds import it. A minimal build imports
// package cli alone and has only the file sink; leaving this out is how a deployment guarantees
// its event sinks cannot make outbound HTTP calls.
package standard

import (
	_ "github.com/phin-tech/ignatius/go/events/sinks/webhook"
)
