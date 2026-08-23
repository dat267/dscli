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
  chat            Chat with DeepSeek (omit the prompt for an interactive
                  session)
  translate       Translate a file (txt, md, lrc, srt, vtt, ass, ttml, epub) via
                  the model
  login           Show how to capture your DeepSeek login (token + cookie)
  version         Show version
  config init     Generate a default configuration file
  config path     Show configuration file path
  config show     Print current configuration values
  config set      Set a config value
  config unset    Unset a config value

Run "dscli <command> --help" for more information on a command.
```

## Quick start

```bash
# Install
go install github.com/dat267/dscli@latest

# Capture your session (one-time)
dscli login

# Save it (the values live only in your config file, created with 0600 perms)
dscli config set token "<token>"
dscli config set cookie "<cookie>"

# Ask a question (the reply streams like a chat)
dscli chat "Explain the halting problem in one paragraph"

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

Inside the REPL (status line is dimmed; the `you>` prompt is styled only when
attached to a terminal):

```
DeepSeek · model default · thinking off · search off · ephemeral (deleted on close)
one question per line · /help for commands
you> What is 2+2?
4

you> /model expert              # switch model (starts a fresh conversation)
you> /thinking on               # or bare /thinking to flip it
you> /search off
you> /new                       # start a fresh conversation
you> /exit
conversation: 0123456789:42
```

Replies stream to stdout as they are generated; a blank line separates turns
(even when the model text ends without a newline). Piped input produces no
prompts, and colours are disabled when stderr is not a terminal.

**No persistence.** Launching `dscli chat` without `-c` creates a *fresh*
session, keeps every turn in that one session, and deletes it when the REPL
closes — `/exit`, `/quit`, Ctrl-D, or Ctrl-C (a delete failure only prints a
warning). `/new` and `/model` spawn additional sessions inside the same run;
those are cleaned up on close too. So the conversation id shown is the live
thread's id — useful while the session lasts, but the session is gone once
you leave. One line per question; multi-line input is not supported.

## Custom translation styles

Every language pair can carry its own translation instructions. Inside each
chunk prompt, the style is appended after the format rules; the `translate_file`
chat tool resolves the same way, so CLI and chat mode stay consistent.

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

drop your own
`translate/zh-en.md` (or point `--instructions` at any file) and it applies
to that pair everywhere:

```bash
dscli translate book.lrc --from Japanese --to English   # picks up translate/ja-en.md
dscli translate --instructions my-style.md chapter.md  # explicit file for any pair
```

## Batch translation in chat

With `/files` on you can drive translation as part of an organising session —
translate several files, then rename/move the outputs:

```
you> translate all .lrc files in Music/ into Chinese
translate_file Music/one.lrc
Music/one.lrc → Music/one.translated.lrc (lrc, 1 chunks, to Chinese)
translate Music/one.lrc → Music/one.translated.lrc to Chinese? [y/N] y
  chunk 1/1 ok
…
you> /help
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
you> /thinking on
you> /search off
you> /model expert          # switches model; starts a fresh conversation
```

A thread's model is fixed when it is created: `--model`/`/model` always start
a new conversation, and `--model` cannot be combined with `--conversation`.
DeepThink and search can be toggled freely at any point of a thread — and
**both default to off** (the status line in the REPL shows the current state:
`thinking off · search off`).

## File tools

The model can read and edit files in a working directory — enabled per run
with `--file-tools` (and `--workdir` to scope it, defaulting to the current
directory), or mid-session with `/files`:

```bash
dscli chat --file-tools "what does this repo's Makefile do?"
dscli chat --file-tools --workdir /path/to/project "rename the Foo function to Bar"
```

It calls a tool by replying with *only* a JSON object:

```json
{"tool":"list_directory","path":".","recursive":true}
{"tool":"file_meta","path":"song.lrc"}
{"tool":"read_file","path":"book.epub"}
{"tool":"translate_file","path":"song.lrc","to":"Chinese"}
{"tool":"create_file","path":"notes.txt","content":"hello"}
{"tool":"edit_file","path":"src/cli.go","old":"func Foo(","new":"func Bar("}
{"tool":"delete_file","path":"old.txt"}
```

- **`list_directory` and `read_file` run without prompting** — reading and
  directory listing inside the workdir are always allowed. `file_meta`
  reports size, mode, modified time, EPUB title/author, LRC/VTT duration,
  and directory entry counts/total size without prompting. `list_directory`
  lists ONE directory per call (directories marked with a trailing `/`,
  files with human-readable sizes); add `"recursive": true` to map the whole
  subtree in a single bounded call (≤ 500 entries, ≤ 6 levels deep) instead
  of one call per directory — the model is instructed to prefer this when
  exploring. `read_file` only reads text, **but `.epub` files are
  auto-extracted** (ZIP of XHTML → spine-ordered chapters, markup stripped,
  capped at the read limit) so books are readable.: binary content (NUL-byte detection over the leading
  8 KB) and files larger than the size ceiling are rejected outright — never
  truncated — and the rejection is fed back to the model, which is told not to
  retry. The ceiling defaults to 512 KiB and is tunable per run:

  ```bash
  dscli chat --file-tools --file-max-read 100000 "summarize client.go"
  ```

  `list_directory` lists ONE directory, non-recursive, directories marked
  with a trailing `/`. The model is instructed to start with
  `{"tool":"list_directory","path":"."}` to discover files.
- **`translate_file`** bundles the whole translate pipeline into one tool
  call (handy for bulk translating a library in chat mode): `path`, `to`
  (target language), optional `output`. It previews (`song.lrc →
  song.translated.lrc`, format + chunk count), asks, then translates in a
  dedicated ephemeral session using the exact same engine as `dscli
  translate` — timestamps/XML/markup preserved and verified — writing the
  result and telling the model it succeeded. Input is capped at 1 MiB per
  call (use `dscli translate` for bigger files).
- **`create_file`, `edit_file` and `delete_file` always ask first — after
  showing a deterministic preview.** `create_file` makes a *new* file (it
  errors if the path already exists, and creates parent directories within
  the workdir); `edit_file` replaces the first exact occurrence; `delete_file`
  removes a file and previews its head. Every write/delete is planned on the
  actual file bytes, previewed line-by-line (`-` removed, `+` added), then
  applied with exactly the planned content — so what you approve is precisely
  what happens:

  ```
  a.txt — replacing first of 1 occurrence(s)
        1 │ module x
  -     2 │ go 1.26.5
  +     2 │ go 1.27.0
        3 │ require (
  ```

  The confirmation answer is read from the controlling terminal (`/dev/tty`),
  so it never collides with the REPL's input; if no terminal is available the
  write/delete is denied and the model is told not to retry. If an edit
  pattern doesn't match, that surfaces at the preview step (nothing is
  prompted) and the model is told to re-read the file and copy the exact
  text.
- Tool calls resolve within the same session (each `read_file`/`edit_file`
  turn feeds a `<tool_result>` back), hard-capped at 12 tool calls per
  user message — after the budget the model is forced to give a final prose
  answer, so exploring a very large tree can never loop indefinitely. Listings
  are read in bounded batches, so a directory with hundreds of thousands of
  entries costs ~200 entries of work, not a full scan. The raw JSON never reaches your screen; a dim
  note shows each call (`read_file Makefile`). All paths are confined to the
  workdir: lexical `..` escapes **and symlink chains leading outside** are
  rejected via canonical resolution, writes preserve the target's file mode,
  and every apply re-verifies the file still matches the preview (a file
  changed in between — or a create target that appeared — refuses the
  operation instead of clobbering). The session is still deleted on close.

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
   json-patch frames: the snapshot frame (`fragments[].type == "response"`)
   plus append frames on `response/fragments/-1/content`; only response
   fragment text is emitted. `message_id` (from `v.message_id` / `v.message.id`
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

- Long files are split into ~24 KiB chunks (line boundaries preserved) and
  translated turn-by-turn in one clean session; each chunk streams through
  the same PoW/SSE pipeline.
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
  overwritten without `-f`. The session is ephemeral, like chat (created per
  run, deleted when the run ends).
