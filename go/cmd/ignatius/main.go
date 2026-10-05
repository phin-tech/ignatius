// Command ignatius orchestrates System One decision models. The work is in package cli, so a
// custom build can add or leave out event sinks; see its doc comment and SPEC 14.5. This is the
// stock build: the command plus the standard sinks.
package main

import (
	"github.com/phin-tech/ignatius/go/cli"
	_ "github.com/phin-tech/ignatius/go/standard"
)

func main() { cli.Main() }
