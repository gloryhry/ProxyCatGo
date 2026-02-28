package control

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"proxycatgo/internal/config"
	"proxycatgo/internal/dataplane"
	"proxycatgo/internal/i18n"
	"proxycatgo/internal/logging"
)

const (
	currentVersion     = "ProxyCat-V2.0.4"
	versionCheckURL    = "https://y.shironekosan.cn/1.html"
	proxyCheckCacheTTL = 10 * time.Second
	proxyDialTimeout   = 10 * time.Second
)

var proxyCheckCache sync.Map

type proxyCheckResult struct {
	at time.Time
	ok bool
}

type Server struct {
	configPath string
	cfgMu      sync.RWMutex
	cfg        *config.RuntimeConfig
	service    *dataplane.Service
	logs       *logging.RingBuffer
	logFile    string
}

func NewServer(cfg *config.RuntimeConfig, service *dataplane.Service, logs *logging.RingBuffer, logFile string) *Server {
	return &Server{
		configPath: cfg.Path,
		cfg:        cfg,
		service:    service,
		logs:       logs,
		logFile:    logFile,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/", s.root())
	mux.Handle("/web", s.withToken(http.HandlerFunc(s.web)))
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	mux.Handle("/api/status", s.withToken(http.HandlerFunc(s.status)))
	mux.Handle("/api/config", http.HandlerFunc(s.configHandler))
	mux.Handle("/api/proxies", http.HandlerFunc(s.proxiesHandler))
	mux.Handle("/api/check_proxies", http.HandlerFunc(s.checkProxies))
	mux.Handle("/api/proxies/refresh_valid", s.withToken(http.HandlerFunc(s.refreshValidProxies)))
	mux.Handle("/api/ip_lists", http.HandlerFunc(s.ipListsHandler))
	mux.Handle("/api/logs", http.HandlerFunc(s.logsHandler))
	mux.Handle("/api/logs/clear", http.HandlerFunc(s.clearLogs))
	mux.Handle("/api/switch_proxy", s.withToken(http.HandlerFunc(s.switchProxy)))
	mux.Handle("/api/service", s.withToken(http.HandlerFunc(s.serviceHandler)))
	mux.Handle("/api/language", http.HandlerFunc(s.languageHandler))
	mux.Handle("/api/version", http.HandlerFunc(s.versionHandler))
	mux.Handle("/api/users", s.withToken(http.HandlerFunc(s.usersHandler)))

	return mux
}

func (s *Server) withToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.cfgMu.RLock()
		tokenCfg := s.cfg.Server["token"]
		lang := fallback(s.cfg.Server["language"], "cn")
		s.cfgMu.RUnlock()

		if strings.TrimSpace(tokenCfg) == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Query().Get("token") != tokenCfg {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"status":  "error",
				"message": i18n.Get("invalid_token", lang),
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) root() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token != "" {
			http.Redirect(w, r, "/web?token="+token, http.StatusFound)
			return
		}
		http.Redirect(w, r, "/web", http.StatusFound)
	})
}

func (s *Server) web(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/templates/index.html")
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	cfg := cloneMap(s.cfg.Server)
	s.cfgMu.RUnlock()

	snap := s.service.GetSnapshot()
	currentProxy := snap.CurrentProxy
	if currentProxy == "" && !snap.UseGetIP {
		currentProxy = i18n.Get("no_proxy", snap.Language)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"current_proxy":  currentProxy,
		"mode":           snap.Mode,
		"port":           toInt(cfg["port"], 1080),
		"interval":       snap.Interval,
		"time_left":      snap.TimeLeft,
		"total_proxies":  snap.TotalProxies,
		"use_getip":      snap.UseGetIP,
		"getip_url":      cfg["getip_url"],
		"auth_required":  snap.AuthRequired,
		"display_level":  toInt(cfg["display_level"], 1),
		"service_status": map[bool]string{true: "running", false: "stopped"}[snap.Running],
		"config":         cfg,
		"health_check":   s.service.GetHealthSnapshot(),
		"m5": map[string]any{
			"active_connections":         s.service.ActiveConnectionCount(),
			"max_concurrent_connections": s.service.MaxConcurrentConnections(),
			"failure_entries":            s.service.FailureEntryCount(),
			"failure_threshold":          s.service.FailureThreshold(),
			"failure_cooldown_seconds":   s.service.FailureCooldownSeconds(),
			"failure_retention_seconds":  s.service.FailureRetentionSeconds(),
			"cleanup_interval_seconds":   s.service.CleanupIntervalSeconds(),
			"connection_timeout_seconds": s.service.ConnectionTimeoutSeconds(),
		},
	})
}

func (s *Server) configHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "error", "message": "method not allowed"})
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"status": "error", "message": err.Error()})
		return
	}

	updates := map[string]string{}
	for k, v := range body {
		if v == nil {
			updates[k] = ""
			continue
		}
		updates[k] = strings.TrimSpace(toString(v))
	}

	s.cfgMu.RLock()
	oldPort := s.cfg.Server["port"]
	s.cfgMu.RUnlock()

	newCfg, err := config.SaveServer(s.configPath, updates)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error()})
		return
	}

	s.cfgMu.Lock()
	s.cfg = newCfg
	s.cfgMu.Unlock()
	s.service.ApplyConfig(newCfg)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "success",
		"port_changed":   oldPort != newCfg.Server["port"],
		"service_status": map[bool]string{true: "running", false: "stopped"}[s.service.GetSnapshot().Running],
	})
}

func (s *Server) proxiesHandler(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	proxyFile := fallback(s.cfg.Server["proxy_file"], "ip.txt")
	lang := fallback(s.cfg.Server["language"], "cn")
	s.cfgMu.RUnlock()

	path := filepath.Join("config", filepath.Base(proxyFile))
	if r.Method == http.MethodGet {
		lines, _ := readLines(path)
		writeJSON(w, http.StatusOK, map[string]any{"proxies": lines})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "error", "message": "method not allowed"})
		return
	}

	var body struct {
		Proxies []string `json:"proxies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": i18n.Get("proxy_save_failed", lang, err.Error())})
		return
	}
	if err := writeLines(path, body.Proxies); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": i18n.Get("proxy_save_failed", lang, err.Error())})
		return
	}
	s.service.SetProxies(body.Proxies)
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": i18n.Get("proxy_save_success", lang)})
}

func (s *Server) checkProxies(w http.ResponseWriter, r *http.Request) {
	testURL := strings.TrimSpace(r.URL.Query().Get("test_url"))
	if testURL == "" {
		testURL = "https://www.baidu.com"
	}

	s.cfgMu.RLock()
	lang := fallback(s.cfg.Server["language"], "cn")
	proxyFile := fallback(s.cfg.Server["proxy_file"], "ip.txt")
	s.cfgMu.RUnlock()

	path := filepath.Join("config", filepath.Base(proxyFile))
	proxies, err := readLines(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "error",
			"message": i18n.Get("proxy_check_failed", lang, err.Error()),
		})
		return
	}

	valid := make([]string, 0, len(proxies))
	for _, p := range proxies {
		ok := checkProxyCompat(p, testURL)
		if ok {
			valid = append(valid, p)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "success",
		"valid_proxies": valid,
		"total":         len(valid),
		"message":       i18n.Get("proxy_check_result", lang, strconv.Itoa(len(valid))),
	})
}

func (s *Server) refreshValidProxies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "error", "message": "method not allowed"})
		return
	}

	s.cfgMu.RLock()
	lang := fallback(s.cfg.Server["language"], "cn")
	defaultTestURL := strings.TrimSpace(s.cfg.Server["test_url"])
	s.cfgMu.RUnlock()
	if defaultTestURL == "" {
		defaultTestURL = "https://www.baidu.com"
	}

	health := s.service.GetHealthSnapshot()
	applyDefault := health.AutoApply
	persistDefault := health.AutoPersist

	var body struct {
		Apply       *bool   `json:"apply"`
		Persist     *bool   `json:"persist"`
		ForceSwitch *bool   `json:"force_switch"`
		TestURL     *string `json:"test_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"status": "error", "message": err.Error()})
		return
	}

	opts := dataplane.RefreshValidOptions{
		Apply:       applyDefault,
		Persist:     persistDefault,
		ForceSwitch: false,
		TestURL:     defaultTestURL,
	}
	if body.Apply != nil {
		opts.Apply = *body.Apply
	}
	if body.Persist != nil {
		opts.Persist = *body.Persist
	}
	if body.ForceSwitch != nil {
		opts.ForceSwitch = *body.ForceSwitch
	}
	if body.TestURL != nil && strings.TrimSpace(*body.TestURL) != "" {
		opts.TestURL = strings.TrimSpace(*body.TestURL)
	}

	result, err := s.service.RefreshValidProxies(opts)
	payload := map[string]any{
		"triggered_at":  result.TriggeredAt.Unix(),
		"duration_ms":   result.DurationMS,
		"before_total":  result.BeforeTotal,
		"valid_total":   result.ValidTotal,
		"applied":       result.Applied,
		"persisted":     result.Persisted,
		"skipped":       result.Skipped,
		"skip_reason":   result.SkipReason,
		"current_proxy": result.CurrentProxy,
		"last_error":    result.LastError,
		"valid_proxies": result.ValidProxies,
	}

	if result.Skipped {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"message": i18n.Get("proxy_refresh_skipped", lang),
			"result":  payload,
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "error",
			"message": i18n.Get("proxy_refresh_failed", lang, err.Error()),
			"result":  payload,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "success",
		"message": i18n.Get("proxy_refresh_success", lang),
		"result":  payload,
	})
}

func (s *Server) ipListsHandler(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	whitelist := filepath.Join("config", filepath.Base(fallback(s.cfg.Server["whitelist_file"], "whitelist.txt")))
	blacklist := filepath.Join("config", filepath.Base(fallback(s.cfg.Server["blacklist_file"], "blacklist.txt")))
	lang := fallback(s.cfg.Server["language"], "cn")
	s.cfgMu.RUnlock()

	if r.Method == http.MethodGet {
		wlist, _ := readLines(whitelist)
		blist, _ := readLines(blacklist)
		writeJSON(w, http.StatusOK, map[string]any{"whitelist": wlist, "blacklist": blist})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "error", "message": "method not allowed"})
		return
	}

	var body struct {
		Type string   `json:"type"`
		List []string `json:"list"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": i18n.Get("ip_list_save_failed", lang, err.Error())})
		return
	}
	file := blacklist
	if body.Type == "whitelist" {
		file = whitelist
	}
	if err := writeLines(file, body.List); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": i18n.Get("ip_list_save_failed", lang, err.Error())})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": i18n.Get("ip_list_save_success", lang)})
}

func (s *Server) logsHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start := 0
	limit := 100
	var err error
	if raw := strings.TrimSpace(q.Get("start")); raw != "" {
		start, err = strconv.Atoi(raw)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error()})
			return
		}
	}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error()})
			return
		}
	}
	level := q.Get("level")
	search := strings.ToLower(q.Get("search"))
	logs, total := s.logs.Query(level, search, start, limit)
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs, "total": total, "status": "success"})
}

func (s *Server) clearLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "error", "message": "method not allowed"})
		return
	}
	s.cfgMu.RLock()
	lang := fallback(s.cfg.Server["language"], "cn")
	s.cfgMu.RUnlock()

	s.logs.Clear()
	_ = os.WriteFile(s.logFile, []byte{}, 0o644)
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": i18n.Get("logs_cleared", lang)})
}

func (s *Server) switchProxy(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	lang := fallback(s.cfg.Server["language"], "cn")
	s.cfgMu.RUnlock()

	if s.service.SwitchProxy() {
		snap := s.service.GetSnapshot()
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "current_proxy": snap.CurrentProxy, "message": i18n.Get("switch_success", lang)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": i18n.Get("operation_failed", lang, "Proxy switch not available")})
}

func (s *Server) serviceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "error", "message": "method not allowed"})
		return
	}

	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error()})
		return
	}

	s.cfgMu.RLock()
	lang := fallback(s.cfg.Server["language"], "cn")
	s.cfgMu.RUnlock()

	switch body.Action {
	case "start":
		started, err := s.service.Start()
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": i18n.Get("operation_failed", lang, err.Error()), "service_status": "stopped"})
			return
		}
		if started {
			writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": i18n.Get("service_start_success", lang), "service_status": "running"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": i18n.Get("service_already_running", lang), "service_status": "running"})
	case "stop":
		stopped, err := s.service.Stop()
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": i18n.Get("operation_failed", lang, err.Error())})
			return
		}
		if stopped {
			writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": i18n.Get("service_stop_success", lang), "service_status": "stopped"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": i18n.Get("service_not_running", lang), "service_status": "stopped"})
	case "restart":
		if err := s.service.Restart(); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": i18n.Get("operation_failed", lang, err.Error()), "service_status": "stopped"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": i18n.Get("service_restart_success", lang), "service_status": "running"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": i18n.Get("invalid_action", lang)})
	}
}

func (s *Server) languageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "error", "message": "method not allowed"})
		return
	}
	var body struct {
		Language string `json:"language"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error()})
		return
	}
	if body.Language != "cn" && body.Language != "en" {
		s.cfgMu.RLock()
		lang := fallback(s.cfg.Server["language"], "cn")
		s.cfgMu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": i18n.Get("unsupported_language", lang)})
		return
	}

	cfg, err := config.SaveServer(s.configPath, map[string]string{"language": body.Language})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error()})
		return
	}
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
	s.service.ApplyConfig(cfg)
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "language": body.Language})
}

func (s *Server) versionHandler(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	lang := fallback(s.cfg.Server["language"], "cn")
	s.cfgMu.RUnlock()

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(versionCheckURL)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "error",
			"message": i18n.Get("update_check_error", lang, err.Error()),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "error",
			"message": i18n.Get("update_check_error", lang, fmt.Sprintf("status %d", resp.StatusCode)),
		})
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "error",
			"message": i18n.Get("update_check_error", lang, err.Error()),
		})
		return
	}

	re := regexp.MustCompile(`<p>(ProxyCat-V\d+\.\d+\.\d+)</p>`)
	m := re.FindStringSubmatch(string(body))
	if len(m) < 2 {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "error",
			"message": i18n.Get("version_info_not_found", lang),
		})
		return
	}

	latest := m[1]
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "success",
		"is_latest":       !isVersionGreater(latest, currentVersion),
		"current_version": currentVersion,
		"latest_version":  latest,
	})
}

func isVersionGreater(a, b string) bool {
	av := strings.TrimPrefix(a, "ProxyCat-V")
	bv := strings.TrimPrefix(b, "ProxyCat-V")
	ap := strings.Split(av, ".")
	bp := strings.Split(bv, ".")
	for len(ap) < 3 {
		ap = append(ap, "0")
	}
	for len(bp) < 3 {
		bp = append(bp, "0")
	}
	for i := 0; i < 3; i++ {
		ai, _ := strconv.Atoi(ap[i])
		bi, _ := strconv.Atoi(bp[i])
		if ai > bi {
			return true
		}
		if ai < bi {
			return false
		}
	}
	return false
}
func (s *Server) usersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.cfgMu.RLock()
		users := cloneMap(s.cfg.Users)
		s.cfgMu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"users": users})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "error", "message": "method not allowed"})
		return
	}

	var body struct {
		Users map[string]string `json:"users"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error()})
		return
	}

	s.cfgMu.RLock()
	lang := fallback(s.cfg.Server["language"], "cn")
	s.cfgMu.RUnlock()

	cfg, err := config.SaveUsers(s.configPath, body.Users)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": i18n.Get("users_save_failed", lang, err.Error())})
		return
	}
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
	s.service.ApplyConfig(cfg)
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": i18n.Get("users_save_success", lang)})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func cloneMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

func toInt(v string, d int) int {
	if strings.TrimSpace(v) == "" {
		return d
	}
	n := 0
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return d
		}
		n = n*10 + int(v[i]-'0')
	}
	if n == 0 {
		return d
	}
	return n
}

func fallback(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return []string{}, err
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result, nil
}

func writeLines(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := strings.Join(lines, "\n")
	return os.WriteFile(path, []byte(content), 0o644)
}

func checkProxyCompat(proxyAddr, testURL string) bool {
	proxyAddr = strings.TrimSpace(proxyAddr)
	if proxyAddr == "" {
		return false
	}
	cacheKey := proxyAddr + "::" + testURL
	if v, ok := proxyCheckCache.Load(cacheKey); ok {
		entry := v.(proxyCheckResult)
		if time.Since(entry.at) < proxyCheckCacheTTL {
			return entry.ok
		}
	}

	u, err := url.Parse(proxyAddr)
	if err != nil {
		proxyCheckCache.Store(cacheKey, proxyCheckResult{at: time.Now(), ok: false})
		return false
	}

	scheme := strings.ToLower(u.Scheme)
	ok := false
	switch scheme {
	case "http", "https":
		ok = checkHTTPProxyCompat(proxyAddr, testURL)
	case "socks5":
		ok = checkSOCKS5ProxyCompat(proxyAddr, testURL)
	default:
		ok = false
	}
	proxyCheckCache.Store(cacheKey, proxyCheckResult{at: time.Now(), ok: ok})
	return ok
}

func checkHTTPProxyCompat(proxyAddr, testURL string) bool {
	proxyURL, err := url.Parse(proxyAddr)
	if err != nil {
		return false
	}
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr, Timeout: proxyDialTimeout}
	resp, err := client.Get(testURL)
	if err != nil {
		if strings.HasPrefix(testURL, "https://") {
			resp, err = client.Get("http://" + strings.TrimPrefix(testURL, "https://"))
		}
	}
	if err != nil || resp == nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func checkSOCKS5ProxyCompat(proxyAddr, testURL string) bool {
	u, err := url.Parse(proxyAddr)
	if err != nil {
		return false
	}
	hostPort := u.Host
	if !strings.Contains(hostPort, ":") {
		hostPort = net.JoinHostPort(hostPort, "1080")
	}
	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pass}
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	socksDialer, err := proxy.SOCKS5("tcp", hostPort, auth, d)
	if err != nil {
		return false
	}

	targetHost := extractHostForSOCKS(testURL)
	conn, err := socksDialer.Dial("tcp", net.JoinHostPort(targetHost, "80"))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func extractHostForSOCKS(testURL string) string {
	if strings.Contains(testURL, "://") {
		u, err := url.Parse(testURL)
		if err == nil && u.Host != "" {
			h := u.Host
			if strings.Contains(h, ":") {
				h, _, _ = net.SplitHostPort(h)
			}
			if h != "" {
				return h
			}
		}
	}
	h := testURL
	if strings.Contains(h, "/") {
		h = strings.SplitN(h, "/", 2)[0]
	}
	if strings.Contains(h, ":") {
		h, _, _ = net.SplitHostPort(h)
	}
	if h == "" {
		return "www.baidu.com"
	}
	return h
}
