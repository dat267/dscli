package deepseek

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Endpoints and constants of chat.deepseek.com's internal web API.
const (
	BaseURL           = "https://chat.deepseek.com"
	CompletionPath    = "/api/v0/chat/completion"
	powChallengePath  = "/api/v0/chat/create_pow_challenge"
	sessionCreatePath = "/api/v0/chat_session/create"
	sessionDeletePath = "/api/v0/chat_session/delete"
	historyPath       = "/api/v0/chat/history_messages"

	// One-minute ceiling for the small JSON exchanges; the completion stream
	// is bounded by the caller-supplied context instead.
	shortTimeout = 30 * time.Second

	// DefaultUserAgent mimics a current desktop Chrome so the WAF in front of
	// the site does not reject the plain HTTP client outright.
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// ErrCredentials reports that no usable session was configured.
var ErrCredentials = errors.New("no DeepSeek session configured")

// sseDebugDump, when set via DSCLI_DEBUG_SSE=<file>, receives every raw SSE
// data payload as it arrives (one per line), for diagnosing stream
// reconstruction issues against the live site. Optional; nil by default.
var sseDebugDump io.Writer

func init() {
	if path := os.Getenv("DSCLI_DEBUG_SSE"); path != "" {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
			sseDebugDump = f
		}
	}
}

// Session carries the signed-in credentials captured from the website
// (token from localStorage.userToken, ds_session_id from the cookies).
type Session struct {
	Token     string
	Cookie    string // ds_session_id value, or a full "k=v; ..." cookie header
	UserAgent string // optional; zero value falls back to DefaultUserAgent
}

// Client is a stateful HTTP client for DeepSeek's web API.
type Client struct {
	http *http.Client
	base string // API base URL; overridable for tests
	sess Session
	ua   string
}

// NewClient builds a client for the given session. timeout bounds the whole
// completion exchange (including streaming); zero means no bound (rely on the
// context passed per call). A base URL beyond the default may be supplied for
// tests or proxies.
func NewClient(sess Session, timeout time.Duration, base ...string) *Client {
	ua := sess.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	b := BaseURL
	if len(base) > 0 && base[0] != "" {
		b = base[0]
	}
	return &Client{
		http: &http.Client{Timeout: timeout},
		base: b,
		sess: sess,
		ua:   ua,
	}
}

// sessionCookie renders the Cookie header. The config stores the bare
// ds_session_id value; a full "k=v; k2=v2" string (any input containing "=")
// is passed through untouched.
func sessionCookie(s string) string {
	if s == "" {
		return ""
	}
	if strings.Contains(s, "=") {
		return s
	}
	return "ds_session_id=" + s
}

func (c *Client) headers() http.Header {
	h := http.Header{}
	h.Set("authorization", "Bearer "+c.sess.Token)
	if cookie := sessionCookie(c.sess.Cookie); cookie != "" {
		h.Set("cookie", cookie)
	}
	h.Set("content-type", "application/json")
	h.Set("accept", "*/*")
	h.Set("user-agent", c.ua)
	h.Set("origin", BaseURL)
	h.Set("referer", BaseURL+"/")
	h.Set("x-app-version", "2.0.0")
	h.Set("x-client-version", "2.0.0")
	h.Set("x-client-platform", "web")
	h.Set("x-client-bundle-id", "com.deepseek.chat")
	h.Set("x-client-locale", "en_US")
	h.Set("x-client-timezone-offset", "19800")
	return h
}

// bizEnvelope is the standard {code, data:{biz_data}} wrapper.
type bizEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		BizData json.RawMessage `json:"biz_data"`
	} `json:"data"`
}

// biz checks the envelope's code and unmarshals data.biz_data into out.
func (e *bizEnvelope) biz(out any) error {
	if e.Code != 0 {
		msg := e.Msg
		if msg == "" {
			msg = fmt.Sprintf("code=%d", e.Code)
		}
		return fmt.Errorf("deepseek api error: %s", msg)
	}
	if len(e.Data.BizData) == 0 {
		return fmt.Errorf("deepseek api error: missing data.biz_data")
	}
	if err := json.Unmarshal(e.Data.BizData, out); err != nil {
		return fmt.Errorf("deepseek api error: bad biz_data: %w", err)
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, path string, body any, out *bizEnvelope) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header = c.headers()
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return httpStatusError(path, resp)
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("deepseek api error: decode response: %w", err)
	}
	return nil
}

// httpStatusError reads a non-200 body and turns it into an error, preferring
// the site's JSON {code, msg} envelope when present.
func httpStatusError(path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(body, &env) == nil && (env.Code != 0 || env.Msg != "") {
		msg := env.Msg
		if msg == "" {
			msg = fmt.Sprintf("code=%d", env.Code)
		}
		return fmt.Errorf("deepseek api error: %s (HTTP %d for %s)", msg, resp.StatusCode, path)
	}
	snippet := strings.TrimSpace(string(body))
	if snippet == "" {
		snippet = resp.Status
	}
	if len(snippet) > 300 {
		snippet = snippet[:300] + "..."
	}
	return fmt.Errorf("POST %s failed with HTTP %d: %s", path, resp.StatusCode, snippet)
}

// CreateChatSession starts a new chat session and returns its id.
func (c *Client) CreateChatSession(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, shortTimeout)
	defer cancel()
	var env bizEnvelope
	if err := c.postJSON(ctx, sessionCreatePath, map[string]any{}, &env); err != nil {
		return "", err
	}
	var data struct {
		Session struct {
			ID string `json:"id"`
		} `json:"chat_session"`
	}
	if err := env.biz(&data); err != nil {
		return "", err
	}
	if data.Session.ID == "" {
		return "", fmt.Errorf("deepseek api error: chat session response missing id")
	}
	return data.Session.ID, nil
}

// DeleteSessions removes chat sessions server-side. The delete endpoint takes
// a batch of ids; an empty slice is a no-op. Only the standard envelope code
// matters in the response (no biz_data is returned).
func (c *Client) DeleteSessions(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, shortTimeout)
	defer cancel()
	var env bizEnvelope
	if err := c.postJSON(ctx, sessionDeletePath, map[string]any{"chat_session_ids": ids}, &env); err != nil {
		return err
	}
	if env.Code != 0 {
		msg := env.Msg
		if msg == "" {
			msg = fmt.Sprintf("code=%d", env.Code)
		}
		return fmt.Errorf("deepseek api error: %s", msg)
	}
	return nil
}

// HistoryMessage is one past message of a chat session, as returned by
// ChatHistory. Role is "USER" or "ASSISTANT"; Content is the visible text.
type HistoryMessage struct {
	MessageID int64  `json:"message_id"`
	ParentID  *int64 `json:"parent_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Status    string `json:"status"`
	Fragments []struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	} `json:"fragments"`
}

// Text returns the message's visible text: the content field when set, else
// the fragments' content with thinking text excluded (assistant replies carry
// the final text in the content field, but fragments are the authoritative
// source and REQUEST/TOOL fragments hold it too).
func (m HistoryMessage) Text() string {
	if m.Content != "" {
		return m.Content
	}
	var b strings.Builder
	for _, f := range m.Fragments {
		switch strings.ToUpper(f.Type) {
		case "THINK", "THINKING", "":
			continue
		}
		b.WriteString(f.Content)
	}
	return b.String()
}

// ChatHistory fetches a chat session's past messages for display. The model
// already carries the thread's context server-side, so this is purely for the
// UI (rendering the resume point); it needs no PoW challenge.
func (c *Client) ChatHistory(ctx context.Context, sessionID string) ([]HistoryMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, shortTimeout)
	defer cancel()
	u := c.base + historyPath + "?chat_session_id=" + url.QueryEscape(sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.headers()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, httpStatusError(historyPath, resp)
	}
	var env bizEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode history: %w", err)
	}
	var data struct {
		ChatMessages []HistoryMessage `json:"chat_messages"`
	}
	if err := env.biz(&data); err != nil {
		return nil, err
	}
	return data.ChatMessages, nil
}

// fetchChallenge fetches a PoW challenge for the completion endpoint.
func (c *Client) fetchChallenge(ctx context.Context) (Challenge, error) {
	ctx, cancel := context.WithTimeout(ctx, shortTimeout)
	defer cancel()
	var env bizEnvelope
	if err := c.postJSON(ctx, powChallengePath, map[string]string{"target_path": CompletionPath}, &env); err != nil {
		return Challenge{}, err
	}
	var data struct {
		Challenge Challenge `json:"challenge"`
	}
	if err := env.biz(&data); err != nil {
		return Challenge{}, err
	}
	if data.Challenge.Challenge == "" {
		return Challenge{}, fmt.Errorf("deepseek api error: pow challenge response missing challenge")
	}
	return data.Challenge, nil
}

// powHeader fetches a challenge and solves it, returning the base64
// x-ds-pow-response header value.
func (c *Client) powHeader(ctx context.Context) (string, error) {
	ch, err := c.fetchChallenge(ctx)
	if err != nil {
		return "", err
	}
	return PowHeader(ctx, ch)
}

// CompletionRequest is the body of POST /api/v0/chat/completion.
type CompletionRequest struct {
	ChatSessionID   string
	ParentMessageID *int64 // nil on the first turn (sent as JSON null)
	Prompt          string
	ModelType       string // "default"/"expert" on the first turn; "" omits the field when resuming
	ThinkingEnabled bool
	SearchEnabled   bool
}

func (r CompletionRequest) body() map[string]any {
	b := map[string]any{
		"chat_session_id":   r.ChatSessionID,
		"parent_message_id": r.ParentMessageID,
		"prompt":            r.Prompt,
		"ref_file_ids":      []any{},
		"thinking_enabled":  r.ThinkingEnabled,
		"search_enabled":    r.SearchEnabled,
		"action":            nil,
		"preempt":           false,
	}
	if r.ModelType != "" {
		b["model_type"] = r.ModelType
	}
	return b
}

// Reply carries a completed stream: the assistant message_id needed to resume
// the conversation with ParentMessageID on the next turn.
type Reply struct {
	MessageID int64
	// Sources are the search citations the reply's inline [citation:N]
	// markers refer to, in order (empty when search was off or no sources).
	Sources []Source
	// Truncated is true when the stream reported the reply was cut short
	// (INCOMPLETE/WIP/AUTO_CONTINUE/CONTENT_FILTER) rather than FINISHED —
	// i.e. the model stopped at its output limit. Callers may retry.
	Truncated bool
	// Filtered is true when the stream ended with CONTENT_FILTER: the reply
	// was rejected by the content-safety filter (a censor), distinct from a
	// plain output-length cut-off.
	Filtered bool
}

// Source is one search citation.
type Source struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

// StreamCompletion runs the full completion flow — fetch+solve a fresh PoW
// challenge, POST the completion, and feed every reply-text delta to emit.
// It returns the assistant message_id. The PoW challenge is short-lived, so a
// single automatic retry re-solves a fresh challenge when the first attempt
// fails with a transport error or an auth/pow-style HTTP error.
func (c *Client) StreamCompletion(ctx context.Context, req CompletionRequest, emit func(string) error) (Reply, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		pow, err := c.powHeader(ctx)
		if err != nil {
			return Reply{}, err
		}
		reply, err := c.streamOnce(ctx, req, pow, emit)
		if err == nil {
			return reply, nil
		}
		lastErr = err
		if !retryable(err) {
			return Reply{}, err
		}
	}
	return Reply{}, lastErr
}

func (c *Client) streamOnce(ctx context.Context, req CompletionRequest, pow string, emit func(string) error) (Reply, error) {
	buf, err := json.Marshal(req.body())
	if err != nil {
		return Reply{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+CompletionPath, bytes.NewReader(buf))
	if err != nil {
		return Reply{}, err
	}
	httpReq.Header = c.headers()
	httpReq.Header.Set("x-ds-pow-response", pow)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Reply{}, httpStatusError(CompletionPath, resp)
	}

	parser := &patchParser{}
	var messageID int64
	if err := readSSE(resp.Body, func(payload []byte) error {
		if sseDebugDump != nil {
			_, _ = fmt.Fprintf(sseDebugDump, "%s\n", payload)
		}
		return parser.Feed(payload, emit)
	}); err != nil {
		return Reply{}, err
	}
	if parser.messageID != nil {
		messageID = *parser.messageID
	}
	return Reply{MessageID: messageID, Sources: parser.sources, Truncated: parser.truncated, Filtered: parser.filtered}, nil
}

// readSSE reads an SSE stream, joining each event's "data:" lines and calling
// handle with the whole payload.
func readSSE(r io.Reader, handle func(payload []byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var fields []string
	flush := func() error {
		if len(fields) == 0 {
			return nil
		}
		payload := strings.Join(fields, "\n")
		fields = fields[:0]
		return handle([]byte(payload))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			content := strings.TrimPrefix(line, "data:")
			// Per the SSE spec a single leading space is stripped.
			content = strings.TrimPrefix(content, " ")
			fields = append(fields, content)
		}
		// "event:", "id:", "retry:", and ":" comment lines are ignored.
	}
	if err := flush(); err != nil {
		return err
	}
	return scanner.Err()
}

// retryable reports whether a completion attempt should be retried with a
// fresh PoW challenge. PoW headers are short-lived, so transport hiccups and
// anything resembling a challenge/rate-limit/auth rejection qualify.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "pow challenge") || strings.Contains(msg, "challenge") ||
		strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "HTTP 403") ||
		strings.Contains(msg, "HTTP 429") {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}
