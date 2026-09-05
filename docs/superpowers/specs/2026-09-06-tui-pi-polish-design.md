# TUI Pi-style polish — design

Date: 2026-09-06
Status: implemented

## Goal

Polish the interactive `dscli chat` TUI to read like Pi's chat: assistant
output rendered as markdown, a pulsing working indicator while a reply
streams, and consistent turn spacing. Zero new dependencies.

## Non-goals

- Syntax highlighting inside code blocks (dim border + language label only).
- Changing user-line styling, system notes, or the input box.
- Scrollbar/"jump to bottom" affordances.

## 1. Markdown renderer (`cmd/markdown.go`)

`renderMarkdown(text string, width int) []string` returns styled,
display-width-safe rows. Line-oriented, state-machine over the input:

| Construct | Rendering |
|---|---|
| `#`/`##` | bold + mint accent, blank line after |
| `###`–`######` | bold |
| `**bold**`, `*italic*`, `~~x~~`, `` `code` `` | ANSI bold/italic/strike; inline code dim |
| fenced blocks (` ``` `/`~~~ `) | dim box (`─`/`│`), dim language label right-aligned on the top border, content hard-wrapped inside, 1-space padding |
| lists (`-`,`*`,`+`,`N.`) | dim `•` bullet (numbered kept), indent nesting |
| blockquotes (`>`) | dim `▎` bar, dim text |
| `---`/`***`/`___` | dim full-width rule |
| `[text](url)` | accent text, dim url |
| paragraphs | blank line between, word-wrapped by display width |

Invariant: every returned row's visible width ≤ `width`.

Wrapping: a styled-aware word wrapper (`wrapStyled`) breaks rows at spaces
tracking ANSI state and re-emits active codes at row starts, so mid-paragraph
inline styles survive wraps.

Commit-time width: turns are rendered to rows at the width in effect when
committed to the scrollback (existing scroll wrap re-flows plain lines on
resize; box rows degrade gracefully on narrowing, as in Pi).

## 2. Streaming integration (active-turn buffer)

- `startTurn`'s answer callback already accumulates raw text in `m.reply` —
  unchanged.
- `streamDelta` no longer appends raw text to `m.scroll`; it only triggers a
  re-render. While busy, `render()` appends `renderMarkdown(m.reply, width)`
  rows below the committed scrollback, so partial turns re-render smoothly
  each frame (unclosed fences render as open boxes).
- On `streamDone`, the rendered reply is committed to the scrollback
  (markdown rows) and the transcript still saves the raw `m.reply` text.
- Scroll math (`scrollUp/Down`, clamp, pin-to-bottom) operates on the combined
  row set: scrollback rows + active-turn rows.
- `renderHistory` renders assistant history as markdown; user lines cyan.

## 3. Spinner

While `busy`, the separator rule doubles as the working indicator — pi-style
`── ⠋ Working ───` (pi's braille frames, ~120ms, accent dot, muted label) —
so streaming never changes the pane layout: `outputRows` is identical busy
or idle. `startTurn` schedules a self-rescheduling `tea.Tick`; the tick
advances the frame and re-renders; `streamDone` stops it. Deltas alone
cannot animate it (thinking pauses).

## 4. Turn spacing

Blank line before each user message and after each reply (mostly existing).
No rules between turns.

## 5. Tests

- `cmd/markdown_test.go`: per-construct table tests + the width invariant on
  every output row.
- `tui_test.go`: partial-fence-during-stream renders an open box; commit on
  `streamDone` puts rendered rows in the scrollback (transcript still raw);
  spinner row appears while busy and animates on tick; history renders
  markdown; existing layout tests adapted.
- README: one line in the chat section about markdown rendering.
