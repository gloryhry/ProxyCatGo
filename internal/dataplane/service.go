package dataplane

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
	"proxycatgo/internal/config"
)

type Service struct {
	mu               sync.RWMutex
	running          bool
	mode             string
	interval         int
	useGetIP         bool
	getipURL         string
	getipProxyScheme string
	proxyUsername    string
	proxyPassword    string
	currentProxy     string
	proxies          []string
	proxyIndex       int
	proxyFile        string
	language         string
	authRequired     bool
	users            map[string]string
	lastSwitchTime   time.Time
	port             int

	switchingProxy      bool
	lastSwitchAttempt   time.Time
	switchCooldown      time.Duration
	consecutiveFailures map[string]int
	failureLastSeen     map[string]time.Time
	proxyFailureThresh  int
	lastProxyFailTime   time.Time
	proxyFailureCD      time.Duration
	failureRetention    time.Duration

	lastGetIPRefresh    time.Time
	getipRefreshMinimum time.Duration

	maxConcurrentConn int
	connIOTimeout     time.Duration
	cleanupInterval   time.Duration
	connSem           chan struct{}
	activeConns       map[net.Conn]time.Time

	healthCheckEnabled     bool
	healthCheckInterval    time.Duration
	healthCheckTimeout     time.Duration
	healthCheckConcurrency int
	healthCheckAutoApply   bool
	healthCheckAutoPersist bool
	healthCheckMinPoolSize int
	healthCheckTestURL     string
	healthCheckRunning     bool
	healthStatus           HealthCheckStatus

	listener net.Listener
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

type Snapshot struct {
	Running      bool
	Mode         string
	Interval     int
	UseGetIP     bool
	CurrentProxy string
	TotalProxies int
	Language     string
	AuthRequired bool
	TimeLeft     float64
}

type RefreshValidOptions struct {
	Apply       bool
	Persist     bool
	ForceSwitch bool
	TestURL     string
}

type RefreshValidResult struct {
	TriggeredAt  time.Time
	DurationMS   int64
	BeforeTotal  int
	ValidTotal   int
	Applied      bool
	Persisted    bool
	Skipped      bool
	SkipReason   string
	CurrentProxy string
	LastError    string
	ValidProxies []string
}

type HealthCheckStatus struct {
	LastCheckAt time.Time
	DurationMS  int64
	BeforeTotal int
	ValidTotal  int
	Applied     bool
	Persisted   bool
	Skipped     bool
	SkipReason  string
	LastError   string
}

type HealthCheckSnapshot struct {
	Enabled          bool   `json:"enabled"`
	IntervalSeconds  int    `json:"interval_seconds"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
	Concurrency      int    `json:"concurrency"`
	AutoApply        bool   `json:"auto_apply"`
	AutoPersist      bool   `json:"auto_persist"`
	MinPoolSize      int    `json:"min_pool_size"`
	Running          bool   `json:"running"`
	LastCheckAt      int64  `json:"last_check_at"`
	DurationMS       int64  `json:"duration_ms"`
	BeforeTotal      int    `json:"before_total"`
	ValidTotal       int    `json:"valid_total"`
	Applied          bool   `json:"applied"`
	Persisted        bool   `json:"persisted"`
	Skipped          bool   `json:"skipped"`
	SkipReason       string `json:"skip_reason"`
	LastError        string `json:"last_error"`
	LastCheckAgoSecs int64  `json:"last_check_ago_seconds"`
}

func healthStatusToSnapshot(status HealthCheckStatus, enabled bool, interval, timeout time.Duration, concurrency int, autoApply, autoPersist bool, minPool int, running bool) HealthCheckSnapshot {
	last := int64(0)
	ago := int64(-1)
	if !status.LastCheckAt.IsZero() {
		last = status.LastCheckAt.Unix()
		ago = int64(time.Since(status.LastCheckAt).Seconds())
	}
	return HealthCheckSnapshot{
		Enabled:          enabled,
		IntervalSeconds:  int(interval.Seconds()),
		TimeoutSeconds:   int(timeout.Seconds()),
		Concurrency:      concurrency,
		AutoApply:        autoApply,
		AutoPersist:      autoPersist,
		MinPoolSize:      minPool,
		Running:          running,
		LastCheckAt:      last,
		DurationMS:       status.DurationMS,
		BeforeTotal:      status.BeforeTotal,
		ValidTotal:       status.ValidTotal,
		Applied:          status.Applied,
		Persisted:        status.Persisted,
		Skipped:          status.Skipped,
		SkipReason:       status.SkipReason,
		LastError:        status.LastError,
		LastCheckAgoSecs: ago,
	}
}

func defaultRefreshValidOptions() RefreshValidOptions {
	return RefreshValidOptions{TestURL: "https://www.baidu.com"}
}

func normalizeRefreshValidOptions(opts RefreshValidOptions) RefreshValidOptions {
	base := defaultRefreshValidOptions()
	base.Apply = opts.Apply
	base.Persist = opts.Persist
	base.ForceSwitch = opts.ForceSwitch
	if strings.TrimSpace(opts.TestURL) != "" {
		base.TestURL = strings.TrimSpace(opts.TestURL)
	}
	return base
}

func parseSecondsWithMin(v string, d, min int) time.Duration {
	n := atoiDefault(v, d)
	if n < min {
		n = min
	}
	return time.Duration(n) * time.Second
}

func parseIntWithMin(v string, d, min int) int {
	n := atoiDefault(v, d)
	if n < min {
		return min
	}
	return n
}

func toBoolDefault(v string, d bool) bool {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return d
	}
	return toBool(trimmed)
}

func shouldPersistToFile(useGetIP bool, requested bool) bool {
	return requested && !useGetIP
}

func containsProxy(list []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

func sanitizeProxyFilePath(proxyFile string) string {
	name := filepath.Base(strings.TrimSpace(proxyFile))
	if name == "" || name == "." || name == "/" {
		name = "ip.txt"
	}
	return filepath.Join("config", name)
}

func persistProxyList(path string, proxies []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := strings.Join(proxies, "\n")
	if len(proxies) > 0 {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func (s *Service) updateHealthStatus(result RefreshValidResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthStatus = HealthCheckStatus{
		LastCheckAt: result.TriggeredAt,
		DurationMS:  result.DurationMS,
		BeforeTotal: result.BeforeTotal,
		ValidTotal:  result.ValidTotal,
		Applied:     result.Applied,
		Persisted:   result.Persisted,
		Skipped:     result.Skipped,
		SkipReason:  result.SkipReason,
		LastError:   result.LastError,
	}
}

func (s *Service) tryBeginHealthCheck() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.healthCheckRunning {
		return false
	}
	s.healthCheckRunning = true
	return true
}

func (s *Service) endHealthCheck() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthCheckRunning = false
}

func (s *Service) snapshotProxiesForHealth() (proxies []string, currentProxy string, proxyFile string, useGetIP bool, minPool int, timeout time.Duration, concurrency int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]string, len(s.proxies))
	copy(list, s.proxies)
	return list, s.currentProxy, s.proxyFile, s.useGetIP, s.healthCheckMinPoolSize, s.healthCheckTimeout, s.healthCheckConcurrency
}

func (s *Service) snapshotHealthConfig() (enabled bool, interval, timeout time.Duration, concurrency int, autoApply, autoPersist bool, minPool int, testURL string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.healthCheckEnabled,
		s.healthCheckInterval,
		s.healthCheckTimeout,
		s.healthCheckConcurrency,
		s.healthCheckAutoApply,
		s.healthCheckAutoPersist,
		s.healthCheckMinPoolSize,
		s.healthCheckTestURL
}

func checkProxyWithTimeout(proxyAddr, testURL string, timeout time.Duration) bool {
	proxyAddr = strings.TrimSpace(proxyAddr)
	if proxyAddr == "" {
		return false
	}
	u, err := url.Parse(proxyAddr)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return checkHTTPProxyWithTimeout(proxyAddr, testURL, timeout)
	case "socks5":
		return checkSOCKS5ProxyWithTimeout(proxyAddr, testURL, timeout)
	default:
		return false
	}
}

func checkHTTPProxyWithTimeout(proxyAddr, testURL string, timeout time.Duration) bool {
	proxyURL, err := url.Parse(proxyAddr)
	if err != nil {
		return false
	}
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr, Timeout: timeout}
	resp, err := client.Get(testURL)
	if err != nil && strings.HasPrefix(testURL, "https://") {
		resp, err = client.Get("http://" + strings.TrimPrefix(testURL, "https://"))
	}
	if err != nil || resp == nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func checkSOCKS5ProxyWithTimeout(proxyAddr, testURL string, timeout time.Duration) bool {
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
	d := &net.Dialer{Timeout: timeout}
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

func filterValidProxiesConcurrently(proxies []string, testURL string, timeout time.Duration, concurrency int) []string {
	if len(proxies) == 0 {
		return []string{}
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(proxies) {
		concurrency = len(proxies)
	}
	type checkResult struct {
		idx int
		ok  bool
	}
	jobs := make(chan int)
	results := make(chan checkResult, len(proxies))
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				results <- checkResult{idx: idx, ok: checkProxyWithTimeout(proxies[idx], testURL, timeout)}
			}
		}()
	}
	for i := range proxies {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(results)

	okFlags := make([]bool, len(proxies))
	for res := range results {
		if res.ok {
			okFlags[res.idx] = true
		}
	}
	valid := make([]string, 0, len(proxies))
	for i, p := range proxies {
		if okFlags[i] {
			valid = append(valid, p)
		}
	}
	return valid
}

func NewService(cfg *config.RuntimeConfig) *Service {
	s := &Service{
		lastSwitchTime:         time.Now(),
		users:                  map[string]string{},
		switchCooldown:         5 * time.Second,
		consecutiveFailures:    map[string]int{},
		failureLastSeen:        map[string]time.Time{},
		proxyFailureThresh:     3,
		proxyFailureCD:         3 * time.Second,
		failureRetention:       5 * time.Minute,
		getipRefreshMinimum:    2 * time.Second,
		maxConcurrentConn:      1000,
		connIOTimeout:          120 * time.Second,
		cleanupInterval:        30 * time.Second,
		activeConns:            map[net.Conn]time.Time{},
		healthCheckEnabled:     true,
		healthCheckInterval:    300 * time.Second,
		healthCheckTimeout:     8 * time.Second,
		healthCheckConcurrency: 50,
		healthCheckMinPoolSize: 1,
		healthCheckTestURL:     "https://www.baidu.com",
	}
	s.ApplyConfig(cfg)
	return s
}

func (s *Service) ApplyConfig(cfg *config.RuntimeConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mode = fallback(cfg.Server["mode"], "cycle")
	s.interval = atoiDefault(cfg.Server["interval"], 300)
	s.useGetIP = toBool(cfg.Server["use_getip"])
	s.getipURL = strings.TrimSpace(cfg.Server["getip_url"])
	s.getipProxyScheme = normalizeGetIPProxyScheme(cfg.Server["getip_proxy_scheme"])
	s.proxyUsername = strings.TrimSpace(cfg.Server["proxy_username"])
	s.proxyPassword = strings.TrimSpace(cfg.Server["proxy_password"])
	s.proxyFile = fallback(cfg.Server["proxy_file"], "ip.txt")
	s.language = fallback(cfg.Server["language"], "cn")
	s.port = atoiDefault(cfg.Server["port"], 1080)
	s.healthCheckEnabled = toBoolDefault(cfg.Server["health_check_enabled"], true)
	s.healthCheckInterval = parseSecondsWithMin(cfg.Server["health_check_interval"], 300, 1)
	s.healthCheckTimeout = parseSecondsWithMin(cfg.Server["health_check_timeout"], 8, 1)
	s.healthCheckConcurrency = parseIntWithMin(cfg.Server["health_check_concurrency"], 50, 1)
	s.healthCheckAutoApply = toBoolDefault(cfg.Server["health_check_auto_apply"], false)
	s.healthCheckAutoPersist = toBoolDefault(cfg.Server["health_check_auto_persist"], false)
	s.healthCheckMinPoolSize = parseIntWithMin(cfg.Server["health_check_min_pool_size"], 1, 1)
	s.healthCheckTestURL = strings.TrimSpace(cfg.Server["test_url"])
	if s.healthCheckTestURL == "" {
		s.healthCheckTestURL = "https://www.baidu.com"
	}

	s.users = cloneMap(cfg.Users)
	s.authRequired = len(s.users) > 0
	if len(s.proxies) == 0 {
		s.currentProxy = ""
	}
}

func (s *Service) SetProxies(proxies []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clean := make([]string, 0, len(proxies))
	seen := map[string]struct{}{}
	for _, p := range proxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		clean = append(clean, p)
	}
	s.proxies = clean
	s.proxyIndex = 0
	s.consecutiveFailures = map[string]int{}
	s.failureLastSeen = map[string]time.Time{}
	s.lastSwitchTime = time.Now()
	if len(s.proxies) == 0 {
		s.currentProxy = ""
		return
	}
	s.currentProxy = s.proxies[0]
}

func (s *Service) Start() (bool, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false, nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", s.port))
	if err != nil {
		slog.Error("proxy listener start failed", "error", err)
		s.mu.Unlock()
		return false, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.listener = ln
	s.cancel = cancel
	s.running = true
	s.connSem = make(chan struct{}, s.maxConcurrentConn)
	s.activeConns = map[net.Conn]time.Time{}
	s.mu.Unlock()

	s.wg.Add(3)
	go s.acceptLoop(ctx)
	go s.cleanupLoop(ctx)
	go s.runHealthCheckLoop(ctx)
	slog.Info("proxy dataplane started", "port", s.port)
	return true, nil
}

func (s *Service) Stop() (bool, error) {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return false, nil
	}
	cancel := s.cancel
	ln := s.listener
	active := make([]net.Conn, 0, len(s.activeConns))
	for c := range s.activeConns {
		active = append(active, c)
	}
	s.running = false
	s.cancel = nil
	s.listener = nil
	s.activeConns = map[net.Conn]time.Time{}
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range active {
		_ = c.Close()
	}
	s.wg.Wait()
	slog.Info("proxy dataplane stopped")
	return true, nil
}

func (s *Service) Restart() error {
	_, _ = s.Stop()
	_, err := s.Start()
	return err
}

func (s *Service) SwitchProxy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switchProxyLocked(false)
}

func (s *Service) GetSnapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		Running:      s.running,
		Mode:         s.mode,
		Interval:     s.interval,
		UseGetIP:     s.useGetIP,
		CurrentProxy: s.currentProxy,
		TotalProxies: len(s.proxies),
		Language:     s.language,
		AuthRequired: s.authRequired,
		TimeLeft:     s.timeUntilNextSwitchLocked(),
	}
}

func (s *Service) acceptLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		s.mu.RLock()
		ln := s.listener
		running := s.running
		s.mu.RUnlock()
		if !running || ln == nil {
			return
		}
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return
		}

		if !s.acquireConnSlot(ctx) {
			_ = conn.Close()
			continue
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer s.releaseConnSlot()
			s.trackConn(c)
			defer s.untrackConn(c)
			s.handleConn(c)
		}(conn)
	}
}

func (s *Service) handleConn(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(s.connIOTimeout))

	br := bufio.NewReader(client)
	first, err := br.ReadByte()
	if err != nil {
		return
	}

	if first == 0x05 {
		s.handleSOCKS5(client, br)
		return
	}
	s.handleHTTP(client, br, first)
}

func (s *Service) handleHTTP(client net.Conn, br *bufio.Reader, first byte) {
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader([]byte{first}), br))
	req, err := http.ReadRequest(reader)
	if err != nil {
		s.writeHTTPError(client, http.StatusBadRequest)
		return
	}
	defer req.Body.Close()

	if !s.checkHTTPAuth(req.Header.Get("Proxy-Authorization")) {
		s.writeProxyAuthRequired(client)
		return
	}

	targetHost := requestTarget(req)
	if targetHost == "" {
		s.writeHTTPError(client, http.StatusBadRequest)
		return
	}

	upConn, usedProxy, err := s.dialTarget(targetHost)
	if err != nil {
		s.onProxyFailure(usedProxy)
		slog.Warn("dial target failed", "target", targetHost, "error", err)
		s.writeHTTPError(client, http.StatusBadGateway)
		return
	}
	defer upConn.Close()
	s.onProxySuccess(usedProxy)

	if strings.EqualFold(req.Method, http.MethodConnect) {
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		s.tunnel(client, upConn)
		return
	}

	req.RequestURI = ""
	if req.URL != nil {
		req.URL.Scheme = ""
		req.URL.Host = ""
	}
	req.Header.Del("Proxy-Authorization")
	if err := req.Write(upConn); err != nil {
		s.onProxyFailure(usedProxy)
		s.writeHTTPError(client, http.StatusBadGateway)
		return
	}

	_, _ = io.Copy(client, upConn)
}

func (s *Service) handleSOCKS5(client net.Conn, br *bufio.Reader) {
	if err := s.socks5Handshake(client, br); err != nil {
		return
	}
	target, err := s.socks5ReadRequest(client, br)
	if err != nil {
		return
	}

	upConn, usedProxy, err := s.dialTarget(target)
	if err != nil {
		s.onProxyFailure(usedProxy)
		_, _ = client.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upConn.Close()
	s.onProxySuccess(usedProxy)

	_, _ = client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	s.tunnel(client, upConn)
}

func (s *Service) socks5Handshake(client net.Conn, br *bufio.Reader) error {
	nMethods, err := br.ReadByte()
	if err != nil {
		return err
	}
	methods := make([]byte, int(nMethods))
	if _, err := io.ReadFull(br, methods); err != nil {
		return err
	}

	needAuth := s.snapshotAuthRequired()
	method := byte(0x00)
	if needAuth {
		method = 0xFF
		for _, m := range methods {
			if m == 0x02 {
				method = 0x02
				break
			}
		}
	} else {
		hasNoAuth := false
		for _, m := range methods {
			if m == 0x00 {
				hasNoAuth = true
				break
			}
		}
		if !hasNoAuth {
			method = 0xFF
		}
	}

	if _, err := client.Write([]byte{0x05, method}); err != nil {
		return err
	}
	if method == 0xFF {
		return errors.New("no acceptable auth method")
	}
	if method != 0x02 {
		return nil
	}

	ver, err := br.ReadByte()
	if err != nil || ver != 0x01 {
		return errors.New("invalid auth version")
	}
	ulen, err := br.ReadByte()
	if err != nil {
		return err
	}
	uname := make([]byte, int(ulen))
	if _, err := io.ReadFull(br, uname); err != nil {
		return err
	}
	plen, err := br.ReadByte()
	if err != nil {
		return err
	}
	pass := make([]byte, int(plen))
	if _, err := io.ReadFull(br, pass); err != nil {
		return err
	}

	if s.validateUser(string(uname), string(pass)) {
		_, _ = client.Write([]byte{0x01, 0x00})
		return nil
	}
	_, _ = client.Write([]byte{0x01, 0x01})
	return errors.New("auth failed")
}

func (s *Service) socks5ReadRequest(client net.Conn, br *bufio.Reader) (string, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(br, head); err != nil {
		return "", err
	}
	if head[0] != 0x05 || head[1] != 0x01 {
		_, _ = client.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return "", errors.New("unsupported command")
	}

	atyp := head[3]
	host := ""
	switch atyp {
	case 0x01:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(br, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()
	case 0x03:
		ln, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		addr := make([]byte, int(ln))
		if _, err := io.ReadFull(br, addr); err != nil {
			return "", err
		}
		host = string(addr)
	case 0x04:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(br, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()
	default:
		return "", errors.New("unsupported atyp")
	}

	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(br, portBytes); err != nil {
		return "", err
	}
	port := int(portBytes[0])<<8 | int(portBytes[1])
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func (s *Service) dialTarget(target string) (net.Conn, string, error) {
	proxyAddr, err := s.pickUpstreamProxy()
	if err != nil {
		return nil, "", err
	}
	conn, err := dialViaUpstream(proxyAddr, target, 20*time.Second)
	return conn, proxyAddr, err
}

func (s *Service) pickUpstreamProxy() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.useGetIP {
		if err := s.ensureGetIPProxiesLocked(false); err != nil {
			return "", err
		}
	}
	if len(s.proxies) == 0 {
		return "", errors.New("no upstream proxy available")
	}

	now := time.Now()
	if s.mode == "loadbalance" {
		s.proxyIndex = (s.proxyIndex + 1) % len(s.proxies)
		s.currentProxy = s.proxies[s.proxyIndex]
		return s.currentProxy, nil
	}

	if s.currentProxy == "" {
		s.currentProxy = s.proxies[0]
		s.proxyIndex = 0
	}
	if s.interval > 0 && now.Sub(s.lastSwitchTime) >= time.Duration(s.interval)*time.Second {
		s.proxyIndex = (s.proxyIndex + 1) % len(s.proxies)
		s.currentProxy = s.proxies[s.proxyIndex]
		s.lastSwitchTime = now
	}
	return s.currentProxy, nil
}

func (s *Service) switchProxyLocked(force bool) bool {
	now := time.Now()
	if s.switchingProxy {
		return false
	}
	if !force && !s.lastSwitchAttempt.IsZero() && now.Sub(s.lastSwitchAttempt) < s.switchCooldown {
		return false
	}

	s.switchingProxy = true
	s.lastSwitchAttempt = now
	defer func() { s.switchingProxy = false }()

	if s.useGetIP {
		if err := s.ensureGetIPProxiesLocked(true); err != nil {
			slog.Warn("switch via getip failed", "error", err)
			return false
		}
		if len(s.proxies) == 0 {
			return false
		}
		s.currentProxy = s.proxies[0]
		s.proxyIndex = 0
		s.lastSwitchTime = now
		return true
	}

	if len(s.proxies) == 0 {
		return false
	}
	if len(s.proxies) == 1 {
		s.currentProxy = s.proxies[0]
		s.lastSwitchTime = now
		return true
	}
	s.proxyIndex = (s.proxyIndex + 1) % len(s.proxies)
	s.currentProxy = s.proxies[s.proxyIndex]
	s.lastSwitchTime = now
	return true
}

func (s *Service) onProxyFailure(proxyAddr string) {
	if strings.TrimSpace(proxyAddr) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.consecutiveFailures[proxyAddr]++
	s.failureLastSeen[proxyAddr] = now
	fails := s.consecutiveFailures[proxyAddr]
	if fails < s.proxyFailureThresh {
		return
	}
	if !s.lastProxyFailTime.IsZero() && now.Sub(s.lastProxyFailTime) < s.proxyFailureCD {
		return
	}
	s.lastProxyFailTime = now
	if s.switchProxyLocked(true) {
		s.consecutiveFailures[proxyAddr] = 0
		s.failureLastSeen[proxyAddr] = now
	}
}

func (s *Service) onProxySuccess(proxyAddr string) {
	if strings.TrimSpace(proxyAddr) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consecutiveFailures[proxyAddr] = 0
	s.failureLastSeen[proxyAddr] = time.Now()
}

func (s *Service) acquireConnSlot(ctx context.Context) bool {
	s.mu.RLock()
	sem := s.connSem
	s.mu.RUnlock()
	if sem == nil {
		return false
	}
	select {
	case sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Service) releaseConnSlot() {
	s.mu.RLock()
	sem := s.connSem
	s.mu.RUnlock()
	if sem == nil {
		return
	}
	select {
	case <-sem:
	default:
	}
}

func (s *Service) trackConn(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeConns == nil {
		s.activeConns = map[net.Conn]time.Time{}
	}
	s.activeConns[c] = time.Now()
}

func (s *Service) untrackConn(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.activeConns, c)
}

func (s *Service) cleanupLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanupFailureMap()
		}
	}
}

func (s *Service) cleanupFailureMap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failureRetention <= 0 {
		return
	}
	now := time.Now()
	for proxy, ts := range s.failureLastSeen {
		if now.Sub(ts) <= s.failureRetention {
			continue
		}
		delete(s.failureLastSeen, proxy)
		delete(s.consecutiveFailures, proxy)
	}
}

func (s *Service) runHealthCheckLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.healthCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			enabled, _, _, _, autoApply, autoPersist, _, testURL := s.snapshotHealthConfig()
			if !enabled {
				continue
			}
			_, err := s.RefreshValidProxies(RefreshValidOptions{
				Apply:       autoApply,
				Persist:     autoPersist,
				ForceSwitch: false,
				TestURL:     testURL,
			})
			if err != nil {
				slog.Warn("health check loop tick failed", "error", err)
			}
		}
	}
}

func (s *Service) RefreshValidProxies(opts RefreshValidOptions) (RefreshValidResult, error) {
	opts = normalizeRefreshValidOptions(opts)
	if !s.tryBeginHealthCheck() {
		res := RefreshValidResult{
			TriggeredAt: time.Now(),
			Skipped:     true,
			SkipReason:  "in_progress",
			LastError:   "health check already running",
		}
		s.updateHealthStatus(res)
		return res, errors.New("health check already running")
	}
	defer s.endHealthCheck()

	start := time.Now()
	proxies, currentProxy, proxyFile, useGetIP, minPool, timeout, concurrency := s.snapshotProxiesForHealth()
	valid := filterValidProxiesConcurrently(proxies, opts.TestURL, timeout, concurrency)

	result := RefreshValidResult{
		TriggeredAt:  start,
		BeforeTotal:  len(proxies),
		ValidTotal:   len(valid),
		CurrentProxy: currentProxy,
		ValidProxies: append([]string(nil), valid...),
	}

	errForReturn := ""
	if opts.Apply {
		if len(valid) < minPool {
			result.Skipped = true
			result.SkipReason = "below_min_pool_size"
			result.LastError = fmt.Sprintf("valid proxies %d below min pool size %d", len(valid), minPool)
		} else {
			s.SetProxies(valid)
			result.Applied = true
			if opts.ForceSwitch {
				if !containsProxy(valid, currentProxy) {
					s.SwitchProxy()
				}
			}
			result.CurrentProxy = s.CurrentProxyValue()
			if shouldPersistToFile(useGetIP, opts.Persist) {
				if err := persistProxyList(sanitizeProxyFilePath(proxyFile), valid); err != nil {
					errForReturn = err.Error()
					result.LastError = errForReturn
				} else {
					result.Persisted = true
				}
			}
		}
	}

	result.DurationMS = time.Since(start).Milliseconds()
	s.updateHealthStatus(result)
	if errForReturn != "" {
		return result, errors.New(errForReturn)
	}
	return result, nil
}

func (s *Service) GetHealthSnapshot() HealthCheckSnapshot {
	enabled, interval, timeout, concurrency, autoApply, autoPersist, minPool, _ := s.snapshotHealthConfig()
	s.mu.RLock()
	status := s.healthStatus
	running := s.healthCheckRunning
	s.mu.RUnlock()
	return healthStatusToSnapshot(status, enabled, interval, timeout, concurrency, autoApply, autoPersist, minPool, running)
}

func (s *Service) ActiveConnectionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.activeConns)
}

func (s *Service) FailureEntryCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.consecutiveFailures)
}

func (s *Service) MaxConcurrentConnections() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxConcurrentConn
}

func (s *Service) FailureRetentionSeconds() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int(s.failureRetention.Seconds())
}

func (s *Service) CleanupIntervalSeconds() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int(s.cleanupInterval.Seconds())
}

func (s *Service) ConnectionTimeoutSeconds() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int(s.connIOTimeout.Seconds())
}

func (s *Service) FailureThreshold() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proxyFailureThresh
}

func (s *Service) FailureCooldownSeconds() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int(s.proxyFailureCD.Seconds())
}

func (s *Service) SwitchCooldownSeconds() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int(s.switchCooldown.Seconds())
}

func (s *Service) GetIPRefreshMinimumSeconds() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int(s.getipRefreshMinimum.Seconds())
}

func (s *Service) CurrentProxyFile() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proxyFile
}

func (s *Service) CurrentPort() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.port
}

func (s *Service) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

func (s *Service) SupportsGetIP() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.useGetIP
}

func (s *Service) CurrentGetIPURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getipURL
}

func (s *Service) CurrentLanguage() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.language
}

func (s *Service) AuthEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authRequired
}

func (s *Service) UserCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

func (s *Service) ProxyCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.proxies)
}

func (s *Service) CurrentProxyValue() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentProxy
}

func (s *Service) CurrentMode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

func (s *Service) CurrentInterval() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.interval
}

func (s *Service) CurrentProxyIndex() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proxyIndex
}

func (s *Service) LastSwitchTimeUnix() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastSwitchTime.IsZero() {
		return 0
	}
	return s.lastSwitchTime.Unix()
}

func (s *Service) LastProxyFailureTimeUnix() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastProxyFailTime.IsZero() {
		return 0
	}
	return s.lastProxyFailTime.Unix()
}

func (s *Service) LastGetIPRefreshUnix() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastGetIPRefresh.IsZero() {
		return 0
	}
	return s.lastGetIPRefresh.Unix()
}

func (s *Service) LastSwitchAttemptUnix() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastSwitchAttempt.IsZero() {
		return 0
	}
	return s.lastSwitchAttempt.Unix()
}

func (s *Service) IsSwitchingProxy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.switchingProxy
}

func (s *Service) ConsecutiveFailureCount(proxyAddr string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.consecutiveFailures[proxyAddr]
}

func (s *Service) ConsecutiveFailureMapSnapshot() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]int, len(s.consecutiveFailures))
	for k, v := range s.consecutiveFailures {
		out[k] = v
	}
	return out
}

func (s *Service) FailureLastSeenSnapshot() map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]int64, len(s.failureLastSeen))
	for k, v := range s.failureLastSeen {
		out[k] = v.Unix()
	}
	return out
}

func (s *Service) CurrentUsersSnapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMap(s.users)
}

func (s *Service) CurrentProxiesSnapshot() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.proxies))
	copy(out, s.proxies)
	return out
}

func (s *Service) ensureGetIPProxiesLocked(force bool) error {
	if !force && !s.lastGetIPRefresh.IsZero() && time.Since(s.lastGetIPRefresh) < s.getipRefreshMinimum {
		if len(s.proxies) > 0 {
			return nil
		}
	}
	if strings.TrimSpace(s.getipURL) == "" {
		if len(s.proxies) == 0 {
			return errors.New("getip_url is empty")
		}
		return nil
	}

	oldProxies := append([]string(nil), s.proxies...)
	oldCurrent := s.currentProxy
	oldIndex := s.proxyIndex

	list, err := fetchGetIPList(s.getipURL, s.proxyUsername, s.proxyPassword, s.getipProxyScheme)
	if err != nil {
		if len(oldProxies) > 0 {
			s.proxies = oldProxies
			s.currentProxy = oldCurrent
			s.proxyIndex = oldIndex
			return nil
		}
		return err
	}
	if len(list) == 0 {
		if len(oldProxies) > 0 {
			s.proxies = oldProxies
			s.currentProxy = oldCurrent
			s.proxyIndex = oldIndex
			return nil
		}
		return errors.New("empty getip proxies")
	}

	testURL := strings.TrimSpace(s.healthCheckTestURL)
	if testURL == "" {
		testURL = "https://www.baidu.com"
	}
	valid := filterValidProxiesConcurrently(list, testURL, s.healthCheckTimeout, s.healthCheckConcurrency)
	if len(valid) == 0 {
		if len(oldProxies) > 0 {
			s.proxies = oldProxies
			s.currentProxy = oldCurrent
			s.proxyIndex = oldIndex
			return nil
		}
		return errors.New("no valid getip proxies")
	}

	s.proxies = valid
	s.proxyIndex = 0
	s.currentProxy = s.proxies[0]
	s.lastGetIPRefresh = time.Now()
	return nil
}

func fetchGetIPList(urlStr, username, password, defaultScheme string) ([]string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(urlStr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("getip status: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	proxies := parseGetIPPayload(body)
	if len(proxies) == 0 {
		return nil, errors.New("no proxies in getip response")
	}

	clean := make([]string, 0, len(proxies))
	seen := map[string]struct{}{}
	for _, p := range proxies {
		n := normalizeProxy(p, username, password, defaultScheme)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		clean = append(clean, n)
	}
	return clean, nil
}

func parseGetIPPayload(body []byte) []string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err == nil {
		if dataRaw, ok := payload["data"]; ok {
			if dataMap, ok := dataRaw.(map[string]any); ok {
				if proxiesRaw, ok := dataMap["proxies"]; ok {
					if arr, ok := proxiesRaw.([]any); ok {
						out := make([]string, 0, len(arr))
						for _, item := range arr {
							out = append(out, fmt.Sprint(item))
						}
						return out
					}
				}
			}
		}
	}

	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

func normalizeProxy(raw, username, password, defaultScheme string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.EqualFold(raw, "error000x-13") {
		return ""
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return ""
		}
		if username != "" && password != "" && u.User == nil {
			u.User = url.UserPassword(username, password)
		}
		return u.String()
	}
	scheme := normalizeGetIPProxyScheme(defaultScheme)
	if username != "" && password != "" {
		return scheme + "://" + username + ":" + password + "@" + raw
	}
	return scheme + "://" + raw
}

func normalizeGetIPProxyScheme(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "http", "https", "socks5":
		return s
	default:
		return "socks5"
	}
}

func dialViaUpstream(proxyRaw, target string, timeout time.Duration) (net.Conn, error) {
	u, err := url.Parse(proxyRaw)
	if err != nil {
		return nil, err
	}
	scheme := strings.ToLower(u.Scheme)
	hostPort := u.Host
	if !strings.Contains(hostPort, ":") {
		hostPort = net.JoinHostPort(hostPort, defaultPortByScheme(scheme))
	}
	user := ""
	pass := ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}

	switch scheme {
	case "socks5":
		var auth *proxy.Auth
		if user != "" {
			auth = &proxy.Auth{User: user, Password: pass}
		}
		d := &net.Dialer{Timeout: timeout}
		dialer, err := proxy.SOCKS5("tcp", hostPort, auth, d)
		if err != nil {
			return nil, err
		}
		return dialer.Dial("tcp", target)
	case "http", "https":
		return dialHTTPConnect(hostPort, target, user, pass, timeout, scheme == "https")
	default:
		return nil, fmt.Errorf("unsupported upstream scheme: %s", scheme)
	}
}

func dialHTTPConnect(proxyAddr, target, user, pass string, timeout time.Duration, tlsProxy bool) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return nil, err
	}
	if tlsProxy {
		tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
		if err := tlsConn.Handshake(); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}

	var b strings.Builder
	b.WriteString("CONNECT " + target + " HTTP/1.1\r\n")
	b.WriteString("Host: " + target + "\r\n")
	if user != "" {
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		b.WriteString("Proxy-Authorization: Basic " + token + "\r\n")
	}
	b.WriteString("Proxy-Connection: Keep-Alive\r\n\r\n")
	if _, err := conn.Write([]byte(b.String())); err != nil {
		_ = conn.Close()
		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream connect failed: %s", resp.Status)
	}
	return conn, nil
}

func requestTarget(req *http.Request) string {
	host := req.Host
	if req.URL != nil && req.URL.Host != "" {
		host = req.URL.Host
	}
	if host == "" {
		return ""
	}
	if strings.EqualFold(req.Method, http.MethodConnect) {
		if !strings.Contains(host, ":") {
			return net.JoinHostPort(host, "443")
		}
		return host
	}
	if !strings.Contains(host, ":") {
		return net.JoinHostPort(host, "80")
	}
	return host
}

func (s *Service) tunnel(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeWrite(a)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeWrite(b)
	}()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		return
	}
	_ = c.Close()
}

func (s *Service) writeHTTPError(client net.Conn, code int) {
	text := http.StatusText(code)
	_, _ = client.Write([]byte(fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", code, text)))
}

func (s *Service) writeProxyAuthRequired(client net.Conn) {
	_, _ = client.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"ProxyCat\"\r\nContent-Length: 0\r\n\r\n"))
}

func (s *Service) checkHTTPAuth(proxyAuth string) bool {
	s.mu.RLock()
	need := s.authRequired
	users := cloneMap(s.users)
	s.mu.RUnlock()
	if !need {
		return true
	}
	if !strings.HasPrefix(strings.ToLower(proxyAuth), "basic ") {
		return false
	}
	raw := strings.TrimSpace(proxyAuth[6:])
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return false
	}
	pwd, ok := users[parts[0]]
	return ok && pwd == parts[1]
}

func (s *Service) validateUser(username, password string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pwd, ok := s.users[username]
	return ok && pwd == password
}

func (s *Service) snapshotAuthRequired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authRequired
}

func (s *Service) timeUntilNextSwitchLocked() float64 {
	if s.mode == "loadbalance" {
		return -1
	}
	left := float64(s.interval) - time.Since(s.lastSwitchTime).Seconds()
	if left < 0 {
		return 0
	}
	return left
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

func defaultPortByScheme(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	default:
		return "80"
	}
}

func atoiDefault(s string, d int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return d
	}
	return n
}

func toBool(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	return v == "true" || v == "1" || v == "yes"
}

func fallback(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

func cloneMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
