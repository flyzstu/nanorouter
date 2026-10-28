#!/usr/bin/env bash
# 启动三个真实网关实例，验证 API Key 的三种鉴权模式。
# 依赖：go、python3。所有临时文件在 mktemp 目录内，失败时保留以便排查。
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
pids=()

cleanup() {
  local status=$?
  for pid in "${pids[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  if [ "$status" -ne 0 ]; then
    echo "API key smoke failed; artifacts kept at $tmp" >&2
    for log in "$tmp"/*.log; do
      [ -f "$log" ] || continue
      echo "--- $log ---" >&2
      tail -40 "$log" >&2 || true
    done
  else
    rm -rf "$tmp"
  fi
}
trap cleanup EXIT

pick_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

public_port=$(pick_port)
noroot_port=$(pick_port)
while [ "$noroot_port" = "$public_port" ]; do noroot_port=$(pick_port); done
root_port=$(pick_port)
while [ "$root_port" = "$public_port" ] || [ "$root_port" = "$noroot_port" ]; do root_port=$(pick_port); done

mkdir -p "$tmp/home"
echo "building nanorouter..."
(cd "$repo_root" && go build -buildvcs=false -o "$tmp/nanorouter" ./cmd/nanorouter)

write_config() {
  local file=$1 port=$2 auth_env_block=$3 manage_public=$4 db=$5
  cat >"$file" <<EOF
listen: "127.0.0.1:$port"
database:
  driver: sqlite
  dsn: "$db"
logging:
  level: ERROR
  format: json
${auth_env_block}
stats_public: false
api_keys:
  enabled: true
  manage_public: $manage_public
routes:
  smoke:
    candidates:
      - id: smoke
        base_url: "http://127.0.0.1:1"
        model: smoke
EOF
}

write_config "$tmp/public.yaml" "$public_port" "auth_env: NANOROUTER_SMOKE_KEY" true "$tmp/public.db"
write_config "$tmp/noroot.yaml" "$noroot_port" "" true "$tmp/noroot.db"
write_config "$tmp/root.yaml" "$root_port" "auth_env: NANOROUTER_SMOKE_KEY" false "$tmp/root.db"

start_instance() {
  local name=$1 config=$2 use_root=$3
  if [ "$use_root" = "true" ]; then
    HOME="$tmp/home" NANOROUTER_SMOKE_KEY=root-secret "$tmp/nanorouter" --config "$config" >"$tmp/$name.log" 2>&1 &
  else
    env -u NANOROUTER_SMOKE_KEY HOME="$tmp/home" "$tmp/nanorouter" --config "$config" >"$tmp/$name.log" 2>&1 &
  fi
  pids+=("$!")
}

wait_health() {
  local port=$1 name=$2
  python3 - "$port" "$name" <<'PY'
import sys, time, urllib.request
port, name = sys.argv[1], sys.argv[2]
for _ in range(60):
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=1) as resp:
            if resp.status == 200:
                sys.exit(0)
    except Exception:
        pass
    time.sleep(0.5)
print(f"{name} did not become healthy", file=sys.stderr)
sys.exit(1)
PY
}

start_instance public "$tmp/public.yaml" true
start_instance noroot "$tmp/noroot.yaml" false
start_instance root "$tmp/root.yaml" true

wait_health "$public_port" public
wait_health "$noroot_port" noroot
wait_health "$root_port" root

python3 "$repo_root/scripts/api-keys-smoke.py" \
  --public "http://127.0.0.1:$public_port" \
  --noroot "http://127.0.0.1:$noroot_port" \
  --root "http://127.0.0.1:$root_port" \
  --public-db "$tmp/public.db"
