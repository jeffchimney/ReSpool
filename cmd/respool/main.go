// Command respool serves a folder of audiobooks and other audio files as
// podcast feeds.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jeffchimney/respool/internal/library"
	"github.com/jeffchimney/respool/internal/server"
	"github.com/jeffchimney/respool/internal/uploads"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

type config struct {
	MediaDir     string
	Addr         string
	BaseURL      string
	Token        string
	ScanInterval time.Duration
	Language     string
	LogLevel     string
	ShowVersion  bool
	UploadDir    string
	UploadLimit  int64 // bytes
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	cfg, err := loadConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "respool:", err)
		os.Exit(2)
	}
	if cfg.ShowVersion {
		fmt.Println("respool", version)
		return
	}
	if err := run(cfg); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

var tokenRe = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

func loadConfig(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("respool", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: respool [flags]\n       respool healthcheck\n\nEvery flag can also be set with the environment variable shown.\n\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&c.MediaDir, "media", env("RESPOOL_MEDIA_DIR", "/media"), "folder of audio files to serve (RESPOOL_MEDIA_DIR)")
	fs.StringVar(&c.Addr, "addr", env("RESPOOL_ADDR", ":8080"), "address to listen on (RESPOOL_ADDR)")
	fs.StringVar(&c.BaseURL, "base-url", env("RESPOOL_BASE_URL", ""), "public URL feeds link to, e.g. https://respool.example.ts.net; defaults to the address each client used (RESPOOL_BASE_URL)")
	fs.StringVar(&c.Token, "token", env("RESPOOL_TOKEN", ""), "secret that must prefix every URL, e.g. http://host:8080/<token>/ (RESPOOL_TOKEN)")
	interval := env("RESPOOL_SCAN_INTERVAL", "5m")
	d, err := time.ParseDuration(interval)
	if err != nil {
		return c, fmt.Errorf("RESPOOL_SCAN_INTERVAL %q: %w", interval, err)
	}
	fs.DurationVar(&c.ScanInterval, "scan-interval", d, "how often to rescan the media folder; 0 disables periodic scans (RESPOOL_SCAN_INTERVAL)")
	fs.StringVar(&c.Language, "language", env("RESPOOL_LANGUAGE", "en"), "language code for feeds (RESPOOL_LANGUAGE)")
	fs.StringVar(&c.LogLevel, "log-level", env("RESPOOL_LOG_LEVEL", "info"), "debug, info, warn or error (RESPOOL_LOG_LEVEL)")
	fs.StringVar(&c.UploadDir, "uploads", env("RESPOOL_UPLOAD_DIR", ""), "writable folder for audio added from the web page; empty disables uploads (RESPOOL_UPLOAD_DIR)")
	limitMB := env("RESPOOL_UPLOAD_LIMIT_MB", strconv.Itoa(server.DefaultUploadLimit>>20))
	mb, err := strconv.ParseInt(limitMB, 10, 64)
	if err != nil || mb <= 0 {
		return c, fmt.Errorf("RESPOOL_UPLOAD_LIMIT_MB %q must be a positive number of megabytes", limitMB)
	}
	fs.Int64Var(&mb, "upload-limit-mb", mb, "largest upload accepted, in MiB (RESPOOL_UPLOAD_LIMIT_MB)")
	fs.BoolVar(&c.ShowVersion, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if mb <= 0 {
		return c, errors.New("upload limit must be positive")
	}
	c.UploadLimit = mb << 20
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.BaseURL != "" && !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return c, fmt.Errorf("base URL %q must start with http:// or https://", c.BaseURL)
	}
	if c.Token != "" && !tokenRe.MatchString(c.Token) {
		return c, errors.New("token may only contain letters, digits and . _ ~ -")
	}
	return c, nil
}

func run(cfg config) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("log level %q: %w", cfg.LogLevel, err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if st, err := os.Stat(cfg.MediaDir); err != nil || !st.IsDir() {
		return fmt.Errorf("media folder %q is not a readable directory (mount your audiobooks there or set RESPOOL_MEDIA_DIR)", cfg.MediaDir)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	roots := []library.Root{{Dir: cfg.MediaDir}}
	var store *uploads.Store
	if cfg.UploadDir != "" {
		var err error
		if store, err = uploads.New(cfg.UploadDir, logger); err != nil {
			// Serving the library matters more than uploads; carry on without them.
			logger.Error("uploads are turned off", "err", err)
		} else {
			logger.Info("uploads enabled", "dir", store.Dir(), "limit_mb", cfg.UploadLimit>>20)
			if !within(store.Dir(), cfg.MediaDir) {
				roots = append(roots, library.Root{Dir: store.Dir(), Prefix: "@uploads/"})
			}
		}
	}
	lib := library.New(logger, roots...)
	go lib.Run(ctx, cfg.ScanInterval)

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: server.New(server.Options{
			Library:     lib,
			BaseURL:     cfg.BaseURL,
			Token:       cfg.Token,
			Language:    cfg.Language,
			Version:     version,
			Logger:      logger,
			Uploads:     store,
			UploadLimit: cfg.UploadLimit,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: downloading a long audiobook can take a while.
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	indexPath := "/"
	if cfg.Token != "" {
		indexPath = "/" + cfg.Token + "/"
	} else {
		logger.Warn("RESPOOL_TOKEN is not set: anyone who can reach this server can list and download your library")
	}
	logger.Info("respool started", "version", version, "addr", cfg.Addr, "media", cfg.MediaDir, "index", indexPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// within reports whether dir is parent or inside it, in which case scanning
// parent already covers dir.
func within(dir, parent string) bool {
	parent, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(parent, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// healthcheck lets the distroless image check itself without curl.
func healthcheck() int {
	_, port, err := net.SplitHostPort(env("RESPOOL_ADDR", ":8080"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
}
