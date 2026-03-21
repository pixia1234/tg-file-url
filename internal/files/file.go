package files

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"mime"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const SecureHashLength = 6
const LinkTokenLength = 12

type Record struct {
	MessageID           int64
	SecureHash          string
	LinkToken           string
	StorageChatID       int64
	SourceChatID        int64
	SourceMessageID     int64
	FileID              string
	FileUniqueID        string
	FileName            string
	MimeType            string
	FileSize            int64
	UploaderUserID      int64
	UploaderDisplayName string
	CreatedAt           time.Time
}

type Links struct {
	DownloadURL string
	StreamURL   string
}

func ComputeSecureHash(uniqueID string) string {
	cleaned := make([]rune, 0, len(uniqueID))
	for _, r := range uniqueID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			cleaned = append(cleaned, r)
		}
	}
	if len(cleaned) >= SecureHashLength {
		return string(cleaned[:SecureHashLength])
	}

	sum := sha1.Sum([]byte(uniqueID))
	return hex.EncodeToString(sum[:])[:SecureHashLength]
}

func ComputeLinkToken(uniqueID string, messageID int64) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%s:%d", uniqueID, messageID)))
	return hex.EncodeToString(sum[:])[:LinkTokenLength]
}

func BuildLinks(baseURL, linkToken, fileName string) Links {
	baseURL = strings.TrimRight(baseURL, "/")

	return Links{
		DownloadURL: fmt.Sprintf("%s/%s", baseURL, linkToken),
		StreamURL:   fmt.Sprintf("%s/watch/%s", baseURL, linkToken),
	}
}

func WithDisposition(rawURL, disposition string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	query := parsed.Query()
	query.Set("disposition", disposition)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func SanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "file.bin"
	}

	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', '\n', '\r', '\t':
			return '_'
		default:
			return r
		}
	}, name)

	if name == "." || name == ".." || name == "" {
		return "file.bin"
	}

	return name
}

func GuessExtension(mimeType, fallback string) string {
	extensions, err := mime.ExtensionsByType(mimeType)
	if err == nil && len(extensions) > 0 {
		return strings.TrimPrefix(extensions[0], ".")
	}
	return strings.TrimPrefix(fallback, ".")
}

func HumanSize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(size)
	for _, unit := range units {
		value /= 1024
		if value < 1024 {
			return fmt.Sprintf("%.2f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.2f PB", value/1024)
}

func PreviewKind(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	default:
		return "other"
	}
}
