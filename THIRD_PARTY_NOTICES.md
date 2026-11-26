# Third-Party Notices

`linkc-node` is distributed with third-party open-source software. This file
identifies the direct dependencies declared in `go.mod`; transitive dependencies
remain subject to the license terms published by their respective projects.

The exact resolved dependency graph is recorded in `go.sum` and can be inspected
with:

```bash
go list -m all
go list -m -json all
```

## Direct dependencies

| Module | License | Upstream |
|---|---|---|
| `github.com/google/uuid` | BSD-3-Clause | https://github.com/google/uuid |
| `github.com/xtls/xray-core` | MPL-2.0 | https://github.com/XTLS/Xray-core |
| `go.etcd.io/bbolt` | MIT | https://github.com/etcd-io/bbolt |
| `golang.org/x/crypto` | BSD-3-Clause | https://cs.opensource.google/go/x/crypto |
| `golang.org/x/net` | BSD-3-Clause | https://cs.opensource.google/go/x/net |
| `golang.org/x/time` | BSD-3-Clause | https://cs.opensource.google/go/x/time |
| `google.golang.org/protobuf` | BSD-3-Clause | https://github.com/protocolbuffers/protobuf-go |
| `gopkg.in/yaml.v3` | Apache-2.0 OR MIT | https://github.com/go-yaml/yaml |

## Xray-core

This project embeds and links portions of Xray-core under the Mozilla Public
License 2.0. The exact resolved version is pinned in `go.mod`; its corresponding
source and license are available from the upstream project and the Go module
proxy:

https://github.com/XTLS/Xray-core

```bash
go mod download -json github.com/xtls/xray-core
```

The notice and license text shipped with the resolved module must be retained
when redistributing binaries or container images.

## Distribution

When distributing binaries, container images, or packages, retain this notice
and the license terms supplied by upstream projects. No third-party license is
replaced or modified by the license for `linkc-node` itself.
