#!/bin/sh
set -eu

profile_dir="${PROFILE_DIR:-/tmp/nanorouter-profile}"
mkdir -p "$profile_dir"

go test ./internal/router \
  -run '^$' \
  -bench '^BenchmarkGatewayProtocols$' \
  -benchtime "${BENCHTIME:-10s}" \
	-o "$profile_dir/router.test" \
  -cpuprofile "$profile_dir/cpu.pprof" \
  -memprofile "$profile_dir/heap.pprof"

printf '%s\n' "profiles written to $profile_dir"
printf '%s\n' "go tool pprof -http=:0 $profile_dir/cpu.pprof"
