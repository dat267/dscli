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
type patchParser struct {
	activePath string
	messageID  *int64
	sawSnapshot bool
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
						if !strings.EqualFold(t, "response") {
							continue
						}
						content, _ := fm["content"].(string)
						if content == "" {
							continue
						}
						p.activePath = "response/fragments/-1/content"
						// Only the first response fragment's content is
						// pre-generated; later text arrives as appends.
						if !p.sawSnapshot {
							p.sawSnapshot = true
							if err := emit(content); err != nil {
								return err
							}
						}
					}
				}
				return nil
			}
		}
	}

	// Path-setting append frame.
	if path, ok := obj["p"].(string); ok {
		p.activePath = path
		if strings.HasSuffix(path, "message_id") {
			if id := numberToInt64(v); id != nil {
				p.messageID = id
			}
		}
		if o, _ := obj["o"].(string); o == "APPEND" {
			if txt, ok := v.(string); ok && strings.HasSuffix(path, "content") {
				return emit(txt)
			}
		}
		return nil
	}

	// Bare append to the current path.
	if txt, ok := v.(string); ok && p.activePath != "" && strings.HasSuffix(p.activePath, "content") {
		return emit(txt)
	}
	return nil
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