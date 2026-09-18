/**
 * The interactive chat TUI, built on pi's own terminal framework
 * (@earendil-works/pi-tui): a scrollable chat pane, the working indicator
 * embedded in the separator rule (no layout change for streaming), the
 * editor with slash-command autocomplete, and the status line.
 *
 * Behaviour ported from cmd/tui.go (Go/bubbletea): submit/stream/finish
 * lifecycle, /resume of filtered partials, session persistence and
 * transcripts.
 */
import {
	Editor,
	Markdown,
	ProcessTerminal,
	ScrollView,
	Text,
	TuiMainScreen,
	VStack,
	type Component,
} from "@earendil-works/pi-tui";
import chalk from "chalk";
import type { DeepSeekClient } from "../../core/deepseek/client.js";
import {
	effectiveModel,
	resolveDefaultSession,
	loadSavedSession,
	persistConversation,
	recoverStaleSession,
	saveSession,
	splitConversation,
} from "../../core/session.js";
import { appendTranscript, loadTranscript, transcriptsEnabled } from "../../core/transcript.js";
import { localSessionRows, sessionRowText } from "../../cli/commands/session.js";
import { oneTurn, resumePrompt, toggleState } from "../../cli/commands/chat.js";
import { stderrNote } from "../../ui/notes.js";
import {
	ruleText,
	statusLine,
	TUI_COMMANDS,
	SPINNER_FRAMES,
} from "./parts.js";

const SPINNER_INTERVAL_MS = 120;

export interface ChatTuiOptions {
	cfgPath: string;
	conversation: string;
	model: string;
	thinking: boolean;
	search: boolean;
	persist: boolean;
	noTranscript: boolean;
	workdir: string;
	token: string;
	cookie: string;
	userAgent: string;
	/** Test hook: overrides the API base URL. */
	clientBase?: string;
}

/** Autocomplete provider for the editor's slash commands. */
class SlashProvider {
	getSuggestions(input: string): Array<{ label: string; description?: string }> {
		if (!input.startsWith("/")) return [];
		const word = input.split(" ")[0] ?? input;
		return TUI_COMMANDS.filter((c) => c.startsWith(word)).map((c) => ({ label: c }));
	}
}

/**
 * ChatTui wires the components together. All stream/session behaviour is
 * shared with the REPL (oneTurn, recoverStaleSession); this class only
 * renders and routes.
 */
export class ChatTui {
	private readonly tui: TuiMainScreen;
	private readonly chat: VStack;
	private readonly scroller: ScrollView;
	private readonly rule: Text;
	private readonly status: Text;
	private readonly editor: Editor;
	private busy = false;
	private spin = 0;
	private spinnerTimer: NodeJS.Timeout | undefined;
	private turns = 0;
	private model: string;
	private thinking: boolean;
	private search: boolean;
	private conversation: string;
	private lastPartial = "";
	private firstTurn: boolean;

	constructor(
		private readonly client: DeepSeekClient,
		private readonly opts: ChatTuiOptions,
	) {
		this.model = effectiveModel(opts.model);
		this.thinking = opts.thinking;
		this.search = opts.search;
		this.conversation = opts.conversation;
		this.tui = new TuiMainScreen(new ProcessTerminal());
		this.chat = new VStack();
		this.scroller = new ScrollView(this.chat, { follow: "end" });
		this.rule = new Text("");
		this.status = new Text("");
		this.editor = new Editor(this.tui, {
			borderColor: (s) => chalk.dim(s),
			selectList: {
				selectedPrefix: (t) => chalk.hex("#12c78f")(t),
				selectedText: (t) => chalk.bold(t),
				description: (t) => chalk.dim(t),
				scrollInfo: (t) => chalk.dim(t),
				noMatch: (t) => chalk.dim(t),
			},
		});
		this.editor.setPaddingX(1);
		this.editor.setAutocompleteProvider(new SlashProvider() as never);
		this.firstTurn = false;

		this.tui.addChild(this.scroller);
		this.tui.addChild(this.rule);
		this.tui.addChild(this.editor);
		this.tui.addChild(this.status);
		this.editor.onSubmit = (text: string) => void this.submit(text);

		if (this.conversation === "") {
			void this.initSession();
		}
	}

	private async initSession(): Promise<void> {
		const resolved = await resolveDefaultSession(this.client, this.opts.cfgPath, this.opts.persist);
		this.conversation = resolved.sessionId;
		this.firstTurn = resolved.trusted;
		if (resolved.cleanup && !this.opts.persist) {
			// The TUI owns the ephemeral session; delete it on exit.
			this.owned.push(this.conversation);
		}
	}

	private owned: string[] = [];

	/** addNote appends dimmed system text (slash feedback, hints). */
	private addNote(s: string): void {
		this.chat.addChild(new Text(chalk.dim(s)));
		this.scroller.scrollToEnd?.();
		this.tui.requestRender();
	}

	/** addUser / addAssistant append chat content. */
	private addUser(text: string): void {
		this.chat.addChild(new Text(chalk.hex("#6b50ff").bold(`> ${text.split("\n")[0]}`)));
	}

	private addAssistant(): Markdown {
		const md = new Markdown("", 0, 0, {
			heading: (t) => chalk.bold(t),
			link: (t) => chalk.hex("#7aa2f7")(t),
			linkUrl: (t) => chalk.dim(t),
			code: (t) => chalk.hex("#9ece6a")(t),
			codeBlock: (t) => t,
			codeBlockBorder: (t) => chalk.dim(t),
			quote: (t) => t,
			quoteBorder: (t) => chalk.dim(t),
			hr: (t) => chalk.dim(t),
			listBullet: (t) => chalk.hex("#12c78f")(t),
			bold: (t) => chalk.bold(t),
			italic: (t) => chalk.italic(t),
			strikethrough: (t) => chalk.strikethrough(t),
			underline: (t) => chalk.underline(t),
		});
		this.chat.addChild(md as unknown as Component);
		return md;
	}

	private refreshChrome(): void {
		const mode = this.opts.conversation !== "" ? "continuing" : this.opts.persist ? "persisted" : "ephemeral";
		this.rule.setText(ruleText(this.busy, this.spin, this.tui.terminal.columns ?? 80, {
			dim: (s) => chalk.dim(s),
			accent: (s) => chalk.hex("#12c78f")(s),
			muted: (s) => chalk.hex("#858392")(s),
		}));
		this.status.setText(chalk.dim(statusLine({
			model: this.model,
			thinking: this.thinking,
			search: this.search,
			mode,
			turn: this.turns,
			conversation: this.conversation,
		})));
		this.tui.requestRender();
	}

	private startSpinner(): void {
		this.busy = true;
		this.refreshChrome();
		this.spinnerTimer = setInterval(() => {
			this.spin = (this.spin + 1) % SPINNER_FRAMES.length;
			this.refreshChrome();
		}, SPINNER_INTERVAL_MS);
	}

	private stopSpinner(): void {
		this.busy = false;
		if (this.spinnerTimer) clearInterval(this.spinnerTimer);
		this.spinnerTimer = undefined;
		this.refreshChrome();
	}

	private async submit(raw: string): Promise<void> {
		const text = raw.trim();
		this.editor.setText("");
		if (text === "") return;
		if (text.startsWith("/")) {
			await this.command(text);
			return;
		}

		// A reset (/new, /model) leaves conversation empty: spawn a fresh session.
		if (this.conversation === "") {
			try {
				this.conversation = await this.client.createChatSession();
			} catch (err) {
				this.addNote(`error: create chat session: ${err instanceof Error ? err.message : err}`);
				return;
			}
			if (this.opts.persist) {
				const err = saveSession(this.opts.cfgPath, this.conversation);
				if (err) this.addNote(`warning: could not save session: ${err.message}`);
			} else {
				this.owned.push(this.conversation);
			}
		}

		this.addUser(text);
		const md = this.addAssistant();
		this.startSpinner();

		const transcriptsOn = transcriptsEnabled(this.opts.cfgPath, this.opts.persist, this.opts.noTranscript);
		if (transcriptsOn) appendTranscript(this.opts.cfgPath, this.conversation, "user", text);

		let replyBuf = "";
		let first = true;
		let convId = this.conversation;
		let filtered = false;
		const r = await recoverStaleSession(this.client, this.opts.cfgPath, this.conversation, this.firstTurn, async (sid) => {
			const t = await oneTurn(this.client, sid, text, this.model, this.thinking, this.search, (delta) => {
				replyBuf += delta;
				md.setText(first ? delta : (md as unknown as { getText(): string }).getText() + delta);
				first = false;
				this.tui.requestRender();
			});
			convId = t.convId;
			filtered = t.filtered;
		});
		this.firstTurn = false;
		this.stopSpinner();
		if (r.err) {
			this.addNote(`error: ${r.err.message}`);
			return;
		}
		if (transcriptsOn) appendTranscript(this.opts.cfgPath, this.conversation, "assistant", replyBuf);
		if (filtered && replyBuf.length > 0) {
			this.lastPartial = replyBuf;
			this.addNote("hint: /resume continues from the partial reply (kept as context)");
		} else if (!filtered) {
			this.lastPartial = "";
		}
		this.chat.addChild(new Text("")); // trailing blank: commit is a visual no-op
		this.conversation = convId;
		persistConversation(this.opts.cfgPath, this.opts.persist, convId);
		this.turns++;
		this.refreshChrome();
	}

	private async command(line: string): Promise<void> {
		const arg = (prefix: string): string => line.slice(prefix.length).trim();
		switch (true) {
			case line === "/exit" || line === "/quit":
				await this.close();
				process.exitCode = 0;
				process.kill(process.pid, "SIGTERM");
				return;
			case line === "/new":
				this.conversation = "";
				this.addNote("new conversation");
				break;
			case line === "/help":
				this.addNote("commands: /exit /quit /new /model /thinking /search /clear /resume /session /sessions /help");
				break;
			case line === "/model" || line.startsWith("/model "): {
				const m = arg("/model");
				if (m === "") {
					this.addNote(`model: ${this.model} (fixed per thread; /model <default|expert> starts a new conversation)`);
					break;
				}
				if (m !== "default" && m !== "expert") {
					this.addNote(`unknown model "${m}" (want default or expert)`);
					break;
				}
				this.model = m;
				this.conversation = "";
				this.addNote("new conversation");
				break;
			}
			case line === "/thinking" || line.startsWith("/thinking "):
				this.thinking = toggleState(line, "/thinking", this.thinking);
				break;
			case line === "/search" || line.startsWith("/search "):
				this.search = toggleState(line, "/search", this.search);
				break;
			case line === "/resume" || line.startsWith("/resume "): {
				if (this.lastPartial === "") {
					this.addNote("nothing to resume: no filtered partial (or the last reply was accepted)");
					break;
				}
				const instruction = arg("/resume");
				await this.submit(resumePrompt(this.lastPartial, instruction));
				return;
			}
			case line === "/sessions": {
				const rows = localSessionRows(this.opts.cfgPath);
				if (rows.length === 0) this.addNote("no local sessions (nothing saved yet)");
				else {
					this.addNote("local sessions (most recent first; the default is resumed on launch):");
					for (const r of rows) this.addNote(sessionRowText(r));
				}
				break;
			}
			case line === "/session" || line.startsWith("/session "): {
				const a = arg("/session");
				if (a === "") {
					const saved = loadSavedSession(this.opts.cfgPath);
					this.addNote(saved !== "" ? "conversation: " + saved : "no persisted session");
					break;
				}
				const { sessionId: bare } = splitConversation(a);
				if (bare === "") {
					this.addNote("give a session id (see /sessions)");
					break;
				}
				const err = saveSession(this.opts.cfgPath, bare);
				if (err) this.addNote(`error: ${err.message}`);
				else {
					const entries = loadTranscript(this.opts.cfgPath, bare);
					this.addNote(
						entries && entries.length > 0
							? `switched to session ${bare} (${entries.length} saved messages; resumes from its root)`
							: `switched to session ${bare} (no local transcript yet)`,
					);
					this.conversation = bare;
					this.lastPartial = "";
				}
				break;
			}
			case line === "/clear":
				this.chat.children.length = 0;
				this.addNote("pane cleared");
				break;
			default:
				this.addNote("unknown command (/help for commands)");
		}
		this.refreshChrome();
	}

	/** loadHistory renders a resumed thread's past messages into the pane. */
	loadHistory(messages: Array<{ role: string; text: string }>): void {
		for (const m of messages) {
			if (m.role === "USER") {
				this.chat.addChild(new Text(chalk.hex("#6b50ff").bold(`> ${m.text.split("\n")[0]}`)));
			} else {
				const md = this.addAssistant();
				md.setText(m.text);
			}
		}
		this.chat.addChild(new Text(""));
		this.refreshChrome();
	}

	async run(): Promise<void> {
		this.refreshChrome();
		this.tui.start();
		// Resume history when the default conversation exists.
		if (this.firstTurn) {
			try {
				const { sessionId } = splitConversation(this.conversation);
				const msgs = await this.client.chatHistory(sessionId);
				this.loadHistory(
					msgs
						.filter((m) => m.content !== "" || m.fragments.length > 0)
						.map((m) => ({
							role: m.role,
							text:
								m.content !== ""
									? m.content
									: m.fragments.filter((f) => !/think/i.test(f.type)).map((f) => f.content).join(""),
						}))
						.filter((m) => m.text !== ""),
				);
			} catch {
				// history is best-effort UI sugar
			}
		}
		// Ctrl+C twice quits; once while busy is left to the editor for now.
		const stop = async (): Promise<void> => {
			this.stopSpinner();
			if (this.owned.length > 0) {
				try {
					await this.client.deleteSessions(this.owned);
				} catch (err) {
					stderrNote(`warning: failed to delete session(s): ${err instanceof Error ? err.message : err}\n`);
				}
			}
			if (this.turns > 0) stderrNote(`conversation: ${this.conversation}\n`);
			this.tui.stop();
		};
		process.on("SIGINT", () => {
			void stop().then(() => process.exit(130));
		});
		// Run until signalled (SIGINT above; the editor owns the keyboard).
		await new Promise<never>(() => {});
	}

	async close(): Promise<void> {
		this.stopSpinner();
		if (this.owned.length > 0) {
			try {
				await this.client.deleteSessions(this.owned);
			} catch {
				// best-effort
			}
		}
		this.tui.stop();
	}
}
