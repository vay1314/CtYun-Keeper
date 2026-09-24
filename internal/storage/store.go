package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ DB *sql.DB }
type Account struct {
	ID                                                                                                            int64
	Name, Username, PasswordEncrypted, DeviceCode, KeepaliveMode, KeepaliveStart, KeepaliveEnd, KeepaliveWeekdays string
	LoginCron, ChatCron, PCCron, DeviceStatus                                                                     string
	Enabled, KeepaliveEnabled, LoginEnabled, ChatEnabled, PCEnabled                                               bool
	LoginDelayMinutes, ChatDelayMinutes, PCDelayMinutes                                                           int
}

const (
	KeepaliveOff       = "off"
	KeepaliveAlways    = "always"
	KeepaliveScheduled = "scheduled"
)

func (a Account) EffectiveKeepaliveMode() string {
	if !a.KeepaliveEnabled {
		return KeepaliveOff
	}
	switch a.KeepaliveMode {
	case KeepaliveScheduled:
		return KeepaliveScheduled
	case KeepaliveOff:
		return KeepaliveOff
	default:
		return KeepaliveAlways
	}
}

func (a Account) KeepaliveActiveAt(now time.Time) bool {
	if !a.Enabled || a.EffectiveKeepaliveMode() == KeepaliveOff {
		return false
	}
	if a.EffectiveKeepaliveMode() == KeepaliveAlways {
		return true
	}
	start, startOK := clockMinute(a.KeepaliveStart)
	end, endOK := clockMinute(a.KeepaliveEnd)
	if !startOK || !endOK || start == end {
		return false
	}
	minute := now.Hour()*60 + now.Minute()
	weekday := isoWeekday(now.Weekday())
	if start < end {
		return keepaliveWeekdaySelected(a.KeepaliveWeekdays, weekday) && minute >= start && minute < end
	}
	if minute >= start {
		return keepaliveWeekdaySelected(a.KeepaliveWeekdays, weekday)
	}
	previous := weekday - 1
	if previous == 0 {
		previous = 7
	}
	return minute < end && keepaliveWeekdaySelected(a.KeepaliveWeekdays, previous)
}

func clockMinute(value string) (int, bool) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	return parsed.Hour()*60 + parsed.Minute(), err == nil
}

func isoWeekday(day time.Weekday) int {
	if day == time.Sunday {
		return 7
	}
	return int(day)
}

func keepaliveWeekdaySelected(value string, day int) bool {
	if strings.TrimSpace(value) == "" {
		value = "1,2,3,4,5,6,7"
	}
	needle := strconv.Itoa(day)
	for _, item := range strings.Split(value, ",") {
		if strings.TrimSpace(item) == needle {
			return true
		}
	}
	return false
}

type Run struct {
	ID, AccountID                                                                   int64
	AccountName, TaskType, Trigger, Status, StartedAt, FinishedAt, LogPath, Message string
}
type PlatformStatus struct {
	AccountID        int64
	TotalPoints      *int
	Tasks            map[string]TaskStatus
	UpdatedAt, Error string
}
type TaskStatus struct {
	State      string `json:"state"`
	StateLabel string `json:"state_label"`
	Current    int    `json:"current"`
	Total      int    `json:"total"`
}
type RedeemConfig struct {
	AccountID                                      int64
	Enabled                                        bool
	ProductID, ProductName, ProductType, DesktopID string
	CostPoints, MaxTimes                           int
	ScheduleType                                   string
	IntervalDays                                   int
	RandomDelayMinutes                             int
	MonthlyDays                                    string
	UpdatedAt                                      string
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db}
	if err = s.Init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.DB.Close() }
func Now() string             { return time.Now().Format(time.RFC3339) }
func (s *Store) Init() error {
	_, err := s.DB.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=30000;
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS accounts(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,username TEXT NOT NULL UNIQUE,password_encrypted TEXT NOT NULL,device_code TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,keepalive_enabled INTEGER NOT NULL DEFAULT 1,keepalive_mode TEXT NOT NULL DEFAULT 'always',keepalive_start TEXT NOT NULL DEFAULT '08:00',keepalive_end TEXT NOT NULL DEFAULT '23:00',keepalive_weekdays TEXT NOT NULL DEFAULT '1,2,3,4,5,6,7',login_enabled INTEGER NOT NULL DEFAULT 1,login_cron TEXT NOT NULL DEFAULT '0 3 * * *',login_delay_minutes INTEGER NOT NULL DEFAULT 0,chat_enabled INTEGER NOT NULL DEFAULT 1,chat_cron TEXT NOT NULL DEFAULT '10 3 * * *',chat_delay_minutes INTEGER NOT NULL DEFAULT 0,pc_enabled INTEGER NOT NULL DEFAULT 1,pc_cron TEXT NOT NULL DEFAULT '5 3 * * *',pc_delay_minutes INTEGER NOT NULL DEFAULT 0,device_status TEXT NOT NULL DEFAULT 'unknown',created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS task_runs(id INTEGER PRIMARY KEY AUTOINCREMENT,account_id INTEGER REFERENCES accounts(id) ON DELETE SET NULL,task_type TEXT NOT NULL,trigger_source TEXT NOT NULL,status TEXT NOT NULL,started_at TEXT NOT NULL,finished_at TEXT,exit_code INTEGER,log_path TEXT NOT NULL,message TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS scheduler_claims(account_id INTEGER NOT NULL,task_type TEXT NOT NULL,minute_key TEXT NOT NULL,PRIMARY KEY(account_id,task_type,minute_key));
CREATE TABLE IF NOT EXISTS account_platform_status(account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,total_points INTEGER,tasks_json TEXT NOT NULL DEFAULT '{}',updated_at TEXT NOT NULL,error TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS account_auth_cache(account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,login_info_encrypted TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS account_native_auth_cache(account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,login_info_encrypted TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS redeem_configs(account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,enabled INTEGER NOT NULL DEFAULT 0,product_id TEXT NOT NULL DEFAULT '',product_name TEXT NOT NULL DEFAULT '',product_type TEXT NOT NULL DEFAULT '',desktop_id TEXT NOT NULL DEFAULT '',cost_points INTEGER NOT NULL DEFAULT 0,max_times INTEGER NOT NULL DEFAULT 1,schedule_type TEXT NOT NULL DEFAULT 'daily',interval_days INTEGER NOT NULL DEFAULT 1,random_delay_minutes INTEGER NOT NULL DEFAULT 0,monthly_days TEXT NOT NULL DEFAULT '',updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS redeem_states(account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,last_attempt_date TEXT NOT NULL DEFAULT '',last_attempt_status TEXT NOT NULL DEFAULT '',last_success_date TEXT NOT NULL DEFAULT '',last_redeem_times INTEGER NOT NULL DEFAULT 0,last_points_spent INTEGER NOT NULL DEFAULT 0,message TEXT NOT NULL DEFAULT '',updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS update_history(id INTEGER PRIMARY KEY AUTOINCREMENT,from_version TEXT NOT NULL,to_version TEXT NOT NULL,platform TEXT NOT NULL,status TEXT NOT NULL,message TEXT NOT NULL DEFAULT '',started_at TEXT NOT NULL,finished_at TEXT);
CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_task_runs_started_at ON task_runs(started_at DESC);`)
	if err == nil {
		tx, txErr := s.DB.Begin()
		if txErr != nil {
			return txErr
		}
		defer tx.Rollback()
		// Existing databases predate the independent keepalive and login-task
		// switches. Preserve their previous behaviour while adding the new fields.
		for _, migration := range []struct{ name, definition string }{
			{"keepalive_enabled", "INTEGER NOT NULL DEFAULT 1"},
			{"keepalive_mode", "TEXT NOT NULL DEFAULT 'always'"},
			{"keepalive_start", "TEXT NOT NULL DEFAULT '08:00'"},
			{"keepalive_end", "TEXT NOT NULL DEFAULT '23:00'"},
			{"keepalive_weekdays", "TEXT NOT NULL DEFAULT '1,2,3,4,5,6,7'"},
			{"login_enabled", "INTEGER NOT NULL DEFAULT 1"},
			{"login_cron", "TEXT NOT NULL DEFAULT '0 3 * * *'"},
		} {
			var count int
			if e := tx.QueryRow("SELECT COUNT(*) FROM pragma_table_info('accounts') WHERE name=?", migration.name).Scan(&count); e != nil {
				return e
			}
			if count == 0 {
				if _, e := tx.Exec("ALTER TABLE accounts ADD COLUMN " + migration.name + " " + migration.definition); e != nil {
					return e
				}
			}
		}
		var legacyTaskDelayColumn int
		if e := tx.QueryRow("SELECT COUNT(*) FROM pragma_table_info('accounts') WHERE name='task_delay_minutes'").Scan(&legacyTaskDelayColumn); e != nil {
			return e
		}
		addedTaskDelayColumns := false
		for _, migration := range []struct{ name, definition string }{
			{"login_delay_minutes", "INTEGER NOT NULL DEFAULT 0"},
			{"chat_delay_minutes", "INTEGER NOT NULL DEFAULT 0"},
			{"pc_delay_minutes", "INTEGER NOT NULL DEFAULT 0"},
		} {
			var count int
			if e := tx.QueryRow("SELECT COUNT(*) FROM pragma_table_info('accounts') WHERE name=?", migration.name).Scan(&count); e != nil {
				return e
			}
			if count == 0 {
				if _, e := tx.Exec("ALTER TABLE accounts ADD COLUMN " + migration.name + " " + migration.definition); e != nil {
					return e
				}
				addedTaskDelayColumns = true
			}
		}
		if legacyTaskDelayColumn > 0 && addedTaskDelayColumns {
			if _, e := tx.Exec("UPDATE accounts SET login_delay_minutes=task_delay_minutes,chat_delay_minutes=task_delay_minutes,pc_delay_minutes=task_delay_minutes"); e != nil {
				return e
			}
		}
		var redeemDelayColumn int
		if e := tx.QueryRow("SELECT COUNT(*) FROM pragma_table_info('redeem_configs') WHERE name='random_delay_minutes'").Scan(&redeemDelayColumn); e != nil {
			return e
		}
		if redeemDelayColumn == 0 {
			if _, e := tx.Exec("ALTER TABLE redeem_configs ADD COLUMN random_delay_minutes INTEGER NOT NULL DEFAULT 0"); e != nil {
				return e
			}
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(1,?)", Now()); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE task_runs SET status='interrupted',finished_at=?,message='服务重启，任务状态已重置' WHERE status IN ('queued','running')", Now()); err != nil {
			return err
		}
		err = tx.Commit()
	}
	return err
}
func (s *Store) Setting(k string) (string, error) {
	var v string
	err := s.DB.QueryRow("SELECT value FROM settings WHERE key=?", k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
func (s *Store) SetSetting(k, v string) error {
	_, e := s.DB.Exec("INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at", k, v, Now())
	return e
}

func (s *Store) HasSetting(key string) bool {
	var found int
	return s.DB.QueryRow("SELECT 1 FROM settings WHERE key=?", key).Scan(&found) == nil
}

// RecordUpdateResult recreates history removed by restoring an older database.
func (s *Store) RecordUpdateResult(id int64, from, to, platform, status, message, started string) error {
	_, err := s.DB.Exec(`INSERT INTO update_history(id,from_version,to_version,platform,status,message,started_at,finished_at)
	VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,message=excluded.message,finished_at=excluded.finished_at`,
		id, from, to, platform, status, message, started, Now())
	return err
}

func (s *Store) InterruptUnfinishedUpdates() error {
	_, err := s.DB.Exec("UPDATE update_history SET status='failed',message='更新过程被中断，未确认安装成功，请重新检测更新',finished_at=? WHERE finished_at IS NULL", Now())
	return err
}
func scanAccount(r interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var en, keepalive, login, ch, pc int
	e := r.Scan(&a.ID, &a.Name, &a.Username, &a.PasswordEncrypted, &a.DeviceCode, &en, &keepalive, &a.KeepaliveMode, &a.KeepaliveStart, &a.KeepaliveEnd, &a.KeepaliveWeekdays, &login, &a.LoginCron, &a.LoginDelayMinutes, &ch, &a.ChatCron, &a.ChatDelayMinutes, &pc, &a.PCCron, &a.PCDelayMinutes, &a.DeviceStatus)
	a.Enabled = en != 0
	a.KeepaliveEnabled = keepalive != 0
	a.LoginEnabled = login != 0
	a.ChatEnabled = ch != 0
	a.PCEnabled = pc != 0
	return a, e
}

const accountCols = "id,name,username,password_encrypted,device_code,enabled,keepalive_enabled,keepalive_mode,keepalive_start,keepalive_end,keepalive_weekdays,login_enabled,login_cron,login_delay_minutes,chat_enabled,chat_cron,chat_delay_minutes,pc_enabled,pc_cron,pc_delay_minutes,device_status"

func (s *Store) Accounts() ([]Account, error) {
	rows, e := s.DB.Query("SELECT " + accountCols + " FROM accounts ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, e := scanAccount(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) Account(id int64) (Account, error) {
	return scanAccount(s.DB.QueryRow("SELECT "+accountCols+" FROM accounts WHERE id=?", id))
}
func (s *Store) SaveAccount(a Account, password string, key []byte, encrypt func(string, []byte) (string, error)) (int64, error) {
	now := Now()
	if a.Name == "" || a.Username == "" || a.DeviceCode == "" {
		return 0, errors.New("名称、账号和设备码不能为空")
	}
	if a.ID == 0 {
		if password == "" {
			return 0, errors.New("密码不能为空")
		}
		enc, e := encrypt(password, key)
		if e != nil {
			return 0, e
		}
		r, e := s.DB.Exec(`INSERT INTO accounts(name,username,password_encrypted,device_code,enabled,keepalive_enabled,keepalive_mode,keepalive_start,keepalive_end,keepalive_weekdays,login_enabled,login_cron,login_delay_minutes,chat_enabled,chat_cron,chat_delay_minutes,pc_enabled,pc_cron,pc_delay_minutes,device_status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'unknown',?,?)`, a.Name, a.Username, enc, a.DeviceCode, a.Enabled, a.KeepaliveEnabled, normalizedKeepaliveMode(a), normalizedKeepaliveStart(a), normalizedKeepaliveEnd(a), normalizedKeepaliveWeekdays(a), a.LoginEnabled, a.LoginCron, normalizedDelayMinutes(a.LoginDelayMinutes), a.ChatEnabled, a.ChatCron, normalizedDelayMinutes(a.ChatDelayMinutes), a.PCEnabled, a.PCCron, normalizedDelayMinutes(a.PCDelayMinutes), now, now)
		if e != nil {
			return 0, e
		}
		return r.LastInsertId()
	}
	old, e := s.Account(a.ID)
	if e != nil {
		return 0, e
	}
	enc := old.PasswordEncrypted
	if password != "" {
		enc, e = encrypt(password, key)
		if e != nil {
			return 0, e
		}
	}
	_, e = s.DB.Exec(`UPDATE accounts SET name=?,username=?,password_encrypted=?,device_code=?,enabled=?,keepalive_enabled=?,keepalive_mode=?,keepalive_start=?,keepalive_end=?,keepalive_weekdays=?,login_enabled=?,login_cron=?,login_delay_minutes=?,chat_enabled=?,chat_cron=?,chat_delay_minutes=?,pc_enabled=?,pc_cron=?,pc_delay_minutes=?,device_status=CASE WHEN username<>? OR device_code<>? THEN 'unknown' ELSE device_status END,updated_at=? WHERE id=?`, a.Name, a.Username, enc, a.DeviceCode, a.Enabled, a.KeepaliveEnabled, normalizedKeepaliveMode(a), normalizedKeepaliveStart(a), normalizedKeepaliveEnd(a), normalizedKeepaliveWeekdays(a), a.LoginEnabled, a.LoginCron, normalizedDelayMinutes(a.LoginDelayMinutes), a.ChatEnabled, a.ChatCron, normalizedDelayMinutes(a.ChatDelayMinutes), a.PCEnabled, a.PCCron, normalizedDelayMinutes(a.PCDelayMinutes), a.Username, a.DeviceCode, now, a.ID)
	return a.ID, e
}

func normalizedDelayMinutes(value int) int {
	if value < 0 {
		return 0
	}
	if value > 120 {
		return 120
	}
	return value
}

func (s *Store) SetAccountEnabled(id int64, enabled bool) error {
	result, err := s.DB.Exec("UPDATE accounts SET enabled=?,updated_at=? WHERE id=?", enabled, Now(), id)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) SetTaskEnabled(id int64, taskType string, enabled bool) error {
	column := map[string]string{"login": "login_enabled", "pc": "pc_enabled", "chat": "chat_enabled"}[taskType]
	if column == "" {
		return errors.New("未知任务类型")
	}
	result, err := s.DB.Exec("UPDATE accounts SET "+column+"=?,updated_at=? WHERE id=?", enabled, Now(), id)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func normalizedKeepaliveMode(a Account) string {
	if !a.KeepaliveEnabled || a.KeepaliveMode == KeepaliveOff {
		return KeepaliveOff
	}
	if a.KeepaliveMode == KeepaliveScheduled {
		return KeepaliveScheduled
	}
	return KeepaliveAlways
}

func normalizedKeepaliveStart(a Account) string {
	if _, ok := clockMinute(a.KeepaliveStart); ok {
		return a.KeepaliveStart
	}
	return "08:00"
}

func normalizedKeepaliveEnd(a Account) string {
	if _, ok := clockMinute(a.KeepaliveEnd); ok {
		return a.KeepaliveEnd
	}
	return "23:00"
}

func normalizedKeepaliveWeekdays(a Account) string {
	if strings.TrimSpace(a.KeepaliveWeekdays) == "" {
		return "1,2,3,4,5,6,7"
	}
	return a.KeepaliveWeekdays
}
func (s *Store) DeleteAccount(id int64) error {
	_, e := s.DB.Exec("DELETE FROM accounts WHERE id=?", id)
	return e
}
func (s *Store) SetDeviceStatus(id int64, status string) error {
	_, e := s.DB.Exec("UPDATE accounts SET device_status=?,updated_at=? WHERE id=?", status, Now(), id)
	return e
}
func (s *Store) AuthCache(id int64) (string, error) {
	var v string
	e := s.DB.QueryRow("SELECT login_info_encrypted FROM account_auth_cache WHERE account_id=?", id).Scan(&v)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	return v, e
}
func (s *Store) SaveAuthCache(id int64, value string) error {
	_, e := s.DB.Exec(`INSERT INTO account_auth_cache(account_id,login_info_encrypted,updated_at) VALUES(?,?,?) ON CONFLICT(account_id) DO UPDATE SET login_info_encrypted=excluded.login_info_encrypted,updated_at=excluded.updated_at`, id, value, Now())
	return e
}
func (s *Store) ClearAuthCache(id int64) {
	_, _ = s.DB.Exec("DELETE FROM account_auth_cache WHERE account_id=?", id)
}
func (s *Store) NativeAuthCache(id int64) (string, error) {
	var v string
	e := s.DB.QueryRow("SELECT login_info_encrypted FROM account_native_auth_cache WHERE account_id=?", id).Scan(&v)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	return v, e
}
func (s *Store) SaveNativeAuthCache(id int64, value string) error {
	_, e := s.DB.Exec(`INSERT INTO account_native_auth_cache(account_id,login_info_encrypted,updated_at) VALUES(?,?,?) ON CONFLICT(account_id) DO UPDATE SET login_info_encrypted=excluded.login_info_encrypted,updated_at=excluded.updated_at`, id, value, Now())
	return e
}
func (s *Store) ClearNativeAuthCache(id int64) {
	_, _ = s.DB.Exec("DELETE FROM account_native_auth_cache WHERE account_id=?", id)
}
func (s *Store) AddRun(accountID int64, typ, trigger, logPath string) (int64, error) {
	r, e := s.DB.Exec("INSERT INTO task_runs(account_id,task_type,trigger_source,status,started_at,log_path) VALUES(?,?,?,'queued',?,?)", accountID, typ, trigger, Now(), logPath)
	if e != nil {
		return 0, e
	}
	return r.LastInsertId()
}
func (s *Store) UpdateRun(id int64, status, message string) error {
	finish := any(nil)
	if status != "queued" && status != "running" {
		finish = Now()
	}
	_, e := s.DB.Exec("UPDATE task_runs SET status=?,message=?,finished_at=COALESCE(?,finished_at) WHERE id=?", status, message, finish, id)
	return e
}
func (s *Store) Runs(limit int) ([]Run, error) {
	rows, e := s.DB.Query(`SELECT r.id,COALESCE(r.account_id,0),COALESCE(a.name,''),r.task_type,r.trigger_source,r.status,r.started_at,COALESCE(r.finished_at,''),r.log_path,r.message FROM task_runs r LEFT JOIN accounts a ON a.id=r.account_id ORDER BY r.id DESC LIMIT ?`, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var v Run
		if e = rows.Scan(&v.ID, &v.AccountID, &v.AccountName, &v.TaskType, &v.Trigger, &v.Status, &v.StartedAt, &v.FinishedAt, &v.LogPath, &v.Message); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) Run(id int64) (Run, error) {
	var v Run
	e := s.DB.QueryRow(`SELECT r.id,COALESCE(r.account_id,0),COALESCE(a.name,''),r.task_type,r.trigger_source,r.status,r.started_at,COALESCE(r.finished_at,''),r.log_path,r.message FROM task_runs r LEFT JOIN accounts a ON a.id=r.account_id WHERE r.id=?`, id).Scan(&v.ID, &v.AccountID, &v.AccountName, &v.TaskType, &v.Trigger, &v.Status, &v.StartedAt, &v.FinishedAt, &v.LogPath, &v.Message)
	return v, e
}

func (s *Store) RunLogs() ([]Run, error) {
	rows, e := s.DB.Query(`SELECT id,status,log_path FROM task_runs ORDER BY id DESC`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var v Run
		if e = rows.Scan(&v.ID, &v.Status, &v.LogPath); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) PurgeCompletedRunsBefore(cutoff string) ([]string, error) {
	tx, e := s.DB.Begin()
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	rows, e := tx.Query(`SELECT log_path FROM task_runs WHERE status NOT IN ('queued','running') AND COALESCE(NULLIF(finished_at,''),started_at) < ?`, cutoff)
	if e != nil {
		return nil, e
	}
	var paths []string
	for rows.Next() {
		var path string
		if e = rows.Scan(&path); e != nil {
			rows.Close()
			return nil, e
		}
		paths = append(paths, path)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if _, e = tx.Exec(`DELETE FROM task_runs WHERE status NOT IN ('queued','running') AND COALESCE(NULLIF(finished_at,''),started_at) < ?`, cutoff); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return paths, nil
}
func (s *Store) SavePlatform(v PlatformStatus) error {
	raw, _ := json.Marshal(v.Tasks)
	points := any(nil)
	if v.TotalPoints != nil {
		points = *v.TotalPoints
	}
	_, e := s.DB.Exec(`INSERT INTO account_platform_status(account_id,total_points,tasks_json,updated_at,error) VALUES(?,?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET total_points=excluded.total_points,tasks_json=excluded.tasks_json,updated_at=excluded.updated_at,error=excluded.error`, v.AccountID, points, string(raw), Now(), v.Error)
	return e
}
func (s *Store) Platforms() (map[int64]PlatformStatus, error) {
	rows, e := s.DB.Query("SELECT account_id,total_points,tasks_json,updated_at,error FROM account_platform_status")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[int64]PlatformStatus{}
	for rows.Next() {
		var v PlatformStatus
		var p sql.NullInt64
		var raw string
		if e = rows.Scan(&v.AccountID, &p, &raw, &v.UpdatedAt, &v.Error); e != nil {
			return nil, e
		}
		if p.Valid {
			x := int(p.Int64)
			v.TotalPoints = &x
		}
		_ = json.Unmarshal([]byte(raw), &v.Tasks)
		out[v.AccountID] = v
	}
	return out, rows.Err()
}
func (s *Store) Claim(id int64, typ, minute string) bool {
	r, e := s.DB.Exec("INSERT OR IGNORE INTO scheduler_claims(account_id,task_type,minute_key) VALUES(?,?,?)", id, typ, minute)
	if e != nil {
		return false
	}
	n, _ := r.RowsAffected()
	return n == 1
}
func (s *Store) ReleaseClaim(id int64, typ, minute string) error {
	_, err := s.DB.Exec("DELETE FROM scheduler_claims WHERE account_id=? AND task_type=? AND minute_key=?", id, typ, minute)
	return err
}
func (s *Store) Redeem(id int64) (RedeemConfig, error) {
	var v RedeemConfig
	var en int
	e := s.DB.QueryRow(`SELECT account_id,enabled,product_id,product_name,product_type,desktop_id,cost_points,max_times,schedule_type,interval_days,random_delay_minutes,monthly_days,updated_at FROM redeem_configs WHERE account_id=?`, id).Scan(&v.AccountID, &en, &v.ProductID, &v.ProductName, &v.ProductType, &v.DesktopID, &v.CostPoints, &v.MaxTimes, &v.ScheduleType, &v.IntervalDays, &v.RandomDelayMinutes, &v.MonthlyDays, &v.UpdatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		v.AccountID = id
		v.ScheduleType = "daily"
		v.IntervalDays = 1
		v.MaxTimes = 1
		return v, nil
	}
	v.Enabled = en != 0
	return v, e
}
func (s *Store) SaveRedeem(v RedeemConfig) error {
	if v.MaxTimes < 1 {
		v.MaxTimes = 1
	}
	if v.IntervalDays < 1 {
		v.IntervalDays = 1
	}
	v.RandomDelayMinutes = normalizedDelayMinutes(v.RandomDelayMinutes)
	_, e := s.DB.Exec(`INSERT INTO redeem_configs(account_id,enabled,product_id,product_name,product_type,desktop_id,cost_points,max_times,schedule_type,interval_days,random_delay_minutes,monthly_days,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET enabled=excluded.enabled,product_id=excluded.product_id,product_name=excluded.product_name,product_type=excluded.product_type,desktop_id=excluded.desktop_id,cost_points=excluded.cost_points,max_times=excluded.max_times,schedule_type=excluded.schedule_type,interval_days=excluded.interval_days,random_delay_minutes=excluded.random_delay_minutes,monthly_days=excluded.monthly_days,updated_at=excluded.updated_at`, v.AccountID, v.Enabled, v.ProductID, v.ProductName, v.ProductType, v.DesktopID, v.CostPoints, v.MaxTimes, v.ScheduleType, v.IntervalDays, v.RandomDelayMinutes, v.MonthlyDays, Now())
	return e
}
func (s *Store) Debug() string { return fmt.Sprintf("%p", s.DB) }

type UpdateHistory struct {
	ID                                       int64
	FromVersion, ToVersion, Platform, Status string
	Message, StartedAt, FinishedAt           string
}

func (s *Store) AddUpdateHistory(from, to, platform, status string) (int64, error) {
	r, e := s.DB.Exec("INSERT INTO update_history(from_version,to_version,platform,status,started_at) VALUES(?,?,?,?,?)", from, to, platform, status, Now())
	if e != nil {
		return 0, e
	}
	return r.LastInsertId()
}

func (s *Store) UpdateUpdateHistory(id int64, status, message string) error {
	finish := any(nil)
	switch status {
	case "success", "failed", "rolled_back":
		finish = Now()
	}
	_, e := s.DB.Exec("UPDATE update_history SET status=?,message=?,finished_at=COALESCE(?,finished_at) WHERE id=?", status, message, finish, id)
	return e
}

func (s *Store) RecentUpdateHistory(limit int) ([]UpdateHistory, error) {
	rows, e := s.DB.Query("SELECT id,from_version,to_version,platform,status,message,started_at,COALESCE(finished_at,'') FROM update_history ORDER BY id DESC LIMIT ?", limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []UpdateHistory
	for rows.Next() {
		var v UpdateHistory
		if e = rows.Scan(&v.ID, &v.FromVersion, &v.ToVersion, &v.Platform, &v.Status, &v.Message, &v.StartedAt, &v.FinishedAt); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) HasPendingRedeem() (bool, error) {
	var count int
	e := s.DB.QueryRow("SELECT COUNT(*) FROM redeem_states WHERE last_attempt_status='pending'").Scan(&count)
	return count > 0, e
}

func (s *Store) Checkpoint() error {
	_, e := s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return e
}

func (s *Store) BackupDatabase(destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0750); err != nil {
		return err
	}
	if err := os.Remove(destPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, e := s.DB.Exec("VACUUM INTO ?", destPath)
	if e == nil {
		e = os.Chmod(destPath, 0600)
	}
	return e
}
