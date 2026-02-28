package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/ini.v1"
)

const (
	DefaultConfigPath = "config/config.ini"
)

var defaultServer = map[string]string{
	"display_level":    "1",
	"port":             "1080",
	"web_port":         "5000",
	"mode":             "cycle",
	"interval":         "300",
	"use_getip":        "false",
	"getip_url":        "http://example.com/getip",
	"proxy_username":   "",
	"proxy_password":   "",
	"proxy_file":       "ip.txt",
	"check_proxies":             "true",
	"test_url":                  "",
	"health_check_enabled":      "true",
	"health_check_interval":     "300",
	"health_check_timeout":      "8",
	"health_check_concurrency":  "50",
	"health_check_auto_apply":   "false",
	"health_check_auto_persist": "false",
	"health_check_min_pool_size": "1",
	"language":                  "cn",
	"whitelist_file":   "whitelist.txt",
	"blacklist_file":   "blacklist.txt",
	"ip_auth_priority": "whitelist",
	"token":            "",
	"username":         "",
	"password":         "",
}

type RuntimeConfig struct {
	Path   string
	Server map[string]string
	Users  map[string]string
}

func Load(path string) (*RuntimeConfig, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigPath
	}

	if err := ensureParent(path); err != nil {
		return nil, err
	}

	cfg, err := ini.LoadSources(ini.LoadOptions{IgnoreInlineComment: true}, path)
	if err != nil {
		return nil, fmt.Errorf("load ini: %w", err)
	}

	if !cfg.Section("Server").HasKey("port") {
		applyServerDefaults(cfg)
		if err := cfg.SaveTo(path); err != nil {
			return nil, fmt.Errorf("save default config: %w", err)
		}
	}

	server := map[string]string{}
	for _, key := range cfg.Section("Server").Keys() {
		server[key.Name()] = key.String()
	}
	for k, v := range defaultServer {
		if _, ok := server[k]; !ok {
			server[k] = v
		}
	}

	users := map[string]string{}
	if cfg.HasSection("Users") {
		for _, key := range cfg.Section("Users").Keys() {
			users[key.Name()] = key.String()
		}
	}

	return &RuntimeConfig{
		Path:   path,
		Server: server,
		Users:  users,
	}, nil
}

func SaveServer(path string, updates map[string]string) (*RuntimeConfig, error) {
	cfg, err := ini.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load ini: %w", err)
	}

	sec := cfg.Section("Server")
	for k, v := range updates {
		if k == "users" {
			continue
		}
		sec.Key(k).SetValue(v)
	}

	if err := cfg.SaveTo(path); err != nil {
		return nil, fmt.Errorf("save ini: %w", err)
	}

	return Load(path)
}

func SaveUsers(path string, users map[string]string) (*RuntimeConfig, error) {
	cfg, err := ini.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load ini: %w", err)
	}

	cfg.DeleteSection("Users")
	if len(users) > 0 {
		sec, err := cfg.NewSection("Users")
		if err != nil {
			return nil, fmt.Errorf("new users section: %w", err)
		}
		for u, p := range users {
			sec.Key(u).SetValue(p)
		}
	}

	if err := cfg.SaveTo(path); err != nil {
		return nil, fmt.Errorf("save ini: %w", err)
	}

	return Load(path)
}

func applyServerDefaults(cfg *ini.File) {
	sec := cfg.Section("Server")
	for k, v := range defaultServer {
		if !sec.HasKey(k) {
			sec.Key(k).SetValue(v)
		}
	}
}

func ensureParent(path string) error {
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
