//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"vault/internal/config"
	"vault/internal/platform/sysdeps"
)

// doctorUsage 是 `Vault-Server doctor` 的用法说明（由 printUsage 追加输出）。
const doctorUsage = `
doctor 子命令（仅 Linux）:
  Vault-Server doctor [-config <配置文件路径>] [-fix=true] [-install=true] [-json]

  探测系统依赖并（默认）自动修复。部署完新机器先跑一次，排障时也跑它：

    1. 是否以 root 运行（挂载、modprobe、装包都需要）；
    2. configfs 是否已挂载在 <configfs_root> 的父目录上；
    3. LIO 三个内核模块是否已加载：target_core_mod / iscsi_target_mod / target_core_iblock；
    4. LIO 执行体 targetcli 是否能跑通（targetcli version）—— 这是 iSCSI 就绪的**行为判据**，
       刻意不判 <root>/iscsi、<root>/core/iblock_0 这类目录：它们由 rtslib 按需创建
       （core/iblock_0 要等第一个 iblock backstore 建出来才出现），当判据会把好机器判成故障；
    5. 其余命令行工具是否在 PATH：LVM2、文件系统工具等。

    自动修复（-fix=true，默认）：mount -t configfs、modprobe 缺失的内核模块、
    写 /etc/modules-load.d/vault-lio.conf；动作幂等，重复执行安全。
    刻意**不**重载已加载的模块：那会拆掉正在给客户端服务的整棵 iSCSI 目标配置。
  自动装包（-install=true，默认）：按发行版调用 apt-get/dnf/yum/zypper/apk/pacman
  安装缺失工具对应的包；离线环境请用 -install=false，报告里会给出人工安装命令。

  退出码：0 = 必需项全部就绪；1 = 仍缺必需项（逐项看「处置」）；2 = 参数错误。
`

// runDoctor 实现 `Vault-Server doctor`。
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("Vault-Server "+doctorSubcommand, flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "配置文件路径（只用于读取 platform.iscsi.configfs_root）")
	fix := fs.Bool("fix", true, "自动修复可修复的缺项（挂载 configfs、加载缺失的内核模块、写 modules-load.d）")
	install := fs.Bool("install", true, "允许自动安装缺失命令行工具对应的软件包（apt-get/dnf/yum/zypper/apk/pacman）")
	asJSON := fs.Bool("json", false, "以 JSON 输出报告（便于脚本/CMDB 消费）")
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), doctorUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// 配置读不到也照样能跑：doctor 的用途之一是"机器还没配好时先看看缺什么"。
	root := ""
	if loaded, err := config.Load(*configPath); err == nil {
		root = loaded.Raw.Platform.Iscsi.ConfigFSRoot
	} else {
		fmt.Fprintf(os.Stderr, "提示：读取配置 %s 失败（%v）；按默认 configfs_root=%s 探测\n",
			*configPath, err, "/sys/kernel/config/target")
	}

	// 修复命令的原始输出走 stderr（Info 级），报告本体走 stdout，方便 `-json > f.json`。
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	report := sysdeps.Ensure(context.Background(), sysdeps.Options{
		Logger:       logger,
		ConfigFSRoot: root,
		Repair:       *fix,
		Install:      *install,
	})

	if *asJSON {
		buf, err := report.JSON()
		if err != nil {
			fmt.Fprintln(os.Stderr, "生成 JSON 报告失败:", err)
			return 1
		}
		fmt.Println(string(buf))
	} else {
		report.Render(os.Stdout)
	}
	if report.OK() {
		return 0
	}
	return 1
}
