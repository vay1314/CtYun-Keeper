package web

import (
	"context"
	"encoding/json"
	"github.com/vay1314/CtYun-Keeper/internal/security"
	"github.com/vay1314/CtYun-Keeper/internal/storage"
	"github.com/vay1314/CtYun-Keeper/internal/update"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNextAutomaticUpdateCheckUsesLocalFourAM(t *testing.T) {
	location := time.FixedZone("test", 8*60*60)
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "before scheduled time",
			now:  time.Date(2026, 9, 11, 3, 59, 59, 0, location),
			want: time.Date(2026, 9, 11, 4, 0, 0, 0, location),
		},
		{
			name: "at scheduled time",
			now:  time.Date(2026, 9, 11, 4, 0, 0, 0, location),
			want: time.Date(2026, 9, 12, 4, 0, 0, 0, location),
		},
		{
			name: "after scheduled time",
			now:  time.Date(2026, 9, 11, 23, 0, 0, 0, location),
			want: time.Date(2026, 9, 12, 4, 0, 0, 0, location),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextAutomaticUpdateCheck(tt.now); !got.Equal(tt.want) || got.Location() != location {
				t.Fatalf("next check = %v (%v), want %v (%v)", got, got.Location(), tt.want, location)
			}
		})
	}
}

func TestUpdateMessagesRenderInOrderAndUseFiniteDurations(t *testing.T) {
	markup := renderUpdateMessageQueue([]updateCardMessage{
		{Level: "error", Text: "第一条错误"},
		{Level: "success", Text: "第二条消息"},
	})
	first := strings.Index(markup, "第一条错误")
	second := strings.Index(markup, "第二条消息")
	if first < 0 || second <= first {
		t.Fatalf("update messages were not rendered in order: %s", markup)
	}
	for _, want := range []string{"data-update-message", "data-duration=6000", "data-duration=4000", "role=alert"} {
		if !strings.Contains(markup, want) {
			t.Fatalf("update queue does not contain %q: %s", want, markup)
		}
	}
}

func TestRedirectUpdateTargetsDeploymentCard(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings/update/check", nil)
	redirectUpdate(w, r, "更新服务未初始化", true)
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusSeeOther || location.Path != "/settings" || location.Query().Get("update_message") != "更新服务未初始化" || location.Query().Get("update_level") != "error" {
		t.Fatalf("unexpected update redirect: status=%d location=%q", w.Code, location.String())
	}
	if location.Query().Get("notice") != "" || location.Query().Get("error") != "" {
		t.Fatalf("update redirect still targets the page-level flash: %s", location.String())
	}
}

func TestUpdateAvailableBadgeOnlyShowsInstallableUpdate(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &Server{store: store, version: "2.1.0"}

	raw, _ := json.Marshal(updateState{LatestVersion: "2.2.0", HasUpdate: true, Supported: true})
	if err := store.SetSetting("update_state", string(raw)); err != nil {
		t.Fatal(err)
	}
	badge := s.updateAvailableBadge("dashboard")
	for _, want := range []string{"/static/update-available.svg?v=2", "update-available-dashboard", "发现可用更新"} {
		if !strings.Contains(badge, want) {
			t.Fatalf("badge does not contain %q: %s", want, badge)
		}
	}

	raw, _ = json.Marshal(updateState{LatestVersion: "2.2.0", HasUpdate: true, Supported: false})
	_ = store.SetSetting("update_state", string(raw))
	if badge := s.updateAvailableBadge("dashboard"); badge != "" {
		t.Fatalf("unsupported update rendered a badge: %s", badge)
	}
	raw, _ = json.Marshal(updateState{LatestVersion: "2.2.0", HasUpdate: true, Supported: true})
	_ = store.SetSetting("update_state", string(raw))
	s.version = "dev"
	if badge := s.updateAvailableBadge("dashboard"); badge == "" {
		t.Fatal("development build did not render an available formal update")
	}
}

func TestAuthenticatedPageRendersBothUpdateBadges(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, _ := json.Marshal(updateState{LatestVersion: "2.2.0", HasUpdate: true, Supported: true})
	if err := store.SetSetting("update_state", string(raw)); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, version: "2.1.0", sessionKey: []byte("test-key")}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	s.page(w, r, "仪表盘", `<span>当前版本</span>`+s.updateAvailableBadgeSlot("dashboard"), true)
	body := w.Body.String()
	if got := strings.Count(body, `/static/update-available.svg`); got != 2 {
		t.Fatalf("rendered %d update icons, want 2", got)
	}
	for _, want := range []string{"update-available-dashboard", "update-available-sidebar", "ui20"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page does not contain %q", want)
		}
	}
}

func TestAutomaticCheckSkipsActiveUpdateTask(t *testing.T) {
	manager := update.NewManager(t.TempDir())
	if err := manager.Begin(); err != nil {
		t.Fatal(err)
	}
	defer manager.Finish()
	manager.SetProgress(update.Progress{Status: update.StatusDownloading, Message: "安装进行中"})
	s := &Server{ctx: context.Background(), updateManager: manager}
	s.runAutomaticUpdateCheck()
	progress := manager.Progress()
	if !manager.Active() || progress.Status != update.StatusDownloading || progress.Message != "安装进行中" {
		t.Fatalf("automatic check disturbed active task: active=%v progress=%+v", manager.Active(), progress)
	}
}

func TestUpdateBadgePartialValidatesLocation(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.SetSetting("admin_auth_enabled", "false")
	_ = store.SetSetting("admin_password_hash", security.HashPassword("test-password"))
	raw, _ := json.Marshal(updateState{LatestVersion: "2.2.0", HasUpdate: true, Supported: true})
	_ = store.SetSetting("update_state", string(raw))
	s := &Server{store: store, version: "2.1.0"}

	w := httptest.NewRecorder()
	s.updateBadgePartial(w, httptest.NewRequest(http.MethodGet, "/partials/update-badge?location=sidebar", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "update-available-sidebar") {
		t.Fatalf("valid badge response = %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.updateBadgePartial(w, httptest.NewRequest(http.MethodGet, "/partials/update-badge?location=invalid", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid badge location returned %d", w.Code)
	}
}

func TestSavedEmptyProxyOverridesEnvironment(t *testing.T) {
	t.Setenv("GITHUB_PROXY", "https://example.com/")
	store, err := storage.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	makeServer := func() *Server {
		return New(store, nil, nil, nil, "2.1.0", t.TempDir(), t.TempDir(), "", "", false, nil)
	}
	s := makeServer()
	if s.updater.Proxy() != "https://example.com/" {
		t.Fatal("missing environment default")
	}
	s.Close()
	_ = store.SetSetting("github_proxy", "")
	s = makeServer()
	defer s.Close()
	if s.updater.Proxy() != "" {
		t.Fatal("saved direct setting ignored")
	}
}

func TestDefaultProxyCanBeCleared(t *testing.T) {
	t.Setenv("GITHUB_PROXY", "")
	store, err := storage.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	makeServer := func() *Server {
		return New(store, nil, nil, nil, "2.1.0", t.TempDir(), t.TempDir(), "", "", false, nil)
	}
	s := makeServer()
	if s.updater.Proxy() != defaultGitHubProxy {
		t.Fatalf("default proxy = %q, want %q", s.updater.Proxy(), defaultGitHubProxy)
	}
	s.Close()
	if err := store.SetSetting("github_proxy", ""); err != nil {
		t.Fatal(err)
	}
	s = makeServer()
	defer s.Close()
	if s.updater.Proxy() != "" {
		t.Fatal("saved empty proxy did not select direct access")
	}
}

func TestRestartedServerShowsResultAndCurrentVersion(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, _ := json.Marshal(updateState{LatestVersion: "2.1.0", HasUpdate: true, Supported: true, Message: "发现新版本"})
	_ = store.SetSetting("update_state", string(raw))
	if err := store.RecordUpdateResult(42, "2.0.0", "2.1.0", "windows-amd64", "success", "更新成功", "now"); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, version: "2.1.0", updateManager: update.NewManager()}
	if s.savedUpdateState().HasUpdate {
		t.Fatal("stale update button")
	}
	if p := s.currentUpdateProgress(); p.Status != update.StatusSuccess || !strings.Contains(p.Message, "成功") {
		t.Fatalf("lost result: %+v", p)
	}
}

func TestDevVersionUsesSavedFormalUpdateState(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, _ := json.Marshal(updateState{LatestVersion: "2.2.0", HasUpdate: true, Supported: true, Message: "发现新版本"})
	_ = store.SetSetting("update_state", string(raw))
	s := &Server{store: store, version: "dev", updateManager: update.NewManager()}
	state := s.savedUpdateState()
	if !state.HasUpdate || !state.CurrentIsDev || state.LatestVersion != "2.2.0" {
		t.Fatalf("development build did not retain formal update state: %+v", state)
	}
}

func TestFreshIdleCheckDoesNotResurfaceOldHistory(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecordUpdateResult(42, "2.0.0", "2.1.0", "windows-amd64", "success", "旧更新成功", "now"); err != nil {
		t.Fatal(err)
	}
	manager := update.NewManager()
	manager.SetProgress(update.Progress{Status: update.StatusIdle, Message: "已是最新版本", FromVersion: "2.1.0", TargetVersion: "2.1.0"})
	s := &Server{store: store, version: "2.1.0", updateManager: manager}
	progress := s.currentUpdateProgress()
	if progress.Status != update.StatusIdle || progress.Message != "已是最新版本" {
		t.Fatalf("old history replaced fresh check result: %+v", progress)
	}
}

func TestInstallRequiresCSRFEvenWithoutLogin(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.SetSetting("admin_auth_enabled", "false")
	_ = store.SetSetting("admin_password_hash", security.HashPassword("correct-password"))
	key := []byte("test-key")
	s := &Server{store: store, sessionKey: key}
	for _, csrf := range []bool{false, true} {
		form := url.Values{"csrf_token": {"token"}}
		r := httptest.NewRequest(http.MethodPost, "/settings/update/install", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if csrf {
			r.AddCookie(&http.Cookie{Name: "ctyun_csrf", Value: security.SignCookie(key, "token")})
		}
		w := httptest.NewRecorder()
		s.installUpdate(w, r)
		if !csrf {
			location, _ := url.Parse(w.Header().Get("Location"))
			if w.Code != http.StatusSeeOther || !strings.Contains(location.Query().Get("update_message"), "请求验证失败") || location.Query().Get("update_level") != "error" {
				t.Fatalf("missing CSRF was not rejected in the update card: status=%d location=%q", w.Code, location.String())
			}
		}
		if csrf {
			location, _ := url.QueryUnescape(w.Header().Get("Location"))
			if strings.Contains(location, "管理密码") {
				t.Fatalf("install still requested an admin password: %s", location)
			}
		}
	}
}

func TestProxyCanBeChangedWithoutPassword(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.SetSetting("admin_auth_enabled", "false")
	_ = store.SetSetting("admin_password_hash", security.HashPassword("correct-password"))
	key := []byte("test-key")
	s := New(store, nil, key, nil, "2.1.0", t.TempDir(), t.TempDir(), "", "", false, nil)
	defer s.Close()
	form := url.Values{"csrf_token": {"token"}, "github_proxy": {"https://proxy.example.com"}}
	r := httptest.NewRequest(http.MethodPost, "/settings/update/proxy", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "ctyun_csrf", Value: security.SignCookie(key, "token")})
	w := httptest.NewRecorder()
	s.saveUpdateProxy(w, r)
	if got, err := store.Setting("github_proxy"); err != nil || got != "https://proxy.example.com" {
		t.Fatalf("proxy was not saved without a password: %q, %v", got, err)
	}
	raw, _ := json.Marshal(updateState{LatestVersion: "2.2.0", HasUpdate: true, Supported: true})
	_ = store.SetSetting("update_state", string(raw))
	card := s.deploymentCard()
	if !strings.Contains(card, "在线更新") {
		t.Fatal("deployment card did not render the install action")
	}
	for _, removed := range []string{"测试代理", "回滚上一版本", "/settings/update/proxy/test", "/settings/update/rollback", "admin_password", ">管理密码<"} {
		if strings.Contains(card, removed) {
			t.Fatalf("deployment card still contains removed control %q", removed)
		}
	}
}

func TestDeploymentCardRejectsUnsafeSavedReleaseURL(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, _ := json.Marshal(updateState{
		LatestVersion: "2.2.0", LatestTag: "v2.2.0", HasUpdate: true, Supported: true,
		ReleaseURL: "javascript:alert(1)",
	})
	if err := store.SetSetting("update_state", string(raw)); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, version: "2.1.0", updater: update.NewChecker("", "2.1.0", "", update.Platform{OS: "windows", Arch: "amd64"}), updateManager: update.NewManager()}
	if card := s.deploymentCard(); strings.Contains(card, "javascript:") || strings.Contains(card, "查看更新说明") {
		t.Fatalf("unsafe persisted release link was rendered: %s", card)
	}
}
