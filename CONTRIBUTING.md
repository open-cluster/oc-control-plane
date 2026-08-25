# Contributing

The control plane is distributed under the [Apache License 2.0](./LICENSE). Contributions
are accepted under the same license.

Contributors should read [AGENTS.md](./AGENTS.md), use the vocabulary in
[CONTEXT.md](./CONTEXT.md), and discuss substantial changes in an issue before writing code.

1. Add a focused test at the observable boundary affected by the change.
2. Preserve explicit Organization scoping, file-backed secrets, and Relay isolation.
3. Update only the relevant product documentation and run `make verify`.
4. Open a pull request describing behavior, security impact, and verification.

The Apache-licensed generated Relay protocol is included under `third_party/relay-protocol`,
so building the control plane requires no private repository access. Dependency additions
require maintainer review, vulnerability scanning, and a compatible license.
