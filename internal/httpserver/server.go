package httpserver

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pixia1234/tg-file-url/internal/app"
	"github.com/pixia1234/tg-file-url/internal/config"
	"github.com/pixia1234/tg-file-url/internal/database"
	"github.com/pixia1234/tg-file-url/internal/files"
	"github.com/pixia1234/tg-file-url/internal/mtproto"
	"github.com/pixia1234/tg-file-url/internal/telegram"
)

//go:embed templates/watch.html
var templateFS embed.FS

var (
	linkTokenPattern = regexp.MustCompile(`^([a-f0-9]{12})(?:/.*)?$`)
)

type Server struct {
	cfg       *config.Config
	store     *database.Store
	tg        *telegram.Client
	mt        *mtproto.Client
	startedAt time.Time
	watchTmpl *template.Template
}

type watchPageData struct {
	FileName    string
	FileSize    string
	MimeType    string
	PreviewKind string
	StreamURL   string
	DownloadURL string
	UploadedBy  string
	CreatedAt   string
}

func New(cfg *config.Config, store *database.Store, tg *telegram.Client, mt *mtproto.Client) (http.Handler, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/watch.html")
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:       cfg,
		store:     store,
		tg:        tg,
		mt:        mt,
		startedAt: time.Now().UTC(),
		watchTmpl: tmpl,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", s.handleRoot)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/watch/{path...}", s.handleWatch)
	mux.HandleFunc("/{path...}", s.handleDownload)

	return loggingMiddleware(mux), nil
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodHead, http.MethodOptions) {
		return
	}
	writeCORS(w)
	w.Header().Set("Content-Type", "application/json")
	payload := map[string]any{
		"name":          app.Name,
		"version":       app.Version,
		"status_url":    s.cfg.PublicBaseURL + "/status",
		"stream_hint":   s.cfg.PublicBaseURL + "/watch/{token}",
		"download_hint": s.cfg.PublicBaseURL + "/{token}",
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodHead, http.MethodOptions) {
		return
	}
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		http.Error(w, "failed to load stats", http.StatusInternalServerError)
		return
	}

	writeCORS(w)
	w.Header().Set("Content-Type", "application/json")
	payload := map[string]any{
		"app": map[string]any{
			"name":       app.Name,
			"version":    app.Version,
			"started_at": s.startedAt.Format(time.RFC3339),
			"uptime":     time.Since(s.startedAt).String(),
		},
		"storage": map[string]any{
			"sqlite_path": s.cfg.SQLitePath,
			"users":       stats.Users,
			"files":       stats.Files,
		},
		"telegram": map[string]any{
			"api_base_url": s.cfg.TelegramAPIBaseURL,
			"bin_channel":  s.cfg.BinChannel,
		},
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodHead, http.MethodOptions) {
		return
	}
	record, err := s.resolveRecord(r)
	if err != nil {
		s.writeResolveError(w, err)
		return
	}

	links := files.BuildLinks(s.cfg.PublicBaseURL, recordLinkToken(record), record.FileName)
	data := watchPageData{
		FileName:    record.FileName,
		FileSize:    files.HumanSize(record.FileSize),
		MimeType:    record.MimeType,
		PreviewKind: files.PreviewKind(record.MimeType),
		StreamURL:   files.WithDisposition(links.DownloadURL, "inline"),
		DownloadURL: files.WithDisposition(links.DownloadURL, "attachment"),
		UploadedBy:  record.UploaderDisplayName,
		CreatedAt:   record.CreatedAt.Format(time.RFC3339),
	}

	writeCORS(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.watchTmpl.Execute(w, data); err != nil {
		log.Printf("watch template error: %v", err)
	}
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodHead, http.MethodOptions) {
		return
	}
	record, err := s.resolveRecord(r)
	if err != nil {
		s.writeResolveError(w, err)
		return
	}

	start, end, partial, err := parseRangeHeader(r.Header.Get("Range"), record.FileSize)
	if err != nil {
		switch {
		case errors.Is(err, errRangeNotSatisfiable):
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", record.FileSize))
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		default:
			http.Error(w, "invalid range header", http.StatusBadRequest)
		}
		return
	}

	contentType := record.MimeType
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}

	writeMediaHeaders(w, record, contentType, r.URL.Query().Get("disposition"))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "public, max-age=31536000")

	statusCode := http.StatusOK
	contentLength := int64(0)
	if end >= start {
		contentLength = end - start + 1
	}
	if partial {
		statusCode = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, record.FileSize))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))

	if r.Method == http.MethodHead {
		w.WriteHeader(statusCode)
		return
	}

	writer := &lazyStreamWriter{w: w, statusCode: statusCode}
	outcome, err := s.mt.Stream(r.Context(), record, start, contentLength, writer)
	if outcome.RefreshedFileID != "" && outcome.RefreshedFileID != record.FileID {
		s.persistRefreshedFileID(record, outcome.RefreshedFileID)
	}
	if err != nil {
		log.Printf("mtproto stream error for message %d: %v", record.MessageID, err)
		if !writer.wroteHeader {
			http.Error(w, "download stream failed", http.StatusBadGateway)
		}
		return
	}

	writer.WriteHeaderIfNeeded()
}

func (s *Server) persistRefreshedFileID(record files.Record, fileID string) {
	record.FileID = fileID

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.store.SaveFile(ctx, record); err != nil {
		log.Printf("persist refreshed file_id for message %d: %v", record.MessageID, err)
	}
}

func (s *Server) resolveRecord(r *http.Request) (files.Record, error) {
	pathValue := r.PathValue("path")
	if pathValue == "" {
		return files.Record{}, database.ErrNotFound
	}

	linkToken, err := parseMediaPath(pathValue)
	if err != nil {
		return files.Record{}, err
	}

	return s.store.GetFileByLinkToken(r.Context(), linkToken)
}

func (s *Server) writeResolveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, database.ErrNotFound):
		http.Error(w, "resource not found", http.StatusNotFound)
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}

func parseMediaPath(rawPath string) (string, error) {
	rawPath = strings.Trim(strings.TrimSpace(rawPath), "/")
	if rawPath == "" {
		return "", errInvalidPath
	}

	if matches := linkTokenPattern.FindStringSubmatch(rawPath); len(matches) == 2 {
		return matches[1], nil
	}

	return "", errInvalidPath
}

func recordLinkToken(record files.Record) string {
	if strings.TrimSpace(record.LinkToken) != "" {
		return record.LinkToken
	}
	return files.ComputeLinkToken(record.FileUniqueID, record.MessageID)
}

func writeCORS(w http.ResponseWriter) {
	headers := w.Header()
	headers.Set("Access-Control-Allow-Origin", "*")
	headers.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	headers.Set("Access-Control-Allow-Headers", "Range, Content-Type, Authorization")
	headers.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Content-Disposition")
}

func writeMediaHeaders(w http.ResponseWriter, record files.Record, contentType, disposition string) {
	writeCORS(w)
	if disposition != "inline" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename*=UTF-8''%s", disposition, url.PathEscape(record.FileName)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func allowMethods(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, method := range allowed {
		if r.Method == method {
			if r.Method == http.MethodOptions {
				writeCORS(w)
				w.WriteHeader(http.StatusNoContent)
				return false
			}
			return true
		}
	}

	w.Header().Set("Allow", strings.Join(allowed, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(started))
	})
}

var (
	errInvalidPath         = errors.New("invalid media path")
	errInvalidRange        = errors.New("invalid range")
	errRangeNotSatisfiable = errors.New("range not satisfiable")
)

type lazyStreamWriter struct {
	w           http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (w *lazyStreamWriter) Write(data []byte) (int, error) {
	w.WriteHeaderIfNeeded()
	return w.w.Write(data)
}

func (w *lazyStreamWriter) WriteHeaderIfNeeded() {
	if w.wroteHeader {
		return
	}
	w.w.WriteHeader(w.statusCode)
	w.wroteHeader = true
}

func parseRangeHeader(header string, fileSize int64) (start, end int64, partial bool, err error) {
	if fileSize < 0 {
		return 0, 0, false, errRangeNotSatisfiable
	}
	if fileSize == 0 {
		if strings.TrimSpace(header) != "" {
			return 0, 0, false, errRangeNotSatisfiable
		}
		return 0, -1, false, nil
	}
	if strings.TrimSpace(header) == "" {
		return 0, fileSize - 1, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false, errInvalidRange
	}

	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, false, errInvalidRange
	}

	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, errInvalidRange
	}

	switch {
	case parts[0] == "":
		suffixLength, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffixLength <= 0 {
			return 0, 0, false, errInvalidRange
		}
		if suffixLength > fileSize {
			suffixLength = fileSize
		}
		return fileSize - suffixLength, fileSize - 1, true, nil
	default:
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 {
			return 0, 0, false, errInvalidRange
		}
		if start >= fileSize {
			return 0, 0, false, errRangeNotSatisfiable
		}

		if parts[1] == "" {
			return start, fileSize - 1, true, nil
		}

		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false, errInvalidRange
		}
		if end >= fileSize {
			end = fileSize - 1
		}
		return start, end, true, nil
	}
}
