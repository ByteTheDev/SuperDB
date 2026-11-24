/* SuperDB VS Code extension — no dependencies, plain Node + vscode API.
 *
 * - Custom read-only inspector for *.sdb files (header, stats, key sample,
 *   with buttons that run the `superdb` CLI in a terminal).
 * - Commands: init / start / stop / status / dump / run .sdbc script.
 * - Status-bar item showing the current db + key count.
 */
'use strict';

const vscode = require('vscode');
const cp = require('child_process');
const fs = require('fs');
const path = require('path');

function cfg() {
  const c = vscode.workspace.getConfiguration('superdb');
  return {
    bin: c.get('binaryPath', 'superdb') || 'superdb',
    db: c.get('db', '') || '',
    host: c.get('host', '127.0.0.1') || '127.0.0.1',
    port: c.get('port', 7381) || 7381
  };
}

function workspaceFolder() {
  const f = vscode.workspace.workspaceFolders;
  return f && f.length ? f[0].uri.fsPath : '';
}

/** Resolve which .sdb file commands should target. */
function resolveDb() {
  const c = cfg();
  if (c.db) return { db: c.db, cwd: workspaceFolder() || undefined };
  const root = workspaceFolder();
  if (root) {
    const cfgPath = path.join(root, 'superdb.json');
    try {
      const j = JSON.parse(fs.readFileSync(cfgPath, 'utf8'));
      if (j && j.db) {
        const db = path.isAbsolute(j.db) ? j.db : path.join(root, j.db);
        return { db, cwd: root };
      }
    } catch (e) { /* no config — fall through */ }
    try {
      const hit = fs.readdirSync(root).find((f) => f.endsWith('.sdb'));
      if (hit) return { db: path.join(root, hit), cwd: root };
    } catch (e) { /* ignore */ }
  }
  return { db: './superdb.sdb', cwd: root || undefined };
}

function term(name) {
  const t = vscode.window.createTerminal(name || 'SuperDB');
  t.show();
  return t;
}

function runInTerminal(line, cwd) {
  const t = term('SuperDB');
  if (cwd) t.sendText(`cd "${cwd}"`, true);
  t.sendText(line, true);
}

function execTool(args, cwd) {
  const c = cfg();
  return new Promise((resolve) => {
    cp.execFile(c.bin, args, { cwd: cwd || workspaceFolder() || undefined, timeout: 15000 },
      (err, stdout, stderr) => resolve({ err, stdout: String(stdout || ''), stderr: String(stderr || '') }));
  });
}

function escapeHtml(s) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

/* ---------------- .sdb inspector (custom read-only editor) ---------------- */

class InspectorProvider {
  constructor(context) {
    this.context = context;
  }

  openCustomDocument(uri) {
    return { uri, dispose() {} };
  }

  async resolveCustomEditor(document, webviewPanel) {
    const file = document.uri.fsPath;
    webviewPanel.webview.options = { enableScripts: true };
    webviewPanel.webview.html = this.loadingHtml(file);

    const update = async () => {
      webviewPanel.webview.html = await this.renderHtml(file);
    };
    webviewPanel.webview.onDidReceiveMessage(async (msg) => {
      const c = cfg();
      if (msg.type === 'refresh') return update();
      if (msg.type === 'inspect') return runInTerminal(`"${c.bin}" inspect "${file}"`);
      if (msg.type === 'dump') {
        const out = file + '.dump.json';
        return runInTerminal(`"${c.bin}" --db "${file}" dump --out "${out}"`);
      }
      if (msg.type === 'serve') {
        return runInTerminal(`"${c.bin}" --db "${file}" serve`);
      }
    });
    await update();
  }

  loadingHtml(file) {
    return `<html><body><p>Loading ${escapeHtml(file)}…</p></body></html>`;
  }

  async renderHtml(file) {
    let stat = null, head = '', keys = [], countLine = '';
    try { stat = fs.statSync(file); } catch (e) { /* gone */ }
    if (!stat) {
      return `<html><body><p>File not found: ${escapeHtml(file)}</p></body></html>`;
    }
    try {
      const fd = fs.openSync(file, 'r');
      const buf = Buffer.alloc(4);
      fs.readSync(fd, buf, 0, 4, 0);
      fs.closeSync(fd);
      head = buf.toString('latin1');
    } catch (e) { head = ''; }
    const valid = head === 'SDB1';
    const cwd = path.dirname(file);
    const info = await execTool(['--db', file, 'info'], cwd);
    if (!info.err) countLine = info.stdout.trim().split('\n').slice(0, 8).join('\n');
    const ks = await execTool(['--db', file, 'keys', '*'], cwd);
    if (!ks.err) {
      keys = ks.stdout.split('\n').map((s) => s.trim()).filter((s) => s && s !== '(empty array)').slice(0, 50);
    }
    const rows = keys.map((k) => `<li><code>${escapeHtml(k)}</code></li>`).join('') || '<li><i>(no keys)</i></li>';
    return `<!DOCTYPE html>
<html><head><meta charset="utf-8">
<style>
body { font-family: var(--vscode-font-family); padding: 16px; }
code, pre { font-family: var(--vscode-editor-font-family); }
.badge { display:inline-block; padding:2px 8px; border-radius:9px; font-size:12px;
  background: ${valid ? 'var(--vscode-testing-iconPassed, #4caf50)' : 'var(--vscode-testing-iconFailed, #f44336)'}; color:#fff; }
button { margin: 4px 8px 4px 0; padding: 6px 12px; cursor: pointer; }
pre { background: var(--vscode-textBlockQuote-background, #222); padding: 8px; border-radius: 4px; }
ul { columns: 2; }
</style></head>
<body>
<h2>SuperDB Inspector</h2>
<p><code>${escapeHtml(file)}</code></p>
<p><span class="badge">${valid ? 'valid SuperDB v1' : 'NOT a SuperDB file'}</span>
&nbsp;${stat.size} bytes</p>
${countLine ? `<h3>info</h3><pre>${escapeHtml(countLine)}</pre>` : ''}
<h3>keys (first ${keys.length})</h3>
<ul>${rows}</ul>
<p>
<button id="b-refresh">Refresh</button>
<button id="b-inspect">Inspect in terminal</button>
<button id="b-dump">Dump to JSON</button>
<button id="b-serve">Serve this DB</button>
</p>
<script>
const vscode = acquireVsCodeApi();
for (const [id, type] of [['b-refresh','refresh'],['b-inspect','inspect'],['b-dump','dump'],['b-serve','serve']]) {
  document.getElementById(id).addEventListener('click', () => vscode.postMessage({ type }));
}
</script>
</body></html>`;
  }
}

/* ---------------- commands ---------------- */

async function cmdInit() {
  const root = workspaceFolder();
  if (!root) {
    vscode.window.showErrorMessage('SuperDB: open a folder first, then run init.');
    return;
  }
  runInTerminal(`"${cfg().bin}" init .`, root);
}

async function cmdInspectFile(uri) {
  const file = uri && uri.fsPath ? uri.fsPath : (resolveDb().db || '');
  if (!file) {
    vscode.window.showErrorMessage('SuperDB: no .sdb file found.');
    return;
  }
  try {
    await vscode.commands.executeCommand('vscode.openWith',
      vscode.Uri.file(file), 'superdb.inspector');
  } catch (e) {
    runInTerminal(`"${cfg().bin}" inspect "${file}"`);
  }
}

async function cmdInspectActive() {
  const doc = vscode.window.activeTextEditor && vscode.window.activeTextEditor.document;
  if (doc && doc.fileName.endsWith('.sdb')) return cmdInspectFile(doc.uri);
  return cmdInspectFile();
}

async function cmdStart() {
  const { db, cwd } = resolveDb();
  runInTerminal(`"${cfg().bin}" --db "${db}" start`, cwd);
}

async function cmdStop() {
  runInTerminal(`"${cfg().bin}" stop`, workspaceFolder() || undefined);
}

async function cmdStatus() {
  const { db, cwd } = resolveDb();
  const c = cfg();
  const r = await execTool(['--db', db, '--host', c.host, '--port', String(c.port), 'status'], cwd);
  vscode.window.showInformationMessage(r.err ? 'SuperDB: stopped' : ('SuperDB: running\n' + r.stdout));
}

async function cmdDump() {
  const { db, cwd } = resolveDb();
  const out = await vscode.window.showInputBox({
    prompt: 'Dump destination JSON file',
    value: db + '.dump.json'
  });
  if (!out) return;
  runInTerminal(`"${cfg().bin}" --db "${db}" dump --out "${out}"`, cwd);
}

async function cmdRunScript() {
  const doc = vscode.window.activeTextEditor && vscode.window.activeTextEditor.document;
  if (!doc) {
    vscode.window.showErrorMessage('SuperDB: open a .sdbc script first.');
    return;
  }
  const { db, cwd } = resolveDb();
  const c = cfg();
  const lines = doc.getText().split(/\r?\n/)
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith('#') && !l.startsWith(';'));
  if (!lines.length) {
    vscode.window.showWarningMessage('SuperDB: script has no commands.');
    return;
  }
  const t = term('SuperDB');
  if (cwd) t.sendText(`cd "${cwd}"`, true);
  for (const line of lines) {
    t.sendText(`"${c.bin}" --db "${db}" ${line}`, true);
  }
}

async function updateStatusBar(item) {
  const { db } = resolveDb();
  const r = await execTool(['--db', db, 'dbsize'], path.dirname(db) === '.' ? workspaceFolder() : path.dirname(db));
  const m = r.stdout.match(/\(integer\)\s+(\d+)/);
  item.text = m ? `$(database) SuperDB: ${m[1]} keys` : '$(database) SuperDB';
  item.tooltip = db;
  item.command = 'superdb.serverStatus';
  item.show();
}

/* ---------------- activation ---------------- */

function activate(context) {
  context.subscriptions.push(
    vscode.window.registerCustomEditorProvider('superdb.inspector',
      new InspectorProvider(context), { supportsMultipleEditorsPerDocument: false }),
    vscode.commands.registerCommand('superdb.init', cmdInit),
    vscode.commands.registerCommand('superdb.inspectFile', cmdInspectFile),
    vscode.commands.registerCommand('superdb.inspectActive', cmdInspectActive),
    vscode.commands.registerCommand('superdb.startServer', cmdStart),
    vscode.commands.registerCommand('superdb.stopServer', cmdStop),
    vscode.commands.registerCommand('superdb.serverStatus', cmdServerStatusSafe),
    vscode.commands.registerCommand('superdb.dumpDatabase', cmdDump),
    vscode.commands.registerCommand('superdb.runScript', cmdRunScript)
  );
  const item = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 100);
  context.subscriptions.push(item);
  updateStatusBar(item);
  const refresh = setInterval(() => updateStatusBar(item), 30000);
  context.subscriptions.push({ dispose() { clearInterval(refresh); } });

  async function cmdServerStatusSafe() {
    try { await cmdStatus(); } catch (e) {
      vscode.window.showErrorMessage('SuperDB: status failed: ' + e.message);
    }
  }
}

function deactivate() {}

module.exports = { activate, deactivate };
