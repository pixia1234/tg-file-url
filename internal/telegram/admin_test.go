package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pixia1234/tg-file-url/internal/config"
	"github.com/pixia1234/tg-file-url/internal/database"
)

type sentMessageRequest struct {
	ChatID          int64 `json:"chat_id"`
	Text            string
	ReplyParameters struct {
		MessageID int64 `json:"message_id"`
	} `json:"reply_parameters"`
}

type telegramAPIMock struct {
	server *httptest.Server

	mu           sync.Mutex
	sentMessages []sentMessageRequest
}

func newTelegramAPIMock(t *testing.T) *telegramAPIMock {
	t.Helper()

	mock := &telegramAPIMock{}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottest-token/sendMessage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}

		var payload sentMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}

		mock.mu.Lock()
		mock.sentMessages = append(mock.sentMessages, payload)
		mock.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 501,
				"chat": map[string]any{
					"id":   payload.ChatID,
					"type": "private",
				},
			},
		})
	}))

	return mock
}

func (m *telegramAPIMock) Close() {
	m.server.Close()
}

func (m *telegramAPIMock) Messages() []sentMessageRequest {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]sentMessageRequest, len(m.sentMessages))
	copy(out, m.sentMessages)
	return out
}

func newTestBot(t *testing.T) (*Bot, *database.Store, *telegramAPIMock) {
	t.Helper()

	cfg := &config.Config{
		SQLitePath:  filepath.Join(t.TempDir(), "test.db"),
		HTTPTimeout: 5 * time.Second,
		Owners: map[int64]struct{}{
			1: {},
		},
	}

	store, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	mock := newTelegramAPIMock(t)
	client := NewClient("test-token", mock.server.URL, 5*time.Second)
	client.httpClient = mock.server.Client()

	bot := NewBot(cfg, store, client)
	t.Cleanup(func() {
		mock.Close()
		_ = store.Close()
	})

	return bot, store, mock
}

func TestCheckAccessRejectsUnauthorizedUser(t *testing.T) {
	bot, _, mock := newTestBot(t)
	msg := &Message{
		MessageID: 77,
		Chat:      Chat{ID: 42, Type: "private"},
		From:      &User{ID: 42, FirstName: "Test"},
		Text:      "/start",
	}

	allowed, err := bot.checkAccess(context.Background(), msg)
	if err != nil {
		t.Fatalf("checkAccess returned error: %v", err)
	}
	if allowed {
		t.Fatal("expected unauthorized user to be rejected")
	}

	messages := mock.Messages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Text != unauthorizedUserText(42) {
		t.Fatalf("unexpected unauthorized text: %q", messages[0].Text)
	}
	if messages[0].ReplyParameters.MessageID != 77 {
		t.Fatalf("unexpected reply message id: %d", messages[0].ReplyParameters.MessageID)
	}
}

func TestCheckAccessAllowsAuthorizedUser(t *testing.T) {
	bot, store, mock := newTestBot(t)
	if err := store.AuthorizeUser(context.Background(), 42, 1); err != nil {
		t.Fatalf("authorize user: %v", err)
	}

	msg := &Message{
		MessageID: 88,
		Chat:      Chat{ID: 42, Type: "private"},
		From:      &User{ID: 42, FirstName: "Authorized"},
		Text:      "/start",
	}

	allowed, err := bot.checkAccess(context.Background(), msg)
	if err != nil {
		t.Fatalf("checkAccess returned error: %v", err)
	}
	if !allowed {
		t.Fatal("expected authorized user to pass access check")
	}
	if len(mock.Messages()) != 0 {
		t.Fatalf("expected no denial messages, got %d", len(mock.Messages()))
	}
}

func TestCheckAccessRejectsDeauthorizedUser(t *testing.T) {
	bot, store, mock := newTestBot(t)
	if err := store.AuthorizeUser(context.Background(), 42, 1); err != nil {
		t.Fatalf("authorize user: %v", err)
	}
	ok, err := store.DeauthorizeUser(context.Background(), 42)
	if err != nil {
		t.Fatalf("deauthorize user: %v", err)
	}
	if !ok {
		t.Fatal("expected deauthorize to remove the user")
	}

	msg := &Message{
		MessageID: 99,
		Chat:      Chat{ID: 42, Type: "private"},
		From:      &User{ID: 42, FirstName: "Removed"},
		Text:      "/help",
	}

	allowed, err := bot.checkAccess(context.Background(), msg)
	if err != nil {
		t.Fatalf("checkAccess returned error: %v", err)
	}
	if allowed {
		t.Fatal("expected deauthorized user to be rejected")
	}

	messages := mock.Messages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Text != unauthorizedUserText(42) {
		t.Fatalf("unexpected deauthorized text: %q", messages[0].Text)
	}
}

func TestCheckAccessAllowsOwnerWithoutAuthorization(t *testing.T) {
	bot, _, mock := newTestBot(t)

	msg := &Message{
		MessageID: 123,
		Chat:      Chat{ID: 1, Type: "private"},
		From:      &User{ID: 1, FirstName: "Owner"},
		Text:      "/status",
	}

	allowed, err := bot.checkAccess(context.Background(), msg)
	if err != nil {
		t.Fatalf("checkAccess returned error: %v", err)
	}
	if !allowed {
		t.Fatal("expected owner to bypass authorization gate")
	}
	if len(mock.Messages()) != 0 {
		t.Fatalf("expected no denial messages, got %d", len(mock.Messages()))
	}
}

func TestCheckAccessRejectsSenderChatWithoutUserIdentity(t *testing.T) {
	bot, _, mock := newTestBot(t)

	msg := &Message{
		MessageID: 124,
		Chat:      Chat{ID: -100987654321, Type: "supergroup", Title: "Group"},
		SenderChat: &Chat{
			ID:    -100987654321,
			Type:  "supergroup",
			Title: "Group",
		},
		Text: "/link",
	}

	allowed, err := bot.checkAccess(context.Background(), msg)
	if err != nil {
		t.Fatalf("checkAccess returned error: %v", err)
	}
	if allowed {
		t.Fatal("expected sender_chat message to be rejected")
	}

	messages := mock.Messages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Text != unauthorizedIdentityText(msg) {
		t.Fatalf("unexpected sender_chat denial text: %q", messages[0].Text)
	}
	if messages[0].ReplyParameters.MessageID != 124 {
		t.Fatalf("unexpected reply message id: %d", messages[0].ReplyParameters.MessageID)
	}
}
