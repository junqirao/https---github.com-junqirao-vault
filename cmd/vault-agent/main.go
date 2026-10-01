// Package main 是 Vault-Agent（客户端本地 Go 代理）入口。
//
// 两种用法：
//
//	Vault-Agent [serve 选项]            # 默认：启动本地代理（见下方启动顺序）
//	Vault-Agent apply-update [选项]     # 替换器：由自更新流程启动的临时副本进程使用
//
// 启动顺序（serve，默认）：
//  1. 解析命令行（-token / -config / -version / -log-dir / -port）
//  2. 初始化日志（失败时退化为仅控制台输出）并清理历史替换器副本
//  3. 获取全局单实例锁（同一台机器只允许一个代理进程）
//  4. 加载本地配置与状态（%ProgramData%\Vault）
//  5. 检测管理员权限并启动 Agent（本地 HTTP + 心跳 + 服务端事件订阅）
//  6. 向 stdout 打印 `AGENT_READY port=<port> token=<token>`，供 Electron 主进程解析
//
// 优雅停机：收到 os.Interrupt 后停止心跳、关闭本地 HTTP，**不自动卸载**已有挂载
// （避免打断用户正在使用的数据）；需要「先卸载再退出」时由前端调用 POST /agent/shutdown。
//
// 注意：本程序仅支持 Windows（依赖 Windows 存储 / iSCSI 发起端能力）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"vault/internal/agent"
	"vault/internal/lock"
	"vault/internal/logging"
	"vault/internal/secret"
	appversion "vault/internal/version"
)

// version 由构建期注入：-ldflags "-X main.version=0.1.0-dev"。
var version = ""

const (
	// singleInstanceName 是全局命名互斥体名。
	singleInstanceName = `Global\VaultAgent`
	// shutdownTimeout 是优雅停机的等待上限。
	shutdownTimeout = 15 * time.Second
	// unmountTimeout 是退出前卸载全部挂载的等待上限。
	unmountTimeout = 2 * time.Minute
	// applyUpdateSubcommand 是"替换器"子命令名（与 update.go 中的常量保持一致）。
	applyUpdateSubcommand = "apply-update"
)

func main() {
	os.Exit(run())
}

func run() int {
	// 被**双击**启动时隐藏系统自动创建的控制台窗口，避免出现"黑框"。
	// 从 cmd/PowerShell 手工启动时该函数不会动手（控制台里还有其他进程），
	// 所以 `-h` / `-version` 等输出依然可见。
	agent.HideOwnConsoleWindow()

	// ---- 子命令派发：必须放在最前面 ----
	//
	// apply-update 是**替换器**路径：它由自更新流程以"当前 exe 的临时副本"身份启动，
	// 因此**绝不能**触碰单实例锁、日志初始化或本地 HTTP —— 否则副本会因锁冲突无法运行，
	// 而这正是"运行中的 exe 无法覆盖自身"所要绕开的问题。
	if len(os.Args) > 1 && os.Args[1] == applyUpdateSubcommand {
		return runApplyUpdate(os.Args[2:])
	}

	fs := flag.NewFlagSet("Vault-Agent", flag.ContinueOnError)
	token := fs.String("token", "", "本地访问令牌（缺省时读 VAULT_AGENT_TOKEN，仍为空则自动生成并打印）")
	configPath := fs.String("config", "", `本地配置文件路径（默认 %ProgramData%\Vault\agent-config.json）`)
	logDir := fs.String("log-dir", "", `日志目录（默认 <数据目录>\logs）`)
	port := fs.Int("port", 0, "本地监听端口，0 表示由系统分配")
	showVersion := fs.Bool("version", false, "打印版本信息后退出")
	fs.Usage = func() { printUsage(fs.Output(), fs) }
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if v := strings.TrimSpace(version); v != "" {
		appversion.Version = v
	}
	if *showVersion {
		fmt.Printf("Vault-Agent %s (api_version=%d, commit=%s, built=%s)\n",
			appversion.Version, appversion.APIVersion, appversion.GitCommit, appversion.BuildTime)
		return 0
	}

	// ---- 本地令牌：参数 > 环境变量 > 自动生成 ----
	localToken := strings.TrimSpace(*token)
	if localToken == "" {
		localToken = strings.TrimSpace(os.Getenv("VAULT_AGENT_TOKEN"))
	}
	generated := false
	if localToken == "" {
		t, err := secret.RandomToken(24)
		if err != nil {
			fmt.Fprintln(os.Stderr, "生成本地令牌失败:", err)
			return 1
		}
		localToken = t
		generated = true
	}

	// ---- 日志 ----
	dir := strings.TrimSpace(*logDir)
	if dir == "" {
		dir = filepath.Join(agent.DataDir(), "logs")
	}
	logger, err := logging.New(logging.Options{
		Level:       "info",
		Dir:         dir,
		FileName:    "Vault-Agent",
		KeepDays:    14,
		AlsoConsole: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化日志失败（退化为仅控制台输出）:", err)
		logger, err = logging.New(logging.Options{Level: "info", AlsoConsole: true})
		if err != nil {
			fmt.Fprintln(os.Stderr, "初始化日志失败:", err)
			return 1
		}
	}
	defer logger.Close() //nolint:errcheck
	log := logger.Logger

	// ---- 清理历史替换器副本（best-effort；正在运行的副本删不掉，忽略即可）----
	agent.CleanupStaleUpdaterCopies(log)

	// ---- 单实例锁 ----
	instance, err := lock.AcquireSingleInstance(singleInstanceName)
	if err != nil {
		log.Error("代理已在运行，无法启动（单实例锁获取失败）", "error", err, "mutex", singleInstanceName)
		fmt.Fprintln(os.Stderr, "启动失败：Vault-Agent 已在运行（单实例锁获取失败）：", err)
		return 1
	}
	defer instance.Release() //nolint:errcheck

	// ---- 本地配置与状态（状态文件与配置同目录）----
	cfgPath := strings.TrimSpace(*configPath)
	if cfgPath == "" {
		cfgPath = agent.DefaultConfigPath()
	}
	statePath := filepath.Join(filepath.Dir(cfgPath), "agent-state.json")

	// ---- 自更新启动对账（必须在代理启动之前）----
	//
	// 上一次自更新若已把新 exe 换上去，本进程就是新版本：对账会清理 pending 与备份。
	// 若版本没变（替换/重启失败），累计失败次数并在达到阈值时回滚——
	// 回滚由 updater 子进程完成（运行中的 exe 无法覆盖自身），此时本进程直接退出。
	updateDir := filepath.Join(filepath.Dir(cfgPath), "update")
	reconcile, err := agent.ReconcileUpdate(updateDir, appversion.Version, log)
	if err != nil {
		log.Warn("自更新启动对账失败（不影响本次启动）", "error", err)
	}
	if reconcile.RolledBack {
		log.Error("自更新连续失败，已交由 updater 用备份回滚并重启；本进程立即退出")
		return 0
	}

	ag, err := agent.New(agent.Options{
		Token:      localToken,
		Port:       *port,
		ConfigPath: cfgPath,
		StatePath:  statePath,
		Logger:     log,
		// 把日志文件访问交给代理，供 GET /agent/log 日志模块读取（见 Agent.Options.LogSource）。
		LogSource: logger,
		Version:   appversion.Version,
	})
	if err != nil {
		log.Error("初始化代理失败", "error", err)
		fmt.Fprintln(os.Stderr, "初始化代理失败:", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, err := ag.Start(ctx)
	if err != nil {
		log.Error("启动代理失败", "error", err)
		fmt.Fprintln(os.Stderr, "启动代理失败:", err)
		return 1
	}

	portStr := "0"
	if _, p, err := net.SplitHostPort(addr); err == nil {
		portStr = p
	}
	log.Info("代理已就绪", "addr", addr, "admin", ag.Admin(), "version", ag.Version())
	if generated {
		log.Warn("未提供本地令牌，已自动生成（仅本次运行有效）")
	}

	// Electron 主进程据此解析连接信息。
	fmt.Printf("AGENT_READY port=%s token=%s\n", portStr, localToken)

	// ---- 优雅停机 ----
	//
	// 三条退出路径：
	//   - POST /agent/shutdown：先卸载全部挂载，再停止（托盘「退出」）；
	//   - 自更新（POST /agent/update/apply）：**不卸载挂载**——注册表/配置里的挂载记录已落盘，
	//     updater 重启新版本后由自动挂载恢复；更新期间 iSCSI 会话必然短暂断开，这是刻意取舍
	//     （见 docs/implementation.md 7.4「不影响挂载」的说明）；
	//   - 操作系统信号（Ctrl+C / SIGTERM）：**不自动卸载**，避免打断用户正在使用的数据。
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	viaAPI := false
	select {
	case sig := <-signals:
		log.Info("收到退出信号，开始优雅停机（不自动卸载已有挂载）", "signal", sig.String())
	case <-ag.Done():
		viaAPI = true
		if ag.UpdateStarted() {
			log.Info("收到退出请求（自更新），保留挂载：替换器重启后将自动恢复")
		} else {
			log.Info("收到退出请求（POST /agent/shutdown），卸载全部挂载后退出")
		}
	}

	if viaAPI && !ag.UpdateStarted() {
		unmountCtx, cancelUnmount := context.WithTimeout(context.Background(), unmountTimeout)
		ag.UnmountAll(unmountCtx)
		cancelUnmount()
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := ag.Stop(shutdownCtx); err != nil {
		log.Warn("停机未完全成功", "error", err)
	}
	return 0
}

// runApplyUpdate 执行"替换器"路径：解析 apply-update 参数并调用 agent.RunApplyUpdate。
//
// 该路径**不初始化日志、不获取单实例锁、不启动本地 HTTP**：它运行在代理已退出之后，
// 只做"等待父进程退出 → 备份 → 替换 →（可选）重启"。
// 返回值即进程退出码（0 成功、非 0 失败）。
func runApplyUpdate(args []string) int {
	fs := flag.NewFlagSet(applyUpdateSubcommand, flag.ContinueOnError)
	pid := fs.Int("pid", 0, "需要等待退出的父进程 PID（必填）")
	staged := fs.String("staged", "", "新文件路径（必填，绝对路径）")
	target := fs.String("target", "", "目标文件路径（必填，绝对路径）")
	backup := fs.String("backup", "", "备份路径（可选；给出时会先备份 target）")
	restart := fs.Bool("restart", false, "替换成功后启动 target")
	timeout := fs.Duration("timeout", agent.DefaultUpdateTimeout, "等待父进程退出的超时")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	opt := agent.UpdateApplyOptions{
		ParentPID:  *pid,
		StagedPath: *staged,
		TargetPath: *target,
		BackupPath: *backup,
		Restart:    *restart,
		Timeout:    *timeout,
	}
	// 日志传 nil：该路径刻意不接入统一日志，替换器需要的是独立可读的 updater.log。
	if err := agent.RunApplyUpdate(context.Background(), opt, nil); err != nil {
		return 1
	}
	return 0
}

// printUsage 打印 Vault-Agent 的完整用法（同时列出 serve 与 apply-update 两种模式）。
func printUsage(w io.Writer, fs *flag.FlagSet) {
	fs.SetOutput(w)
	fmt.Fprint(w, `Vault-Agent —— Vault 客户端本地代理（仅 Windows）

用法:
  Vault-Agent [选项]                 启动本地代理（默认 serve）
  Vault-Agent apply-update [选项]    以替换器身份运行（由自更新流程的临时副本调用）

serve 选项:
`)
	fs.PrintDefaults()
	fmt.Fprint(w, `
apply-update 选项:
  -pid <n>        需要等待退出的父进程 PID（必填）
  -staged <path>  新文件路径（必填，绝对路径）
  -target <path>  目标文件路径（必填，绝对路径）
  -backup <path>  备份路径（可选）
  -restart        替换成功后启动 target
  -timeout <dur>  等待父进程退出的超时（默认 120s）

说明:
  - 运行中的 exe 无法覆盖自身：自更新时本程序把**自身**复制为临时副本
    （%ProgramData%\Vault\update\updater-<pid>-<rand>.exe），并以 apply-update 启动它；
    副本从另一个文件运行，因此可以覆盖正式路径上的 Vault-Agent.exe。
`)
}
