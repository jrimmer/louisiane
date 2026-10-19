// Command lal is the louisiane relay, its operator commands, the agents'
// commands for talking on the mesh, and the session connectors.
package main

import (
	"os"

	"github.com/jrimmer/louisiane/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
