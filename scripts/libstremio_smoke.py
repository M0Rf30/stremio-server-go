#!/usr/bin/env python3
"""ctypes smoke test for libstremio-server.so (built from ./cmd/libstremio).

Loads the shared library, starts the server on a background thread (per the
Contract, ServerStart BLOCKS until stopped), polls the configured HTTP port
with a raw socket connect (no HTTP client, no curl), asks ServerStop for a
graceful shutdown, and repeats once more to prove the
Start -> Stop -> Start -> Stop restart contract holds through the C ABI the
same way internal/app.TestRunStartStopTwice proves it for the Go API.

Usage:
    python3 scripts/libstremio_smoke.py /path/to/libstremio.so
"""
import ctypes
import json
import socket
import sys
import tempfile
import threading
import time

HOST = "127.0.0.1"
PORT = 19470  # fixed high port for this smoke test only; not user-configurable


def wait_for_port(host, port, timeout=15.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with socket.create_connection((host, port), timeout=0.5):
                return True
        except OSError:
            time.sleep(0.1)
    return False


def wait_for_port_closed(host, port, timeout=15.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with socket.create_connection((host, port), timeout=0.5):
                pass
            time.sleep(0.1)
        except OSError:
            return True
    return False


def main():
    if len(sys.argv) != 2:
        print("usage: libstremio_smoke.py /path/to/libstremio.so", file=sys.stderr)
        return 2
    lib_path = sys.argv[1]

    lib = ctypes.CDLL(lib_path)
    lib.ServerVersion.restype = ctypes.c_char_p
    lib.ServerVersion.argtypes = []
    lib.ServerStart.restype = ctypes.c_int
    lib.ServerStart.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
    lib.ServerStop.restype = ctypes.c_int
    lib.ServerStop.argtypes = []

    version = lib.ServerVersion()
    if not version:
        print("FAIL: ServerVersion returned NULL", file=sys.stderr)
        return 1
    print("ServerVersion:", version.decode())

    with tempfile.TemporaryDirectory() as tmp:
        log_path = (tmp + "/log.txt").encode()
        env = {
            "APP_PATH": tmp,
            "HTTP_PORT": str(PORT),
            "HTTPS_PORT": "0",
            "BT_LISTEN_PORT": "0",
            "STREMIO_DISABLE_TRACKERS": "1",
            "STREMIO_DISABLE_WEBTORRENT": "1",
            "STREMIO_TRACKERS_URL": "off",
            "STREMIO_METADATA_URL": "off",
            "STREMIO_ENABLE_DLNA": "0",
        }
        env_json = json.dumps(env).encode()

        for cycle in range(1, 3):
            print(f"-- cycle {cycle}: ServerStart --")
            result = {}

            def run():
                result["code"] = lib.ServerStart(log_path, env_json)

            t = threading.Thread(target=run, daemon=True)
            t.start()

            if not wait_for_port(HOST, PORT):
                print("FAIL: port never opened", file=sys.stderr)
                lib.ServerStop()
                t.join(timeout=10)
                return 1
            print(f"port {PORT} is accepting connections")

            # A second, concurrent ServerStart must be rejected with 2
            # ("already running"), not block or corrupt the first run's state.
            already = lib.ServerStart(log_path, env_json)
            if already != 2:
                print(f"FAIL: concurrent ServerStart returned {already}, want 2", file=sys.stderr)
                lib.ServerStop()
                t.join(timeout=10)
                return 1
            print("re-entrant ServerStart correctly rejected (2 = already running)")

            stop_code = lib.ServerStop()
            if stop_code != 0:
                print(f"FAIL: ServerStop returned {stop_code}, want 0", file=sys.stderr)
                return 1

            t.join(timeout=15)
            if t.is_alive():
                print("FAIL: ServerStart did not return after ServerStop", file=sys.stderr)
                return 1
            if result.get("code") != 0:
                print(f"FAIL: ServerStart returned {result.get('code')}, want 0", file=sys.stderr)
                return 1

            if not wait_for_port_closed(HOST, PORT, timeout=5.0):
                print("FAIL: port still accepting connections after ServerStop", file=sys.stderr)
                return 1

            print(f"cycle {cycle}: clean start/stop OK")

        # ServerStop when nothing is running must be a safe no-op (returns 0).
        idle_stop = lib.ServerStop()
        if idle_stop != 0:
            print(f"FAIL: ServerStop with nothing running returned {idle_stop}, want 0", file=sys.stderr)
            return 1
        print("ServerStop with nothing running: OK (0)")

    print("ALL OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
