package deepseek

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// patchParser turns DeepSeek's SSE json-patch stream into reply-text deltas.
//
// The stream sends an initial snapshot frame whose v is the whole response
// object (fragments[].type == "response" carries the content and the
// assistant message_id lives at v.message_id / v.message.id or inside v.response),
// then a series of append frames:
//
//	{"p":"response/fragments/-1/content","o":"APPEND","v":" He"}
//	{"v":"llo"}
//
// Only text appended to a path ending in "/content" is treated as reply text.
// Search-enabled replies may also carry TOOL_SEARCH fragments (with
// references/results) or .../results patch paths; those are captured into
// Sources so the CLI can print the citations the model references inline as
// [citation:N].
type patchParser struct {
	activePath  string
	messageID   *int64
	sawSnapshot bool
	emittedAny  bool // any reply text emitted so far (gates SET-as-initial)
	sources     []Source
}

// Feed processes one SSE data payload (a single JSON patch operation) and
// calls emit for each reply-text delta. Malformed payloads are skipped, like
// the website's client does.
func (p *patchParser) Feed(payload []byte, emit func(string) error) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil // not JSON: ignore
	}

	v, hasV := obj["v"]

	// Snapshot frame: v is the whole response object.
	if hasV {
		if snap, ok := v.(map[string]any); ok {
			if _, isResponse := snap["response"]; isResponse {
				p.captureMessageID(snap)
				if frags, ok := mapAny(snap["response"])["fragments"].([]any); ok {
					for _, f := range frags {
						fm, ok := f.(map[string]any)
						if !ok {
							continue
						}
						t, _ := fm["type"].(string)
						switch {
						case strings.EqualFold(t, "response"):
							// The content path is live even when the snapshot
							// text is empty: the first real chunk may arrive
							// as a pathless {"v":...} before any "p" frame.
							p.activePath = "response/fragments/-1/content"
							content, _ := fm["content"].(string)
							if content == "" {
								continue
							}
							// Only the first response fragment's content is
							// pre-generated; later text arrives as appends.
							if !p.sawSnapshot {
								p.sawSnapshot = true
								if err := emit(content); err != nil {
									return err
								}
								p.emittedAny = true
							}
						case strings.EqualFold(t, "tool_search"):
							p.collectSources(fm)
						}
					}
				}
				return nil
			}
		}
	}

	// Path-setting frame (single patch, or BATCH of nested patches).
	if path, ok := obj["p"].(string); ok {
		p.activePath = path
		if strings.HasSuffix(path, "message_id") {
			if id := numberToInt64(v); id != nil {
				p.messageID = id
			}
		}
		op, _ := obj["o"].(string)
		if op == "BATCH" {
			if items, ok := v.([]any); ok {
				for _, it := range items {
					if m, ok := it.(map[string]any); ok {
						pp, _ := m["p"].(string)
						oo, _ := m["o"].(string)
						if err := p.applyPatch(pp, m["v"], oo, emit); err != nil {
							return err
						}
					}
				}
			}
			return nil
		}
		return p.applyPatch(path, v, op, emit)
	}

	// Bare (pathless) chunk: per upstream behaviour these are visible-text
	// candidates, and they may arrive before any path is active (this is
	// what used to eat the reply's first characters).
	return p.applyPathless(v, emit)
}

// applyPatch handles one content/results patch operation.
func (p *patchParser) applyPatch(path string, v any, op string, emit func(string) error) error {
	if strings.HasSuffix(path, "/results") || strings.HasSuffix(path, "/references") {
		p.collectSources(v)
	}
	if !strings.HasSuffix(path, "content") {
		return nil
	}
	switch op {
	case "APPEND":
		if txt, ok := textOf(v); ok {
			return emit(txt)
		}
	case "SET":
		// SET replaces the whole content slot; only treat it as the initial
		// text, before anything has been emitted, to avoid duplicates.
		if !p.emittedAny {
			if txt, ok := textOf(v); ok {
				if err := emit(txt); err != nil {
					return err
				}
				p.emittedAny = true
			}
		}
	}
	return nil
}

// applyPathless emits a pathless chunk when the active path is empty or a
// content path (status-path values are control signals, not text).
func (p *patchParser) applyPathless(v any, emit func(string) error) error {
	if p.activePath != "" && !strings.HasSuffix(p.activePath, "content") {
		return nil
	}
	txt, ok := textOf(v)
	if !ok {
		return nil
	}
	return emit(txt)
}

// textOf extracts visible text from a chunk value: a plain string, or an
// object carrying a "text"/"content" string field.
func textOf(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case map[string]any:
		for _, k := range []string{"text", "content"} {
			if s, ok := t[k].(string); ok {
				return s, true
			}
		}
	}
	return "", false
}

// collectSources appends citation sources found in a TOOL_SEARCH fragment or
// a .../results patch value, deduplicated by URL.
func (p *patchParser) collectSources(v any) {
	if m, ok := v.(map[string]any); ok {
		for _, key := range []string{"references", "results", "result"} {
			if items, ok := m[key].([]any); ok {
				p.addSourceItems(items)
			}
		}
		return
	}
	if items, ok := v.([]any); ok {
		p.addSourceItems(items)
	}
}

func (p *patchParser) addSourceItems(items []any) {
	for _, it := range items {
		s := sourceFromItem(it)
		if s.URL == "" {
			continue
		}
		dup := false
		for _, have := range p.sources {
			if have.URL == s.URL {
				dup = true
				break
			}
		}
		if !dup {
			p.sources = append(p.sources, s)
		}
	}
}

// sourceFromItem extracts {url, title} from a search-result item, accepting
// the common key spellings; a bare URL string also counts.
func sourceFromItem(it any) Source {
	switch item := it.(type) {
	case string:
		if strings.HasPrefix(item, "http://") || strings.HasPrefix(item, "https://") {
			return Source{URL: item}
		}
	case map[string]any:
		url := firstString(item, "url", "link", "href", "source")
		title := firstString(item, "title", "name", "label")
		return Source{URL: url, Title: title}
	}
	return Source{}
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}

// captureMessageID best-effort: pull the assistant message_id out of a
// snapshot frame, checking v.response first, then the snapshot root, and
// accepting "message_id" or "id".
func (p *patchParser) captureMessageID(snap map[string]any) {
	for _, container := range []map[string]any{mapAny(snap["response"]), snap} {
		for _, key := range []string{"message_id", "id"} {
			if id := numberToInt64(container[key]); id != nil {
				p.messageID = id
				return
			}
		}
	}
}

func mapAny(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func numberToInt64(v any) *int64 {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return &i
		}
		if f, err := n.Float64(); err == nil {
			i := int64(f)
			return &i
		}
	case float64:
		i := int64(n)
		return &i
	case int64:
		return &n
	case int:
		i := int64(n)
		return &i
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return &i
		}
	}
	return nil
}
