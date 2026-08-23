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
DeepSeek · model default · ephemeral session (deleted on close)
one question per line · /help for commands
you> What is 2+2?
4

you> /model expert              # switch model (starts a fresh conversation)
you> /thinking on
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

## Models, DeepThink & web search

Model, DeepThink (thinking) and web search are per-thread or per-request
toggles:

```bash
dscli chat -m expert "explain Gödel's incompleteness"   # strong model
dscli chat -t "reason step by step"                     # DeepThink
dscli chat -s "latest Mars rover news"                  # web search
dscli chat -t -s -m expert "both, on the strong model"
```

Inside the REPL the same switches are slash commands (see `/help`):

```
you> /thinking on
you> /search off
you> /model expert          # switches model; starts a fresh conversation
```

A thread's model is fixed when it is created: `--model`/`/model` always start
a new conversation, and `--model` cannot be combined with `--conversation`.
DeepThink and search can be toggled freely at any point of a thread.

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