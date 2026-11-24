# SuperDB in VS Code

Extension source: `editors/vscode-superdb/` (dependency-free: plain Node,
only the `vscode` API). No build step — it runs straight from the folder.

## Install

**Option A — from this repo (no packaging tools needed):**

```sh
# Windows (PowerShell)
Copy-Item -Recurse editors/vscode-superdb "$HOME/.vscode/extensions/superdb.vscode-superdb-0.1.0"
# POSIX
cp -r editors/vscode-superdb ~/.vscode/extensions/superdb.vscode-superdb-0.1.0
```

`tools/install.sh` / `tools/install.bat` do this for you (plus binaries
and OS file association). Then reload VS Code.

**Option B — package once, share the file:**

```sh
npm install -g @vscode/vsce
vsce package --out superdb-0.1.0.vsix   # run inside editors/vscode-superdb/
code --install-extension superdb-0.1.0.vsix
```

## What you get

- **`.sdb` inspector** — open any database file: header/magic check, file
  stats, key sample, and one-click *Inspect in terminal*, *Dump to JSON*,
  *Serve this DB*. Also via right-click → *SuperDB: Inspect this database*.
  (Registered with `"priority": "option"`, so the default binary viewer
  stays available.)
- **`.sdbc` scripts** — TextMate grammar + language configuration for
  SuperDB command files. Try `examples/demo.sdbc`, then
  *SuperDB: Run script file* to execute each line against your database.
- **`superdb.json` schema** — validation + completion for the project
  config written by `superdb init`.
- **Commands** — init, inspect, start/stop/status, dump (palette:
  `SuperDB: …`). They shell out to the `superdb` CLI — set
  `superdb.binaryPath` if it isn't on `PATH`.
- **Status bar** — live key count for the resolved database (refreshes
  every 30 s).

Database resolution order: `superdb.db` setting → `./superdb.json`
(`db` field, relative to the workspace) → first `*.sdb` in the workspace
→ `./superdb.sdb`.

## Settings

| Setting | Default | Meaning |
|---|---|---|
| `superdb.binaryPath` | `superdb` | Path to the project CLI |
| `superdb.db` | `""` | DB override |
| `superdb.host` | `127.0.0.1` | Server host |
| `superdb.port` | `7381` | Server port |

## Files

```
editors/vscode-superdb/
  package.json                 contributes: language, grammar, custom editor,
                               commands, configuration, jsonValidation
  extension.js                 inspector provider + commands + status bar
  language-configuration.json  brackets, comments, auto-closing
  syntaxes/superdb.tmGrammar.json
  schemas/superdb-config.schema.json
  images/sdb-icon.svg
```
