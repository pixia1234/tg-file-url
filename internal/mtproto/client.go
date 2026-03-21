package mtproto

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/gotd/td/constant"
	"github.com/gotd/td/fileid"
	"github.com/gotd/td/session"
	gotdtelegram "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.uber.org/zap"

	"github.com/pixia1234/tg-file-url/internal/config"
	"github.com/pixia1234/tg-file-url/internal/files"
)

const chunkSize = 1024 * 1024
const preciseUnit = 1024

type Client struct {
	cfg    *config.Config
	client *gotdtelegram.Client
	ready  chan struct{}

	binChannelMu sync.RWMutex
	binChannel   *tg.InputChannel
}

type StreamResult struct {
	RefreshedFileID string
}

func New(cfg *config.Config) *Client {
	c := &Client{
		cfg:   cfg,
		ready: make(chan struct{}),
	}
	c.client = gotdtelegram.NewClient(cfg.APIID, cfg.APIHash, gotdtelegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.MTProtoSessionPath},
		Logger:         zap.NewNop(),
		UpdateHandler:  gotdtelegram.UpdateHandlerFunc(c.handleUpdate),
	})
	return c
}

func (c *Client) Run(ctx context.Context) error {
	return c.client.Run(ctx, func(ctx context.Context) error {
		status, err := c.client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("mtproto auth status: %w", err)
		}
		if !status.Authorized {
			if _, err := c.client.Auth().Bot(ctx, c.cfg.BotToken); err != nil {
				return fmt.Errorf("mtproto bot auth: %w", err)
			}
		}

		close(c.ready)

		<-ctx.Done()
		return ctx.Err()
	})
}

func (c *Client) Stream(ctx context.Context, record files.Record, offset, limit int64, dst io.Writer) (StreamResult, error) {
	var result StreamResult

	if err := c.waitReady(ctx); err != nil {
		return result, err
	}

	location, refreshedFileID, err := c.locationFromRecord(ctx, record)
	if err != nil {
		return result, err
	}
	result.RefreshedFileID = refreshedFileID

	current := offset
	remaining := limit

	for remaining > 0 {
		reqOffset, reqLimit, skip, err := buildPreciseRequestWindow(current, remaining)
		if err != nil {
			return result, err
		}

		chunk, err := c.getChunk(ctx, location, reqOffset, reqLimit)
		if err != nil {
			if !isFileReferenceError(err) {
				return result, err
			}

			location, refreshedFileID, err = c.refreshLocationFromMessage(ctx, record.MessageID)
			if err != nil {
				return result, err
			}
			result.RefreshedFileID = refreshedFileID
			continue
		}
		if len(chunk) == 0 {
			return result, io.ErrUnexpectedEOF
		}
		if skip >= len(chunk) {
			return result, io.ErrUnexpectedEOF
		}

		toWrite := chunk[skip:]
		if int64(len(toWrite)) > remaining {
			toWrite = toWrite[:remaining]
		}

		n, err := dst.Write(toWrite)
		if err != nil {
			return result, err
		}
		if n != len(toWrite) {
			return result, io.ErrShortWrite
		}

		current += int64(n)
		remaining -= int64(n)
	}

	return result, nil
}

func buildPreciseRequestWindow(offset, remaining int64) (int64, int, int, error) {
	if offset < 0 {
		return 0, 0, 0, fmt.Errorf("invalid negative offset %d", offset)
	}
	if remaining <= 0 {
		return 0, 0, 0, fmt.Errorf("invalid non-positive remaining length %d", remaining)
	}

	reqOffset := offset - (offset % preciseUnit)
	skip := int(offset - reqOffset)

	chunkOffset := reqOffset % chunkSize
	maxWithinChunk := int64(chunkSize) - chunkOffset

	desired := int64(skip) + remaining
	reqLimit := roundUpToUnit(desired, preciseUnit)
	if reqLimit < preciseUnit {
		reqLimit = preciseUnit
	}
	if reqLimit > maxWithinChunk {
		reqLimit = maxWithinChunk
	}
	if reqLimit <= 0 {
		return 0, 0, 0, fmt.Errorf("unable to build request window for offset=%d remaining=%d", offset, remaining)
	}

	return reqOffset, int(reqLimit), skip, nil
}

func roundUpToUnit(value, unit int64) int64 {
	if value <= 0 {
		return unit
	}
	remainder := value % unit
	if remainder == 0 {
		return value
	}
	return value + unit - remainder
}

func (c *Client) waitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ready:
		return nil
	}
}

func (c *Client) locationFromRecord(ctx context.Context, record files.Record) (tg.InputFileLocationClass, string, error) {
	location, err := decodeLocation(record.FileID)
	if err == nil {
		return location, "", nil
	}

	location, refreshedFileID, refreshErr := c.refreshLocationFromMessage(ctx, record.MessageID)
	if refreshErr != nil {
		return nil, "", fmt.Errorf("decode file_id: %w; refresh from message: %v", err, refreshErr)
	}
	return location, refreshedFileID, nil
}

func (c *Client) refreshLocationFromMessage(ctx context.Context, messageID int64) (tg.InputFileLocationClass, string, error) {
	channel, err := c.resolveBinChannel(ctx)
	if err != nil {
		return nil, "", err
	}

	response, err := c.client.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: channel,
		ID: []tg.InputMessageClass{
			&tg.InputMessageID{ID: int(messageID)},
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("channels.getMessages: %w", err)
	}

	modified, ok := response.AsModified()
	if !ok {
		return nil, "", fmt.Errorf("unexpected channels.getMessages result %T", response)
	}

	for _, msgClass := range modified.GetMessages() {
		msg, ok := msgClass.(*tg.Message)
		if !ok || int64(msg.ID) != messageID {
			continue
		}
		media, ok := msg.GetMedia()
		if !ok {
			break
		}
		return locationFromMedia(media)
	}

	return nil, "", fmt.Errorf("message %d not found in BIN_CHANNEL", messageID)
}

func (c *Client) resolveBinChannel(ctx context.Context) (*tg.InputChannel, error) {
	c.binChannelMu.RLock()
	if c.binChannel != nil {
		channel := *c.binChannel
		c.binChannelMu.RUnlock()
		return &channel, nil
	}
	c.binChannelMu.RUnlock()

	return nil, fmt.Errorf("BIN_CHANNEL access hash is not cached yet")
}

func decodeLocation(encoded string) (tg.InputFileLocationClass, error) {
	decoded, err := fileid.DecodeFileID(encoded)
	if err != nil {
		return nil, err
	}

	location, ok := decoded.AsInputFileLocation()
	if !ok {
		return nil, fmt.Errorf("unsupported Bot API file_id type %v", decoded.Type)
	}
	return location, nil
}

func locationFromMedia(media tg.MessageMediaClass) (tg.InputFileLocationClass, string, error) {
	switch media := media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := media.Document.AsNotEmpty()
		if !ok {
			return nil, "", errors.New("message document is empty")
		}

		decoded := fileid.FromDocument(doc)
		location, ok := decoded.AsInputFileLocation()
		if !ok {
			return nil, "", errors.New("document location is not streamable")
		}

		encoded, err := fileid.EncodeFileID(decoded)
		if err != nil {
			return nil, "", err
		}
		return location, encoded, nil
	case *tg.MessageMediaPhoto:
		photo, ok := media.Photo.AsNotEmpty()
		if !ok {
			return nil, "", errors.New("message photo is empty")
		}

		thumbType, err := pickPhotoThumbType(photo.Sizes)
		if err != nil {
			return nil, "", err
		}

		decoded := fileid.FromPhoto(photo, thumbType)
		location, ok := decoded.AsInputFileLocation()
		if !ok {
			return nil, "", errors.New("photo location is not streamable")
		}

		encoded, err := fileid.EncodeFileID(decoded)
		if err != nil {
			return nil, "", err
		}
		return location, encoded, nil
	default:
		return nil, "", fmt.Errorf("unsupported media type %T", media)
	}
}

func pickPhotoThumbType(sizes []tg.PhotoSizeClass) (rune, error) {
	for i := len(sizes) - 1; i >= 0; i-- {
		size, ok := sizes[i].AsNotEmpty()
		if !ok {
			continue
		}

		thumbType := size.GetType()
		if thumbType == "" {
			continue
		}
		return []rune(thumbType)[0], nil
	}

	return 0, errors.New("photo has no downloadable sizes")
}

func (c *Client) getChunk(ctx context.Context, location tg.InputFileLocationClass, offset int64, limit int) ([]byte, error) {
	for {
		request := &tg.UploadGetFileRequest{
			Location: location,
			Offset:   offset,
			Limit:    limit,
		}
		request.SetPrecise(true)

		response, err := c.client.API().UploadGetFile(ctx, request)
		if flood, err := tgerr.FloodWait(ctx, err); err != nil {
			if flood || tgerr.Is(err, tg.ErrTimeout) {
				continue
			}
			return nil, err
		}

		switch response := response.(type) {
		case *tg.UploadFile:
			return response.Bytes, nil
		case *tg.UploadFileCDNRedirect:
			return nil, errors.New("unexpected CDN redirect during direct download")
		default:
			return nil, fmt.Errorf("unexpected upload.getFile result %T", response)
		}
	}
}

func isFileReferenceError(err error) bool {
	return tg.IsFileReferenceExpired(err) || tg.IsFileReferenceInvalid(err) || tg.IsFileReferenceEmpty(err)
}

func (c *Client) handleUpdate(ctx context.Context, u tg.UpdatesClass) error {
	targetChannelID := constant.TDLibPeerID(c.cfg.BinChannel).ToPlain()
	if targetChannelID == 0 {
		return nil
	}

	switch updates := u.(type) {
	case *tg.Updates:
		c.captureBinChannel(updates.GetChats(), targetChannelID)
	case *tg.UpdatesCombined:
		c.captureBinChannel(updates.GetChats(), targetChannelID)
	}

	return nil
}

func (c *Client) captureBinChannel(chats []tg.ChatClass, targetChannelID int64) {
	for _, chat := range chats {
		switch chat := chat.(type) {
		case *tg.Channel:
			if chat.ID != targetChannelID {
				continue
			}
			c.storeBinChannel(&tg.InputChannel{
				ChannelID:  chat.ID,
				AccessHash: chat.AccessHash,
			})
			return
		case *tg.ChannelForbidden:
			if chat.ID != targetChannelID {
				continue
			}
			c.storeBinChannel(&tg.InputChannel{
				ChannelID:  chat.ID,
				AccessHash: chat.AccessHash,
			})
			return
		}
	}
}

func (c *Client) storeBinChannel(channel *tg.InputChannel) {
	if channel == nil {
		return
	}

	c.binChannelMu.Lock()
	defer c.binChannelMu.Unlock()

	if c.binChannel != nil && c.binChannel.ChannelID == channel.ChannelID && c.binChannel.AccessHash == channel.AccessHash {
		return
	}
	c.binChannel = &tg.InputChannel{
		ChannelID:  channel.ChannelID,
		AccessHash: channel.AccessHash,
	}
	log.Printf("mtproto cached BIN_CHANNEL access hash for channel %d", channel.ChannelID)
}
