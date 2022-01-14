#!/usr/bin/env bash

source ./scripts/test_lib.sh

GIT_SHA=$(git rev-parse --short HEAD || echo "GitNotFound")
if [[ -n "$FAILPOINTS" ]]; then
  GIT_SHA="$GIT_SHA"-FAILPOINTS
fi

VERSION_SYMBOL="${ROOT_MODULE}/api/v3/version.GitSHA"

# Set GO_LDFLAGS="-s" for building without symbols for debugging.
# shellcheck disable=SC2206
GO_LDFLAGS=(${GO_LDFLAGS} "-X=${VERSION_SYMBOL}=${GIT_SHA}")
GO_BUILD_ENV=("CGO_ENABLED=0" "GO_BUILD_FLAGS=${GO_BUILD_FLAGS}" "GOOS=${GOOS}" "GOARCH=${GOARCH}")

# enable/disable failpoints
toggle_failpoints() {
  mode="$1"
  if command -v gofail >/dev/null 2>&1; then
    run gofail "$mode" server/etcdserver/ server/mvcc/backend/
  elif [[ "$mode" != "disable" ]]; then
    log_error "FAILPOINTS set but gofail not found"
    exit 1
  fi
}

toggle_failpoints_default() {
  mode="disable"
  if [[ -n "$FAILPOINTS" ]]; then mode="enable"; fi
  toggle_failpoints "$mode"
}

tools_build() {
  out="bin"
  if [[ -n "${BINDIR}" ]]; then out="${BINDIR}"; fi
  tools_path="tools/benchmark
    tools/etcd-dump-db
    tools/etcd-dump-logs
    tools/local-tester/bridge"
  for tool in ${tools_path}
  do
    echo "Building" "'${tool}'"...
    run rm -f "${out}/${tool}"
    # shellcheck disable=SC2086
    run env GO_BUILD_FLAGS="${GO_BUILD_FLAGS}" CGO_ENABLED=0 go build ${GO_BUILD_FLAGS} \
      -installsuffix=cgo \
      "-ldflags='${GO_LDFLAGS[*]}'" \
      -o="${out}/${tool}" "./${tool}" || return 2
  done
  tests_build "${@}"
}

tests_build() {
  out=${BINDIR:-./bin}
  out=$(readlink -m "$out")
  out="${out}/functional/cmd"
  mkdir -p "${out}"
  BINDIR="${out}" run ./tests/functional/build.sh || return 2
}

toggle_failpoints_default

# only build when called directly, not sourced
if echo "$0" | grep -E "build_tools(.sh)?$" >/dev/null; then
  if tools_build; then
    log_success "SUCCESS: tools_build (GOARCH=${GOARCH})"
  else
    log_error "FAIL: tools_build (GOARCH=${GOARCH})"
    exit 2
  fi
fi
