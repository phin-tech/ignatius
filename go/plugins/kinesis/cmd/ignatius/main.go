// Command ignatius, built with the Kinesis event sink compiled in. It is the core command
// (package cli) plus one blank import: that import's init registers the "kinesis" sink type.
// A build with other plugins is the same file with other imports.
package main

import (
	"github.com/phin-tech/ignatius/go/cli"
	_ "github.com/phin-tech/ignatius/go/plugins/kinesis"
	_ "github.com/phin-tech/ignatius/go/standard"
)

func main() { cli.Main() }
