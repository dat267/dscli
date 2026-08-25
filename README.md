# dscli

DeepSeek chat from your terminal. **No API key, no billing** — it uses your
free signed-in [chat.deepseek.com](https://chat.deepseek.com) account directly,
speaking the site's internal web API: it creates a chat session, solves
DeepSeek's DeepSeekHashV1 proof-of-work challenge by running DeepSeek's own
WebAssembly module inside the [wazero](https://wazero.io) sandbox (vendored
`sha3_wasm_bg.wasm`), and streams the reply.

Unofficial project for personal use — not affiliated with DeepSeek.

```
Usage: dscli <command> [flags]

DeepSeek chat from your terminal

Flags:
  -h, --help                  Show context-sensitive help.
      --config-file=STRING    Config file path

Commands:
  chat                  Chat with DeepSeek (omit the prompt for an interactive
                        session)
  ask                   Ask the model once and print the answer (input from args
                        or stdin)
  translate             Translate a file (txt, md, lrc, srt, vtt, ass, ttml,
                        epub) via the model
  session transcript    Print or delete the saved session texts (transcript) for
                        a session
  session delete        Delete the persisted default session server-side and
                        forget it
  session forget        Forget the persisted default session (the thread is kept
                        server-side)
  login                 Show how to capture your DeepSeek login (token + cookie)
  version               Show version
  config init           Generate a default configuration file
  config path           Show configuration file path
  config show           Print current configuration values
  config set            Set a config value
  config unset          Unset a config value

Run "dscli <command> --help" for more information on a command.
```

## Quick start

```bash
# Install — straight from GitHub: no tags, no Go module proxy
GOPROXY=direct GOSUMDB=off go install github.com/dat267/dscli@main

# The binary lands in $(go env GOBIN) (~/go/bin by default); make sure that
# is on your PATH, then:
dscli login

# Save it (the values live only in your config file, created with 0600 perms)
dscli config set token "<token>"
dscli config set cookie "<cookie>"

# Ask a question (the reply streams like a chat)
dscli chat "Explain the halting problem in one paragraph"

# Or a pure one-shot: input in, answer out, no conversation bookkeeping
dscli ask "What is 2+2?"
echo "summarize this" | dscli ask
dscli ask --thinking --search "latest Mars rover news"

# Continue that conversation later
dscli chat -c '<conversation id>' "And what about Rice's theorem?"
```

`token` and `cookie` can also come from `DS_TOKEN`/`DS_COOKIE` env vars or the
`--token`/`--cookie` flags. The cookie field stores either the bare
`ds_session_id` value or a full `k=v; k2=v2` cookie header, which is passed
through untouched.

> **Why the console snippet only returns the token:** the web app keeps the
> bearer token in `localStorage.userToken` (readable from JavaScript), but
> `ds_session_id` is an **HttpOnly** cookie, so `document.cookie` cannot see
> it. Path of least resistance: DevTools → Network → click any
> `chat.deepseek.com` request → Headers → copy the whole `cookie:` line
> (or just the `ds_session_id=...` value) into `config set cookie`. Pasting
> the full cookie line also forwards the AWS WAF token, which the site
> sometimes wants alongside the session cookie.

## Interactive session

```bash
dscli chat                       # open a REPL
dscli chat -m expert             # REPL on the stronger model
dscli chat --thinking --search   # DeepThink + web search
```

When stdin and stdout are terminals, `dscli chat` runs a terminal-agent TUI
(open-code/Claude Code style): the conversation scrolls in a pane above a
bottom-pinned, multi-line input. Slash commands get a suggestion menu as you
type; replies, tool notes and file-write previews render inside the pane
instead of stdout:

```
DeepSeek · model default · thinking off · search off · persisted

What is 2+2?
4

/model expert              # switch model (starts a fresh conversation)
/thinking on               # or bare /thinking to flip it
/search off
/exit
conversation: 0123456789:42
```

Keys: **Enter** submits, **Ctrl+J / Alt+Enter** inserts a newline, **Up/Down**
move the cursor one *visual* row at a time through wrapped input (at the first
and last rows they fall back to history recall), **Ctrl+←/→** (or **Alt+←/→**)
move by word, **Alt+Backspace** deletes the word before the cursor, **Ctrl+A /**
**Ctrl+E** jump to the start/end of the line, and **Tab** completes a command
(cycling through the candidates when several match; Enter also completes).
**Esc** first dismisses the completion menu without losing what you typed, then
clears the input, and **Ctrl+C** interrupts a reply while it streams — the chat
stays open — or quits when idle. The layout is a scrollable chat pane on top, a
2-line input with a `::: ` prompt at the bottom, and the status line below it.
Text wraps inside the box by *display* width (so line breaks land correctly for
CJK/wide characters too), and a `…` in the rightmost cell marks content
scrolled out of the box. PgUp/PgDn scroll a page, the mouse wheel scrolls a few
lines, **Ctrl+L** clears the pane, and Home / End jump to the top/bottom. When
a persisted conversation is resumed the past messages are loaded from the
server into the pane first. User messages render in a foreground colour to
stand apart from the assistant's replies. `/new` starts a fresh conversation,
`/clear` forgets the persisted default session and starts a fresh one
(`/clear --delete` also removes the old thread server-side).
**`/file <path>`** loads a file's (or directory's) contents (relative to
`--workdir`, defaulting to `.`) into a buffer that is prepended to the next
submitted message inside a `<file>`/`<dir>` block — repeat it to stack several
files, and a system note in the chat shows each load and the final
attachments.

**Censorship.** Prompts are sent exactly as written — no hidden instructions.
If DeepSeek's content filter rejects a reply, the CLI prints a short dim note
("reply was filtered by DeepSeek") instead of silently returning an empty
answer. When the filter cuts a reply off mid-stream, the partial text that
already streamed is kept, and **`/resume [hint]`** (TUI and line REPL) sends it
back as context with a "continue from where it stopped" instruction — an
honest recovery for wrongly flagged replies: the text is sent as an ordinary
prompt, and the filter still applies to whatever the model generates next.
Translation keeps its own separate style (see below).

When stdin or stdout is **not** a terminal (pipes, scripts, `--json-out`), the
line-based loop is used instead: replies stream to stdout, no prompts or
colours are drawn, and a line ending in a single `\` continues the message on
the next line (`...> `); a lone `\` line inserts a blank line and keeps going,
and a trailing `\\` sends the line literally.

**Persistence by default.** Launching `dscli chat` without `-c` resumes the
*persisted default conversation* — the same thread every command uses, saved
under `session` in the config file as a `session:message` position. On first
use a session is created and saved; every later run resumes from the last
message and re-saves its position, so the conversation carries across
invocations. `/new` and `/model` start a fresh session that replaces the saved
default (the old thread stays server-side, just no longer the default). The
conversation id shown is the live thread's position, and the config's
`session` value is the default to resume next time. Manage it with:

```bash
dscli session                 # show the persisted conversation
dscli session forget          # forget it (thread stays server-side)
dscli session delete          # delete it server-side and forget it
dscli config unset session    # equivalent to `session forget`
```

Run with `--no-persist` for the old stateless behaviour: a *fresh* session is
created per run, kept for its turns, and deleted on close (`/exit`, `/quit`,
Ctrl-D, or Ctrl-C). If the saved default session no longer exists server-side
(e.g. deleted in the web UI), the CLI creates a fresh one, saves it, and
retries once automatically.

**Session texts.** Every turn's typed prompt and the streamed reply are
appended to a JSONL transcript — one line per message, `{"time": "...",
"role": "user|assistant", "text": "..."}` — in the `transcripts/` folder
inside the app's persistent data directory (next to the config file,
`~/.config/dscli/transcripts/` by default), named `<session-id>.jsonl`. The
whole thread's texts accumulate in one file as the conversation advances,
whether from the TUI, the line REPL, or a one-shot `chat`/`ask` (in the TUI,
`/file` attachments are the only things not copied — the typed prompt is).
Print a transcript with:

```bash
dscli session transcript            # the persisted default session
dscli session transcript <session>  # any session id
dscli session transcript --delete   # delete the default session's transcript
```

Ephemeral runs (`--no-persist`) leave no transcript, and `--no-transcript`
(or `config set no-transcript true`) disables saving when you do not want the
texts kept locally. `--delete` removes the JSONL file (and the `transcripts/`
folder when it becomes empty) without touching the server-side thread —
`session delete`, on the other hand, removes the thread but leaves any saved
texts alone.

## File naming & grouping

Translations use the i18n name-coding convention — **`<base>.translated.<lang>.<ext>`**:

```
chapter-012.md                  ← original
chapter-012.translated.en.md    ← translation
chapter-012.translated.zh.md    ← a second target, same dir
```

- The language code comes from the target label (ISO 639-1 for common
  languages: `en`, `ja`, `zh`, `fr`, `es`, …; unknown labels fall back to a
  lowercase token). Originals are never renamed.
- An existing `.translated[.<lang>]` suffix is stripped before re-naming, so
  translating a translation never stacks suffixes.
- **Any tool can group a pair** by regex: `^(.*)\.translated(?:\.([a-z0-9]{1,8}))?\.([^.]+)$`
  → base + optional language.

> Tip: paths containing spaces must be quoted in the shell, e.g.
> `dscli translate "my documents/notes.md"`. The output path mirrors the
> input's (relative or absolute) form.

## Custom translation styles

Every language pair can carry its own translation instructions. Inside each
chunk prompt, the style is appended after the format rules.

**Resolution order** for a `from → to` pair:

1. `--instructions <file>` (explicit, any pair, both `dscli translate` and
   `dscli chat`).
2. A sidecar file `translate/<from>-<to>.md` (language labels lowercased,
   e.g. `translate/ja-en.md`), searched in `./translate/` then
   `~/.config/dscli/translate/`; `translate/default.md` is the fallback.
3. A built-in general style — the Japanese→English principles phrased
   universally (subject inference, gender neutrality, active voice, register,
   false friends, connectors, structure preservation) — applies to all pairs
   with no custom file.

Drop your own `translate/<from>-<to>.md` (e.g. `translate/ja-en.md`, or
`translate/zh-en.md`) into `./translate/` (or point `--instructions` at any
file) and it applies to that pair everywhere:

```bash
dscli translate book.lrc --from Japanese --to English   # picks up translate/ja-en.md if present
dscli translate --instructions my-style.md chapter.md  # explicit file for any pair
```

## Models, DeepThink & web search

Model, DeepThink (thinking) and web search are per-thread or per-request
toggles:

```bash
dscli chat -m expert "explain Gödel's incompleteness"   # strong model
dscli chat -t "reason step by step"                     # DeepThink
dscli chat -s "latest Mars rover news"                  # web search
dscli chat -t -s -m expert "both, on the strong model"
```

Inside the REPL the same switches are slash commands (see `/help`): a bare
`/thinking` or `/search` flips the current state, or give an explicit value
(`/thinking on`). The status line redraws after every change.

```
/thinking on
/search off
/model expert          # switches model; starts a fresh conversation
```

A thread's model is fixed when it is created: `--model`/`/model` always start
a new conversation, and `--model` cannot be combined with `--conversation`.
DeepThink and search can be toggled freely at any point of a thread — and
**both default to off** (the status line in the REPL shows the current state:
`thinking off · search off`).

**Search citations:** with `-s` the reply carries `[citation:N]` markers; the
CLI extracts the search sources from the stream (TOOL_SEARCH fragments /
`.../results` patches) and prints them as footnotes on stderr — or, with
`--json-out`, as a final `{"sources":[...]}` line.

## Scripting

```bash
dscli chat --json-out "Summarize this repo" | jq -s 'map(.delta) | join("")'
```

`--json-out` emits NDJSON: one `{"delta":"..."}` line per chunk, then a final
`{"done":true,"conversation_id":"..."}` line. It is for one-shot scripting —
the interactive REPL always prints plain text. Text output is written to
stdout; prompts, warnings and the conversation id go to stderr, so piping
plain `dscli chat` also gives you clean text.

`conversation_id` encodes `<chat_session_id>:<parent_message_id>` and lets you
resume any thread. A thread's model is fixed when it is created, so `--model`
cannot be combined with `--conversation`.

## How it works

1. `POST /api/v0/chat_session/create` — starts a session (new threads only).
2. `POST /api/v0/chat/create_pow_challenge` with `{"target_path":"/api/v0/chat/completion"}`.
3. Solves the DeepSeekHashV1 challenge by driving the vendored
   `sha3_wasm_bg.wasm` (`wasm_solve(retptr, challenge, clen, prefix, plen, difficulty)`)
   with the shadow-stack convention the website's JS wrapper uses, and
   base64-encodes `{algorithm, challenge, salt, answer, signature, target_path}`
   into the `x-ds-pow-response` header.
4. `POST /api/v0/chat/completion` with the site headers, streaming the SSE
   json-patch frames: the snapshot frame (`fragments[].type == "response"`),
   append/SET/BATCH patches on `response/fragments/-1/content`, and bare
   pathless `{"v":...}` chunks (which can carry the reply's very first
   characters) — all reconstructed in arrival order without trimming, so no
   leading text is lost. Fragments are tracked by type: content belonging to
   THINK/SEARCH fragments is never rendered as answer text, so `--thinking`
   mode cannot leak reasoning into the reply (or drop the answer's opening
   token after it). `message_id` (from `v.message_id` / `v.message.id`
   or a patch path) becomes the next turn's `parent_message_id`.

Credentials are sent as `authorization: Bearer <token>` plus the `ds_session_id`
cookie, with the site's `x-app-version` / `x-client-version` /
`x-client-platform` / `x-client-bundle-id` headers and origin/referer.

The PoW challenge is short-lived, so a failed completion (transport hiccup,
HTTP 401/403/429) automatically re-solves a fresh challenge once.

## Config

Config is a JSON file, managed exactly like the `min` CLI toolkit:

```bash
dscli config path        # e.g. ~/.config/dscli/dscli.json
dscli config init
dscli config show
dscli config set token <token>
dscli config unset token
dscli --config-file /path/to/dscli.json config set token <token>
```

The `session` key holds the persisted default conversation position
(`<session_id>:<message_id>`, set automatically on every turn; `dscli session
forget` clears it and the next run starts a fresh thread).

A `dscli.json` in the current directory takes precedence over the per-user
config file (a warning is printed when that happens implicitly), and
`DSCLI_CONFIG_FILE` overrides both.

## Development

```bash
# Build with version
go build -ldflags="-X main.version=$(git describe --tags --always)" -o dscli .

# The PoW solver tests run DeepSeek's actual wasm (wazero) against a known
# golden challenge, so no network or login is needed:
go test ./...
```

## Notes

- **PoW internals.** DeepSeekHashV1 turns out to be a Keccak variant: SHA3-style
  `0x06` domain padding, Keccak-f[1600] with rounds 1..23, over
  `salt_expireAt_<nonce>`; the challenge is the expected digest, `difficulty`
  bounds the nonce search. This repo does not reimplement it (the vendored wasm
  is the authority) — the variant knowledge is only used to generate offline
  test vectors.
- Structures follow [github.com/dat267/min](https://github.com/dat267/min)
  (kong CLI, JSON config with nested keys, config-flag merging, version
  injection via ldflags).
## Translate

`dscli translate` runs a format-aware, chunked translation of a file and
writes the result:

```bash
dscli translate notes.md                       # → notes.translated.md
dscli translate song.lrc --to "Chinese"        # LRC timestamps byte-for-byte
dscli translate movie.srt -o ja.srt            # SRT timing lines preserved
dscli translate movie.vtt -o zh.vtt            # WebVTT cues preserved
dscli translate sub.ass -o it.ass              # ASS dialogue fields preserved
dscli translate sub.ttml -o de.ttml            # TTML XML structure preserved
dscli translate book.epub -o book.txt          # EPUB text extraction → translation
dscli translate -f lyrics.lrc -o lyrics.lrc    # overwrite the source in place
```

- **Adaptive chunking — no cut-off, nothing wasted.** The binding limit on a
  translation is the model's per-response *output* length (the site cuts
  replies around 36 KiB and flags them `INCOMPLETE`). Instead of a fixed
  chunk size, the CLI probes a small first chunk, learns the real
  output/input byte ratio, and sizes the remaining chunks to fill the output
  budget — every completed chunk is kept, and if a reply is ever still cut
  off the chunk size shrinks and that chunk is retried, so an incomplete
  result is never silently written. `--chunk-bytes N` is an upper bound on
  chunk size, not a fixed size.
- `--thinking`/`-t` enables DeepThink reasoning per chunk. The reasoning
  model allows far longer replies than Instant, so chunks are sized bigger
  (roughly 3×) and a file needs fewer generations — the real output cap is
  still learned from the first truncation either way.
- **Structural awareness for every subtitle/lyric format:** after every
  chunk the CLI verifies that the structure survived byte-for-byte — LRC
  `[mm:ss.xx]` timecodes, SRT `HH:MM:SS,mmm --> ...` timing lines, WebVTT
  timing/`WEBVTT`/NOTE lines, ASS/SSA script/style headers plus every
  `Dialogue:` line's prefix fields (layer, start, end, style, name,
  margins, effect — only the text field may change), and TTML's complete
  XML tag/attribute sequence. A broken chunk is retried once with a strict
  reminder, then fails loudly instead of producing a corrupt file.
- Markdown keeps code blocks/URLs/list markers; `.epub` is extracted to text
  first and defaults to a `.translated.txt` output.
- `file_meta` reports duration for lrc/srt/vtt/ass/ssa/ttml files.
- The output path defaults to `<input>.translated.<ext>` and is never
  overwritten without `-f`. Each translation runs in a fresh session deleted
  when the run ends (`--no-persist` is the default; pass `--no-persist=false`
  to resume the persisted default session instead).
- If DeepSeek's content filter cuts a chunk's reply off mid-stream, the
  partial translation produced before the filter is kept instead of the run
  retrying (a smaller chunk cannot un-censor content); a reply with no
  content at all fails loudly.
