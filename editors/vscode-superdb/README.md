# SuperDB for VS Code

Local-first key-value database tooling: a `.sdb` inspector, syntax
highlighting for SuperDB command scripts (`.sdbc`), and one-click project
commands powered by the [`superdb` CLI](../../docs/CLI.md).

## Features

- **SuperDB Inspector** — open any `.sdb` file to see its header, stats, and
  key sample, with buttons for *Inspect in terminal*, *Dump to JSON*, and
  *Serve this DB*. (Right-click a `.sdb` file → *SuperDB: Inspect this database*.)
- **Command scripts** — `.sdbc` files get full highlighting for all 20+
  SuperDB commands (`SET`, `GET`, `DEL`, `KEYS`, `EXPIRE`, …). Run the open
  script with **SuperDB: Run script file**.
- **Project commands** — init, start/stop/status, dump, all from the command
  palette. Settings: `superdb.binaryPath`, `superdb.db`, `superdb.host`, `superdb.port`.
- **Status bar** — live key count for the current database.

## Requirements

Install the `superdb` CLI and put it on `PATH` (or set
`superdb.binaryPath`):

```sh
# from a SuperDB checkout
make && make install          # POSIX, PREFIX=/usr/local by default
# or Windows (MSYS2 ucrt64)
build.bat
```

`superdb init` scaffolds `superdb.json` (schema-validated by this
extension), `.vscode/` settings, and `.gitignore` entries.

## Extension settings

| Setting | Default | Meaning |
|---|---|---|
| `superdb.binaryPath` | `superdb` | Path to the project CLI |
| `superdb.db` | `""` | DB override (else `superdb.json` → first `*.sdb`) |
| `superdb.host` | `127.0.0.1` | Server host |
| `superdb.port` | `7381` | Server port |

## Release notes

See [CHANGELOG.md](CHANGELOG.md).
