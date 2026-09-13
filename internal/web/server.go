package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/vay1314/CtYun-Keeper/internal/ctyun"
	"github.com/vay1314/CtYun-Keeper/internal/security"
	"github.com/vay1314/CtYun-Keeper/internal/service"
	"github.com/vay1314/CtYun-Keeper/internal/storage"
	"github.com/vay1314/CtYun-Keeper/internal/update"
)

type Server struct {
	store                     *storage.Store
	manager                   *service.Manager
	sessionKey, credentialKey []byte
	version, dataDir          string
	secure                    bool
	updater                   *update.Checker
	updateManager             *update.Manager
	updateInitError           error
	requestShutdown           func(int)
	ctx                       context.Context
	cancel                    context.CancelFunc
	updateWG                  sync.WaitGroup
	mux                       *http.ServeMux
}

const defaultGitHubProxy = "https://gh-proxy.com/"

func New(store *storage.Store, m *service.Manager, sessionKey, credentialKey []byte, version, dataDir, staticDir, updateRepo, updatePublicKey string, secure bool, requestShutdown func(int)) *Server {
	proxy := strings.TrimSpace(os.Getenv("GITHUB_PROXY"))
	if proxy == "" {
		proxy = defaultGitHubProxy
	}
	if saved, err := store.Setting("github_proxy"); err == nil && store.HasSetting("github_proxy") {
		proxy = strings.TrimSpace(saved)
	}
	updater := update.NewChecker(updateRepo, version, proxy, update.CurrentPlatform())
	publicKey, keyErr := update.ParsePublicKey(updatePublicKey)
	if keyErr == nil && len(publicKey) > 0 {
		updater.SetPublicKey(publicKey)
	}
	if keyErr == nil && len(publicKey) == 0 {
		keyErr = errors.New("当前程序未内置更新签名公钥，请使用正式发布包或镜像")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		store:           store,
		manager:         m,
		sessionKey:      sessionKey,
		credentialKey:   credentialKey,
		version:         version,
		dataDir:         dataDir,
		secure:          secure,
		updater:         updater,
		updateManager:   update.NewManager(dataDir),
		updateInitError: keyErr,
		requestShutdown: requestShutdown,
		ctx:             ctx,
		cancel:          cancel,
		mux:             http.NewServeMux(),
	}
	s.routes(staticDir)
	s.startAutomaticUpdateChecks()
	return s
}

func (s *Server) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.updateWG.Wait()
}
func (s *Server) Handler() http.Handler { return s.securityHeaders(s.mux) }
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; font-src 'self'")
		next.ServeHTTP(w, r)
	})
}
func (s *Server) routes(staticDir string) {
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))
	s.mux.HandleFunc("/health", s.health)
	s.mux.HandleFunc("/setup", s.setup)
	s.mux.HandleFunc("/login", s.login)
	s.mux.HandleFunc("/logout", s.logout)
	s.mux.HandleFunc("/accounts/save", s.saveAccount)
	s.mux.HandleFunc("/accounts/new", s.accountNew)
	s.mux.HandleFunc("/accounts/", s.accountRoute)
	s.mux.HandleFunc("/accounts", s.accounts)
	s.mux.HandleFunc("/partials/status", s.statusPartial)
	s.mux.HandleFunc("/partials/accounts", s.accountsPartial)
	s.mux.HandleFunc("/partials/log-sources", s.logSourcesPartial)
	s.mux.HandleFunc("/partials/task-accounts", s.taskCards)
	s.mux.HandleFunc("/tasks/", s.taskRoute)
	s.mux.HandleFunc("/tasks", s.tasks)
	s.mux.HandleFunc("/logs/clear", s.clearLog)
	s.mux.HandleFunc("/logs", s.logs)
	s.mux.HandleFunc("/logs/", s.logStream)
	s.mux.HandleFunc("/ctyun/restart", s.restart)
	s.mux.HandleFunc("/settings/auth", s.authSettings)
	s.mux.HandleFunc("/settings/password", s.password)
	s.mux.HandleFunc("/settings/logs/clear", s.clearAllLogs)
	s.mux.HandleFunc("/settings/logs", s.logSettings)
	s.mux.HandleFunc("/settings/update/check", s.checkUpdate)
	s.mux.HandleFunc("/settings/update/install", s.installUpdate)
	s.mux.HandleFunc("/settings/update/proxy", s.saveUpdateProxy)
	s.mux.HandleFunc("/partials/update-status", s.updateStatusPartial)
	s.mux.HandleFunc("/partials/update-badge", s.updateBadgePartial)
	s.mux.HandleFunc("/settings", s.settings)
	s.mux.HandleFunc("/api/status", s.apiStatus)
	s.mux.HandleFunc("/api/update/status", s.apiUpdateStatus)
	s.mux.HandleFunc("/api/accounts/", s.apiAccountRoute)
	s.mux.HandleFunc("/", s.dashboard)
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	database, scheduler, status := "ok", "ok", "ok"
	if err := s.store.DB.PingContext(r.Context()); err != nil {
		database, status = "error", "error"
	}
	if !s.manager.SchedulerHealthy() {
		scheduler, status = "error", "error"
	}
	if status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	fmt.Fprintf(w, `{"status":%q,"version":%q,"database":%q,"scheduler":%q}`, status, s.version, database, scheduler)
}
func (s *Server) cookie(w http.ResponseWriter, name, value string, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: 12 * 3600, HttpOnly: httpOnly, Secure: s.secure, SameSite: http.SameSiteStrictMode})
}
func (s *Server) csrf(w http.ResponseWriter, r *http.Request) string {
	if c, e := r.Cookie("ctyun_csrf"); e == nil && security.VerifyCookie(s.sessionKey, c.Value) {
		return strings.Split(c.Value, ".")[0]
	}
	v := security.RandomToken(24)
	s.cookie(w, "ctyun_csrf", security.SignCookie(s.sessionKey, v), true)
	return v
}
func (s *Server) checkCSRF(r *http.Request) bool {
	c, e := r.Cookie("ctyun_csrf")
	if e != nil || !security.VerifyCookie(s.sessionKey, c.Value) {
		return false
	}
	expected := strings.Split(c.Value, ".")[0]
	return r.FormValue("csrf_token") == expected || r.Header.Get("X-CSRF-Token") == expected
}
func (s *Server) authed(r *http.Request) bool {
	if !s.passwordAuthEnabled() {
		hash, _ := s.store.Setting("admin_password_hash")
		return hash != ""
	}
	c, e := r.Cookie("ctyun_session")
	return e == nil && security.VerifyCookie(s.sessionKey, c.Value) && strings.HasPrefix(c.Value, "authenticated.")
}

func (s *Server) passwordAuthEnabled() bool {
	value, _ := s.store.Setting("admin_auth_enabled")
	return value != "false"
}

func (s *Server) guard(w http.ResponseWriter, r *http.Request) bool {
	hash, _ := s.store.Setting("admin_password_hash")
	if hash == "" {
		http.Redirect(w, r, "/setup", 303)
		return false
	}
	if !s.authed(r) {
		http.Redirect(w, r, "/login", 303)
		return false
	}
	return true
}
func esc(v string) string { return html.EscapeString(v) }
func formatTime(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "尚未更新"
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, v); err == nil {
			return parsed.Format("2006-01-02 15:04:05")
		}
	}
	if len(v) >= 19 && v[10] == 'T' {
		return v[:10] + " " + v[11:19]
	}
	return v
}
func platformStatusUpdatedToday(updatedAt string, now time.Time) bool {
	updatedAt = strings.TrimSpace(updatedAt)
	if updatedAt == "" {
		return false
	}
	var updated time.Time
	var err error
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		updated, err = time.Parse(layout, updatedAt)
		if err == nil {
			break
		}
	}
	if err != nil {
		updated, err = time.ParseInLocation("2006-01-02 15:04:05", updatedAt, now.Location())
	}
	if err != nil {
		return false
	}
	updated = updated.In(now.Location())
	year, month, day := now.Date()
	updatedYear, updatedMonth, updatedDay := updated.Date()
	return year == updatedYear && month == updatedMonth && day == updatedDay
}
func reverseLogText(v string) string {
	v = strings.ReplaceAll(v, "\r\n", "\n")
	if v == "" {
		return ""
	}
	trailingNewline := strings.HasSuffix(v, "\n")
	v = strings.TrimSuffix(v, "\n")
	lines := strings.Split(v, "\n")
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	result := strings.Join(lines, "\n")
	if trailingNewline {
		result += "\n"
	}
	return result
}
func taskLabel(v string) string {
	return map[string]string{"login": "登录云电脑", "chat": "AI 对话", "pc": "云电脑挂机", "redeem": "自动兑换"}[v]
}
func statusLabel(v string) string {
	if label := map[string]string{"queued": "等待中", "running": "运行中", "success": "已完成", "failed": "失败", "stopped": "已停止", "interrupted": "已中断"}[v]; label != "" {
		return label
	}
	return v
}
func buttonIcon(name string) string {
	path := map[string]string{
		"login":  `<path d="M10 17l5-5-5-5"/><path d="M15 12H3"/><path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4"/>`,
		"usage":  `<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>`,
		"chat":   `<path d="M21 15a4 4 0 0 1-4 4H8l-5 3V7a4 4 0 0 1 4-4h10a4 4 0 0 1 4 4z"/><path d="M8 9h8M8 13h5"/>`,
		"status": `<rect x="4" y="3" width="16" height="18" rx="2"/><path d="M8 8h8M8 12h5M8 16h8"/>`,
	}[name]
	return `<svg class="button-icon" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` + path + `</svg>`
}
func dashboardIcon(name string) string {
	path := map[string]string{
		"cloud":   `<path d="M7 18h10a4 4 0 0 0 .7-7.94A6 6 0 0 0 6.26 8.1 4.5 4.5 0 0 0 7 18Z"/><path d="m9.5 13 1.7 1.7 3.5-3.7"/>`,
		"users":   `<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75"/>`,
		"tasks":   `<path d="m13 2-9 12h8l-1 8 9-12h-8l1-8Z"/>`,
		"history": `<path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5M12 7v5l3 2"/>`,
		"server":  `<rect x="3" y="4" width="18" height="6" rx="2"/><rect x="3" y="14" width="18" height="6" rx="2"/><path d="M7 7h.01M7 17h.01"/>`,
		"uptime":  `<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>`,
		"cpu":     `<rect x="7" y="7" width="10" height="10" rx="1"/><path d="M9 1v3M15 1v3M9 20v3M15 20v3M20 9h3M20 14h3M1 9h3M1 14h3"/>`,
		"version": `<path d="m12 3 8 4.5v9L12 21l-8-4.5v-9L12 3Z"/><path d="m4.5 7.5 7.5 4 7.5-4M12 21v-9.5"/>`,
	}[name]
	return `<svg class="dashboard-icon" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` + path + `</svg>`
}
func dashboardTaskLabel(v string) string {
	if label := map[string]string{"login": "登陆任务", "pc": "时长任务", "chat": "AI对话任务", "redeem": "自动兑换任务"}[v]; label != "" {
		return label
	}
	return "其他任务"
}
func passwordToggle() string {
	return `<button class="password-toggle" type="button" data-password-toggle aria-label="显示密码" aria-pressed="false"><svg class="eye-open" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6S2 12 2 12Z"/><circle cx="12" cy="12" r="3"/></svg><svg class="eye-closed" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m3 3 18 18"/><path d="M10.6 6.2A10.9 10.9 0 0 1 12 6c6.5 0 10 6 10 6a18.5 18.5 0 0 1-2.1 2.8M6.6 6.6C3.6 8.4 2 12 2 12s3.5 6 10 6c1.8 0 3.3-.5 4.6-1.2"/></svg></button>`
}
func navActive(path, target string) string {
	if (target == "/" && path == "/") || (target != "/" && strings.HasPrefix(path, target)) {
		return "active"
	}
	return ""
}
func checked(v bool) string {
	if v {
		return " checked"
	}
	return ""
}
func disabled(v bool) string {
	if v {
		return " disabled"
	}
	return ""
}
func selected(v bool) string {
	if v {
		return " selected"
	}
	return ""
}
func (s *Server) page(w http.ResponseWriter, r *http.Request, title, content string, auth bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	token := s.csrf(w, r)
	content = strings.ReplaceAll(content, "{{CSRF}}", esc(token))
	notice := r.URL.Query().Get("notice")
	flash := ""
	if notice != "" {
		flash = `<div class="flash success" role="status">` + esc(notice) + `</div>`
	}
	if errText := r.URL.Query().Get("error"); errText != "" {
		flash = `<div class="flash error" role="alert">` + esc(errText) + `</div>`
	}
	nav := ""
	mainClass := "auth-shell"
	if auth {
		mainClass = "main-shell"
		logoutAction := ""
		if s.passwordAuthEnabled() {
			logoutAction = `<form method="post" action="/logout"><input type="hidden" name="csrf_token" value="` + esc(token) + `"><button class="icon-button danger-icon" aria-label="退出"><span class="material-symbols-rounded">power_settings_new</span></button></form>`
		}
		nav = `<aside class="sidebar" id="sidebar"><a class="brand" href="/"><span class="brand-mark material-symbols-rounded">cloud_sync</span><span><strong>CtYunKeeper</strong><small>云电脑管理台</small></span></a><nav><span class="nav-section">管理</span><a class="` + navActive(r.URL.Path, "/") + `" href="/"><span class="material-symbols-rounded">dashboard</span><span>仪表盘</span></a><a class="` + navActive(r.URL.Path, "/accounts") + `" href="/accounts"><span class="material-symbols-rounded">manage_accounts</span><span>账号管理</span></a><a class="` + navActive(r.URL.Path, "/tasks") + `" href="/tasks"><span class="material-symbols-rounded">schedule</span><span>任务中心</span></a><span class="nav-section">系统</span><a class="` + navActive(r.URL.Path, "/logs") + `" href="/logs"><span class="material-symbols-rounded">terminal</span><span>日志中心</span></a><a class="` + navActive(r.URL.Path, "/settings") + `" href="/settings"><span class="material-symbols-rounded">settings</span><span>系统设置</span></a></nav><div class="sidebar-foot"><span class="material-symbols-rounded">deployed_code</span><span><span class="sidebar-product-title"><strong>CtYunKeeper</strong>` + s.updateAvailableBadgeSlot("sidebar") + `</span><small>版本 v` + esc(s.version) + `</small></span></div></aside><header class="topbar"><button class="icon-button sidebar-toggle" type="button"><span class="material-symbols-rounded">menu</span></button><strong>天翼云电脑自动化管理</strong><div class="topbar-actions"><button class="icon-button theme-toggle" type="button" data-theme-toggle aria-label="切换网页主题"><span class="local-icon theme-icon-moon" aria-hidden="true"></span><span class="local-icon theme-icon-sun" aria-hidden="true"></span></button><a class="icon-button" href="/logs" aria-label="查看日志"><span class="material-symbols-rounded">notifications</span></a><form method="post" action="/ctyun/restart"><input type="hidden" name="csrf_token" value="` + esc(token) + `"><button class="icon-button" aria-label="重新加载保活"><span class="material-symbols-rounded">refresh</span></button></form>` + logoutAction + `</div></header><button class="sidebar-backdrop" type="button"></button>`
	}
	fmt.Fprintf(w, "<!doctype html><html lang=zh-CN><head><meta charset=utf-8><meta name=viewport content='width=device-width,initial-scale=1'><meta name=color-scheme content='light dark'><meta name=csrf-token content='%s'><title>%s · CtYunKeeper</title><script src='/static/theme.js?v=%s-ui19'></script><link rel=stylesheet href='/static/app.css?v=%s-ui19'><script src='/static/htmx.min.js' defer></script><script src='/static/app.js?v=%s-ui19' defer></script></head><body data-authenticated='%t' data-app-version='%s'>%s<main class='%s'>%s%s</main></body></html>", esc(token), esc(title), esc(s.version), esc(s.version), esc(s.version), auth, esc(s.version), nav, mainClass, flash, content)
}
func redirect(w http.ResponseWriter, r *http.Request, path, msg string, isErr bool) {
	key := "notice"
	if isErr {
		key = "error"
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	http.Redirect(w, r, path+sep+key+"="+url.QueryEscape(msg), 303)
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	hash, _ := s.store.Setting("admin_password_hash")
	if hash != "" {
		if s.passwordAuthEnabled() {
			http.Redirect(w, r, "/login", 303)
		} else {
			http.Redirect(w, r, "/", 303)
		}
		return
	}
	if r.Method == "POST" {
		_ = r.ParseForm()
		if !s.checkCSRF(r) {
			http.Error(w, "CSRF validation failed", 403)
			return
		}
		p := r.FormValue("password")
		if len(p) < 8 || p != r.FormValue("confirmation") {
			redirect(w, r, "/setup", "密码至少 8 位且两次输入必须一致", true)
			return
		}
		_ = s.store.SetSetting("admin_password_hash", security.HashPassword(p))
		s.cookie(w, "ctyun_session", security.SignCookie(s.sessionKey, "authenticated"), true)
		http.Redirect(w, r, "/", 303)
		return
	}
	s.page(w, r, "初始化", `<section class="auth-card"><p class="eyebrow">首次运行</p><h1>设置管理密码</h1><form method=post><input type=hidden name=csrf_token value="{{CSRF}}"><label>密码<input type=password name=password minlength=8 required></label><label>确认密码<input type=password name=confirmation minlength=8 required></label><button class=primary>开始使用</button></form></section>`, false)
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.passwordAuthEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.Method == "POST" {
		_ = r.ParseForm()
		if !s.checkCSRF(r) {
			http.Error(w, "CSRF validation failed", 403)
			return
		}
		hash, _ := s.store.Setting("admin_password_hash")
		if !security.VerifyPassword(hash, r.FormValue("password")) {
			redirect(w, r, "/login", "密码不正确", true)
			return
		}
		s.cookie(w, "ctyun_session", security.SignCookie(s.sessionKey, "authenticated"), true)
		http.Redirect(w, r, "/", 303)
		return
	}
	s.page(w, r, "登录", `<section class="auth-card"><p class="eyebrow">管理面板</p><h1>欢迎回来</h1><form method=post><input type=hidden name=csrf_token value="{{CSRF}}"><label>管理密码<input type=password name=password required autofocus></label><button class=primary>登录</button></form></section>`, false)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || !s.checkCSRF(r) {
		http.Error(w, "Forbidden", 403)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "ctyun_session", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", 303)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.guard(w, r) {
		return
	}
	content := fmt.Sprintf(`<header class="page-head"><div><p class=eyebrow>运行状态</p><h1>仪表盘</h1><p class=page-subtitle>查看保活服务、账号与自动任务的实时状态</p></div></header><section class=metric-grid id=status-cards hx-get=/partials/status hx-trigger='every 10s'>%s</section><article class="panel info-panel"><div class="panel-head dashboard-info-head"><div class=panel-title><span class="panel-icon dashboard-panel-icon">%s</span><div><p class=eyebrow>运行信息</p><h2>服务状态</h2></div></div><span class=service-health><i></i>服务在线</span></div><dl class=service-facts><div><dt><span class=fact-icon>%s</span><span>程序运行时长</span></dt><dd id=program-uptime data-uptime-seconds="%d">计算中</dd></div><div><dt><span class=fact-icon>%s</span><span>运行架构</span></dt><dd>%s</dd></div><div><dt><span class=fact-icon>%s</span><span>当前版本</span>%s</dt><dd>v%s</dd></div></dl></article>`, s.dashboardMetrics(), dashboardIcon("server"), dashboardIcon("uptime"), int(time.Since(s.manager.Started()).Seconds()), dashboardIcon("cpu"), esc(runtime.GOARCH), dashboardIcon("version"), s.updateAvailableBadgeSlot("dashboard"), esc(s.version))
	s.page(w, r, "仪表盘", content, true)
}
func (s *Server) statusPartial(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	fmt.Fprint(w, s.dashboardMetrics())
}

func (s *Server) dashboardMetrics() string {
	accounts, _ := s.store.Accounts()
	runs, _ := s.store.Runs(200)
	state, workers := s.manager.KeepaliveStatus()
	runningAccounts := s.manager.RunningAccountCount()

	keepaliveText := "当前没有云电脑正在保活"
	keepaliveAccounts := 0
	activeKeepaliveAccounts := 0
	now := time.Now()
	for _, account := range accounts {
		if account.Enabled && account.EffectiveKeepaliveMode() != storage.KeepaliveOff {
			keepaliveAccounts++
			if account.KeepaliveActiveAt(now) {
				activeKeepaliveAccounts++
			}
		}
	}
	if keepaliveAccounts == 0 {
		state = "已关闭"
		keepaliveText = "未启用云电脑保活"
	} else if activeKeepaliveAccounts == 0 && workers == 0 {
		state = "等待周期"
		keepaliveText = "当前不在定时保活时段"
	}
	if workers > 0 {
		keepaliveText = fmt.Sprintf("正在保活 %d 台云电脑", workers)
	}
	var active []storage.Run
	var recent *storage.Run
	for i := range runs {
		if runs[i].Status == "queued" || runs[i].Status == "running" {
			active = append(active, runs[i])
		} else if recent == nil {
			recent = &runs[i]
		}
	}

	activeContent := `<strong class="metric-value small">当前空闲</strong><span class="metric-note">暂无正在执行的自动化任务</span>`
	if len(active) > 0 {
		var list strings.Builder
		fmt.Fprintf(&list, `<strong class="metric-value small">%d 项任务</strong><ul class="metric-task-list">`, len(active))
		for _, run := range active {
			name := run.AccountName
			if name == "" {
				name = fmt.Sprintf("账号 #%d", run.AccountID)
			}
			fmt.Fprintf(&list, `<li><span class="task-state-dot %s"></span><span><b>%s</b><small>%s · %s</small></span></li>`, esc(run.Status), esc(name), esc(dashboardTaskLabel(run.TaskType)), esc(statusLabel(run.Status)))
		}
		list.WriteString(`</ul>`)
		activeContent = list.String()
	}

	recentContent := `<strong class="metric-value small">暂无记录</strong><span class="metric-note">任务结束后将在这里显示结果</span>`
	if recent != nil {
		name := recent.AccountName
		if name == "" {
			name = fmt.Sprintf("账号 #%d", recent.AccountID)
		}
		finishedAt := recent.FinishedAt
		if finishedAt == "" {
			finishedAt = recent.StartedAt
		}
		recentContent = fmt.Sprintf(`<strong class="metric-value small metric-account-name" title="%s">%s</strong><div class=metric-result><span class="pill %s">%s</span><span>%s</span></div><span class=metric-time>%s</span>`, esc(name), esc(name), esc(recent.Status), esc(statusLabel(recent.Status)), esc(dashboardTaskLabel(recent.TaskType)), esc(formatTime(finishedAt)))
	}

	stateTone := "success"
	if strings.Contains(state, "异常") || strings.Contains(state, "失败") || strings.Contains(state, "错误") {
		stateTone = "danger"
	} else if workers == 0 {
		stateTone = "warning"
	}
	return fmt.Sprintf(`<article class="panel metric-card metric-%s"><div class=metric-top><span class=metric-icon>%s</span><span class=metric-label>云电脑保活</span><i class="metric-status-dot %s"></i></div><strong class="metric-value small">%s</strong><span class="metric-note">%s</span></article><article class="panel metric-card metric-accounts"><div class=metric-top><span class=metric-icon>%s</span><span class=metric-label>账号概况</span></div><strong class=metric-number>%d<em>个账号</em></strong><span class=metric-note><b>%d</b> 个正在运行</span></article><article class="panel metric-card metric-card-tasks"><div class=metric-top><span class=metric-icon>%s</span><span class=metric-label>执行中的任务</span></div>%s</article><article class="panel metric-card metric-recent"><div class=metric-top><span class=metric-icon>%s</span><span class=metric-label>最近任务</span></div>%s</article>`, stateTone, dashboardIcon("cloud"), stateTone, esc(state), esc(keepaliveText), dashboardIcon("users"), len(accounts), runningAccounts, dashboardIcon("tasks"), activeContent, dashboardIcon("history"), recentContent)
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	var b strings.Builder
	b.WriteString(`<header class=page-head><div><p class=eyebrow>配置</p><h1>账号</h1></div><a class="primary button" href=/accounts/new>添加账号</a></header>`)
	b.WriteString(s.accountTable())
	s.page(w, r, "账号", b.String(), true)
}
func (s *Server) accountTable() string {
	values, _ := s.store.Accounts()
	var b strings.Builder
	b.WriteString(`<article id=account-table class="panel table-panel" hx-get=/partials/accounts hx-trigger="every 5s" hx-swap=outerHTML><div class=table-wrap><table><thead><tr><th>账号</th><th>保活模式</th><th>登录任务</th><th>AI 对话任务</th><th>时长任务</th><th>操作</th></tr></thead><tbody>`)
	for _, a := range values {
		status := s.manager.AccountStatus(a.ID)
		modeLabel, modeTone := keepaliveModeLabel(a)
		fmt.Fprintf(&b, `<tr><td><strong>%s</strong><small>%s</small></td><td><span class="pill %s">%s</span><small>%s</small></td><td>%s</td><td>%s</td><td>%s</td><td class=actions><a href="/accounts/%d/edit">编辑</a><a href="/accounts/%d/redeem">兑换</a><form method=post action="/accounts/%d/device-verification/start"><input type=hidden name=csrf_token value="{{CSRF}}"><button class=link-button>设备验证</button></form><form method=post action="/accounts/%d/delete"><input type=hidden name=csrf_token value="{{CSRF}}"><button class=danger-link>删除</button></form></td></tr>`, esc(a.Name), esc(mask(a.Username)), modeTone, modeLabel, esc(status), scheduleSummary(a.LoginEnabled, a.LoginCron), scheduleSummary(a.ChatEnabled, a.ChatCron), scheduleSummary(a.PCEnabled, a.PCCron), a.ID, a.ID, a.ID, a.ID)
	}
	b.WriteString(`</tbody></table></div></article>`)
	return b.String()
}
func (s *Server) accountsPartial(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	content := strings.ReplaceAll(s.accountTable(), "{{CSRF}}", esc(s.csrf(w, r)))
	_, _ = io.WriteString(w, content)
}
func mask(v string) string {
	if len(v) < 6 {
		return v[:min(1, len(v))] + "***"
	}
	return v[:3] + "****" + v[len(v)-3:]
}
func (s *Server) accountNew(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	s.accountForm(w, r, storage.Account{Enabled: true, KeepaliveEnabled: true, KeepaliveMode: storage.KeepaliveAlways, KeepaliveStart: "08:00", KeepaliveEnd: "23:00", KeepaliveWeekdays: "1,2,3,4,5,6,7", LoginEnabled: true, LoginCron: "0 3 * * *", PCEnabled: true, PCCron: "5 3 * * *", ChatEnabled: true, ChatCron: "10 3 * * *", DeviceCode: security.RandomToken(24)})
}

func scheduleSummary(enabled bool, expression string) string {
	if !enabled {
		return `<span class="pill warning">关闭</span>`
	}
	return `<span class="pill success">启用</span><small>` + esc(expression) + `</small>`
}

func keepaliveModeLabel(a storage.Account) (string, string) {
	if !a.Enabled {
		return "账号停用", "warning"
	}
	switch a.EffectiveKeepaliveMode() {
	case storage.KeepaliveAlways:
		return "全天", "success"
	case storage.KeepaliveScheduled:
		return "定时", "success"
	default:
		return "关闭", "warning"
	}
}

func keepaliveForm(a storage.Account) string {
	mode := a.EffectiveKeepaliveMode()
	if a.KeepaliveStart == "" {
		a.KeepaliveStart = "08:00"
	}
	if a.KeepaliveEnd == "" {
		a.KeepaliveEnd = "23:00"
	}
	if a.KeepaliveWeekdays == "" {
		a.KeepaliveWeekdays = "1,2,3,4,5,6,7"
	}
	days := map[string]bool{}
	for _, day := range strings.Split(a.KeepaliveWeekdays, ",") {
		days[strings.TrimSpace(day)] = true
	}
	labels := []string{"周一", "周二", "周三", "周四", "周五", "周六", "周日"}
	var weekdays strings.Builder
	for i, label := range labels {
		fmt.Fprintf(&weekdays, `<label class=weekday-option><input type=checkbox name=keepalive_weekdays value="%d"%s><span>%s</span></label>`, i+1, checked(days[strconv.Itoa(i+1)]), label)
	}
	return fmt.Sprintf(`<div class=keepalive-config><label>保活模式<select name=keepalive_mode data-keepalive-mode><option value=off%s>关闭</option><option value=always%s>全天保活</option><option value=scheduled%s>定时保活</option></select></label><div class=keepalive-period data-keepalive-period%s><div class=form-grid><label>开始时间<input type=time name=keepalive_start value="%s"></label><label>结束时间<input type=time name=keepalive_end value="%s"></label></div><div><span class=form-label>运行日期</span><div class=weekday-options>%s</div></div></div><p class=muted data-keepalive-hint=off%s>关闭后不会建立常驻保活连接；积分任务仍按各自计划执行，时长任务需要时会临时连接。</p><p class=muted data-keepalive-hint=always%s>启用后将进行 24 小时持续保活，连接异常时会自动恢复。</p><p class=muted data-keepalive-hint=scheduled%s>仅在所选日期和时间段建立常驻保活连接；跨天时间段按开始日计算，时长任务需要时仍可临时连接。</p></div>`, selected(mode == storage.KeepaliveOff), selected(mode == storage.KeepaliveAlways), selected(mode == storage.KeepaliveScheduled), map[bool]string{true: "", false: " hidden"}[mode == storage.KeepaliveScheduled], esc(a.KeepaliveStart), esc(a.KeepaliveEnd), weekdays.String(), map[bool]string{true: "", false: " hidden"}[mode == storage.KeepaliveOff], map[bool]string{true: "", false: " hidden"}[mode == storage.KeepaliveAlways], map[bool]string{true: "", false: " hidden"}[mode == storage.KeepaliveScheduled])
}

func validateKeepaliveSettings(a storage.Account) error {
	switch a.KeepaliveMode {
	case storage.KeepaliveOff, storage.KeepaliveAlways:
		return nil
	case storage.KeepaliveScheduled:
		start, startErr := time.Parse("15:04", a.KeepaliveStart)
		end, endErr := time.Parse("15:04", a.KeepaliveEnd)
		if startErr != nil || endErr != nil {
			return fmt.Errorf("定时保活的开始和结束时间格式不正确")
		}
		if start.Hour() == end.Hour() && start.Minute() == end.Minute() {
			return fmt.Errorf("定时保活的开始时间和结束时间不能相同")
		}
		if strings.TrimSpace(a.KeepaliveWeekdays) == "" {
			return fmt.Errorf("定时保活至少需要选择一个运行日期")
		}
		seen := map[int]bool{}
		for _, raw := range strings.Split(a.KeepaliveWeekdays, ",") {
			day, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || day < 1 || day > 7 {
				return fmt.Errorf("定时保活的运行日期无效")
			}
			seen[day] = true
		}
		if len(seen) == 0 {
			return fmt.Errorf("定时保活至少需要选择一个运行日期")
		}
		return nil
	default:
		return fmt.Errorf("保活模式无效")
	}
}
func (s *Server) accountForm(w http.ResponseWriter, r *http.Request, a storage.Account) {
	title := "添加账号"
	required := " required"
	if a.ID > 0 {
		title = "编辑账号"
		required = ""
	}
	content := fmt.Sprintf(`<header class=page-head><div><p class=eyebrow>账号配置</p><h1>%s</h1></div><a class="secondary button" href=/accounts>返回</a></header><form method=post action=/accounts/save class="panel form-panel"><input type=hidden name=csrf_token value="{{CSRF}}"><input type=hidden name=account_id value="%d"><fieldset><legend>登录信息</legend><div class=form-grid><label>显示名称<input name=name value="%s" required></label><label>天翼云账号<input name=username value="%s" required></label><label>密码<div class=password-field><input name=password type=password%s placeholder="%s">%s</div></label><label>设备码<input name=device_code value="%s" required></label></div><label class=switch-row><input type=checkbox name=enabled%s><span>启用账号</span></label></fieldset><fieldset><legend>云电脑保活</legend>%s</fieldset><fieldset><legend>每日积分任务</legend><div class=schedule-box><label class=switch-row><input type=checkbox name=login_enabled%s><span>启用登录任务</span></label><label>Cron 计划<input name=login_cron value="%s" required></label></div><div class=schedule-box><label class=switch-row><input type=checkbox name=pc_enabled%s><span>启用时长任务</span></label><label>Cron 计划<input name=pc_cron value="%s" required></label></div><div class=schedule-box><label class=switch-row><input type=checkbox name=chat_enabled%s><span>启用 AI 对话任务</span></label><label>Cron 计划<input name=chat_cron value="%s" required></label></div></fieldset><div class=form-actions><a href=/accounts>取消</a><button class=primary>保存并检查设备</button></div></form>`, title, a.ID, esc(a.Name), esc(a.Username), required, map[bool]string{true: "留空表示不修改", false: "请输入密码"}[a.ID > 0], passwordToggle(), esc(a.DeviceCode), checked(a.Enabled), keepaliveForm(a), checked(a.LoginEnabled), esc(a.LoginCron), checked(a.PCEnabled), esc(a.PCCron), checked(a.ChatEnabled), esc(a.ChatCron))
	s.page(w, r, title, content, true)
}
func (s *Server) saveAccount(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || r.Method != "POST" {
		return
	}
	_ = r.ParseForm()
	if !s.checkCSRF(r) {
		http.Error(w, "CSRF validation failed", 403)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
	mode := strings.TrimSpace(r.FormValue("keepalive_mode"))
	a := storage.Account{ID: id, Name: strings.TrimSpace(r.FormValue("name")), Username: strings.TrimSpace(r.FormValue("username")), DeviceCode: strings.TrimSpace(r.FormValue("device_code")), Enabled: r.Form.Has("enabled"), KeepaliveEnabled: mode != storage.KeepaliveOff, KeepaliveMode: mode, KeepaliveStart: strings.TrimSpace(r.FormValue("keepalive_start")), KeepaliveEnd: strings.TrimSpace(r.FormValue("keepalive_end")), KeepaliveWeekdays: strings.Join(r.Form["keepalive_weekdays"], ","), LoginEnabled: r.Form.Has("login_enabled"), LoginCron: strings.TrimSpace(r.FormValue("login_cron")), ChatEnabled: r.Form.Has("chat_enabled"), ChatCron: strings.TrimSpace(r.FormValue("chat_cron")), PCEnabled: r.Form.Has("pc_enabled"), PCCron: strings.TrimSpace(r.FormValue("pc_cron"))}
	if e := validateKeepaliveSettings(a); e != nil {
		redirect(w, r, "/accounts", e.Error(), true)
		return
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	for _, schedule := range []struct{ label, expression string }{{"登录任务", a.LoginCron}, {"时长任务", a.PCCron}, {"AI 对话任务", a.ChatCron}} {
		if _, parseErr := parser.Parse(schedule.expression); parseErr != nil {
			redirect(w, r, "/accounts", schedule.label+"的 Cron 计划格式不正确", true)
			return
		}
	}
	saved, e := s.store.SaveAccount(a, r.FormValue("password"), s.credentialKey, security.EncryptFernet)
	if e != nil {
		redirect(w, r, "/accounts", e.Error(), true)
		return
	}
	s.store.ClearAuthCache(saved)
	s.manager.ClearNativeAuth(saved)
	if !a.Enabled {
		s.manager.RestartKeepalive()
		redirect(w, r, "/accounts", "账号已保存（当前停用）", false)
		return
	}
	bound, e := s.manager.BeginVerification(r.Context(), saved)
	if e != nil {
		redirect(w, r, "/accounts", "账号已保存，自动检查失败："+e.Error(), true)
		return
	}
	if bound {
		s.manager.RestartKeepalive()
		message := "账号已保存，积分任务计划已生效"
		if a.EffectiveKeepaliveMode() != storage.KeepaliveOff {
			message = "账号已保存，持续保活和积分任务计划已生效"
		}
		redirect(w, r, "/accounts", message, false)
	} else {
		redirect(w, r, fmt.Sprintf("/accounts/%d/device-verification", saved), "短信验证码已发送", false)
	}
}

func (s *Server) accountRoute(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		return
	}
	id, _ := strconv.ParseInt(parts[1], 10, 64)
	if len(parts) == 3 && parts[2] == "edit" {
		a, e := s.store.Account(id)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		s.accountForm(w, r, a)
		return
	}
	if len(parts) >= 3 && parts[2] == "delete" && r.Method == "POST" {
		if !s.checkCSRF(r) {
			http.Error(w, "Forbidden", 403)
			return
		}
		_ = s.store.DeleteAccount(id)
		s.manager.ClearNativeAuth(id)
		s.manager.RestartKeepalive()
		redirect(w, r, "/accounts", "账号已删除", false)
		return
	}
	if len(parts) >= 4 && parts[2] == "device-verification" {
		if parts[3] == "start" && r.Method == "POST" {
			if !s.checkCSRF(r) {
				http.Error(w, "Forbidden", 403)
				return
			}
			bound, e := s.manager.BeginVerification(r.Context(), id)
			if e != nil {
				redirect(w, r, "/accounts", e.Error(), true)
			} else if bound {
				s.manager.RestartKeepalive()
				redirect(w, r, "/accounts", "设备已经绑定", false)
			} else {
				http.Redirect(w, r, fmt.Sprintf("/accounts/%d/device-verification", id), 303)
			}
			return
		}
		if parts[3] == "complete" && r.Method == "POST" {
			if !s.checkCSRF(r) {
				http.Error(w, "Forbidden", 403)
				return
			}
			e := s.manager.CompleteVerification(r.Context(), id, r.FormValue("verification_code"))
			if e != nil {
				redirect(w, r, fmt.Sprintf("/accounts/%d/device-verification", id), e.Error(), true)
			} else {
				redirect(w, r, "/accounts", "设备绑定成功并已开始保活", false)
			}
			return
		}
	}
	if len(parts) == 3 && parts[2] == "device-verification" {
		a, _ := s.store.Account(id)
		content := fmt.Sprintf(`<header class=page-head><div><p class=eyebrow>安全验证</p><h1>绑定设备</h1></div></header><form class="panel form-panel" method=post action="/accounts/%d/device-verification/complete"><input type=hidden name=csrf_token value="{{CSRF}}"><p>验证码已发送到账号 %s，请在 10 分钟内完成。</p><label>短信验证码<input name=verification_code inputmode=numeric minlength=4 maxlength=8 required autofocus></label><button class=primary>完成验证</button></form>`, id, esc(mask(a.Username)))
		s.page(w, r, "设备验证", content, true)
		return
	}
	if len(parts) >= 4 && parts[2] == "tasks" && r.Method == "POST" {
		if !s.checkCSRF(r) {
			http.Error(w, "Forbidden", 403)
			return
		}
		run, e := s.manager.StartTask(id, parts[3], "manual")
		if e != nil {
			redirect(w, r, "/tasks", e.Error(), true)
		} else {
			redirect(w, r, "/tasks", fmt.Sprintf("任务已启动，编号 #%d", run), false)
		}
		return
	}
	if len(parts) >= 3 && parts[2] == "platform-status" && r.Method == "POST" {
		if !s.checkCSRF(r) {
			http.Error(w, "Forbidden", 403)
			return
		}
		v, e := s.manager.RefreshStatus(r.Context(), id)
		if e != nil {
			redirect(w, r, "/tasks", "平台状态查询失败："+e.Error(), true)
		} else {
			redirect(w, r, "/tasks", fmt.Sprintf("状态已更新，总积分 %d", *v.TotalPoints), false)
		}
		return
	}
	if len(parts) >= 3 && parts[2] == "redeem" {
		s.redeem(w, r, id, parts)
		return
	}
	http.NotFound(w, r)
}

func initial(v string) string {
	r := []rune(v)
	if len(r) == 0 {
		return "云"
	}
	return string(r[0])
}
func (s *Server) taskCards(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	accounts, _ := s.store.Accounts()
	platforms, _ := s.store.Platforms()
	now := time.Now()
	var b strings.Builder
	for _, a := range accounts {
		p := platforms[a.ID]
		fresh := platformStatusUpdatedToday(p.UpdatedAt, now)
		points := "--"
		if p.TotalPoints != nil {
			points = strconv.Itoa(*p.TotalPoints)
		}
		pointsLabel := "当前总积分"
		updatedClass := ""
		updatedText := "今日尚未查询"
		if p.UpdatedAt != "" && fresh {
			updatedText = "今日更新于 " + formatTime(p.UpdatedAt)
		} else if p.UpdatedAt != "" {
			pointsLabel = "最近总积分"
			updatedClass = " stale"
			updatedText = "上次查询于 " + formatTime(p.UpdatedAt) + " · 今日待更新"
		}
		accountState := map[bool]string{true: "success", false: "stopped"}[a.Enabled]
		fmt.Fprintf(&b, `<article class="panel launch-card platform-card"><div class=platform-card-head><div class=platform-account><span class=account-avatar>%s</span><span><strong>%s</strong><small><i class="account-state-dot %s"></i>%s</small></span></div><div class=points-summary><small>%s</small><strong>%s</strong></div></div><div class=platform-task-grid>`, esc(initial(a.Name)), esc(a.Name), accountState, map[bool]string{true: "账号启用", false: "账号停用"}[a.Enabled], pointsLabel, points)
		for _, x := range []struct{ k, n string }{{"login", "登录 AI 云电脑"}, {"usage", "使用 1 小时"}, {"chat", "AI 对话"}} {
			t, ok := p.Tasks[x.k]
			if !fresh {
				t = storage.TaskStatus{State: "stale", StateLabel: "今日未查询", Total: map[string]int{"usage": 3600}[x.k]}
				if t.Total == 0 {
					t.Total = 1
				}
			} else if !ok {
				t = storage.TaskStatus{State: "warning", StateLabel: "待查询"}
			}
			progress := 0
			if t.Total > 0 {
				progress = min(100, max(0, t.Current*100/t.Total))
			}
			fmt.Fprintf(&b, `<div class="platform-task %s"><div class=platform-task-top><span class=platform-task-name>%s</span><span class="pill %s">%s</span></div><div class=platform-progress><span style="width:%d%%"></span></div><small>进度 %d/%d</small></div>`, esc(t.State), x.n, esc(t.State), esc(t.StateLabel), progress, t.Current, t.Total)
		}
		fmt.Fprintf(&b, `</div><div class="platform-updated%s"><span class="material-symbols-rounded">schedule</span>%s</div><div class=launch-actions><form method=post action="/accounts/%d/tasks/login"><input type=hidden name=csrf_token value="{{CSRF}}"><button class=secondary%s>%s<span>登陆任务</span></button></form><form method=post action="/accounts/%d/tasks/pc"><input type=hidden name=csrf_token value="{{CSRF}}"><button class=primary%s>%s<span>时长任务</span></button></form><form method=post action="/accounts/%d/tasks/chat"><input type=hidden name=csrf_token value="{{CSRF}}"><button class=secondary%s>%s<span>AI对话任务</span></button></form><form method=post action="/accounts/%d/platform-status/refresh"><input type=hidden name=csrf_token value="{{CSRF}}"><button class=secondary%s>%s<span>任务状态查询</span></button></form></div></article>`, updatedClass, esc(updatedText), a.ID, disabled(!a.Enabled), buttonIcon("login"), a.ID, disabled(!a.Enabled), buttonIcon("usage"), a.ID, disabled(!a.Enabled), buttonIcon("chat"), a.ID, disabled(!a.Enabled), buttonIcon("status"))
	}
	if len(accounts) == 0 {
		b.WriteString(`<article class="panel task-empty-card"><span class="material-symbols-rounded">manage_accounts</span><div class=task-empty-copy><strong>还没有可运行的账号</strong><p>添加账号后，即可在这里查看每日任务状态并快速执行任务。</p></div><a class="secondary button" href=/accounts/new>添加账号</a></article>`)
	}
	fmt.Fprint(w, strings.ReplaceAll(b.String(), "{{CSRF}}", esc(s.csrf(w, r))))
}
func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	token := s.csrf(w, r)
	r.AddCookie(&http.Cookie{Name: "ctyun_csrf", Value: security.SignCookie(s.sessionKey, token), Path: "/"})
	content := `<header class=page-head><div><p class=eyebrow>自动化</p><h1>任务中心</h1><p class=page-subtitle>按账号查看三项平台任务进度，并独立执行登录、对话与挂机。</p></div></header><section class=task-launch-grid id=task-account-status hx-get=/partials/task-accounts hx-trigger='every 10s' hx-swap=innerHTML>`
	var recorder strings.Builder
	rw := responseWriter{&recorder, http.Header{}}
	s.taskCards(&rw, r)
	content += recorder.String() + `</section>`
	runs, _ := s.store.Runs(100)
	content += `<article class="panel table-panel task-history-panel"><div class=panel-head><div class=panel-title><span class="panel-icon material-symbols-rounded">history</span><div><p class=eyebrow>历史记录</p><h2>任务运行记录</h2></div></div></div><div class=table-wrap><table><thead><tr><th>编号</th><th>任务</th><th>账号</th><th>状态</th><th>开始时间</th><th>操作</th></tr></thead><tbody>`
	for _, v := range runs {
		label := taskLabel(v.TaskType)
		if label == "" {
			label = v.TaskType
		}
		content += fmt.Sprintf(`<tr><td data-label="编号">#%d</td><td data-label="任务">%s</td><td data-label="账号">%s</td><td data-label="状态"><span class="pill %s">%s</span></td><td data-label="开始时间"><time>%s</time></td><td data-label="操作" class=actions><a href="/logs?run_id=%d">查看日志</a>`, v.ID, esc(label), esc(v.AccountName), esc(v.Status), esc(statusLabel(v.Status)), esc(formatTime(v.StartedAt)), v.ID)
		if v.Status == "running" || v.Status == "queued" {
			content += fmt.Sprintf(`<form method=post action="/tasks/%d/stop"><input type=hidden name=csrf_token value="{{CSRF}}"><button class=danger-link>停止</button></form>`, v.ID)
		}
		content += `</td></tr>`
	}
	content += `</tbody></table></div></article>`
	s.page(w, r, "任务", content, true)
}

type responseWriter struct {
	io.Writer
	h http.Header
}

func (r *responseWriter) Header() http.Header { return r.h }
func (r *responseWriter) WriteHeader(int)     {}
func (s *Server) taskRoute(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 3 && parts[2] == "stop" && r.Method == "POST" {
		if !s.checkCSRF(r) {
			http.Error(w, "Forbidden", 403)
			return
		}
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		if s.manager.StopTask(id) {
			redirect(w, r, "/tasks", "任务已停止", false)
		} else {
			redirect(w, r, "/tasks", "任务当前无法停止", true)
		}
		return
	}
	http.NotFound(w, r)
}

func (s *Server) redeem(w http.ResponseWriter, r *http.Request, id int64, parts []string) {
	if r.Method == "POST" {
		if !s.checkCSRF(r) {
			http.Error(w, "Forbidden", 403)
			return
		}
		if len(parts) > 3 && parts[3] == "resolve" {
			if e := s.manager.ResolveRedeem(id, r.FormValue("succeeded") == "1"); e != nil {
				redirect(w, r, fmt.Sprintf("/accounts/%d/redeem", id), e.Error(), true)
				return
			}
			redirect(w, r, fmt.Sprintf("/accounts/%d/redeem", id), "待确认订单状态已处理", false)
			return
		}
		cfg := storage.RedeemConfig{AccountID: id, Enabled: r.Form.Has("enabled"), ProductID: r.FormValue("product_id"), DesktopID: r.FormValue("desktop_id"), MaxTimes: security.Int(r.FormValue("max_times")), ScheduleType: r.FormValue("schedule_type"), IntervalDays: security.Int(r.FormValue("interval_days")), MonthlyDays: r.FormValue("monthly_days")}
		immediate := r.FormValue("action") == "redeem"
		var e error
		if immediate {
			cfg, e = s.manager.ValidateImmediateRedeem(r.Context(), id, cfg)
		} else {
			cfg, e = s.manager.ValidateRedeemConfig(r.Context(), id, cfg)
		}
		if e != nil {
			redirect(w, r, r.URL.Path, e.Error(), true)
			return
		}
		if e = s.store.SaveRedeem(cfg); e != nil {
			redirect(w, r, r.URL.Path, e.Error(), true)
			return
		}
		if immediate {
			run, startErr := s.manager.StartTask(id, "redeem", "manual")
			if startErr != nil {
				redirect(w, r, r.URL.Path, startErr.Error(), true)
			} else {
				redirect(w, r, r.URL.Path, fmt.Sprintf("兑换任务已启动，编号 #%d", run), false)
			}
			return
		}
		redirect(w, r, r.URL.Path, "自动兑换配置已保存", false)
		return
	}
	a, e := s.store.Account(id)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	cfg, _ := s.store.Redeem(id)
	var pending string
	_ = s.store.DB.QueryRow("SELECT last_attempt_status FROM redeem_states WHERE account_id=?", id).Scan(&pending)
	rewards, desktops, e := s.manager.RedeemCatalog(r.Context(), id)
	warning := ""
	if e != nil {
		warning = `<div class="form-error">无法读取实时目录：` + esc(e.Error()) + `</div>`
	}
	points, pointsErr := s.manager.AccountPoints(r.Context(), id)
	pointsSummary := fmt.Sprintf(`<div class="points-summary redeem-points"><small>当前积分</small><strong>%d</strong></div>`, points)
	pointsWarning := ""
	if pointsErr != nil {
		pointsSummary = `<div class="points-summary redeem-points unavailable"><small>当前积分</small><strong>查询失败</strong></div>`
		pointsWarning = `<div class="form-error">无法读取当前积分：` + esc(pointsErr.Error()) + `</div>`
	}
	var productOptions strings.Builder
	for _, p := range rewards {
		sel := ""
		if strconv.FormatInt(p.ProductID, 10) == cfg.ProductID {
			sel = " selected"
		}
		fmt.Fprintf(&productOptions, `<option value="%d" data-name="%s" data-type="%s" data-cost="%d"%s>%s（%d 积分）</option>`, p.ProductID, esc(p.ProductName), esc(p.ProductType), p.CostPoints, sel, esc(p.ProductName), p.CostPoints)
	}
	var desktopOptions strings.Builder
	for _, d := range desktops {
		sel := ""
		if d.ID() == cfg.DesktopID {
			sel = " selected"
		}
		fmt.Fprintf(&desktopOptions, `<option value="%s"%s>%s</option>`, esc(d.ID()), sel, esc(d.Name()))
	}
	pendingPanel := ""
	if pending == "pending" {
		pendingPanel = fmt.Sprintf(`<article class="panel form-panel"><h2>上一笔订单待确认</h2><p>为避免重复扣除积分，自动兑换已暂停。请在平台核对订单后选择结果。</p><div class=form-actions><form method=post action="/accounts/%d/redeem/resolve"><input type=hidden name=csrf_token value="{{CSRF}}"><input type=hidden name=succeeded value=1><button class=primary>确认已成功</button></form><form method=post action="/accounts/%d/redeem/resolve"><input type=hidden name=csrf_token value="{{CSRF}}"><input type=hidden name=succeeded value=0><button class=secondary>确认未成功</button></form></div></article>`, id, id)
	}
	content := fmt.Sprintf(`<header class=page-head><div><p class=eyebrow>积分奖励</p><h1>%s · 积分兑换</h1></div><div class=form-actions><a class="secondary button" href=/accounts>返回</a></div></header>%s%s<form class="panel form-panel" method=post><input type=hidden name=csrf_token value="{{CSRF}}"><div class="panel-head redeem-panel-head"><div><p class=eyebrow>兑换设置</p><h2>设置积分兑换</h2></div>%s</div>%s<label class=switch-row><input type=checkbox name=enabled%s><span>启用自动兑换（仅控制按计划自动执行）</span></label><div class=form-grid><label>奖励商品<select name=product_id required>%s</select></label><label>目标云电脑<select name=desktop_id>%s</select><small>数据盘和规格升配商品必须选择；普通权益由平台绑定当前账号。</small></label><label>单次最多兑换次数<input type=number name=max_times min=1 value="%d"></label><label>计划<select name=schedule_type><option value=daily%s>每天检查</option><option value=interval%s>按间隔天数</option><option value=monthly%s>指定每月日期</option></select></label><label>间隔天数<input type=number name=interval_days min=1 value="%d"></label><label>每月日期<input name=monthly_days value="%s" placeholder="1,15,28；-1 表示月末"></label></div><p class=muted>立即兑换不受自动兑换开关和计划日期限制。提交订单前会重新校验商品价格、状态、有效期、积分和云电脑归属；平台返回 code=0 即记为成功，积分和兑换统计仅用于辅助核对。网络中断等无法取得明确平台结果时才进入待人工确认；检测到风控后会自动关闭后续自动兑换。</p><div class=form-actions><button class=primary type=submit>保存配置</button><button class=secondary type=submit name=action value=redeem>立即兑换</button></div></form>`, esc(a.Name), warning, pendingPanel, pointsSummary, pointsWarning, checked(cfg.Enabled), productOptions.String(), desktopOptions.String(), cfg.MaxTimes, selected(cfg.ScheduleType == "daily"), selected(cfg.ScheduleType == "interval"), selected(cfg.ScheduleType == "monthly"), cfg.IntervalDays, esc(cfg.MonthlyDays))
	s.page(w, r, "自动兑换", content, true)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	runs, _ := s.store.Runs(200)
	path := filepath.Join(s.dataDir, "logs", "ctyun.log")
	selectedID := r.URL.Query().Get("run_id")
	var selected storage.Run
	if selectedID != "" {
		for _, run := range runs {
			if strconv.FormatInt(run.ID, 10) == selectedID {
				selected = run
				path = run.LogPath
				break
			}
		}
	}
	raw, _ := os.ReadFile(path)
	fileSize := int64(len(raw))
	if len(raw) > 256<<10 {
		raw = raw[len(raw)-(256<<10):]
	}
	if len(raw) == 0 {
		raw = []byte("暂无日志输出。")
	} else {
		raw = []byte(reverseLogText(string(raw)))
	}
	logTitle := "运行日志"
	logMeta := "CtYunKeeper 服务与保活状态"
	indicator := `<span class="live-indicator"><i></i>实时输出</span>`
	attrs := ` id="log-output" data-stream="/logs/system/stream" data-offset="` + strconv.FormatInt(fileSize, 10) + `"`
	if selected.ID != 0 {
		label := taskLabel(selected.TaskType)
		if label == "" {
			label = selected.TaskType
		}
		logTitle = fmt.Sprintf("#%d · %s", selected.ID, label)
		logMeta = fmt.Sprintf("%s · %s · %s", selected.AccountName, statusLabel(selected.Status), formatTime(selected.StartedAt))
		attrs = ` id="log-output" data-stream="/logs/` + strconv.FormatInt(selected.ID, 10) + `/stream" data-offset="` + strconv.FormatInt(fileSize, 10) + `"`
		indicator = `<span class="live-indicator"><i></i>实时输出</span>`
	}
	clearID := "0"
	if selected.ID != 0 {
		clearID = strconv.FormatInt(selected.ID, 10)
	}
	clearAction := `<div class=log-panel-actions><form method=post action=/logs/clear data-confirm="确认清空当前日志？清空后无法恢复。"><input type=hidden name=csrf_token value="{{CSRF}}"><input type=hidden name=run_id value="` + clearID + `"><button class="icon-button log-clear-button" type=submit title="清空当前日志" aria-label="清空当前日志"><img src=/static/delete-sweep.svg alt=""></button></form>` + indicator + `</div>`
	content := `<header class=page-head><div><p class=eyebrow>诊断</p><h1>日志中心</h1><p class=page-subtitle>集中查看系统运行日志和每次自动化任务的独立日志。</p></div></header><section class=log-layout>` + s.logSources(runs, selected.ID) + `<article class="panel log-panel"><div class=log-panel-head><div><h2>` + esc(logTitle) + `</h2><p>` + esc(logMeta) + `</p></div>` + clearAction + `</div><pre class=log-output` + attrs + `>` + esc(string(raw)) + `</pre></article></section>`
	s.page(w, r, "日志", content, true)
}
func (s *Server) clearLog(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || r.Method != http.MethodPost {
		return
	}
	if !s.checkCSRF(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	runID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("run_id")), 10, 64)
	if err != nil || runID < 0 {
		redirect(w, r, "/logs", "日志类型无效", true)
		return
	}
	if err := s.manager.ClearLog(runID); err != nil {
		redirect(w, r, "/logs", "清空日志失败："+err.Error(), true)
		return
	}
	target := "/logs"
	if runID > 0 {
		target += "?run_id=" + strconv.FormatInt(runID, 10)
	}
	redirect(w, r, target, "当前日志已清空", false)
}
func (s *Server) logSources(runs []storage.Run, selectedID int64) string {
	var sources strings.Builder
	partialURL := "/partials/log-sources"
	if selectedID != 0 {
		partialURL += "?run_id=" + strconv.FormatInt(selectedID, 10)
	}
	systemActive := "active"
	if selectedID != 0 {
		systemActive = ""
	}
	fmt.Fprintf(&sources, `<aside id=log-sources class="panel log-sources" hx-get="%s" hx-trigger="every 5s" hx-swap=outerHTML><h2>系统日志</h2><a class="%s" href="/logs"><strong>运行日志</strong><small>CtYunKeeper 服务输出</small></a><h2>任务日志</h2>`, partialURL, systemActive)
	if len(runs) == 0 {
		sources.WriteString(`<p class=log-empty>还没有任务记录</p>`)
	}
	for _, run := range runs {
		active := ""
		if run.ID == selectedID {
			active = "active"
		}
		label := taskLabel(run.TaskType)
		if label == "" {
			label = run.TaskType
		}
		fmt.Fprintf(&sources, `<a class="%s" href="/logs?run_id=%d"><span class=log-source-title><strong>#%d · %s</strong><span class="status-dot %s"></span></span><small>%s · %s</small></a>`, active, run.ID, run.ID, esc(label), esc(run.Status), esc(run.AccountName), esc(formatTime(run.StartedAt)))
	}
	sources.WriteString(`</aside>`)
	return sources.String()
}
func (s *Server) logSourcesPartial(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	selectedID, _ := strconv.ParseInt(r.URL.Query().Get("run_id"), 10, 64)
	runs, _ := s.store.Runs(200)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, s.logSources(runs, selectedID))
}
func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[2] != "stream" {
		http.NotFound(w, r)
		return
	}
	id, _ := strconv.ParseInt(parts[1], 10, 64)
	run := storage.Run{}
	path := filepath.Join(s.dataDir, "logs", "ctyun.log")
	systemLog := parts[1] == "system"
	if !systemLog {
		runs, _ := s.store.Runs(500)
		for _, v := range runs {
			if v.ID == id {
				run = v
				path = v.LogPath
				break
			}
		}
		if run.ID == 0 {
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if offset < 0 {
		offset = 0
	}
	for {
		f, e := os.Open(path)
		if e == nil {
			if info, statErr := f.Stat(); statErr == nil && info.Size() < offset {
				offset = 0
				fmt.Fprint(w, "event: reset\ndata: true\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
			_, _ = f.Seek(offset, io.SeekStart)
			raw, _ := io.ReadAll(f)
			offset, _ = f.Seek(0, io.SeekCurrent)
			f.Close()
			if len(raw) > 0 {
				fmt.Fprintf(w, "data: %q\n\n", reverseLogText(string(raw)))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if !systemLog {
			runs, _ := s.store.Runs(500)
			active := false
			for _, v := range runs {
				if v.ID == id && (v.Status == "running" || v.Status == "queued") {
					active = true
					break
				}
			}
			if !active {
				fmt.Fprint(w, "event: done\ndata: true\n\n")
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
}
func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || r.Method != "POST" {
		return
	}
	if !s.checkCSRF(r) {
		http.Error(w, "Forbidden", 403)
		return
	}
	s.manager.RestartKeepalive()
	redirect(w, r, "/", "保活服务已重新加载", false)
}
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	authEnabled := s.passwordAuthEnabled()
	authIcon := "icon-lock"
	authSwitchClass := " active"
	authSwitchChecked := "true"
	authTarget := "false"
	authLabel := "密码登录已开启"
	if !authEnabled {
		authIcon = "icon-lock-open"
		authSwitchClass = ""
		authSwitchChecked = "false"
		authTarget = "true"
		authLabel = "密码登录已关闭"
	}
	authToggle := fmt.Sprintf(`<form method=post action=/settings/auth class=settings-auth-toggle-form><input type=hidden name=csrf_token value="{{CSRF}}"><input type=hidden name=auth_enabled value="%s"><button class=auth-toggle-button type=submit role=switch aria-checked="%s" aria-label="切换管理面板密码登录"><strong>%s</strong><span class="auth-switch-control%s" aria-hidden=true><i></i></span></button></form>`, authTarget, authSwitchChecked, authLabel, authSwitchClass)
	securityCard := fmt.Sprintf(`<article class="panel form-panel settings-card password-settings-card"><div class="settings-card-head password-settings-head"><span class="panel-icon"><span class="local-icon %s" aria-hidden="true"></span></span><div><h2>访问安全</h2><small>管理面板登录方式与访问密码</small></div>%s</div><form method=post action=/settings/password class=password-change-form><input type=hidden name=csrf_token value="{{CSRF}}"><div class=settings-fields><label>当前密码<div class=password-field><input name=current_password type=password required autocomplete=current-password>%s</div></label><label>新密码<div class=password-field><input name=password type=password minlength=8 required autocomplete=new-password>%s</div></label><label>确认新密码<div class=password-field><input name=confirmation type=password minlength=8 required autocomplete=new-password>%s</div></label></div><p class=settings-hint>新密码至少需要 8 位字符</p><button class="primary settings-primary-action">更新密码</button></form></article>`, authIcon, authToggle, passwordToggle(), passwordToggle(), passwordToggle())
	logCard := fmt.Sprintf(`<article class="panel form-panel settings-card log-settings-panel"><div class="settings-card-head log-settings-head"><span class="panel-icon neutral material-symbols-rounded">history</span><div><h2>日志管理</h2><small>设置应用日志的保留与清理方式</small></div></div><div class=retention-form><div class=retention-controls><form class=retention-save-form method=post action=/settings/logs><input type=hidden name=csrf_token value="{{CSRF}}"><label>日志保留天数<div class=number-field><input name=retention_days type=number min=1 max=3650 value="%d" required><span>天</span></div></label><button class=primary>保存日志设置</button></form><form class=retention-clear-form method=post action=/settings/logs/clear data-confirm="确认清空全部日志？该操作无法恢复。"><input type=hidden name=csrf_token value="{{CSRF}}"><button class=danger-button type=submit><img src=/static/delete-forever.svg alt=""><span>清空全部日志</span></button></form></div><p class=retention-note>每天自动清理超过保留期限的系统日志和已结束任务日志，正在执行的任务不会被删除。</p></div></article>`, s.manager.LogRetentionDays())
	content := fmt.Sprintf(`<header class=page-head><div><p class=eyebrow>系统</p><h1>系统设置</h1><p class=page-subtitle>管理访问安全、运行环境与日志存储策略。</p></div></header><section class=settings-grid>%s%s%s</section>`, securityCard, s.deploymentCard(updateMessagesFromRequest(r)...), logCard)
	s.page(w, r, "设置", content, true)
}

type updateState struct {
	LatestVersion       string `json:"latestVersion"`
	LatestTag           string `json:"latestTag"`
	HasUpdate           bool   `json:"hasUpdate"`
	Supported           bool   `json:"supported"`
	CurrentIsDev        bool   `json:"currentIsDev"`
	NewerThanLatest     bool   `json:"newerThanLatest"`
	Message             string `json:"message"`
	CheckedAt           string `json:"checkedAt"`
	PublishedAt         string `json:"publishedAt"`
	ReleaseURL          string `json:"releaseUrl"`
	Notes               string `json:"notes"`
	RequiresImageUpdate bool   `json:"requiresImageUpdate"`
}

type updateCardMessage struct {
	Level string
	Text  string
}

func updateMessagesFromRequest(r *http.Request) []updateCardMessage {
	query := r.URL.Query()
	texts := query["update_message"]
	levels := query["update_level"]
	messages := make([]updateCardMessage, 0, len(texts))
	for index, text := range texts {
		level := "neutral"
		if index < len(levels) {
			level = levels[index]
		}
		messages = append(messages, updateCardMessage{Level: level, Text: text})
	}
	return messages
}

func redirectUpdate(w http.ResponseWriter, r *http.Request, message string, isErr bool) {
	level := "success"
	if isErr {
		level = "error"
	}
	redirectUpdateLevel(w, r, message, level)
}

func redirectUpdateLevel(w http.ResponseWriter, r *http.Request, message, level string) {
	query := url.Values{}
	query.Add("update_message", message)
	query.Add("update_level", level)
	http.Redirect(w, r, "/settings?"+query.Encode(), http.StatusSeeOther)
}

func updateStateFromResult(r *update.CheckResult) updateState {
	return updateState{
		LatestVersion:       r.LatestVersion,
		LatestTag:           r.LatestTag,
		HasUpdate:           r.HasUpdate,
		Supported:           r.Supported,
		CurrentIsDev:        r.CurrentIsDev,
		NewerThanLatest:     r.NewerThanLatest,
		Message:             r.Message,
		CheckedAt:           time.Now().Format("2006-01-02 15:04:05"),
		PublishedAt:         r.PublishedAt,
		ReleaseURL:          r.ReleaseURL,
		Notes:               r.Notes,
		RequiresImageUpdate: r.RequiresImageUpdate,
	}
}

func (s *Server) savedUpdateState() *updateState {
	if _, valid := update.ParseSemVer(s.version); !valid && !update.IsDevVersion(s.version) {
		return &updateState{Supported: false, Message: "当前版本格式无效，无法检测更新"}
	}
	raw, _ := s.store.Setting("update_state")
	if raw == "" {
		return nil
	}
	var state updateState
	if json.Unmarshal([]byte(raw), &state) != nil {
		return nil
	}
	state.CurrentIsDev = update.IsDevVersion(s.version)
	if latest, ok := update.ParseSemVer(state.LatestVersion); ok {
		if current, valid := update.ParseSemVer(s.version); valid && current.Compare(latest) >= 0 {
			state.HasUpdate = false
			if state.Supported {
				state.Message = "已是最新版本"
				if current.Compare(latest) > 0 {
					state.Message = "正在运行预发布或自定义版本"
				}
			}
		}
	}
	return &state
}

func (s *Server) updateAvailableBadge(location string) string {
	state := s.savedUpdateState()
	if state == nil || !state.HasUpdate || !state.Supported || (update.CurrentPlatform().InDocker && state.RequiresImageUpdate) {
		return ""
	}
	return `<a class="update-available-link update-available-` + esc(location) + `" href="/settings" title="发现可用更新" aria-label="发现可用更新，前往系统设置"><img class="update-available-icon" src="/static/update-available.svg?v=2" alt="" aria-hidden="true"></a>`
}

func (s *Server) updateAvailableBadgeSlot(location string) string {
	return `<span class="update-available-slot" hx-get="/partials/update-badge?location=` + esc(location) + `" hx-trigger="every 1m" hx-swap="innerHTML">` + s.updateAvailableBadge(location) + `</span>`
}

func (s *Server) updateBadgePartial(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	location := r.URL.Query().Get("location")
	if location != "dashboard" && location != "sidebar" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	fmt.Fprint(w, s.updateAvailableBadge(location))
}

func (s *Server) deploymentCard(messages ...updateCardMessage) string {
	platform := update.CurrentPlatform()
	runMode := platform.OS
	if platform.OS == "windows" {
		runMode = "Windows"
	}
	if platform.InDocker {
		runMode = "Docker · Alpine"
	}
	proxyValue := s.updater.Proxy()
	state := s.savedUpdateState()
	if state != nil && platform.InDocker && state.RequiresImageUpdate && platform.ImageVersion != state.LatestVersion && s.version == state.LatestVersion {
		state.Message = "当前程序已是最新版，但容器运行环境需要升级"
	}
	latestText := "尚未检测"
	releaseMarkup := ""
	installMarkup := ""
	if state != nil {
		if state.LatestVersion != "" {
			latestText = "v" + state.LatestVersion
		}
		if state.ReleaseURL != "" && update.ValidateReleaseURL(state.ReleaseURL, state.LatestTag) == nil {
			releaseMarkup = fmt.Sprintf(`<p class=update-release><a href="%s" target=_blank rel="noopener noreferrer">查看更新说明</a>%s</p>`, esc(state.ReleaseURL), func() string {
				if state.PublishedAt != "" {
					return ` · 发布于 ` + esc(formatTime(state.PublishedAt))
				}
				return ""
			}())
		}
		if state.HasUpdate && state.Supported && !(platform.InDocker && state.RequiresImageUpdate) {
			installMarkup = `<form method=post action=/settings/update/install class=update-install-form data-confirm="确认安装此更新？程序将自动重启。"><input type=hidden name=csrf_token value="{{CSRF}}"><button class=primary>在线更新</button></form>`
		}
	}
	extraFacts := ""
	if platform.InDocker {
		if notice := os.Getenv("CTYUN_IMAGE_UPDATE_REQUIRED"); notice != "" {
			messages = append(messages, updateCardMessage{Level: "warning", Text: notice})
		}
		if current, ok := update.ParseSemVer(s.version); ok {
			if imageVersion, valid := update.ParseSemVer(platform.ImageVersion); valid && current.Compare(imageVersion) > 0 {
				messages = append(messages, updateCardMessage{Level: "neutral", Text: "程序已通过容器内更新升级"})
			}
		}
	}
	statusMarkup := renderUpdateMessageQueue(messages)
	if platform.InDocker {
		extraFacts = fmt.Sprintf(`<div><dt>Launcher 版本</dt><dd>v%s</dd></div><div><dt>Docker 镜像版本</dt><dd>v%s</dd></div>`, esc(orDash(platform.LauncherVersion)), esc(orDash(platform.ImageVersion)))
	}
	return fmt.Sprintf(`<article class="panel info-panel settings-card deployment-settings-card"><div class=settings-card-head><span class="panel-icon material-symbols-rounded">deployed_code</span><div><h2>部署信息</h2><small>CtYunKeeper 当前运行环境</small></div></div><dl class=deployment-facts><div><dt>当前版本</dt><dd>v%s</dd></div><div><dt>运行平台</dt><dd>%s</dd></div><div><dt>系统架构</dt><dd>%s</dd></div>%s<div><dt>最新版本</dt><dd>%s</dd></div></dl>%s%s<div id=update-progress hx-get=/partials/update-status hx-trigger="load, every 2s" hx-swap=innerHTML></div><form id=update-proxy-form class=update-proxy-form method=post action=/settings/update/proxy><input type=hidden name=csrf_token value="{{CSRF}}"><label>GitHub 代理<input name=github_proxy value="%s" placeholder="留空使用 GitHub 直连"></label><button class=secondary type=submit>保存代理</button></form><div class=deployment-actions><form method=post action=/settings/update/check><input type=hidden name=csrf_token value="{{CSRF}}"><button class=primary>检测更新</button></form>%s</div></article>`, esc(s.version), runMode, platform.OS+"/"+platform.Arch, extraFacts, latestText, statusMarkup, releaseMarkup, esc(proxyValue), installMarkup)
}

func renderUpdateMessageQueue(messages []updateCardMessage) string {
	seen := map[string]bool{}
	items := ""
	for _, message := range messages {
		message.Text = strings.TrimSpace(message.Text)
		if message.Text == "" {
			continue
		}
		switch message.Level {
		case "success", "warning", "error", "neutral":
		default:
			message.Level = "neutral"
		}
		key := message.Level + "\x00" + message.Text
		if seen[key] {
			continue
		}
		seen[key] = true
		duration := 4000
		role := "status"
		if message.Level == "warning" {
			duration = 5000
		}
		if message.Level == "error" {
			duration = 6000
			role = "alert"
		}
		items += fmt.Sprintf(`<div class="update-status %s" role=%s data-update-message data-duration=%d hidden>%s</div>`, message.Level, role, duration, esc(message.Text))
	}
	return `<div class=update-message-queue data-update-message-queue aria-live=polite>` + items + `</div>`
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "--"
	}
	return strings.TrimPrefix(value, "v")
}

func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || r.Method != http.MethodPost {
		return
	}
	if !s.checkCSRF(r) {
		redirectUpdate(w, r, "请求验证失败，请刷新页面后重试", true)
		return
	}
	if s.updater == nil || s.updateManager == nil {
		redirectUpdate(w, r, "更新服务未初始化", true)
		return
	}
	if s.updateInitError != nil {
		redirectUpdate(w, r, "更新签名公钥无效："+s.updateInitError.Error(), true)
		return
	}
	if err := s.updateManager.Begin(); err != nil {
		redirectUpdate(w, r, err.Error(), true)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	result, err := s.performUpdateCheck(ctx)
	if err != nil {
		redirectUpdate(w, r, err.Error(), true)
		return
	}
	level := "neutral"
	if result.HasUpdate {
		level = "success"
	}
	if !result.Supported || result.NewerThanLatest {
		level = "warning"
	}
	redirectUpdateLevel(w, r, result.Message, level)
}

func (s *Server) performUpdateCheck(ctx context.Context) (*update.CheckResult, error) {
	s.updateManager.SetProgress(update.Progress{Status: update.StatusChecking, Message: "正在获取更新清单", FromVersion: s.version})
	result, err := s.updater.Check(ctx)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			err = errors.New("检测更新超时，请检查网络或代理设置")
		}
		s.updateManager.Fail(err)
		return nil, err
	}
	status := update.StatusIdle
	if result.HasUpdate {
		status = update.StatusAvailable
	}
	raw, _ := json.Marshal(updateStateFromResult(result))
	if err := s.store.SetSetting("update_state", string(raw)); err != nil {
		err = fmt.Errorf("保存检测结果失败: %w", err)
		s.updateManager.Fail(err)
		return nil, err
	}
	s.updateManager.SetProgress(update.Progress{Status: status, Message: result.Message, FromVersion: s.version, TargetVersion: result.LatestVersion})
	s.updateManager.Finish()
	return result, nil
}

const automaticUpdateCheckHour = 4

func nextAutomaticUpdateCheck(now time.Time) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), automaticUpdateCheckHour, 0, 0, 0, now.Location())
	if !next.After(now) {
		tomorrow := now.AddDate(0, 0, 1)
		next = time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), automaticUpdateCheckHour, 0, 0, 0, now.Location())
	}
	return next
}

func (s *Server) startAutomaticUpdateChecks() {
	_, validVersion := update.ParseSemVer(s.version)
	if s.ctx == nil || s.updater == nil || s.updateManager == nil || (!validVersion && !update.IsDevVersion(s.version)) {
		return
	}
	s.updateWG.Add(1)
	go func() {
		defer s.updateWG.Done()
		for {
			delay := time.Until(nextAutomaticUpdateCheck(time.Now()))
			timer := time.NewTimer(delay)
			select {
			case <-s.ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
				s.runAutomaticUpdateCheck()
			}
		}
	}()
}

func (s *Server) runAutomaticUpdateCheck() {
	if s.ctx.Err() != nil || s.updateInitError != nil {
		return
	}
	if err := s.updateManager.Begin(); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
	defer cancel()
	_, _ = s.performUpdateCheck(ctx)
}

func (s *Server) saveUpdateProxy(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || r.Method != http.MethodPost {
		return
	}
	if !s.checkCSRF(r) {
		redirectUpdate(w, r, "请求验证失败，请刷新页面后重试", true)
		return
	}
	proxy := strings.TrimSpace(r.FormValue("github_proxy"))
	if err := update.ValidateProxy(proxy); err != nil {
		redirectUpdate(w, r, "代理设置无效："+err.Error(), true)
		return
	}
	if err := s.store.SetSetting("github_proxy", proxy); err != nil {
		redirectUpdate(w, r, "保存代理设置失败："+err.Error(), true)
		return
	}
	if s.updater != nil {
		s.updater.SetProxy(proxy)
	}
	redirectUpdate(w, r, "代理设置已保存", false)
}

func (s *Server) installUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || r.Method != http.MethodPost {
		return
	}
	if !s.checkCSRF(r) {
		redirectUpdate(w, r, "请求验证失败，请刷新页面后重试", true)
		return
	}
	if s.requestShutdown == nil || s.updater == nil || s.updateManager == nil {
		redirectUpdate(w, r, "当前程序未启用在线更新执行器", true)
		return
	}
	if s.updateInitError != nil {
		redirectUpdate(w, r, "更新签名公钥无效："+s.updateInitError.Error(), true)
		return
	}
	if err := s.updateManager.Begin(); err != nil {
		redirectUpdate(w, r, err.Error(), true)
		return
	}
	s.updateWG.Add(1)
	go func() {
		defer s.updateWG.Done()
		s.runUpdateInstall()
	}()
	redirectUpdate(w, r, "更新任务已启动", false)
}

func (s *Server) runUpdateInstall() {
	var historyID int64
	fail := func(err error) {
		s.manager.CancelUpdateMaintenance()
		if historyID > 0 {
			_ = s.store.UpdateUpdateHistory(historyID, "failed", err.Error())
		}
		s.updateManager.Fail(err)
	}
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Minute)
	defer cancel()
	if err := s.manager.CheckUpdateBlockers(); err != nil {
		fail(err)
		return
	}
	if err := ensureWritable(s.dataDir); err != nil {
		fail(err)
		return
	}
	s.updateManager.SetProgress(update.Progress{Status: update.StatusChecking, Message: "正在获取更新清单", FromVersion: s.version})
	result, err := s.updater.Check(ctx)
	if err != nil {
		fail(err)
		return
	}
	if !result.HasUpdate || !result.Supported || result.Manifest == nil {
		fail(fmt.Errorf("%s", result.Message))
		return
	}
	platform := update.CurrentPlatform()
	asset, _ := result.Manifest.AssetFor(platform.OS, platform.Arch)
	restart, serviceName, err := update.ResolveRestart(os.Getenv("UPDATE_RESTART_MODE"))
	if err != nil {
		fail(err)
		return
	}
	if asset.Size <= 0 {
		fail(errors.New("更新包大小无效"))
		return
	}
	if free, diskErr := update.AvailableDiskSpace(s.dataDir); diskErr != nil {
		fail(fmt.Errorf("读取磁盘空间失败: %w", diskErr))
		return
	} else if uint64(asset.Size) > free/3 {
		fail(fmt.Errorf("磁盘空间不足：至少需要 %.1f MB", float64(asset.Size*3)/(1<<20)))
		return
	}
	historyID, err = s.store.AddUpdateHistory(s.version, result.LatestVersion, platform.AssetKey(), "downloading")
	if err != nil {
		fail(err)
		return
	}
	installer := update.NewInstaller(s.updater, s.dataDir, s.version, platform)
	s.updateManager.SetProgress(update.Progress{Status: update.StatusDownloading, Message: "正在下载更新包", FromVersion: s.version, TargetVersion: result.LatestVersion})
	req, err := installer.Prepare(ctx, result.Manifest, func(received, total int64) {
		percent := 0
		if total > 0 {
			percent = int(received * 100 / total)
		}
		s.updateManager.SetProgress(update.Progress{Status: update.StatusDownloading, Percent: percent, Received: received, Total: total, Message: "正在下载更新包", FromVersion: s.version, TargetVersion: result.LatestVersion})
	})
	if err != nil {
		fail(err)
		return
	}
	s.updateManager.SetProgress(update.Progress{Status: update.StatusVerifying, Percent: 100, Message: "更新包校验完成，正在检查运行状态", FromVersion: s.version, TargetVersion: result.LatestVersion})
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
	err = s.manager.PrepareForUpdate(waitCtx)
	waitCancel()
	if err != nil {
		fail(err)
		return
	}
	if err = ensureWritable(s.dataDir); err != nil {
		fail(err)
		return
	}
	if err = s.store.Checkpoint(); err != nil {
		fail(fmt.Errorf("数据库 checkpoint 失败: %w", err))
		return
	}
	databasePath := filepath.Join(s.dataDir, "ctyun-keeper.db")
	databaseBackup := filepath.Join(req.BackupDir, "database", "ctyun-keeper.db")
	_ = os.Remove(databaseBackup)
	if err = s.store.BackupDatabase(databaseBackup); err != nil {
		fail(fmt.Errorf("备份数据库失败: %w", err))
		return
	}
	token, err := update.NewRequestToken()
	if err != nil {
		fail(err)
		return
	}
	req.Action = "install"
	req.ParentPID = os.Getpid()
	req.DatabasePath = databasePath
	req.DatabaseBackup = databaseBackup
	req.HealthURL = "http://127.0.0.1:" + appPort() + "/health"
	req.RestartMode, req.ServiceName = restart, serviceName
	req.Token = token
	req.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	req.HistoryID = historyID
	req.ResultPath = filepath.Join(s.dataDir, "updates", "results", fmt.Sprintf("%d.json", historyID))
	if err = s.writeUpdateRequest(req, token, platform.InDocker); err != nil {
		fail(err)
		return
	}
	_ = s.store.UpdateUpdateHistory(historyID, "staged", "更新包已验证")
	s.updateManager.SetProgress(update.Progress{Status: update.StatusStopping, Percent: 100, Message: "正在准备重启", FromVersion: s.version, TargetVersion: result.LatestVersion})
	if platform.InDocker {
		if err := s.updateManager.HandOff(req.Token); err != nil {
			fail(err)
			return
		}
		go func() {
			time.Sleep(500 * time.Millisecond)
			s.requestShutdown(update.ExitUpdateRequested)
		}()
		return
	}
	updaterSource := filepath.Join(filepath.Dir(req.Executable), "ctyun-keeper-updater.exe")
	updaterTemp := filepath.Join(s.dataDir, "updates", "helper", token+".exe")
	if err = update.CopyFile(updaterSource, updaterTemp); err != nil {
		fail(fmt.Errorf("准备更新助手失败: %w", err))
		return
	}
	if err := s.updateManager.HandOff(req.Token); err != nil {
		fail(err)
		return
	}
	if err = update.LaunchUpdater(updaterTemp, updateRequestPath(s.dataDir), token, filepath.Dir(req.Executable), s.dataDir); err != nil {
		update.ReleaseUpdateLock(s.dataDir, req.Token)
		fail(fmt.Errorf("启动更新助手失败: %w", err))
		return
	}
	if req.RestartMode == "supervisor" {
		return
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.requestShutdown(0)
	}()
}

func ensureWritable(dir string) error {
	path := filepath.Join(dir, "updates", ".write-test")
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return fmt.Errorf("更新目录不可写: %w", err)
	}
	if err := os.WriteFile(path, []byte("ok"), 0600); err != nil {
		return fmt.Errorf("更新目录不可写: %w", err)
	}
	return os.Remove(path)
}

func appPort() string {
	if value := strings.TrimSpace(os.Getenv("APP_PORT")); value != "" {
		return value
	}
	return "9845"
}

func updateRequestPath(dataDir string) string {
	return filepath.Join(dataDir, "updates", "install-request.json")
}

func (s *Server) writeUpdateRequest(req update.InstallRequest, token string, inDocker bool) error {
	if err := req.Validate(s.dataDir, filepath.Dir(req.Executable), token); err != nil {
		return err
	}
	if err := update.WriteInstallRequest(updateRequestPath(s.dataDir), req); err != nil {
		return err
	}
	if inDocker {
		return update.WriteInstallToken(s.dataDir, token)
	}
	return nil
}

func (s *Server) updateStatusPartial(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	progress := s.currentUpdateProgress()
	if progress.Status == update.StatusIdle {
		return
	}
	if progress.Status == update.StatusAvailable || progress.Status == update.StatusSuccess || progress.Status == update.StatusFailed || progress.Status == update.StatusRolledBack {
		if updatedAt, err := time.Parse(time.RFC3339, progress.UpdatedAt); err == nil && time.Since(updatedAt) > time.Minute {
			return
		}
	}
	fmt.Fprintf(w, `<div class="update-progress-state %s" data-version="%s"><strong>%s</strong><progress max=100 value="%d"></progress><small>%s</small></div>`, esc(string(progress.Status)), esc(s.version), esc(progress.Message), progress.Percent, esc(formatBytesProgress(progress.Received, progress.Total)))
}

func formatBytesProgress(received, total int64) string {
	if received <= 0 {
		return ""
	}
	if total > 0 {
		return fmt.Sprintf("%.1f MB / %.1f MB", float64(received)/(1<<20), float64(total)/(1<<20))
	}
	return fmt.Sprintf("%.1f MB", float64(received)/(1<<20))
}

func (s *Server) apiUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.currentUpdateProgress())
}

func (s *Server) currentUpdateProgress() update.Progress {
	p := s.updateManager.Progress()
	if raw, err := os.ReadFile(filepath.Join(s.dataDir, "updates", "executor-progress.json")); err == nil {
		var external update.Progress
		if json.Unmarshal(raw, &external) == nil {
			return external
		}
	}
	if p.Status != update.StatusIdle {
		return p
	}
	if p.Message != "" {
		return p
	}
	if history, err := s.store.RecentUpdateHistory(1); err == nil && len(history) > 0 {
		h := history[0]
		return update.Progress{Status: update.Status(h.Status), Message: h.Message, FromVersion: h.FromVersion, TargetVersion: h.ToVersion, UpdatedAt: h.FinishedAt}
	}
	return p
}

// Called after consuming executor results, including failures before shutdown.
func (s *Server) RefreshUpdateResult() {
	p := s.updateManager.Progress()
	if p.Status != update.StatusStopping && p.Status != update.StatusRollingBack {
		return
	}
	history, err := s.store.RecentUpdateHistory(1)
	if err != nil || len(history) == 0 || history[0].FinishedAt == "" {
		return
	}
	h := history[0]
	s.manager.CancelUpdateMaintenance()
	s.updateManager.Finish()
	s.updateManager.SetProgress(update.Progress{Status: update.Status(h.Status), Message: h.Message, TargetVersion: h.ToVersion})
}

func (s *Server) authSettings(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || r.Method != http.MethodPost {
		return
	}
	if !s.checkCSRF(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	enabled := r.FormValue("auth_enabled") == "true"
	if err := s.store.SetSetting("admin_auth_enabled", strconv.FormatBool(enabled)); err != nil {
		redirect(w, r, "/settings", "保存访问设置失败："+err.Error(), true)
		return
	}
	if enabled {
		s.cookie(w, "ctyun_session", security.SignCookie(s.sessionKey, "authenticated"), true)
		redirect(w, r, "/settings", "密码登录已启用", false)
		return
	}
	redirect(w, r, "/settings", "密码登录已关闭，请仅在可信局域网中使用", false)
}
func (s *Server) logSettings(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || r.Method != http.MethodPost {
		return
	}
	if !s.checkCSRF(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("retention_days")))
	if err != nil || days < 1 || days > 3650 {
		redirect(w, r, "/settings", "日志保留天数必须在 1 到 3650 天之间", true)
		return
	}
	if err := s.manager.SetLogRetentionDays(days); err != nil {
		redirect(w, r, "/settings", "保存日志设置失败："+err.Error(), true)
		return
	}
	redirect(w, r, "/settings", "日志设置已保存", false)
}
func (s *Server) clearAllLogs(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || r.Method != http.MethodPost {
		return
	}
	if !s.checkCSRF(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err := s.manager.ClearAllLogs(); err != nil {
		redirect(w, r, "/settings", "清空全部日志失败："+err.Error(), true)
		return
	}
	redirect(w, r, "/settings", "全部日志已清空", false)
}
func (s *Server) password(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || r.Method != "POST" {
		return
	}
	if !s.checkCSRF(r) {
		http.Error(w, "Forbidden", 403)
		return
	}
	old, _ := s.store.Setting("admin_password_hash")
	p := r.FormValue("password")
	if !security.VerifyPassword(old, r.FormValue("current_password")) {
		redirect(w, r, "/settings", "当前密码不正确", true)
		return
	}
	if len(p) < 8 || p != r.FormValue("confirmation") {
		redirect(w, r, "/settings", "新密码至少 8 位且两次输入一致", true)
		return
	}
	_ = s.store.SetSetting("admin_password_hash", security.HashPassword(p))
	redirect(w, r, "/settings", "管理密码已更新", false)
}
func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(s.manager.MarshalStatus())
}
func (s *Server) apiAccountRoute(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, `{"error":"unauthorized"}`, 401)
		return
	}
	if r.Method != "POST" || !s.checkCSRF(r) {
		http.Error(w, `{"error":"forbidden"}`, 403)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 5 || parts[0] != "api" || parts[1] != "accounts" || parts[3] != "tasks" {
		http.NotFound(w, r)
		return
	}
	id, _ := strconv.ParseInt(parts[2], 10, 64)
	run, e := s.manager.StartTask(id, parts[4], "webmcp")
	w.Header().Set("Content-Type", "application/json")
	if e != nil {
		w.WriteHeader(409)
		fmt.Fprintf(w, `{"error":%q}`, e.Error())
		return
	}
	fmt.Fprintf(w, `{"run_id":%d,"status":"queued"}`, run)
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = []ctyun.Reward{}
