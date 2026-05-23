# LyEve Core.
#
# The rules live in make/*.mk, one file per concern, each declaring the .PHONY
# for the targets it owns so the two cannot drift apart. Included in dependency
# order: common.mk defines what the rest read.
#
#   make            list every target, grouped by the file it comes from
#   make print-VAR  show what a variable expanded to

.DEFAULT_GOAL := help

include make/common.mk    # colors, build metadata, shared paths, help
include make/build.mk     # binaries
include make/dev.mk       # run the engine from a working tree
include make/test.mk      # unit, contract and mutation tests
include make/fuzz.mk      # fuzz harnesses
include make/bench.mk     # benchmarks and k6 load tests
include make/verify.mk    # formatting and source lints, preflight
include make/release.mk   # version pinning, vulnerability scan, SBOM
