// The protocol contract as its own module.
//
// The Relay module proper depends on client-go and the rest of the Kubernetes
// libraries because it reads clusters. A control plane speaks the protocol but has no
// business knowing Kubernetes exists, so it must be able to depend on the generated
// types without inheriting that graph — otherwise every client-go advisory and license
// obligation propagates to every consumer of the contract.
//
// The module path is the directory that holds it, so import paths are unchanged by the
// split; only the dependency boundary moved. Versions are tagged gen/go/vX.Y.Z.
module github.com/open-cluster/oc-relay/gen/go

// Deliberately below the Relay module's own floor: the contract should be consumable by
// anything that speaks it, so it requires only what the generated code actually needs.
go 1.25.0

require (
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
)
