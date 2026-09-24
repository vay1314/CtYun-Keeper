package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vay1314/CtYun-Keeper/internal/ctyun"
	"github.com/vay1314/CtYun-Keeper/internal/storage"
)

func TestObserveRedeemResultReportsPointsAndStatistics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/selforder/api/marketing/userPoints/getUserPoints":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []map[string]any{{"pointType": 1, "points": 800}}})
		case "/selforder/api/desktop-admin/order/mgr/listOrderInstStatisticsV2":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"currentUser": map[string]any{"17010101": map[string]any{"count": 2}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := ctyun.NewNativeClientWithOptions("user", "password", "device", nil, ctyun.NativeOptions{
		APIOrigin: server.URL, MarketplaceOrigin: server.URL, HTTPClient: server.Client(),
		Now: time.Now, Random: strings.NewReader(strings.Repeat("a", 2048)),
	})
	client.UseProfile(ctyun.NativeProfile{UserID: 1, UserEID: "eid", TenantID: 2, SecretKey: "secret", CommonLoginReqHeader: "common"})
	result := observeRedeemResult(context.Background(), client, ctyun.Reward{ProductID: 17010101, ProductType: "cpcai"}, 1000, 200, 1, true)
	if !strings.Contains(result, "1000") || !strings.Contains(result, "800") || !strings.Contains(result, "1") || !strings.Contains(result, "2") {
		t.Fatalf("verification result = %q", result)
	}
}

func TestScheduledTaskKey(t *testing.T) {
	if got := scheduledTaskKey("pc"); got != "usage" {
		t.Fatalf("scheduledTaskKey(pc) = %q", got)
	}
	if got := scheduledTaskKey("chat"); got != "chat" {
		t.Fatalf("scheduledTaskKey(chat) = %q", got)
	}
}

func TestStatusUpdatedToday(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 9, 9, 0, 5, 0, 0, location)
	if !statusUpdatedToday("2026-09-08T16:01:00Z", now) {
		t.Fatal("UTC timestamp on the current local day should be current")
	}
	if statusUpdatedToday("2026-09-08T15:59:59Z", now) {
		t.Fatal("timestamp from the previous local day should be stale")
	}
}

func TestValidateRedeemReward(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local)
	if err := validateRedeemReward(ctyun.Reward{
		ProductID: 99, ProductType: "gift", CostPoints: 300, Status: 2,
		EffectiveAt: "2026-09-01", ExpiresAt: "2026-09-09",
	}, now); err != nil {
		t.Fatalf("reward should remain valid through its expiration date: %v", err)
	}
	for _, reward := range []ctyun.Reward{
		{ProductID: 99, ProductType: "gift", CostPoints: 300, Status: 3},
		{ProductID: 99, ProductType: "gift", CostPoints: 300, Status: 2, EffectiveAt: "2026-09-10"},
		{ProductID: 99, ProductType: "gift", CostPoints: 300, Status: 2, ExpiresAt: "2026-09-08"},
	} {
		if err := validateRedeemReward(reward, now); err == nil {
			t.Fatalf("reward should be rejected: %#v", reward)
		}
	}
}

func TestMonthlyRedeemDaysSupportsLastDay(t *testing.T) {
	got, err := monthlyRedeemDays("1, 15, -1,15")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 15 || got[2] != -1 {
		t.Fatalf("monthlyRedeemDays() = %v", got)
	}
	if _, err = monthlyRedeemDays("0,32"); err == nil {
		t.Fatal("invalid monthly days should fail")
	}
}

func TestResolveRedeemRequiresPendingAndRecordsSuccessDate(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := New(store, nil, dir, "")
	defer manager.Close()
	accountID, err := store.SaveAccount(storage.Account{Name: "test", Username: "user", DeviceCode: "device"}, "password", nil, func(value string, _ []byte) (string, error) {
		return value, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB.Exec(`INSERT INTO redeem_states(account_id,last_attempt_date,last_attempt_status,last_redeem_times,last_points_spent,message,updated_at) VALUES(?,'2026-09-09','pending',2,600,'pending',?)`, accountID, storage.Now()); err != nil {
		t.Fatal(err)
	}
	if err = manager.ResolveRedeem(accountID, true); err != nil {
		t.Fatal(err)
	}
	var status, successDate string
	var times, points int
	if err = store.DB.QueryRow(`SELECT last_attempt_status,last_success_date,last_redeem_times,last_points_spent FROM redeem_states WHERE account_id=?`, accountID).Scan(&status, &successDate, &times, &points); err != nil {
		t.Fatal(err)
	}
	if status != "success" || successDate != "2026-09-09" || times != 2 || points != 600 {
		t.Fatalf("resolved state = %q %q %d %d", status, successDate, times, points)
	}
	if err = manager.ResolveRedeem(accountID, true); err == nil {
		t.Fatal("resolving a non-pending order should fail")
	}
}

func TestStartTaskRejectsReservedTaskKey(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := New(store, nil, dir, "")
	defer manager.Close()
	manager.starting["1:redeem"] = struct{}{}
	if _, err = manager.StartTask(1, "redeem", "test"); err == nil {
		t.Fatal("a task being created must block a duplicate start")
	}
}

func TestStartTaskRejectsDisabledAccount(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accountID, err := store.SaveAccount(storage.Account{Name: "disabled", Username: "user", DeviceCode: "device"}, "password", nil, func(value string, _ []byte) (string, error) { return value, nil })
	if err != nil {
		t.Fatal(err)
	}
	manager := New(store, nil, dir, "")
	defer manager.Close()
	if _, err = manager.StartTask(accountID, "login", "manual"); err == nil || !strings.Contains(err.Error(), "停用") {
		t.Fatalf("disabled account task start error = %v", err)
	}
}

func TestScheduledTaskRejectsDisabledTask(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accountID, err := store.SaveAccount(storage.Account{Name: "enabled", Username: "user", DeviceCode: "device", Enabled: true}, "password", nil, func(value string, _ []byte) (string, error) { return value, nil })
	if err != nil {
		t.Fatal(err)
	}
	manager := New(store, nil, dir, "")
	defer manager.Close()
	if _, err = manager.StartTask(accountID, "chat", "schedule"); err == nil || !strings.Contains(err.Error(), "任务已停用") {
		t.Fatalf("disabled scheduled task start error = %v", err)
	}
}

func TestAutomationRandomDelayStaysWithinConfiguredRange(t *testing.T) {
	account := storage.Account{LoginDelayMinutes: 3, PCDelayMinutes: 7, ChatDelayMinutes: 11}
	if scheduledTaskDelay(account, "login") != 3 || scheduledTaskDelay(account, "pc") != 7 || scheduledTaskDelay(account, "chat") != 11 {
		t.Fatal("task-specific random delays were not selected independently")
	}
	if got := automationRandomDelay(0); got != 0 {
		t.Fatalf("disabled random delay = %s", got)
	}
	for range 100 {
		got := automationRandomDelay(5)
		if got < 0 || got > 5*time.Minute {
			t.Fatalf("random delay outside configured range: %s", got)
		}
	}
	if got := readableDelay(2*time.Minute + 7*time.Second); got != "2 分 7 秒" {
		t.Fatalf("readable delay = %q", got)
	}
}

func TestStopAccountTaskOnlyCancelsMatchingTask(t *testing.T) {
	manager := &Manager{active: map[int64]running{}}
	loginCtx, cancelLogin := context.WithCancel(context.Background())
	chatCtx, cancelChat := context.WithCancel(context.Background())
	defer cancelLogin()
	defer cancelChat()
	manager.active[1] = running{accountID: 9, typ: "9:login", cancel: cancelLogin}
	manager.active[2] = running{accountID: 9, typ: "9:chat", cancel: cancelChat}
	manager.StopAccountTask(9, "login")
	if !errors.Is(loginCtx.Err(), context.Canceled) {
		t.Fatal("matching account task was not cancelled")
	}
	if chatCtx.Err() != nil {
		t.Fatal("unrelated account task was cancelled")
	}
}

func TestScheduledKeepaliveWaitsForUsageTaskBeforeStopping(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := New(store, nil, dir, "")
	defer manager.Close()
	ctx, cancel := context.WithCancel(context.Background())
	account := storage.Account{ID: 7, Name: "test", Enabled: true, KeepaliveEnabled: true, KeepaliveMode: storage.KeepaliveScheduled, KeepaliveStart: "08:00", KeepaliveEnd: "09:00", KeepaliveWeekdays: "1"}
	manager.clients[account.ID] = &clientState{ctx: ctx, cancel: cancel, status: "保活运行中"}
	manager.active[1] = running{typ: "7:pc", accountID: account.ID, cancel: func() {}}

	outside := time.Date(2026, 9, 7, 10, 0, 0, 0, time.Local)
	manager.reconcileAccountKeepalive(account, outside)
	if ctx.Err() != nil {
		t.Fatal("keepalive was stopped while the usage task was active")
	}
	delete(manager.active, 1)
	manager.reconcileAccountKeepalive(account, outside)
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("keepalive was not stopped after the usage task finished")
	}
}
