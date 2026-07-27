// The end-to-end proof harness, kept as a module of its own.
//
// It runs the Relay and a control plane as real processes and needs whatever that takes:
// container control, a TLS terminator, a database driver. The shipping module must not
// acquire those dependencies, and a gate there fails the build if it ever requires the
// Relay's own module — so the harness cannot live in it.
module github.com/open-cluster/oc-control-plane/test/e2e

go 1.26

require golang.org/x/net v0.57.0

require golang.org/x/text v0.40.0 // indirect
