#!/bin/sh
#
# Runs build + tests for the wallet and its cmd/ submodules.
#
# Each command module is processed explicitly because "go test ./..." is scoped
# to the module where it is invoked.  Enumerate go.mod files because go.work is
# not tracked in this repository.

set -e

GO=${GO:-go}
export GOWORK=off

${GO} version

status=0
for gomod in $(find . -name go.mod -not -path './vendor/*'); do
	dir=$(dirname "$gomod")
	echo "=== $dir"
	if ! (cd "$dir" && ${GO} build ./... && ${GO} test -short -vet=all "$@" ./...); then
		echo "=== FAILED: $dir"
		status=1
	fi
done

if [ $status -ne 0 ]; then
	echo "------------------------------------------"
	echo "One or more modules failed."
	exit 1
fi

echo "------------------------------------------"
echo "Tests completed successfully!"
