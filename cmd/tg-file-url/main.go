package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/pixia1234/tg-file-url/internal/app"
	"github.com/pixia1234/tg-file-url/internal/config"
	"github.com/pixia1234/tg-file-url/internal/database"
	"github.com/pixia1234/tg-file-url/internal/httpserver"
	"github.com/pixia1234/tg-file-url/internal/mtproto"
	"github.com/pixia1234/tg-file-url/internal/telegram"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if err := cfg.EnsureDataDir(); err != nil {
		log.Fatalf("prepare data directories: %v", err)
	}

	logFile, err := setupLogging(cfg.LogPath)
	if err != nil {
		log.Printf("log file setup failed: %v", err)
	}
	if logFile != nil {
		defer logFile.Close()
	}

	store, err := database.Open(cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer store.Close()

	ctx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tgClient := telegram.NewClient(cfg.BotToken, cfg.TelegramAPIBaseURL, cfg.HTTPTimeout)
	bot := telegram.NewBot(cfg, store, tgClient)
	mtClient := mtproto.New(cfg)

	handler, err := httpserver.New(cfg, store, tgClient, mtClient)
	if err != nil {
		log.Fatalf("build http handler: %v", err)
	}

	httpServer := &http.Server{
		Addr:              net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Port)),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("%s %s starting", app.Name, app.Version)
	log.Printf("public base url: %s", cfg.PublicBaseURL)
	log.Printf("telegram api base url: %s", cfg.TelegramAPIBaseURL)

	errCh := make(chan error, 3)

	go func() {
		log.Printf("http server listening on %s", httpServer.Addr)
		err := httpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	go func() {
		if err := bot.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
		}
	}()

	go func() {
		if err := mtClient.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
		}
	}()

	var runErr error

	select {
	case <-ctx.Done():
	case err := <-errCh:
		runErr = err
		cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("http shutdown error: %v", err)
	}

	if runErr != nil {
		log.Fatalf("runtime error: %v", runErr)
	}
}

func setupLogging(path string) (*os.File, error) {
	if path == "" {
		return nil, nil
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	log.SetOutput(io.MultiWriter(os.Stdout, file))
	return file, nil
}
