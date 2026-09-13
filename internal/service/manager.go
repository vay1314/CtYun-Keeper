package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/vay1314/CtYun-Keeper/internal/ctyun"
	"github.com/vay1314/CtYun-Keeper/internal/eai"
	"github.com/vay1314/CtYun-Keeper/internal/security"
	"github.com/vay1314/CtYun-Keeper/internal/storage"
)

type clientState struct {
	client  *ctyun.Client
	ctx     context.Context
	cancel  context.CancelFunc
	status  string
	workers int
	updated time.Time
}
type running struct {
	cancel    context.CancelFunc
	typ       string
	accountID int64
}
type VerifySession struct {
	Client  *ctyun.Client
	Expires time.Time
}
type Manager struct {
	store         *storage.Store
	credentialKey []byte
	dataDir, ocr  string
	logger        *log.Logger
	logFile       *os.File
	logMu         sync.Mutex
	logCloseOnce  sync.Once
	wg            sync.WaitGroup
	launchMu      sync.Mutex
	closing       bool
	mu            sync.RWMutex
	clients       map[int64]*clientState
	nativeClients map[int64]*ctyun.NativeClient
	active        map[int64]running
	starting      map[string]struct{}
	verify        map[int64]VerifySession
	ctx           context.Context
	cancel        context.CancelFunc
	started       time.Time
	maintenance   bool
}

func New(store *storage.Store, key []byte, dataDir, ocr string) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	_ = os.MkdirAll(filepath.Join(dataDir, "logs"), 0750)
	f, _ := os.OpenFile(filepath.Join(dataDir, "logs", "ctyun.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	return &Manager{store: store, credentialKey: key, dataDir: dataDir, ocr: ocr, logger: log.New(f, "", log.LstdFlags), logFile: f, clients: map[int64]*clientState{}, nativeClients: map[int64]*ctyun.NativeClient{}, active: map[int64]running{}, starting: map[string]struct{}{}, verify: map[int64]VerifySession{}, ctx: ctx, cancel: cancel, started: time.Now()}
}
func (m *Manager) Start() {
	m.RestartKeepalive()
	_ = m.CleanupExpiredLogs()
	m.launch(m.scheduler)
}
func (m *Manager) Close() {
	m.cancel()
	m.launchMu.Lock()
	m.closing = true
	m.launchMu.Unlock()
	m.mu.Lock()
	for _, c := range m.clients {
		if c.cancel != nil {
			c.cancel()
		}
	}
	for _, r := range m.active {
		r.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
	m.logCloseOnce.Do(func() {
		if m.logFile != nil {
			_ = m.logFile.Close()
		}
	})
}

func (m *Manager) launch(fn func()) {
	m.launchMu.Lock()
	if m.closing {
		m.launchMu.Unlock()
		return
	}
	m.wg.Add(1)
	m.launchMu.Unlock()
	go func() {
		defer m.wg.Done()
		fn()
	}()
}
func (m *Manager) Started() time.Time { return m.started }
func (m *Manager) logf(format string, v ...any) {
	m.logMu.Lock()
	m.logger.Printf(format, v...)
	m.logMu.Unlock()
}

func (m *Manager) LogRetentionDays() int {
	value, _ := m.store.Setting("log_retention_days")
	days, e := strconv.Atoi(value)
	if e != nil || days < 1 || days > 3650 {
		return 15
	}
	return days
}

func (m *Manager) SetLogRetentionDays(days int) error {
	if days < 1 || days > 3650 {
		return errors.New("日志保留天数必须在 1 到 3650 天之间")
	}
	if e := m.store.SetSetting("log_retention_days", strconv.Itoa(days)); e != nil {
		return e
	}
	return m.CleanupExpiredLogs()
}

func (m *Manager) ClearLog(runID int64) error {
	if runID == 0 {
		m.logMu.Lock()
		defer m.logMu.Unlock()
		return truncateLogFile(filepath.Join(m.dataDir, "logs", "ctyun.log"))
	}
	run, e := m.store.Run(runID)
	if e != nil {
		return e
	}
	if !m.allowedTaskLog(run.LogPath) {
		return errors.New("任务日志路径无效")
	}
	return truncateLogFile(run.LogPath)
}

func (m *Manager) ClearAllLogs() error {
	if e := m.ClearLog(0); e != nil {
		return e
	}
	runs, e := m.store.RunLogs()
	if e != nil {
		return e
	}
	var clearErrors []error
	for _, run := range runs {
		if !m.allowedTaskLog(run.LogPath) {
			clearErrors = append(clearErrors, fmt.Errorf("任务 #%d 日志路径无效", run.ID))
			continue
		}
		if run.Status == "queued" || run.Status == "running" {
			if e := truncateLogFile(run.LogPath); e != nil {
				clearErrors = append(clearErrors, e)
			}
			continue
		}
		if e := os.Remove(run.LogPath); e != nil && !errors.Is(e, os.ErrNotExist) {
			clearErrors = append(clearErrors, e)
		}
	}
	return errors.Join(clearErrors...)
}

func (m *Manager) CleanupExpiredLogs() error {
	cutoff := time.Now().AddDate(0, 0, -m.LogRetentionDays())
	if e := m.trimSystemLog(cutoff); e != nil {
		return e
	}
	paths, e := m.store.PurgeCompletedRunsBefore(cutoff.Format(time.RFC3339))
	if e != nil {
		return e
	}
	for _, path := range paths {
		if m.allowedTaskLog(path) {
			_ = os.Remove(path)
		}
	}
	return nil
}

func (m *Manager) trimSystemLog(cutoff time.Time) error {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	path := filepath.Join(m.dataDir, "logs", "ctyun.log")
	raw, e := os.ReadFile(path)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if len(raw) == 0 {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if len(line) >= 19 {
			if stamp, parseErr := time.ParseInLocation("2006/01/02 15:04:05", line[:19], time.Local); parseErr == nil && stamp.Before(cutoff) {
				continue
			}
		}
		kept = append(kept, line)
	}
	return os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0640)
}

func (m *Manager) allowedTaskLog(path string) bool {
	base, e := filepath.Abs(filepath.Join(m.dataDir, "logs", "tasks"))
	if e != nil {
		return false
	}
	target, e := filepath.Abs(path)
	if e != nil {
		return false
	}
	rel, e := filepath.Rel(base, target)
	if e != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	if info, statErr := os.Lstat(target); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return true
}

func truncateLogFile(path string) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0640)
	if e != nil {
		return e
	}
	return f.Close()
}
func (m *Manager) newClient(a storage.Account) (*ctyun.Client, error) {
	pwd, e := security.DecryptFernet(a.PasswordEncrypted, m.credentialKey)
	if e != nil {
		return nil, e
	}
	c := ctyun.NewClient(a.Username, pwd, a.DeviceCode, m.ocr)
	if encrypted, _ := m.store.AuthCache(a.ID); encrypted != "" {
		if raw, e := security.DecryptFernet(encrypted, m.credentialKey); e == nil {
			var p ctyun.Profile
			if json.Unmarshal([]byte(raw), &p) == nil && p.UserID > 0 && p.SecretKey != "" {
				c.Profile = &p
			}
		}
	}
	return c, nil
}
func (m *Manager) saveProfile(id int64, p ctyun.Profile) {
	raw, _ := json.Marshal(p)
	if encrypted, e := security.EncryptFernet(string(raw), m.credentialKey); e == nil {
		_ = m.store.SaveAuthCache(id, encrypted)
	}
}

func (m *Manager) newNativeClient(a storage.Account) (*ctyun.NativeClient, error) {
	pwd, err := security.DecryptFernet(a.PasswordEncrypted, m.credentialKey)
	if err != nil {
		return nil, err
	}
	ocrClient := ctyun.NewClient(a.Username, pwd, a.DeviceCode, m.ocr)
	client := ctyun.NewNativeClient(a.Username, pwd, a.DeviceCode, ocrClient.SolveCaptcha)
	if encrypted, _ := m.store.NativeAuthCache(a.ID); encrypted != "" {
		if raw, decryptErr := security.DecryptFernet(encrypted, m.credentialKey); decryptErr == nil {
			var profile ctyun.NativeProfile
			if json.Unmarshal([]byte(raw), &profile) == nil && profile.UserID > 0 && profile.UserEID != "" && profile.TenantID > 0 && profile.SecretKey != "" && profile.CommonLoginReqHeader != "" {
				client.UseProfile(profile)
			}
		}
	}
	return client, nil
}

func (m *Manager) saveNativeProfile(id int64, profile ctyun.NativeProfile) error {
	raw, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	encrypted, err := security.EncryptFernet(string(raw), m.credentialKey)
	if err != nil {
		return err
	}
	return m.store.SaveNativeAuthCache(id, encrypted)
}

func (m *Manager) nativeClient(ctx context.Context, a storage.Account) (*ctyun.NativeClient, error) {
	m.mu.RLock()
	client := m.nativeClients[a.ID]
	m.mu.RUnlock()
	if client != nil {
		if _, ok := client.Profile(); ok {
			return client, nil
		}
	}
	var err error
	if client == nil {
		client, err = m.newNativeClient(a)
		if err != nil {
			return nil, err
		}
	}
	if _, ok := client.Profile(); !ok {
		profile, loginErr := client.Login(ctx)
		if loginErr != nil {
			return nil, loginErr
		}
		if err := m.saveNativeProfile(a.ID, profile); err != nil {
			client.ClearProfile()
			return nil, fmt.Errorf("保存原生登录态失败：%w", err)
		}
	}
	m.mu.Lock()
	m.nativeClients[a.ID] = client
	m.mu.Unlock()
	return client, nil
}

func (m *Manager) ClearNativeAuth(id int64) {
	m.store.ClearNativeAuthCache(id)
	m.mu.Lock()
	delete(m.nativeClients, id)
	m.mu.Unlock()
}

func (m *Manager) runChat(ctx context.Context, a storage.Account, logger *log.Logger) error {
	logger.Printf("正在使用原生登录态获取 AI 授权")
	client, err := m.nativeClient(ctx, a)
	if err != nil {
		return err
	}
	err = eai.New(client).Chat(ctx, "你好")
	if !ctyun.IsLoginExpired(err) {
		return err
	}
	logger.Printf("AI 原生登录态已失效，正在重新登录")
	client.ClearProfile()
	m.store.ClearNativeAuthCache(a.ID)
	profile, loginErr := client.Login(ctx)
	if loginErr != nil {
		return fmt.Errorf("AI 原生登录态失效，重新登录失败：%w", loginErr)
	}
	if saveErr := m.saveNativeProfile(a.ID, profile); saveErr != nil {
		client.ClearProfile()
		return fmt.Errorf("保存刷新后的原生登录态失败：%w", saveErr)
	}
	logger.Printf("原生登录态已刷新，正在重试 AI 对话")
	return eai.New(client).Chat(ctx, "你好")
}
func (m *Manager) client(ctx context.Context, a storage.Account) (*ctyun.Client, error) {
	m.mu.RLock()
	s := m.clients[a.ID]
	m.mu.RUnlock()
	if s != nil && s.client != nil && s.client.Profile != nil {
		return s.client, nil
	}
	c, e := m.newClient(a)
	if e != nil {
		return nil, e
	}
	if c.Profile == nil {
		if p, loginErr := c.Login(ctx); loginErr != nil {
			return nil, loginErr
		} else {
			m.saveProfile(a.ID, p)
		}
	}
	m.mu.Lock()
	old := m.clients[a.ID]
	if old == nil {
		m.clients[a.ID] = &clientState{client: c, status: "已登录", updated: time.Now()}
	} else {
		old.client = c
		old.status = "已登录"
		old.updated = time.Now()
	}
	m.mu.Unlock()
	return c, nil
}
func (m *Manager) RestartKeepalive() {
	accounts, e := m.store.Accounts()
	if e != nil {
		m.logf("加载账号失败：%v", e)
		return
	}
	m.mu.Lock()
	for _, s := range m.clients {
		if s.cancel != nil {
			s.cancel()
		}
	}
	m.clients = map[int64]*clientState{}
	m.mu.Unlock()
	now := time.Now()
	for _, a := range accounts {
		m.reconcileAccountKeepalive(a, now)
	}
}

func (m *Manager) reconcileKeepalive(now time.Time) {
	accounts, e := m.store.Accounts()
	if e != nil {
		m.logf("同步保活周期失败：%v", e)
		return
	}
	for _, a := range accounts {
		m.reconcileAccountKeepalive(a, now)
	}
}

func (m *Manager) reconcileAccountKeepalive(a storage.Account, now time.Time) {
	want := a.DeviceStatus != "pending" && a.KeepaliveActiveAt(now)
	m.mu.RLock()
	if m.maintenance {
		m.mu.RUnlock()
		return
	}
	state := m.clients[a.ID]
	running := state != nil && state.ctx != nil && state.ctx.Err() == nil
	usageActive := m.accountUsageActiveLocked(a.ID)
	m.mu.RUnlock()
	if want && !running {
		m.launch(func() { m.startAccount(a) })
		return
	}
	if !want && running && usageActive {
		return
	}
	if !want && running {
		m.stopAccountKeepalive(a, keepaliveIdleStatus(a))
		return
	}
	if !want && !running {
		m.mu.Lock()
		current := m.clients[a.ID]
		if current == nil || current.ctx == nil || current.ctx.Err() != nil {
			m.clients[a.ID] = &clientState{status: keepaliveIdleStatus(a), updated: now}
		}
		m.mu.Unlock()
	}
}

func (m *Manager) accountUsageActiveLocked(accountID int64) bool {
	for _, task := range m.active {
		if task.accountID == accountID && task.typ == fmt.Sprintf("%d:pc", accountID) {
			return true
		}
	}
	return false
}

func keepaliveIdleStatus(a storage.Account) string {
	if !a.Enabled {
		return "账号已停用"
	}
	if a.DeviceStatus == "pending" {
		return "等待短信验证"
	}
	if a.EffectiveKeepaliveMode() == storage.KeepaliveScheduled {
		return fmt.Sprintf("等待定时保活（%s–%s）", a.KeepaliveStart, a.KeepaliveEnd)
	}
	return "持续保活已关闭"
}

func (m *Manager) stopAccountKeepalive(a storage.Account, status string) {
	m.mu.Lock()
	state := m.clients[a.ID]
	if state != nil && state.cancel != nil {
		state.cancel()
	}
	m.clients[a.ID] = &clientState{status: status, updated: time.Now()}
	m.mu.Unlock()
	m.logf("[%s] %s", a.Name, status)
}

func (m *Manager) startAccount(a storage.Account) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.mu.Lock()
	if current := m.clients[a.ID]; current != nil && current.ctx != nil && current.ctx.Err() == nil {
		m.mu.Unlock()
		cancel()
		return
	}
	m.clients[a.ID] = &clientState{ctx: ctx, cancel: cancel, status: "正在启动保活", updated: time.Now()}
	m.mu.Unlock()
	c, e := m.newClient(a)
	if e != nil {
		m.setState(a.ID, nil, "凭据错误："+e.Error(), 0, ctx, cancel)
		return
	}
	var p ctyun.Profile
	if c.Profile != nil {
		p = *c.Profile
	} else {
		p, e = c.Login(ctx)
		if e != nil {
			m.setState(a.ID, c, "登录失败："+e.Error(), 0, ctx, cancel)
			m.logf("[%s] %v", a.Name, e)
			return
		}
		m.saveProfile(a.ID, p)
	}
	if !p.BondedDevice {
		_ = m.store.SetDeviceStatus(a.ID, "pending")
		m.setState(a.ID, c, "等待短信验证", 0, ctx, cancel)
		return
	}
	_ = m.store.SetDeviceStatus(a.ID, "verified")
	desktops, e := c.ListDesktops(ctx)
	if e != nil && c.Profile != nil {
		c.Profile = nil
		m.store.ClearAuthCache(a.ID)
		if fresh, loginErr := c.Login(ctx); loginErr == nil {
			p = fresh
			m.saveProfile(a.ID, p)
			desktops, e = c.ListDesktops(ctx)
		}
	}
	if e != nil {
		m.setState(a.ID, c, "读取云电脑失败："+e.Error(), 0, ctx, cancel)
		return
	}
	workers := 0
	forbidden := 0
	issues := 0
	m.setState(a.ID, c, "正在建立保活", 0, ctx, cancel)
	for _, d := range desktops {
		if d.Forbidden {
			forbidden++
			m.logf("[%s/%s] 平台禁止连接，已跳过", a.Name, d.Name())
			continue
		}
		if !d.Running() {
			var startErr error
			d, startErr = waitForDesktopRunning(ctx, c, d, func(message string) {
				m.logf("[%s/%s] %s", a.Name, d.Name(), message)
				m.setState(a.ID, c, message, workers, ctx, cancel)
			})
			if startErr != nil {
				if ctx.Err() != nil {
					return
				}
				issues++
				m.logf("[%s/%s] 自动开机失败：%v", a.Name, d.Name(), startErr)
				continue
			}
		}
		info, e := waitForConnectionInfo(ctx, c, d, func(message string) {
			m.logf("[%s/%s] %s", a.Name, d.Name(), message)
		}, 2*time.Minute)
		if e != nil {
			if ctx.Err() != nil {
				return
			}
			issues++
			m.logf("[%s/%s] 获取连接失败：%v", a.Name, d.Name(), e)
			continue
		}
		workers++
		if e := c.ReportDesktopLogin(ctx, d); e != nil {
			m.logf("[%s/%s] 登录事件上报失败，将继续使用桌面握手：%v", a.Name, d.Name(), e)
		}
		m.launch(func() {
			desktop, info := d, info
			name := desktop.Name()
			refresh := func(refreshCtx context.Context) (ctyun.ConnectionInfo, error) {
				return waitForConnectionInfo(refreshCtx, c, desktop, nil, 30*time.Second)
			}
			e := ctyun.RunClink(ctx, info, p, a.DeviceCode, refresh, func(status string) { m.logf("[%s/%s] %s", a.Name, name, status) })
			if e != nil && !errors.Is(e, context.Canceled) {
				m.logf("[%s/%s] 保活结束：%v", a.Name, name, e)
			}
		})
	}
	if ctx.Err() != nil {
		return
	}
	status := fmt.Sprintf("保活运行中（%d 台云电脑）", workers)
	if workers > 0 && (issues > 0 || forbidden > 0) {
		status = fmt.Sprintf("保活运行中（%d 台，%d 台未接入）", workers, issues+forbidden)
	} else if workers == 0 {
		switch {
		case len(desktops) == 0:
			status = "已登录，账号下没有云电脑"
		case forbidden == len(desktops):
			status = "已登录，但云电脑被平台禁止连接"
		case issues > 0:
			status = "已登录，云电脑自动开机或连接失败"
		default:
			status = "已登录，暂无可保活的云电脑"
		}
	}
	m.setState(a.ID, c, status, workers, ctx, cancel)
}

func waitForDesktopRunning(ctx context.Context, c *ctyun.Client, desktop ctyun.Desktop, notify func(string)) (ctyun.Desktop, error) {
	if desktop.Running() {
		return desktop, nil
	}
	if notify != nil {
		notify(fmt.Sprintf("云电脑处于“%s”，正在自动唤醒", desktop.StatusText()))
	}
	// CtYun 上游在桌面未运行时直接调用 connect；平台会由该请求启动或
	// 唤醒桌面。先复用这条已验证路径，再用 operate 作为兼容性兜底。
	if info, e := c.Connect(ctx, desktop); e == nil && info.Ready() {
		if notify != nil {
			notify("云电脑连接已就绪，正在建立保活连接")
		}
		return desktop, nil
	}
	if e := c.PowerOn(ctx, desktop); e != nil {
		return desktop, fmt.Errorf("下发开机指令：%w", e)
	}
	if notify != nil {
		notify("唤醒请求已发送，正在等待云电脑就绪")
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	lastStatus := desktop.StatusText()
	for {
		select {
		case <-ctx.Done():
			return desktop, ctx.Err()
		case <-timer.C:
			return desktop, fmt.Errorf("等待开机超过 5 分钟，最后状态为“%s”", lastStatus)
		case <-ticker.C:
			// status 判断连接凭据是否真正就绪，pageDesktop 则用于展示电源状态。
			if info, statusErr := c.DesktopConnectionStatus(ctx, desktop); statusErr == nil && info.Ready() {
				if notify != nil {
					notify("云电脑连接已就绪，正在建立保活连接")
				}
				return desktop, nil
			}
			values, e := c.ListDesktops(ctx)
			if e != nil {
				if notify != nil {
					notify("等待开机时暂时无法刷新云电脑状态")
				}
				continue
			}
			for _, current := range values {
				if !sameDesktop(desktop, current) {
					continue
				}
				desktop = current
				if current.Forbidden {
					return current, errors.New("云电脑被平台禁止连接")
				}
				if current.Running() {
					if notify != nil {
						notify("云电脑已开机，正在等待连接凭据")
					}
					return current, nil
				}
				if current.StatusText() != lastStatus {
					lastStatus = current.StatusText()
					if notify != nil {
						notify(fmt.Sprintf("正在等待云电脑就绪，当前状态为“%s”", lastStatus))
					}
				}
				break
			}
		}
	}
}

func waitForConnectionInfo(ctx context.Context, c *ctyun.Client, desktop ctyun.Desktop, notify func(string), timeout time.Duration) (ctyun.ConnectionInfo, error) {
	if timeout <= 0 {
		timeout = time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	firstWait := true
	var lastErr error
	for {
		if info, e := c.DesktopConnectionStatus(ctx, desktop); e == nil && info.Ready() {
			return info, nil
		} else if e != nil {
			lastErr = e
		}
		if info, e := c.Connect(ctx, desktop); e == nil && info.Ready() {
			return info, nil
		} else if e != nil {
			lastErr = e
		}
		if firstWait && notify != nil {
			notify("云电脑已运行，正在等待平台生成 Clink 连接凭据")
			firstWait = false
		}
		select {
		case <-ctx.Done():
			return ctyun.ConnectionInfo{}, ctx.Err()
		case <-timer.C:
			if lastErr != nil {
				return ctyun.ConnectionInfo{}, fmt.Errorf("等待 Clink 连接凭据超时：%w", lastErr)
			}
			return ctyun.ConnectionInfo{}, errors.New("等待 Clink 连接凭据超时：平台尚未返回完整地址或证书")
		case <-time.After(3 * time.Second):
		}
	}
}

func sameDesktop(left, right ctyun.Desktop) bool {
	if left.ID() != "" && left.ID() == right.ID() {
		return true
	}
	return left.ObjectID != "" && left.ObjectID == right.ObjectID
}
func (m *Manager) setState(id int64, c *ctyun.Client, status string, workers int, ctx context.Context, cancel context.CancelFunc) {
	if ctx != nil && ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	m.clients[id] = &clientState{client: c, ctx: ctx, status: status, workers: workers, updated: time.Now(), cancel: cancel}
	m.mu.Unlock()
}
func (m *Manager) KeepaliveStatus() (string, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workers := 0
	errorsN := 0
	for _, s := range m.clients {
		workers += s.workers
		if strings.Contains(s.status, "失败") || strings.Contains(s.status, "错误") {
			errorsN++
		}
	}
	if errorsN > 0 {
		return fmt.Sprintf("部分异常（%d 个账号）", errorsN), workers
	}
	if workers > 0 {
		return "运行中", workers
	}
	return "等待云电脑", 0
}
func (m *Manager) AccountStatus(id int64) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s := m.clients[id]; s != nil {
		return s.status
	}
	return "未启动"
}

func scheduledTaskKey(typ string) string {
	if typ == "pc" {
		return "usage"
	}
	return typ
}

func statusUpdatedToday(value string, now time.Time) bool {
	updated, e := time.Parse(time.RFC3339, value)
	if e != nil {
		return false
	}
	updated = updated.In(now.Location())
	return updated.Year() == now.Year() && updated.YearDay() == now.YearDay()
}

func (m *Manager) startScheduledTask(a storage.Account, typ string, now time.Time, claimKey string) {
	// Query first so a second Cron time or a service restart does not repeat a
	// task which the platform has already credited today. If the query itself
	// fails, still run the task and let its own login recovery handle it.
	ctx, cancel := context.WithTimeout(m.ctx, 90*time.Second)
	status, e := m.RefreshStatus(ctx, a.ID)
	cancel()
	key := scheduledTaskKey(typ)
	if e == nil && statusUpdatedToday(status.UpdatedAt, now) {
		if task, ok := status.Tasks[key]; ok && task.State == "success" {
			m.logf("[%s] 今日%s已完成，跳过定时执行", a.Name, map[string]string{"login": "登录任务", "pc": "时长任务", "chat": "AI 对话任务"}[typ])
			return
		}
	}
	if _, e = m.StartTask(a.ID, typ, "schedule"); e != nil {
		if !strings.Contains(e.Error(), "已在运行") {
			_ = m.store.ReleaseClaim(a.ID, typ, claimKey)
			m.logf("[%s] 启动定时任务失败：%v", a.Name, e)
		}
	}
}

func normalizeTasks(tasks []ctyun.Task) map[string]storage.TaskStatus {
	out := map[string]storage.TaskStatus{}
	for _, t := range tasks {
		key := ""
		name := strings.ReplaceAll(t.Name, " ", "")
		switch {
		case t.ID == 1002 || strings.Contains(name, "登录AI云电脑"):
			key = "login"
		case t.ID == 1003 || strings.Contains(name, "使用1小时"):
			key = "usage"
		case strings.Contains(name, "AI对话"):
			key = "chat"
		}
		if key == "" {
			continue
		}
		total := t.Total
		if total == 0 {
			if key == "usage" {
				total = 3600
			} else {
				total = 1
			}
		}
		state := "warning"
		label := "未完成"
		if t.Status == 2 || (total > 0 && t.Current >= total) {
			state = "success"
			label = "已完成"
		} else if t.Current > 0 {
			state = "running"
			label = "进行中"
		}
		out[key] = storage.TaskStatus{State: state, StateLabel: label, Current: t.Current, Total: total}
	}
	for _, k := range []string{"login", "usage", "chat"} {
		if _, ok := out[k]; !ok {
			out[k] = storage.TaskStatus{State: "warning", StateLabel: "待查询"}
		}
	}
	return out
}
func (m *Manager) RefreshStatus(ctx context.Context, id int64) (storage.PlatformStatus, error) {
	a, e := m.store.Account(id)
	if e != nil {
		return storage.PlatformStatus{}, e
	}
	c, e := m.nativeClient(ctx, a)
	if e != nil {
		return storage.PlatformStatus{}, e
	}
	tasks, e := c.Tasks(ctx)
	if e != nil {
		if ctyun.IsLoginExpired(e) {
			c.ClearProfile()
			m.store.ClearNativeAuthCache(id)
			if c, e = m.nativeClient(ctx, a); e == nil {
				tasks, e = c.Tasks(ctx)
			}
		}
	}
	if e != nil {
		return storage.PlatformStatus{}, e
	}
	points, e := c.Points(ctx)
	if e != nil {
		return storage.PlatformStatus{}, e
	}
	v := storage.PlatformStatus{AccountID: id, TotalPoints: &points, Tasks: normalizeTasks(tasks), UpdatedAt: storage.Now()}
	e = m.store.SavePlatform(v)
	return v, e
}
func (m *Manager) StartTask(id int64, typ, trigger string) (int64, error) {
	if typ != "login" && typ != "chat" && typ != "pc" && typ != "redeem" {
		return 0, errors.New("未知任务类型")
	}
	key := fmt.Sprintf("%d:%s", id, typ)
	m.mu.Lock()
	if m.maintenance {
		m.mu.Unlock()
		return 0, errors.New("系统正在准备更新，暂不接受新任务")
	}
	if _, ok := m.starting[key]; ok {
		m.mu.Unlock()
		return 0, errors.New("该任务已在运行")
	}
	for _, v := range m.active {
		if v.typ == key {
			m.mu.Unlock()
			return 0, errors.New("该任务已在运行")
		}
	}
	m.starting[key] = struct{}{}
	m.mu.Unlock()
	dir := filepath.Join(m.dataDir, "logs", "tasks")
	_ = os.MkdirAll(dir, 0750)
	path := filepath.Join(dir, fmt.Sprintf("%s-%d-%d.log", typ, id, time.Now().Unix()))
	runID, e := m.store.AddRun(id, typ, trigger, path)
	if e != nil {
		m.mu.Lock()
		delete(m.starting, key)
		m.mu.Unlock()
		return 0, e
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.mu.Lock()
	delete(m.starting, key)
	if m.maintenance {
		m.mu.Unlock()
		cancel()
		_ = m.store.UpdateRun(runID, "interrupted", "系统正在准备更新")
		return 0, errors.New("系统正在准备更新，暂不接受新任务")
	}
	m.active[runID] = running{cancel: cancel, typ: key, accountID: id}
	m.mu.Unlock()
	m.launch(func() { m.run(ctx, cancel, runID, id, typ, trigger, path) })
	return runID, nil
}
func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, runID, accountID int64, typ, trigger, path string) {
	defer cancel()
	_ = m.store.UpdateRun(runID, "running", "")
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	logger := log.New(f, "", log.LstdFlags)
	defer f.Close()
	taskName := map[string]string{"login": "登录 AI 云电脑", "chat": "AI 对话", "pc": "挂机", "redeem": "自动兑换"}[typ]
	automaticRedeem := typ == "redeem" && (trigger == "schedule" || trigger == "after_pc")
	if typ == "redeem" && !automaticRedeem {
		taskName = "立即兑换"
	}
	logger.Printf("开始%s任务", taskName)
	a, e := m.store.Account(accountID)
	execute := func() error {
		if typ == "chat" {
			return m.runChat(ctx, a, logger)
		}
		if typ == "redeem" {
			native, nativeErr := m.nativeClient(ctx, a)
			if nativeErr != nil {
				return nativeErr
			}
			return m.redeem(ctx, a, native, logger, automaticRedeem)
		}
		var c *ctyun.Client
		c, clientErr := m.client(ctx, a)
		if clientErr != nil {
			return clientErr
		}
		switch typ {
		case "login":
			return m.activateDesktopLogin(ctx, a, c, logger)
		case "pc":
			return m.runUsage(ctx, a, c, logger)
		}
		return nil
	}
	if e == nil {
		attempts := 1
		if trigger == "schedule" && typ != "redeem" {
			attempts = 3
		}
		for attempt := 1; attempt <= attempts; attempt++ {
			e = execute()
			if e == nil || errors.Is(e, context.Canceled) || attempt == attempts {
				break
			}
			logger.Printf("第 %d 次执行失败：%v；10 分钟后自动重试", attempt, e)
			select {
			case <-ctx.Done():
				e = ctx.Err()
				break
			case <-time.After(10 * time.Minute):
			}
			if e != nil && errors.Is(e, context.Canceled) {
				break
			}
			if status, refreshErr := m.RefreshStatus(ctx, accountID); refreshErr == nil {
				if task, ok := status.Tasks[scheduledTaskKey(typ)]; ok && task.State == "success" {
					logger.Printf("平台已确认今日任务完成，取消重试")
					e = nil
					break
				}
			}
		}
	}
	status := "success"
	message := "任务完成"
	if e != nil {
		status = "failed"
		message = e.Error()
		if errors.Is(e, context.Canceled) {
			status = "stopped"
			message = "任务已停止"
		}
		logger.Printf("%s", message)
	} else {
		_, _ = m.RefreshStatus(context.Background(), accountID)
		if typ == "pc" {
			if cfg, _ := m.store.Redeem(accountID); cfg.Enabled && m.redeemDue(accountID, cfg, time.Now()) {
				_, _ = m.StartTask(accountID, "redeem", "after_pc")
			}
		}
	}
	_ = m.store.UpdateRun(runID, status, message)
	m.mu.Lock()
	delete(m.active, runID)
	m.mu.Unlock()
}

func (m *Manager) activateDesktopLogin(ctx context.Context, a storage.Account, c *ctyun.Client, l *log.Logger) error {
	desktops, e := c.ListDesktops(ctx)
	if e != nil {
		return e
	}
	m.mu.RLock()
	liveSession := m.clients[a.ID] != nil && m.clients[a.ID].workers > 0
	m.mu.RUnlock()
	for _, d := range desktops {
		if d.Forbidden {
			continue
		}
		if !d.Running() {
			d, e = waitForDesktopRunning(ctx, c, d, func(message string) { l.Printf("%s：%s", d.Name(), message) })
			if e != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				l.Printf("%s 自动开机失败：%v", d.Name(), e)
				continue
			}
		}
		if e := c.ReportDesktopLogin(ctx, d); e != nil {
			l.Printf("平台登录事件上报未确认：%v", e)
		} else {
			l.Printf("平台登录事件已上报")
		}
		if liveSession {
			l.Printf("复用现有保活会话，避免建立重复桌面连接")
		} else {
			info, connectErr := waitForConnectionInfo(ctx, c, d, func(message string) { l.Printf("%s：%s", d.Name(), message) }, 90*time.Second)
			if connectErr != nil {
				l.Printf("%s 获取连接信息失败：%v", d.Name(), connectErr)
				continue
			}
			handshakeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			e = ctyun.ActivateClink(handshakeCtx, info, *c.Profile, a.DeviceCode, func(status string) { l.Printf("%s：%s", d.Name(), status) })
			cancel()
			if e != nil {
				l.Printf("%s 登录握手失败：%v", d.Name(), e)
				continue
			}
			l.Printf("登录会话握手完成，已发送桌面登录凭据")
		}
		native, nativeErr := m.nativeClient(ctx, a)
		if nativeErr != nil {
			l.Printf("原生积分状态登录失败：%v", nativeErr)
		}
		for attempt := 0; attempt < 6; attempt++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			if nativeErr != nil {
				continue
			}
			tasks, queryErr := native.Tasks(ctx)
			if queryErr != nil {
				l.Printf("等待平台同步时查询失败：%v", queryErr)
				continue
			}
			for _, task := range tasks {
				if task.ID == 1002 || strings.Contains(task.Name, "登录AI云电脑") {
					l.Printf("平台登录任务进度：%d/%d", task.Current, task.Total)
					if task.Status == 2 || (task.Total > 0 && task.Current >= task.Total) {
						l.Printf("平台已确认登录 AI 云电脑任务完成")
						return nil
					}
				}
			}
		}
		l.Printf("登录凭据已上报，平台状态可能稍后更新")
		return nil
	}
	return errors.New("没有可激活登录会话的运行中云电脑")
}
func (m *Manager) waitUsage(ctx context.Context, accountID int64, c *ctyun.NativeClient, l *log.Logger) error {
	deadline := time.Now().Add(80 * time.Minute)
	for {
		tasks, e := c.Tasks(ctx)
		if e != nil {
			return e
		}
		platforms, _ := m.store.Platforms()
		progress := platforms[accountID]
		progress.AccountID = accountID
		progress.Tasks = normalizeTasks(tasks)
		progress.Error = ""
		_ = m.store.SavePlatform(progress)
		for _, t := range tasks {
			if t.ID == 1003 || strings.Contains(t.Name, "使用1小时") {
				l.Printf("当前使用进度：%d/%d", t.Current, t.Total)
				if t.Status == 2 || (t.Total > 0 && t.Current >= t.Total) {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return errors.New("挂机等待超过 80 分钟")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

// runUsage uses an existing all-day keepalive when available. If keepalive is
// disabled, it creates one temporary desktop session and closes it as soon as
// the platform confirms the daily one-hour task.
func (m *Manager) runUsage(ctx context.Context, a storage.Account, c *ctyun.Client, l *log.Logger) error {
	native, e := m.nativeClient(ctx, a)
	if e != nil {
		return fmt.Errorf("原生积分状态登录：%w", e)
	}
	tasks, e := native.Tasks(ctx)
	if e == nil {
		if task, ok := normalizeTasks(tasks)["usage"]; ok && task.State == "success" {
			l.Printf("平台已确认今日时长任务完成，无需重复连接")
			return nil
		}
	}
	m.mu.RLock()
	liveSession := m.clients[a.ID] != nil && m.clients[a.ID].workers > 0
	m.mu.RUnlock()
	if liveSession {
		l.Printf("复用现有持续保活连接累计使用时长")
		return m.waitUsage(ctx, a.ID, native, l)
	}

	desktops, e := c.ListDesktops(ctx)
	if e != nil {
		return e
	}
	for _, desktop := range desktops {
		if desktop.Forbidden {
			continue
		}
		if !desktop.Running() {
			desktop, e = waitForDesktopRunning(ctx, c, desktop, func(message string) { l.Printf("%s：%s", desktop.Name(), message) })
			if e != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				l.Printf("%s 自动开机失败：%v", desktop.Name(), e)
				continue
			}
		}
		info, connectErr := waitForConnectionInfo(ctx, c, desktop, func(message string) { l.Printf("%s：%s", desktop.Name(), message) }, 90*time.Second)
		if connectErr != nil {
			l.Printf("%s 获取连接信息失败：%v", desktop.Name(), connectErr)
			continue
		}
		if c.Profile == nil {
			return errors.New("云电脑登录状态不可用")
		}
		if reportErr := c.ReportDesktopLogin(ctx, desktop); reportErr != nil {
			l.Printf("平台登录事件上报未确认：%v", reportErr)
		}
		temporaryCtx, stopTemporary := context.WithCancel(ctx)
		defer stopTemporary()
		activated := make(chan struct{})
		var activatedOnce sync.Once
		connectionErr := make(chan error, 1)
		refresh := func(refreshCtx context.Context) (ctyun.ConnectionInfo, error) {
			return waitForConnectionInfo(refreshCtx, c, desktop, nil, 30*time.Second)
		}
		profile := *c.Profile
		m.launch(func() {
			connectionErr <- ctyun.RunClink(temporaryCtx, info, profile, a.DeviceCode, refresh, func(status string) {
				l.Printf("%s：%s", desktop.Name(), status)
				if strings.Contains(status, "桌面登录会话已激活") {
					activatedOnce.Do(func() { close(activated) })
				}
			})
		})
		l.Printf("已启动临时时长连接，完成每日 1 小时任务后将自动断开")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case runErr := <-connectionErr:
			if runErr == nil {
				return errors.New("临时时长连接意外结束")
			}
			return fmt.Errorf("建立临时时长连接：%w", runErr)
		case <-activated:
			return m.waitUsage(ctx, a.ID, native, l)
		case <-time.After(60 * time.Second):
			return errors.New("等待临时时长连接激活超时")
		}
	}
	return errors.New("没有可用于时长任务的云电脑")
}
func (m *Manager) StopTask(id int64) bool {
	m.mu.RLock()
	r, ok := m.active[id]
	m.mu.RUnlock()
	if ok {
		r.cancel()
	}
	return ok
}
func (m *Manager) ActiveCount() int { m.mu.RLock(); defer m.mu.RUnlock(); return len(m.active) }

func (m *Manager) RunningAccountCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, state := range m.clients {
		if state != nil && state.workers > 0 {
			count++
		}
	}
	return count
}

// BlockingForUpdate returns labels for tasks that should prevent an in-place
// update, plus any redeem order whose result is still unknown.
func (m *Manager) BlockingForUpdate() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	labels := map[string]string{
		"login":  "登录任务",
		"chat":   "AI 对话任务",
		"pc":     "时长任务",
		"redeem": "兑换任务",
	}
	seen := map[string]bool{}
	var blocks []string
	for _, v := range m.active {
		typ := v.typ
		if i := strings.LastIndexByte(typ, ':'); i >= 0 {
			typ = typ[i+1:]
		}
		if label, ok := labels[typ]; ok && !seen[typ] {
			blocks = append(blocks, label)
			seen[typ] = true
		}
	}
	if pending, _ := m.store.HasPendingRedeem(); pending {
		blocks = append(blocks, "存在结果不确定的兑换订单")
	}
	return blocks
}

// PrepareForUpdate prevents new jobs, stops background keepalive connections,
// waits for short jobs and rejects updates while usage/redeem work is active.
func (m *Manager) PrepareForUpdate(ctx context.Context) error {
	m.mu.Lock()
	if m.maintenance {
		m.mu.Unlock()
		return errors.New("系统已处于更新维护状态")
	}
	m.maintenance = true
	m.mu.Unlock()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.RLock()
		var shortRunning bool
		var blocking string
		shortRunning = len(m.starting) > 0
		for _, item := range m.active {
			typ := item.typ
			if index := strings.LastIndexByte(typ, ':'); index >= 0 {
				typ = typ[index+1:]
			}
			switch typ {
			case "pc":
				blocking = "时长任务正在运行"
			case "redeem":
				blocking = "兑换任务正在运行"
			case "login", "chat":
				shortRunning = true
			}
		}
		m.mu.RUnlock()
		if blocking != "" {
			m.CancelUpdateMaintenance()
			return errors.New(blocking)
		}
		if pending, err := m.store.HasPendingRedeem(); err != nil {
			m.CancelUpdateMaintenance()
			return err
		} else if pending {
			m.CancelUpdateMaintenance()
			return errors.New("存在结果不确定的兑换订单")
		}
		if !shortRunning {
			m.mu.Lock()
			for _, state := range m.clients {
				if state != nil && state.cancel != nil {
					state.cancel()
				}
			}
			m.mu.Unlock()
			return nil
		}
		select {
		case <-ctx.Done():
			m.CancelUpdateMaintenance()
			return fmt.Errorf("等待短任务结束: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (m *Manager) CheckUpdateBlockers() error {
	m.mu.RLock()
	blocked := false
	for key := range m.starting {
		blocked = blocked || strings.HasSuffix(key, ":pc") || strings.HasSuffix(key, ":redeem")
	}
	for _, task := range m.active {
		blocked = blocked || strings.HasSuffix(task.typ, ":pc") || strings.HasSuffix(task.typ, ":redeem")
	}
	m.mu.RUnlock()
	if blocked {
		return errors.New("时长或兑换任务正在运行，请完成后重试")
	}
	pending, err := m.store.HasPendingRedeem()
	if err != nil {
		return err
	}
	if pending {
		return errors.New("存在结果不确定的兑换订单")
	}
	return nil
}

func (m *Manager) CancelUpdateMaintenance() {
	m.mu.Lock()
	wasMaintenance := m.maintenance
	m.maintenance = false
	m.mu.Unlock()
	if wasMaintenance && m.ctx.Err() == nil {
		m.reconcileKeepalive(time.Now())
	}
}

func (m *Manager) SchedulerHealthy() bool {
	return m.ctx.Err() == nil
}

func (m *Manager) RedeemCatalog(ctx context.Context, id int64) ([]ctyun.Reward, []ctyun.Desktop, error) {
	a, e := m.store.Account(id)
	if e != nil {
		return nil, nil, e
	}
	c, e := m.nativeClient(ctx, a)
	if e != nil {
		return nil, nil, e
	}
	r, e := c.Rewards(ctx)
	if e != nil {
		return nil, nil, e
	}
	d, e := c.Desktops(ctx)
	if e != nil {
		return nil, nil, e
	}
	available := r[:0]
	for _, reward := range r {
		if e := validateRedeemReward(reward, time.Now()); e == nil {
			available = append(available, reward)
		}
	}
	return available, d, nil
}

// AccountPoints reads the account's current marketplace points. The native
// login profile is shared with the other account requests, so opening the
// redeem page does not require a second platform login.
func (m *Manager) AccountPoints(ctx context.Context, id int64) (int, error) {
	a, err := m.store.Account(id)
	if err != nil {
		return 0, err
	}
	c, err := m.nativeClient(ctx, a)
	if err != nil {
		return 0, err
	}
	points, err := c.Points(ctx)
	if err == nil || !ctyun.IsLoginExpired(err) {
		return points, err
	}

	// A saved native profile can expire independently of the local session.
	// Clear it once and retry so the redeem page can refresh without requiring
	// the user to run another task first.
	c.ClearProfile()
	m.store.ClearNativeAuthCache(id)
	c, err = m.nativeClient(ctx, a)
	if err != nil {
		return 0, err
	}
	return c.Points(ctx)
}

func (m *Manager) ValidateRedeemConfig(ctx context.Context, id int64, cfg storage.RedeemConfig) (storage.RedeemConfig, error) {
	cfg.AccountID = id
	if !cfg.Enabled {
		if cfg.MaxTimes < 1 {
			cfg.MaxTimes = 1
		}
		if cfg.IntervalDays < 1 {
			cfg.IntervalDays = 1
		}
		if cfg.ScheduleType == "" {
			cfg.ScheduleType = "daily"
		}
		return cfg, nil
	}
	if cfg.MaxTimes < 1 {
		return cfg, errors.New("单次最多兑换次数必须大于 0")
	}
	switch cfg.ScheduleType {
	case "daily":
	case "interval":
		if cfg.IntervalDays < 1 {
			return cfg, errors.New("兑换间隔天数必须大于 0")
		}
	case "monthly":
		if _, e := monthlyRedeemDays(cfg.MonthlyDays); e != nil {
			return cfg, e
		}
	default:
		return cfg, errors.New("未知的兑换计划类型")
	}
	if cfg.ProductID == "" {
		return cfg, errors.New("启用自动兑换前必须选择商品")
	}
	return m.validateRedeemTarget(ctx, id, cfg)
}

func (m *Manager) ValidateImmediateRedeem(ctx context.Context, id int64, cfg storage.RedeemConfig) (storage.RedeemConfig, error) {
	cfg.AccountID = id
	if cfg.MaxTimes < 1 {
		return cfg, errors.New("单次最多兑换次数必须大于 0")
	}
	if cfg.ProductID == "" {
		return cfg, errors.New("立即兑换前必须选择商品")
	}
	return m.validateRedeemTarget(ctx, id, cfg)
}

func (m *Manager) validateRedeemTarget(ctx context.Context, id int64, cfg storage.RedeemConfig) (storage.RedeemConfig, error) {
	old, e := m.store.Redeem(id)
	if e != nil {
		return cfg, e
	}
	var pending string
	e = m.store.DB.QueryRow("SELECT last_attempt_status FROM redeem_states WHERE account_id=?", id).Scan(&pending)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return cfg, e
	}
	if pending == "pending" && (old.ProductID != cfg.ProductID || old.DesktopID != cfg.DesktopID) {
		return cfg, errors.New("上一笔兑换结果待确认，不能更换兑换目标")
	}
	rewards, desktops, e := m.RedeemCatalog(ctx, id)
	if e != nil {
		return cfg, fmt.Errorf("读取实时兑换目录：%w", e)
	}
	var selectedReward *ctyun.Reward
	for _, reward := range rewards {
		if strconv.FormatInt(reward.ProductID, 10) == cfg.ProductID {
			cfg.ProductName = reward.ProductName
			cfg.ProductType = reward.ProductType
			cfg.CostPoints = reward.CostPoints
			selectedReward = &reward
			break
		}
	}
	if selectedReward == nil {
		return cfg, errors.New("选择的兑换商品已下架或当前不可兑换")
	}
	if !ctyun.RewardNeedsDesktop(*selectedReward) {
		cfg.DesktopID = ""
		return cfg, nil
	}
	if strings.TrimSpace(cfg.DesktopID) == "" {
		return cfg, errors.New("该商品必须选择目标云电脑")
	}
	for _, desktop := range desktops {
		if desktop.ID() == cfg.DesktopID {
			return cfg, nil
		}
	}
	return cfg, errors.New("选择的目标云电脑已不存在或不属于当前账号")
}

func (m *Manager) redeem(ctx context.Context, a storage.Account, c *ctyun.NativeClient, l *log.Logger, automatic bool) error {
	cfg, e := m.store.Redeem(a.ID)
	if e != nil {
		return e
	}
	if automatic && !cfg.Enabled {
		return errors.New("自动兑换未启用")
	}
	var state struct{ LastAttemptDate, LastAttemptStatus string }
	e = m.store.DB.QueryRow("SELECT last_attempt_date,last_attempt_status FROM redeem_states WHERE account_id=?", a.ID).Scan(&state.LastAttemptDate, &state.LastAttemptStatus)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	today := time.Now().Format("2006-01-02")
	if state.LastAttemptStatus == "pending" {
		return errors.New("上一笔兑换结果待确认，已停止自动兑换")
	}
	if automatic && state.LastAttemptDate == today {
		return errors.New("今日已执行过兑换检查")
	}
	products, e := c.Rewards(ctx)
	if e != nil {
		return e
	}
	var found *ctyun.Reward
	for i := range products {
		if strconv.FormatInt(products[i].ProductID, 10) == cfg.ProductID {
			found = &products[i]
			break
		}
	}
	if found == nil {
		return errors.New("配置的兑换商品已下架")
	}
	if cfg.ProductType != "" && found.ProductType != cfg.ProductType {
		return errors.New("商品类型已变化，请重新确认兑换配置")
	}
	if cfg.CostPoints > 0 && found.CostPoints != cfg.CostPoints {
		return fmt.Errorf("商品积分价格已从 %d 变为 %d，请重新确认兑换配置", cfg.CostPoints, found.CostPoints)
	}
	if e := validateRedeemReward(*found, time.Now()); e != nil {
		return e
	}
	points, e := c.Points(ctx)
	if e != nil {
		return e
	}
	if found.CostPoints <= 0 {
		return errors.New("商品积分价格无效")
	}
	times := cfg.MaxTimes
	if possible := points / found.CostPoints; times > possible {
		times = possible
	}
	if times < 1 {
		return fmt.Errorf("积分不足：当前 %d，需要 %d", points, found.CostPoints)
	}
	desktop := &ctyun.Desktop{}
	if ctyun.RewardNeedsDesktop(*found) {
		desktops, desktopErr := c.Desktops(ctx)
		if desktopErr != nil {
			return fmt.Errorf("兑换前验证云电脑：%w", desktopErr)
		}
		desktop = nil
		for i := range desktops {
			if desktops[i].ID() == cfg.DesktopID {
				desktop = &desktops[i]
				break
			}
		}
		if desktop == nil {
			return errors.New("配置的目标云电脑已不存在或不属于当前账号")
		}
	}
	beforeStatistic, statisticSnapshotErr := c.RedemptionStatisticCount(ctx, *found)
	if statisticSnapshotErr != nil {
		l.Printf("兑换前统计读取失败，不影响平台下单结果：%v", statisticSnapshotErr)
	}
	pointsSpent := found.CostPoints * times
	_, e = m.store.DB.Exec(`INSERT INTO redeem_states(account_id,last_attempt_date,last_attempt_status,last_redeem_times,last_points_spent,message,updated_at) VALUES(?,?,'pending',?,?,'订单提交中',?) ON CONFLICT(account_id) DO UPDATE SET last_attempt_date=excluded.last_attempt_date,last_attempt_status='pending',last_redeem_times=excluded.last_redeem_times,last_points_spent=excluded.last_points_spent,message='订单提交中',updated_at=excluded.updated_at`, a.ID, today, times, pointsSpent, storage.Now())
	if e != nil {
		return e
	}
	if ctyun.RewardNeedsDesktop(*found) {
		l.Printf("兑换前复核：%s，目标云电脑 %s，数量 %d，预计 %d 积分", found.ProductName, desktop.Name(), times, pointsSpent)
	} else {
		l.Printf("兑换前复核：%s，数量 %d，预计 %d 积分", found.ProductName, times, pointsSpent)
	}
	receipt, orderErr := c.PlaceOrder(ctx, *found, times, *desktop)
	e = orderErr
	status := "success"
	msg := "兑换成功"
	if e != nil {
		if ctyun.IsPlatformError(e) {
			status = "failed"
			msg = "兑换失败：" + e.Error()
			if ctyun.IsRiskControl(e) {
				msg += "；已自动关闭后续自动兑换"
				_, _ = m.store.DB.Exec("UPDATE redeem_configs SET enabled=0,updated_at=? WHERE account_id=?", storage.Now(), a.ID)
				l.Printf("检测到平台风控，已关闭该账号的自动兑换，请勿反复提交；如需恢复，请先联系平台管理员确认")
			}
		} else {
			status = "pending"
			msg = "订单结果不确定：" + e.Error()
		}
	} else {
		reference := receipt.Reference()
		if reference != "" {
			l.Printf("平台返回下单成功：%s，正在辅助核对积分和兑换统计", reference)
		} else {
			l.Printf("平台返回下单成功（code=0），正在辅助核对积分和兑换统计")
		}
		observation := observeRedeemResult(ctx, c, *found, points, pointsSpent, beforeStatistic, statisticSnapshotErr == nil)
		if reference != "" {
			msg = fmt.Sprintf("兑换成功（订单 %s；%s）", reference, observation)
		} else {
			msg = fmt.Sprintf("兑换成功（平台返回 code=0；%s）", observation)
		}
		l.Printf("%s", msg)
	}
	_, stateErr := m.store.DB.Exec(`UPDATE redeem_states SET last_attempt_status=?,last_success_date=CASE WHEN ?='success' THEN ? ELSE last_success_date END,last_redeem_times=?,last_points_spent=?,message=?,updated_at=? WHERE account_id=?`, status, status, today, times, pointsSpent, msg, storage.Now(), a.ID)
	return errors.Join(e, stateErr)
}

func observeRedeemResult(ctx context.Context, c *ctyun.NativeClient, reward ctyun.Reward, pointsBefore, pointsSpent, statisticBefore int, hasStatisticSnapshot bool) string {
	observations := make([]string, 0, 2)
	pointsAfter, pointsErr := c.Points(ctx)
	if pointsErr != nil {
		observations = append(observations, "积分复查失败，不影响 code=0 的成功结果")
	} else if pointsAfter <= pointsBefore-pointsSpent {
		observations = append(observations, fmt.Sprintf("积分已由 %d 扣减至 %d", pointsBefore, pointsAfter))
	} else {
		observations = append(observations, fmt.Sprintf("积分数据暂未同步（当前 %d）", pointsAfter))
	}

	statisticAfter, statisticErr := c.RedemptionStatisticCount(ctx, reward)
	if statisticErr != nil {
		observations = append(observations, "兑换统计复查失败")
	} else if hasStatisticSnapshot && statisticAfter > statisticBefore {
		observations = append(observations, fmt.Sprintf("兑换统计已由 %d 增至 %d", statisticBefore, statisticAfter))
	} else {
		observations = append(observations, "兑换统计暂未变化")
	}
	return strings.Join(observations, "；")
}
func (m *Manager) BeginVerification(ctx context.Context, id int64) (bool, error) {
	a, e := m.store.Account(id)
	if e != nil {
		return false, e
	}
	c, e := m.newClient(a)
	if e != nil {
		return false, e
	}
	p, e := c.Login(ctx)
	if e != nil {
		return false, e
	}
	if p.BondedDevice {
		_ = m.store.SetDeviceStatus(id, "verified")
		return true, nil
	}
	if e = c.SendSMS(ctx); e != nil {
		return false, e
	}
	m.mu.Lock()
	m.verify[id] = VerifySession{c, time.Now().Add(10 * time.Minute)}
	m.mu.Unlock()
	_ = m.store.SetDeviceStatus(id, "pending")
	return false, nil
}
func (m *Manager) CompleteVerification(ctx context.Context, id int64, code string) error {
	m.mu.RLock()
	v, ok := m.verify[id]
	m.mu.RUnlock()
	if !ok || time.Now().After(v.Expires) {
		return errors.New("短信验证会话已过期，请重新获取")
	}
	if e := v.Client.BindDevice(ctx, code); e != nil {
		return e
	}
	_ = m.store.SetDeviceStatus(id, "verified")
	m.mu.Lock()
	delete(m.verify, id)
	m.mu.Unlock()
	m.RestartKeepalive()
	return nil
}

func (m *Manager) scheduler() {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	lastLogCleanup := ""
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.reconcileKeepalive(now)
			if day := now.Format("20060102"); day != lastLogCleanup {
				if e := m.CleanupExpiredLogs(); e != nil {
					m.logf("自动清理过期日志失败：%v", e)
				}
				lastLogCleanup = day
			}
			accounts, _ := m.store.Accounts()
			for _, a := range accounts {
				if !a.Enabled || a.DeviceStatus == "pending" {
					continue
				}
				checks := []struct {
					on        bool
					expr, typ string
				}{{a.LoginEnabled, a.LoginCron, "login"}, {a.PCEnabled, a.PCCron, "pc"}, {a.ChatEnabled, a.ChatCron, "chat"}}
				for _, x := range checks {
					if !x.on {
						continue
					}
					s, e := parser.Parse(x.expr)
					if e != nil {
						continue
					}
					prev := s.Next(now.Add(-time.Minute - time.Second))
					if !prev.After(now) {
						minute := now.Format("200601021504")
						m.mu.RLock()
						maintenance := m.maintenance
						m.mu.RUnlock()
						if !maintenance && m.store.Claim(a.ID, x.typ, minute) {
							a := a
							m.launch(func() { m.startScheduledTask(a, x.typ, now, minute) })
						}
					}
				}
				if now.Hour() == 6 && now.Minute() == 5 {
					cfg, _ := m.store.Redeem(a.ID)
					claimKey := now.Format("20060102")
					m.mu.RLock()
					maintenance := m.maintenance
					m.mu.RUnlock()
					if cfg.Enabled && !maintenance && m.redeemDue(a.ID, cfg, now) && m.store.Claim(a.ID, "redeem", claimKey) {
						if _, err := m.StartTask(a.ID, "redeem", "schedule"); err != nil && !strings.Contains(err.Error(), "已在运行") {
							_ = m.store.ReleaseClaim(a.ID, "redeem", claimKey)
							m.logf("[%s] 启动定时兑换失败：%v", a.Name, err)
						}
					}
				}
			}
		}
	}
}

func (m *Manager) redeemDue(id int64, cfg storage.RedeemConfig, now time.Time) bool {
	var lastAttempt, lastSuccess, status string
	e := m.store.DB.QueryRow("SELECT last_attempt_date,last_success_date,last_attempt_status FROM redeem_states WHERE account_id=?", id).Scan(&lastAttempt, &lastSuccess, &status)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return false
	}
	today := now.Format("2006-01-02")
	if status == "pending" || lastAttempt == today {
		return false
	}
	switch cfg.ScheduleType {
	case "monthly":
		days, e := monthlyRedeemDays(cfg.MonthlyDays)
		if e != nil {
			return false
		}
		lastDay := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, now.Location()).Day()
		for _, day := range days {
			if day == now.Day() || (day == -1 && now.Day() == lastDay) {
				return true
			}
		}
		return false
	case "interval":
		if lastSuccess == "" {
			return true
		}
		t, e := time.ParseInLocation("2006-01-02", lastSuccess, now.Location())
		return e == nil && !now.Before(t.AddDate(0, 0, cfg.IntervalDays))
	default:
		return lastAttempt != today
	}
}
func (m *Manager) ResolveRedeem(id int64, succeeded bool) error {
	status := "failed"
	msg := "已人工确认未成功"
	if succeeded {
		status = "success"
		msg = "已人工确认成功"
	}
	result, e := m.store.DB.Exec(`UPDATE redeem_states SET last_attempt_status=?,last_success_date=CASE WHEN ?='success' THEN last_attempt_date ELSE last_success_date END,message=?,updated_at=? WHERE account_id=? AND last_attempt_status='pending'`, status, status, msg, storage.Now(), id)
	if e != nil {
		return e
	}
	affected, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if affected == 0 {
		return errors.New("当前没有待确认的兑换订单")
	}
	return nil
}

func validateRedeemReward(reward ctyun.Reward, now time.Time) error {
	if reward.ProductID <= 0 || strings.TrimSpace(reward.ProductType) == "" || reward.CostPoints <= 0 {
		return errors.New("商品参数无效")
	}
	if reward.Status != 0 && reward.Status != 2 {
		return fmt.Errorf("商品当前不可兑换（状态 %d）", reward.Status)
	}
	checks := []struct {
		value     string
		effective bool
	}{{reward.EffectiveAt, true}, {reward.ExpiresAt, false}}
	for _, check := range checks {
		value := strings.TrimSpace(check.value)
		if value == "" {
			continue
		}
		var parsed time.Time
		var e error
		dateOnly := false
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
			parsed, e = time.ParseInLocation(layout, value, now.Location())
			if e == nil {
				dateOnly = layout == "2006-01-02"
				break
			}
		}
		if e != nil {
			continue
		}
		if check.effective && now.Before(parsed) {
			return errors.New("商品尚未生效")
		}
		if !check.effective && dateOnly {
			parsed = parsed.AddDate(0, 0, 1)
		}
		if !check.effective && !now.Before(parsed) {
			return errors.New("商品已经过期")
		}
	}
	return nil
}

func monthlyRedeemDays(value string) ([]int, error) {
	var days []int
	seen := map[int]bool{}
	for _, raw := range strings.Split(value, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		day, e := strconv.Atoi(raw)
		if e != nil || (day != -1 && (day < 1 || day > 31)) {
			return nil, fmt.Errorf("每月兑换日期 %q 无效", raw)
		}
		if !seen[day] {
			days = append(days, day)
			seen[day] = true
		}
	}
	if len(days) == 0 {
		return nil, errors.New("每月兑换日期不能为空")
	}
	return days, nil
}
func (m *Manager) MarshalStatus() []byte {
	state, workers := m.KeepaliveStatus()
	b, _ := json.Marshal(map[string]any{"state": state, "workers": workers, "activeTasks": m.ActiveCount(), "uptimeSeconds": int(time.Since(m.started).Seconds())})
	return b
}
