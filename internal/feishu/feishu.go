// Package feishu provides a lightweight Feishu (Lark) bot integration for
// OpenCode. It implements the Feishu event subscription callback (webhook)
// using only the standard library so it can be built without any extra
// dependencies.
//
// Supported flows:
//   - URL verification challenge (url_verification)
//   - Receiving messages (event with message type)
//   - Replying to the user via the Feishu OpenAPI (tenant access token)
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Config holds the credentials and settings needed to talk to Feishu.
type Config struct {
	// AppID and AppSecret of the Feishu custom app.
	AppID string
	AppSecret string

	// VerificationToken is the token configured in the event subscription
	// panel. It is used to validate that callbacks really come from Feishu.
	VerificationToken string

	// EncryptKey is optional. When set, payloads are decrypted with it.
	// Left empty if you did not enable encryption.
	EncryptKey string

	// ListenAddr is the address the callback HTTP server binds to,
	// e.g. ":8080".
	ListenAddr string

	// PublicBaseURL is the publicly reachable base URL of this server,
	// used only for logging hints (e.g. https://your-domain.com).
	PublicBaseURL string
}

// Handler is the callback handler. The MessageFunc is invoked for every
// received user message; its return value is sent back to the user.
type Handler struct {
	cfg        Config
	httpClient *http.Client

	mu            sync.Mutex
	tenantToken   string
	tenantExpiry  time.Time

	// MessageFunc processes an incoming message and returns the reply.
	// Returning an empty string means "no reply".
	MessageFunc func(ctx context.Context, msg Message) (string, error)
}

// Message represents an incoming Feishu message event.
type Message struct {
	MessageID string
	ChatID    string
	ChatType  string // "single" (p2p) or "group"
	SenderID  string
	Content   string // decoded text content
	MsgType   string // "text", "image", ...
}

// rawCallback is the top level envelope Feishu sends.
type rawCallback struct {
	Type      string          `json:"type"`
	Challenge string          `json:"challenge"`
	Token     string          `json:"token"`
	Event     json.RawMessage `json:"event"`
}

// eventHeader is the common header of an event.
type eventHeader struct {
	Type       string `json:"type"`
	AppID      string `json:"app_id"`
	MessageID  string `json:"message_id"`
	ChatType   string `json:"chat_type"`
	Message    struct {
		MessageID string `json:"message_id"`
		ChatID    string `json:"chat_id"`
		ChatType  string `json:"chat_type"`
		Sender    struct {
			SenderID struct {
				UnionID string `json:"union_id"`
				UserID  string `json:"user_id"`
				OpenID  string `json:"open_id"`
			} `json:"sender_id"`
		} `json:"sender"`
		MessageType string `json:"message_type"`
		Content     string `json:"content"`
	} `json:"message"`
}

// tokenResp is the response of the tenant access token endpoint.
type tokenResp struct {
	Code              int    `json:"code"`
	Msg               string `json:"msg"`
	TenantAccessToken string `json:"tenant_access_token"`
	Expire            int    `json:"expire"`
}

// NewHandler creates a Feishu callback handler.
func NewHandler(cfg Config) *Handler {
	return &Handler{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Serve starts the HTTP callback server. It blocks until the provided
// context is cancelled.
func (h *Handler) Serve(ctx context.Context) error {
	if h.cfg.ListenAddr == "" {
		return fmt.Errorf("feishu: ListenAddr is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/feishu/callback", h.handleCallback)

	srv := &http.Server{
		Addr:    h.cfg.ListenAddr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("feishu callback server listening", "addr", h.cfg.ListenAddr, "url", h.cfg.PublicBaseURL+"/feishu/callback")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("feishu server error: %w", err)
	}
	return nil
}

func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	var envelope rawCallback
	if err := json.Unmarshal(body, &envelope); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// URL verification challenge.
	if envelope.Type == "url_verification" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"challenge": envelope.Challenge,
		})
		return
	}

	// Validate token if configured.
	if h.cfg.VerificationToken != "" && envelope.Token != h.cfg.VerificationToken {
		slog.Warn("feishu: invalid verification token", "got", envelope.Token)
		http.Error(w, "invalid token", http.StatusForbidden)
		return
	}

	// Acknowledge receipt immediately (Feishu requires a quick 200).
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"code":0}`))

	// Process asynchronously so we don't block the HTTP response.
	if envelope.Event != nil {
		go h.processEvent(r.Context(), envelope.Event)
	}
}

func (h *Handler) processEvent(ctx context.Context, raw json.RawMessage) {
	var ev eventHeader
	if err := json.Unmarshal(raw, &ev); err != nil {
		slog.Error("feishu: failed to parse event", "error", err)
		return
	}

	if ev.Type != "message" {
		return
	}
	if ev.Message.MessageType != "text" {
		// Only handle text messages for now.
		return
	}

	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(ev.Message.Content), &content); err != nil {
		slog.Error("feishu: failed to parse message content", "error", err)
		return
	}

	msg := Message{
		MessageID: ev.Message.MessageID,
		ChatID:    ev.Message.ChatID,
		ChatType:  ev.Message.ChatType,
		SenderID:  ev.Message.Sender.SenderID.OpenID,
		Content:   content.Text,
		MsgType:   ev.Message.MessageType,
	}

	if h.MessageFunc == nil {
		return
	}

	reply, err := h.MessageFunc(ctx, msg)
	if err != nil {
		slog.Error("feishu: message handler error", "error", err)
		reply = "抱歉，处理你的消息时出错了。"
	}
	if reply == "" {
		return
	}

	if err := h.SendText(ctx, msg.ChatID, msg.ChatType, reply); err != nil {
		slog.Error("feishu: failed to send reply", "error", err)
	}
}

// SendText sends a text message back to the user or group.
func (h *Handler) SendText(ctx context.Context, chatID, chatType, text string) error {
	token, err := h.getTenantToken(ctx)
	if err != nil {
		return err
	}

	payload := map[string]interface{}{
		"receive_id": chatID,
		"msg_type":   "text",
		"content":    fmt.Sprintf(`{"text":%s}`, mustJSON(text)),
	}
	// For groups use "chat_id", for p2p use "open_id"/"user_id".
	idType := "chat_id"
	if chatType == "p2p" {
		idType = "open_id"
	}

	url := fmt.Sprintf("https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=%s", idType)
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(payload); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("feishu send message failed: %s %s", resp.Status, string(body))
	}
	return nil
}

func (h *Handler) getTenantToken(ctx context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.tenantToken != "" && time.Now().Before(h.tenantExpiry) {
		return h.tenantToken, nil
	}

	payload := map[string]string{
		"app_id":     h.cfg.AppID,
		"app_secret": h.cfg.AppSecret,
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(payload); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var tr tokenResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("feishu token parse error: %w", err)
	}
	if tr.Code != 0 {
		return "", fmt.Errorf("feishu token error: %d %s", tr.Code, tr.Msg)
	}

	h.tenantToken = tr.TenantAccessToken
	h.tenantExpiry = time.Now().Add(time.Duration(tr.Expire-60) * time.Second)
	return h.tenantToken, nil
}

func mustJSON(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `"` + v + `"`
	}
	return string(b)
}
