# cmd/lyeve

The server program inside the official image,
`ghcr.io/lyeve-labs/lyeve-core`. It adds the plugins the image ships with,
which are not published, so it does not build from a clone of this
repository.

## Build the engine instead

From the root of the repository:

```bash
make build-kernel          # writes bin/lyeve-core
go build ./cmd/lyeve-core  # the same program
```

`cmd/lyeve-core` is the engine with no plugins compiled in. It builds and runs
from this repository alone.

To run LyEve with its plugins, use the official image. The
[quick start](../../README.md#quick-start) shows how.
