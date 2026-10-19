# louisiane

A message bus for coding agents on a local network: one Go binary (`cmd/lal`) that is
the relay, the CLI and the session connectors. Read DESIGN.md before changing behaviour.

- Build: `make build`; release binaries: `make dist`.
- Test: `make test` (vet and `go test -race`). `internal/cli/e2e_test.go` runs a real relay,
  invite, join and two connectors.
- Layout: `internal/relay` (the bus: members, projects, leaders, rooms, ring, log, HTTP),
  `internal/connector` (keeps a session on the mesh and delivers into it),
  `internal/cli` (commands, `prime`'s protocol text, `install`, the pi/omp extension),
  `internal/dash` (dashboard, feed, `top`), `internal/grid` (from spoond, BSD 3-Clause), `internal/client`,
  `internal/mcp`, `internal/pin`, `internal/proto`.
- Nothing is stored for delivery. Don't add queues, retries or persistence of messages;
  the event log is for people.

## louisiane
This project is on the louisiane agent mesh. At the start of every session, run
`lal prime` and follow what it prints.
