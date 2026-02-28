package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"proxycatgo/internal/config"
	"proxycatgo/internal/control"
	"proxycatgo/internal/dataplane"
	"proxycatgo/internal/logging"
)

type Bootstrap struct {
	cfgPath    string
	cfg        *config.RuntimeConfig
	service    *dataplane.Service
	control    *control.Server
	httpServer *http.Server
	logs       *logging.RingBuffer
	slogLogger *slog.Logger
}

func NewBootstrap(cfgPath string) (*Bootstrap, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}

	if err := os.Chdir(projectRootFromConfig(cfg.Path)); err != nil {
		return nil, fmt.Errorf("switch cwd: %w", err)
	}

	buffer := logging.NewRingBuffer(10000)
	logger, err := logging.Setup("logs/proxycat.log", buffer)
	if err != nil {
		return nil, err
	}

	service := dataplane.NewService(cfg)
	service.SetProxies(loadInitialProxies(cfg.Server["proxy_file"]))
	if started, err := service.Start(); err != nil {
		logger.Error("initial dataplane start failed", "error", err)
	} else if started {
		logger.Info("initial dataplane started")
	}

	ctrl := control.NewServer(cfg, service, buffer, "logs/proxycat.log")
	webPort := cfg.Server["web_port"]
	if webPort == "" {
		webPort = "5000"
	}
	httpServer := &http.Server{
		Addr:              ":" + webPort,
		Handler:           ctrl.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	return &Bootstrap{
		cfgPath:    cfgPath,
		cfg:        cfg,
		service:    service,
		control:    ctrl,
		httpServer: httpServer,
		logs:       buffer,
		slogLogger: logger,
	}, nil
}

func (b *Bootstrap) Run(ctx context.Context) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = b.httpServer.Shutdown(shutdownCtx)
	}()

	b.slogLogger.Info("ProxyCatGo control plane listening", "addr", b.httpServer.Addr)
	err := b.httpServer.ListenAndServe()
	_, _ = b.service.Stop()
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func loadInitialProxies(proxyFile string) []string {
	if proxyFile == "" {
		proxyFile = "ip.txt"
	}
	path := filepath.Join("config", filepath.Base(proxyFile))
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := []string{}
	current := ""
	for _, ch := range string(b) {
		if ch == '\n' || ch == '\r' {
			if current != "" {
				lines = append(lines, current)
				current = ""
			}
			continue
		}
		current += string(ch)
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func projectRootFromConfig(cfgPath string) string {
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return "."
	}
	return filepath.Dir(filepath.Dir(abs))
}
