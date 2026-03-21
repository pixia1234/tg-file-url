package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	APIID              int
	APIHash            string
	BotToken           string
	BinChannel         int64
	BindAddress        string
	Port               int
	PublicBaseURL      string
	SQLitePath         string
	LogPath            string
	MTProtoSessionPath string
	TelegramAPIBaseURL string
	PollTimeout        time.Duration
	HTTPTimeout        time.Duration
	Owners             map[int64]struct{}
}

func Load() (*Config, error) {
	_ = godotenv.Load("config.env", ".env")

	botToken := strings.TrimSpace(os.Getenv("BOT_TOKEN"))
	if botToken == "" {
		return nil, errors.New("BOT_TOKEN is required")
	}

	apiID, err := parseIntEnv("API_ID", 0)
	if err != nil {
		return nil, err
	}
	if apiID == 0 {
		return nil, errors.New("API_ID is required")
	}

	apiHash := strings.TrimSpace(os.Getenv("API_HASH"))
	if apiHash == "" {
		return nil, errors.New("API_HASH is required")
	}

	binChannel, err := parseInt64Env("BIN_CHANNEL", true)
	if err != nil {
		return nil, err
	}

	port, err := parseIntEnv("PORT", 8080)
	if err != nil {
		return nil, err
	}

	baseURL, err := buildPublicBaseURL(port)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		APIID:              apiID,
		APIHash:            apiHash,
		BotToken:           botToken,
		BinChannel:         binChannel,
		BindAddress:        getEnv("BIND_ADDRESS", "0.0.0.0"),
		Port:               port,
		PublicBaseURL:      baseURL,
		SQLitePath:         resolveSQLitePath(),
		LogPath:            getEnv("LOG_PATH", filepath.Join("data", "tg-file-url.log")),
		MTProtoSessionPath: getEnv("MTPROTO_SESSION_PATH", filepath.Join("data", "mtproto-bot.session")),
		TelegramAPIBaseURL: strings.TrimRight(getEnv("TELEGRAM_API_BASE_URL", "https://api.telegram.org"), "/"),
		PollTimeout:        time.Duration(getIntEnv("POLL_TIMEOUT_SECONDS", 30)) * time.Second,
		HTTPTimeout:        time.Duration(getIntEnv("HTTP_TIMEOUT_SECONDS", 60)) * time.Second,
		Owners:             parseOwnerIDs(),
	}

	return cfg, nil
}

func (c *Config) SQLiteDSN() string {
	if strings.HasPrefix(c.SQLitePath, "file:") {
		if strings.Contains(c.SQLitePath, "_busy_timeout=") {
			return c.SQLitePath
		}
		separator := "?"
		if strings.Contains(c.SQLitePath, "?") {
			separator = "&"
		}
		return c.SQLitePath + separator + "_busy_timeout=5000&_foreign_keys=on"
	}

	return fmt.Sprintf("file:%s?_busy_timeout=5000&_foreign_keys=on", c.SQLitePath)
}

func (c *Config) EnsureDataDir() error {
	for _, path := range []string{c.SQLitePath, c.LogPath, c.MTProtoSessionPath} {
		if strings.HasPrefix(path, "file:") {
			continue
		}

		dir := filepath.Dir(path)
		if dir == "." || dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) IsOwner(userID int64) bool {
	_, ok := c.Owners[userID]
	return ok
}

func buildPublicBaseURL(port int) (string, error) {
	if publicBaseURL := strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")); publicBaseURL != "" {
		parsed, err := url.Parse(publicBaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return "", fmt.Errorf("PUBLIC_BASE_URL is invalid: %q", publicBaseURL)
		}
		return strings.TrimRight(publicBaseURL, "/"), nil
	}

	fqdn := strings.TrimSpace(os.Getenv("FQDN"))
	if fqdn == "" {
		return "", errors.New("set PUBLIC_BASE_URL or FQDN")
	}

	hasSSL := getBoolEnv("HAS_SSL", false)
	noPort := getBoolEnv("NO_PORT", false)
	scheme := "http"
	if hasSSL {
		scheme = "https"
	}

	host := fqdn
	if !noPort && !hasExplicitPort(host) {
		host = net.JoinHostPort(host, strconv.Itoa(port))
	}

	return fmt.Sprintf("%s://%s", scheme, host), nil
}

func resolveSQLitePath() string {
	if sqlitePath := strings.TrimSpace(os.Getenv("SQLITE_PATH")); sqlitePath != "" {
		return sqlitePath
	}

	if databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL")); databaseURL != "" {
		switch {
		case strings.HasPrefix(databaseURL, "sqlite:///"):
			return strings.TrimPrefix(databaseURL, "sqlite:///")
		case strings.HasPrefix(databaseURL, "sqlite://"):
			return strings.TrimPrefix(databaseURL, "sqlite://")
		default:
			return databaseURL
		}
	}

	return filepath.Join("data", "tg-file-url.db")
}

func parseOwnerIDs() map[int64]struct{} {
	owners := make(map[int64]struct{})
	for _, key := range []string{"OWNER_IDS", "OWNER_ID"} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		for _, item := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ' ' || r == ';'
		}) {
			if item == "" {
				continue
			}
			value, err := strconv.ParseInt(item, 10, 64)
			if err == nil {
				owners[value] = struct{}{}
			}
		}
	}
	return owners
}

func parseIntEnv(key string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return parsed, nil
}

func getIntEnv(key string, fallback int) int {
	value, err := parseIntEnv(key, fallback)
	if err != nil {
		return fallback
	}
	return value
}

func parseInt64Env(key string, required bool) (int64, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		if required {
			return 0, fmt.Errorf("%s is required", key)
		}
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return parsed, nil
}

func getEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func getBoolEnv(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback
	}

	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func hasExplicitPort(host string) bool {
	if strings.Count(host, ":") == 0 {
		return false
	}

	if strings.HasPrefix(host, "[") {
		_, _, err := net.SplitHostPort(host)
		return err == nil
	}

	_, _, err := net.SplitHostPort(host)
	return err == nil
}
