package storage

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLegacyAccountMigrationEnablesExistingKeepaliveAndLoginTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE accounts(
		id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,username TEXT NOT NULL UNIQUE,
		password_encrypted TEXT NOT NULL,device_code TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,
		chat_enabled INTEGER NOT NULL DEFAULT 1,chat_cron TEXT NOT NULL DEFAULT '0 3,20 * * *',
		pc_enabled INTEGER NOT NULL DEFAULT 1,pc_cron TEXT NOT NULL DEFAULT '0 4,6 * * *',task_delay_minutes INTEGER NOT NULL DEFAULT 0,
		device_status TEXT NOT NULL DEFAULT 'unknown',created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
		INSERT INTO accounts(name,username,password_encrypted,device_code,enabled,chat_enabled,chat_cron,pc_enabled,pc_cron,task_delay_minutes,device_status,created_at,updated_at)
		VALUES('legacy','user','password','device',1,1,'0 3,20 * * *',1,'0 4,6 * * *',23,'verified','now','now');
		CREATE TABLE redeem_configs(account_id INTEGER PRIMARY KEY,enabled INTEGER NOT NULL DEFAULT 0,product_id TEXT NOT NULL DEFAULT '',product_name TEXT NOT NULL DEFAULT '',product_type TEXT NOT NULL DEFAULT '',desktop_id TEXT NOT NULL DEFAULT '',cost_points INTEGER NOT NULL DEFAULT 0,max_times INTEGER NOT NULL DEFAULT 1,schedule_type TEXT NOT NULL DEFAULT 'daily',interval_days INTEGER NOT NULL DEFAULT 1,monthly_days TEXT NOT NULL DEFAULT '',updated_at TEXT NOT NULL);
		INSERT INTO redeem_configs(account_id,updated_at) VALUES(1,'now');`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.Account(1)
	if err != nil {
		t.Fatal(err)
	}
	if !a.KeepaliveEnabled || a.KeepaliveMode != KeepaliveAlways || a.KeepaliveStart != "08:00" || a.KeepaliveEnd != "23:00" || a.KeepaliveWeekdays != "1,2,3,4,5,6,7" || !a.LoginEnabled || a.LoginCron != "0 3 * * *" || a.LoginDelayMinutes != 23 || a.ChatDelayMinutes != 23 || a.PCDelayMinutes != 23 {
		t.Fatalf("legacy defaults not migrated: %#v", a)
	}
	redeem, err := s.Redeem(1)
	if err != nil || redeem.RandomDelayMinutes != 0 {
		t.Fatalf("legacy redeem delay not migrated: %#v, %v", redeem, err)
	}
}

func TestOpenAndMigrate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, table := range []string{"accounts", "task_runs", "account_platform_status", "account_auth_cache", "account_native_auth_cache", "redeem_configs", "redeem_states"} {
		var name string
		if err := s.DB.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name); err != nil {
			t.Fatalf("missing %s: %v", table, err)
		}
	}
}

func TestNativeAuthCacheRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := Account{Name: "test", Username: "user", DeviceCode: "device"}
	id, err := s.SaveAccount(a, "password", []byte("unused"), func(value string, _ []byte) (string, error) { return value, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveNativeAuthCache(id, "encrypted-native-profile"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAuthCache(id, "encrypted-web-profile"); err != nil {
		t.Fatal(err)
	}
	got, err := s.NativeAuthCache(id)
	if err != nil || got != "encrypted-native-profile" {
		t.Fatalf("NativeAuthCache() = %q, %v", got, err)
	}
	s.ClearNativeAuthCache(id)
	got, err = s.NativeAuthCache(id)
	if err != nil || got != "" {
		t.Fatalf("NativeAuthCache() after clear = %q, %v", got, err)
	}
	web, err := s.AuthCache(id)
	if err != nil || web != "encrypted-web-profile" {
		t.Fatalf("clearing native cache changed Web cache: %q, %v", web, err)
	}
}

func TestUpdateHistoryRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.AddUpdateHistory("2.0.0", "2.1.0", "linux-amd64", "downloading")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUpdateHistory(id, "success", ""); err != nil {
		t.Fatal(err)
	}
	history, err := s.RecentUpdateHistory(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Status != "success" || history[0].FinishedAt == "" {
		t.Fatalf("history = %#v", history)
	}
}

func TestHasPendingRedeem(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	pending, err := s.HasPendingRedeem()
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("expected no pending redeem in empty database")
	}
	_, err = s.DB.Exec("INSERT INTO accounts(name,username,password_encrypted,device_code,created_at,updated_at) VALUES('a','u','p','d','now','now')")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec("INSERT INTO redeem_states(account_id,last_attempt_status,updated_at) VALUES(1,'pending',?)", Now())
	if err != nil {
		t.Fatal(err)
	}
	pending, err = s.HasPendingRedeem()
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("expected pending redeem to be detected")
	}
}

func TestBackupDatabase(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("test", "value"); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backups", "ctyun-keeper.db")
	if err := s.BackupDatabase(backup); err != nil {
		t.Fatal(err)
	}
	s.Close()

	restored, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	value, err := restored.Setting("test")
	if err != nil {
		t.Fatal(err)
	}
	if value != "value" {
		t.Fatalf("restored setting = %q", value)
	}
}

func TestAccountAutomationSettingsRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := Account{
		Name: "scheduled", Username: "user", DeviceCode: "device", Enabled: true,
		KeepaliveEnabled: true, KeepaliveMode: KeepaliveScheduled, KeepaliveStart: "22:00", KeepaliveEnd: "06:00", KeepaliveWeekdays: "1,3,5", LoginEnabled: true, LoginCron: "0 2 * * *",
		PCEnabled: true, PCCron: "5 2 * * *", PCDelayMinutes: 37, ChatEnabled: true, ChatCron: "10 2 * * *", ChatDelayMinutes: 28, LoginDelayMinutes: 19,
	}
	id, err := s.SaveAccount(a, "password", []byte("unused"), func(value string, _ []byte) (string, error) { return value, nil })
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.KeepaliveEnabled || got.KeepaliveMode != KeepaliveScheduled || got.KeepaliveStart != "22:00" || got.KeepaliveEnd != "06:00" || got.KeepaliveWeekdays != "1,3,5" || !got.LoginEnabled || got.LoginCron != a.LoginCron || got.PCCron != a.PCCron || got.ChatCron != a.ChatCron || got.LoginDelayMinutes != 19 || got.PCDelayMinutes != 37 || got.ChatDelayMinutes != 28 {
		t.Fatalf("automation settings were not preserved: %#v", got)
	}
	if err = s.SetTaskEnabled(id, "chat", false); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAccountEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	got, err = s.Account(id)
	if err != nil || got.Enabled || got.ChatEnabled {
		t.Fatalf("inline enabled state was not preserved: %#v, %v", got, err)
	}
}

func TestRedeemRandomDelayRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.DB.Exec("INSERT INTO accounts(name,username,password_encrypted,device_code,created_at,updated_at) VALUES('a','u','p','d','now','now')"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveRedeem(RedeemConfig{AccountID: 1, MaxTimes: 1, IntervalDays: 1, ScheduleType: "daily", RandomDelayMinutes: 45}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Redeem(1)
	if err != nil || got.RandomDelayMinutes != 45 {
		t.Fatalf("redeem random delay = %d, %v", got.RandomDelayMinutes, err)
	}
}

func TestKeepaliveActiveAt(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	sameDay := Account{Enabled: true, KeepaliveEnabled: true, KeepaliveMode: KeepaliveScheduled, KeepaliveStart: "08:00", KeepaliveEnd: "23:00", KeepaliveWeekdays: "1"}
	for _, test := range []struct {
		name   string
		value  Account
		now    time.Time
		active bool
	}{
		{"same-day start", sameDay, time.Date(2026, 9, 7, 8, 0, 0, 0, location), true},
		{"same-day end excluded", sameDay, time.Date(2026, 9, 7, 23, 0, 0, 0, location), false},
		{"unselected weekday", sameDay, time.Date(2026, 9, 8, 12, 0, 0, 0, location), false},
		{"cross-midnight start day", Account{Enabled: true, KeepaliveEnabled: true, KeepaliveMode: KeepaliveScheduled, KeepaliveStart: "22:00", KeepaliveEnd: "06:00", KeepaliveWeekdays: "1"}, time.Date(2026, 9, 7, 23, 0, 0, 0, location), true},
		{"cross-midnight next day", Account{Enabled: true, KeepaliveEnabled: true, KeepaliveMode: KeepaliveScheduled, KeepaliveStart: "22:00", KeepaliveEnd: "06:00", KeepaliveWeekdays: "1"}, time.Date(2026, 9, 8, 5, 59, 0, 0, location), true},
		{"cross-midnight end excluded", Account{Enabled: true, KeepaliveEnabled: true, KeepaliveMode: KeepaliveScheduled, KeepaliveStart: "22:00", KeepaliveEnd: "06:00", KeepaliveWeekdays: "1"}, time.Date(2026, 9, 8, 6, 0, 0, 0, location), false},
		{"always", Account{Enabled: true, KeepaliveEnabled: true, KeepaliveMode: KeepaliveAlways}, time.Date(2026, 9, 8, 6, 0, 0, 0, location), true},
		{"off", Account{Enabled: true, KeepaliveEnabled: false, KeepaliveMode: KeepaliveAlways}, time.Date(2026, 9, 8, 6, 0, 0, 0, location), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.value.KeepaliveActiveAt(test.now); got != test.active {
				t.Fatalf("KeepaliveActiveAt() = %v, want %v", got, test.active)
			}
		})
	}
}

func TestPurgeCompletedRunsBeforeKeepsActiveRuns(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	insert := func(status, started, finished, path string) {
		t.Helper()
		if _, err := s.DB.Exec(`INSERT INTO task_runs(account_id,task_type,trigger_source,status,started_at,finished_at,log_path) VALUES(NULL,'login','test',?,?,?,?)`, status, started, finished, path); err != nil {
			t.Fatal(err)
		}
	}
	insert("success", "2026-01-01T00:00:00Z", "2026-01-01T00:01:00Z", "old.log")
	insert("success", "2026-09-01T00:00:00Z", "2026-09-01T00:01:00Z", "new.log")
	insert("running", "2026-01-01T00:00:00Z", "", "active.log")
	logs, err := s.RunLogs()
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 3 || logs[0].LogPath != "active.log" {
		t.Fatalf("run logs = %#v", logs)
	}

	paths, err := s.PurgeCompletedRunsBefore("2026-06-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"old.log"}) {
		t.Fatalf("purged paths = %v", paths)
	}
	runs, err := s.Runs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].LogPath != "active.log" || runs[1].LogPath != "new.log" {
		t.Fatalf("remaining runs = %#v", runs)
	}
}
