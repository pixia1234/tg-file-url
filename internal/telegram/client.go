package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Client struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

func NewClient(token, baseURL string, timeout time.Duration) *Client {
	return &Client{
		token:   token,
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var user User
	if err := c.call(ctx, "getMe", map[string]any{}, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSeconds int) ([]Update, error) {
	var updates []Update
	payload := map[string]any{
		"offset":          offset,
		"timeout":         timeoutSeconds,
		"allowed_updates": []string{"message"},
	}

	if err := c.call(ctx, "getUpdates", payload, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyToMessageID int64) error {
	_, err := c.SendMessageResult(ctx, chatID, text, replyToMessageID)
	return err
}

func (c *Client) SendMessageResult(ctx context.Context, chatID int64, text string, replyToMessageID int64) (*Message, error) {
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	if replyToMessageID != 0 {
		payload["reply_parameters"] = map[string]any{"message_id": replyToMessageID}
	}

	var msg Message
	if err := c.call(ctx, "sendMessage", payload, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	payload := map[string]any{
		"chat_id":                  chatID,
		"message_id":               messageID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	return c.call(ctx, "editMessageText", payload, nil)
}

func (c *Client) CopyMessage(ctx context.Context, toChatID, fromChatID, messageID int64) error {
	payload := map[string]any{
		"chat_id":      toChatID,
		"from_chat_id": fromChatID,
		"message_id":   messageID,
	}
	return c.call(ctx, "copyMessage", payload, nil)
}

func (c *Client) ForwardMessage(ctx context.Context, toChatID, fromChatID, messageID int64) (*Message, error) {
	payload := map[string]any{
		"chat_id":      toChatID,
		"from_chat_id": fromChatID,
		"message_id":   messageID,
	}

	var msg Message
	if err := c.call(ctx, "forwardMessage", payload, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func (c *Client) LeaveChat(ctx context.Context, chatID int64) error {
	return c.call(ctx, "leaveChat", map[string]any{"chat_id": chatID}, nil)
}

func (c *Client) SendDocument(ctx context.Context, chatID int64, filePath, caption string, replyToMessageID int64) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return err
	}
	if strings.TrimSpace(caption) != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return err
		}
	}
	if replyToMessageID != 0 {
		replyParameters, err := json.Marshal(map[string]any{"message_id": replyToMessageID})
		if err != nil {
			return err
		}
		if err := writer.WriteField("reply_parameters", string(replyParameters)); err != nil {
			return err
		}
	}

	part, err := writer.CreateFormFile("document", filepath.Base(filePath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, file); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, "sendDocument"),
		body,
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var envelope apiResponse
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return fmt.Errorf("decode telegram response: %w", err)
	}
	if !envelope.OK {
		return &APIError{
			Method:      "sendDocument",
			Description: envelope.Description,
			ErrorCode:   envelope.ErrorCode,
		}
	}
	return nil
}

func (c *Client) GetFile(ctx context.Context, fileID string) (*File, error) {
	payload := map[string]any{"file_id": fileID}
	var file File
	if err := c.call(ctx, "getFile", payload, &file); err != nil {
		return nil, err
	}
	return &file, nil
}

func (c *Client) BuildFileURL(filePath string) string {
	return fmt.Sprintf("%s/file/bot%s/%s", c.baseURL, c.token, strings.TrimLeft(filePath, "/"))
}

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	return c.httpClient.Do(req)
}

func (c *Client) call(ctx context.Context, method string, payload any, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method),
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var envelope apiResponse
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return fmt.Errorf("decode telegram response: %w", err)
	}

	if !envelope.OK {
		return &APIError{
			Method:      method,
			Description: envelope.Description,
			ErrorCode:   envelope.ErrorCode,
		}
	}

	if target == nil {
		return nil
	}

	if err := json.Unmarshal(envelope.Result, target); err != nil {
		return fmt.Errorf("decode telegram result: %w", err)
	}

	return nil
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

type APIError struct {
	Method      string
	Description string
	ErrorCode   int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s failed: %s (code %d)", e.Method, e.Description, e.ErrorCode)
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

type Message struct {
	MessageID      int64       `json:"message_id"`
	Date           int64       `json:"date"`
	Chat           Chat        `json:"chat"`
	From           *User       `json:"from"`
	SenderChat     *Chat       `json:"sender_chat"`
	Text           string      `json:"text"`
	Caption        string      `json:"caption"`
	ReplyToMessage *Message    `json:"reply_to_message"`
	Document       *Document   `json:"document"`
	Photo          []PhotoSize `json:"photo"`
	Video          *Video      `json:"video"`
	Audio          *Audio      `json:"audio"`
	Voice          *Voice      `json:"voice"`
	Animation      *Animation  `json:"animation"`
	VideoNote      *VideoNote  `json:"video_note"`
}

type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

func (c Chat) IsPrivate() bool {
	return c.Type == "private"
}

func (c Chat) DisplayName() string {
	switch {
	case c.Title != "":
		return c.Title
	case c.FirstName != "" && c.LastName != "":
		return c.FirstName + " " + c.LastName
	case c.FirstName != "":
		return c.FirstName
	case c.Username != "":
		return "@" + c.Username
	default:
		return fmt.Sprintf("chat-%d", c.ID)
	}
}

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

func (u *User) DisplayName() string {
	if u == nil {
		return "Unknown User"
	}
	switch {
	case u.FirstName != "" && u.LastName != "":
		return u.FirstName + " " + u.LastName
	case u.FirstName != "":
		return u.FirstName
	case u.Username != "":
		return "@" + u.Username
	default:
		return fmt.Sprintf("user-%d", u.ID)
	}
}

type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
	FilePath     string `json:"file_path"`
}

type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size"`
}

type Video struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type Audio struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type Voice struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type Animation struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type VideoNote struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
}
