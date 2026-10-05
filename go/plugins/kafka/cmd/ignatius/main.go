// Command ignatius, built with the Kafka event sink compiled in: the core command (package
// cli) plus one blank import whose init registers the "kafka" sink type.
package main

import (
	"github.com/phin-tech/ignatius/go/cli"
	_ "github.com/phin-tech/ignatius/go/plugins/kafka"
	_ "github.com/phin-tech/ignatius/go/standard"
)

func main() { cli.Main() }
