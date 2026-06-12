#!/bin/sh
# E2E entrypoint: run the federation suite, then the package-publish suite.
# Both run regardless of each other's outcome so a single failure doesn't mask
# the rest; the container exits non-zero if either suite fails.
rc=0
sh /tests/federation.sh || rc=1
sh /tests/packages.sh || rc=1
exit "$rc"
