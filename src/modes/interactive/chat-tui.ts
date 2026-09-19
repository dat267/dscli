/**
 * The interactive chat TUI — pi's exact recipe (@earendil-works/pi-coding-agent,
 * modes/interactive): a ScrollView transcript that grows to fill the screen,
 * a fixed dock below it (working indicator in the DynamicBorder, editor,
 * two-sided footer), user messages in background boxes, markdown replies.
 *
 * Built from pi-tui primitives with the layout of pi's createChatViewport:
 *
 *   VStack root
 *   ├ ScrollView(document, follow: end, primary)   ← transcript
 *   └ VStack dock
 *      ├ DynamicBorder (embeds the ⠋ Working loader while streaming)
 *      ├ Editor (min 3 rows, slash-command autocomplete)
 *      └ Footer (left: run facts · right: model, right-aligned)
 */
import {
	CombinedAutocompleteProvider,
	Container,
	Editor,
	ProcessTerminal,
	ScrollView,
	TuiMainScreen,
	VStack,
	type Component,
} from "@earendil-works/pi-tui";
import {
	AssistantMessageComponent,
	FooterComponent,
	NoteComponent,
	UserMessageComponent,
	WorkingBorder,
} from "./components.js";
import { TUI_COMMANDS } from "./parts.js";
import { theme } from "./theme.js";
import type { DeepSeekClient } from "../../core/deepseek/client.js";
import {
	effectiveModel,
	loadSavedSession,
	persistConversation,
	recoverStaleSession,
	resolveDefaultSession,
	saveSession,
	splitConversation,
} from "../../core/session.js";
import { appendTranscript, loadTranscript, transcriptsEnabled } from "../../core/transcript.js";
import { localSessionRows, sessionRowText } from "../../cli/commands/session.js";
import { oneTurn, resumePrompt, toggleState } from "../../cli/commands/chat.js";
import { stderrNote } from "../../ui/notes.js";
import { homedir } from "node:os";
import { resolve } from "node:path";

const SPINNER_INTERVAL_MS = 80; // pi's Loader default

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

/**
 * The editor's autocomplete provider: pi's CombinedAutocompleteProvider over
 * the dscli slash commands (fuzzy matching, applyCompletion, tab-complete).
 */
class SlashProvider extends CombinedAutocompleteProvider {
	constructor() {
		super(
			TUI_COMMANDS.map((name) => ({
				name,
				description: SlashProvider.description(name),
			})),
			process.cwd(),
			null,
		);
	}

	private static description(name: string): string {
		switch (name) {
			case "/exit":
			case "/quit":
				return "leave the session";
			case "/new":
				return "start a fresh conversation";
			case "/help":
				return "this help";
			case "/model":
				return "switch model (starts a fresh conversation)";
			case "/thinking":
				return "toggle DeepThink reasoning";
			case "/search":
				return "toggle web search";
			case "/resume":
				return "continue a filtered reply";
			case "/session":
				return "show or switch the conversation";
			case "/sessions":
				return "list sessions with saved texts";
			case "/clear":
				return "clear the pane";
			default:
				return "";
		}
	}
}

export class ChatTui {
	private readonly tui: TuiMainScreen;
	private readonly document: Container;
	private readonly scroller: ScrollView;
	private readonly border: WorkingBorder;
	private readonly footer: FooterComponent;
	private readonly editor: Editor;
	private busy = false;
	private spinnerTimer: NodeJS.Timeout | undefined;
	private turns = 0;
	private model: string;
	private thinking: boolean;
	private search: boolean;
	private conversation: string;
	private lastPartial = "";
	private firstTurn = false;
	private readonly owned: string[] = [];
	private readonly startedAt = Date.now();
	private closed = false;

	constructor(
		private readonly client: DeepSeekClient,
		private readonly opts: ChatTuiOptions,
	) {
		this.model = effectiveModel(opts.model);
		this.thinking = opts.thinking;
		this.search = opts.search;
		this.conversation = opts.conversation;
		this.tui = new TuiMainScreen(new ProcessTerminal());
		this.document = new Container();
		this.scroller = new ScrollView(this.document, { follow: "end", primary: true });

		this.border = new WorkingBorder();
		this.footer = new FooterComponent({
			model: this.model,
			thinking: this.thinking,
			search: this.search,
			mode: this.opts.conversation !== "" ? "continuing" : this.opts.persist ? "persisted" : "ephemeral",
			turns: 0,
			conversation: this.conversation,
			cwd: formatCwd(resolve(this.opts.workdir), homedir()),
		});
		this.editor = new Editor(this.tui, {
			borderColor: (s) => theme.fg("border", s),
			selectList: {
				selectedPrefix: (t) => theme.fg("accent", t),
				selectedText: (t) => theme.bold(t),
				description: (t) => theme.fg("muted", t),
				scrollInfo: (t) => theme.fg("dim", t),
				noMatch: (t) => theme.fg("dim", t),
			},
		}, { paddingX: 1 });
		this.editor.setAutocompleteProvider(new SlashProvider());

		// pi's createChatViewport layout: transcript grows, dock is fixed.
		const dock = new VStack([
			{ component: this.border, shrink: 1, minSize: 1 },
			{ component: this.editor as unknown as Component, shrink: 1, minSize: 3 },
			{ component: this.footer as unknown as Component, shrink: 1, minSize: 1 },
		]);
		this.tui.addChild(new VStack([
			{ component: this.scroller, basis: 0, grow: 1, shrink: 1, minSize: 1 },
			{ component: dock, basis: "auto", grow: 0, shrink: 1, minSize: 1 },
		]));

		this.editor.onSubmit = (text: string) => void this.submit(text);
		this.editor.addToHistory("");

		// pi's ctrl+c contract: clear the input first, exit on a second press.
		let lastCtrlC = 0;
		this.tui.addInputListener((data: string): { consume?: boolean } | undefined => {
			if (data === "\x03") {
				const now = Date.now();
				if (this.editor.getText().trim() !== "" || now - lastCtrlC < 2000) {
					if (this.editor.getText().trim() !== "") {
						this.editor.setText("");
					} else {
						void this.close();
						process.exit(130);
					}
				}
				lastCtrlC = now;
				return { consume: true };
			}
			if (data === "\x04") {
				void this.close();
				process.exit(0);
				return { consume: true };
			}
			return undefined;
		});

		if (this.conversation === "") {
			void this.initSession();
		}
	}

	private async initSession(): Promise<void> {
		try {
			const resolved = await resolveDefaultSession(this.client, this.opts.cfgPath, this.opts.persist);
			this.conversation = resolved.sessionId;
			this.firstTurn = resolved.trusted;
			if (resolved.cleanup && !this.opts.persist) {
				// The TUI owns the ephemeral session; delete it on exit.
				this.owned.push(this.conversation);
			}
		} catch (err) {
			// The session is created lazily on the first submit instead; the
			// pane must not crash on startup (stale token, offline, ...).
			this.addNote(`note: no session yet (${err instanceof Error ? err.message : err}) — it is created on your first message`);
		}
		this.refreshFooter();
	}

	// --- transcript helpers -------------------------------------------------

	private addComponent(c: Component): void {
		this.document.addChild(c);
		this.tui.requestRender();
	}

	private addNote(text: string): void {
		this.addComponent(new NoteComponent(text));
	}

	private addBlank(): void {
		this.addComponent(new NoteComponent(""));
	}

	private refreshFooter(): void {
		this.footer.setParams({
			model: this.model,
			thinking: this.thinking,
			search: this.search,
			mode: this.opts.conversation !== "" ? "continuing" : this.opts.persist ? "persisted" : "ephemeral",
			turns: this.turns,
			conversation: this.conversation,
			cwd: formatCwd(resolve(this.opts.workdir), homedir()),
		});
		this.tui.requestRender();
	}

	private startSpinner(): void {
		this.busy = true;
		this.border.setBusy(true);
		this.spinnerTimer = setInterval(() => {
			this.border.tick();
			this.tui.requestRender();
		}, SPINNER_INTERVAL_MS);
		this.tui.requestRender();
	}

	private stopSpinner(): void {
		this.busy = false;
		this.border.setBusy(false);
		if (this.spinnerTimer) clearInterval(this.spinnerTimer);
		this.spinnerTimer = undefined;
		this.tui.requestRender();
	}

	// --- submission ---------------------------------------------------------

	private async submit(raw: string): Promise<void> {
		const text = raw.trim();
		this.editor.setText("");
		if (text === "" || this.busy) return;
		if (text.startsWith("/")) {
			await this.command(text);
			return;
		}
		this.editor.addToHistory(text);

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

		this.addComponent(new UserMessageComponent(text));
		const md = new AssistantMessageComponent("");
		this.addComponent(md);
		this.startSpinner();

		const transcriptsOn = transcriptsEnabled(this.opts.cfgPath, this.opts.persist, this.opts.noTranscript);
		if (transcriptsOn) appendTranscript(this.opts.cfgPath, this.conversation, "user", text);

		let replyBuf = "";
		let convId = this.conversation;
		let filtered = false;
		const r = await recoverStaleSession(this.client, this.opts.cfgPath, this.conversation, this.firstTurn, async (sid) => {
			const t = await oneTurn(this.client, sid, text, this.model, this.thinking, this.search, (delta) => {
				replyBuf += delta;
				md.setText(replyBuf);
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
		this.addBlank(); // spacing between turns
		this.conversation = convId;
		persistConversation(this.opts.cfgPath, this.opts.persist, convId);
		this.turns++;
		this.refreshFooter();
	}

	// --- slash commands ------------------------------------------------------

	private async command(line: string): Promise<void> {
		const arg = (prefix: string): string => line.slice(prefix.length).trim();
		switch (true) {
			case line === "/exit" || line === "/quit":
				await this.close();
				process.exit(0);
				return;
			case line === "/new":
				this.conversation = "";
				this.addNote("new conversation");
				break;
			case line === "/help":
				this.addNote("commands: /exit /quit /new /model /thinking /search /resume /session /sessions /clear /help");
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
				this.document.clear();
				this.addNote("pane cleared");
				break;
			default:
				this.addNote("unknown command (/help for commands)");
		}
		this.refreshFooter();
	}

	/** loadHistory renders a resumed thread's past messages into the pane. */
	loadHistory(messages: Array<{ role: string; text: string }>): void {
		for (const m of messages) {
			if (m.role === "USER") {
				this.addComponent(new UserMessageComponent(m.text));
			} else {
				this.addComponent(new AssistantMessageComponent(m.text));
			}
		}
		this.addBlank();
		this.refreshFooter();
	}

	async run(): Promise<void> {
		this.addNote("DeepSeek · /help for commands · ctrl+c clears (twice exits)");
		this.tui.setFocus(this.editor);
		this.tui.start();
		// Resume history when the default conversation exists.
		if (this.firstTurn) {
			try {
				const { sessionId } = splitConversation(this.conversation);
				const msgs = await this.client.chatHistory(sessionId);
				this.loadHistory(
					msgs
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
		// Run until the input listener exits the process.
		await new Promise<never>(() => {});
	}

	async close(): Promise<void> {
		if (this.closed) return;
		this.closed = true;
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
	}
}

/** formatCwd: replace the home directory with ~. */
function formatCwd(cwd: string, home: string | undefined): string {
	if (!home) return cwd;
	if (cwd === home) return "~";
	if (cwd.startsWith(home + "/")) return "~" + cwd.slice(home.length);
	return cwd;
}
