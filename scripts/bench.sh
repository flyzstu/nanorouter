#!/bin/sh
set -eu

exec go test ./internal/router \
  -run '^$' \
  -bench '^BenchmarkGatewayProtocols$' \
  -benchmem \
  -benchtime "${BENCHTIME:-3s}" \
  "$@"
