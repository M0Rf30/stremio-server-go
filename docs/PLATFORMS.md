<!--
SPDX-FileCopyrightText: 2026 The rivulet-engine Authors

SPDX-License-Identifier: MIT
-->

# Platforms

`linux/{amd64,arm64,arm}`, `darwin/{amd64,arm64}`, `windows/{amd64,arm64}` all
build `CGO_ENABLED=0` as pure-Go cross-compiles. `android/{arm64,armv7,amd64,386}` are
the exception: Android ships no `/etc/resolv.conf`, so a pure-Go binary's
resolver falls back to `127.0.0.1:53` (nothing listens there) and every DNS
lookup fails, so both build `CGO_ENABLED=1` against an NDK clang, linking
against bionic's real resolver instead (`-ldflags=-checklinkname=0` is also
needed for `github.com/wlynxg/anet` on Go 1.23+). `android/arm64` also runs
as a plain `linux/arm64` binary under Termux.

## Library mode (`librivulet-engine.so`)

Android 10+ denies `exec()` of files an app downloaded into its own data
directory (SELinux W^X for targetSdk ≥ 29, i.e. Kodi 19+), but still allows
`dlopen()` of them. `cmd/librivulet-engine` therefore builds the same server with
`-buildmode=c-shared` for `android/{arm64,armv7,amd64,386}` (`make lib-android`; shipped
inside the Android release archives next to the executable). Exports:

```c
int   ServerStart(char* logPath, char* envJSON); /* blocks until stopped; 0 ok, 1 init error, 2 already running */
int   ServerStop(void);                          /* graceful, safe when idle */
char* ServerVersion(void);                       /* static, do not free */
```

`envJSON` is an object of the same environment variables the executable reads.
Keys that are read straight from the process environment (`RIVULET_LOG_LEVEL`,
`RIVULET_LOG_FORMAT`, `RIVULET_HWACCEL`, `RIVULET_ARCHIVE_ALLOW_PRIVATE`,
`RIVULET_ARCHIVE_LOCAL_ROOT`, `RIVULET_FTP_ALLOW_PRIVATE`, `LOCAL_FILES_DIR`) are
exported for the duration of `ServerStart` and restored afterwards.
`RIVULET_SERVER_VERSION` (library mode only) overrides the `serverVersion`
reported in `/settings`; `ServerVersion()` always returns the compiled default.
Start may be called again after Stop. The library never installs signal
handlers or calls `os.Exit`; Go cannot unload it (golang/go#11100), so load it
once per process. `scripts/librivulet_engine_smoke.py` exercises it through ctypes.
