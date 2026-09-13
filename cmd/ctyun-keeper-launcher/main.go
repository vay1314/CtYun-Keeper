//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/vay1314/CtYun-Keeper/internal/update"
)

var launcherVersion = "dev"
var updatePublicKey = ""
var healthTimeout = 60 * time.Second
var healthInterval = 2 * time.Second

type versionInfo struct {
	Version                string `json:"version"`
	MinimumLauncherVersion string `json:"minimumLauncherVersion"`
}

type pendingUpdate struct {
	Recovery        bool
	NoFallback      bool
	TargetDatabase  string
	Request         update.InstallRequest
	Previous        string
	Target          string
	SuccessStatus   string
	SuccessMessage  string
	CompletedResult *update.InstallResult `json:"completedResult,omitempty"`
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func main() {
	dataDir := env("CTYUN_DATA_DIR", "/app/data")
	builtinDir := env("BUILTIN_DIR", "/app/builtin")
	runtimeDir := filepath.Join(dataDir, "runtime")
	currentLink := filepath.Join(runtimeDir, "current")
	versionsDir := filepath.Join(runtimeDir, "versions")
	if err := os.MkdirAll(versionsDir, 0750); err != nil {
		log.Fatal(err)
	}
	launcherLock, err := acquireLauncherLock(dataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer releaseLauncherLock(launcherLock)
	pendingPath := filepath.Join(dataDir, "updates", "pending.json")
	var pending *pendingUpdate
	var startupManager *update.Manager
	if raw, err := os.ReadFile(pendingPath); err == nil {
		pending = &pendingUpdate{}
		if err := json.Unmarshal(raw, pending); err != nil {
			log.Fatalf("读取未完成更新失败: %v", err)
		}
		if err := validatePending(pending, dataDir, builtinDir, versionsDir); err != nil {
			log.Fatalf("未完成更新记录无效: %v", err)
		}
	}
	// PIDs are namespace-local in Docker. Keep recent locks so another
	// container sharing this volume cannot have its update lock stolen.
	if pending == nil {
		if err := update.ClearStaleUpdateLock(dataDir, time.Hour); err != nil {
			log.Fatal(err)
		}
		startupManager = update.NewManager(dataDir)
		if err := startupManager.Begin(); err != nil {
			log.Fatal(err)
		}
	} else if err := update.TakeOverUpdateLock(dataDir, pending.Request.Token); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Fatal(err)
		}
		manager := update.NewManager(dataDir)
		if beginErr := manager.Begin(); beginErr != nil {
			log.Fatal(beginErr)
		}
		if handoffErr := manager.HandOff(pending.Request.Token); handoffErr != nil {
			log.Fatal(handoffErr)
		}
	}
	startupFallback := ""
	err = nil
	if pending == nil {
		var target string
		var incompatible bool
		target, startupFallback, incompatible, err = selectStartupVersion(currentLink, builtinDir)
		if err == nil && startupFallback != "" {
			previousInfo, loadErr := loadVersion(startupFallback)
			if loadErr != nil {
				err = loadErr
			} else {
				builtinInfo, loadErr := loadVersion(target)
				if loadErr != nil {
					err = loadErr
				} else {
					source := filepath.Join(dataDir, "ctyun-keeper.db")
					id := time.Now().UnixMilli()
					recoveryDB := ""
					if _, statErr := os.Stat(source); statErr == nil {
						recoveryDB = filepath.Join(dataDir, "updates", "backups", previousInfo.Version, "database", "ctyun-keeper.db")
						if incompatible {
							recoveryDB = filepath.Join(dataDir, "updates", "recovery", fmt.Sprintf("image-%d.db", id))
						}
						if backupErr := update.BackupStoppedDatabase(source, recoveryDB); backupErr != nil {
							err = fmt.Errorf("备份镜像切换前数据库失败: %w", backupErr)
						}
					}
					pending = &pendingUpdate{Request: update.InstallRequest{Action: "image", HistoryID: id, FromVersion: previousInfo.Version, ToVersion: builtinInfo.Version, DatabasePath: source, DatabaseBackup: recoveryDB, ResultPath: filepath.Join(dataDir, "updates", "results", fmt.Sprintf("%d.json", id))}, Previous: startupFallback, Target: target, SuccessStatus: "success", SuccessMessage: "镜像升级成功，已运行 v" + builtinInfo.Version, NoFallback: incompatible}
					if incompatible {
						targetDB := filepath.Join(dataDir, "updates", "backups", builtinInfo.Version, "database", "ctyun-keeper.db")
						if _, statErr := os.Stat(source); statErr == nil {
							if info, targetErr := os.Stat(targetDB); targetErr != nil || !info.Mode().IsRegular() {
								err = errors.New("在线版本与当前 Launcher 不兼容，且缺少镜像内置版本的数据库备份；请更新 Docker 镜像")
							} else {
								pending.TargetDatabase = targetDB
							}
						}
						pending.SuccessMessage = "已安全回退镜像内置程序 v" + builtinInfo.Version + "；请更新 Docker 镜像"
					}
					if err == nil {
						err = saveNewPending(dataDir, pending, startupManager)
						startupManager = nil
					}
				}
			}
		}
		if err == nil {
			if pending != nil {
				err = preparePending(pending, currentLink)
			} else if target != resolveCurrent(currentLink, "") {
				err = switchCurrent(currentLink, target)
			}
		}
	} else {
		err = preparePending(pending, currentLink)
	}
	if err != nil {
		if startupManager != nil {
			startupManager.Finish()
		}
		log.Fatalf("选择启动版本失败: %v", err)
	}
	if startupManager != nil {
		startupManager.Finish()
	}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	for {
		current := resolveCurrent(currentLink, builtinDir)
		info, err := loadVersion(current)
		if err != nil {
			log.Fatalf("读取运行版本失败: %v", err)
		}
		if !launcherCompatible(info.MinimumLauncherVersion) {
			if current != builtinDir {
				log.Fatal("在线版本要求更高 Launcher，请重启容器以执行安全回退，或更新 Docker 镜像")
			}
			log.Fatal("镜像内置程序与 Launcher 不兼容，请更新 Docker 镜像")
		}
		healthy := func() {
			if pending != nil {
				if err := completePending(dataDir, pending); err != nil {
					log.Printf("提交更新结果失败，将保留恢复记录: %v", err)
					return
				}
				pending = nil
			}
			startupFallback = ""
		}
		if pending != nil {
			update.WriteExecutorProgress(pending.Request, update.StatusRestarting, "正在启动程序并检查健康状态")
		}
		code, healthErr := runProgram(current, info.Version, env("APP_PORT", "9845"), signals, healthy)
		if errors.Is(healthErr, errLauncherStopped) {
			os.Exit(0)
		}
		if healthErr != nil && pending != nil {
			if pending.Recovery {
				writeResult(pending.Request, "failed", "恢复原版本后健康检查仍失败，需要人工修复："+healthErr.Error())
				log.Fatal(healthErr)
			}
			log.Printf("新版本健康检查失败，正在回滚: %v", healthErr)
			failed := pending
			pending = nil
			if err := switchCurrent(currentLink, failed.Previous); err != nil {
				writeResult(failed.Request, "failed", "新版本失败且无法切回旧版本："+err.Error())
				log.Fatal(err)
			}
			restoreErr := restoreDatabase(failed.Request)
			if restoreErr != nil {
				writeResult(failed.Request, "failed", "已切回旧程序但数据库恢复失败："+restoreErr.Error())
				log.Fatal(restoreErr)
			}
			if failed.NoFallback {
				pending = &pendingUpdate{
					Request: failed.Request, Previous: failed.Previous, Target: failed.Previous, Recovery: true, NoFallback: true,
					SuccessStatus: "failed", SuccessMessage: "镜像内置版本健康检查失败；已恢复在线版本数据，请更新 Docker 镜像：" + healthErr.Error(),
				}
				if err := savePending(dataDir, pending); err != nil {
					log.Printf("保存失败恢复记录失败: %v", err)
				}
				log.Fatal(healthErr)
			}
			pending = &pendingUpdate{Request: failed.Request, Previous: failed.Previous, Target: failed.Previous, Recovery: true, SuccessStatus: "rolled_back", SuccessMessage: "新版本健康检查失败，已自动回滚"}
			if err := savePending(dataDir, pending); err != nil {
				log.Fatal(err)
			}
			continue
		}
		if code != update.ExitUpdateRequested {
			os.Exit(code)
		}
		request, err := consumeRequest(dataDir, current)
		if err != nil {
			log.Printf("容器内更新请求无效: %v", err)
			update.ReleaseUpdateLock(dataDir)
			continue
		}
		if err := update.TakeOverUpdateLock(dataDir, request.Token); err != nil {
			writeResult(request, "failed", err.Error())
			continue
		}
		if err := update.FinalizeDatabaseBackup(request); err != nil {
			writeResult(request, "failed", err.Error())
			update.ReleaseUpdateLock(dataDir)
			continue
		}
		update.WriteExecutorProgress(request, update.StatusInstalling, "正在切换运行版本")
		previous, target, err := applyUpdate(request, versionsDir, currentLink, builtinDir)
		if err != nil {
			writeResult(request, "failed", err.Error())
			log.Printf("容器内更新失败: %v", err)
			update.ReleaseUpdateLock(dataDir)
			continue
		}
		pending = &pendingUpdate{
			Request: request, Previous: previous, Target: target,
			SuccessStatus: "success", SuccessMessage: "已更新到 v" + request.ToVersion,
		}
	}
}

func chooseStartupVersion(currentLink, builtinDir string) (string, error) {
	target, fallback, _, err := selectStartupVersion(currentLink, builtinDir)
	if err != nil {
		return "", err
	}
	if target != resolveCurrent(currentLink, "") {
		if err := switchCurrent(currentLink, target); err != nil {
			return "", err
		}
	}
	return fallback, nil
}

func selectStartupVersion(currentLink, builtinDir string) (target, fallback string, incompatible bool, err error) {
	builtin, err := loadVersion(builtinDir)
	if err != nil {
		return "", "", false, err
	}
	current := resolveCurrent(currentLink, "")
	if current == "" {
		return builtinDir, "", false, nil
	}
	currentInfo, err := loadVersion(current)
	if err != nil || !launcherCompatible(currentInfo.MinimumLauncherVersion) {
		if err == nil {
			_ = os.Setenv("CTYUN_IMAGE_UPDATE_REQUIRED", "在线版本要求更高 Launcher，已回退镜像内置程序，请更新 Docker 镜像")
			return builtinDir, current, true, nil
		}
		return builtinDir, "", false, nil
	}
	builtinVersion, builtinOK := update.ParseSemVer(builtin.Version)
	currentVersion, currentOK := update.ParseSemVer(currentInfo.Version)
	if builtinOK && (!currentOK || builtinVersion.Compare(currentVersion) > 0) && launcherCompatible(builtin.MinimumLauncherVersion) {
		return builtinDir, current, false, nil
	}
	return current, "", false, nil
}

func resolveCurrent(link, fallback string) string {
	target, err := os.Readlink(link)
	if err != nil || target == "" {
		return fallback
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		return filepath.Clean(target)
	}
	return fallback
}

func loadVersion(dir string) (versionInfo, error) {
	var info versionInfo
	data, err := os.ReadFile(filepath.Join(dir, "version.json"))
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return info, err
	}
	info.Version = strings.TrimSpace(info.Version)
	if info.Version == "" {
		return info, errors.New("version.json 缺少版本")
	}
	if _, ok := update.ParseSemVer(info.Version); !ok && !update.IsDevVersion(info.Version) {
		return info, errors.New("version.json 版本无效")
	}
	return info, nil
}

func launcherCompatible(minimum string) bool {
	if minimum == "" {
		return true
	}
	required, requiredOK := update.ParseSemVer(minimum)
	current, currentOK := update.ParseSemVer(launcherVersion)
	return requiredOK && currentOK && current.Compare(required) >= 0
}

var errLauncherStopped = errors.New("launcher stopped")

func runProgram(current, version, port string, signals <-chan os.Signal, onHealthy func()) (int, error) {
	exe := filepath.Join(current, "ctyun-keeper")
	cmd := exec.Command(exe)
	cmd.Dir = current
	cmd.Env = replaceEnv(os.Environ(), map[string]string{
		"CTYUN_STATIC_DIR":       filepath.Join(current, "static"),
		"CTYUN_RUNTIME_VERSION":  version,
		"CTYUN_LAUNCHER_VERSION": launcherVersion,
	})
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	health := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), healthTimeout+5*time.Second)
		defer cancel()
		health <- update.WaitForHealth(ctx, "http://127.0.0.1:"+port+"/health", version, healthTimeout, healthInterval, 3)
	}()
	healthPending := true
	stopping := false
	for {
		select {
		case sig := <-signals:
			stopping = true
			_ = cmd.Process.Signal(sig)
		case err := <-health:
			if stopping {
				if err != nil {
					_ = cmd.Process.Kill()
					<-wait
					return 0, errLauncherStopped
				}
				continue
			}
			healthPending = false
			if err != nil {
				_ = cmd.Process.Kill()
				<-wait
				return 1, err
			}
			onHealthy()
		case err := <-wait:
			code := exitCode(err)
			if stopping {
				return code, errLauncherStopped
			}
			if code == update.ExitUpdateRequested {
				return code, nil
			}
			if healthPending {
				return code, errors.New("程序在健康检查完成前退出")
			}
			return code, nil
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 1
}

func replaceEnv(values []string, replacements map[string]string) []string {
	out := values[:0]
	for _, value := range values {
		key, _, _ := strings.Cut(value, "=")
		if _, replace := replacements[key]; !replace {
			out = append(out, value)
		}
	}
	for key, value := range replacements {
		out = append(out, key+"="+value)
	}
	return out
}

func consumeRequest(dataDir, current string) (update.InstallRequest, error) {
	path := filepath.Join(dataDir, "updates", "install-request.json")
	request, err := update.ReadInstallRequest(path)
	if err != nil {
		return request, err
	}
	_ = os.Remove(path)
	token, err := update.ConsumeInstallToken(dataDir)
	if err != nil {
		return request, err
	}
	if err := request.Validate(dataDir, current, token); err != nil {
		return request, err
	}
	if request.Executable != filepath.Join(current, "ctyun-keeper") {
		return request, errors.New("更新请求与当前运行程序不匹配")
	}
	return request, nil
}

func applyUpdate(req update.InstallRequest, versionsDir, currentLink, builtinDir string) (string, string, error) {
	if err := verifyInstallPolicy(req, builtinDir); err != nil {
		return "", "", err
	}
	if !launcherCompatible(req.MinimumLauncherVersion) {
		return "", "", errors.New("当前 Launcher 版本不满足更新包要求，请拉取新 Docker 镜像")
	}
	if err := update.VerifyPackageWithManifestDigest(req.StagingDir, req.ToVersion, "linux-"+runtime.GOARCH, req.PackageManifestSHA256); err != nil {
		return "", "", fmt.Errorf("更新包二次校验失败: %w", err)
	}
	previous := resolveCurrent(currentLink, "")
	if !updatePathWithin(versionsDir, previous) {
		snapshot := filepath.Join(versionsDir, "v"+req.FromVersion)
		if err := os.RemoveAll(snapshot); err != nil {
			return "", "", err
		}
		if err := update.CopyDir(previous, snapshot); err != nil {
			return "", "", fmt.Errorf("保存镜像内置版本快照: %w", err)
		}
		previous = snapshot
	}
	targetDir := filepath.Join(versionsDir, "v"+req.ToVersion)
	if !updatePathWithin(versionsDir, targetDir) {
		return "", "", errors.New("目标版本目录无效")
	}
	if err := os.RemoveAll(targetDir); err != nil {
		return "", "", err
	}
	if err := os.Rename(req.StagingDir, targetDir); err != nil {
		return "", "", err
	}
	if err := writeVersion(targetDir, req.ToVersion, req.MinimumLauncherVersion); err != nil {
		return "", "", err
	}
	journal := &pendingUpdate{Request: req, Previous: previous, Target: targetDir, SuccessStatus: "success", SuccessMessage: "已更新到 v" + req.ToVersion}
	if err := savePending(filepath.Dir(filepath.Dir(versionsDir)), journal); err != nil {
		return "", "", err
	}
	if err := switchCurrent(currentLink, targetDir); err != nil {
		_ = os.RemoveAll(targetDir)
		_ = os.Remove(filepath.Join(filepath.Dir(filepath.Dir(versionsDir)), "updates", "pending.json"))
		return "", "", err
	}
	return previous, targetDir, nil
}

func writeVersion(dir, version, minimumLauncherVersion string) error {
	data, _ := json.Marshal(versionInfo{Version: version, MinimumLauncherVersion: minimumLauncherVersion})
	return os.WriteFile(filepath.Join(dir, "version.json"), data, 0640)
}

func switchCurrent(link, target string) error {
	if target == "" {
		return errors.New("切换目标为空")
	}
	if err := os.MkdirAll(filepath.Dir(link), 0750); err != nil {
		return err
	}
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

func preparePending(pending *pendingUpdate, currentLink string) error {
	if pending == nil {
		return nil
	}
	if pending.TargetDatabase != "" {
		if err := update.RestoreDatabaseFile(pending.TargetDatabase, pending.Request.DatabasePath); err != nil {
			return err
		}
	}
	return switchCurrent(currentLink, pending.Target)
}

func restoreDatabase(req update.InstallRequest) error {
	if req.DatabaseBackup == "" {
		return nil
	}
	return update.RestoreDatabaseFile(req.DatabaseBackup, req.DatabasePath)
}

func writeResult(req update.InstallRequest, status, message string) error {
	err := update.WriteInstallResult(req.ResultPath, update.ResultFor(req, status, message, "linux-"+runtime.GOARCH))
	if err != nil {
		log.Printf("写入更新结果失败: %v", err)
	}
	return err
}

func updatePathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func completePending(dataDir string, pending *pendingUpdate) error {
	if pending.CompletedResult == nil {
		result := update.ResultFor(pending.Request, pending.SuccessStatus, pending.SuccessMessage, "linux-"+runtime.GOARCH)
		pending.CompletedResult = &result
		if err := savePending(dataDir, pending); err != nil {
			return err
		}
	}
	if err := update.WriteInstallResult(pending.Request.ResultPath, *pending.CompletedResult); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dataDir, "updates", "pending.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	update.ReleaseUpdateLock(dataDir, pending.Request.Token)
	return nil
}

func validatePending(pending *pendingUpdate, dataDir, builtinDir, versionsDir string) error {
	if pending == nil || pending.Previous == "" || pending.Target == "" {
		return errors.New("缺少版本切换路径")
	}
	if len(pending.Request.Token) != 64 || pending.Request.HistoryID <= 0 {
		return errors.New("事务令牌或历史记录编号无效")
	}
	if pending.SuccessStatus != "success" && pending.SuccessStatus != "failed" && pending.SuccessStatus != "rolled_back" {
		return errors.New("完成状态无效")
	}
	expectedResult := filepath.Join(dataDir, "updates", "results", fmt.Sprintf("%d.json", pending.Request.HistoryID))
	if filepath.Clean(pending.Request.ResultPath) != filepath.Clean(expectedResult) || hasLinkedPath(dataDir, expectedResult) {
		return errors.New("结果文件路径无效")
	}
	previousVersion, err := validateVersionDir(pending.Previous, versionsDir, builtinDir)
	if err != nil {
		return fmt.Errorf("上一版本路径无效: %w", err)
	}
	targetVersion, err := validateVersionDir(pending.Target, versionsDir, builtinDir)
	if err != nil {
		return fmt.Errorf("目标版本路径无效: %w", err)
	}
	if previousVersion != pending.Request.FromVersion {
		return errors.New("上一版本与更新事务不一致")
	}
	wantedTarget := pending.Request.ToVersion
	if pending.Recovery {
		wantedTarget = pending.Request.FromVersion
	}
	if targetVersion != wantedTarget {
		return errors.New("目标版本与更新事务不一致")
	}
	if filepath.Clean(pending.Request.DatabasePath) != filepath.Clean(filepath.Join(dataDir, "ctyun-keeper.db")) {
		return errors.New("数据库路径无效")
	}
	if hasLinkedPath(dataDir, pending.Request.DatabasePath) {
		return errors.New("数据库路径不能是符号链接")
	}
	if pending.TargetDatabase != "" {
		if !validDatabaseBackup(dataDir, pending.TargetDatabase) {
			return errors.New("目标数据库恢复文件无效")
		}
	}
	if pending.CompletedResult != nil {
		result := pending.CompletedResult
		if result.HistoryID != pending.Request.HistoryID || result.FromVersion != pending.Request.FromVersion || result.Version != pending.Request.ToVersion || result.Status != pending.SuccessStatus {
			return errors.New("完成结果与更新事务不一致")
		}
	}
	if pending.Request.Action == "image" {
		if pending.Request.DatabaseBackup != "" && !validDatabaseBackup(dataDir, pending.Request.DatabaseBackup) {
			return errors.New("镜像切换数据库备份无效")
		}
		return nil
	}
	req := pending.Request
	req.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if filepath.Clean(req.Executable) != filepath.Clean(filepath.Join(pending.Previous, "ctyun-keeper")) {
		return errors.New("运行程序路径与上一版本不一致")
	}
	if err := req.Validate(dataDir, pending.Previous, req.Token); err != nil {
		return err
	}
	return verifyInstallPolicy(req, builtinDir)
}

func verifyInstallPolicy(req update.InstallRequest, builtinDir string) error {
	manifest, err := update.VerifySignedRequestManifest(req, trustedUpdatePublicKey(), "linux-"+runtime.GOARCH)
	if err != nil {
		return err
	}
	builtin, err := loadVersion(builtinDir)
	if err != nil {
		return fmt.Errorf("读取镜像内置版本失败: %w", err)
	}
	platform := update.Platform{
		OS: "linux", Arch: runtime.GOARCH, InDocker: true,
		LauncherVersion: launcherVersion,
		ImageVersion:    builtin.Version,
	}
	return manifest.CompatibleWith(req.FromVersion, platform)
}

func trustedUpdatePublicKey() string {
	if value := strings.TrimSpace(updatePublicKey); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("CTYUN_UPDATE_PUBLIC_KEY"))
}

func validDatabaseBackup(dataDir, candidate string) bool {
	inBackups := resolvedPathWithin(filepath.Join(dataDir, "updates", "backups"), candidate)
	inRecovery := resolvedPathWithin(filepath.Join(dataDir, "updates", "recovery"), candidate)
	info, err := os.Lstat(candidate)
	return (inBackups || inRecovery) && err == nil && info.Mode().IsRegular() && !hasLinkedPath(dataDir, candidate)
}

func resolvedPathWithin(root, candidate string) bool {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	return err == nil && updatePathWithin(resolvedRoot, resolvedCandidate)
}

func hasLinkedPath(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	current := filepath.Clean(root)
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

func validateVersionDir(candidate, versionsDir, builtinDir string) (string, error) {
	clean := filepath.Clean(candidate)
	if clean != filepath.Clean(builtinDir) && !updatePathWithin(versionsDir, clean) {
		return "", errors.New("路径超出受信任版本目录")
	}
	info, err := os.Lstat(clean)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("版本目录不存在或是符号链接")
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", err
	}
	if clean != filepath.Clean(builtinDir) && !updatePathWithin(versionsDir, resolved) {
		return "", errors.New("版本目录解析后超出受信任目录")
	}
	version, err := loadVersion(clean)
	if err != nil {
		return "", err
	}
	return version.Version, nil
}

func savePending(dataDir string, pending *pendingUpdate) error {
	path := filepath.Join(dataDir, "updates", "pending.json")
	raw, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func saveNewPending(dataDir string, pending *pendingUpdate, manager *update.Manager) error {
	if pending == nil {
		return errors.New("更新事务为空")
	}
	if pending.Request.Token == "" {
		token, err := update.NewRequestToken()
		if err != nil {
			return err
		}
		pending.Request.Token = token
	}
	if manager == nil {
		return errors.New("缺少启动更新锁")
	}
	if err := savePending(dataDir, pending); err != nil {
		manager.Finish()
		return err
	}
	return manager.HandOff(pending.Request.Token)
}
