// Package main 是 Vault 服务端入口。
//
// 三种用法：
//
//	Vault-Server [serve 选项]       启动服务端（默认：**前台运行**，日志直接打印在当前控制台）
//	Vault-Server --background       派生一个脱离本进程的子进程在**后台**运行服务端
//	Vault-Server stop [选项]        向正在运行的服务端发送停止请求（优雅停机）
//	Vault-Server sign <子命令>      发布方签名工具（构建/发布阶段使用，见下）
//
// 关于运行模式（重要）：
//
//	默认前台：进程就跑在启动它的控制台里，日志、横幅、panic 栈全部直接可见，
//	按 Ctrl+C 停止。排障时**必须**用这种方式——后台模式下 stdout/stderr 无人接收，
//	进程一旦崩溃就完全没有任何提示。
//	只有显式传入 --background 才走后台：探测服务端是否已在运行，
//	否则派生一个脱离控制台的子进程作为后台服务，等它就绪后打印摘要并退出本进程。
//	该路径见 runLauncher（launcher_windows.go / launcher_other.go）。
//
//	后台子进程同样会把 stdout/stderr 重定向到 <配置目录>/logs/ 下的文件，
//	这样即使以后台方式运行，Go 运行时的 panic / fatal error 也不会丢（见 spawnDetached）。
//
// 启动顺序（serve，默认，见 docs/implementation.md）：
//  1. 解析命令行并加载一次配置（日志目录、单实例名需要配置）
//  2. 初始化日志
//  3. 获取全局单实例锁（同一台机器只允许一个服务端进程）
//  4. 加载配置监听器（热重载 + 变更审计）
//  5. 初始化主密钥与数据保险箱
//  6. 初始化自建 CA
//  7. 打开数据库并迁移
//  8. 装配应用服务与任务处理器，执行一次启动对账
//  9. 启动任务 worker 与后台定时任务
//  10. 启动 HTTP(S) 服务并等待优雅停机
//
// 注意：服务端的"虚拟磁盘 / iSCSI 目标 / 存储池"由平台后端提供，按构建产物选择——
// Windows 包用 VHDX + IscsiTarget 模块，Linux 包用 LVM thin LV + LIO。
// 装配入口见 buildPlatform（platform_windows.go / platform_linux.go），
// 本文件不出现任何平台专属类型。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"vault/internal/api"
	"vault/internal/app"
	"vault/internal/cert"
	"vault/internal/config"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
	"vault/internal/logging"
	"vault/internal/platform"
	"vault/internal/secret"
	"vault/internal/signtool"
	"vault/internal/store"
	appversion "vault/internal/version"
)

// version 由构建期注入：-ldflags "-X main.version=0.1.0-dev"。
var version = ""

// fatalLogPath 记录本次运行的固定日志文件路径，供 main 最外层 panic 兜底直接落盘。
//
// 在 runServe 初始化日志时写入；在此之前（例如加载配置阶段）为空，此时兜底只写 stderr。
var fatalLogPath string

// logDirName / logFileName 是固定日志文件的目录名与文件名。
//
// 路径解析为 <配置文件所在目录>/logs/vault-server.log：位置固定、便于运维查找，
// 与 log.dir 的按天切分文件无关（双击/计划任务启动时 stdout 无人接收，此处是现场兜底）。
const (
	logDirName  = "logs"
	logFileName = "vault-server.log"
	// backgroundLogFileName 是后台子进程的 stdout/stderr 落盘文件名。
	//
	// 后台进程没有控制台，Go 运行时的 panic / fatal error 只写 fd 1/2，
	// 不落这里就会彻底消失（表现就是"点一下就宕机、没有任何提示"）。
	backgroundLogFileName = "vault-server.background.log"
)

// backgroundLogPathFor 返回后台子进程控制台输出的落盘路径：<配置目录>/logs/vault-server.background.log。
func backgroundLogPathFor(configPath string) string {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		abs = configPath
	}
	return filepath.Join(filepath.Dir(abs), logDirName, backgroundLogFileName)
}

// backgroundChildArgs 由当前命令行参数构造后台子进程的参数：
// 去掉 --background（避免子进程再次派生），并追加 -detached 内部标记。
func backgroundChildArgs(raw []string) []string {
	out := make([]string, 0, len(raw)+1)
	for _, a := range raw {
		if a == "-"+backgroundFlagName || a == "--"+backgroundFlagName {
			continue
		}
		out = append(out, a)
	}
	return append(out, "-"+detachedFlagName)
}

const (
	// singleInstanceName 是全局命名互斥体名。
	singleInstanceName = `Global\VaultServer`
	// defaultAuditRetentionDays 是审计记录保留天数。
	defaultAuditRetentionDays = 180
	// shutdownTimeout 是优雅停机的等待上限。
	shutdownTimeout = 15 * time.Second
	// signSubcommand 是"发布方签名工具"的子命令名。
	signSubcommand = "sign"
	// stopSubcommand 是"停止运行中的服务端"的子命令名。
	stopSubcommand = "stop"
	// backgroundFlagName 是"后台运行"开关名：只有显式指定它才派生后台进程。
	backgroundFlagName = "background"
)

func main() {
	code, promptHandled := runWithPanicGuard()

	// 双击运行时：失败后控制台窗口会立刻关闭，用户根本看不到错误原因。
	// 因此在"交互式控制台 + 未自行提示 + 失败退出"时等待一次回车。
	//
	// promptHandled=true 的场景（sign / stop 子命令、启动器路径）都已自行给出
	// 明确输出或已自行等待输入，不能再暂停一次：
	//   - sign / stop 是给脚本/CI 用的，暂停会让脚本挂住；
	//   - 启动器路径自己就会"按回车退出"，重复暂停会多按一次。
	// 计划任务/服务方式运行时 stdout 不是控制台设备，也不会暂停，避免无人值守被卡住。
	if code != 0 && !promptHandled && isInteractiveConsole() {
		fmt.Fprintln(os.Stderr, "\n启动失败。请按回车键关闭窗口。")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}

	os.Exit(code)
}

// runWithPanicGuard 包住 run()，兜住 main 最外层（含初始化阶段）的 panic。
//
// 目的：初始化阶段的 panic 若无人处理，进程会静默消失、现场全丢。这里统一捕获，
// 把带完整调用栈的 FATAL 记录写到固定日志文件（尽力而为）与 stderr，再以非 0 退出。
func runWithPanicGuard() (code int, promptHandled bool) {
	defer func() {
		if r := recover(); r != nil {
			writeFatalPanic(r, debug.Stack())
			code, promptHandled = 1, false
		}
	}()
	return run()
}

// writeFatalPanic 把 main 最外层的未捕获 panic 写到固定日志文件（若已知路径）与 stderr。
//
// 之所以直接追加写文件而不复用 slog：runServe 的 defer 已经关闭了日志文件句柄，
// panic 传到此处时原有 logger 已不可用，只能重新打开、尽力而为。
func writeFatalPanic(recovered any, stack []byte) {
	fmt.Fprintf(os.Stderr, "FATAL panic: %v\n%s\n", recovered, stack)
	if fatalLogPath == "" {
		return
	}
	f, err := os.OpenFile(fatalLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	entry, err := json.Marshal(map[string]any{
		"ts_ms": time.Now().UnixMilli(),
		"level": "ERROR",
		"msg":   "main 未捕获 panic，进程将退出",
		"panic": fmt.Sprint(recovered),
		"stack": string(stack),
	})
	if err != nil {
		return
	}
	_, _ = f.Write(append(entry, '\n'))
}

// isInteractiveConsole 判断当前进程是否挂在交互式控制台上。
//
// 依据：stdout 是"字符设备"。被重定向到文件/管道、或由计划任务与 SYSTEM
// 无人值守启动时，都不是字符设备，此时不应暂停等待输入。
func isInteractiveConsole() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// printStartupBanner 在控制台打印启动摘要（serve 前台运行路径）。
//
// 这是给"从命令行启动"这种用法准备的：默认配置下日志只写文件，控制台一片空白，
// 用户会误以为进程消失了。横幅与日志开关无关，始终输出到 stdout。
func printStartupBanner(raw config.Config, configPath string) {
	fmt.Printf("\nVault 服务端 %s 已启动\n", appversion.Version)
	printStartupSummary(raw, configPath)
	fmt.Printf("\n服务已就绪（前台模式）：按 Ctrl+C 停止；后台模式请用 Vault-Server %s 停止。\n", stopSubcommand)
	fmt.Printf("若需脱离本窗口在后台运行，请改用：Vault-Server -%s\n", backgroundFlagName)
	fmt.Printf("首次使用请用客户端连接本地址完成初始化。\n\n")
}

// printLauncherBanner 在控制台打印后台启动的摘要（--background 启动器路径）。
//
// 与 serve 路径的横幅同源（共用 printStartupSummary），差异只在结尾：
// 这里没有 Ctrl+C 可用，改为提示"本窗口可以关闭"与停止命令。
func printLauncherBanner(raw config.Config, configPath string, pid int) {
	fmt.Printf("\nVault 服务端 %s 已在后台启动\n", appversion.Version)
	printStartupSummary(raw, configPath)
	fmt.Printf("  后台进程 : PID %d\n", pid)
	fmt.Printf("  后台输出 : %s（崩溃现场也在其中）\n", backgroundLogPathFor(configPath))
	fmt.Printf("\n服务已在后台运行，本窗口可以关闭（关闭它不会停止服务）。\n")
	fmt.Printf("停止服务：在本程序所在目录执行  Vault-Server %s\n", stopSubcommand)
	fmt.Printf("首次使用请用客户端连接本地址完成初始化。\n\n")
}

// printStartupSummary 打印启动摘要中两种路径共用的配置信息。
func printStartupSummary(raw config.Config, configPath string) {
	scheme := "http"
	if raw.HTTP.TLS.Enabled {
		scheme = "https（自签证书，首次连接需核对指纹）"
	}

	fmt.Printf("  配置文件 : %s\n", configPath)
	fmt.Printf("  监听地址 : %s  [%s]\n", raw.HTTP.Listen, scheme)
	fmt.Printf("  数据目录 : %s\n", strings.Join(raw.Storage.EffectiveRoots(), "; "))
	if raw.Log.Dir != "" {
		fmt.Printf("  日志目录 : %s\n", raw.Log.Dir)
	} else {
		fmt.Printf("  日志     : 仅输出到控制台（未配置 log.dir）\n")
	}
	fmt.Printf("  更新分发 : %s（通道 %s）\n", raw.Update.ArtifactsDir, raw.Update.Channel)
}

// run 执行一次完整的启动流程。
//
// 返回退出码与 promptHandled：后者为 true 表示"本次运行已自行给出结论或已自行
// 等待用户输入"，main 不应再补一次暂停（见 main 的注释）。
func run() (code int, promptHandled bool) {
	// ---- 子命令派发：第一个参数是 sign / stop 时走对应子命令 ----
	//
	// ⚠️ 服务端**运行时永不签名**：sign 子命令仅供发布方在构建/发布阶段使用，
	// 私钥不参与任何 serve 路径。默认（无子命令）行为保持为 serve，
	// 即 `Vault-Server -config config.yaml` 继续可用。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case signSubcommand:
			return runSign(os.Args[2:]), true
		case stopSubcommand:
			return runStop(os.Args[2:]), true
		}
	}

	fs := flag.NewFlagSet("Vault-Server", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "配置文件路径")
	showVersion := fs.Bool("version", false, "打印版本信息后退出")
	migrateOnly := fs.Bool("migrate-only", false, "仅执行数据库迁移后退出")
	background := fs.Bool(backgroundFlagName, false,
		"以后台方式运行：派生一个脱离本进程的子进程作为服务端（默认前台运行，日志直接可见）")
	detached := fs.Bool(detachedFlagName, false, "内部标记：由 --background 启动器自动添加，通常无需手工指定")
	fs.Usage = func() { printUsage(fs.Output(), fs) }
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2, false
	}
	if v := strings.TrimSpace(version); v != "" {
		appversion.Version = v
	}
	if *showVersion {
		fmt.Printf("Vault-Server %s (api_version=%d, commit=%s, built=%s)\n",
			appversion.Version, appversion.APIVersion, appversion.GitCommit, appversion.BuildTime)
		return 0, false
	}

	// ---- 0) 后台模式：只有显式 --background 才派生子进程 ----
	//
	// ⚠️ 判据只有一个：命令行显式给了 --background，且当前进程不是后台子进程
	// （启动器派生的子进程会带上 -detached，避免"子进程再派生子进程"）。
	//
	// 这里**不再**依据"是否独占控制台（双击启动）"自动切后台：
	// 默认一律前台运行，日志、横幅与 panic 栈直接打印在启动它的控制台上——
	// 后台进程的 stdout/stderr 无人接收，崩溃时会"静默消失、没有任何提示"，
	// 排障必须能看到现场。需要后台运行时请显式加 --background
	// （Windows 下另可用计划任务/服务托管）。
	//
	// 注意：启动器路径**不**获取单实例锁，只做 配置加载 → TCP 探测 → 派生子进程。
	if *background && !*detached {
		return runLauncher(*configPath), true
	}

	return runServe(configPath, migrateOnly), false
}

// configCreatedHint 返回"已从模板生成配置文件"的启动提示。
//
// ⚠️ 必须分平台写：storage.whitelist_root 在 Windows 上是 VHDX 的存放目录（要运维自己建），
// 在 Linux 上**不是**存储目录——「存储」是受管的 thin LV，只能在管理端创建，
// 该字段仅作 GET /v1/fs/* 与建库 source_dir 的浏览白名单。
// 照搬 Windows 文案会引导 Linux 运维去"设一个数据目录"，而那个目录永远不会被当作存储。
func configCreatedHint(path string) string {
	root := config.DefaultStorageRoot(runtime.GOOS)
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("未找到配置文件，已从模板生成: %s\n"+
			"  提示：请确认其中的 storage.whitelist_root 指向你的数据目录（模板默认 %s）。\n"+
			"        该文件已写入默认值，服务将继续启动；后续修改可保存后重启生效。\n",
			path, root)
	}
	return fmt.Sprintf("未找到配置文件，已从模板生成: %s\n"+
		"  提示：已按当前平台调整存储相关默认值（storage.whitelist_root=%s）。\n"+
		"        Linux 上该字段只作浏览白名单；「存储」请在管理端创建\n"+
		"        （服务端会自动 lvcreate → mkfs → 挂载）。\n"+
		"        该文件已写入默认值，服务将继续启动；后续修改可保存后重启生效。\n",
		path, root)
}

// runServe 执行 serve 主流程：前台运行，直到收到退出信号或跨进程停止请求。
//
// configPath 与 migrateOnly 来自 run 中的命令行解析结果。
func runServe(configPath *string, migrateOnly *bool) int {
	// ---- 1) 先加载一次配置：日志目录与实例信息都来自配置 ----
	// 若配置文件不存在，尝试从同目录的 config.example.yaml 生成一份：
	// 这样部署包只需携带模板（升级解压不会覆盖运维已改的配置），
	// 且首次启动能开箱可跑；计划任务无人值守启动时也不会因为"缺配置"而失败。
	if created, err := config.EnsureFromExample(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "准备配置文件失败:", err)
		return 1
	} else if created {
		fmt.Fprint(os.Stdout, configCreatedHint(*configPath))
	}

	loaded, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载配置失败:", err)
		return 1
	}
	raw := loaded.Raw

	// ---- 2) 日志 ----
	// 固定文件日志：<配置文件所在目录>/logs/vault-server.log。
	// 双击/计划任务启动时 stdout 无人接收，进程一旦因 panic 退出，现场全丢；
	// 落一个位置固定、易查找的文件，保证崩溃现场（含 panic 栈）可追溯。
	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析配置文件绝对路径失败:", err)
		return 1
	}
	logFilePath := filepath.Join(filepath.Dir(absConfig), logDirName, logFileName)
	fatalLogPath = logFilePath

	logger, err := logging.New(logging.Options{
		Level:          raw.Log.Level,
		Dir:            raw.Log.Dir,
		KeepDays:       raw.Log.KeepDays,
		AlsoConsole:    raw.Log.AlsoConsole,
		SingleFilePath: logFilePath,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化日志失败:", err)
		return 1
	}
	defer logger.Close() //nolint:errcheck
	log := logger.Logger
	log.Info("服务端启动中",
		"version", appversion.Version, "api_version", appversion.APIVersion,
		"instance_id", raw.Server.InstanceID, "config", *configPath)
	log.Info("日志同时写入固定文件", "path", logFilePath)

	// ---- 3) 单实例锁 ----
	instance, err := lock.AcquireSingleInstance(singleInstanceName)
	if err != nil {
		log.Error("服务端已在运行，无法启动（单实例锁获取失败）", "error", err, "mutex", singleInstanceName)
		// 同时输出到 stderr：服务化部署时错误提示需要可见（日志可能只落文件）。
		fmt.Fprintln(os.Stderr, "启动失败：服务端已在运行（单实例锁获取失败）：", err)
		return 1
	}
	defer instance.Release() //nolint:errcheck

	ctx := context.Background()

	// ---- 4) 主密钥 ----
	masterKey, encodedKey, generated, err := secret.LoadOrCreateMasterKey(raw.Security.MasterKey)
	if err != nil {
		log.Error("初始化主密钥失败", "error", err)
		return 1
	}
	// 实例 ID 缺失时补写；主密钥由本次生成时补写（写入会重新序列化 YAML，见函数注释）。
	if err := ensureBootstrapValues(*configPath, raw.Server.InstanceID, encodedKey, generated, log); err != nil {
		log.Error("写回首次启动生成的值失败", "error", err)
		return 1
	}
	box, err := secret.NewCipher(masterKey)
	if err != nil {
		log.Error("初始化数据保险箱失败", "error", err)
		return 1
	}

	// ---- 5) CA ----
	// absConfig 已在步骤 2 解析（固定日志文件路径复用同一份）。
	caDir := filepath.Join(filepath.Dir(absConfig), "pki")
	ca, err := cert.LoadOrCreateCA(cert.Config{
		Dir:        caDir,
		InstanceID: raw.Server.InstanceID,
		Logger:     log,
	})
	if err != nil {
		log.Error("初始化 CA 失败", "error", err, "dir", caDir)
		return 1
	}

	// ---- 6) 数据库 ----
	dialect, err := dialectOf(raw.Database.Driver)
	if err != nil {
		log.Error("数据库配置非法", "error", err)
		return 1
	}
	st, err := store.Open(ctx, store.Options{
		Dialect:      dialect,
		SQLitePath:   raw.Database.SQLite.Path,
		MySQLDSN:     raw.Database.MySQL.DSN,
		MaxOpenConns: raw.Database.MySQL.MaxOpenConns,
		MaxIdleConns: raw.Database.MySQL.MaxIdleConns,
		Logger:       log,
	})
	if err != nil {
		log.Error("打开数据库失败", "error", err)
		return 1
	}
	defer st.Close() //nolint:errcheck

	if err := st.Migrate(ctx); err != nil {
		log.Error("数据库迁移失败", "error", err)
		return 1
	}
	if *migrateOnly {
		log.Info("数据库迁移完成（-migrate-only）")
		return 0
	}

	// ---- 7) 配置热重载 ----
	lastRaw := raw
	watcher, err := config.NewWatcher(*configPath, func(next *config.Loaded) {
		if next == nil {
			// 配置写坏不应把运行中的服务打挂：保留旧配置并报错。
			log.Error("配置热重载失败，继续使用旧配置（请检查配置文件语法）")
			return
		}
		changes := diffConfig(lastRaw, next.Raw)
		lastRaw = next.Raw
		if len(changes) == 0 {
			log.Info("配置文件已重载（无生效变更）")
			return
		}
		log.Info("配置已热重载", "changes", strings.Join(changes, "; "))
		auditCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		entry := &domain.AuditLog{
			UserID:   "system",
			Action:   "config.reload",
			Resource: "config",
			Detail:   strings.Join(changes, "; "),
			Result:   domain.AuditResultOK,
		}
		if err := st.AppendAudit(auditCtx, entry); err != nil {
			log.Warn("记录配置变更审计失败", "error", err)
		}
	})
	if err != nil {
		log.Error("启动配置监听失败", "error", err)
		return 1
	}
	defer watcher.Close() //nolint:errcheck

	// ---- 8) 平台层 ----
	// 各后端由同一份上层代码驱动，具体实现随构建产物而定（见 platform_* 文件）。
	// StorageVol 在 Windows 上刻意留 nil（存储就是普通目录），Linux 上是 thin LV 卷后端。
	plat, err := buildPlatform(ctx, raw, log)
	if err != nil {
		log.Error("装配平台后端失败", "error", err)
		return 1
	}

	// ---- 9) 应用层装配 ----
	locks := lock.NewKeyed()
	jobs := job.NewWorker(st, locks, job.Options{Logger: log})
	security := raw.Security
	// 事件广播器需在应用层装配前创建：app.Deps.Events 依赖它（SSE 推送）。
	// app 层不 import api（避免循环依赖），因此用下面的适配器把 EventHub 接进去。
	events := api.NewEventHub()
	application := app.New(app.Deps{
		Store:    st,
		Cfg:      watcher,
		Log:      log,
		Locks:    locks,
		Jobs:     jobs,
		Cipher:   box,
		CA:       ca,
		Disk:     plat.Disk,
		Vol:      plat.Vol,
		Iscsi:    plat.Iscsi,
		Platform: plat.Platform,
		// StorageVol 为 nil 表示平台无存储卷概念（Windows）：存储按目录模式处理。
		StorageVol: plat.StorageVol,
		Tokens:     cert.NewTokenStore(security.EnrollmentTokenTTL.Std()),
		Sessions:   app.NewSessionStore(security.SessionTTL.Std()),
		Events:     eventSinkAdapter{hub: events},
	})
	application.RegisterJobHandlers()

	// 内置超级管理员：账号存放在数据库中，由初始化向导创建；
	// 配置文件里的 super_admin_enabled 仅作为可用性开关（见 app/auth.go SyncSuperAdminSwitch）。
	if err := application.Auth().SyncSuperAdminSwitch(ctx); err != nil {
		log.Error("应用超级管理员开关失败", "error", err)
	}

	// 记录首次启动时间，供初始化时间窗口判定使用。
	if err := application.EnsureFirstStart(ctx); err != nil {
		log.Error("记录首次启动时间失败", "error", err)
	}

	// 存储实体：若数据库中尚无任何存储记录，则用配置里的白名单根做**一次性种子**。
	// 之后 config.yaml 的 storage.whitelist_roots 不再实时生效，请在管理端「存储」模块维护。
	if n, err := application.Storages().EnsureSeededFromConfig(ctx, raw.Storage.EffectiveRoots()); err != nil {
		log.Error("从配置初始化存储种子失败（将在 DB 无存储时回退配置根）", "error", err)
	} else if n > 0 {
		log.Info("已从配置初始化存储，后续请在管理端维护", "count", n)
	}

	// 兼容区间若因默认推导不自洽而被放宽，必须留下 WARN，避免"保护被静默削弱"。
	if compat := watcher.Current().Compat; compat.WidenedUpperBound {
		log.Warn("客户端兼容区间的默认推导结果不自洽，已放宽上界为「不限制」",
			"hint", "请在 config.yaml 中显式配置 client_compat.min / max")
	}

	// 输出当前初始化状态，便于运维判断是否需要引导初始化。
	if st, err := application.BootstrapStatus(ctx); err != nil {
		log.Error("读取初始化状态失败", "error", err)
	} else if st.NeedsBootstrap {
		log.Warn("系统尚未初始化，请在客户端连接后按向导创建超级管理员",
			"bootstrap_enabled", st.BootstrapEnabled,
			"has_super_admin", st.HasSuperAdmin,
			"has_data", st.HasData)
	} else if st.BlockedReason != "" {
		log.Info("系统已初始化", "blocked_reason", st.BlockedReason)
	}

	// ---- 10) 启动对账（任务恢复 / 目标收敛 / 临时共享崩溃恢复）----
	//
	// ⚠️ 刻意放到**后台**，不挡住 HTTP 监听：一轮完整对账要跑大量 PowerShell ——
	// 单个 iSCSI 目标的"删除旧命名目标 / 登记虚拟盘 / 建目标 / 建映射"各需 10~20 秒，
	// 同步执行会把"服务可对外服务"推迟一分钟以上（真实反馈：启动特别慢，且对账预算耗尽、
	// 工作被截断）。对账是**收敛性**的，下面还有每 5 分钟的周期对账接手，
	// 因此延后执行不会丢工作，只是不再占用启动时间。
	reconcileCtx, cancelReconcile := context.WithCancel(context.Background())
	defer cancelReconcile()
	safeGo(log, "startup_reconcile", func() {
		if err := application.Reconcile(reconcileCtx); err != nil {
			log.Error("启动对账失败", "error", err)
		}
	})

	// ---- 11) 任务 worker ----
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	safeGo(log, "job_worker", func() { jobs.Start(workerCtx) })

	// ---- 12) 后台定时任务 ----
	bgCtx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
	// startTicker 用 safeGo 启动周期任务：panic 只记 ERROR（含栈）并终止该 ticker，
	// 绝不拖垮进程（runTicker 内部还会对每次 fn 调用单独兜底）。
	startTicker := func(name string, every time.Duration, fn func(context.Context) error) {
		safeGo(log, "ticker:"+name, func() { runTicker(bgCtx, log, name, every, fn) })
	}
	startTicker("sqlite_maintenance", 10*time.Minute, func(ctx context.Context) error {
		return st.MaintenanceSQLite(ctx)
	})
	startTicker("lease_reaper", time.Minute, func(ctx context.Context) error {
		n, err := application.Leases().ReapExpired(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			log.Info("已回收过期租约", "count", n)
		}
		return nil
	})
	startTicker("audit_purge", time.Hour, func(ctx context.Context) error {
		before := time.Now().AddDate(0, 0, -defaultAuditRetentionDays).UnixMilli()
		n, err := st.PurgeAuditBefore(ctx, before)
		if err != nil {
			return err
		}
		if n > 0 {
			log.Info("已清理过期审计记录", "count", n)
		}
		return nil
	})
	startTicker("session_cleanup", 10*time.Minute, func(_ context.Context) error {
		application.Auth().Cleanup()
		return nil
	})
	startTicker("disk_usage_sample", 5*time.Minute, func(ctx context.Context) error {
		return application.Disks().SamplePhysicalUsage(ctx)
	})
	// 定时对账：每 5 分钟一次增量对账（见 docs/implementation.md 5.11）。
	startTicker("reconcile", 5*time.Minute, func(ctx context.Context) error {
		return application.Reconcile(ctx)
	})
	// 上传暂存 GC：清理超过 24h 未完成的上传会话与暂存目录（见 5.10 步骤 ⑥）。
	startTicker("upload_gc", 30*time.Minute, func(ctx context.Context) error {
		n, err := application.Uploads().GCStaleUploads(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			log.Info("已清理过期上传会话", "count", n)
		}
		return nil
	})

	// ---- 13) HTTP 服务 ----
	jobs.OnFinished(func(j *domain.Job) {
		events.Publish(api.Event{Type: "job", Data: map[string]any{
			"job_id":   j.ID,
			"type":     string(j.Type),
			"ref_id":   j.RefID,
			"state":    string(j.State),
			"progress": j.Progress,
		}})
	})

	// 建盘任务重试用尽后必须把磁盘从 creating 回滚为 error：
	// 否则磁盘永远停在中间态，挂载/发布每次都报 disk.not_ready 且不会自愈。
	// 各建盘任务的 ref_id 都是磁盘 ID（见 app/repo.go 的 EnqueueWith 调用）。
	jobs.OnFinalFailure(func(ctx context.Context, j *domain.Job, cause error) {
		if err := application.Disks().MarkCreateFailed(ctx, j.RefID, cause); err != nil {
			log.Warn("回滚磁盘状态失败", "job_id", j.ID, "ref_id", j.RefID, "error", err)
		}
		// 建库预创建（JobPrepareRepo）失败时，除了上面把母盘回滚为 error，
		// 还必须把**存储库**从 creating 回滚为 error：库停在 creating 的表现是
		// 前端永远显示"创建中"、既分配不了也挂载不了，而错误码没有落到任何地方。
		// 库 ID 在 payload 里（ref_id 是母盘 ID），解析交给 app 层。
		if j != nil && j.Type == domain.JobPrepareRepo {
			if err := application.Repos().MarkPrepareFailedByJob(ctx, j.Payload, cause); err != nil {
				log.Warn("回滚存储库状态失败", "job_id", j.ID, "error", err)
			}
		}
	})

	// 任务 handler panic：除日志（worker 内已写带栈 ERROR）外再落一条审计，
	// 否则审计页只看到"任务执行发生内部错误"的结果、看不到原因，无法排障。
	jobs.OnPanic(func(jobCtx context.Context, j *domain.Job, panicValue any, stack string) {
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(jobCtx), 5*time.Second)
		defer cancel()
		application.Audit(auditCtx, "", "system.panic", "job:"+j.ID,
			"type="+string(j.Type)+" "+job.PanicSummary(panicValue, stack), domain.AuditResultError)
	})

	// Logs 交给管理端「服务日志」页读取：读的是服务端自己的按天日志文件
	// （客户端日志是本机代理的事，走 GET /agent/log，两者互不相干）。
	router := api.New(api.Deps{
		App: application, Log: log, Cfg: watcher, Store: st, Events: events, Logs: logger,
	})
	server := &http.Server{
		Addr:              raw.HTTP.Listen,
		Handler:           router,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	serveErr := make(chan error, 1)
	safeGo(log, "http_serve", func() {
		if raw.HTTP.TLS.Enabled {
			// 证书由服务端自建 CA **自动签发**：不存在则创建，已存在则复用，
			// 若 SAN 主机集合发生变化（换 IP/域名）则自动重签。
			// 因此本服务不依赖任何外部证书文件，开箱即可用 TLS。
			certPEM, keyPEM, err := ca.EnsureServerCert(serverHosts(raw))
			if err != nil {
				serveErr <- err
				return
			}
			// 把证书导出到配置指定路径（相对路径基于配置文件所在目录解析），
			// 便于运维查看/备份/交给前置代理使用；仅内容有变化时才写盘。
			exportCertFiles(raw, absConfig, certPEM, keyPEM, log)

			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				serveErr <- err
				return
			}
			server.TLSConfig = &tls.Config{
				Certificates: []tls.Certificate{pair},
				// 请求客户端证书但不强制：管理页用会话令牌，客户端用 mTLS。
				ClientAuth: tls.RequestClientCert,
				MinVersion: tls.VersionTLS12,
			}
			log.Info("HTTP 服务已启动（TLS）", "listen", raw.HTTP.Listen,
				"ca_dir", caDir, "cert_export", raw.HTTP.TLS.CertFile)
			serveErr <- server.ListenAndServeTLS("", "")
			return
		}
		log.Warn("HTTP 未启用 TLS：管理接口将以明文暴露，仅建议在受控内网使用", "listen", raw.HTTP.Listen)
		log.Info("HTTP 服务已启动（明文）", "listen", raw.HTTP.Listen)
		serveErr <- server.ListenAndServe()
	})

	// 给"双击运行"的场景一个明确的可见反馈：
	// 日志虽同时写固定文件与 stdout，但后台派生进程没有控制台（stdout 被接到空设备），
	// 用户仍看不到任何输出。这里直接往 stdout 打一段横幅，让人一眼确认服务已起来。
	printStartupBanner(raw, *configPath)
	// 双击启动（独占一个控制台）时补一句提示：崩溃后 Windows 会立刻关掉这个控制台，
	// 现场会随窗口一起消失，因此明确告知"改用命令行或 --background"。
	if ownsOwnConsole() {
		fmt.Println("提示：检测到你是直接双击启动的，本窗口一旦关闭/崩溃，控制台输出会随之消失；")
		fmt.Println("      排障建议在命令行（PowerShell/cmd）中运行，或用 --background 后台运行。")
	}

	// ---- 14) 优雅停机 ----
	//
	// 两条触发路径最终都收敛到同一个 select：Ctrl+C（信号）与 `Vault-Server stop`
	// （跨进程命名事件）。停止事件由本进程创建、由 stop 子命令触发；创建失败
	// （例如权限受限）只降级为"stop 子命令不可用"，不影响正常启动与 Ctrl+C。
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	stopEvent, err := createStopEvent()
	if err != nil {
		log.Warn("创建跨进程停止事件失败，`Vault-Server stop` 将不可用", "error", err)
	}
	defer closeStopEvent(stopEvent)

	stopRequests := make(chan stopRequest, 1)
	safeGo(log, "stop_waiter", func() {
		select {
		case sig := <-signals:
			stopRequests <- stopRequest{source: stopSourceSignal, signal: sig.String()}
		case <-waitStopEvent(stopEvent):
			stopRequests <- stopRequest{source: stopSourceCommand}
		}
	})

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP 服务异常退出", "error", err)
			return 1
		}
	case req := <-stopRequests:
		if req.source == stopSourceCommand {
			log.Info("收到停止请求，开始优雅停机", "source", req.source)
		} else {
			log.Info("收到退出信号，开始优雅停机", "source", req.source, "signal", req.signal)
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("HTTP 优雅停机未在超时内完成", "error", err)
	}
	cancelWorker()
	cancelBackground()
	log.Info("服务端已停止")
	return 0
}

// eventSinkAdapter 把 api.EventHub 适配为 app.EventSink。
//
// 之所以需要适配器：`internal/app` 不能 import `internal/api`（会形成循环依赖），
// 因此 app 层只声明 EventSink 接口，由 main 在这里完成桥接。
type eventSinkAdapter struct{ hub *api.EventHub }

// Publish 实现 app.EventSink。
func (a eventSinkAdapter) Publish(topic string, payload any) {
	if a.hub == nil {
		return
	}
	a.hub.Publish(api.Event{Type: topic, Data: payload})
}

// platformDeps 是"按运行平台装配好的四个平台后端"。
//
// 各字段都是平台中立接口；同一份上层代码（app 层）据此驱动全部磁盘/iSCSI 逻辑。
// 装配实现见 platform_windows.go / platform_linux.go（同一函数名的两个构建变体）。
type platformDeps struct {
	// Disk 虚拟磁盘（Windows=VHDX 文件；Linux=LVM thin LV）。
	Disk platform.DiskBackend
	// Vol 卷/文件系统（初始化、格式化、拷入、空间查询）。
	Vol platform.VolumeBackend
	// Iscsi iSCSI 目标（Windows=IscsiTarget 模块；Linux=LIO）。
	Iscsi platform.IscsiBackend
	// Platform 存储池与缓存设备管理（仅 Linux 有实现，Windows 返回 ErrUnsupported）。
	Platform platform.StorageAdmin
	// StorageVol 存储底层卷管理（Linux=thin LV 建/挂/卸/扩/删）。
	//
	// 装配为 nil 表示"平台没有存储卷概念"（Windows）：此时 app 层按**目录模式**处理，
	// 相关 API 返回 platform.unsupported（501）。见 platform.StorageVolumeBackend。
	StorageVol platform.StorageVolumeBackend
}

// checkPlatformKind 校验配置声明的平台种类与当前构建产物是否一致。
//
// 之所以 fail-fast：声明为 windows 却跑在 Linux 上，磁盘布局语义完全不同，
// 静默继续会以错误的引用格式操作磁盘（例如把 LV 名当文件路径），比启动失败危险得多。
func checkPlatformKind(configured, actual string) error {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case "", "auto", actual:
		return nil
	default:
		return fmt.Errorf("配置错误：platform.kind=%q 与当前构建产物（%s）不一致", configured, actual)
	}
}

// dialectOf 把配置中的驱动名映射为 store 方言。
func dialectOf(driver string) (store.Dialect, error) {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "", "sqlite":
		return store.DialectSQLite, nil
	case "mysql":
		return store.DialectMySQL, nil
	default:
		return "", fmt.Errorf("不支持的数据库驱动 %q", driver)
	}
}

// ensureBootstrapValues 把首次启动生成的值写回配置文件。
//
// 覆盖两项：
//   - server.instance_id：缺失时写入（实例 ID 必须跨重启稳定，否则客户端证书的实例绑定会失效）；
//   - security.master_key：本次由程序生成时写入（数据保险箱根密钥，缺失将无法解密已有 CHAP 密钥）。
//
// ⚠️ 该写入会重新序列化整个 YAML 文档，**注释与字段顺序会丢失**；
// 首次启动后按 config.example.yaml 整理注释即可（取值不受影响）。
func ensureBootstrapValues(path, instanceID, masterKey string, wroteMasterKey bool, log *slog.Logger) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg config.Config
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return err
	}

	changed := false
	if strings.TrimSpace(cfg.Server.InstanceID) == "" && instanceID != "" {
		cfg.Server.InstanceID = instanceID
		changed = true
	}
	if wroteMasterKey && strings.TrimSpace(cfg.Security.MasterKey) == "" {
		cfg.Security.MasterKey = masterKey
		changed = true
	}
	if !changed {
		return nil
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return err
	}
	log.Warn("已把首次启动生成的值写回配置文件（重新序列化会丢失注释，请人工整理）", "config", path)
	return nil
}

// serverHosts 汇总服务端证书需要覆盖的 SAN（监听地址 + 本机名 + 本机 IP）。
func serverHosts(raw config.Config) []string {
	hosts := []string{"127.0.0.1", "localhost"}
	listenHost := raw.HTTP.Listen
	if h, _, err := net.SplitHostPort(raw.HTTP.Listen); err == nil {
		listenHost = h
	}
	switch listenHost {
	case "", "0.0.0.0", "::", "[::]":
		if addrs, err := net.InterfaceAddrs(); err == nil {
			added := 0
			for _, addr := range addrs {
				ipNet, ok := addr.(*net.IPNet)
				if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
					continue
				}
				hosts = append(hosts, ipNet.IP.String())
				added++
				if added >= 2 {
					break
				}
			}
		}
	default:
		hosts = append(hosts, listenHost)
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		hosts = append(hosts, name)
	}
	return hosts
}

// exportCertFiles 把自建 CA 签发的服务端证书导出到配置指定路径。
//
// 语义：**服务端始终自行管理证书**（pki/ 下由 CA 自动签发与维护），
// 配置中的 cert_file / key_file 只作为"导出副本"位置，便于运维查看、备份，
// 或交给前置反向代理使用。相对路径基于**配置文件所在目录**解析。
//
// 导出失败只记日志、不影响服务启动——证书的实际使用走内存中的那一份。
func exportCertFiles(raw config.Config, absConfig string, certPEM, keyPEM []byte, log *slog.Logger) {
	baseDir := filepath.Dir(absConfig)

	writeIfChanged := func(name, path string, data []byte, perm os.FileMode) {
		if strings.TrimSpace(path) == "" {
			return
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(baseDir, path)
		}
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
			return // 内容未变，不写盘
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			log.Warn("创建证书导出目录失败", "kind", name, "path", path, "error", err)
			return
		}
		if err := os.WriteFile(path, data, perm); err != nil {
			log.Warn("导出证书失败", "kind", name, "path", path, "error", err)
			return
		}
		log.Info("已导出证书副本", "kind", name, "path", path)
	}

	certPerm := os.FileMode(0o644)
	keyPerm := os.FileMode(0o600)
	writeIfChanged("证书", raw.HTTP.TLS.CertFile, certPEM, certPerm)
	// 私钥文件权限在 Windows 上不生效，部署时需用 ACL 收紧（见 docs/implementation.md 9.2）。
	writeIfChanged("私钥", raw.HTTP.TLS.KeyFile, keyPEM, keyPerm)
}

// runTicker 周期执行后台任务，直到 ctx 结束。
//
// 关键约束：任何一次任务执行的 panic 都**不得**终止本循环或整个进程。
// 后台 goroutine 的 panic 不会被 net/http 的 handler recover 捕获，必须就地兜底。
func runTicker(ctx context.Context, log *slog.Logger, name string, every time.Duration, fn func(context.Context) error) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runTickerOnce(ctx, log, name, fn)
		}
	}
}

// runTickerOnce 执行一次后台任务，捕获并记录 panic 后正常返回（循环继续下一个 tick）。
func runTickerOnce(ctx context.Context, log *slog.Logger, name string, fn func(context.Context) error) {
	defer func() {
		if r := recover(); r != nil {
			// 记完整调用栈——这是排查"进程为何消失"的唯一现场。
			log.Error("后台任务 panic，已捕获并继续运行",
				"task", name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := fn(runCtx); err != nil && !errors.Is(err, context.Canceled) {
		log.Warn("后台任务执行失败", "task", name, "error", err)
	}
}

// safeGo 启动一个后台 goroutine，并统一兜住其 panic。
//
// 语义：panic 时记 ERROR（含任务名、panic 值与完整调用栈）后**仅终止该 goroutine**，
// 进程与其他后台任务继续运行。防止"某个后台任务 panic → 整个服务端静默退出"。
func safeGo(log *slog.Logger, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("后台 goroutine panic，已捕获并继续运行",
					"task", name, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// diffConfig 比较两份配置的**生效项**，返回人类可读的变更列表。
//
// 只覆盖运维常改且影响运行行为的项，避免把所有字段都做反射比较。
func diffConfig(prev, next config.Config) []string {
	var changes []string
	add := func(name, before, after string) {
		if before != after {
			changes = append(changes, name+": "+before+" -> "+after)
		}
	}
	add("server.name", prev.Server.Name, next.Server.Name)
	add("server.instance_id", prev.Server.InstanceID, next.Server.InstanceID)
	add("http.listen", prev.HTTP.Listen, next.HTTP.Listen)
	add("http.tls.enabled", boolString(prev.HTTP.TLS.Enabled), boolString(next.HTTP.TLS.Enabled))
	add("database.driver", prev.Database.Driver, next.Database.Driver)
	add("storage.whitelist_roots",
		strings.Join(prev.Storage.EffectiveRoots(), "; "), strings.Join(next.Storage.EffectiveRoots(), "; "))
	add("storage.disks_dir", prev.Storage.DisksDir, next.Storage.DisksDir)
	add("storage.staging_dir", prev.Storage.StagingDir, next.Storage.StagingDir)
	add("storage.default_max_diff_disks",
		intString(prev.Storage.DefaultMaxDiffDisks), intString(next.Storage.DefaultMaxDiffDisks))
	add("storage.min_volume_free_permille",
		int64String(prev.Storage.MinVolumeFreePermille), int64String(next.Storage.MinVolumeFreePermille))
	add("log.level", prev.Log.Level, next.Log.Level)
	add("log.dir", prev.Log.Dir, next.Log.Dir)
	add("security.session_ttl",
		prev.Security.SessionTTL.Std().String(), next.Security.SessionTTL.Std().String())
	add("security.lease_ttl",
		prev.Security.LeaseTTL.Std().String(), next.Security.LeaseTTL.Std().String())
	add("security.lease_heartbeat_interval",
		prev.Security.LeaseHeartbeatInterval.Std().String(), next.Security.LeaseHeartbeatInterval.Std().String())
	add("security.revoke_cooldown",
		prev.Security.RevokeCooldown.Std().String(), next.Security.RevokeCooldown.Std().String())
	add("security.bootstrap_enabled",
		nullableBoolString(prev.Security.BootstrapEnabled), nullableBoolString(next.Security.BootstrapEnabled))
	add("security.super_admin_enabled",
		nullableBoolString(prev.Security.SuperAdminEnabled), nullableBoolString(next.Security.SuperAdminEnabled))
	add("security.bootstrap_window",
		prev.Security.BootstrapWindow.Std().String(), next.Security.BootstrapWindow.Std().String())
	add("client_compat", compatString(prev), compatString(next))
	return changes
}

// nullableBoolString 把可空布尔开关渲染为可读文本（nil 视为启用）。
func nullableBoolString(v *bool) string {
	if v == nil {
		return "true(默认)"
	}
	if *v {
		return "true"
	}
	return "false"
}

// compatString 输出客户端兼容区间的可读描述（只读展示，见 3.4.2）。
func compatString(c config.Config) string {
	enabled := c.ClientCompat.Enabled == nil || *c.ClientCompat.Enabled
	if !enabled {
		return "disabled"
	}
	part := func(p *string) string {
		if p == nil {
			return "derived"
		}
		if strings.TrimSpace(*p) == "" {
			return "unbounded"
		}
		return strings.TrimSpace(*p)
	}
	return "[" + part(c.ClientCompat.Min) + ", " + part(c.ClientCompat.Max) + "]"
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func setString(configured bool) string {
	if configured {
		return "configured"
	}
	return "empty"
}

func intString(v int) string { return fmt.Sprintf("%d", v) }

func int64String(v int64) string { return fmt.Sprintf("%d", v) }

// ---- sign 子命令（发布方签名工具） ----

// runSign 派发签名工具的子命令（keygen / release / verify）。
//
// ⚠️ 服务端运行时永不签名：该路径只由发布方在构建/发布阶段手工调用，
// 仅 release 子命令会读取发布方私钥；serve 路径绝不加载私钥。
func runSign(args []string) int {
	if len(args) == 0 {
		signUsage(os.Stderr)
		return 2
	}
	stdout, stderr := os.Stdout, os.Stderr
	switch args[0] {
	case "keygen":
		return signExitCode(signtool.RunKeygen(args[1:], stdout, stderr))
	case "release":
		return signExitCode(signtool.RunRelease(args[1:], stdout, stderr))
	case "verify":
		return signExitCode(signtool.RunVerify(args[1:], stdout, stderr))
	case "-h", "--help", "help":
		signUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "未知子命令: %q\n\n", args[0])
		signUsage(stderr)
		return 2
	}
}

// signExitCode 把签名工具返回的错误映射为进程退出码。
func signExitCode(err error) int {
	if err != nil {
		return 1
	}
	return 0
}

// signUsage 打印签名工具的用法。
func signUsage(w io.Writer) {
	fmt.Fprint(w, `Vault-Server sign —— Vault 客户端更新包签名工具（仅供发布方使用）

用法:
  Vault-Server sign keygen  -out-dir ./keys [-force]
  Vault-Server sign release -dir ./release -channel stable -version 0.2.0 \
                            -notes "更新说明" [-min-supported-version 0.1.0] \
                            -key ./keys/update-private.pem \
                            -artifact agent=./dist/Vault-Agent.exe \
                            [-artifact client=./dist/vault-client-setup.exe]
  Vault-Server sign verify  -dir ./release -pub ./keys/update-public.txt

说明:
  - 发布目录（-dir）产出 manifest.json、manifest.json.sig、artifacts/<filename>；
  - 发布目录可直接作为服务端 update.artifacts_dir（服务端只做 mirror，不解析、不改写、不签名）；
  - 客户端内置的公钥必须与签名私钥配对（构建期用 -X 注入）；
  - 服务端运行时永不签名，私钥不参与任何 serve 路径。
`)
}

// printUsage 打印 Vault-Server 的完整用法（列出 serve（默认）/ stop / sign 三种用法）。
func printUsage(w io.Writer, fs *flag.FlagSet) {
	fs.SetOutput(w)
	fmt.Fprint(w, `Vault-Server —— Vault 服务端（Windows / Linux）

用法:
  Vault-Server [选项]           启动服务端（默认 serve，前台运行）
  Vault-Server --background     派生后台子进程运行服务端
  Vault-Server stop [选项]      向正在运行的服务端发送停止请求（优雅停机）
  Vault-Server sign <子命令>    发布方签名工具（构建/发布阶段使用）

serve 选项:
`)
	fs.PrintDefaults()
	fmt.Fprint(w, `
stop 选项:
  Vault-Server stop [-config <配置文件路径>]
    Windows：通过全局命名事件通知运行中的服务端优雅停机（等价于 Ctrl+C）。
    Linux  ：读取 PID 文件并向其发送 SIGTERM（同样是优雅停机）。
    配置文件仅用于确认监听地址以便确认停止结果；默认 config.yaml。

sign 子命令:
  Vault-Server sign keygen  -out-dir ./keys [-force]
  Vault-Server sign release -dir ./release -channel stable -version 0.2.0 \
                            -key ./keys/update-private.pem -artifact agent=./dist/Vault-Agent.exe
  Vault-Server sign verify  -dir ./release -pub ./keys/update-public.txt

提示:
  - 签名工具的完整用法见 "Vault-Server sign -h"；服务端运行时永不签名。
  - **默认前台运行**（含 Windows 下双击 Vault-Server.exe）：进程就跑在当前控制台里，
    日志、横幅与 panic 栈直接可见，按 Ctrl+C 停止，不会派生任何后台进程。
    排障请用这种方式——后台模式下崩溃是看不到任何提示的。
  - **加 --background 才是后台运行**：探测到服务端未运行时派生一个脱离本窗口的
    子进程，就绪后打印摘要即可关闭本窗口（服务继续运行）；已在运行则提示且不重复启动。
    后台子进程的 stdout/stderr 会写入 <配置目录>/logs/vault-server.background.log。
  - -detached 为内部标记（仅后台启动器派生的子进程使用），通常无需手工指定。
`)
}
