// Forgo VS Code extension entry point.
//
// Modeled on the structure of the official Go extension (golang.go): it
// contributes .fgo syntax highlighting unconditionally, then starts an LSP
// client against forgopls — gopls (golang.org/x/tools/gopls) built with the
// forgo toolchain, so it links against this repo's patched go/parser,
// go/ast, and go/types (see go/types/expr.go) instead of vanilla Go's,
// which teaches it to parse and type-check ? without forking gopls's own
// source. It still doesn't understand //fgo:comptime or //fgo:macro
// (those are compiler-only), so expect false-positive diagnostics there.
//
// forgopls only type-checks; the errors the compiler finds after that
// (//fgo:nogc, with, comptime evaluation) come from building the package,
// which buildOnSave does on each save and reports as "forgo build"
// diagnostics.
//
// forgopls ships in the toolchain release tarball (see release.yml); if
// it's not found, this falls back to a plain "gopls" on PATH, which will
// also be wrong about ? (no forgo-aware go/parser/go/types), not just
// //fgo:comptime/macro.
const vscode = require("vscode");
const cp = require("child_process");
const fs = require("fs");
const os = require("os");
const path = require("path");
const { LanguageClient } = require("vscode-languageclient/node");

let client;

// languageServerEnv, with ${env:VAR} expanded so settings like PATH can
// extend rather than clobber VS Code's own environment.
function envOverlay() {
	const raw = vscode.workspace.getConfiguration("forgo").get("languageServerEnv") || {};
	const env = {};
	for (const [key, value] of Object.entries(raw)) {
		env[key] = String(value).replace(/\$\{env:([^}]+)\}/g, (_, name) => process.env[name] || "");
	}
	return env;
}

// gopls defaults to serving over stdio when given no arguments — it has no
// --stdio flag and errors out if passed one. vscode-languageclient's
// declarative `transport: TransportKind.stdio` auto-injects `--stdio`
// unconditionally, which gopls rejects, so spawn it ourselves instead and
// hand the client bare stdio streams (a ServerOptions function returning
// StreamInfo) to skip that injection entirely.
function startClient(serverPath, onFailure) {
	// forgopls/gopls shells out to "go list"/"go build" using its own
	// process environment, not the editor's — if GOROOT isn't already the
	// forgo checkout in that environment (e.g. VS Code was launched from
	// the Dock, not a shell with GOROOT exported), it'll silently resolve
	// packages against a different, unrelated Go toolchain. Set this
	// explicitly rather than relying on inheritance.
	const env = envOverlay();

	const serverOptions = () => {
		return new Promise((resolve, reject) => {
			const child = cp.spawn(serverPath, [], {
				stdio: ["pipe", "pipe", "pipe"],
				env: { ...process.env, ...env },
			});
			child.on("error", reject);
			child.stderr.on("data", (d) => console.error(`${serverPath}: ${d}`));
			resolve({ reader: child.stdout, writer: child.stdin });
		});
	};

	const clientOptions = {
		documentSelector: [{ scheme: "file", language: "forgo" }],
		outputChannel: vscode.window.createOutputChannel("Forgo Language Server"),
	};

	const c = new LanguageClient("forgo", "Forgo Language Server", serverOptions, clientOptions);
	c.start().then(undefined, (err) => onFailure(err));
	return c;
}

// Starts (or restarts) the client from current configuration, with the
// gopls-on-PATH fallback. Shared by activate() and the restart command so
// a restart picks up settings changes the same way a fresh window would.
function launch() {
	const config = vscode.workspace.getConfiguration("forgo");
	const configuredPath = config.get("languageServerPath") || "forgopls";
	const triedFallback = configuredPath !== "gopls";

	client = startClient(configuredPath, (err) => {
		if (triedFallback) {
			vscode.window.showWarningMessage(
				`forgo: couldn't start ${configuredPath} (${err.message}); falling back to gopls on PATH, ` +
					"which won't understand ? correctly (no forgo-aware go/parser/go/types). " +
					"Install forgopls from the latest release, or set forgo.languageServerPath."
			);
			client = startClient("gopls", (fallbackErr) => {
				vscode.window.showWarningMessage(
					`forgo: couldn't start gopls either (${fallbackErr.message}). ` +
						"Syntax highlighting still works; set forgo.enableLanguageServer to false to silence this."
				);
			});
		} else {
			vscode.window.showWarningMessage(
				`forgo: couldn't start language server (${configuredPath}): ${err.message}. ` +
					"Syntax highlighting still works; install forgopls/gopls or set forgo.languageServerPath, " +
					"or set forgo.enableLanguageServer to false to silence this."
			);
		}
	});
}

// The forgo command to build with: $GOROOT/bin/forgo if GOROOT (from
// languageServerEnv, else VS Code's environment) has one, else forgo on
// PATH.
function forgoCommand(env) {
	const goroot = env.GOROOT || process.env.GOROOT;
	if (goroot) {
		const p = path.join(goroot, "bin", process.platform === "win32" ? "forgo.exe" : "forgo");
		if (fs.existsSync(p)) {
			return p;
		}
	}
	return "forgo";
}

const buildDiagnostics = vscode.languages.createDiagnosticCollection("forgo build");
const builds = new Map(); // package directory -> running build
const buildFiles = new Map(); // package directory -> URIs with diagnostics

// buildPackage builds the package in dir and reports the errors in it
// that no other source (forgopls) already reports on the same line: the
// errors only the compiler finds.
function buildPackage(dir) {
	const running = builds.get(dir);
	if (running) {
		running.kill();
	}
	const env = { ...process.env, ...envOverlay() };
	const child = cp.execFile(
		forgoCommand(env),
		["build", "-gcflags=-e", "-o", os.devNull, "."],
		{ cwd: dir, env, maxBuffer: 16 << 20 },
		(err, stdout, stderr) => {
			if (builds.get(dir) !== child) {
				return; // superseded by a newer build
			}
			builds.delete(dir);
			if (err && err.code === "ENOENT") {
				vscode.window.showWarningMessage(
					"forgo: couldn't run forgo to build on save; put it on PATH, set GOROOT in forgo.languageServerEnv, " +
						"or set forgo.buildOnSave to false."
				);
				return;
			}
			for (const uri of buildFiles.get(dir) || []) {
				buildDiagnostics.delete(uri);
			}
			const byFile = new Map();
			let last;
			for (const line of stderr.split(/\r?\n/)) {
				const m = /^(.+?\.(?:fgo|go)):(\d+):(\d+): (.*)$/.exec(line);
				if (!m) {
					if (last && /^\s/.test(line) && line.trim()) {
						last.message += "\n" + line.trim();
					}
					continue;
				}
				const file = path.resolve(dir, m[1]);
				if (path.dirname(file) !== dir) {
					continue;
				}
				const pos = new vscode.Position(Number(m[2]) - 1, Number(m[3]) - 1);
				last = new vscode.Diagnostic(wordRange(file, pos), m[4], vscode.DiagnosticSeverity.Error);
				last.source = "forgo build";
				if (!byFile.has(file)) {
					byFile.set(file, []);
				}
				byFile.get(file).push(last);
			}
			const uris = [];
			for (const [file, diags] of byFile) {
				const uri = vscode.Uri.file(file);
				const others = vscode.languages
					.getDiagnostics(uri)
					.filter((d) => d.source !== "forgo build" && d.severity === vscode.DiagnosticSeverity.Error);
				const fresh = diags.filter((d) => !others.some((o) => o.range.start.line === d.range.start.line));
				buildDiagnostics.set(uri, fresh);
				uris.push(uri);
			}
			buildFiles.set(dir, uris);
		}
	);
	builds.set(dir, child);
}

// wordRange is the identifier at pos in file if it's open, else pos.
function wordRange(file, pos) {
	const doc = vscode.workspace.textDocuments.find((d) => d.uri.fsPath === file);
	return (doc && doc.getWordRangeAtPosition(pos)) || new vscode.Range(pos, pos);
}

function buildOnSave(doc) {
	if (doc.uri.scheme !== "file" || !/\.(fgo|go)$/.test(doc.uri.fsPath)) {
		return;
	}
	if (!vscode.workspace.getConfiguration("forgo").get("buildOnSave")) {
		return;
	}
	buildPackage(path.dirname(doc.uri.fsPath));
}

async function restartLanguageServer() {
	if (client) {
		await client.stop().catch(() => undefined);
		client = undefined;
	}
	const config = vscode.workspace.getConfiguration("forgo");
	if (!config.get("enableLanguageServer")) {
		vscode.window.showInformationMessage("forgo: language server is disabled (forgo.enableLanguageServer is false).");
		return;
	}
	launch();
	vscode.window.showInformationMessage("forgo: language server restarted.");
}

function activate(context) {
	context.subscriptions.push(vscode.commands.registerCommand("forgo.restartLanguageServer", restartLanguageServer));
	context.subscriptions.push(
		buildDiagnostics,
		vscode.workspace.onDidSaveTextDocument(buildOnSave),
		vscode.workspace.onDidOpenTextDocument((doc) => doc.languageId === "forgo" && buildOnSave(doc))
	);
	vscode.workspace.textDocuments.filter((doc) => doc.languageId === "forgo").forEach(buildOnSave);

	const config = vscode.workspace.getConfiguration("forgo");
	if (!config.get("enableLanguageServer")) {
		return;
	}

	launch();

	context.subscriptions.push({ dispose: () => client && client.stop() });
}

function deactivate() {
	return client ? client.stop() : undefined;
}

module.exports = { activate, deactivate };
