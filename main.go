package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/divizn/godis/client"
)

type config struct {
	addr        string
	godis       []string
	baseURL     string
	adminToken  string
	sbKey       string
	sbEndpoint  string
	createLimit int
	trustProxy  bool
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	var cfg config
	var addrs string
	flag.StringVar(&cfg.addr, "addr", ":8090", "address to listen on")
	flag.StringVar(&addrs, "godis", "localhost:8081,localhost:8082,localhost:8083", "godis client addresses, comma separated")
	flag.StringVar(&cfg.baseURL, "base-url", "http://localhost:8090", "public base url, used in short and management urls")
	flag.StringVar(&cfg.adminToken, "admin-token", "", "admin password, or SHORTENER_ADMIN_TOKEN; admin pages are off when empty")
	flag.StringVar(&cfg.sbKey, "safe-browsing-key", "", "google safe browsing api key, or SAFE_BROWSING_KEY; the check is off when empty")
	flag.IntVar(&cfg.createLimit, "create-limit", 20, "links per ip per hour, also the report limit")
	flag.BoolVar(&cfg.trustProxy, "trust-proxy", false, "take the client ip from the first X-Forwarded-For hop")
	flag.Parse()
	cfg.godis = strings.Split(addrs, ",")
	if cfg.adminToken == "" {
		cfg.adminToken = os.Getenv("SHORTENER_ADMIN_TOKEN")
	}
	if cfg.sbKey == "" {
		cfg.sbKey = os.Getenv("SAFE_BROWSING_KEY")
	}
	if cfg.sbKey == "" {
		log.Warn("no safe browsing key, links are not checked")
	}
	if cfg.adminToken == "" {
		log.Warn("no admin token, admin pages are off")
	}

	kv, err := client.Dial(cfg.godis...)
	if err != nil {
		return fmt.Errorf("dial godis: %w", err)
	}
	defer kv.Close()
	a, err := newApp(cfg, kv, log)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.addr, "base", cfg.baseURL)
	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
