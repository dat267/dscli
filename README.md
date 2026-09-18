# dscli

DeepSeek chat from your terminal. **No API key, no billing** — it uses your
free signed-in [chat.deepseek.com](https://chat.deepseek.com) account directly,
speaking the site's internal web API: it creates a chat session, solves
DeepSeek's DeepSeekHashV1 proof-of-work challenge by running DeepSeek's own
WebAssembly module (`sha3_wasm_bg.wasm`, vendored byte-for-byte from the site's
CDN) on Node's native WebAssembly runtime, and streams the reply.

Unofficial project for personal use — not affiliated with DeepSeek.
Implemented in TypeScript on Node.js (≥ 22), structured after
[pi](https://github.com/earendil-works/pi-mono)'s CLI layout and built on
pi's own terminal framework (`@earendil-works/pi-tui`).

```
Usage: dscli [--config-file FILE] <command> [flags]

DeepSeek chat from your terminal

Commands:
  chat             Chat with DeepSeek (omit the prompt for an interactive session)
  ask              Ask the model once and print the answer (input from args or stdin)
  translate        Translate a file (txt, md, lrc, srt, vtt, ass, ttml, epub) via the model
  improve-writing  Improve the writing of a file in place (txt, md, lrc, srt, vtt, ass, ttml) via the model
  summarize        Summarize a file (txt, md, lrc, srt, vtt, ass, ttml, epub) via the model
  session          Inspect, forget or delete the persisted default session
  login            Show how to capture your DeepSeek login (token + cookie)
  version          Show version
  config           Manage application configuration

  session <command>
    list        List sessions with saved texts
    select      Select a session to resume as the default
    transcript  Print or delete the saved session texts (transcript) for a session
    delete      Delete the persisted default session server-side and forget it
    forget      Forget the persisted default session (the thread is kept server-side)

  config <command>
    init   Generate a default configuration file
    path   Show configuration file path
    set    Set a config value
    unset  Unset a config value

Use "dscli <command> --help" for command flags.
```

## Quick start

```bash
# Install — straight from GitHub: no tags, no npm registry publishing
npm install -g github:dat267/dscli

dscli login

# Save it (the values live only in your config file, created with 0600 perms)
dscli config set token "<token>"
dscli config set cookie "<cookie>"

# Chat interactively (TUI in a terminal)
dscli chat

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
dscli chat                       # open the TUI
dscli chat -m expert             # stronger model
dscli chat --thinking --search   # DeepThink + web search
```

When stdin and stdout are terminals, `dscli chat` runs the interactive TUI —
built on [pi](https://github.com/earendil-works/pi-mono)'s terminal framework
(`@earendil-works/pi-tui`): the conversation scrolls in a markdown-rendered
pane above a bottom-pinned multi-line editor with slash-command
autocomplete, a status line, and the working indicator embedded in the
separator rule — a pi-style `── ⠋ Working ───` while a reply streams, so the
pane layout never changes for streaming.

Slash commands: `/exit` `/quit` `/new` `/model <default|expert>` `/thinking
[on|off]` `/search [on|off]` `/resume [instruction]` `/session [id]`
`/sessions` `/clear` `/help`. Type `/` to get a completion menu.

**Censorship.** Prompts are sent exactly as written — no hidden instructions.
If DeepSeek's content filter rejects a reply, the CLI prints a short note
("reply was filtered by DeepSeek") instead of silently returning an empty
answer. When the filter cuts a reply off mid-stream, the partial text that
already streamed is kept, and **`/resume [hint]`** sends it back as context
with a "continue from where it stopped" instruction — an honest recovery for
wrongly flagged replies: the text is sent as an ordinary prompt, and the
filter still applies to whatever the model generates next.

When stdin or stdout is **not** a terminal (pipes, scripts, `--json-out`), the
line-based REPL is used instead: replies stream to stdout, no prompts or
colours are drawn, and a line ending in a single `\` continues the message on
the next line; a lone `\` line inserts a blank line and keeps going, and a
trailing `\\` sends the line literally.

**Nothing is persisted by default.** Each run creates a *fresh* session, keeps
it for its turns, and deletes it on close (`/exit`, `/quit`, Ctrl-D, or
Ctrl-C), leaving nothing in the config or the transcripts folder. Run with
`--persist` to opt in: the session and its conversation position are saved
under `session` in the config file, the next run resumes that exact thread,
and texts are kept as local transcripts. If a persisted default session no
longer exists server-side (e.g. deleted in the web UI), the CLI creates a
fresh one, saves it, and retries once automatically. Manage the default with:

```bash
dscli session                 # show the persisted conversation
dscli session list            # list sessions with saved texts, most recent first (default marked)
dscli session select <id>     # make a session the default to resume
dscli session forget          # forget it (thread stays server-side)
dscli session delete          # delete it server-side and forget it
dscli config unset session    # equivalent to `session forget`
```

**Session texts.** Every turn's typed prompt and the streamed reply are
appended to a JSONL transcript — one line per message, `{"time": "...",
"role": "user|assistant", "text": "..."}` — in the `transcripts/` folder next
to the config file (`~/.config/dscli/transcripts/` by default), named
`<session-id>.jsonl`. Print it with:

```bash
dscli session transcript            # the persisted default session
dscli session transcript <session>  # any session id
dscli session transcript --delete   # delete the default session's transcript
```

Runs without `--persist` leave no transcript, and `--no-transcript`
(or `config set no-transcript true`) disables saving even for persisted runs.

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

1. `--instructions <file>` (explicit, any pair).
2. A sidecar file `translate/<from>-<to>.md` (language labels lowercased,
   e.g. `translate/ja-en.md`), searched in `./translate/` then
   `~/.config/dscli/translate/`; `translate/default.md` is the fallback.
   `improve-writing/default.md` and `summarize/default.md` work the same way
   for those commands.
3. A built-in general style — subject inference, active voice, register,
   false friends, structure preservation — applies to all pairs with no
   custom file.

A project glossary can be appended to every chunk prompt with
`--glossary <file>` (translate and improve-writing).

## Models, DeepThink & web search

```bash
dscli chat -m expert "explain Gödel's incompleteness"   # strong model
dscli chat -t "reason step by step"                     # DeepThink
dscli chat -s "latest Mars rover news"                  # web search
dscli chat -t -s -m expert "both, on the strong model"
```

Inside the TUI the same switches are slash commands: a bare `/thinking` or
`/search` flips the current state, or give an explicit value (`/thinking on`).
The status line redraws after every change and always shows the current mode:

```
DeepSeek · model default · thinking on · search off · persisted · turn 3 · 910c6ac2…
```

A thread's model is fixed when it is created: `--model`/`/model` always start
a new conversation, and `--model` cannot be combined with `--conversation`.
DeepThink and search can be toggled freely at any point of a thread — and
**both default to off**.

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
the interactive modes always print plain text. Text output is written to
stdout; prompts, warnings and the conversation id go to stderr.

`conversation_id` encodes `<chat_session_id>:<parent_message_id>` and lets you
resume any thread. A thread's model is fixed when it is created, so `--model`
cannot be combined with `--conversation`.

## Translate, improve-writing & summarize

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
- `--thinking`/`-t` enables DeepThink reasoning per chunk. The reasoning model
  allows longer replies, so chunks are sized bigger and fewer are needed.
- **Structural verification.** For subtitle/lyric formats every protected
  line (timestamps, cue indices, headers, TTML tags) is compared
  byte-for-byte after each chunk; a mismatch triggers one strict retry and
  the run fails loudly rather than writing a corrupted file.
- Multiple files: `-p`/`--parallel` translates files concurrently (each in
  its own session; terminology may drift between files — use a glossary).
  Sequential mode reuses one session per run and stops at the first error.
- `dscli improve-writing` rewrites the file **in place** (`-i` is required in
  spirit: there is no separate output) with improved prose, preserving
  structure; epub is rejected (extraction is one-way).
- `dscli summarize` prints a summary to stdout (or `-o file`); a multi-chunk
  document gets its per-chunk summaries combined in a final pass, and a file
  within the chunk cap is summarized in a single reply.

## How it works

1. `POST /api/v0/chat_session/create` — starts a session (new threads only).
2. `POST /api/v0/chat/create_pow_challenge` with `{"target_path":"/api/v0/chat/completion"}`.
3. Solves the DeepSeekHashV1 challenge by driving the vendored
   `sha3_wasm_bg.wasm` (`wasm_solve(retptr, challenge, clen, prefix, plen, difficulty)`)
   with the wasm-bindgen shadow-stack convention the website's JS wrapper
   uses, and base64-encodes `{algorithm, challenge, salt, answer, signature,
   target_path}` into the `x-ds-pow-response` header.
4. `POST /api/v0/chat/completion` with the site headers, streaming the SSE
   json-patch frames: the snapshot frame (`fragments[].type == "response"`),
   append/SET/BATCH patches on `response/fragments/-1/content`, and bare
   pathless `{"v":...}` chunks (which can carry the reply's very first
   characters) — all reconstructed in arrival order without trimming, so no
   leading text is lost. Fragments are tracked by type: content belonging to
   THINK/SEARCH fragments is never rendered as answer text, so `--thinking`
   mode cannot leak reasoning into the reply (or drop the answer's opening
   token after it). `message_id` becomes the next turn's `parent_message_id`.

Credentials are sent as `authorization: Bearer <token>` plus the `ds_session_id`
cookie, with the site's `x-app-version` / `x-client-version` /
`x-client-platform` / `x-client-bundle-id` headers and origin/referer.

The PoW challenge is short-lived, so a failed completion (transport hiccup,
HTTP 401/403/429) automatically re-solves a fresh challenge once.

## Config

```bash
dscli config path        # e.g. ~/.config/dscli/dscli.json
dscli config init
dscli config set token <token>
dscli config unset token
dscli --config-file /path/to/dscli.json config set token <token>
```

The `session` key holds the persisted default conversation position
(`<session_id>:<message_id>`, set automatically on every persisted turn;
`dscli session forget` clears it and the next run starts a fresh thread).

A `dscli.json` in the current directory takes precedence over the per-user
config file (a warning is printed when that happens implicitly), and
`DSCLI_CONFIG_FILE` overrides both. The config is written atomically with
0600 permissions.

## Development

```bash
npm install
npm run build        # tsc + copy the vendored wasm into dist/
npm test             # node --test via tsx: 100+ tests, no network needed

npm link             # run the local build as `dscli`
```

The PoW solver tests run DeepSeek's actual wasm against a known golden
challenge (answer 999, exact header), so no network or login is needed.

## Notes

- **PoW internals.** DeepSeekHashV1 turns out to be a Keccak variant: SHA3-style
  `0x06` domain padding, Keccak-f[1600] with rounds 1..23, over
  `salt_expireAt_<nonce>`; the challenge is the expected digest, `difficulty`
  bounds the nonce search. This repo does not reimplement it (the vendored wasm
  is the authority) — the variant knowledge is only used to generate offline
  test vectors.
- Project structure follows [pi](https://github.com/earendil-works/pi-mono)
  (`@earendil-works/pi-coding-agent`): `src/cli` argument parsing and command
  dispatch, `src/config.ts` as the single source for app identity and paths,
  `src/core/<domain>` for the engine modules, and `src/modes/interactive` for
  the terminal UI.
