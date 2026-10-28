#!/usr/bin/env python3
"""API Key 三模式冒烟断言。

由 scripts/api-keys-smoke.sh 启动三个真实网关实例后调用：
  - public: auth_env + api_keys.manage_public=true
  - noroot: 无 auth_env + api_keys.manage_public=true
  - root:   auth_env + api_keys.manage_public=false

只使用标准库，不依赖 jq/curl。完整 Key 只保存在内存里，不打印。
"""

import argparse
import json
import sqlite3
import sys
import urllib.error
import urllib.request

ROOT_KEY = "root-secret"
FAILURES = []


def parse_json(raw):
    try:
        return json.loads(raw)
    except Exception:
        return None


def request(method, url, key=None, body=None, content_type="application/json", extra_headers=None):
    data = body.encode() if body is not None else None
    headers = {"Accept": "application/json"}
    if body is not None:
        headers["Content-Type"] = content_type
    if key:
        headers["Authorization"] = "Bearer " + key
    if extra_headers:
        headers.update(extra_headers)
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            raw = resp.read().decode()
            return resp.status, {k.lower(): v for k, v in resp.headers.items()}, parse_json(raw), raw
    except urllib.error.HTTPError as err:
        raw = err.read().decode()
        return err.code, {k.lower(): v for k, v in err.headers.items()}, parse_json(raw), raw


def expect(name, got, want):
    if got != want:
        FAILURES.append(f"{name}: got {got}, want {want}")
        print(f"FAIL {name}: got {got}, want {want}")
    else:
        print(f"ok   {name} = {got}")


def expect_true(name, condition, detail=""):
    if not condition:
        FAILURES.append(f"{name}: {detail}")
        print(f"FAIL {name}: {detail}")
    else:
        print(f"ok   {name}")


def create_key(base, key=None):
    return request("POST", base + "/v1/api-keys", key=key, body='{"name":"smoke"}')


def test_public(base, public_db):
    status, headers, body, raw = request("GET", base + "/v1/api-keys")
    expect("public anonymous list", status, 200)
    expect_true("public list no-store", headers.get("cache-control") == "no-store", headers)
    expect_true("public list empty", body is not None and body.get("keys") == [], body)
    expect_true("public manage_public", body is not None and body.get("manage_public") is True, body)
    expect_true("public root_auth", body is not None and body.get("root_auth") is True, body)

    status, headers, body, raw = create_key(base)
    expect("public anonymous create", status, 201)
    expect_true("public create no-store", headers.get("cache-control") == "no-store", headers)
    key = body.get("key") if body else None
    key_id = body.get("id") if body else None
    prefix = body.get("prefix") if body else None
    expect_true("public key shape", bool(key) and key.startswith("nr_") and prefix == key[:12], body)

    status, _, body, raw = request("GET", base + "/v1/api-keys")
    expect("public list after create", status, 200)
    expect_true("public list hides key", key not in raw, raw)
    expect_true("public list active", body is not None and body["keys"][0]["status"] == "active", body)

    expect("public managed models", request("GET", base + "/v1/models", key=key)[0], 200)
    expect("public managed x-api-key", request("GET", base + "/v1/models", extra_headers={"X-Api-Key": key})[0], 200)
    for path in ["/stats", "/v1/stats/summary", "/v1/zcode/usage", "/v1/zcode/reset/status", "/metrics"]:
        expect("public managed scope " + path, request("GET", base + path, key=key)[0], 403)
    expect("public managed admin", request("GET", base + "/v1/api-keys", key=key)[0], 200)
    expect("public root stats", request("GET", base + "/stats", key=ROOT_KEY)[0], 200)
    expect("public bad content type", request("POST", base + "/v1/api-keys", body='{"name":"x"}', content_type="text/plain")[0], 415)
    expect("public empty name", request("POST", base + "/v1/api-keys", body='{"name":""}')[0], 400)

    expect("public revoke", request("DELETE", base + "/v1/api-keys/" + key_id)[0], 200)
    expect("public revoked models", request("GET", base + "/v1/models", key=key)[0], 401)
    status, _, body, _ = request("DELETE", base + "/v1/api-keys/" + key_id)
    expect("public revoke again", status, 200)
    expect_true("public already revoked", body is not None and body.get("already_revoked") is True, body)
    expect("public revoke missing", request("DELETE", base + "/v1/api-keys/missing")[0], 404)

    conn = sqlite3.connect(f"file:{public_db}?mode=ro", uri=True)
    try:
        rows = conn.execute("SELECT key_hash, key_prefix FROM api_keys").fetchall()
    finally:
        conn.close()
    expect_true("db has one row", len(rows) == 1, rows)
    expect_true("db hash length", bool(rows) and len(rows[0][0]) == 64, rows)
    expect_true("db no plaintext", bool(rows) and rows[0][0] != key and key not in rows[0][0], rows)
    expect_true("db prefix", bool(rows) and rows[0][1] == prefix, rows)


def test_noroot(base):
    expect("noroot anonymous models", request("GET", base + "/v1/models")[0], 401)
    status, _, body, _ = request("GET", base + "/v1/api-keys")
    expect("noroot anonymous list", status, 200)
    expect_true("noroot root_auth false", body is not None and body.get("root_auth") is False, body)

    status, _, body, _ = create_key(base)
    expect("noroot anonymous create", status, 201)
    key = body["key"]
    key_id = body["id"]
    expect("noroot managed models", request("GET", base + "/v1/models", key=key)[0], 200)
    expect("noroot managed stats", request("GET", base + "/stats", key=key)[0], 403)
    expect("noroot anonymous revoke", request("DELETE", base + "/v1/api-keys/" + key_id)[0], 200)
    expect("noroot revoked models", request("GET", base + "/v1/models", key=key)[0], 401)


def test_root(base):
    expect("root anonymous list", request("GET", base + "/v1/api-keys")[0], 401)
    expect("root list", request("GET", base + "/v1/api-keys", key=ROOT_KEY)[0], 200)
    status, _, body, _ = create_key(base, key=ROOT_KEY)
    expect("root create", status, 201)
    key = body["key"]
    expect("root managed admin", request("GET", base + "/v1/api-keys", key=key)[0], 403)
    expect("root managed models", request("GET", base + "/v1/models", key=key)[0], 200)
    expect("root anonymous models", request("GET", base + "/v1/models")[0], 401)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--public", required=True)
    parser.add_argument("--noroot", required=True)
    parser.add_argument("--root", required=True)
    parser.add_argument("--public-db", required=True)
    args = parser.parse_args()

    test_public(args.public.rstrip("/"), args.public_db)
    test_noroot(args.noroot.rstrip("/"))
    test_root(args.root.rstrip("/"))

    print()
    if FAILURES:
        print(f"{len(FAILURES)} FAILURES")
        return 1
    print("ALL API KEY SMOKE CHECKS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
