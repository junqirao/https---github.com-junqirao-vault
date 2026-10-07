#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Vault 项目一体化构建脚本。

设计目标
--------
1. **一条命令构建全项目**：前端（React/Vite）+ 两个 Go 产物
   （Vault-Server / Vault-Agent）。
2. **自动版本管理**：以"客户端源码指纹"为准，检测到客户端相关源码有变动时
   自动递增版本号，并把版本号写入 `VERSION`、前端 package.json 与 Go 产物。
3. **自动生成客户端更新包**：调用 `Vault-Server sign release` 用发布方私钥签名，
   产出 manifest.json / manifest.json.sig / artifacts/，并复制到服务端
   `update.artifacts_dir`（默认为 `updates/`）——即服务端镜像目录。
4. **零第三方依赖**：仅用 Python 标准库（PyYAML 若存在则用于更稳地读配置）。
5. **自动清理旧版本残留**：每次构建都清掉 `dist/` 中**不属于本次版本**的产物
   （顶层的 `Vault-Web-<旧版本>.zip`、`dist/package/` 下旧版本的目录与压缩包），
   避免越积越多、也避免误把旧产物当成这次的发布物。

关于 TLS 证书（重要）
---------------------
**本脚本不生成也不管理 TLS 证书**。服务端在启动时由自建 CA 自动签发并维护
`<配置文件目录>/pki/server.crt` 与 `server.key`：不存在则创建，已存在则复用，
SAN 主机集合变化时自动重签。因此部署时无需任何外部证书文件。（见 docs/implementation.md 9.1）

关于更新签名密钥（重要）
------------------------
- 私钥（`keys/update-private.pem`）是**发布方唯一信任根**：
  **绝不可提交到代码库、绝不可上传到服务端**。`.gitignore` 已屏蔽 `keys/` 与 `*.pem`。
- 公钥在构建时通过 ldflags 注入到 Vault-Agent，客户端据此验签。
- 若 `keys/` 不存在，脚本会为**本地联调**自动生成一对密钥并显著告警；
  生产环境请改用离线保管的正式私钥（`--key` 指定）。

用法示例
--------
    python build.py                      # 智能构建（客户端源码变了就自增版本并出更新包）
    python build.py --no-bump            # 不自动递增版本（仅重新构建）
    python build.py --version 0.3.0      # 指定版本
    python build.py --bump minor         # 指定自增级别：patch|minor|major
    python build.py --skip-frontend      # 只构建 Go 产物（迭代时更快）
    python build.py --skip-update        # 不生成更新包（不出 manifest，不复制到 updates/）
    python build.py --clean              # 构建前清理 dist/ 与前端产物
    python build.py --check              # 只做环境与版本体检，不构建
    python build.py --package            # 额外生成服务端部署包（Windows zip + Linux tar.gz）
    python build.py --package-all        # 服务端（Windows + Linux）+ 客户端部署包
    python build.py --package-only       # 只打包（dist/ 已构建过时最快）
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import time
import zipfile
from pathlib import Path

# ---------------------------------------------------------------------------
# 常量与路径
# ---------------------------------------------------------------------------

ROOT = Path(__file__).resolve().parent

VERSION_FILE = ROOT / "VERSION"
BUILD_STATE_FILE = ROOT / ".build-state.json"
CONFIG_FILE = ROOT / "config.yaml"
CONFIG_EXAMPLE_FILE = ROOT / "config.example.yaml"

DIST_DIR = ROOT / "dist"
RELEASE_DIR = ROOT / "build" / "release"  # Vault-Server sign 的临时打包目录（中间产物）
KEYS_DIR = ROOT / "keys"
PRIVATE_KEY_FILE = KEYS_DIR / "update-private.pem"
PUBLIC_KEY_FILE = KEYS_DIR / "update-public.txt"

FRONTEND_DIR = ROOT / "frontend"
CLIENT_APP_DIR = FRONTEND_DIR / "apps" / "client"
CLIENT_PKG_JSON = CLIENT_APP_DIR / "package.json"

# Go 产物定义：(输出名, 包路径, 是否需要注入发布方公钥)
GO_TARGETS = [
    ("Vault-Server.exe", "./cmd/vault-server", False),
    ("Vault-Agent.exe", "./cmd/vault-agent", True),
]

# Linux 服务端的交叉编译产物名（无 .exe 后缀）。
#
# 只交叉编译服务端：Vault-Agent 依赖 golang.org/x/sys/windows（Windows 专属构建约束），
# 无法在非 Windows 目标下编译；客户端本就是随 Windows 部署分发的本机程序。
LINUX_SERVER_TARGET = "vault-server-linux-amd64"

# 决定"客户端版本是否变化"的源码范围。
# 只覆盖会影响 Vault-Agent 行为与其依赖的路径 —— 服务端专属改动不应触发客户端版本递增。
CLIENT_SOURCE_PATHS = [
    "frontend",
    "internal/agent",
    "internal/platform",
    "internal/updatepkg",
    "cmd/vault-agent",
]

# 服务端专属路径：仅用于提示"服务端有改动但版本未变"，不参与版本递增决策。
SERVER_SOURCE_PATHS = [
    "internal/api",
    "internal/app",
    "internal/store",
    "internal/config",
    "internal/cert",
    "internal/domain",
    "cmd/vault-server",
]

# 指纹计算时跳过的目录名与文件后缀。
SKIP_DIR_NAMES = {
    "node_modules", "dist", "dist-electron", "release", "build",
    ".vite", "__pycache__", ".git", "logs", "data", "pki", "keys",
    "updates", ".cache",
}
SKIP_SUFFIXES = {".log", ".exe", ".dll", ".pdb", ".map", ".tmp", ".part"}

# 单个文件超过该大小则只按 size+mtime 计入指纹（避免哈希大二进制）。
FINGERPRINT_MAX_HASH_BYTES = 5 * 1024 * 1024

DEFAULT_CHUNK_HINT = ""


# ---------------------------------------------------------------------------
# 输出辅助
# ---------------------------------------------------------------------------

class C:
    """极简 ANSI 着色（Windows Terminal / PowerShell 7 支持；不支持时无副作用）。"""

    RESET = "\033[0m"
    BOLD = "\033[1m"
    DIM = "\033[2m"
    RED = "\033[31m"
    GREEN = "\033[32m"
    YELLOW = "\033[33m"
    BLUE = "\033[36m"


def _supports_color() -> bool:
    if os.environ.get("NO_COLOR"):
        return False
    if not sys.stdout.isatty():
        return False
    return True


_COLOR = _supports_color()


def c(text: str, color: str) -> str:
    return f"{color}{text}{C.RESET}" if _COLOR else text


def step(msg: str) -> None:
    print(f"\n{c('▶', C.BLUE)} {c(msg, C.BOLD)}")


def info(msg: str) -> None:
    print(f"  {msg}")


def ok(msg: str) -> None:
    print(f"  {c('✓', C.GREEN)} {msg}")


def warn(msg: str) -> None:
    print(f"  {c('!', C.YELLOW)} {msg}")


def fail(msg: str) -> None:
    print(f"  {c('✗', C.RED)} {msg}", file=sys.stderr)


def die(msg: str, code: int = 1) -> None:
    fail(msg)
    sys.exit(code)


# ---------------------------------------------------------------------------
# 子进程封装
# ---------------------------------------------------------------------------

# 让子进程不再弹出控制台窗口。
#
# 从终端直接运行本脚本时子进程本就依附于当前控制台，看不出差别；但从 IDE（无控制台）
# 或由其它程序调用时，每个控制台子程序（pnpm/go/git/.cmd 垫片）都会新开一个黑框，
# 一闪一闪很影响观感。统一加 CREATE_NO_WINDOW（不影响 stdout/stderr 捕获）。
_NO_WINDOW: dict[str, object] = (
    {"creationflags": subprocess.CREATE_NO_WINDOW} if os.name == "nt" else {}
)


def run(cmd: list[str], cwd: Path | None = None, capture: bool = True,
        check: bool = True, env: dict[str, str] | None = None,
        quiet: bool = False) -> subprocess.CompletedProcess:
    """执行子进程；capture=False 时直接透传到控制台（用于 npm/vite 这类长输出）。"""
    merged_env = os.environ.copy()
    if env:
        merged_env.update(env)
    if not quiet:
        info(c("$ " + " ".join(cmd), C.DIM))
    try:
        proc = subprocess.run(
            cmd,
            cwd=str(cwd) if cwd else str(ROOT),
            env=merged_env,
            capture_output=capture,
            text=True,
            encoding="utf-8",
            errors="replace",
            shell=False,
            **_NO_WINDOW,
        )
    except FileNotFoundError as exc:
        if check:
            die(f"命令不存在：{cmd[0]}（{exc}）")
        raise

    if capture and not quiet:
        # 仅回显尾部，避免刷屏
        for stream in (proc.stdout, proc.stderr):
            if stream and stream.strip():
                tail = stream.strip().splitlines()[-12:]
                for line in tail:
                    print(f"    {line}")

    if check and proc.returncode != 0:
        die(f"命令失败（exit={proc.returncode}）：{' '.join(cmd)}")
    return proc


def which(name: str) -> str | None:
    return shutil.which(name)


def go_bin() -> str:
    """定位 go 可执行文件（本机 PATH 可能缺少 Go，需回退到常见安装位置）。"""
    found = which("go")
    if found:
        return found
    candidates = [
        Path(r"C:\Program Files\Go\bin\go.exe"),
        Path(r"C:\Go\bin\go.exe"),
        Path(os.environ.get("LOCALAPPDATA", "")) / "Programs" / "Go" / "bin" / "go.exe",
    ]
    for cand in candidates:
        if cand.is_file():
            return str(cand)
    die("找不到 go 可执行文件；请安装 Go 或将 Go 的 bin 目录加入 PATH")
    raise SystemExit(1)


def go_env() -> dict[str, str]:
    """Go 构建统一环境变量：禁用 cgo（本项目零处需要 cgo，见 docs/implementation.md 10.4）。"""
    return {
        "CGO_ENABLED": "0",
        "GOTOOLCHAIN": "local",
    }


# ---------------------------------------------------------------------------
# 配置读取（用于定位 update.artifacts_dir / update.channel）
# ---------------------------------------------------------------------------

def _read_yaml_module():
    try:
        import yaml  # type: ignore
        return yaml
    except Exception:
        return None


def load_config() -> dict:
    """读取 config.yaml；不存在则退回 config.example.yaml。

    有 PyYAML 时用 PyYAML；否则用针对本项目配置格式的极简缩进解析
    （只解析两级嵌套的标量，足够读取 update.* 与 server.* 等字段）。
    """
    path = CONFIG_FILE if CONFIG_FILE.is_file() else CONFIG_EXAMPLE_FILE
    if not path.is_file():
        return {}
    text = path.read_text(encoding="utf-8")
    yaml_mod = _read_yaml_module()
    if yaml_mod is not None:
        try:
            data = yaml_mod.safe_load(text) or {}
            if isinstance(data, dict):
                return data
        except Exception as exc:  # 配置写坏时不要让构建挂掉
            warn(f"PyYAML 解析 {path.name} 失败，改用内置解析：{exc}")

    # 内置极简解析：仅支持 "key:" / "  key: value" / "# 注释"
    result: dict = {}
    current_top: str | None = None
    for raw_line in text.splitlines():
        line = raw_line.split("#", 1)[0].rstrip()
        if not line.strip():
            continue
        indent = len(line) - len(line.lstrip())
        m = re.match(r"^(\s*)([A-Za-z0-9_]+):\s*(.*)$", line)
        if not m:
            continue
        key, value = m.group(2), m.group(3).strip()
        if indent == 0:
            if value == "":
                current_top = key
                result.setdefault(key, {})
            else:
                result[key] = _scalar(value)
                current_top = None
        elif indent > 0 and current_top:
            if not isinstance(result.get(current_top), dict):
                result[current_top] = {}
            result[current_top][key] = _scalar(value)
    return result


def _scalar(value: str):
    v = value.strip().strip('"').strip("'")
    if v in ("true", "True"):
        return True
    if v in ("false", "False"):
        return False
    if re.fullmatch(r"-?\d+", v):
        return int(v)
    return v


def resolve_update_out_dir(cfg: dict, cli_out: str | None) -> Path:
    """确定更新包输出目录（= 服务端 update.artifacts_dir）。"""
    if cli_out:
        p = Path(cli_out)
        return p if p.is_absolute() else (ROOT / p)
    update_cfg = cfg.get("update") or {}
    raw = str(update_cfg.get("artifacts_dir") or "").strip()
    if raw:
        p = Path(raw)
        return p if p.is_absolute() else (ROOT / p)
    return ROOT / "updates"


def resolve_channel(cfg: dict) -> str:
    update_cfg = cfg.get("update") or {}
    return str(update_cfg.get("channel") or "stable").strip() or "stable"


# ---------------------------------------------------------------------------
# 源码指纹
# ---------------------------------------------------------------------------

def _walk_files(base: Path):
    """遍历 base 下的文件，**进入目录前就按名字剪枝**。

    刻意不用 base.rglob("*")：
      - rglob 会先把整棵树展开、再由调用方过滤，因此 `node_modules` 这类目录会被
        整体遍历一遍（几十万条目，构建明显变慢）；
      - 更致命的是 Windows 上的连接点/装载点（如 pnpm 装的 `@ant-design/icons`）：
        rglob 展开时会对它 stat，直接抛 `OSError: [WinError 448] 无法遍历该路径，
        因为它包含不受信任的装入点`，把整个构建打断。

    这里先按目录名剪枝（不需要 stat），剩余条目再做 stat，且任何 OSError 都跳过——
    "算不出指纹"绝不能变成"构建跑不起来"。
    """
    stack = [base]
    while stack:
        current = stack.pop()
        try:
            entries = sorted(current.iterdir())
        except OSError:
            continue
        for entry in entries:
            if entry.name in SKIP_DIR_NAMES:
                continue
            try:
                if entry.is_dir():
                    stack.append(entry)
                    continue
                if not entry.is_file():
                    continue
            except OSError:
                continue
            yield entry


def _iter_source_files(rel_paths: list[str]):
    for rel in rel_paths:
        base = ROOT / rel
        try:
            if base.is_file():
                yield base
                continue
            if not base.is_dir():
                continue
        except OSError:
            continue
        for path in _walk_files(base):
            if path.suffix.lower() in SKIP_SUFFIXES:
                continue
            yield path


def source_fingerprint(rel_paths: list[str]) -> str:
    """对给定路径集合计算内容级指纹（相对路径 + 内容哈希 / 大文件用 size+mtime）。"""
    digest = hashlib.sha256()
    count = 0
    for path in _iter_source_files(rel_paths):
        try:
            rel = path.relative_to(ROOT).as_posix()
        except ValueError:
            continue
        digest.update(rel.encode("utf-8"))
        digest.update(b"\0")
        try:
            st = path.stat()
        except OSError:
            continue
        if st.st_size > FINGERPRINT_MAX_HASH_BYTES:
            digest.update(f"big:{st.st_size}:{int(st.st_mtime)}".encode("utf-8"))
        else:
            try:
                digest.update(hashlib.sha256(path.read_bytes()).digest())
            except OSError:
                digest.update(f"err:{st.st_size}".encode("utf-8"))
        digest.update(b"\n")
        count += 1
    digest.update(f"files={count}".encode("utf-8"))
    return digest.hexdigest()


def load_build_state() -> dict:
    if BUILD_STATE_FILE.is_file():
        try:
            return json.loads(BUILD_STATE_FILE.read_text(encoding="utf-8"))
        except Exception:
            warn(".build-state.json 内容损坏，按首次构建处理")
    return {}


def save_build_state(state: dict) -> None:
    BUILD_STATE_FILE.write_text(
        json.dumps(state, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


# ---------------------------------------------------------------------------
# 版本管理
# ---------------------------------------------------------------------------

SEMVER_RE = re.compile(r"^(\d+)\.(\d+)\.(\d+)$")


def read_version() -> str:
    """读取版本真源：优先 VERSION 文件，其次前端 package.json。"""
    if VERSION_FILE.is_file():
        v = VERSION_FILE.read_text(encoding="utf-8").strip()
        if SEMVER_RE.match(v):
            return v
        warn(f"VERSION 文件内容非法（{v!r}），将回退到 package.json")

    if CLIENT_PKG_JSON.is_file():
        try:
            v = str(json.loads(CLIENT_PKG_JSON.read_text(encoding="utf-8")).get("version", "")).strip()
            if SEMVER_RE.match(v):
                return v
        except Exception:
            pass
    return "0.1.0"


def bump_version(version: str, level: str) -> str:
    m = SEMVER_RE.match(version)
    if not m:
        die(f"版本号非法，无法递增：{version!r}")
        raise SystemExit(1)
    major, minor, patch = (int(x) for x in m.groups())
    if level == "major":
        return f"{major + 1}.0.0"
    if level == "minor":
        return f"{major}.{minor + 1}.0"
    return f"{major}.{minor}.{patch + 1}"


def write_version_everywhere(version: str) -> None:
    """把版本号写入所有真源，避免多处版本漂移。"""
    VERSION_FILE.write_text(version + "\n", encoding="utf-8")

    if CLIENT_PKG_JSON.is_file():
        try:
            data = json.loads(CLIENT_PKG_JSON.read_text(encoding="utf-8"))
            if data.get("version") != version:
                data["version"] = version
                CLIENT_PKG_JSON.write_text(
                    json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        except Exception as exc:
            warn(f"写入前端 package.json 版本失败：{exc}")

    # 根 package.json（pnpm workspace）通常不含版本，存在则同步
    root_pkg = FRONTEND_DIR / "package.json"
    if root_pkg.is_file():
        try:
            data = json.loads(root_pkg.read_text(encoding="utf-8"))
            if "version" in data and data["version"] != version:
                data["version"] = version
                root_pkg.write_text(
                    json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        except Exception:
            pass


def decide_version(args, state: dict) -> tuple[str, str, bool, str]:
    """决定本次构建使用的版本号。

    返回 (新版本, 旧版本, 是否递增, 递增原因)
    """
    current = read_version()

    if args.version:
        target = args.version.strip()
        if not SEMVER_RE.match(target):
            die(f"--version 非法（需形如 1.2.3）：{target!r}")
        return target, current, target != current, "显式指定版本"

    client_fp = source_fingerprint(CLIENT_SOURCE_PATHS)
    last_fp = str(state.get("client_fingerprint") or "")

    if not last_fp:
        return current, current, False, "首次构建，沿用当前版本"

    if client_fp == last_fp:
        return current, current, False, "客户端源码无变化"

    if args.no_bump:
        return current, current, False, "客户端源码有变化，但已用 --no-bump 抑制递增"

    target = bump_version(current, args.bump)
    return target, current, True, f"检测到客户端源码变化（客户端指纹 {last_fp[:12]} → {client_fp[:12]}）"


# ---------------------------------------------------------------------------
# 发布密钥
# ---------------------------------------------------------------------------

def ensure_signing_keys(server_exe: Path, key_override: str | None, allow_generate: bool) -> Path:
    """确保发布方私钥可用，返回私钥路径（keygen 由 Vault-Server sign 子命令执行）。"""
    if key_override:
        key_path = Path(key_override)
        if not key_path.is_absolute():
            key_path = ROOT / key_path
        if not key_path.is_file():
            die(f"指定的私钥不存在：{key_path}")
        if not PUBLIC_KEY_FILE.is_file():
            die("指定了私钥但缺少公钥文件 keys/update-public.txt；"
                "请用 `Vault-Server sign keygen` 重新生成，或手动放置对应的公钥")
        return key_path

    if PRIVATE_KEY_FILE.is_file() and PUBLIC_KEY_FILE.is_file():
        return PRIVATE_KEY_FILE

    if not allow_generate:
        die("找不到发布方密钥（keys/update-private.pem）。"
            "请用 `Vault-Server sign keygen -out-dir ./keys` 生成，或用 --key 指定私钥")

    print()
    warn("未找到发布方密钥，正在为**本地联调**生成一对新密钥")
    warn("生产环境请改用离线保管的正式私钥（--key 指定），且绝不可把私钥入库/上传")
    if not server_exe.is_file():
        die(f"缺少 {server_exe}，无法生成密钥（请先构建 Vault-Server，或用 --skip-go 后重试）")
    KEYS_DIR.mkdir(parents=True, exist_ok=True)
    run([str(server_exe), "sign", "keygen", "-out-dir", str(KEYS_DIR)])
    if not PRIVATE_KEY_FILE.is_file():
        die("密钥生成失败：未产出私钥文件")
    return PRIVATE_KEY_FILE


def read_public_key() -> str:
    if not PUBLIC_KEY_FILE.is_file():
        die(f"缺少发布方公钥文件：{PUBLIC_KEY_FILE}")
    pub = PUBLIC_KEY_FILE.read_text(encoding="utf-8").strip()
    if not pub:
        die("发布方公钥文件为空")
    return pub


# ---------------------------------------------------------------------------
# 构建步骤
# ---------------------------------------------------------------------------

def clean_outputs() -> None:
    step("清理构建产物")
    for path in [DIST_DIR, RELEASE_DIR]:
        if path.exists():
            shutil.rmtree(path, ignore_errors=True)
            info(f"已删除 {path.relative_to(ROOT)}")
    for path in [CLIENT_APP_DIR / "dist", CLIENT_APP_DIR / "dist-electron"]:
        if path.exists():
            shutil.rmtree(path, ignore_errors=True)
            info(f"已删除 {path.relative_to(ROOT)}")
    ok("清理完成")


def _version_in_name(name: str) -> str | None:
    """从产物名里取出语义化版本（如 `Vault-Web-0.1.62.zip` → `0.1.62`）；没有则 None。"""
    m = re.search(r"\d+\.\d+\.\d+", name)
    return m.group(0) if m else None


def clean_stale_dist_artifacts(keep_version: str) -> None:
    """清掉 `dist/` 中**不属于本次版本**的旧版本产物（best-effort，失败只告警）。

    为什么需要：`dist/` 顶层每构建一次就多一个 `Vault-Web-<版本>.zip`，`dist/package/`
    每打包一次就多一组 `<包名>-<版本>-<平台>` 的目录与压缩包。长期累积既白占空间，又容易
    让人误拿到旧包（"发出去的客户端怎么还是旧界面"这类事故，根因常常就是取了目录里
    上一版留下的产物）。

    归属判定只看文件名里的语义化版本号：与本次版本不同的即视为旧产物并删除；没有版本号的
    文件（`Vault-Server.exe` / `Vault-Agent.exe` / `vault-server-linux-amd64`）是每次原地
    覆盖的固定产物，一律不动。

    删除刻意 best-effort：旧产物可能正被占用（例如运行中的客户端正用着 `dist/package/`
    里的文件，或资源管理器开着预览），此时只告警跳过，绝不影响本次构建。
    """
    # PACKAGE_DIR 定义在本文件靠后的"部署打包"一节，模块级常量在调用时已就绪。
    stale: list[Path] = []
    for directory in (DIST_DIR, PACKAGE_DIR):
        if not directory.is_dir():
            continue
        for item in sorted(directory.iterdir()):
            ver = _version_in_name(item.name)
            if ver is not None and ver != keep_version:
                stale.append(item)

    if not stale:
        return

    step(f"清理 dist 中的旧版本产物（仅保留 {keep_version}）")
    for item in stale:
        rel = item.relative_to(ROOT) if item.is_relative_to(ROOT) else item
        try:
            if item.is_dir():
                shutil.rmtree(item)
            else:
                item.unlink()
            info(f"已删除 {rel}")
        except OSError as exc:
            warn(f"旧版本产物未能删除（可能被占用，已跳过）：{rel}（{exc}）")
    ok(f"旧版本产物清理完成（dist 中只保留 {keep_version} 的产物）")


def _frontend_deps_ready() -> bool:
    """判断前端依赖是否**真的可用**（不只是"目录存在"）。

    只判断 `frontend/node_modules` 是否存在远远不够：pnpm 用连接点把
    `node_modules/<pkg>` 指向虚拟仓库 `.pnpm/...`，一旦安装被中断、虚拟仓库被清理、
    或 store 在另一个盘/另一个账号下，这些目录会退化成"存在但打不开"的空壳——
    目录列得出来，`tsc` / `vite` 二进制却根本不在，构建会在 typecheck 处报
    `Cannot find module ...\typescript\bin\tsc` 才失败，很难联想到依赖问题。

    因此直接探测真正要用到的二进制是否可读；stat 抛错一律按"不可用"处理。
    """
    for rel in ("node_modules/.bin/tsc", "node_modules/.bin/tsc.cmd",
                "packages/ui/node_modules/typescript/bin/tsc",
                "packages/ui/node_modules/typescript/bin/tsc.cmd"):
        try:
            if (FRONTEND_DIR / rel).is_file():
                return True
        except OSError:
            continue
    return False


def build_frontend(args) -> None:
    step("构建前端（React + Vite）")
    pnpm = which("pnpm") or which("pnpm.cmd")
    if not pnpm:
        die("找不到 pnpm；请先安装（npm i -g pnpm）并确保在 PATH 中")

    if _frontend_deps_ready():
        info("依赖已就绪，跳过 install（如需强制刷新请先删除 frontend/node_modules）")
    else:
        info("未发现可用的前端依赖（node_modules 缺失或链接已失效），执行 pnpm install")
        install_cmd = [pnpm, "install"]
        if args.registry:
            install_cmd += ["--registry", args.registry]
        run(install_cmd, cwd=FRONTEND_DIR, capture=False)
        if not _frontend_deps_ready():
            die("pnpm install 之后仍未找到可用的 typescript（tsc）；"
                "请删除 frontend/node_modules 与 frontend/*/node_modules 后重试")

    run([pnpm, "-r", "typecheck"], cwd=FRONTEND_DIR, capture=False)
    run([pnpm, "--filter", "@vault/client", "build"], cwd=FRONTEND_DIR, capture=False)
    ok("前端构建完成")


def _git_commit() -> str:
    commit = "unknown"
    if which("git"):
        proc = subprocess.run(["git", "rev-parse", "--short", "HEAD"],
                              cwd=str(ROOT), capture_output=True, text=True, **_NO_WINDOW)
        if proc.returncode == 0 and proc.stdout.strip():
            commit = proc.stdout.strip()
    return commit


def _ldflags(version: str, commit: str, build_time: str, public_key: str | None) -> str:
    """拼装 Go 构建的 ldflags（public_key 非空时注入发布方公钥）。"""
    flags = (
        f"-s -w "
        f"-X main.version={version} "
        f"-X vault/internal/version.Version={version} "
        f"-X vault/internal/version.GitCommit={commit} "
        f"-X vault/internal/version.BuildTime={build_time}"
    )
    if public_key:
        flags += f" -X vault/internal/agent.updatePublicKeyBase64={public_key}"
    return flags


def build_server_exe(version: str) -> None:
    """仅构建 Vault-Server.exe。

    用于"先生成密钥再构建 agent"的流程：keygen/release/verify 由 Vault-Server 的
    sign 子命令提供，因此需要先有一个可执行的服务端二进制（随后被 build_go 复用）。
    """
    DIST_DIR.mkdir(parents=True, exist_ok=True)
    ldflags = _ldflags(version, _git_commit(), time.strftime("%Y-%m-%dT%H:%M:%S%z"), None)
    run([
        go_bin(), "build", "-trimpath",
        "-ldflags", ldflags,
        "-o", str(DIST_DIR / "Vault-Server.exe"), "./cmd/vault-server",
    ], env=go_env(), quiet=True)
    ok("Vault-Server.exe（用于 Vault-Server sign）已就绪")


def build_go(version: str, public_key: str | None, args, prebuilt: set[str] | None = None) -> None:
    step(f"构建 Go 产物（版本 {version}）")
    go = go_bin()
    env = go_env()
    DIST_DIR.mkdir(parents=True, exist_ok=True)

    commit = _git_commit()
    build_time = time.strftime("%Y-%m-%dT%H:%M:%S%z")
    prebuilt = prebuilt or set()

    for out_name, pkg, needs_pubkey in GO_TARGETS:
        out_path = DIST_DIR / out_name
        if out_name in prebuilt and out_path.is_file():
            size = out_path.stat().st_size
            ok(f"{out_name:<20} {size:>12,} 字节 （复用已构建产物）")
            continue
        if needs_pubkey and not public_key:
            die(f"{out_name} 需要注入发布方公钥，但公钥不可用")
        ldflags = _ldflags(version, commit, build_time, public_key if needs_pubkey else None)

        run([
            go, "build", "-trimpath",
            "-ldflags", ldflags,
            "-o", str(out_path), pkg,
        ], env=env, quiet=True)

        size = out_path.stat().st_size if out_path.is_file() else 0
        extra = "（已注入发布方公钥）" if needs_pubkey else ""
        ok(f"{out_name:<20} {size:>12,} 字节 {extra}")

    build_linux_server(go, env, version, commit, build_time)

    info(f"commit={commit}  build_time={build_time}")


def build_linux_server(go: str, env: dict[str, str], version: str, commit: str, build_time: str) -> None:
    """交叉编译 Linux 服务端（GOOS=linux GOARCH=amd64）。

    零 cgo（go_env 已置 CGO_ENABLED=0），因此交叉编译无需任何交叉工具链。
    仅构建服务端：Vault-Agent 是 Windows 专属程序，不可跨操作系统目标编译。
    """
    out_path = DIST_DIR / LINUX_SERVER_TARGET
    ldflags = _ldflags(version, commit, build_time, None)
    run([
        go, "build", "-trimpath",
        "-ldflags", ldflags,
        "-o", str(out_path), "./cmd/vault-server",
    ], env={**env, "GOOS": "linux", "GOARCH": "amd64"}, quiet=True)

    size = out_path.stat().st_size if out_path.is_file() else 0
    ok(f"{LINUX_SERVER_TARGET:<20} {size:>12,} 字节 （linux/amd64）")


def _electron_env() -> dict[str, str]:
    """electron / electron-builder 的下载相关环境变量。

    两个作用（都是实测踩出来的）：
      1. 把下载缓存指到项目内 .cache/，避免受系统目录权限与沙箱限制；
      2. 走国内镜像：GitHub Releases 在部分网络下不可达，会导致
         "connectex: A connection attempt failed"；官方支持用镜像变量覆盖。
    """
    cache = ROOT / ".cache"
    return {
        "ELECTRON_CACHE": str(cache / "electron"),
        "ELECTRON_BUILDER_CACHE": str(cache / "builder"),
        "ELECTRON_MIRROR": "https://npmmirror.com/mirrors/electron/",
        "ELECTRON_BUILDER_BINARIES_MIRROR":
            "https://npmmirror.com/mirrors/electron-builder-binaries/",
        # 未配置代码签名证书时不要尝试签名（否则会额外下载 winCodeSign 并可能失败）
        "CSC_IDENTITY_AUTO_DISCOVERY": "false",
    }


# build_client_app 记录"本次实际使用的客户端程序目录"。
#
# 为什么需要：默认输出目录 release/win-unpacked 里的 app.asar 会被**正在运行的客户端**
# 占住（客户端是 requireAdministrator 进程，构建脚本无法结束它），此时 electron-builder
# 会失败在"删除旧目录"这一步。重试时改用备用输出目录 release-alt，并把结果记在这里，
# 后续 package_client 就不会再回到那个被占用、内容陈旧的旧目录。
_client_app_dir: Path | None = None


REQUIRED_APP_FILES = ("Vault.exe", "resources/app.asar", "icudtl.dat")

# 程序目录里的最小文件数。
#
# 这个阈值只用来识破"清空旧目录半途失败留下的空壳"，不需要贴近真实数量，但**必须明显小于
# 精简后的真实数量**：locales 只保留 4 种语言（见 electron-builder.yml 的 electronLanguages）
# 并做许可外链化（见 scripts/after-pack.cjs）之后，整个目录只剩 22 个文件，
# 沿用精简前的 40 会把正常产物误判成残缺，直接导致构建拒绝产出客户端包。
# 取 15 而不是 22：留出继续精简（例如删掉 vk_swiftshader 那几个文件）的余量；
# 空壳本身已由 REQUIRED_APP_FILES 挡住，这个数字只是第二道防线。
MIN_APP_FILES = 15


def is_complete_app_dir(app_dir: Path | None) -> bool:
    """判断目录是否是**完整可运行**的客户端程序目录。

    为什么必须校验：`release/win-unpacked` 若被正在运行的客户端占用，electron-builder 会在
    "清空旧目录" 这一步删掉一批文件后半途失败，留下一个只剩几个文件（但 Vault.exe 与 app.asar
    因被占用而残留）的空壳。此时若直接拿它打包，会**静默产出一个人畜无害但完全跑不起来的客户端包**
    （体积明显偏小）。所以这里用几个必备文件 + 文件数量做完整性判断。
    """
    if app_dir is None or not app_dir.is_dir():
        return False
    for name in REQUIRED_APP_FILES:
        if not (app_dir / name).is_file():
            return False
    if not list((app_dir / "locales").glob("*.pak")):
        return False
    return sum(1 for _ in app_dir.rglob("*") if _.is_file()) >= MIN_APP_FILES


# 精简后的客户端程序目录应有的形态（由 electron-builder.yml 的 electronLanguages 与
# scripts/after-pack.cjs 决定，改那两处时这里要跟着改）。
SLIM_LOCALES = ("en-US.pak", "zh-CN.pak", "ja.pak", "ko.pak")
CHROMIUM_LICENSES_NAME = "LICENSES.chromium.html"
# app.asar 的体积上限（仅用于告警）。
#
# 正常约 2.5 MB（vite 打包后的 dist + 主进程 dist-electron + package.json）。
# 一旦明显超过这个数，几乎只可能是一件事：electron-builder 又把 package.json 的
# dependencies 复制进来了（那会让它涨到约 79 MB）。判据见 electron-builder.yml
# files 里的 `!node_modules/**/*`。
ASAR_SIZE_WARN_BYTES = 10 * 1024 * 1024


def _client_app_slim_issues(app_dir: Path | None) -> list[str]:
    """返回"体积没精简到位"的问题清单（空列表 = 已精简）。

    为什么需要它：精简由"打包配置 + afterPack 钩子"两个隐式环节实现，任何一环失效（有人手工
    跑 electron-builder 时覆盖了 --config、Electron 换了语言包命名、钩子抛错被忽略…）都只会
    体现为"包大了约 46 MB"——不报错、不影响运行，因而极易被忽略。所以这里把判断结果交给调用方：
    打包前据此决定"要不要重新打包"（见 package_client），打完包再打印一遍（见 check_client_app_slim）。
    """
    if app_dir is None or not app_dir.is_dir():
        return []
    issues: list[str] = []
    locales_dir = app_dir / "locales"
    extra = [p for p in sorted(locales_dir.glob("*.pak")) if p.name not in SLIM_LOCALES]
    if extra:
        size = sum(p.stat().st_size for p in extra)
        issues.append(f"多出 {len(extra)} 个语言包（约 {size / 1048576:.1f} MB）")
    licenses = app_dir / CHROMIUM_LICENSES_NAME
    if licenses.is_file():
        issues.append(f"仍随包携带 {CHROMIUM_LICENSES_NAME}"
                      f"（约 {licenses.stat().st_size / 1048576:.1f} MB）")
    asar = app_dir / "resources" / "app.asar"
    if asar.is_file() and asar.stat().st_size > ASAR_SIZE_WARN_BYTES:
        issues.append(f"resources/app.asar 达 {asar.stat().st_size / 1048576:.1f} MB"
                      "（正常约 2.5 MB，疑似把 node_modules 打进了包）")
    return issues


def check_client_app_slim(app_dir: Path | None) -> None:
    """体检客户端程序目录是否**真的**做了体积精简（只告警，绝不阻断打包）。"""
    for issue in _client_app_slim_issues(app_dir):
        warn(f"客户端体积未精简到位：{issue}"
             "（见 electron-builder.yml 的 electronLanguages / afterPack）")
    if app_dir is None or not app_dir.is_dir():
        return
    missing = [name for name in SLIM_LOCALES
               if not (app_dir / "locales" / name).is_file()]
    if missing:
        warn(f"客户端缺少语言包：{'、'.join(missing)}（Chromium 原生控件文案会回退英文）")


def _app_dir_built_at(app_dir: Path | None) -> float:
    """程序目录的"构建时间"：以 app.asar 的修改时间为准（界面就装在它里面）。"""
    if app_dir is None:
        return 0.0
    try:
        return (app_dir / "resources" / "app.asar").stat().st_mtime
    except OSError:
        return 0.0


def _client_app_candidates() -> list[Path]:
    """全部候选客户端程序目录：默认输出目录 + 各备用输出目录。"""
    return [CLIENT_APP_DIR / name / "win-unpacked"
            for name in ["release", *_fallback_output_dirs()]]


def find_client_app_dir() -> Path | None:
    """返回 electron-builder 产出的**最新**可直接运行的程序目录（win-unpacked）。

    刻意不使用 NSIS 安装包：客户端就是"用 electron 打包出来的程序目录"，
    双击其中的 Vault.exe 即打开界面，不需要安装步骤、也不会产生安装包。

    ⚠️ 多个候选目录时**按 app.asar 的修改时间取最新的一份**，而不是固定挑 release/。
    原因（真实事故）：备用输出目录（release-alt / release-build-2 …）是在"默认目录被
    运行中的客户端占用"时自动产生的，几十天下来会攒下一堆；此时固定挑 release/，
    或者人工从某个旧的 release-build-N 里启动客户端，都会看到**旧界面**，
    表现为"前端明明改了，客户端却毫无变化"，而且极难自查。
    """
    if is_complete_app_dir(_client_app_dir):
        return _client_app_dir
    candidates = [d for d in _client_app_candidates() if is_complete_app_dir(d)]
    if not candidates:
        return None
    return max(candidates, key=_app_dir_built_at)


def _client_app_stale(app_dir: Path | None) -> bool:
    """客户端程序目录是否**早于当前渲染层产物**（即 app.asar 里装的是旧界面）。

    判据：app.asar 的修改时间早于 apps/client/dist/index.html（vite build 的产物）。
    vite build 每次都会重写 index.html，因此它的时间可以当作"本次前端构建"的标记。

    真实事故：渲染层已重建（dist 是新的），但 electron-builder 没有重跑，
    于是部署包里装的是旧 app.asar —— 现象就是"改了前端样式、客户端毫无变化"。
    """
    if not is_complete_app_dir(app_dir):
        return False
    dist_index = CLIENT_APP_DIR / "dist" / "index.html"
    try:
        if not dist_index.is_file():
            # 没有渲染层产物（例如 --skip-frontend 的纯回滚打包）：无从比较，按"不陈旧"处理。
            return False
        return _app_dir_built_at(app_dir) < dist_index.stat().st_mtime
    except OSError:
        return False


def _dir_available(out_dir: Path) -> bool:
    """备用输出目录是否**没被运行中的客户端占用**。

    electron-builder 打包前会清空输出目录，一旦该目录里的 app.asar 正被运行中的客户端
    持有（客户端以管理员权限运行，构建脚本无权结束它），清空就会半途失败并把目录破坏成
    空壳。这里先做一次可写性探测，直接跳过被占用的目录，避免"试一个毁一个"。
    """
    asar = out_dir / "win-unpacked" / "resources" / "app.asar"
    if not asar.is_file():
        return True
    try:
        with open(asar, "r+b"):
            return True
    except OSError:
        return False


# 备用输出目录候选（按顺序取第一个未被占用的）。
#
# 之所以要有多个、且**自动追加序号**：开发机上经常有人直接从上一个备用目录里启动客户端，
# 导致它也被 app.asar 占用；写死两三个名字会不断被追上，所以这里自动扩展候选。
def _fallback_output_dirs() -> list[str]:
    names = ["release-alt", "release-build"]
    names += [f"release-build-{i}" for i in range(2, 10)]
    return names


def build_client_app(args) -> Path | None:
    """用 electron-builder 产出客户端程序目录（可选，依赖网络下载 Electron 运行时）。"""
    step("打包客户端（electron-builder）")
    pnpm = which("pnpm") or which("pnpm.cmd")
    if not pnpm:
        warn("找不到 pnpm，跳过客户端打包")
        return None

    # 先把 agent 放到 electron-builder 约定的位置（对应 electron-builder.yml 的 extraFiles），
    # 这样打出来的程序目录里就有 Vault-Agent.exe，客户端启动时才能找到它。
    agent = DIST_DIR / "Vault-Agent.exe"
    if agent.is_file():
        staged = CLIENT_APP_DIR / "build" / "Vault-Agent.exe"
        staged.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(agent, staged)
        info(f"已暂存 {agent.name} → {staged.relative_to(ROOT)}")
    else:
        warn("未找到 dist/Vault-Agent.exe，客户端会因找不到本地代理而进入降级模式")

    proc = run([pnpm, "--filter", "@vault/client", "run", "package"],
               cwd=FRONTEND_DIR, capture=False, check=False,
               env=_electron_env())
    app_dir = find_client_app_dir()
    if proc.returncode == 0 and is_complete_app_dir(app_dir):
        return app_dir

    # 首次失败最常见的原因：release/win-unpacked 里的 app.asar 正被**正在运行的客户端**占用
    #（客户端以管理员权限运行，构建脚本无权结束它）。换一个**未被占用**的输出目录重试，
    # 绝不拿残缺目录凑数。
    warn("electron-builder 首次打包失败，改用备用输出目录重试")
    warn("常见原因：release/win-unpacked 里的 app.asar 正被**正在运行的客户端**占用"
         "（请先退出从 release* 目录启动的 Vault.exe）")
    tried = False
    for name in _fallback_output_dirs():
        alt_out = CLIENT_APP_DIR / name
        if not _dir_available(alt_out):
            warn(f"跳过被占用的输出目录：{name}（有客户端正在该目录里运行）")
            continue
        tried = True
        info(f"重试输出目录：{name}")
        retry = run([pnpm, "exec", "electron-builder",
                     "--config", "electron-builder.yml",
                     "-c.directories.output", str(alt_out)],
                    cwd=CLIENT_APP_DIR, capture=False, check=False,
                    env=_electron_env())
        if retry.returncode == 0:
            alt_app = alt_out / "win-unpacked"
            if is_complete_app_dir(alt_app):
                global _client_app_dir
                _client_app_dir = alt_app
                ok(f"客户端程序目录（备用输出）：{alt_app.relative_to(ROOT)}")
                return alt_app
            warn(f"备用输出目录 {name} 的产物不完整，继续尝试下一个")
    if not tried:
        fail("所有备用输出目录都被运行中的客户端占用，未能打包客户端")
    error_msgs = (
        "客户端打包失败：没有可用的程序目录。",
        "请先退出所有从 frontend/apps/client/release* 目录启动的客户端（含管理员权限实例），"
        "或手工执行：cd frontend; pnpm --filter @vault/client run package",
    )
    for msg in error_msgs:
        fail(msg)
    if app_dir is not None and not is_complete_app_dir(app_dir):
        warn(f"现有目录 {app_dir.relative_to(ROOT)} 不完整（缺文件），已拒绝用它打包")
    warn("本次不会产出客户端部署包（避免发布一个跑不起来的客户端）")
    return None


def build_client_web_artifact(version: str) -> Path | None:
    """把前端渲染层产物（Vite 产出的 dist/）打成 zip（client_web 产物的载体）。

    只打 index.html 与 assets/** 等相对路径内容；解压后即为渲染层根目录。
    产物命名 Vault-Web-<version>.zip（符合 updatepkg 的产物名白名单 [A-Za-z0-9._-]）。
    """
    dist = CLIENT_APP_DIR / "dist"
    if not (dist / "index.html").is_file():
        warn(f"未找到渲染层产物 {dist / 'index.html'}，本次不生成 client_web 产物")
        return None
    DIST_DIR.mkdir(parents=True, exist_ok=True)
    out = DIST_DIR / f"Vault-Web-{version}.zip"
    if out.exists():
        out.unlink()
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zf:
        for path in sorted(dist.rglob("*")):
            if path.is_file():
                zf.write(path, path.relative_to(dist).as_posix())
    ok(f"渲染层资源包：{out.name}（{out.stat().st_size:,} 字节）")
    return out


def _manifest_artifact_urls(manifest: dict) -> list[str]:
    """返回清单中 artifacts[] 实际引用的 url 列表（如 artifacts/Vault-Agent.exe）。"""
    urls: list[str] = []
    for art in manifest.get("artifacts") or []:
        url = str(art.get("url") or "").replace("\\", "/").strip()
        if url:
            urls.append(url)
    return urls


def sync_release_dir(dst_dir: Path, src_dir: Path, manifest: dict) -> list[tuple[str, int]]:
    """把发布目录内容同步到 dst_dir：**只复制清单实际引用的产物**，并删除不再被引用的旧产物。

    为什么要"只复制引用的产物 + 删除旧产物"：发布目录里可能残留历史版本的大文件（例如
    一个不再被任何 manifest 引用的旧安装包），若整体复制会把它们一并塞进服务端包/镜像目录，
    造成体积膨胀与误导。这里以"本次清单"为唯一真源。

    返回 [(相对 url, 字节数)]，供打包输出展示内置产物清单。
    """
    dst_dir.mkdir(parents=True, exist_ok=True)
    for name in ("manifest.json", "manifest.json.sig"):
        src = src_dir / name
        if src.is_file():
            shutil.copy2(src, dst_dir / name)

    urls = _manifest_artifact_urls(manifest)
    # 名字比较必须走 os.path.normcase：Windows 文件系统大小写不敏感，
    # 若用原始字符串比较，"Vault-Agent.exe" 与磁盘上的 "vault-agent.exe" 会被判为不同文件，
    # 从而把**刚复制好的**产物当成"未被引用的旧产物"删掉（这正是曾经的 bug）。
    wanted = {os.path.normcase(u.rsplit("/", 1)[-1]) for u in urls}
    dst_artifacts = dst_dir / "artifacts"
    dst_artifacts.mkdir(parents=True, exist_ok=True)

    # 先清理不再被本次清单引用的旧产物，再复制——顺序很重要：
    # 反过来的话，清理步骤可能删掉刚刚复制进来的文件（大小写差异时）。
    for item in sorted(dst_artifacts.iterdir()):
        if item.is_file() and os.path.normcase(item.name) not in wanted:
            info(f"删除本次清单不再引用的旧产物：artifacts/{item.name}")
            item.unlink()

    copied: list[tuple[str, int]] = []
    for url in urls:
        src_file = src_dir / url
        if not src_file.is_file():
            # 兜底：清单里写的名字与磁盘上的实际大小写不一致时（Windows 上很常见），
            # 在源目录里按大小写不敏感再找一次。
            src_file = _find_artifact_ci(src_dir / "artifacts", url.rsplit("/", 1)[-1])
        if src_file is None or not src_file.is_file():
            warn(f"清单引用的产物缺失，已跳过：{url}")
            continue
        shutil.copy2(src_file, dst_dir / url)
        copied.append((url, src_file.stat().st_size))
    return copied


def _find_artifact_ci(artifacts_dir: Path, name: str) -> Path | None:
    """在产物目录里按**大小写不敏感**查找同名文件；找不到返回 None。"""
    if not artifacts_dir.is_dir():
        return None
    target = os.path.normcase(name)
    for item in artifacts_dir.iterdir():
        if item.is_file() and os.path.normcase(item.name) == target:
            return item
    return None


def make_update_package(version: str, channel: str, out_dir: Path, args) -> bool:
    """生成并签名更新包，然后复制到服务端 artifacts_dir。"""
    step("生成客户端更新包")
    server_exe = DIST_DIR / "Vault-Server.exe"
    if not server_exe.is_file():
        die(f"缺少 {server_exe}（Vault-Server 未构建成功）")

    agent_exe = DIST_DIR / "Vault-Agent.exe"
    if not agent_exe.is_file():
        die(f"缺少 {agent_exe}，无法生成 agent 更新产物")

    key_path = ensure_signing_keys(server_exe, args.key, allow_generate=not args.no_keygen)
    public_key = read_public_key()

    if RELEASE_DIR.exists():
        shutil.rmtree(RELEASE_DIR, ignore_errors=True)

    cmd = [
        str(server_exe), "sign", "release",
        "-dir", str(RELEASE_DIR),
        "-channel", channel,
        "-version", version,
        "-key", str(key_path),
        "-artifact", f"agent={agent_exe}",
    ]
    if args.notes:
        cmd += ["-notes", args.notes]
    if args.min_supported_version:
        cmd += ["-min-supported-version", args.min_supported_version]

    client_artifact = None
    if args.with_client:
        app_dir = build_client_app(args)
        if app_dir:
            # 单文件形态（portable / 安装包）才适合放进更新清单；
            # 目录形态（win-unpacked）不作为更新产物，随客户端 zip 整体分发。
            single = [p for p in sorted((CLIENT_APP_DIR / "release").glob("*.exe"))]
            if single:
                client_artifact = single[0]
    if client_artifact:
        cmd += ["-artifact", f"client={client_artifact}"]
    else:
        info("更新清单中不含客户端产物（仅含 agent；Electron 本体更新尚未接入）")

    # 渲染层资源包（client_web）：热更 Electron 的渲染层（Vite 产出的 dist/），不替换本体。
    web_artifact = build_client_web_artifact(version)
    if web_artifact:
        cmd += ["-artifact", f"client_web={web_artifact}"]

    run(cmd, quiet=True)

    # 发布前自检：签名与产物 sha256 必须一致
    verify = run([str(server_exe), "sign", "verify", "-dir", str(RELEASE_DIR), "-pub", str(PUBLIC_KEY_FILE)],
                 check=False, quiet=True)
    if verify.returncode != 0:
        fail("发布目录自检失败，已中止（不复制到服务端目录）")
        if verify.stdout:
            print(verify.stdout)
        if verify.stderr:
            print(verify.stderr, file=sys.stderr)
        return False
    ok("发布目录自检通过（签名 + 产物 sha256）")

    # 同步到服务端镜像目录：只复制本次清单实际引用的产物，并清理不再引用的旧产物。
    manifest = json.loads((RELEASE_DIR / "manifest.json").read_text(encoding="utf-8"))
    copied = sync_release_dir(out_dir, RELEASE_DIR, manifest)
    ok(f"更新包已就绪：{out_dir.relative_to(ROOT) if out_dir.is_relative_to(ROOT) else out_dir}")
    info(f"version={manifest.get('version')}  channel={manifest.get('channel')}  "
         f"artifacts={len(manifest.get('artifacts') or [])}")
    if copied:
        info("本次分发目录内容：")
        for url, size in copied:
            info(f"  {url:<44} {size:>12,} 字节")
    info("把该目录内容放到服务端的 update.artifacts_dir 即可对外分发")
    return True


# ---------------------------------------------------------------------------
# 部署打包
# ---------------------------------------------------------------------------

PACKAGE_DIR = DIST_DIR / "package"

# 服务端 zip 内的目录名模板（Windows）
SERVER_PKG_DIRNAME = "vault-server-{version}-win64"

# 服务端 tar.gz 内的目录名模板（Linux）。
#
# 打包形态刻意不同：Linux 侧用 tar.gz —— 它**保留可执行权限与符号链接**，
# 可执行文件解压出来即可直接运行；zip 在部分解压工具下会丢掉 +x。
SERVER_PKG_DIRNAME_LINUX = "vault-server-{version}-linux-amd64"

# Linux 包内的可执行文件名（不带 .exe，小写便于命令行使用）。
LINUX_PKG_BIN_NAME = "vault-server"

DEPLOY_README = """Vault 服务端部署说明
============================================================
版本：{version}
构建时间：{built_at}
目标平台：Windows Server 2016+ / Windows 10+（64 位）

------------------------------------------------------------
一、这个包是什么
------------------------------------------------------------
本包只需要**一个可执行文件**即可运行服务端：

    Vault-Server.exe      服务端主程序（同时内含发布签名工具 sign 子命令）

它负责：VHDX 管理、iSCSI 目标管理、存储库与用户管理、REST API 与 SSE。
服务端**不承载任何网页**（管理界面已合并进客户端应用），因此：

    ★ 首次使用必须先取得客户端，用客户端完成初始化设置。
      （没有客户端时将无法初始化，这是刻意的设计取舍）

------------------------------------------------------------
二、环境要求
------------------------------------------------------------
1. 操作系统：Windows Server 2016 或更高（64 位）
2. 必须安装的 Windows 功能：
     - iSCSI 目标服务器（FS-iSCSITarget-Server）
     - iSCSI 目标存储提供程序（VDS 与 VSS 硬件提供程序）
       ------------------------------
       PowerShell（管理员）执行：
         Install-WindowsFeature FS-iSCSITarget-Server
       或用服务器管理器 → 添加角色和功能 → 文件和存储服务 →
       iSCSI 目标服务器
       ------------------------------
3. 启动服务的账号必须是**本机管理员**（创建/挂载 VHDX、管理 iSCSI 目标需要）。
4. 准备一块数据盘用于存放 VHDX，例如 D:\\VaultData（建议独立磁盘，不要用系统盘）。

------------------------------------------------------------
三、部署步骤
------------------------------------------------------------
1) 把本目录整体复制到服务器上，例如 C:\\Vault\\

2) 创建数据目录（与下面配置里的 storage.whitelist_root 保持一致）：
      New-Item -ItemType Directory -Force D:\\VaultData

3) 生成并确认配置：
   本包**故意只带配置模板 config.example.yaml，不带 config.yaml**，
   这样以后升级解压不会覆盖你已经改好的配置文件。

   方式一（推荐，先改好再启动）：
        copy config.example.yaml config.yaml
      然后编辑 config.yaml，重点改 storage.whitelist_root。

   方式二（直接启动，自动生成）：
        .\\Vault-Server.exe -config .\\config.yaml
      → 检测到没有 config.yaml 时，会自动从同目录的 config.example.yaml
        生成一份并**继续启动**（生成后会提示你去确认 whitelist_root）。

   需要关注的配置项：
      storage.whitelist_root     "D:\\\\VaultData"   ← 必须改成你的数据目录
      http.listen                "0.0.0.0:8443"      ← 默认即可；只本机访问写 127.0.0.1:8443
      http.tls.enabled           true                ← 证书无需准备，首次启动自动签发
      database.driver            sqlite              ← 单机推荐；要 MySQL 改 mysql 并填 dsn
      security.bootstrap_enabled true                ← 允许用客户端一次性初始化，完成后自动关闭
      security.super_admin_enabled true              ← 设 false 可让超管立即无法登录（锁定恢复用）
      update.artifacts_dir       updates             ← 客户端更新分发目录（不用可忽略）
      log.dir                    logs                ← 按天切分

4) 首次试运行（可跳过，直接用第 5 步的启动器）：
   想看到启动日志时可临时在命令行里跑：
      .\\Vault-Server.exe -config .\\config.yaml
   正常会看到：
      "HTTP 服务已启动（TLS）" listen=0.0.0.0:8443
   首次启动会自动补写 server.instance_id 与 security.master_key，并生成 pki\\ 证书。
   ⚠️ security.master_key 用于加密 CHAP 密钥，**丢失后已配置的 iSCSI 鉴权将无法解密**，
      请务必备份 config.yaml（该文件请勿随升级包一起覆盖）。

5) 日常启动（推荐）：**双击【Vault-Server.exe】即为启动器**
   它会：检查服务端是否已在运行 → 若已在运行则提示"已经在运行中"后退出
   （不会重复启动第二个实例）→ 否则以**无控制台的后台进程**方式启动服务端 →
   探测监听端口直到就绪 → 打印启动摘要（含后台进程 PID）并提示"已在后台启动"。

   看完提示后**按回车键关闭该窗口即可，服务端在后台继续运行**，
   **不会留下任何控制台黑框**。

   停止服务（优雅停机，与 Ctrl+C 等价）：
   在本目录打开命令行执行：
      .\\Vault-Server.exe stop

   ★ 两种启动方式都可用，但行为不同（按需选择）：
     - 直接双击 Vault-Server.exe ：走上面的启动器路径，服务端在**后台**运行；
     - 命令行   Vault-Server.exe -config .\\config.yaml  ：**前台**运行，
       不派生任何后台进程，启动横幅与排障日志直接可见，按 Ctrl+C 停止。
       （排查启动问题时推荐这种方式）

6) 注册为开机自启（可选，见第五节），然后客户端连上来做初始化。

------------------------------------------------------------
四、目录说明（首次启动后）
------------------------------------------------------------
    Vault-Server.exe      主程序（★ 双击即为启动器；`Vault-Server stop` 优雅停机）
    config.example.yaml   配置模板（随包提供，不要改它）
    config.yaml           实际配置（首次启动自动生成，含自动生成的密钥，注意备份权限）
    pki\\                  自建 CA 与服务端证书（自动生成）
    data\\                 SQLite 数据库
    logs\\                 日志（按天切分）
    scripts\\              开机自启脚本（计划任务方式）
    updates\\              客户端更新分发目录（可选）
    D:\\VaultData\\          VHDX 存储（disks\\ / staging\\ / meta\\）

------------------------------------------------------------
五、开机自启 / 后台运行
------------------------------------------------------------
注意：**服务端程序未实现 Windows 服务（SCM）接口**，
      因此不能用 sc.exe create 注册为"服务"（会报 1053 错误）。
      请用随包提供的计划任务脚本（等价效果，且无需登录即可运行）：

    # 管理员 PowerShell，在本目录下执行：
    powershell -ExecutionPolicy Bypass -File .\\scripts\\install-autostart.ps1

    取消自启：
    powershell -ExecutionPolicy Bypass -File .\\scripts\\uninstall-autostart.ps1

查看/管理：
    Get-ScheduledTask -TaskName VaultServer
    Stop-ScheduledTask -TaskName VaultServer
    Start-ScheduledTask -TaskName VaultServer

------------------------------------------------------------
六、客户端更新分发（本包已内置）
------------------------------------------------------------
服务端不参与更新包签名，只做"存储 + 分发"。
★ 本包已在 updates\\ 目录内**内置本次构建的更新包**
  （updates\\manifest.json、updates\\manifest.json.sig、updates\\artifacts\\...），
  服务端开箱即可对外分发，**无需任何手工拷贝**。
  只要 config.yaml 中 update.artifacts_dir 指向包内 updates\\（默认即为 "updates"，
  相对路径按**配置文件所在目录**解析），客户端即可检查并下载更新。

若要替换为新的发布内容（在发布方机器上操作，随后整体覆盖服务器的 updates\\ 目录）：
  1) 用本 exe 的 sign 子命令生成已签名的发布目录：
       Vault-Server.exe sign keygen  -out-dir .\\keys
       Vault-Server.exe sign release -dir .\\release -channel stable ^
           -version {version} -key .\\keys\\update-private.pem ^
           -artifact agent=.\\dist\\Vault-Agent.exe ^
           -artifact client_web=.\\dist\\Vault-Web-{version}.zip
       Vault-Server.exe sign verify  -dir .\\release -pub .\\keys\\update-public.txt
  2) 把 release 目录的**内容**（manifest.json、manifest.json.sig、artifacts\\）
     **整体覆盖**到本目录的 updates\\ 下（保持结构，不要只替换其中一部分）。
     ⚠️ 覆盖后不要做任何编辑/格式化/改行尾，否则签名会失效。
  ⚠️ 私钥（update-private.pem）**绝不能**放到服务器上。

------------------------------------------------------------
七、排障
------------------------------------------------------------
Q: 启动报"单实例锁获取失败"
A: 已有一个 Vault-Server 在运行。查：Get-Process Vault-Server

Q: 客户端提示"该服务端已关闭版本检查" / 版本不兼容
A: 看 config.yaml 的 client_compat 段。enabled:false 表示关闭了客户端版本区间检查
   （协议版本仍强制校验）；min/max 显式写错会导致启动失败（这是刻意的 fail-fast）。

Q: 客户端连不上
A: 1) 确认服务端在跑：Get-Process Vault-Server
   2) 确认监听：netstat -ano | findstr :8443
   3) 确认防火墙放行 8443：New-NetFirewallRule -DisplayName "Vault" -Direction Inbound -LocalPort 8443 -Protocol TCP -Action Allow
   4) 客户端首次连接需要核对证书指纹（自建 CA，属正常流程）

Q: 日志在哪
A: logs\\ 目录，按天切分。

------------------------------------------------------------
八、卸载
------------------------------------------------------------
1) 停止：先取消自启（第五节），再优雅停止服务端（.\\Vault-Server.exe stop）；
   若确认无响应可强制结束：Stop-Process -Name Vault-Server
2) 删除本目录
3) 按需删除数据目录（D:\\VaultData）
⚠️ 删除数据目录会**永久丢失所有 VHDX 与用户数据**。
"""

UPDATES_DIR_README = """此目录用于存放"客户端更新包"，供服务端对外分发。

★ 本包已内置本次构建的更新包（manifest.json / manifest.json.sig / artifacts/），
  服务端开箱即可对外分发，**无需手工拷贝**。

服务端对更新包只做原样镜像（不解析、不改写、不重算摘要），
更新包的信任链完全由发布方签名保证。

如需替换为新的发布内容：
  1) 用**新的发布目录内容整体覆盖本目录**（保持下列结构，不要只替换其中一部分）：
         updates\\manifest.json
         updates\\manifest.json.sig
         updates\\artifacts\\Vault-Agent.exe
         updates\\artifacts\\Vault-Web-<版本>.zip
  2) 确认 config.yaml 中：
         update.artifacts_dir: "updates"      ← 相对路径按**配置文件所在目录**解析
         update.channel: "stable"             ← 必须与 manifest.json 里的 channel 一致
  3) 重启服务端（或保存 config.yaml 触发热重载）。

⚠️ 不要手工编辑 manifest.json，也不要做格式化/行尾转换，否则签名会失效，所有客户端都会更新失败。
⚠️ 不要把发布方私钥（update-private.pem）复制到服务器上。

客户端随后即可检查并下载更新；更新包会经过四重校验（发布方签名 → SHA256 →
版本单调性 → 兼容区间），任一失败都会拒绝安装。
"""

# 未内置更新包时的说明（避免写出"已内置"的误导文案）。
UPDATES_DIR_README_EMPTY = """此目录用于存放"客户端更新包"，供服务端对外分发。

⚠️ 本次打包**未内置**更新包（未生成/未找到已签名的发布目录），因此服务端此刻无法对外分发更新。
   取得已签名的发布目录后，把其**内容整体**放到本目录（保持下列结构），
   再重启服务端（或保存 config.yaml 触发热重载）：
         updates\\manifest.json
         updates\\manifest.json.sig
         updates\\artifacts\\Vault-Agent.exe
         updates\\artifacts\\Vault-Web-<版本>.zip

手动生成发布目录（在发布方机器上，需要发布方私钥）：
  Vault-Server sign keygen  -out-dir .\\keys
  Vault-Server sign release -dir .\\release -channel stable -version <版本> ^
      -key .\\keys\\update-private.pem ^
      -artifact agent=.\\dist\\Vault-Agent.exe ^
      -artifact client_web=.\\dist\\Vault-Web-<版本>.zip
  Vault-Server sign verify  -dir .\\release -pub .\\keys\\update-public.txt

⚠️ 不要手工编辑 manifest.json，也不要做格式化/行尾转换，否则签名会失效。
⚠️ 不要把发布方私钥（update-private.pem）复制到服务器上。
"""

INSTALL_AUTOSTART = """# 注册 Vault 服务端为开机自启的计划任务。
#
# 为什么不注册成 Windows"服务"？
#   服务端程序未实现 SCM（服务控制管理器）接口，用 sc.exe create 注册后
#   启动会报 "1053 服务没有及时响应启动或控制请求"。计划任务可以达到
#   等价效果（开机启动、无需登录、以 SYSTEM 运行），且不依赖额外软件。
#
# 用法（必须在**管理员** PowerShell 中执行）：
#   powershell -ExecutionPolicy Bypass -File .\\scripts\\install-autostart.ps1
#   可选参数：-TaskName VaultServer -ConfigPath <config.yaml 的绝对路径>

[CmdletBinding()]
param(
    [string]$TaskName = "VaultServer",
    [string]$ConfigPath = ""
)

$ErrorActionPreference = "Stop"

# 脚本位于 <包根>\\scripts\\，因此包根是上一级目录
$Root = Split-Path -Parent $PSScriptRoot
$Exe = Join-Path $Root "Vault-Server.exe"

if (-not (Test-Path $Exe)) {
    throw "找不到 Vault-Server.exe：$Exe（请把整个目录一起复制过去）"
}

if ([string]::IsNullOrWhiteSpace($ConfigPath)) {
    $ConfigPath = Join-Path $Root "config.yaml"
}
if (-not (Test-Path $ConfigPath)) {
    throw "找不到配置文件：$ConfigPath"
}

# 需要管理员权限才能注册以 SYSTEM 身份运行的任务
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principalCheck = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principalCheck.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "请以管理员身份运行 PowerShell 后再执行本脚本。"
}

if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
    Write-Host "已存在同名任务，先移除旧任务：$TaskName"
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
}

$action = New-ScheduledTaskAction -Execute $Exe `
    -Argument "-config `"$ConfigPath`"" -WorkingDirectory $Root
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -StartWhenAvailable -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) `
    -ExecutionTimeLimit ([TimeSpan]::Zero)

# 以 SYSTEM 运行：无需用户登录，且具备管理 VHDX / iSCSI 所需的权限
$principal = New-ScheduledTaskPrincipal -UserId "SYSTEM" `
    -LogonType ServiceAccount -RunLevel Highest

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Settings $settings -Principal $principal `
    -Description "Vault 存储服务端（开机自启，无需登录）" | Out-Null

Write-Host ""
Write-Host "已注册计划任务：$TaskName" -ForegroundColor Green
Write-Host "  可执行文件：$Exe"
Write-Host "  配置文件  ：$ConfigPath"
Write-Host ""
Write-Host "立即启动： Start-ScheduledTask   -TaskName $TaskName"
Write-Host "查看状态： Get-ScheduledTask     -TaskName $TaskName"
Write-Host "停止：     Stop-ScheduledTask    -TaskName $TaskName"
Write-Host ""
Write-Host "提示：任务以 SYSTEM 身份运行，可执行文件与配置应放在 SYSTEM 有权访问的路径"
Write-Host "      （例如 C:\\Vault\\）。不要放在某个用户的重定向目录下。"
"""

UNINSTALL_AUTOSTART = """# 取消 Vault 服务端的开机自启计划任务。
#
# 用法（管理员 PowerShell）：
#   powershell -ExecutionPolicy Bypass -File .\\scripts\\uninstall-autostart.ps1
#   可选参数：-TaskName VaultServer

[CmdletBinding()]
param(
    [string]$TaskName = "VaultServer"
)

$ErrorActionPreference = "Stop"

$task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
if (-not $task) {
    Write-Host "未找到计划任务：$TaskName（无需处理）"
    exit 0
}

Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false

Write-Host "已移除计划任务：$TaskName" -ForegroundColor Green
Write-Host ""
Write-Host "若服务端进程仍在运行，可手动结束："
Write-Host "  Get-Process Vault-Server | Stop-Process"
"""

# ---------------------------------------------------------------------------
# Linux 部署包内文本（说明 / systemd unit / 安装脚本）
# ---------------------------------------------------------------------------
# ⚠️ 这些文件一律用 **UTF-8 无 BOM + LF** 写出（见 _write_text_unix）：
#    shell 脚本带 BOM 会直接执行失败，CRLF 会让 #!/bin/sh 找不到解释器。

DEPLOY_README_LINUX = """Vault 服务端部署说明（Linux）
============================================================
版本：{version}
构建时间：{built_at}
目标平台：Linux x86_64（64 位）

------------------------------------------------------------
一、这个包是什么
------------------------------------------------------------
本包只需要**一个可执行文件**即可运行服务端：

    vault-server      服务端主程序（同时内含发布签名工具 sign 子命令）

它负责：LVM thin 卷管理、LIO iSCSI 目标管理、存储库与用户管理、REST API 与 SSE。
服务端**不承载任何网页**（管理界面已合并进客户端应用），因此：

    ★ 首次使用必须先取得客户端，用客户端完成初始化设置。

本包是 Windows 包的**对等产物**：同一版本、同一套 API，只是平台后端不同——
    · Windows：VHDX 文件 + IscsiTarget 角色
    · Linux  ：LVM thin LV（差异盘 = thin snapshot）+ LIO（写经 targetcli、读直读 configfs）

------------------------------------------------------------
二、环境要求
------------------------------------------------------------
1. 必须以 **root** 运行（LVM、configfs、挂载都需要）。
2. 内核需支持 LIO（target_core_mod / iscsi_target_mod / target_core_iblock），并挂载 configfs：
       modprobe target_core_mod
       modprobe iscsi_target_mod
       modprobe target_core_iblock                    # 缺它建盘必失败（backstore 插件）
       mount -t configfs none /sys/kernel/config      # 多数发行版已默认挂载
   需要 targetcli —— 它是 iSCSI 目标的**执行体**（写操作全部经它下发到 configfs），不是可选包装；
  读与复验则直读 configfs。targetcli 自身依赖 Python3，装好发行版包即可，服务端不复用 rtslib。
   ⚠️ 注意 lsmod 里有 iscsi_target_mod ≠ configfs 里已注册成功，
      要看 /sys/kernel/config/target/iscsi 是否存在——两者 doctor 会分别检查。
3. 需要 LVM2 工具链（lvm2 包）：pvcreate / vgcreate / lvcreate / lvs / thin 相关命令。
4. 需要文件系统工具：e2fsprogs（mkfs.ext4）或 xfsprogs（mkfs.xfs）。
5. 准备一块 **独立的数据盘** 用作 LVM PV（不要用系统盘），例如 /dev/sdb。

★ 上面第 2~4 项**服务端会自己检测**，默认还会自动补齐：
     · 自动修复：挂载 configfs、加载/重载 LIO 内核模块、写 /etc/modules-load.d/vault-lio.conf；
     · 自动安装：缺 lvm2/e2fsprogs 等工具时按发行版调 apt-get/dnf/yum/zypper/apk/pacman 装。
  想让 Ansible/镜像构建等外部工具接管依赖，或处在离线环境，
  把 config.yaml 里 platform.auto_repair / platform.auto_install 设为 false（只告警不改动）。
  无论开关如何，随时可手工自检（这才是部署新机器的第一步）：
       ./vault-server doctor                  # 探测 + 自动修复，逐项给出结果与处置命令
       ./vault-server doctor -json            # JSON 输出，供脚本/CMDB 消费
       ./vault-server doctor -install=false   # 只探测不装包（报告里给人工安装命令）

  ⚠️ doctor 修不了"内核根本没提供模块文件"这类问题（模块文件不存在时 modprobe 只会报
    Module not found）：Debian/Ubuntu 需要 linux-modules-extra-$(uname -r)，RHEL 系需要
    kernel-modules-extra，Alpine 需要与运行内核匹配的 linux-lts 等。也修不了"连包管理器
    都没有"的最小化系统。这两种情况用包内的依赖安装脚本兜底（幂等，可反复执行）：
       sudo ./scripts/setup.sh                # 装齐全部依赖（apt/dnf/yum/zypper/apk/pacman 通用）
       sudo ./scripts/setup.sh --dry-run      # 只打印它将要执行的命令，不改动系统

------------------------------------------------------------
三、部署步骤
------------------------------------------------------------
1) 解压到部署目录，例如 /opt/vault/：
       tar -xzf vault-server-{version}-linux-amd64.tar.gz
       cd vault-server-{version}-linux-amd64
       chmod +x vault-server                    # 若解压后丢失了可执行权限

   ★ 解压后先装依赖（一条命令，任何发行版通用；已装过的项会跳过）：
       sudo ./scripts/setup.sh
     它做三件事：装齐软件包（lvm2/e2fsprogs/…）、必要时补装内核模块包并加载 LIO 三个模块、
     挂载 configfs；末尾复检并打印"仍未就绪"与诊断（退出码 0 = 必需项全部就绪）。

   ★ 再跑一次服务端自检（十秒钟）——它会挂载 configfs、加载 LIO 三个内核模块、
     按需安装缺的命令行工具，并逐项打印结果（退出码 0 = 必需项全部就绪）：
       ./vault-server doctor
     机器不允许自动改动时（离线 / 由配置管理工具接管）用：
       ./vault-server doctor -fix=false -install=false      # 只探测，给出人工处置命令
     其中"缺内核模块"类问题 doctor 修不了，交给上面的 setup.sh（它只管装，不做探测之外的改动）。

2) 准备 LVM 卷组（两种方式，选其一）：
   方式一（推荐，在管理端做）：先按第 3、4 步起服务，然后在客户端
        「系统设置 → LVM 存储池」里用块设备选择器初始化 VG + thin pool。
   方式二（命令行预建）：
       pvcreate /dev/sdb
       vgcreate vg0 /dev/sdb

3) 生成配置：
   本包**故意只带配置模板 config.example.yaml，不带 config.yaml**，
   这样以后升级解压不会覆盖你已经改好的配置文件。

       # 直接启动即可（推荐）：检测不到 config.yaml 时会自动从模板生成一份并继续启动，
       # 且会按 Linux 改写模板里的存储相关默认值（storage.whitelist_root → /var/lib/vault）。
       ./vault-server

   ⚠️ 若你更想"先改好再启动"，也可以 `cp config.example.yaml config.yaml`，
      但**必须**把 storage.whitelist_root / whitelist_roots 改成 Linux 绝对路径：
      模板是两个平台共用的一份，里面写的是 Windows 的 D:\\VaultData，
      它在 Linux 上不是绝对路径，服务端会直接拒绝启动（报"每个根都必须是绝对路径"）。

4) Linux 侧需要关注的配置项（与 Windows 包差异最大之处）：
      platform.kind              "auto"            ← 跑在 Linux 上即为 linux
      platform.lvm.vg            "vg0"             ← 卷组名，**不得含 '-'**
      platform.lvm.thin_pool     "vault"           ← thin pool 名，**不得含 '-'**
      platform.iscsi.backend     "lio"
      platform.iscsi.configfs_root  "/sys/kernel/config/target"
      platform.auto_repair       true              ← 启动时自动修复依赖缺项（挂载/模块/持久化）
      platform.auto_install      true              ← 缺失 lvm2 等工具时自动装包（离线环境设 false）
      http.listen                "0.0.0.0:8443"
      http.tls.enabled           true              ← 证书无需准备，首次启动自动签发
      database.driver            sqlite
      security.bootstrap_enabled true              ← 允许用客户端一次性初始化

   ★「存储」在 Linux 上不是普通目录，而是**受管的 thin LV**：
       config.yaml 的 storage.whitelist_root(s) 在 Linux 上**不会**被种成存储
       （宿主机普通目录会让落盘绕过 thin pool）。
       请在管理端「存储」模块创建：服务端会自动 lvcreate → mkfs → 挂载
       （默认挂载点 /var/lib/vault/storages/<名称>-<短 ID>）。

5) 首次试运行（推荐，日志直接可见）：
       ./vault-server -config ./config.yaml
   正常会看到 "HTTP 服务已启动（TLS）" listen=0.0.0.0:8443。
   首次启动会自动补写 server.instance_id 与 security.master_key，并生成 pki/ 证书。
   ⚠️ security.master_key 用于加密 CHAP 密钥，**丢失后已配置的 iSCSI 鉴权将无法解密**，
      请务必备份 config.yaml（该文件请勿随升级包一起覆盖）。
   按 Ctrl+C 停止（等价于 SIGTERM，走优雅停机）。

------------------------------------------------------------
四、目录说明（首次启动后）
------------------------------------------------------------
    vault-server          主程序
    config.example.yaml   配置模板（不要改它）
    config.yaml           实际配置（首次启动自动生成，含自动生成的密钥，注意备份）
    pki/                  自建 CA 与服务端证书（自动生成）
    data/                 SQLite 数据库
    logs/                 日志（按天切分；另有固定文件 logs/vault-server.log）
    scripts/              依赖安装脚本（setup.sh）与 systemd 托管脚本
    updates/              客户端更新分发目录（可选）

------------------------------------------------------------
五、交给 systemd 托管（推荐的生产用法）
------------------------------------------------------------
    sudo ./scripts/install-systemd.sh            # 默认装在 /opt/vault

安装后：
    systemctl status  vault-server
    systemctl restart vault-server
    systemctl stop    vault-server               # SIGTERM，优雅停机
    journalctl -u vault-server -f

卸载：
    sudo systemctl disable --now vault-server
    sudo rm -f /etc/systemd/system/vault-server.service
    sudo systemctl daemon-reload

⚠️ 服务端**不支持**在 Linux 上自行 daemonize 成"传统守护进程"：
   默认就是**前台运行**（日志直接可见，便于排障），后台由 systemd 负责。
   命令行下也可用 nohup/setsid 自行后台化，但推荐用上面的 unit。

------------------------------------------------------------
六、客户端更新分发（本包已内置）
------------------------------------------------------------
服务端不参与更新包签名，只做"存储 + 分发"。
★ 本包已在 updates/ 目录内**内置本次构建的更新包**
  （updates/manifest.json、updates/manifest.json.sig、updates/artifacts/...），
  服务端开箱即可对外分发，**无需任何手工拷贝**。
只要 config.yaml 中 update.artifacts_dir 指向包内 updates/（默认即为 "updates"），
客户端即可检查并下载更新。

替换更新内容时，把**新的发布目录内容整体覆盖** updates/（保持结构，不要只替换一部分），
然后重启服务端（或保存 config.yaml 触发热重载）。
⚠️ 不要手工编辑 manifest.json，也不要做格式化/行尾转换，否则签名会失效。
⚠️ 私钥（update-private.pem）**绝不能**放到服务器上。

------------------------------------------------------------
七、排障
------------------------------------------------------------
Q: 启动报"单实例锁获取失败"
A: 已有一个 vault-server 在运行：pgrep -af vault-server

Q: 日志报 "LVM 工具链不可用" / "LIO configfs 不可用" / "系统依赖自检未通过"
A: 先跑 ./vault-server doctor —— 它会逐项说明缺什么、能自动修的当场修掉
   （mount configfs、modprobe/重载内核模块、按需装包），修不了的把「处置」命令打出来照做即可。
   注意：lsmod 里有 iscsi_target_mod 不等于 configfs 里已注册成功
   （判据是 /sys/kernel/config/target/iscsi 与 /sys/kernel/config/target/core/iblock_0 是否存在），
   doctor 会把"模块未加载""模块在但 fabric 未注册""缺 backstore 插件"分开报。
   这些项只做启动期告警、不阻断启动，但对应功能不可用。

Q: 客户端连不上
A: 1) ss -lntp | grep 8443      确认监听
   2) 放通防火墙：firewall-cmd --add-port=8443/tcp --permanent && firewall-cmd --reload
      （iSCSI 还需放通 3260）
   3) 客户端首次连接需要核对证书指纹（自建 CA，属正常流程）

Q: 日志在哪
A: logs/ 目录（按天切分）；崩溃现场另在 logs/vault-server.log。

------------------------------------------------------------
八、卸载
------------------------------------------------------------
1) systemctl disable --now vault-server（或 pkill -TERM vault-server）
2) 删除本目录
3) 按需清理 LVM：lvremove / vgremove / pvremove
⚠️ 清理 LVM 会**永久丢失所有虚拟磁盘与用户数据**。
"""

LINUX_SYSTEMD_UNIT = """[Unit]
Description=Vault 存储服务端
Documentation=file:///opt/vault/部署说明.txt
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# 必须是 root：LVM、configfs 与挂载都需要特权
User=root
WorkingDirectory=/opt/vault
ExecStart=/opt/vault/vault-server -config /opt/vault/config.yaml
# SIGTERM 即优雅停机（关闭 HTTP、停止任务 worker、释放资源）
KillSignal=SIGTERM
TimeoutStopSec=20s
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
"""

LINUX_INSTALL_SYSTEMD = """#!/bin/sh
# 把 Vault 服务端注册为 systemd 服务（需要在 root 下执行）。
#
# 用法：
#   sudo ./scripts/install-systemd.sh [安装目录]
#   默认安装目录：/opt/vault
#
# 为什么不用"传统守护进程"：服务端默认就是前台运行（日志直接可见、便于排障），
# 后台托管交给 systemd 更可靠（自动拉起、日志进 journald）。

set -eu

SRC_DIR=$(cd "$(dirname "$0")/.." && pwd)
DEST_DIR=${1:-/opt/vault}
SERVICE_NAME=vault-server

if [ "$(id -u)" -ne 0 ]; then
    echo "请以 root 执行（或用 sudo）。" >&2
    exit 1
fi

if [ ! -f "$SRC_DIR/vault-server" ]; then
    echo "找不到 $SRC_DIR/vault-server（请把整个目录一起复制过去）" >&2
    exit 1
fi

# 1) 安装程序与配置模板
mkdir -p "$DEST_DIR"
cp -f "$SRC_DIR/vault-server" "$DEST_DIR/vault-server"
chmod 755 "$DEST_DIR/vault-server"
if [ -f "$SRC_DIR/config.example.yaml" ]; then
    cp -f "$SRC_DIR/config.example.yaml" "$DEST_DIR/config.example.yaml"
fi
# config.yaml 刻意**不在这里预生成**：服务端首次启动会自动从模板生成一份，
# 并按运行平台改写其中的平台专属默认值（Linux 上 storage.whitelist_root → /var/lib/vault）。
# 这里直接 cp 模板会把它原样带过去：模板是 Windows/Linux 共用的一份，
# 其中的 D:\\VaultData 在 Linux 上不是绝对路径，服务端会**直接拒绝启动**。
# 已存在的 config.yaml 绝不覆盖：里面含自动生成的 master_key。
if [ -f "$DEST_DIR/config.yaml" ]; then
    echo "$DEST_DIR/config.yaml 已存在，保持不动（含已生成的密钥）"
else
    echo "首次启动将自动从模板生成 $DEST_DIR/config.yaml"
fi
if [ -d "$SRC_DIR/updates" ]; then
    mkdir -p "$DEST_DIR/updates"
    cp -rf "$SRC_DIR/updates/." "$DEST_DIR/updates/"
fi

# 2) 安装并启用 unit
UNIT_SRC="$SRC_DIR/scripts/vault-server.service"
if [ ! -f "$UNIT_SRC" ]; then
    echo "找不到 $UNIT_SRC" >&2
    exit 1
fi
sed "s#/opt/vault#$DEST_DIR#g" "$UNIT_SRC" > "/etc/systemd/system/$SERVICE_NAME.service"
chmod 644 "/etc/systemd/system/$SERVICE_NAME.service"

systemctl daemon-reload
systemctl enable "$SERVICE_NAME"
systemctl restart "$SERVICE_NAME"

echo
echo "已安装并启动：$SERVICE_NAME"
echo "  安装目录：$DEST_DIR"
echo "  查看状态：systemctl status  $SERVICE_NAME"
echo "  跟踪日志：journalctl -u $SERVICE_NAME -f"
echo "  停止服务：systemctl stop    $SERVICE_NAME"
"""


def _write_text(path: Path, content: str) -> None:
    """统一用 UTF-8 with BOM 写文本：Windows 记事本/PowerShell 5.1 才不会乱码。"""
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8-sig", newline="\r\n")


def _write_text_unix(path: Path, content: str) -> None:
    """用 UTF-8 **无 BOM** + LF 写文本（Linux 包内的脚本与说明专用）。

    BOM 会让 shell 脚本直接执行失败，CRLF 会让 `#!/bin/sh` 找不到解释器，
    因此这里必须与 Windows 侧的文件分开写。
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8", newline="\n")


def _make_targz(pkg_dir: Path, archive_path: Path, exec_names: set[str]) -> Path:
    """把目录打成 tar.gz，并**显式指定每个条目的权限位**。

    ⚠️ 不能用 shutil.make_archive：它把本地 stat() 的结果原样写进 tar，
       而 Windows 文件系统根本没有可执行位（普通文件一律是 0o666），
       这样打出来的包在 Linux 上解压后 `./vault-server` 会直接 permission denied。
       这里统一：目录 0755、可执行文件 0755、其余 0644，属主 root，
       解压即可运行，不依赖解压工具是否"保留权限"。
    """
    archive_path.parent.mkdir(parents=True, exist_ok=True)
    if archive_path.exists():
        archive_path.unlink()
    with tarfile.open(archive_path, "w:gz") as tf:
        # 首条目是包根目录本身：保证解压时该目录以 0755 建立（rglob 不含自身）。
        for path in [pkg_dir] + sorted(pkg_dir.rglob("*")):
            rel = path.relative_to(pkg_dir).as_posix()
            arcname = pkg_dir.name if rel == "." else f"{pkg_dir.name}/{rel}"
            info = tf.gettarinfo(str(path), arcname=arcname)
            info.uid, info.gid = 0, 0
            info.uname, info.gname = "root", "root"
            if path.is_dir():
                info.mode = 0o755
            elif path.name in exec_names:
                info.mode = 0o755
            else:
                info.mode = 0o644
            if path.is_file():
                with path.open("rb") as fh:
                    tf.addfile(info, fh)
            else:
                tf.addfile(info)
    return archive_path


def package_server(version: str, include_updates: bool, linux: bool = False) -> Path | None:
    """把服务端打成可直接拿去部署的包，返回包路径。

    linux=False → Windows zip（dist/package/vault-server-<版本>-win64.zip）
    linux=True  → Linux tar.gz（dist/package/vault-server-<版本>-linux-amd64.tar.gz）

    两个包**同版本、同结构**：主程序 + 配置模板 + 部署说明 + scripts/ + updates/，
    只有"平台专属的三件套"（可执行文件名、部署说明、托管脚本）不同。
    这样发布时两个平台一并产出，不会出现"只有 Windows 包"的漏发。

    包内**绝不包含**：发布方私钥、pki 证书（服务端自动生成）、数据库、日志、虚拟磁盘。
    """
    plat = "linux-amd64" if linux else "win64"
    step(f"打包服务端部署包（版本 {version}，{plat}）")

    if linux:
        src_bin = DIST_DIR / LINUX_SERVER_TARGET
        bin_name = LINUX_PKG_BIN_NAME
        pkg_dir = PACKAGE_DIR / SERVER_PKG_DIRNAME_LINUX.format(version=version)
    else:
        src_bin = DIST_DIR / "Vault-Server.exe"
        bin_name = "Vault-Server.exe"
        pkg_dir = PACKAGE_DIR / SERVER_PKG_DIRNAME.format(version=version)

    if not src_bin.is_file():
        # 不阻断整个打包流程：Windows 包是主产物，Linux 包缺失时只告警。
        warn(f"缺少 {src_bin}，跳过 {plat} 部署包（请先完整构建，去掉 --skip-go）")
        return None

    if pkg_dir.exists():
        shutil.rmtree(pkg_dir, ignore_errors=True)
    pkg_dir.mkdir(parents=True, exist_ok=True)

    # 1) 主程序
    shutil.copy2(src_bin, pkg_dir / bin_name)
    if linux:
        # tar.gz 会保留权限，但解压工具未必；这里显式补齐可执行位。
        (pkg_dir / bin_name).chmod(0o755)

    # 2) 配置模板：包内只放 config.example.yaml，**不放 config.yaml**，
    #    以免升级解压时覆盖运维已修改的配置。服务端首次启动若发现没有
    #    config.yaml，会自动从同目录的 config.example.yaml 生成一份。
    template = CONFIG_EXAMPLE_FILE if CONFIG_EXAMPLE_FILE.is_file() else CONFIG_FILE
    if template.is_file():
        shutil.copy2(template, pkg_dir / "config.example.yaml")
    else:
        warn("未找到配置模板，包内将不含 config.example.yaml")

    # 3) 部署说明与平台脚本
    if linux:
        # 说明文档用无 BOM + LF 写：否则 shell 脚本在 Linux 上无法执行。
        _write_text_unix(pkg_dir / "部署说明.txt", DEPLOY_README_LINUX.format(
            version=version,
            built_at=time.strftime("%Y-%m-%d %H:%M:%S"),
        ))
        _write_text_unix(pkg_dir / "scripts" / "vault-server.service", LINUX_SYSTEMD_UNIT)
        _write_text_unix(pkg_dir / "scripts" / "install-systemd.sh", LINUX_INSTALL_SYSTEMD)
        (pkg_dir / "scripts" / "install-systemd.sh").chmod(0o755)

        # 依赖安装脚本：源文件在仓库根 scripts/setup.sh（不放字面量常量里，避免两处维护）。
        # 必须显式把 CRLF 归一成 LF —— _write_text_unix 只保证"写出去的换行是 \n"，
        # 不会清除内容里已有的 \r；而 Windows 上按 autocrlf 检出的源码可能是 CRLF，
        # 带 \r 的 shell 脚本到 Linux 上会因 `#!/bin/sh\r` 直接执行失败。
        setup_src = ROOT / "scripts" / "setup.sh"
        if setup_src.is_file():
            setup_text = setup_src.read_text(encoding="utf-8").replace("\r\n", "\n").replace("\r", "\n")
            _write_text_unix(pkg_dir / "scripts" / "setup.sh", setup_text)
            (pkg_dir / "scripts" / "setup.sh").chmod(0o755)
        else:
            warn("未找到 scripts/setup.sh，包内将不含依赖安装脚本")
    else:
        _write_text(pkg_dir / "部署说明.txt", DEPLOY_README.format(
            version=version,
            built_at=time.strftime("%Y-%m-%d %H:%M:%S"),
        ))
        _write_text(pkg_dir / "scripts" / "install-autostart.ps1", INSTALL_AUTOSTART)
        _write_text(pkg_dir / "scripts" / "uninstall-autostart.ps1", UNINSTALL_AUTOSTART)

    # 启动器/停止器已内建进服务端程序本身（Windows 双击即启动、stop 子命令优雅停机），
    # Linux 侧由 systemd 托管，包内不再提供任何额外的启动器脚本。

    # 4) 更新分发目录：把**本次构建真实产出的发布目录内容**一并放进包内（开箱即可分发）。
    #    历史上这里只有一份"说明.txt"，要求运维手工把发布目录拷进去，导致开箱部署的
    #    服务端永远没有 manifest.json（客户端报 update.no_manifest/reason=manifest_missing）。
    updates_dir = pkg_dir / "updates"
    updates_dir.mkdir(parents=True, exist_ok=True)
    included: list[tuple[str, int]] = []
    if include_updates:
        src = resolve_update_out_dir(load_config(), None)
        if (src / "manifest.json").is_file():
            manifest = json.loads((src / "manifest.json").read_text(encoding="utf-8"))
            included = sync_release_dir(updates_dir, src, manifest)
            ok(f"已内置本次构建的更新包（version={manifest.get('version')} "
               f"channel={manifest.get('channel')}，产物 {len(included)} 个）")
        else:
            warn(f"{src} 下没有已签名的 manifest.json，包内 updates/ 将只含说明（服务端无法分发更新）")
    else:
        warn("已跳过更新包（--skip-update）；包内 updates/ 只含说明")
    # 说明文档始终写入：即便已内置更新包，也要说明替换方式与安全红线。
    # Linux 包统一走无 BOM + LF 的写法，避免包内混用两种编码/换行。
    writer = _write_text_unix if linux else _write_text
    writer(updates_dir / "说明.txt",
           UPDATES_DIR_README if included else UPDATES_DIR_README_EMPTY)

    # 5) 打包
    PACKAGE_DIR.mkdir(parents=True, exist_ok=True)
    if linux:
        # tar.gz 由本函数生成：显式写权限位（见 _make_targz 的说明）。
        archive_path = _make_targz(pkg_dir, PACKAGE_DIR / f"{pkg_dir.name}.tar.gz",
                                   {bin_name, "install-systemd.sh", "setup.sh"})
    else:
        archive_path = Path(shutil.make_archive(str(PACKAGE_DIR / pkg_dir.name), "zip",
                                                root_dir=PACKAGE_DIR, base_dir=pkg_dir.name))

    size = archive_path.stat().st_size
    sep = "/" if linux else "\\"
    ok(f"服务端部署包（{plat}）：{archive_path}")
    info(f"  大小 {size:,} 字节（解压后目录 {pkg_dir.name}{sep}）")
    info(f"  内容：{bin_name} / config.example.yaml")
    info(f"        / 部署说明.txt / scripts{sep} / updates{sep}")
    if included:
        info("  updates/ 内置本次构建的更新产物（服务端开箱即可分发）：")
        info(f"    updates{sep}manifest.json")
        info(f"    updates{sep}manifest.json.sig")
        for url, art_size in included:
            info(f"    updates/{url:<42} {art_size:>12,} 字节")
    else:
        info("  updates/ 未内置更新产物（仅 说明.txt）")
    info("  ★ 不含 config.yaml（避免覆盖你已改好的配置；服务端首次启动会自动生成）")
    info("  ★ 不含私钥、不含证书（证书由服务端首次启动自动生成）")
    return archive_path


def package_client(version: str, args=None) -> Path | None:
    """把客户端相关产物打成 zip（若客户端程序目录缺失或过期，则明确失败而不发旧界面）。

    args 为 None 时不做"重新打包"（仅校验并拒绝陈旧产物），便于从 package_server 等
    无参上下文调用。
    """
    step(f"打包客户端部署包（版本 {version}）")

    agent = DIST_DIR / "Vault-Agent.exe"
    if not agent.is_file():
        fail(f"缺少 {agent}，请先构建（去掉 --skip-go）")
        return None

    pkg_name = f"vault-client-{version}-win64"
    pkg_dir = PACKAGE_DIR / pkg_name
    if pkg_dir.exists():
        shutil.rmtree(pkg_dir, ignore_errors=True)
    pkg_dir.mkdir(parents=True, exist_ok=True)

    # 客户端 = electron-builder 产出的**可运行程序目录**整体打包。
    # 不做安装包：解压后双击 Vault.exe 即打开界面，无需安装步骤。
    app_dir = find_client_app_dir()
    stale = _client_app_stale(app_dir)
    slim_issues = _client_app_slim_issues(app_dir)
    if stale or slim_issues:
        # 两种"目录不能直接对外"的情况，都必须重新打包：
        #   1. 渲染层已经是新的，但 electron-builder 没重跑 → 目录里是**旧界面**。
        #      直接打包的后果：用户拿到新版本包，界面却丝毫未变（"改了前端毫无变化"）。
        #   2. 目录是**精简之前**留下的产物（例如本次改动之前打的包）：白白多出约 46 MB
        #      （51 个多余语言包 + 9 MB 许可清单）。它不影响运行、不会报错，所以没人会发现，
        #      只能在这里主动识别并重打。
        if stale:
            warn(f"客户端程序目录（{app_dir.relative_to(ROOT)}）早于当前渲染层产物，"
                 "里面是旧界面，重新打包客户端")
        for issue in slim_issues:
            warn(f"客户端程序目录（{app_dir.relative_to(ROOT)}）{issue}，重新打包客户端")
        if args is not None:
            app_dir = build_client_app(args) or app_dir
    if not is_complete_app_dir(app_dir):
        # 宁可不出包，也不出一个"只有代理、没有界面"的伪客户端包：用户拿到后双击什么都没有，
        # 而且体积上看起来像正常产物。这里直接失败并清掉同名残留 zip。
        fail("未找到完整可运行的客户端程序目录（缺少 Vault.exe 等文件），**不会**产出客户端部署包")
        fail("请先退出所有从 frontend/apps/client/release* 启动的客户端，再重新执行构建")
        stale = PACKAGE_DIR / f"{pkg_name}.zip"
        if stale.is_file():
            stale.unlink()
            warn(f"已删除残留的 {stale.name}（内容不完整）")
        return None
    if _client_app_stale(app_dir):
        # 重新打包也没能把界面刷新（通常是运行中的客户端占着 app.asar）。
        # 此时绝不静默发一个旧界面的包——否则就是本轮踩过的坑。
        fail("客户端程序目录仍早于当前渲染层产物，**已跳过客户端包**（避免发出旧界面）")
        fail("请完全退出从 frontend/apps/client/release* 启动的客户端（含托盘），"
             "或手工执行：cd frontend; pnpm --filter @vault/client run package")
        stale = PACKAGE_DIR / f"{pkg_name}.zip"
        if stale.is_file():
            stale.unlink()
            warn(f"已删除残留的 {stale.name}（界面是旧的）")
        return None
    info(f"客户端程序目录：{app_dir.relative_to(ROOT)}"
         f"（界面构建于 {time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(_app_dir_built_at(app_dir)))}）")
    # 体积精简是否真的生效，只能在这里看出来（多出的体积不会报错，只会让包变大）。
    check_client_app_slim(app_dir)
    shutil.copytree(app_dir, pkg_dir, dirs_exist_ok=True)
    # 目录里应已有 Vault-Agent.exe（由 electron-builder.yml 的 extraFiles 放入）；
    # 若缺失（例如复用了旧的产物目录），补一份，保证客户端能找到代理。
    if not (pkg_dir / "Vault-Agent.exe").is_file() and agent.is_file():
        shutil.copy2(agent, pkg_dir / "Vault-Agent.exe")

    lines = [
        "Vault 客户端使用说明",
        "=" * 60,
        f"版本：{version}",
        "",
        "-" * 60,
        "一、怎么启动（重要）",
        "-" * 60,
    ]
    if app_dir:
        lines += [
            "  解压本目录后，**双击 Vault.exe** 即打开客户端界面。",
            "  无需安装，也不会有安装程序。",
            "",
            "  ⚠️ 不要双击 Vault-Agent.exe —— 它是后台代理（负责 iSCSI 连接与磁盘挂载），",
            "     不是界面，由客户端自动在后台拉起，无需手工启动。",
        ]
    else:
        lines += [
            "  ⚠️ 本包内**没有**客户端界面程序（Vault.exe）—— 缺少它服务端将无法完成首次初始化",
            "     （服务端不提供网页初始化入口）。",
            "",
            "     构建方式（在发布方机器上执行，需要联网下载 Electron 运行时）：",
            "         python build.py --package-all",
            "     或手工：",
            "         cd frontend",
            "         pnpm install",
            "         pnpm --filter @vault/client run package",
            "     产物位于 frontend/apps/client/release/win-unpacked/，再重新打包即可带上。",
        ]
    lines += [
        "",
        "-" * 60,
        "二、包含什么",
        "-" * 60,
        "  Vault.exe           客户端界面（双击启动的就是它）",
        "  Vault-Agent.exe     本地代理：iSCSI 连接、磁盘挂载、租约心跳、自更新",
        "  其余 dll / locales / resources   Electron 运行时，请勿删除",
        "",
        "  说明：locales 目录只保留界面支持的 4 种语言（简体中文 / English / 日本語 /",
        "  韓国語）——这是刻意的精简，删掉其余语言包不影响界面文案（界面文字由程序自带，",
        "  与 locales 无关），只是让系统语言不在这 4 种之内的机器上、Chromium 原生控件",
        "  文案回退英文。请勿再手工删减目录里的其他文件。",
        "",
        "-" * 60,
        "三、首次连接服务端",
        "-" * 60,
        "  1) 在客户端中填入服务端地址（例如 https://<服务器IP>:8443）",
        "  2) 首次连接会要求核对服务端证书指纹 —— 这是自建 CA 的正常流程，",
        "     请与服务器上 pki\\server.crt 的指纹比对后确认",
        "  3) 若服务端尚未初始化，客户端会引导你创建超级管理员并完成初始化设置",
        "",
        "-" * 60,
        "四、挂载要求",
        "-" * 60,
        "  · 客户端代理需要**管理员权限**才能连接 iSCSI 磁盘并分配盘符/挂载点：",
        "     如需挂载，请以管理员身份运行 Vault.exe",
        "  · 客户端需能访问服务端的 iSCSI 端口（默认 3260）与 API 端口（默认 8443）",
        "  · 若挂载到目录，目标目录必须已存在且为空",
        "",
        "-" * 60,
        "五、自更新",
        "-" * 60,
        "  · 客户端会向服务端检查更新；更新包由发布方签名，客户端内置公钥验签",
        "  · 有活跃挂载时会拒绝更新（返回 agent.busy_mounts），需先卸载",
        "  · 更新失败连续 3 次会自动回滚到上一版本",
        "",
        "-" * 60,
        "六、开源许可",
        "-" * 60,
        "  · Electron 许可（MIT）全文随包提供：LICENSE.electron.txt",
        "  · Chromium 及其第三方组件的完整许可清单约 9 MB，为控制分发包体积未随包分发；",
        "     获取方式与在线地址见同目录的 THIRD-PARTY-NOTICES.txt，",
        "     也可以从客户端托盘右键菜单的「开源许可」直接打开。",
        "",
    ]
    _write_text(pkg_dir / "使用说明.txt", "\n".join(lines))

    PACKAGE_DIR.mkdir(parents=True, exist_ok=True)
    zip_base = PACKAGE_DIR / pkg_name
    zip_path = Path(shutil.make_archive(str(zip_base), "zip", root_dir=PACKAGE_DIR,
                                       base_dir=pkg_dir.name))

    ok(f"客户端包：{zip_path}")
    info(f"  大小 {zip_path.stat().st_size:,} 字节")
    if app_dir:
        info("  解压后双击 Vault.exe 即启动，无需安装")
    else:
        warn("包内没有客户端界面程序（Vault.exe）")
    return zip_path


def run_package(version: str, want_server: bool, want_client: bool,
                include_updates: bool, args=None) -> list[Path]:
    """执行打包，返回生成的包列表。

    服务端**两个平台一起产出**（Windows zip + Linux tar.gz）：
    Linux 服务端本来就会交叉编译，若只打 Windows 包，发布时必然漏 Linux 部署包。
    """
    results: list[Path] = []
    if want_server:
        for pkg in (package_server(version, include_updates),
                    package_server(version, include_updates, linux=True)):
            if pkg:
                results.append(pkg)
    if want_client:
        z = package_client(version, args)
        if z:
            results.append(z)
    return results


def print_summary(version: str, channel: str, out_dir: Path | None, bumped: bool) -> None:
    step("构建结果")
    info(f"版本：{c(version, C.BOLD)}" + (c("  （本次已递增）", C.YELLOW) if bumped else ""))
    if DIST_DIR.is_dir():
        for exe in sorted(DIST_DIR.glob("*.exe")):
            info(f"  {exe.name:<22} {exe.stat().st_size:>12,} 字节")
        linux_server = DIST_DIR / LINUX_SERVER_TARGET
        if linux_server.is_file():
            info(f"  {linux_server.name:<22} {linux_server.stat().st_size:>12,} 字节")
    if out_dir and (out_dir / "manifest.json").is_file():
        info(f"更新包目录：{out_dir}")
        info(f"分发通道：{channel}")
    print()
    ok("构建完成")


# ---------------------------------------------------------------------------
# 体检
# ---------------------------------------------------------------------------

def run_check(version: str, state: dict) -> int:
    step("环境与版本体检")
    problems = 0

    go = which("go")
    if go:
        proc = subprocess.run([go, "version"], capture_output=True, text=True, **_NO_WINDOW)
        ok(f"Go：{proc.stdout.strip() or go}")
    else:
        try:
            go = go_bin()
            proc = subprocess.run([go, "version"], capture_output=True, text=True, **_NO_WINDOW)
            ok(f"Go：{proc.stdout.strip()}（不在 PATH，构建时自动定位）")
        except SystemExit:
            fail("Go：未找到")
            problems += 1

    for tool, required in (("pnpm", True), ("node", True), ("git", False)):
        path = which(tool)
        if path:
            ok(f"{tool}：{path}")
        elif required:
            fail(f"{tool}：未找到（必需）")
            problems += 1
        else:
            warn(f"{tool}：未找到（可选，仅影响 commit 记录）")

    if CLIENT_PKG_JSON.is_file():
        ok(f"前端版本：{json.loads(CLIENT_PKG_JSON.read_text(encoding='utf-8')).get('version')}")
    ok(f"VERSION 文件：{version}")

    cfg = load_config()
    out_dir = resolve_update_out_dir(cfg, None)
    ok(f"更新包输出目录：{out_dir}")
    ok(f"分发通道：{resolve_channel(cfg)}")

    if PRIVATE_KEY_FILE.is_file():
        ok(f"发布方私钥：{PRIVATE_KEY_FILE}（请确认已离线备份且未入库）")
    else:
        warn("发布方私钥不存在：构建更新包时将自动生成（仅适合联调）")

    client_fp = source_fingerprint(CLIENT_SOURCE_PATHS)
    last_fp = str(state.get("client_fingerprint") or "")
    if not last_fp:
        info("客户端指纹：无历史记录（首次构建将沿用当前版本）")
    elif client_fp == last_fp:
        ok("客户端源码未变化（下次构建不会递增版本）")
    else:
        warn("客户端源码已变化（下次构建将递增版本）")

    cfg_file = CONFIG_FILE if CONFIG_FILE.is_file() else CONFIG_EXAMPLE_FILE
    info(f"读取的配置：{cfg_file.name}")
    info("TLS 证书：由服务端启动时自建 CA 自动签发/维护（构建脚本不参与）")

    return 1 if problems else 0


# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------

def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        prog="build.py",
        description="Vault 项目一体化构建脚本（前端 + Go 产物 + 签名更新包 + 自动版本管理）",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--version", help="显式指定版本号（形如 1.2.3），不自动递增")
    p.add_argument("--bump", choices=["patch", "minor", "major"], default="patch",
                   help="自动递增级别，默认 patch")
    p.add_argument("--no-bump", action="store_true",
                   help="检测到客户端源码变化时也不递增版本")
    p.add_argument("--skip-frontend", action="store_true", help="跳过前端构建")
    p.add_argument("--skip-go", action="store_true", help="跳过 Go 构建")
    p.add_argument("--skip-update", action="store_true", help="不生成更新包")
    p.add_argument("--with-client", action="store_true",
                   help="同时用 electron-builder 产出客户端程序目录（需联网，较慢）")
    p.add_argument("--clean", action="store_true", help="构建前清理产物")
    p.add_argument("--check", action="store_true", help="只做环境与版本体检，不构建")

    # 部署打包
    p.add_argument("--package", action="store_true",
                   help="额外生成服务端部署包（Windows zip + Linux tar.gz）")
    p.add_argument("--package-all", action="store_true",
                   help="生成服务端（Windows + Linux）与客户端部署包")
    p.add_argument("--package-only", action="store_true",
                   help="只打包不构建（等价于 --skip-frontend --skip-go --skip-update --package）")
    p.add_argument("--package-with-updates", action="store_true",
                   help="把当前已签名的客户端更新包一并放进服务端部署包")
    p.add_argument("--key", help="发布方私钥路径（默认 keys/update-private.pem）")
    p.add_argument("--no-keygen", action="store_true",
                   help="缺少发布方密钥时直接报错，不自动生成")
    p.add_argument("--out", help="更新包输出目录（默认取配置的 update.artifacts_dir）")
    p.add_argument("--channel", help="分发通道（默认取配置的 update.channel）")
    p.add_argument("--notes", help="写入更新清单的发布说明")
    p.add_argument("--min-supported-version", help="清单中的最低支持版本")
    p.add_argument("--registry", default="https://registry.npmmirror.com",
                   help="pnpm 首次安装使用的镜像源")
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    started = time.time()

    print(c("Vault 构建脚本", C.BOLD))
    print(c(f"项目根目录：{ROOT}", C.DIM))

    cfg = load_config()
    state = load_build_state()
    current_version = read_version()

    if args.check:
        return run_check(current_version, state)

    # --package-only：只打包，不重新构建（版本号沿用当前值，不触发递增）
    if args.package_only:
        args.skip_frontend = True
        args.skip_go = True
        args.skip_update = True
        args.package = True
        args.no_bump = True

    # --package-all 意味着"要一份能直接运行的客户端"，因此自动打包客户端程序目录
    if args.package_all:
        args.with_client = True

    # ---- 1) 决定版本 ----
    step("版本判定")
    version, old_version, bumped, reason = decide_version(args, state)
    info(f"当前版本：{old_version}")
    info(f"本次版本：{version}")
    info(f"判定依据：{reason}")
    if bumped:
        write_version_everywhere(version)
        ok(f"版本已递增 {old_version} → {version}（已同步 VERSION 与前端 package.json）")
    else:
        # 即使不递增，也确保各处版本一致
        write_version_everywhere(version)

    channel = (args.channel or resolve_channel(cfg)).strip() or "stable"
    out_dir = resolve_update_out_dir(cfg, args.out)

    if args.clean:
        clean_outputs()

    # 每次构建都把 dist 里不属于本次版本的旧产物清掉（--clean 已整体清空时无事可做）。
    clean_stale_dist_artifacts(version)

    # ---- 2) 前端 ----
    if args.skip_frontend:
        warn("已跳过前端构建（--skip-frontend）")
    else:
        build_frontend(args)

    # ---- 3) Go 产物 ----
    # 注意：agent 需要注入公钥，若公钥尚未生成需先产出 Vault-Server（其 sign 子命令负责 keygen），
    # 再用它生成公钥，最后回来构建 agent。
    public_key: str | None = None
    prebuilt: set[str] = set()
    if PUBLIC_KEY_FILE.is_file():
        public_key = read_public_key()
    else:
        warn("尚无发布方公钥，Vault-Agent 将回退到内置开发公钥（其私钥不在仓库，任何更新都会验签失败）")

    if not args.skip_go:
        if public_key is None and not args.skip_update:
            # 先生成密钥，再构建（保证 agent 内置的是本仓库对应的公钥）。
            step("准备发布方密钥")
            build_server_exe(version)
            prebuilt.add("Vault-Server.exe")
            ensure_signing_keys(DIST_DIR / "Vault-Server.exe", args.key, allow_generate=not args.no_keygen)
            public_key = read_public_key()
            ok("发布方密钥就绪")

        build_go(version, public_key, args, prebuilt)
    else:
        warn("已跳过 Go 构建（--skip-go）")

    # ---- 4) 更新包 ----
    update_ok = True
    if args.skip_update:
        warn("已跳过更新包生成（--skip-update）")
    else:
        update_ok = make_update_package(version, channel, out_dir, args)
        if not update_ok:
            fail("更新包生成失败")
            return 1

    # ---- 5) 部署打包 ----
    packages: list[Path] = []
    if args.package or args.package_all:
        # 服务端包默认内联"当前已签名的发布目录内容"：否则开箱部署的服务端只有 updates/说明.txt，
        # 永远无法对外分发更新（客户端报 update.no_manifest）。仅当发布目录确无 manifest 时才退化为只带说明。
        updates_ready = (out_dir / "manifest.json").is_file()
        packages = run_package(
            version,
            want_server=True,
            want_client=bool(args.package_all),
            include_updates=updates_ready or bool(args.package_with_updates),
            args=args,
        )

    # ---- 6) 记录构建状态 ----
    state.update({
        "version": version,
        "channel": channel,
        "client_fingerprint": source_fingerprint(CLIENT_SOURCE_PATHS),
        "server_fingerprint": source_fingerprint(SERVER_SOURCE_PATHS),
        "built_at": int(time.time()),
        "update_package_dir": str(out_dir),
    })
    save_build_state(state)

    print_summary(version, channel, out_dir if not args.skip_update else None, bumped)

    if packages:
        step("部署包（可直接拿去部署）")
        for z in packages:
            info(f"  {z}")
            info(f"    大小 {z.stat().st_size:,} 字节")
        info("Windows：把 zip 传到服务器解压，按包内《部署说明.txt》操作即可")
        info("Linux  ：把 tar.gz 传到服务器解压（保留可执行权限），同样按《部署说明.txt》操作")

    info(f"耗时 {time.time() - started:.1f}s")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print()
        die("已中断", 130)
