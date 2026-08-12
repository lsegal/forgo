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
// forgopls ships in the toolchain release tarball (see release.yml); if
// it's not found, this falls back to a plain "gopls" on PATH, which will
// also be wrong about ? (no forgo-aware go/parser/go/types), not just
// //fgo:comptime/macro.
const vscode = require("vscode");
const cp = require("child_process");
const { LanguageClient } = require("vscode-languageclient/node");

let client;

// gopls defaults to serving over stdio when given no arguments — it has no
// --stdio flag and errors out if passed one. vscode-languageclient's
// declarative `transport: TransportKind.stdio` auto-injects `--stdio`
// unconditionally, which gopls rejects, so spawn it ourselves instead and
// hand the client bare stdio streams (a ServerOptions function returning
// StreamInfo) to skip that injection entirely.
function startClient(serverPath, onFailure) {
	const config = vscode.workspace.getConfiguration("forgo");
	// forgopls/gopls shells out to "go list"/"go build" using its own
	// process environment, not the editor's — if GOROOT isn't already the
	// forgo checkout in that environment (e.g. VS Code was launched from
	// the Dock, not a shell with GOROOT exported), it'll silently resolve
	// packages against a different, unrelated Go toolchain. Set this
	// explicitly rather than relying on inheritance.
	const rawEnvOverlay = config.get("languageServerEnv") || {};
	// Expand ${env:VAR} so settings like PATH can extend rather than
	// clobber VS Code's own environment.
	const envOverlay = {};
	for (const [key, value] of Object.entries(rawEnvOverlay)) {
		envOverlay[key] = String(value).replace(/\$\{env:([^}]+)\}/g, (_, name) => process.env[name] || "");
	}

	const serverOptions = () => {
		return new Promise((resolve, reject) => {
			const child = cp.spawn(serverPath, [], {
				stdio: ["pipe", "pipe", "pipe"],
				env: { ...process.env, ...envOverlay },
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

function activate(context) {
	const config = vscode.workspace.getConfiguration("forgo");
	if (!config.get("enableLanguageServer")) {
		return;
	}

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

	context.subscriptions.push({ dispose: () => client && client.stop() });
}

function deactivate() {
	return client ? client.stop() : undefined;
}

module.exports = { activate, deactivate };
