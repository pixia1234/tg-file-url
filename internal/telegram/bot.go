package telegram

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pixia1234/tg-file-url/internal/app"
	"github.com/pixia1234/tg-file-url/internal/config"
	"github.com/pixia1234/tg-file-url/internal/database"
	"github.com/pixia1234/tg-file-url/internal/files"
)

type Bot struct {
	cfg       *config.Config
	store     *database.Store
	client    *Client
	me        *User
	startedAt time.Time

	broadcastMu      sync.Mutex
	broadcastRunning bool
}

type MediaFile struct {
	FileID       string
	FileUniqueID string
	FileName     string
	MimeType     string
	FileSize     int64
}

func NewBot(cfg *config.Config, store *database.Store, client *Client) *Bot {
	return &Bot{
		cfg:       cfg,
		store:     store,
		client:    client,
		startedAt: time.Now().UTC(),
	}
}

func (b *Bot) Run(ctx context.Context) error {
	me, err := b.client.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram getMe: %w", err)
	}
	b.me = me
	log.Printf("telegram bot connected as @%s", me.Username)
	if err := b.notifyRestarted(ctx); err != nil {
		log.Printf("restart notification failed: %v", err)
	}

	var offset int64

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		updates, err := b.client.GetUpdates(ctx, offset, int(b.cfg.PollTimeout/time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("telegram getUpdates error: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		for _, update := range updates {
			offset = update.UpdateID + 1
			if update.Message == nil {
				continue
			}

			if err := b.handleMessage(ctx, update.Message); err != nil {
				log.Printf("telegram message handler error: %v", err)
			}
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *Message) error {
	if msg.From != nil {
		if err := b.store.UpsertUser(ctx, msg.From.ID, msg.From.Username, msg.From.FirstName, msg.From.LastName); err != nil {
			log.Printf("upsert user %d failed: %v", msg.From.ID, err)
		}
	}

	allowed, err := b.checkAccess(ctx, msg)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}

	if strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		return b.handleCommand(ctx, msg)
	}

	if msg.Chat.IsPrivate() {
		if _, ok := extractMedia(msg); ok {
			return b.ingestMedia(ctx, msg, msg)
		}
	}

	return nil
}

func (b *Bot) handleCommand(ctx context.Context, msg *Message) error {
	command, args := parseCommand(msg.Text)
	isOwner := msg.From != nil && b.cfg.IsOwner(msg.From.ID)

	switch command {
	case "start":
		return b.client.SendMessage(ctx, msg.Chat.ID, startText(b.cfg.PublicBaseURL, isOwner), msg.MessageID)
	case "help":
		return b.client.SendMessage(ctx, msg.Chat.ID, helpText(b.cfg.PublicBaseURL, isOwner), msg.MessageID)
	case "about":
		return b.client.SendMessage(ctx, msg.Chat.ID, aboutText(b.cfg.PublicBaseURL, b.me), msg.MessageID)
	case "ping":
		return b.handlePing(ctx, msg)
	case "status":
		if !isOwner {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Only configured owners can use /status.", msg.MessageID)
		}
		return b.handleStatus(ctx, msg)
	case "stats":
		if !isOwner {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Only configured owners can use /stats.", msg.MessageID)
		}
		return b.handleStats(ctx, msg)
	case "users":
		if !isOwner {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Only configured owners can use /users.", msg.MessageID)
		}
		return b.handleUsers(ctx, msg)
	case "authorize":
		if !isOwner {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Only configured owners can use /authorize.", msg.MessageID)
		}
		return b.handleAuthorize(ctx, msg, args)
	case "deauthorize":
		if !isOwner {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Only configured owners can use /deauthorize.", msg.MessageID)
		}
		return b.handleDeauthorize(ctx, msg, args)
	case "listauth":
		if !isOwner {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Only configured owners can use /listauth.", msg.MessageID)
		}
		return b.handleListAuth(ctx, msg)
	case "ban":
		if !isOwner {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Only configured owners can use /ban.", msg.MessageID)
		}
		return b.handleBan(ctx, msg, args)
	case "unban":
		if !isOwner {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Only configured owners can use /unban.", msg.MessageID)
		}
		return b.handleUnban(ctx, msg, args)
	case "broadcast":
		if !isOwner {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Only configured owners can use /broadcast.", msg.MessageID)
		}
		return b.handleBroadcast(ctx, msg, args)
	case "log":
		if !isOwner {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Only configured owners can use /log.", msg.MessageID)
		}
		return b.handleLog(ctx, msg)
	case "restart":
		if !isOwner {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Only configured owners can use /restart.", msg.MessageID)
		}
		return b.handleRestart(ctx, msg)
	case "link":
		if msg.ReplyToMessage == nil {
			return b.client.SendMessage(ctx, msg.Chat.ID, "Reply to a media message with /link.", msg.MessageID)
		}
		if _, ok := extractMedia(msg.ReplyToMessage); !ok {
			return b.client.SendMessage(ctx, msg.Chat.ID, "The replied message does not contain a supported file.", msg.MessageID)
		}
		return b.ingestMedia(ctx, msg, msg.ReplyToMessage)
	default:
		return nil
	}
}

func (b *Bot) handlePing(ctx context.Context, msg *Message) error {
	started := time.Now()
	sent, err := b.client.SendMessageResult(ctx, msg.Chat.ID, "Pinging...", msg.MessageID)
	if err != nil {
		return err
	}

	text := fmt.Sprintf(
		"PONG!\nLatency: %d ms\nBot uptime: %s",
		time.Since(started).Milliseconds(),
		humanDuration(time.Since(b.startedAt)),
	)
	if err := b.client.EditMessageText(ctx, msg.Chat.ID, sent.MessageID, text); err != nil {
		log.Printf("telegram edit ping message failed: %v", err)
		return b.client.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID)
	}
	return nil
}

func (b *Bot) handleStatus(ctx context.Context, msg *Message) error {
	stats, err := b.store.Stats(ctx)
	if err != nil {
		return err
	}

	text := fmt.Sprintf(
		"System Status: operational\n\nBot: %s\nVersion: %s\nStarted: %s\nUptime: %s\nUsers: %d\nFiles: %d\nGoroutines: %d\nBIN_CHANNEL: %d\nSQLite: %s\nPublic URL: %s",
		b.botName(),
		app.Version,
		b.startedAt.Format(time.RFC3339),
		humanDuration(time.Since(b.startedAt)),
		stats.Users,
		stats.Files,
		runtime.NumGoroutine(),
		b.cfg.BinChannel,
		b.cfg.SQLitePath,
		b.cfg.PublicBaseURL,
	)
	return b.client.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID)
}

func (b *Bot) handleStats(ctx context.Context, msg *Message) error {
	dbStats, err := b.store.Stats(ctx)
	if err != nil {
		return err
	}

	hostUptime, _ := readHostUptime()
	totalRAM, availableRAM, _ := readMemInfo()
	diskTotal, diskUsed, diskFree, _ := readDiskUsage(diskUsagePath(b.cfg.SQLitePath))

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	text := fmt.Sprintf(
		"System Statistics\n\nHost uptime: %s\nBot uptime: %s\nCPU cores: %d\nGoroutines: %d\nGo version: %s\n\nMemory:\nHost total: %s\nHost available: %s\nGo alloc: %s\nGo system: %s\n\nStorage:\nDisk total: %s\nDisk used: %s\nDisk free: %s\nSQLite path: %s\n\nDatabase:\nUsers: %d\nFiles: %d",
		formatOptionalDuration(hostUptime),
		humanDuration(time.Since(b.startedAt)),
		runtime.NumCPU(),
		runtime.NumGoroutine(),
		runtime.Version(),
		humanOptionalSize(totalRAM),
		humanOptionalSize(availableRAM),
		files.HumanSize(int64(mem.Alloc)),
		files.HumanSize(int64(mem.Sys)),
		humanOptionalSize(diskTotal),
		humanOptionalSize(diskUsed),
		humanOptionalSize(diskFree),
		b.cfg.SQLitePath,
		dbStats.Users,
		dbStats.Files,
	)
	return b.client.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID)
}

func (b *Bot) handleUsers(ctx context.Context, msg *Message) error {
	stats, err := b.store.Stats(ctx)
	if err != nil {
		return err
	}

	text := fmt.Sprintf(
		"Database Statistics\n\nTotal users: %d\nStored files: %d",
		stats.Users,
		stats.Files,
	)
	return b.client.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID)
}

func (b *Bot) ingestMedia(ctx context.Context, requestMessage, sourceMessage *Message) error {
	sourceMedia, ok := extractMedia(sourceMessage)
	if !ok {
		return b.client.SendMessage(ctx, requestMessage.Chat.ID, "Unsupported media type.", requestMessage.MessageID)
	}

	forwarded, err := b.client.ForwardMessage(ctx, b.cfg.BinChannel, sourceMessage.Chat.ID, sourceMessage.MessageID)
	if err != nil {
		return b.client.SendMessage(ctx, requestMessage.Chat.ID, "Failed to store the file in BIN_CHANNEL. Check bot admin permissions.", requestMessage.MessageID)
	}

	if forwardedMedia, ok := extractMedia(forwarded); ok {
		sourceMedia = forwardedMedia
	}

	uploaderID := int64(0)
	uploaderName := sourceMessage.Chat.DisplayName()
	if sourceMessage.From != nil {
		uploaderID = sourceMessage.From.ID
		uploaderName = sourceMessage.From.DisplayName()
	}

	record := files.Record{
		MessageID:           forwarded.MessageID,
		SecureHash:          files.ComputeSecureHash(sourceMedia.FileUniqueID),
		LinkToken:           files.ComputeLinkToken(sourceMedia.FileUniqueID, forwarded.MessageID),
		StorageChatID:       b.cfg.BinChannel,
		SourceChatID:        sourceMessage.Chat.ID,
		SourceMessageID:     sourceMessage.MessageID,
		FileID:              sourceMedia.FileID,
		FileUniqueID:        sourceMedia.FileUniqueID,
		FileName:            files.SanitizeFileName(sourceMedia.FileName),
		MimeType:            sourceMedia.MimeType,
		FileSize:            sourceMedia.FileSize,
		UploaderUserID:      uploaderID,
		UploaderDisplayName: uploaderName,
	}

	if record.MimeType == "" {
		record.MimeType = "application/octet-stream"
	}

	if err := b.store.SaveFile(ctx, record); err != nil {
		return err
	}

	links := files.BuildLinks(b.cfg.PublicBaseURL, record.LinkToken, record.FileName)
	response := fmt.Sprintf(
		"Stored: %s\nSize: %s\n\nStream: %s\nDownload: %s",
		record.FileName,
		files.HumanSize(record.FileSize),
		links.StreamURL,
		links.DownloadURL,
	)

	return b.client.SendMessage(ctx, requestMessage.Chat.ID, response, requestMessage.MessageID)
}

func extractMedia(msg *Message) (MediaFile, bool) {
	switch {
	case msg == nil:
		return MediaFile{}, false
	case msg.Document != nil:
		return MediaFile{
			FileID:       msg.Document.FileID,
			FileUniqueID: msg.Document.FileUniqueID,
			FileName:     fallbackFileName(msg.Document.FileName, msg.Document.MimeType, "document", msg.MessageID),
			MimeType:     firstNonEmpty(msg.Document.MimeType, "application/octet-stream"),
			FileSize:     msg.Document.FileSize,
		}, true
	case msg.Video != nil:
		return MediaFile{
			FileID:       msg.Video.FileID,
			FileUniqueID: msg.Video.FileUniqueID,
			FileName:     fallbackFileName(msg.Video.FileName, msg.Video.MimeType, "video", msg.MessageID),
			MimeType:     firstNonEmpty(msg.Video.MimeType, "video/mp4"),
			FileSize:     msg.Video.FileSize,
		}, true
	case msg.Audio != nil:
		return MediaFile{
			FileID:       msg.Audio.FileID,
			FileUniqueID: msg.Audio.FileUniqueID,
			FileName:     fallbackFileName(msg.Audio.FileName, msg.Audio.MimeType, "audio", msg.MessageID),
			MimeType:     firstNonEmpty(msg.Audio.MimeType, "audio/mpeg"),
			FileSize:     msg.Audio.FileSize,
		}, true
	case msg.Voice != nil:
		return MediaFile{
			FileID:       msg.Voice.FileID,
			FileUniqueID: msg.Voice.FileUniqueID,
			FileName:     fmt.Sprintf("voice_%d.%s", msg.MessageID, files.GuessExtension(msg.Voice.MimeType, "ogg")),
			MimeType:     firstNonEmpty(msg.Voice.MimeType, "audio/ogg"),
			FileSize:     msg.Voice.FileSize,
		}, true
	case msg.Animation != nil:
		return MediaFile{
			FileID:       msg.Animation.FileID,
			FileUniqueID: msg.Animation.FileUniqueID,
			FileName:     fallbackFileName(msg.Animation.FileName, msg.Animation.MimeType, "animation", msg.MessageID),
			MimeType:     firstNonEmpty(msg.Animation.MimeType, "video/mp4"),
			FileSize:     msg.Animation.FileSize,
		}, true
	case msg.VideoNote != nil:
		return MediaFile{
			FileID:       msg.VideoNote.FileID,
			FileUniqueID: msg.VideoNote.FileUniqueID,
			FileName:     fmt.Sprintf("video_note_%d.mp4", msg.MessageID),
			MimeType:     "video/mp4",
			FileSize:     msg.VideoNote.FileSize,
		}, true
	case len(msg.Photo) > 0:
		best := msg.Photo[len(msg.Photo)-1]
		return MediaFile{
			FileID:       best.FileID,
			FileUniqueID: best.FileUniqueID,
			FileName:     fmt.Sprintf("photo_%d.jpg", msg.MessageID),
			MimeType:     "image/jpeg",
			FileSize:     best.FileSize,
		}, true
	default:
		return MediaFile{}, false
	}
}

func parseCommand(text string) (string, string) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return "", ""
	}
	command := strings.TrimPrefix(fields[0], "/")
	if idx := strings.IndexByte(command, '@'); idx >= 0 {
		command = command[:idx]
	}

	args := ""
	if len(fields) > 1 {
		args = strings.Join(fields[1:], " ")
	}
	return strings.ToLower(command), args
}

func fallbackFileName(candidate, mimeType, prefix string, messageID int64) string {
	if strings.TrimSpace(candidate) != "" {
		return candidate
	}
	ext := files.GuessExtension(mimeType, "bin")
	return fmt.Sprintf("%s_%d.%s", prefix, messageID, ext)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func startText(baseURL string, owner bool) string {
	return fmt.Sprintf(
		"tg-file-url is ready.\n\nSend a file to me in private chat, or reply to a media message with /link.\nI store the file in BIN_CHANNEL and return stream/download links.\n\nCommands: %s\nHTTP service: %s",
		commandSummary(owner),
		baseURL,
	)
}

func helpText(baseURL string, owner bool) string {
	text := fmt.Sprintf(
		"tg-file-url\n\nHow to use:\n1. Send a file to the bot in private chat.\n2. Or reply to a supported media message with /link.\n3. The bot stores the file in BIN_CHANNEL.\n4. You get both a stream URL and a direct download URL.\n\nUser commands:\n/start - quick introduction\n/help - this help\n/about - project details\n/ping - measure bot latency\n/link - generate links for the replied file\n\nHTTP service: %s",
		baseURL,
	)
	if owner {
		text += "\n\nOwner commands:\n/status - bot summary\n/stats - runtime and host metrics\n/users - database totals\n/authorize - add authorized user\n/deauthorize - remove authorized user\n/listauth - list authorized users\n/ban - ban user or chat\n/unban - remove user or chat ban\n/broadcast - copy a replied message to users\n/log - send current log file\n/restart - restart the service"
	}
	return text
}

func aboutText(baseURL string, me *User) string {
	return fmt.Sprintf(
		"tg-file-url\n\nBot: %s\nVersion: %s\nSource: %s\n\nThis build forwards uploads into BIN_CHANNEL, stores metadata in SQLite, and streams downloads directly from Telegram over MTProto instead of relying on temporary Bot API file redirects.\n\nHTTP service: %s",
		displayBotName(me),
		app.Version,
		app.Repository,
		baseURL,
	)
}

func commandSummary(owner bool) string {
	commands := []string{"/help", "/about", "/ping", "/link"}
	if owner {
		commands = append(commands, "/status", "/stats", "/users", "/authorize", "/deauthorize", "/listauth", "/ban", "/unban", "/broadcast", "/log", "/restart")
	}
	return strings.Join(commands, " ")
}

func (b *Bot) botName() string {
	return displayBotName(b.me)
}

func displayBotName(me *User) string {
	if me == nil {
		return app.Name
	}
	if username := strings.TrimSpace(me.Username); username != "" {
		return "@" + username
	}
	return me.DisplayName()
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalSeconds := int64(d.Round(time.Second) / time.Second)
	days := totalSeconds / 86400
	totalSeconds %= 86400
	hours := totalSeconds / 3600
	totalSeconds %= 3600
	minutes := totalSeconds / 60
	seconds := totalSeconds % 60

	parts := make([]string, 0, 4)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 || len(parts) > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 || len(parts) > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	parts = append(parts, fmt.Sprintf("%ds", seconds))
	return strings.Join(parts, " ")
}

func formatOptionalDuration(d time.Duration) string {
	if d <= 0 {
		return "n/a"
	}
	return humanDuration(d)
}

func humanOptionalSize(size uint64) string {
	if size == 0 {
		return "n/a"
	}
	return files.HumanSize(int64(size))
}

func readHostUptime() (time.Duration, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}

	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("invalid /proc/uptime contents")
	}

	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func readMemInfo() (uint64, uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}

	var totalKB uint64
	var availableKB uint64
	var freeKB uint64

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}

		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			totalKB = value
		case "MemAvailable":
			availableKB = value
		case "MemFree":
			freeKB = value
		}
	}

	if totalKB == 0 {
		return 0, 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
	}
	if availableKB == 0 {
		availableKB = freeKB
	}

	return totalKB * 1024, availableKB * 1024, nil
}

func readDiskUsage(path string) (uint64, uint64, uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, 0, err
	}

	blockSize := uint64(stat.Bsize)
	total := stat.Blocks * blockSize
	free := stat.Bavail * blockSize
	used := total - free
	return total, used, free, nil
}

func diskUsagePath(sqlitePath string) string {
	if strings.TrimSpace(sqlitePath) == "" || strings.HasPrefix(sqlitePath, "file:") {
		return "."
	}

	dir := filepath.Dir(sqlitePath)
	if dir == "" {
		return "."
	}
	return dir
}
