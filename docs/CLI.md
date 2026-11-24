# `superdb` CLI — project manager + offline toolkit

`bin/superdb` (`src/cli/superdb_tool.c`, links only against `src/core/`)
is the project-facing companion to `superdb-server` / `superdb-cli`:

- **no server needed** for day-to-day data work — commands open the `.sdb`
  file directly through the embedded engine (local-first);
- **scaffolding** — `init` a repo, `install` (vendor) the sources;
- **lifecycle** — `serve` / `start` / `stop` / `status` / `logs`;
- **environment** — `doctor`, `assoc` (`.sdb` file association), `shell`.

```sh
superdb [--db FILE] [--host ADDR] [--port N] <command> [args]
```

Leading `--db/--host/--port` flags apply to every command. Without flags,
values resolve as: flag → `$SUPERDB_DB` / `$SUPERDB_HOST` / `$SUPERDB_PORT`
→ `./superdb.json` → `./superdb.sdb` + `127.0.0.1:7381`.

## Project setup

```sh
superdb init [DIR] [--db ./data.sdb] [--host 127.0.0.1] [--port 7381] [--force] [--no-vscode]
```

Writes `superdb.json`, a `superdb_example.c` embed snippet, `.vscode/`
settings (+ extension recommendation), and `.gitignore` entries
(`*.sdb`, `*.sdb.tmp`, `.superdb.pid`, `superdb.log`).

```sh
superdb install [DIR] [--from /path/to/superdb] [--force]
```

Vendors the embeddable engine into `DIR/third_party/superdb`
(`include/` + `src/core/` + README with CMake/Make snippets). Source
root is auto-detected (walks up from cwd, honors `$SUPERDB_SRC`).

Build with it:

```sh
gcc -std=c99 -O2 -Ithird_party/superdb/include app.c third_party/superdb/src/core/*.c -o app
```

## Offline data (no server)

Work directly on the file. Text-oriented (values are argv strings;
use the server + RESP for binary payloads).

| Command | Example |
|---|---|
| `get KEY` | `superdb get greeting` → `hello` / `(nil)` (exit 1 if missing) |
| `set KEY VALUE [EX sec]` | `superdb set session:1 abc EX 3600` → `OK` |
| `del KEY...` / `exists KEY...` | → `(integer) N` |
| `keys [PAT]` | `superdb keys 'user:*'` (`*` = all) |
| `incr KEY [BY]` | → `(integer) N` |
| `expire KEY SEC` / `ttl KEY` / `pttl KEY` | `ttl` in seconds, `pttl` in ms (`-1` no TTL, `-2` missing) |
| `dbsize` / `info` | counts, file bytes, version |
| `compact` | rewrite live keys only → `OK` |
| `flushall --yes` | delete everything (requires `--yes`) |

Backup / seed / move data between machines:

```sh
superdb --db ./a.sdb dump --out backup.json
superdb --db ./b.sdb restore --in backup.json [--flush]
```

Dump format is plain JSON (`{"superdb":1,"keys":[{"k":…,"ttl_ms":…,"b64":…}]}`),
values base64-encoded, TTLs preserved. `inspect [FILE.sdb]` prints
magic/size/key-count plus a key sample without changing anything.

## Server lifecycle

```sh
superdb serve [--daemon]     # foreground (exec); --daemon detaches like `start`
superdb start                # background server, pid in ./.superdb.pid, output in ./superdb.log
superdb stop                 # terminate the pid-file process
superdb status               # process alive? + TCP probe of host:port
superdb logs [--lines 50]    # tail ./superdb.log
superdb shell                # open superdb-cli REPL against the configured server
```

`start` locates `superdb-server` next to the `superdb` binary, in `./bin`,
or on `PATH`. `stop`/`status` never need the server binary — they use the
pid file and a short-timeout TCP connect.

## Environment

```sh
superdb doctor               # checklist: binaries, config, db health, server, assoc
superdb assoc [--uninstall]  # register *.sdb
```

`assoc` behavior per OS:

- **Windows** — `HKCU\Software\Classes\.sdb` → `SuperDB.Database`, open
  command `"<path>\superdb.exe" inspect "%1"` (double-click inspects).
  `tools/assoc-windows.reg` is a template; `superdb assoc` writes your
  real install path.
- **Linux** — installs `application/x-superdb` MIME type (magic `SDB1` +
  `*.sdb` glob) and a `superdb inspect %f` desktop entry into
  `~/.local/share`, refreshes the mime/desktop databases.
- **macOS** — no CLI-registered association (needs an app bundle);
  prints the Finder + `duti` manual steps.

See `docs/VSCODE.md` for the editor side and `tools/` for full
installers (`install.sh`, `install.bat`).
