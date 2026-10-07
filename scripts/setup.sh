#!/bin/sh
# Vault 服务端 Linux 依赖一键安装/修复脚本
# ============================================================
# 解决什么问题
# ------------
# `vault-server doctor`（以及服务端启动时的自动修复）只能解决"宿主机本来就有、
# 只是没生效"的依赖：挂载 configfs、modprobe 已存在的模块、按发行版装几个命令行工具。
# 下面三种它**修不了**，因为在它动手之前依赖就不存在：
#
#   ① 内核没提供 LIO 模块文件 —— Debian/Ubuntu 的 target_core_* / iscsi_target_mod 在
#      `linux-modules-extra-$(uname -r)` 里，RHEL 系在 `kernel-modules-extra` 里，
#      Alpine 在 `linux-lts` 之类内核包里。缺包时 modprobe 只报 Module not found，
#      而 doctor 的自动装包只装"命令行工具对应的包"，不会去装内核模块包。
#   ② 连包管理器/基础工具都没有 —— 自动装包自身无从下手（最小化镜像、离线系统）。
#   ③ configfs 从未挂载过且服务端还没跑起来 —— 部署新机器时的"先有鸡还是先有蛋"。
#
# 所以本脚本是**部署期兜底**：动作与 doctor 一致且幂等，重复执行安全。
#
# 用法
# ----
#   sudo ./scripts/setup.sh                  # 装包 + 加载模块 + 挂 configfs + 复检
#   sudo ./scripts/setup.sh --no-install     # 不装任何软件包（离线/由 Ansible 接管时）
#   sudo ./scripts/setup.sh --dry-run        # 只打印将执行的命令，不改动系统
#   sudo ./scripts/setup.sh --fstab          # 额外把 configfs 写进 /etc/fstab（开机自动挂载）
#   sudo ./scripts/setup.sh --extra          # 额外装可选工具（mdadm/nvme-cli/smartmontools/sg3-utils）
#   sudo ./scripts/setup.sh --no-update      # 跳过 apt-get update（内网源已就绪时更快）
#   sudo ./scripts/setup.sh -h               # 帮助
#
# 退出码：0 = 必需项全部就绪；1 = 仍缺必需项（见末尾「仍未就绪」与「诊断」）；2 = 用法错误
#
# 装完请再跑一次服务端自带自检（给出与前端横幅一致的逐项报告）：./vault-server doctor
#
# 兼容性：POSIX sh（bash/dash/ash/busybox 均可），支持 apt/dnf/yum/zypper/apk/pacman。

set -u

# ---- 与 internal/platform/sysdeps 保持一致的常量（改这里必须同步改 Go 侧）----
MODULES="target_core_mod iscsi_target_mod target_core_iblock"
# 本脚本只做 modprobe（对已加载的模块是空操作），**不做** modprobe -r 重载：
# 重载会拆掉正在给客户端服务的整棵目标配置，而它能修的"configfs 组不见了"并不是就绪判据
# （见 4.5 节）。真需要重载时人工按逆序执行：
#   modprobe -r iscsi_target_mod target_core_iblock && modprobe target_core_mod iscsi_target_mod target_core_iblock
MODULES_LOAD_FILE=/etc/modules-load.d/vault-lio.conf
MODULES_LOAD_HEADER='# 由 vault-server 生成：LIO（iSCSI target）所需内核模块，请勿删改本文件。'
CONFIGFS_DIR=/sys/kernel/config
CONFIGFS_TARGET="$CONFIGFS_DIR/target"

DO_INSTALL=1
DO_UPDATE=1
DO_FSTAB=0
DO_EXTRA=0
DRY_RUN=0

# ---- 输出 ----
BOLD=''; RED=''; YEL=''; GRN=''; DIM=''; RST=''
if [ -t 1 ]; then
    BOLD=$(printf '\033[1m'); RED=$(printf '\033[31m'); YEL=$(printf '\033[33m')
    GRN=$(printf '\033[32m'); DIM=$(printf '\033[2m'); RST=$(printf '\033[0m')
fi

step() { printf '\n%s==> %s%s\n' "$BOLD" "$*" "$RST"; }
info() { printf '    %s\n' "$*"; }
dim()  { printf '    %s%s%s\n' "$DIM" "$*" "$RST"; }
ok()   { printf '    %s[ok]%s %s\n' "$GRN" "$RST" "$*"; }
warn() { printf '    %s[警告]%s %s\n' "$YEL" "$RST" "$*"; }
err()  { printf '    %s[失败]%s %s\n' "$RED" "$RST" "$*" >&2; }

usage() { sed -n '2,33p' "$0" | sed 's/^# \{0,1\}//'; }

# ---- 提权 ----
# 先粗扫一遍参数：--dry-run / -h 决定"要不要 sudo 提权"，必须在提权之前就知道；
# 而提权又要保留全部参数，所以放在正式解析之前。
# 非 root 是"执行了还是不可用"的头号原因：modprobe / mount / 装包都会被拒。
for a in "$@"; do
    case "$a" in
        --dry-run) DRY_RUN=1 ;;
        -h|--help) usage; exit 0 ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    if [ "$DRY_RUN" = 1 ]; then
        # 预览不需要特权：探测类判断（command -v、/proc/mounts、modinfo）非 root 也能做。
        warn "以非 root 运行：仅预览（--dry-run 不会修改系统）"
    elif command -v sudo >/dev/null 2>&1 && [ "${VAULT_SETUP_SUDO:-}" != 1 ]; then
        info "需要 root 权限，改用 sudo 重新执行……"
        VAULT_SETUP_SUDO=1 exec sudo -E "$0" "$@"
    else
        err "请以 root 运行（LVM、configfs、modprobe、装包都需要 root）：sudo $0"
        exit 1
    fi
fi

# ---- 参数 ----
while [ $# -gt 0 ]; do
    case "$1" in
        --no-install) DO_INSTALL=0 ;;
        --no-update)  DO_UPDATE=0 ;;
        --fstab)      DO_FSTAB=1 ;;
        --extra)      DO_EXTRA=1 ;;
        --dry-run)    DRY_RUN=1 ;;
        -h|--help)    usage; exit 0 ;;
        *)            err "未知参数：$1（用 -h 查看用法）"; exit 2 ;;
    esac
    shift
done

# ---- 命令执行封装（--dry-run 时只打印）----
run() {
    if [ "$DRY_RUN" = 1 ]; then
        printf '    %s[dry-run]%s %s\n' "$DIM" "$RST" "$*"
        return 0
    fi
    "$@"
}

# run_capture 执行并捕获合并输出到 OUT（失败时便于把原因写进报告）。
OUT=''
run_capture() {
    if [ "$DRY_RUN" = 1 ]; then
        printf '    %s[dry-run]%s %s\n' "$DIM" "$RST" "$*"
        OUT=''
        return 0
    fi
    OUT=$("$@" 2>&1)
    return $?
}

# uniq_words <列表字符串>：按空白拆分、去重后以单空格连接（顺带去掉首尾空白）。
# 报告里要能直接把缺项列表贴给别人看，所以不留多余空格。
uniq_words() {
    # shellcheck disable=SC2086 # 刻意依赖默认 IFS 做词拆分
    set -- $1
    out=''
    for w in "$@"; do
        case " $out " in
            *" $w "*) ;;
            *) out="${out:+$out }$w" ;;
        esac
    done
    printf '%s' "$out"
}

# ============================================================
# 1) 发行版与包管理器
# ============================================================
step "识别发行版与包管理器"

PM_FAMILY=''; PM_INSTALL=''; PM_ENV=''
if command -v apt-get >/dev/null 2>&1; then
    PM_FAMILY=apt; PM_INSTALL='apt-get install -y --no-install-recommends'
    PM_ENV='DEBIAN_FRONTEND=noninteractive'
elif command -v dnf >/dev/null 2>&1; then
    PM_FAMILY=rpm; PM_INSTALL='dnf install -y'
elif command -v yum >/dev/null 2>&1; then
    PM_FAMILY=rpm; PM_INSTALL='yum install -y'
elif command -v zypper >/dev/null 2>&1; then
    PM_FAMILY=rpm; PM_INSTALL='zypper --non-interactive install --no-recommends'
elif command -v apk >/dev/null 2>&1; then
    PM_FAMILY=apk; PM_INSTALL='apk add --no-cache'
elif command -v pacman >/dev/null 2>&1; then
    PM_FAMILY=pacman; PM_INSTALL='pacman -S --noconfirm --needed'
fi

if [ -n "$PM_FAMILY" ]; then
    DISTRO=$( ( . /etc/os-release 2>/dev/null && printf '%s %s' "${NAME:-}" "${VERSION_ID:-}" ) || printf '未知发行版' )
    ok "发行版：$DISTRO（包管理器族 $PM_FAMILY）"
else
    warn "未找到受支持的包管理器（apt-get/dnf/yum/zypper/apk/pacman）：跳过装包，只做模块与 configfs"
    DO_INSTALL=0
fi

# 包名映射（按族）。REQUIRED 缺失会让服务端功能不可用；OPTIONAL 只在特定场景用得到。
#
# targetcli 属 REQUIRED：它是 iSCSI 目标的**执行体**（写操作全部经它下发到 configfs），
# 不是可有可无的便利包装，缺了它 iSCSI 目标功能完全不可用。
# 包名跨发行版不同：Debian/Ubuntu 为 targetcli-fb，RHEL/Fedora/SUSE 为 targetcli。
case "$PM_FAMILY" in
    apt)
        PKG_REQUIRED='kmod lvm2 util-linux e2fsprogs parted targetcli-fb'
        PKG_OPTIONAL='xfsprogs thin-provisioning-tools ntfs-3g'
        PKG_EXTRA='mdadm nvme-cli smartmontools sg3-utils' ;;
    rpm)
        # thin 元数据工具：RHEL 系叫 device-mapper-persistent-data，SUSE 叫 thin-provisioning-tools。
        # 两个都列上，装不上的那个由"逐个重试"逻辑忽略。
        PKG_REQUIRED='kmod lvm2 util-linux e2fsprogs parted targetcli'
        PKG_OPTIONAL='xfsprogs device-mapper-persistent-data thin-provisioning-tools ntfs-3g'
        PKG_EXTRA='mdadm nvme-cli smartmontools sg3_utils' ;;
    apk)
        PKG_REQUIRED='kmod lvm2 util-linux e2fsprogs parted targetcli'
        PKG_OPTIONAL='xfsprogs thin-provisioning-tools ntfs-3g'
        PKG_EXTRA='mdadm nvme-cli smartmontools sg3-utils' ;;
    pacman)
        PKG_REQUIRED='kmod lvm2 util-linux e2fsprogs parted targetcli'
        PKG_OPTIONAL='xfsprogs thin-provisioning-tools ntfs-3g'
        PKG_EXTRA='mdadm nvme-cli smartmontools sg3_utils' ;;
    *)
        PKG_REQUIRED=''; PKG_OPTIONAL=''; PKG_EXTRA='' ;;
esac

# pm_install 执行装包命令（按族拼好环境变量与参数，参数以列表传递，绝不拼 shell 字符串）。
pm_install() {
    if [ -n "$PM_ENV" ]; then
        # shellcheck disable=SC2086 # PM_INSTALL 是刻意做词拆分的命令前缀
        run env "$PM_ENV" $PM_INSTALL "$@"
    else
        # shellcheck disable=SC2086
        run $PM_INSTALL "$@"
    fi
}

# install_pkgs <描述> <包...>：先整批装（最快），失败再逐个装。
# 逐个重试是必要的：不同发行版/版本的包名并不一致（例如 thin 工具、sg3_utils），
# 一个包名不存在不该让整批安装失败、连累其它必需的包。
LAST_FAILED=''
install_pkgs() {
    label=$1; shift
    [ $# -eq 0 ] && return 0
    if [ "$DO_INSTALL" != 1 ]; then
        dim "跳过${label}包安装（--no-install）：$*"
        return 0
    fi
    info "安装${label}软件包：$*"
    if pm_install "$@"; then
        ok "已安装：$*"
        return 0
    fi
    warn "整批安装失败，改为逐个安装（包名在各发行版间并不统一）"
    # 累积到 LAST_FAILED（而不是每次覆盖）：末尾报告要能看到**所有**调用里装不上的包。
    before=$LAST_FAILED
    for p in "$@"; do
        if pm_install "$p" >/dev/null 2>&1; then
            ok "已安装 $p"
        else
            warn "装不上 $p（继续，稍后由复检判定影响）"
            LAST_FAILED="${LAST_FAILED:+$LAST_FAILED }$p"
        fi
    done
    [ "$LAST_FAILED" = "$before" ]
}

if [ "$PM_FAMILY" = apt ] && [ "$DO_INSTALL" = 1 ] && [ "$DO_UPDATE" = 1 ]; then
    step "刷新软件源索引（apt-get update）"
    if run env DEBIAN_FRONTEND=noninteractive apt-get update -qq; then
        ok "软件源索引已刷新"
    else
        warn "apt-get update 失败（内网源不可达？）——继续尝试安装；也可用 --no-update 跳过"
    fi
fi

if [ -n "$PKG_REQUIRED" ]; then
    step "安装必需软件包（targetcli / LVM2 / util-linux / kmod / 文件系统工具）"
    install_pkgs "必需" $PKG_REQUIRED || warn "有必需包未装上，见上面逐条警告"
fi

if [ -n "$PKG_OPTIONAL" ]; then
    step "安装推荐软件包（XFS / thin 元数据 / NTFS）"
    install_pkgs "推荐" $PKG_OPTIONAL || dim "部分推荐包装不上：不影响默认 ext4 + thin 主流程"
fi

if [ "$DO_EXTRA" = 1 ]; then
    step "安装可选工具（--extra：RAID 释放、SMART、NVMe 识别）"
    install_pkgs "可选" $PKG_EXTRA || dim "部分可选工具装不上：不影响主流程"
fi

# ============================================================
# 2) LIO 内核模块
# ============================================================
# module_file_missing <模块>：判断内核有没有这个模块的 .ko 文件。
# 已加载的模块即使文件不可见（容器里看不到宿主 /lib/modules）也算通过。
module_file_missing() {
    grep -q "^$1 " /proc/modules 2>/dev/null && return 1
    if command -v modinfo >/dev/null 2>&1 && modinfo -n "$1" >/dev/null 2>&1; then
        return 1
    fi
    return 0
}

# install_kernel_modules_pkg：按发行版补装"带 LIO 模块的内核模块包"。
# 这是本脚本存在的主要理由：doctor 不会替你去装内核模块包。
install_kernel_modules_pkg() {
    case "$PM_FAMILY" in
        apt)
            # Debian/Ubuntu：模块文件在 linux-modules-extra-<内核版本> 里（版本必须与运行内核一致）
            install_pkgs "内核模块" "linux-modules-extra-$(uname -r)" ;;
        rpm)
            # RHEL/Fedora：先试与运行内核同版本的，再试不带版本的同名包
            install_pkgs "内核模块" "kernel-modules-extra-$(uname -r)" ||
                install_pkgs "内核模块" kernel-modules-extra ;;
        apk)
            case "$(uname -r)" in
                *-lts)  kp=linux-lts ;;
                *-virt) kp=linux-virt ;;
                *)      kp=linux-lts ;;
            esac
            install_pkgs "内核模块" "$kp" ;;
        pacman)
            # Arch 系的 linux/linux-lts 包本身就带全部模块：缺模块基本是"升级过内核没重启"
            warn "Arch 系：请确认已安装与运行内核匹配的 linux/linux-lts 包，并重启到该内核"
            return 1 ;;
        *)
            return 1 ;;
    esac
}

step "检查 LIO 内核模块：$MODULES"
KREL=$(uname -r)
KERNEL_MISSING=''
for m in $MODULES; do
    if module_file_missing "$m"; then
        KERNEL_MISSING="${KERNEL_MISSING:+$KERNEL_MISSING }$m"
    fi
done

if [ -n "$KERNEL_MISSING" ]; then
    warn "内核未提供这些模块的模块文件：$(uniq_words "$KERNEL_MISSING")（modprobe 必然报 Module not found）"
    if [ ! -d "/lib/modules/$KREL" ]; then
        warn "/lib/modules/$KREL 不存在：运行内核与已安装模块不匹配（升级过内核但没重启？）"
    fi
    info "尝试安装发行版的内核模块包……"
    install_kernel_modules_pkg || dim "未能自动装到内核模块包，见末尾「诊断」"
    if command -v depmod >/dev/null 2>&1; then
        if run depmod -a; then
            [ "$DRY_RUN" = 1 ] || dim "已执行 depmod -a 刷新模块依赖"
        fi
    fi
fi

# ---- 加载/卸载封装：必须"一个模块一次 modprobe" ----
# `modprobe $MODULES` 是**致命陷阱**：modprobe 只把第 1 个参数当模块名，后面的全被当作
# **该模块的参数**传给内核，于是只加载了第一个模块，其余"静默消失"，症状是
#   dmesg: target_core_mod: unknown parameter 'iscsi_target_mod' ignored
#   /proc/modules 里只有 target_core_mod，configfs 里只有 target/core、没有 target/iscsi
# ——症状（"模块明明加载了却没有 configfs 组"）和原因完全不沾边，所以这里写死注释。
modprobe_each() {
    rc=0
    for m in "$@"; do
        if [ "$DRY_RUN" = 0 ] && grep -q "^$m " /proc/modules 2>/dev/null; then
            ok "$m 已加载"
            continue
        fi
        if run_capture modprobe "$m"; then
            [ "$DRY_RUN" = 1 ] || ok "已加载 $m"
        else
            warn "modprobe $m 失败：$OUT"
            rc=1
        fi
    done
    return $rc
}

MODPROBE_FAIL=''
if [ "$DRY_RUN" = 1 ]; then
    modprobe_each $MODULES || true   # 预览：逐个打印将要执行的 modprobe，不判定失败
elif ! modprobe_each $MODULES; then
    # 失败清单以 /proc/modules 的实际情况为准，而不是 modprobe 的返回码：
    # 模块可能已被别的进程/依赖顺手加载（modprobe 报错但结果是好的）。
    for m in $MODULES; do
        grep -q "^$m " /proc/modules 2>/dev/null || MODPROBE_FAIL="${MODPROBE_FAIL:+$MODPROBE_FAIL }$m"
    done
fi

# 3) 开机自动加载（内容与 sysdeps.ensureModulesLoadFile 完全一致，避免两边互相覆盖）
step "写入开机自动加载配置：$MODULES_LOAD_FILE"
need_write=0
if [ -f "$MODULES_LOAD_FILE" ]; then
    for m in $MODULES; do
        grep -qx "$m" "$MODULES_LOAD_FILE" 2>/dev/null || need_write=1
    done
else
    need_write=1
fi
if [ "$need_write" = 0 ]; then
    ok "已列出全部模块，保持不动"
elif [ "$DRY_RUN" = 1 ]; then
    dim "[dry-run] 写 $MODULES_LOAD_FILE"
else
    mkdir -p /etc/modules-load.d &&
        { printf '%s\n' "$MODULES_LOAD_HEADER"; for m in $MODULES; do printf '%s\n' "$m"; done; } >"$MODULES_LOAD_FILE" &&
        ok "已写入（重启后仍会自动加载）" || warn "写入失败：重启后需要手工 modprobe"
fi

# ============================================================
# 4) configfs
# ============================================================
step "挂载 configfs 到 $CONFIGFS_DIR"
if [ "$DRY_RUN" = 0 ] && grep -qs " $CONFIGFS_DIR configfs " /proc/mounts; then
    ok "configfs 已挂载"
else
    mkdir -p "$CONFIGFS_DIR" 2>/dev/null || true
    if run_capture mount -t configfs none "$CONFIGFS_DIR"; then
        [ "$DRY_RUN" = 1 ] || ok "已挂载"
    else
        # 最常见的两种失败：容器里没有 mount 权限；或挂载点不存在（受限/只读 /sys）
        warn "挂载失败：$OUT"
    fi
fi

if [ "$DO_FSTAB" = 1 ]; then
    if grep -qs " $CONFIGFS_DIR configfs " /etc/fstab; then
        dim "已在 /etc/fstab 中，保持不动"
    elif [ "$DRY_RUN" = 1 ]; then
        dim "[dry-run] 追加 configfs 到 /etc/fstab"
    else
        printf 'configfs %s configfs defaults 0 0\n' "$CONFIGFS_DIR" >>/etc/fstab &&
            ok "已写入 /etc/fstab（开机自动挂载）" || warn "写入 /etc/fstab 失败"
    fi
fi

# ============================================================
# 4.5) LIO 就绪验收：只看 targetcli 能不能跑通
# ============================================================
# 判据是**行为**，不是"configfs 里某个目录在不在"。
# 曾经检查 $CONFIGFS_TARGET/iscsi 与 $CONFIGFS_TARGET/core/iblock_0 是否存在，结果把正常机器
# 判成缺项：这两个目录都是 rtslib 按需创建、内核注册时机也随版本不同的内部细节——
# core/iblock_0 要等第一个 iblock backstore 建出来才出现——于是报告长期报红（用户看到的就是
# "装了还是不可用"），而 targetcli 明明是好的。
# targetcli 启动时就会读整棵 configfs 树，所以"它能跑通"已经覆盖了「configfs 挂上了 +
# rtslib 能读到 LIO 树」这两件事，这才是 iSCSI 就绪与否的真正判据。
TARGETCLI_MISSING=0
TARGETCLI_PROBE_ERR=''
# probe_line 从 $OUT 里挑一行有信息量的：跳过空行与 rtslib 的无害告警。
# 必须跳过（实测）：targetcli 首次运行会往 stderr 打
#   Warning: Could not load preferences file /root/.targetcli/prefs.bin.
# 它与"能不能用"无关，却排在版本号前面——直接 head -n1 等于把这句噪声当成检测结果贴出来。
# first：成功时取版本行；last：失败时取异常栈末尾（结论在最后一行）。
probe_line() {
    if [ "$1" = last ]; then
        printf '%s\n' "$OUT" | grep -viE '^[[:space:]]*(warning|warn|deprecationwarning):' |
            grep -v '^[[:space:]]*$' | tail -n1 || true
    else
        printf '%s\n' "$OUT" | grep -viE '^[[:space:]]*(warning|warn|deprecationwarning):' |
            grep -v '^[[:space:]]*$' | head -n1 || true
    fi
}

targetcli_probe() {
    if ! command -v targetcli >/dev/null 2>&1; then
        TARGETCLI_PROBE_ERR='PATH 中没有 targetcli'
        return 1
    fi
    if [ "$DRY_RUN" = 1 ]; then
        dim "[dry-run] targetcli version"
        return 0
    fi
    if run_capture targetcli version; then
        ok "targetcli 可用：$(probe_line first)"
        return 0
    fi
    TARGETCLI_PROBE_ERR="$(probe_line last)"
    [ -n "$TARGETCLI_PROBE_ERR" ] || TARGETCLI_PROBE_ERR='(targetcli 无输出，退出码非 0)'
    return 1
}

if ! targetcli_probe; then
    warn "targetcli 跑不通 —— iSCSI 能不能用只看它，与 configfs 里的具体目录无关"
    dim "    targetcli 报错：$TARGETCLI_PROBE_ERR"
    if configfs_instance_mismatch; then
        dim "    连 $CONFIGFS_TARGET 整个不存在：本进程看到的 configfs 与内核注册 LIO 的那份不是同一个实例"
        dim "    （容器里自己 mount 了一份空的）——见末尾「诊断」第 1 条"
    fi
    TARGETCLI_MISSING=1
fi

# 说明：这里刻意**不再**做 modprobe -r 重载（旧版 4.5 会做）。它唯一的用途是让上面那两个
# 目录重新出现，而那并不是判据；代价却是把正在给客户端服务的整棵目标配置一并拆掉。
# 真需要重载时人工执行（会短暂中断在线目标）：
#   modprobe -r iscsi_target_mod && modprobe iscsi_target_mod

# configfs_instance_mismatch 只作**辅助判据**：configfs 已挂载、target_core_mod 已加载，
# 但连 $CONFIGFS_TARGET 都没有。依据是 target_core_mod 的 init 必须注册 target 子系统，
# 注册成功才会有该目录（注册失败模块根本加载不进来）——所以这组合指向"看到的不是同一份
# configfs"（容器里自己 mount 了一份空的），而不是模块问题。它不再参与任何缺项判定。
configfs_instance_mismatch() {
    [ -d "$CONFIGFS_TARGET" ] && return 1
    grep -qs " $CONFIGFS_DIR configfs " /proc/mounts || return 1
    grep -q '^target_core_mod ' /proc/modules 2>/dev/null || return 1
    return 0
}

# ============================================================
# 5) 复检：与 sysdeps 的判据保持一致
# ============================================================
step "复检"

MISSING_REQUIRED=''

# req_cmds 检查必需命令；缺任何一个都计入退出码。
req_cmds() {
    label=$1; shift
    miss=''
    for c in "$@"; do
        command -v "$c" >/dev/null 2>&1 || miss="${miss:+$miss }$c"
    done
    if [ -z "$miss" ]; then
        ok "$label"
    else
        warn "$label 缺失：$miss"
        MISSING_REQUIRED="${MISSING_REQUIRED:+$MISSING_REQUIRED }$miss"
    fi
}

# opt_cmds 检查可选命令；缺失只提示，不影响退出码。
opt_cmds() {
    label=$1; shift
    miss=''
    for c in "$@"; do
        command -v "$c" >/dev/null 2>&1 || miss="${miss:+$miss }$c"
    done
    if [ -z "$miss" ]; then
        ok "$label"
    else
        dim "$label 未就绪：$miss（可选，用到相关功能时才需要）"
    fi
}

req_cmds "LVM2 工具链" lvm lvcreate lvchange lvs dmsetup
req_cmds "块设备/挂载工具" mount umount blkid lsblk wipefs
req_cmds "内核模块工具" modprobe
# targetcli：LIO iSCSI 目标的必需执行体（写操作经它下发）。缺它则 iSCSI 目标不可用。
req_cmds "LIO 目标命令行（targetcli）" targetcli

# 文件系统工具：ext4 与 xfs 任意一族齐全即可（与 sysdeps.probeFSTools 同判据）。
if command -v mkfs.ext4 >/dev/null 2>&1 && command -v resize2fs >/dev/null 2>&1; then
    ok "文件系统工具（ext4）"
elif command -v mkfs.xfs >/dev/null 2>&1 && command -v xfs_growfs >/dev/null 2>&1; then
    ok "文件系统工具（xfs）"
else
    warn "文件系统工具缺失：mkfs.ext4+resize2fs 或 mkfs.xfs+xfs_growfs 至少要有一族"
    MISSING_REQUIRED="$MISSING_REQUIRED fs_tools"
fi

# 代码实际会调用、但 sysdeps 未逐项探测的 LVM 子命令：一并校验，避免"探测通过但建盘失败"。
opt_cmds "LVM 扩展命令" pvcreate vgcreate vgextend pvs vgs lvremove lvextend lvconvert pvremove
opt_cmds "辅助工具" thin_ls thin_check fstrim ntfsfix ntfslabel

# LIO 侧只看行为（4.5 节已实测），缺项名与 sysdeps 的 Item.Key 对齐，便于和 doctor -json 对照。
# 刻意**不**再把 $CONFIGFS_TARGET/iscsi、$CONFIGFS_TARGET/core/iblock_0 这类 configfs 目录当缺项：
# 它们是 rtslib 按需创建的内部细节（core/iblock_0 要等第一个 iblock backstore 建出来才出现），
# 拿它们当判据会把好机器判成故障，报告长期报红而 targetcli 其实是好的。
if [ "$TARGETCLI_MISSING" = 1 ]; then
    MISSING_REQUIRED="$MISSING_REQUIRED lio_tools"
fi

# 去重后输出，便于直接贴给别人看。
MISSING_REQUIRED=$(uniq_words "$MISSING_REQUIRED")

# ============================================================
# 6) 结论与诊断
# ============================================================
in_container() {
    [ -f /.dockerenv ] && return 0
    [ -f /run/.containerenv ] && return 0
    [ -n "${container:-}" ] && return 0   # systemd 在容器内会导出 container=
    if [ -r /proc/1/cgroup ] && grep -qE '(docker|kubepods|containerd|lxc|podman)' /proc/1/cgroup 2>/dev/null; then
        return 0
    fi
    # cgroup v2 下 /proc/1/cgroup 只有 "0::/"，上面的关键词匹配不到，改看根文件系统是否 overlay。
    if grep -qs '^overlay / overlay ' /proc/mounts; then
        return 0
    fi
    if command -v systemd-detect-virt >/dev/null 2>&1; then
        v=$(systemd-detect-virt -c 2>/dev/null || true)
        [ -n "$v" ] && [ "$v" != none ] && return 0
    fi
    return 1
}

step "结果"
if [ -z "$MISSING_REQUIRED" ]; then
    ok "必需项全部就绪（configfs / LIO 模块 / LVM2 / 文件系统工具 / 块设备工具）"
    info "下一步："
    dim "1) ./vault-server doctor            # 服务端自带自检，逐项复核（退出码 0 = 就绪）"
    dim "2) sudo ./scripts/install-systemd.sh  # 注册为 systemd 服务（root 运行）"
    dim "3) 准备一块独立数据盘做 LVM PV（如 /dev/sdb），在客户端「系统设置 → LVM 存储池」初始化"
    exit 0
fi

err "仍未就绪：$MISSING_REQUIRED"
[ -n "$MODPROBE_FAIL" ] && err "modprobe 失败：$(uniq_words "$MODPROBE_FAIL")"
[ -n "$LAST_FAILED" ] && err "有软件包装不上：$(uniq_words "$LAST_FAILED")"

printf '\n%s诊断（"装了还是不可用"的常见原因）%s\n' "$BOLD" "$RST"
# targetcli 跑不通时下面会给出更具体的 docker 提法，这里不重复。
if in_container && [ "$TARGETCLI_MISSING" = 0 ]; then
    warn "当前环境像是容器/沙箱：加载内核模块与挂载需要特权，且必须用宿主机的内核与模块——"
    dim "   docker run --privileged -v /lib/modules:/lib/modules:ro -v /sys/kernel/config:/sys/kernel/config:rslave ..."
    dim "   更稳妥的做法：把服务端直接装在宿主机上（裸机或虚拟机），而不是容器里。"
fi
if [ ! -d "/lib/modules/$KREL" ]; then
    warn "/lib/modules/$KREL 不存在：运行内核（$KREL）与已安装的模块不匹配。"
    dim "   多半是升级了内核但没重启：reboot 后重跑本脚本。"
fi
if ! grep -qs " $CONFIGFS_DIR configfs " /proc/mounts; then
    warn "configfs 未挂载成功：容器需 bind 宿主机的 /sys/kernel/config，或给足 mount 权限。"
fi
# 唯一判据：targetcli 能不能跑通（4.5 节实测）。
# 注意 $CONFIGFS_TARGET 下有没有 iscsi/core/iblock_0 **不代表**故障：那些目录是 rtslib 按需
# 创建的内部细节（core/iblock_0 要等第一个 iblock backstore 建出来才出现），它们不在时不必做
# 任何处理，更不要为了它们去重载模块——重载会拆掉正在给客户端服务的整棵目标配置。
if [ "$TARGETCLI_MISSING" = 1 ]; then
    if configfs_instance_mismatch; then
        # 辅助判据（能定性）：configfs 已挂载、target_core_mod 已加载，却连 $CONFIGFS_TARGET 都没有。
        # 依据：target_core_mod 的 init 必须注册 target 子系统，注册成功才会有该目录（注册失败模块
        # 根本加载不进来）——所以这只能说明看到的不是内核注册进的那一份 configfs。
        warn "targetcli 跑不通，且连 $CONFIGFS_TARGET 都不存在 —— configfs 实例错位："
        dim "   内核注册是成功的（否则该目录不会出现），所以不是模块、也不是 targetcli 的问题，"
        dim "   而是本进程看到的 configfs 与内核注册 LIO 的那一份不是同一个实例"
        dim "   （容器里自己 mount 的那份是空的，宿主机那份才有 target）。"
        dim "   重载模块永远修不好，还会拆掉正在服务的目标 —— 别反复 modprobe -r / 重跑本脚本。"
        dim "   处置：容器改成共享宿主机的 configfs 后重启服务端："
        dim "     docker run --privileged -v /lib/modules:/lib/modules:ro -v $CONFIGFS_DIR:$CONFIGFS_DIR:rslave ..."
        dim "   或把服务端直接装在宿主机（裸机/虚拟机）上跑。"
    else
        warn "targetcli 跑不通（报错见上）—— 逐条排除："
        dim "   1) LIO 模块没加载：grep -E '^(target_core_mod|iscsi_target_mod|target_core_iblock) ' /proc/modules"
        dim "      若 dmesg 里出现「target_core_mod: unknown parameter 'iscsi_target_mod' ignored」，"
        dim "      说明有脚本把多个模块名写进了同一条 modprobe（后面的名字被当成参数吃掉了）。"
        dim "      本脚本已改成逐模块 modprobe，重跑即可；手工修：逐个 modprobe。"
        dim "   2) configfs 没挂：mount -t configfs none $CONFIGFS_DIR（容器需 bind 宿主机的 $CONFIGFS_DIR）。"
        dim "   3) 内核没编译 LIO（缺 CONFIG_TARGET_CORE/CONFIG_ISCSI_TARGET）：dmesg | grep -iE 'target|iscsi'。"
        dim "   4) rtslib/Python 环境损坏，或权限不足（targetcli 写 configfs 需 root）。"
        dim "   把这几条的输出一起提供："
        dim "     targetcli version ; grep -E 'target|iscsi' /proc/modules"
        dim "     mount | grep configfs"
        dim "     dmesg | grep -iE 'target|iscsi|configfs' | tail -20"
    fi
fi
dim "Secure Boot：未签名的内核模块会被拒绝（dmesg 里报 Key was rejected by service），"
dim "   用发行版签名的模块，或在 BIOS 里关掉 Secure Boot。"
dim "精简/云厂商内核可能未编译 LIO（缺 CONFIG_TARGET_CORE）：换发行版通用内核。"
dim "仍解决不了时，把 './vault-server doctor -json' 的输出一并提供。"
exit 1
