package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vay1314/CtYun-Keeper/internal/security"
	"github.com/vay1314/CtYun-Keeper/internal/storage"
)

func TestFormatTime(t *testing.T) {
	tests := map[string]string{
		"2026-09-07T21:35:47+08:00": "2026-09-07 21:35:47",
		"2026-09-07T13:35:47Z":      "2026-09-07 13:35:47",
		"2026-09-07 21:35:47":       "2026-09-07 21:35:47",
		"":                          "尚未更新",
	}
	for input, want := range tests {
		if got := formatTime(input); got != want {
			t.Errorf("formatTime(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPageLoadsThemeBootstrapUnderContentSecurityPolicy(t *testing.T) {
	server := &Server{version: "test", sessionKey: []byte("test-session-key")}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()

	server.page(recorder, request, "test", "content", false)
	body := recorder.Body.String()
	themeScript := strings.Index(body, "<script src='/static/theme.js?")
	stylesheet := strings.Index(body, "<link rel=stylesheet")
	if themeScript < 0 {
		t.Fatal("page does not load the external theme bootstrap")
	}
	if stylesheet < 0 || themeScript > stylesheet {
		t.Fatal("theme bootstrap must run before the stylesheet is loaded")
	}
	if strings.Contains(body, "localStorage.getItem") {
		t.Fatal("page contains an inline theme script blocked by the content security policy")
	}
}

func TestValidateKeepaliveSettings(t *testing.T) {
	valid := storage.Account{KeepaliveMode: storage.KeepaliveScheduled, KeepaliveStart: "22:00", KeepaliveEnd: "06:00", KeepaliveWeekdays: "1,3,5"}
	if err := validateKeepaliveSettings(valid); err != nil {
		t.Fatalf("valid cross-midnight schedule was rejected: %v", err)
	}
	for _, account := range []storage.Account{
		{KeepaliveMode: "unknown"},
		{KeepaliveMode: storage.KeepaliveScheduled, KeepaliveStart: "08:00", KeepaliveEnd: "08:00", KeepaliveWeekdays: "1"},
		{KeepaliveMode: storage.KeepaliveScheduled, KeepaliveStart: "08:00", KeepaliveEnd: "09:00"},
		{KeepaliveMode: storage.KeepaliveScheduled, KeepaliveStart: "08:00", KeepaliveEnd: "09:00", KeepaliveWeekdays: "8"},
	} {
		if err := validateKeepaliveSettings(account); err == nil {
			t.Fatalf("invalid keepalive settings were accepted: %#v", account)
		}
	}
}

func TestInlineAccountAndTaskToggles(t *testing.T) {
	account := accountToggle(storage.Account{ID: 7, Enabled: true})
	if !strings.Contains(account, `/accounts/7/enabled`) || !strings.Contains(account, `value=0`) || !strings.Contains(account, `已启用`) {
		t.Fatalf("enabled account toggle = %q", account)
	}
	task := scheduleSummary(7, "chat", false, "10 3 * * *")
	for _, want := range []string{`/accounts/7/tasks/chat/enabled`, `value=1`, `关闭`, `10 3 * * *`} {
		if !strings.Contains(task, want) {
			t.Fatalf("disabled task toggle does not contain %q: %s", want, task)
		}
	}
}

func TestPasswordAuthenticationCanBeDisabled(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, sessionKey: []byte("test-session-key")}
	request := httptest.NewRequest("GET", "/", nil)
	if !server.passwordAuthEnabled() {
		t.Fatal("password authentication must default to enabled")
	}
	if server.authed(request) {
		t.Fatal("request without a session must not be authenticated by default")
	}
	if err = store.SetSetting("admin_auth_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	if err = store.SetSetting("admin_password_hash", "configured"); err != nil {
		t.Fatal(err)
	}
	if server.passwordAuthEnabled() {
		t.Fatal("password authentication setting was not disabled")
	}
	if !server.authed(request) {
		t.Fatal("LAN access should be authorized while password authentication is disabled")
	}

	recorder := httptest.NewRecorder()
	server.login(recorder, request)
	if recorder.Code != 303 || recorder.Header().Get("Location") != "/" {
		t.Fatalf("disabled login redirect = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestAuthSettingsUsesAuthenticatedSessionWithoutPassword(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.SetSetting("admin_password_hash", security.HashPassword("password1")); err != nil {
		t.Fatal(err)
	}
	key := []byte("test-session-key")
	server := &Server{store: store, sessionKey: key}
	form := url.Values{"csrf_token": {"csrf-value"}, "auth_enabled": {"false"}}
	request := httptest.NewRequest(http.MethodPost, "/settings/auth", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "ctyun_session", Value: security.SignCookie(key, "authenticated")})
	request.AddCookie(&http.Cookie{Name: "ctyun_csrf", Value: security.SignCookie(key, "csrf-value")})
	recorder := httptest.NewRecorder()
	server.authSettings(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("auth settings response = %d", recorder.Code)
	}
	if server.passwordAuthEnabled() {
		t.Fatal("password authentication remained enabled")
	}
}

func TestReverseLogText(t *testing.T) {
	tests := map[string]string{
		"":                         "",
		"one":                      "one",
		"old\nnew\n":               "new\nold\n",
		"old\r\nmiddle\r\nnew\r\n": "new\nmiddle\nold\n",
	}
	for input, want := range tests {
		if got := reverseLogText(input); got != want {
			t.Errorf("reverseLogText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPlatformStatusUpdatedToday(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 9, 2, 0, 5, 0, 0, location)
	tests := map[string]bool{
		"2026-09-02T00:01:00+08:00": true,
		"2026-09-01T23:59:59+08:00": false,
		"2026-09-01T16:01:00Z":      true,
		"2026-09-02 00:03:00":       true,
		"":                          false,
		"invalid":                   false,
	}
	for updatedAt, want := range tests {
		if got := platformStatusUpdatedToday(updatedAt, now); got != want {
			t.Errorf("platformStatusUpdatedToday(%q) = %v, want %v", updatedAt, got, want)
		}
	}
}
