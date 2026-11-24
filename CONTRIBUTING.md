# Contributing to SuperDB

## Build & test

Windows (MSYS2 ucrt64 + gcc, no make needed):
```
.\build.bat        # builds everything, runs all tests + quick benches
```

POSIX / CI (Linux, macOS, MSYS2/WSL shell):
```
make test          # core + batch + protocol tests
make bench         # perf smoke (nosync + durable batch)
```
`make` needs a unix-like shell (MSYS2, WSL, macOS, Linux). From plain
PowerShell/cmd on Windows, `build.bat` is the supported path.

CMake (any platform with CMake 3.15+):
```
cmake -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build
ctest --test-dir build
```

## Rules

- C99, zero dependencies (libc + OS sockets/threads only).
- `-Wall -Wextra` must be clean on gcc (MSVC `/W3` should be quiet too).
- Public API lives in `include/superdb.h` only. Keep it backward compatible;
  new functions are fine, changing signatures is not.
- On-disk format (`src/sdb_log.h`) is stable. If you must change it, bump
  the version and keep loading old files.
- Every behavior change needs a test: core -> `tests/test_basic.c`,
  batching -> `tests/test_batch.c`, protocol -> `tests/test_resp.c`.
- Follow the module map in `docs/ARCHITECTURE.md`: no new cross-module
  includes without a reason written in the PR.
- Update `docs/PROTOCOL.md` when touching commands, `README.md` benchmarks
  when touching the hot path (`bench/bench_sdb` repro commands included).

## Style

- 4-space indent, `sdb_` prefix for internals, `superdb_` for public API.
- `_locked` suffix for functions requiring the db mutex.
- Prefer small, reviewable diffs; one module per commit when refactoring.
