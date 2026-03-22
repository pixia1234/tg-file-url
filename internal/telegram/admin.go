package telegram

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pixia1234/tg-file-url/internal/database"
)

const noBanReason = "No reason provided."

func (b *Bot) checkAccess(ctx context.Context, msg *Message) (bool, error) {
	if msg == nil {
		return false, nil
	}

	if msg.From == nil {
		text := unauthorizedIdentityText(msg)
		if text != "" {
			if err := b.client.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	if msg.From != nil && !b.cfg.IsOwner(msg.From.ID) {
		ban, err := b.store.GetBannedUser(ctx, msg.From.ID)
		switch {
		case err == nil:
			text := fmt.Sprintf(
				"Access denied.\nReason: %s\nBanned at: %s",
				firstNonEmpty(strings.TrimSpace(ban.Reason), noBanReason),
				ban.BannedAt.Format(time.RFC3339),
			)
			if err := b.client.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID); err != nil {
				return false, err
			}
			return false, nil
		case errors.Is(err, database.ErrNotFound):
		default:
			return false, err
		}
	}

	if msg.Chat.ID < 0 && (msg.From == nil || !b.cfg.IsOwner(msg.From.ID)) {
		ban, err := b.store.GetBannedChannel(ctx, msg.Chat.ID)
		switch {
		case err == nil:
			if strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
				text := fmt.Sprintf(
					"This chat is banned.\nReason: %s\nBanned at: %s",
					firstNonEmpty(strings.TrimSpace(ban.Reason), noBanReason),
					ban.BannedAt.Format(time.RFC3339),
				)
				if err := b.client.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID); err != nil {
					return false, err
				}
			}
			return false, nil
		case errors.Is(err, database.ErrNotFound):
		default:
			return false, err
		}
	}

	if msg.From != nil && !b.cfg.IsOwner(msg.From.ID) {
		authorized, err := b.store.IsUserAuthorized(ctx, msg.From.ID)
		if err != nil {
			return false, err
		}
		if !authorized {
			if err := b.client.SendMessage(ctx, msg.Chat.ID, unauthorizedUserText(msg.From.ID), msg.MessageID); err != nil {
				return false, err
			}
			return false, nil
		}
	}

	return true, nil
}

func unauthorizedUserText(userID int64) string {
	return fmt.Sprintf("Please contact the administrator for authorization.\nYour ID is: %d", userID)
}

func unauthorizedIdentityText(msg *Message) string {
	if msg == nil {
		return ""
	}
	if msg.SenderChat != nil {
		return "Please contact the administrator for authorization.\nAnonymous or sender chat identities are not supported."
	}
	return "Please contact the administrator for authorization."
}

func (b *Bot) handleAuthorize(ctx context.Context, msg *Message, args string) error {
	userID, err := parsePositiveInt64Arg(args)
	if err != nil {
		return b.client.SendMessage(ctx, msg.Chat.ID, "Usage: /authorize <user_id>", msg.MessageID)
	}

	if err := b.store.AuthorizeUser(ctx, userID, msg.From.ID); err != nil {
		return err
	}
	return b.client.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("Authorized user: %d", userID), msg.MessageID)
}

func (b *Bot) handleDeauthorize(ctx context.Context, msg *Message, args string) error {
	userID, err := parsePositiveInt64Arg(args)
	if err != nil {
		return b.client.SendMessage(ctx, msg.Chat.ID, "Usage: /deauthorize <user_id>", msg.MessageID)
	}

	ok, err := b.store.DeauthorizeUser(ctx, userID)
	if err != nil {
		return err
	}
	if !ok {
		return b.client.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("User %d is not in the authorized list.", userID), msg.MessageID)
	}
	return b.client.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("Removed authorized user: %d", userID), msg.MessageID)
}

func (b *Bot) handleListAuth(ctx context.Context, msg *Message) error {
	users, err := b.store.ListAuthorizedUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return b.client.SendMessage(ctx, msg.Chat.ID, "No authorized users found.", msg.MessageID)
	}

	var lines []string
	lines = append(lines, "Authorized Users:")
	for i, user := range users {
		lines = append(lines, fmt.Sprintf("%d. %d authorized_by=%d at=%s", i+1, user.UserID, user.AuthorizedBy, user.AuthorizedAt.Format(time.RFC3339)))
	}
	return b.client.SendMessage(ctx, msg.Chat.ID, strings.Join(lines, "\n"), msg.MessageID)
}

func (b *Bot) handleBan(ctx context.Context, msg *Message, args string) error {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 {
		return b.client.SendMessage(ctx, msg.Chat.ID, "Usage: /ban <user_id|chat_id> [reason]", msg.MessageID)
	}

	targetID, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return b.client.SendMessage(ctx, msg.Chat.ID, "Usage: /ban <user_id|chat_id> [reason]", msg.MessageID)
	}
	if targetID == msg.From.ID {
		return b.client.SendMessage(ctx, msg.Chat.ID, "Refusing to ban the configured owner.", msg.MessageID)
	}

	reason := strings.TrimSpace(strings.TrimPrefix(args, fields[0]))
	reason = firstNonEmpty(reason, noBanReason)

	if targetID < 0 {
		if err := b.store.BanChannel(ctx, targetID, msg.From.ID, reason); err != nil {
			return err
		}
		response := fmt.Sprintf("Banned chat: %d\nReason: %s", targetID, reason)
		if err := b.client.SendMessage(ctx, msg.Chat.ID, response, msg.MessageID); err != nil {
			return err
		}
		if err := b.client.LeaveChat(ctx, targetID); err != nil {
			log.Printf("leave banned chat %d failed: %v", targetID, err)
		}
		return nil
	}

	if b.cfg.IsOwner(targetID) {
		return b.client.SendMessage(ctx, msg.Chat.ID, "Refusing to ban the configured owner.", msg.MessageID)
	}
	if err := b.store.BanUser(ctx, targetID, msg.From.ID, reason); err != nil {
		return err
	}
	response := fmt.Sprintf("Banned user: %d\nReason: %s", targetID, reason)
	if err := b.client.SendMessage(ctx, msg.Chat.ID, response, msg.MessageID); err != nil {
		return err
	}
	if err := b.client.SendMessage(ctx, targetID, "You have been banned from using this bot.", 0); err != nil {
		log.Printf("ban notification to %d failed: %v", targetID, err)
	}
	return nil
}

func (b *Bot) handleUnban(ctx context.Context, msg *Message, args string) error {
	targetID, err := parseSignedInt64Arg(args)
	if err != nil {
		return b.client.SendMessage(ctx, msg.Chat.ID, "Usage: /unban <user_id|chat_id>", msg.MessageID)
	}

	if targetID < 0 {
		ok, err := b.store.UnbanChannel(ctx, targetID)
		if err != nil {
			return err
		}
		if !ok {
			return b.client.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("Chat %d is not banned.", targetID), msg.MessageID)
		}
		return b.client.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("Unbanned chat: %d", targetID), msg.MessageID)
	}

	ok, err := b.store.UnbanUser(ctx, targetID)
	if err != nil {
		return err
	}
	if !ok {
		return b.client.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("User %d is not in the ban list.", targetID), msg.MessageID)
	}
	if err := b.client.SendMessage(ctx, targetID, "Your access to this bot has been restored.", 0); err != nil {
		log.Printf("unban notification to %d failed: %v", targetID, err)
	}
	return b.client.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("Unbanned user: %d", targetID), msg.MessageID)
}

func (b *Bot) handleBroadcast(ctx context.Context, msg *Message, args string) error {
	if msg.ReplyToMessage == nil {
		return b.client.SendMessage(ctx, msg.Chat.ID, "Reply to a message with /broadcast [all|authorized|regular].", msg.MessageID)
	}

	mode := strings.ToLower(strings.TrimSpace(args))
	if mode == "" {
		mode = "all"
	}

	var (
		recipients []int64
		err        error
	)
	switch mode {
	case "all":
		recipients, err = b.store.ListAllUserIDs(ctx)
	case "authorized":
		recipients, err = b.store.ListAuthorizedUserIDs(ctx)
	case "regular":
		recipients, err = b.store.ListRegularUserIDs(ctx)
	default:
		return b.client.SendMessage(ctx, msg.Chat.ID, "Usage: /broadcast [all|authorized|regular] as a reply to the message to copy.", msg.MessageID)
	}
	if err != nil {
		return err
	}
	if len(recipients) == 0 {
		return b.client.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("No recipients found for broadcast mode: %s", mode), msg.MessageID)
	}

	if !b.startBroadcast() {
		return b.client.SendMessage(ctx, msg.Chat.ID, "A broadcast is already running.", msg.MessageID)
	}

	status, err := b.client.SendMessageResult(
		ctx,
		msg.Chat.ID,
		fmt.Sprintf("Broadcast started.\nMode: %s\nRecipients: %d", mode, len(recipients)),
		msg.MessageID,
	)
	if err != nil {
		b.finishBroadcast()
		return err
	}

	go b.runBroadcast(ctx, status.Chat.ID, status.MessageID, msg.Chat.ID, msg.ReplyToMessage.MessageID, mode, recipients)
	return nil
}

func (b *Bot) handleLog(ctx context.Context, msg *Message) error {
	info, err := os.Stat(b.cfg.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Log file does not exist yet.", msg.MessageID)
		}
		return err
	}
	if info.Size() == 0 {
		return b.client.SendMessage(ctx, msg.Chat.ID, "Log file is empty.", msg.MessageID)
	}
	caption := fmt.Sprintf("%s log", b.botName())
	return b.client.SendDocument(ctx, msg.Chat.ID, b.cfg.LogPath, caption, msg.MessageID)
}

func (b *Bot) handleRestart(ctx context.Context, msg *Message) error {
	status, err := b.client.SendMessageResult(ctx, msg.Chat.ID, "Restarting service...", msg.MessageID)
	if err != nil {
		return err
	}
	if err := b.store.SaveRestartNotice(ctx, status.Chat.ID, status.MessageID); err != nil {
		log.Printf("save restart notice failed: %v", err)
	}

	go func() {
		time.Sleep(750 * time.Millisecond)
		log.Printf("restart requested by %d", msg.From.ID)
		os.Exit(0)
	}()
	return nil
}

func (b *Bot) notifyRestarted(ctx context.Context) error {
	notice, err := b.store.GetRestartNotice(ctx)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return nil
		}
		return err
	}
	defer func() {
		if err := b.store.ClearRestartNotice(ctx); err != nil {
			log.Printf("clear restart notice failed: %v", err)
		}
	}()

	text := fmt.Sprintf("Restart completed.\nBot: %s\nTime: %s", b.botName(), time.Now().UTC().Format(time.RFC3339))
	if err := b.client.EditMessageText(ctx, notice.ChatID, notice.MessageID, text); err != nil {
		log.Printf("edit restart notice failed: %v", err)
	}
	return nil
}

func (b *Bot) startBroadcast() bool {
	b.broadcastMu.Lock()
	defer b.broadcastMu.Unlock()
	if b.broadcastRunning {
		return false
	}
	b.broadcastRunning = true
	return true
}

func (b *Bot) finishBroadcast() {
	b.broadcastMu.Lock()
	b.broadcastRunning = false
	b.broadcastMu.Unlock()
}

func (b *Bot) runBroadcast(ctx context.Context, ownerChatID, statusMessageID, sourceChatID, sourceMessageID int64, mode string, recipients []int64) {
	defer b.finishBroadcast()

	started := time.Now()
	var successCount int
	var failureCount int
	var deletedCount int

	for _, userID := range recipients {
		if err := b.client.CopyMessage(ctx, userID, sourceChatID, sourceMessageID); err != nil {
			failureCount++
			var apiErr *APIError
			if errors.As(err, &apiErr) && shouldDeleteUserAfterBroadcastFailure(apiErr) {
				authorized, authErr := b.store.IsUserAuthorized(ctx, userID)
				if authErr != nil {
					log.Printf("broadcast auth lookup failed for %d: %v", userID, authErr)
				} else if !authorized {
					if err := b.store.DeleteUser(ctx, userID); err != nil {
						log.Printf("broadcast delete user %d failed: %v", userID, err)
					} else {
						deletedCount++
					}
				}
			}
			continue
		}
		successCount++
	}

	text := fmt.Sprintf(
		"Broadcast completed.\nMode: %s\nElapsed: %s\nRecipients: %d\nSuccess: %d\nFailed: %d\nDeleted stale users: %d",
		mode,
		humanDuration(time.Since(started)),
		len(recipients),
		successCount,
		failureCount,
		deletedCount,
	)
	if err := b.client.EditMessageText(ctx, ownerChatID, statusMessageID, text); err != nil {
		log.Printf("broadcast completion edit failed: %v", err)
		if err := b.client.SendMessage(ctx, ownerChatID, text, 0); err != nil {
			log.Printf("broadcast completion send failed: %v", err)
		}
	}
}

func shouldDeleteUserAfterBroadcastFailure(err *APIError) bool {
	if err == nil {
		return false
	}

	description := strings.ToLower(strings.TrimSpace(err.Description))
	for _, pattern := range []string{
		"bot was blocked by the user",
		"user is deactivated",
		"chat not found",
		"user not found",
	} {
		if strings.Contains(description, pattern) {
			return true
		}
	}
	return false
}

func parsePositiveInt64Arg(args string) (int64, error) {
	value, err := parseSignedInt64Arg(args)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return value, nil
}

func parseSignedInt64Arg(args string) (int64, error) {
	field := firstField(args)
	if field == "" {
		return 0, fmt.Errorf("missing argument")
	}
	return strconv.ParseInt(field, 10, 64)
}

func firstField(text string) string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
