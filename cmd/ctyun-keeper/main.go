package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vay1314/CtYun-Keeper/internal/security"
	"github.com/vay1314/CtYun-Keeper/internal/service"
	"github.com/vay1314/CtYun-Keeper/internal/storage"
	"github.com/vay1314/CtYun-Keeper/internal/update"
	webapp "github.com/vay1314/CtYun-Keeper/internal/web"
)

var (
	version         = "dev"
	commit          = "unknown"
	buildTime       = "unknown"
	updatePublicKey = ""
)

func env(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}
func main() {
	os.Exit(run())
}

func run() int {
	if strings.TrimSpace(version) == "" {
		version = "dev"
	}
	dataDir := env("CTYUN_DATA_DIR", "./data")
	dataDir, e := filepath.Abs(dataDir)
	if e != nil {
		log.Printf("解析数据目录失败: %v", e)
		return 1
	}
	if e := os.MkdirAll(dataDir, 0750); e != nil {
		log.Printf("创建数据目录失败: %v", e)
		return 1
	}
	if started, err := update.StartPendingWindowsRecovery(dataDir); err != nil {
		log.Printf("启动未完成更新恢复失败: %v", err)
		return 1
	} else if started {
		log.Print("检测到未完成的 Windows 更新，已交给更新助手恢复")
		return 0
	}
	store, e := storage.Open(filepath.Join(dataDir, "ctyun-keeper.db"))
	if e != nil {
		log.Printf("打开数据库失败: %v", e)
		return 1
	}
	finalizeWindowsUpdateResult(dataDir)
	consumeUpdateResults(store, dataDir)
	if !update.UpdateLockActive(dataDir) {
		_ = store.InterruptUnfinishedUpdates()
		_ = os.Remove(filepath.Join(dataDir, "updates", "executor-progress.json"))
	}
	update.CleanupArtifacts(dataDir)
	credentialKey, e := security.LoadOrCreateKey(filepath.Join(dataDir, ".credential_key"), 32)
	if e != nil {
		_ = store.Close()
		log.Printf("加载凭据密钥失败: %v", e)
		return 1
	}
	sessionKey, e := security.LoadOrCreateKey(filepath.Join(dataDir, ".web_session_key"), 32)
	if e != nil {
		_ = store.Close()
		log.Printf("加载会话密钥失败: %v", e)
		return 1
	}
	manager := service.New(store, credentialKey, dataDir, env("OCR_ENDPOINT", "https://orc.1999111.xyz/ocr"))
	manager.Start()
	exitRequests := make(chan int, 1)
	requestShutdown := func(code int) {
		select {
		case exitRequests <- code:
		default:
		}
	}
	staticDir := env("CTYUN_STATIC_DIR", "./app/web/static")
	webServer := webapp.New(store, manager, sessionKey, credentialKey, version, dataDir, staticDir, env("CTYUN_UPDATE_REPO", "vay1314/CtYun-Keeper"), trustedUpdatePublicKey(), strings.EqualFold(env("WEB_SECURE_COOKIE", "false"), "true"), requestShutdown)
	handler := webServer.Handler()
	server := &http.Server{Addr: ":" + env("APP_PORT", "9845"), Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	resultTicker := time.NewTicker(2 * time.Second)
	defer resultTicker.Stop()
	log.Printf("CtYunKeeper v%s (%s, %s) listening on %s", version, commit, buildTime, server.Addr)
	exitCode := 0
	nextCleanup := time.Now().Add(24 * time.Hour)
running:
	for {
		select {
		case <-stop:
			break running
		case exitCode = <-exitRequests:
			break running
		case err := <-serverErrors:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Web 服务异常退出: %v", err)
				exitCode = 1
			}
			break running
		case <-resultTicker.C:
			if started, err := update.StartPendingWindowsRecovery(dataDir); err != nil {
				log.Printf("启动未完成更新恢复失败: %v", err)
			} else if started {
				log.Print("更新助手意外中断，已重新启动恢复助手")
				break running
			}
			finalizeWindowsUpdateResult(dataDir)
			consumeUpdateResults(store, dataDir)
			webServer.RefreshUpdateResult()
			if time.Now().After(nextCleanup) {
				previous := ""
				if history, err := store.RecentUpdateHistory(100); err == nil {
					for _, h := range history {
						if h.Status == "success" && h.ToVersion == version {
							previous = h.FromVersion
							break
						}
					}
				}
				update.CleanupRetainedVersions(dataDir, version, previous, update.CurrentPlatform().ImageVersion)
				update.CleanupArtifacts(dataDir)
				nextCleanup = time.Now().Add(24 * time.Hour)
			}
		}
	}
	signal.Stop(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = server.Shutdown(ctx)
	cancel()
	webServer.Close()
	manager.Close()
	_ = store.Checkpoint()
	if err := store.Close(); err != nil {
		log.Printf("关闭数据库失败: %v", err)
		if exitCode == 0 {
			exitCode = 1
		}
	}
	return exitCode
}

func trustedUpdatePublicKey() string {
	if value := strings.TrimSpace(updatePublicKey); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("CTYUN_UPDATE_PUBLIC_KEY"))
}

func finalizeWindowsUpdateResult(dataDir string) {
	if update.UpdateLockActive(dataDir) {
		return
	}
	if err := update.FinalizeCompletedWindowsTransaction(dataDir); err != nil {
		log.Printf("提交 Windows 更新结果失败: %v", err)
	}
}

func consumeUpdateResults(store *storage.Store, dataDir string) {
	err := update.ConsumeInstallResults(filepath.Join(dataDir, "updates", "results"), func(result update.InstallResult) error {
		return store.RecordUpdateResult(result.HistoryID, result.FromVersion, result.Version, result.Platform, result.Status, result.Message, result.CreatedAt)
	})
	if err != nil {
		log.Printf("读取更新结果失败: %v", err)
	}
}
