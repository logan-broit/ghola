// Command pi-integration-server exposes real Ghola handlers over owned test storage.
// It never uses deployment configuration, model services, or the operator's corpus.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/logan-broit/ghola/internal/chapterhouse"
	"github.com/logan-broit/ghola/internal/core"
	gholahttp "github.com/logan-broit/ghola/internal/http"
	"github.com/logan-broit/ghola/internal/sietch"
	"github.com/logan-broit/ghola/pkg/embedding"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	root := flag.String("root", "", "owned pi-ghola-* temporary directory with a .pi-ghola-test marker")
	flag.Parse()
	if !filepath.IsAbs(*root) || !strings.HasPrefix(filepath.Base(*root), "pi-ghola-") {
		return errors.New("fixture requires an owned absolute pi-ghola-* test root")
	}
	info, err := os.Lstat(*root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("fixture root must be a private, non-symlink directory")
	}
	marker, err := os.ReadFile(filepath.Join(*root, ".pi-ghola-test"))
	if err != nil || string(marker) != "owned test root\n" {
		return errors.New("fixture ownership marker missing")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(logger)
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fixture: Chapterhouse unavailable", http.StatusServiceUnavailable)
	}))
	defer archive.Close()
	store, err := sietch.Open(filepath.Join(*root, "sessions"))
	if err != nil {
		return err
	}
	defer store.Close()
	c := core.New(store, chapterhouse.New(archive.URL, ""), embedding.New(embedding.Config{}))
	c.Settle = "off"
	c.TierTimeout = 250 * time.Millisecond
	c.SessionIdleTimeout = 0
	c.SietchRetention = 0
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	server := &http.Server{Handler: gholahttp.NewServer(c, logger).Handler(), ReadHeaderTimeout: time.Second}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"address": "http://" + listener.Addr().String()}); err != nil {
		_ = server.Close()
		return err
	}
	select {
	case <-ctx.Done():
		deadline, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if err := server.Shutdown(deadline); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
