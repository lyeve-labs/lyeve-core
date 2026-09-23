# Third-party notices

The `lyeve` binary and the `ghcr.io/lyeve-labs/lyeve-core` image link Go
modules published by other authors. Almost all of them are under the MIT, BSD,
Apache 2.0 or ISC licenses. This file lists the ones that are not. Each module
keeps its own license file in its source. `go version -m` on the binary prints
the exact module versions a build contains.

The list was built from the modules the release binary links, not from
`go.mod`, which also names test and tooling dependencies that never reach the
binary.

## Mozilla Public License 2.0

| Module | Version | Source |
|---|---|---|
| `github.com/go-sql-driver/mysql` | v1.10.1 | https://github.com/go-sql-driver/mysql/tree/v1.10.1 |

The module is used unmodified, exactly as published at the version above. Its
source code, including the MPL-2.0 license text, is available at the link
above and from the Go module proxy at
https://proxy.golang.org/github.com/go-sql-driver/mysql/@v/v1.10.1.zip. The
MPL-2.0 covers that module's files only. It does not extend to the rest of
this repository or the binary.

`github.com/hashicorp/go-version`, `github.com/hashicorp/logutils` and
`pgregory.net/rapid` are also MPL-2.0. They appear in `go.mod` but are not
linked into the binary. All three are used only by tests: the first two come
in through the Pact contract-test library, and `rapid` drives property tests.

## Boost Software License 1.0

| Module | Version | Source |
|---|---|---|
| `github.com/bytedance/sonic` | v1.15.3 | https://github.com/bytedance/sonic/tree/v1.15.3 |

The module is Apache 2.0 and carries code from Drachennest under the Boost
Software License 1.0. Its license texts are in the module's `licenses/`
directory. The Boost license asks nothing of a distribution in machine
executable form. It is listed here for completeness.

## Updating this file

When a dependency is added or bumped, list the modules the binary links and
check each license:

```sh
go version -m bin/lyeve
```

Add any module whose license is not MIT, BSD, Apache 2.0 or ISC to the tables
above with its version and source. The image copies this file and `LICENSE`
to `/usr/share/doc/lyeve/`.
