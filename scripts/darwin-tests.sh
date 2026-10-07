#!/usr/bin/env bash
# Cross-compiled darwin test binaries: build them in the devcontainer, run them
# on any Mac (the host or a Tart VM) without a Go toolchain there.
#   darwin-tests.sh build        compile into bin/darwin-test/ (run in the devcontainer)
#   darwin-tests.sh run [root]   run them from the repo at root (default: this repo)
set -euo pipefail

root=${2:-$(cd "$(dirname "$0")/.." && pwd)}
out=bin/darwin-test

case ${1:-} in
build)
	cd "$root"
	rm -rf "$out" && mkdir -p "$out"
	mod=$(go list -m)
	for pkg in $(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...); do
		rel=${pkg#"$mod"/}
		GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go test -c -o "$out/${rel//\//_}.test" "$pkg"
		echo "$rel" >>"$out/packages"
	done
	;;
run)
	[[ $(uname -s) == Darwin ]] || { echo "darwin-tests: run needs macOS" >&2; exit 2; }
	fail=0
	while read -r rel; do
		# Run from the package dir, as go test does, so testdata paths resolve.
		if (cd "$root/$rel" && "$root/$out/${rel//\//_}.test" -test.count=1 >/tmp/hr-test.log 2>&1); then
			echo "ok   $rel"
		else
			echo "FAIL $rel"; cat /tmp/hr-test.log; fail=1
		fi
	done <"$root/$out/packages"
	exit $fail
	;;
*)
	echo "usage: $0 build | run [root]" >&2; exit 2
	;;
esac
