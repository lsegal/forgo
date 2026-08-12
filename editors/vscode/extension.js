// Forgo VS Code extension entry point.
//
// Modeled on the structure of the official Go extension (golang.go): it
// contributes .fgo syntax highlighting unconditionally, then optionally
// starts an LSP client against gopls for the language features gopls can
// still provide on forgo source (imports, most diagnostics) — it doesn't
// understand forgo-specific syntax (?, //forgo:comptime, //forgo:macro),
// so those will surface as parse errors from gopls until forgo ships its
// own language server.
const vscode = require("vscode");
const { LanguageClient, TransportKind } = require("vscode-languageclient/node");

let client;

function activate(context) {
	const config = vscode.workspace.getConfiguration("forgo");
	if (!config.get("enableLanguageServer")) {
		return;
	}

	const serverPath = config.get("languageServerPath") || "gopls";

	const serverOptions = {
		run: { command: serverPath, transport: TransportKind.stdio },
		debug: { command: serverPath, transport: TransportKind.stdio },
	};

	const clientOptions = {
		documentSelector: [{ scheme: "file", language: "forgo" }],
		outputChannel: vscode.window.createOutputChannel("Forgo Language Server"),
	};

	client = new LanguageClient("forgo", "Forgo Language Server", serverOptions, clientOptions);

	client.start().then(undefined, (err) => {
		vscode.window.showWarningMessage(
			`forgo: couldn't start language server (${serverPath}): ${err.message}. ` +
				"Syntax highlighting still works; install gopls or set forgo.languageServerPath, " +
				"or set forgo.enableLanguageServer to false to silence this."
		);
	});

	context.subscriptions.push({ dispose: () => client && client.stop() });
}

function deactivate() {
	return client ? client.stop() : undefined;
}

module.exports = { activate, deactivate };
