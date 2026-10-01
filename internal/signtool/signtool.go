// Package signtool 是 Vault 客户端更新包的发布方签名工具（原独立程序 vault-sign）。
//
// 职责（见 docs/implementation.md 7.4.2）：
//   - keygen  ：生成 Ed25519 发布密钥对。**私钥只应存在于发布方的离线环境中**；
//   - release ：计算产物 SHA256、生成 manifest.json、对清单字节签名，并把产物复制进发布目录；
//   - verify  ：发布前自检（校验清单签名与每个产物的 SHA256）。
//
// 安全约定：
//   - 私钥绝不可入库、绝不可写日志；本工具只在 keygen 时把它写到指定的 PEM 文件（0600）；
//   - 服务端与其他任何组件都不需要、也不应持有该私钥（服务端只做 mirror）；
//   - 分发到服务端的文件是：manifest.json、manifest.json.sig、artifacts/。
//
// ⚠️ 服务端进程运行时**永不签名**：本包只由 `Vault-Server sign ...` 子命令在
// 构建/发布阶段调用，私钥不参与任何 serve 路径。
package signtool

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vault/internal/semver"
	"vault/internal/updatepkg"
)

// 发布目录中的固定布局（与 internal/updatepkg 共用同一套名字）。
const (
	manifestName  = updatepkg.ManifestFileName
	signatureName = updatepkg.SignatureFileName
	artifactsDir  = updatepkg.ArtifactsDirName
)

// privateKeyFile / publicKeyFile 是 keygen 的产出文件名。
const (
	privateKeyFile = "update-private.pem"
	publicKeyFile  = "update-public.txt"
)

// ---- keygen ----

// RunKeygen 生成 Ed25519 发布密钥对。
func RunKeygen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outDir := fs.String("out-dir", "./keys", "密钥输出目录")
	force := fs.Bool("force", false, "允许覆盖已存在的密钥文件（危险：会使已发布客户端的验签失效）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir := strings.TrimSpace(*outDir)
	if dir == "" {
		fmt.Fprintln(stderr, "错误: -out-dir 不能为空")
		return fmt.Errorf("signtool: -out-dir 不能为空")
	}
	privPath := filepath.Join(dir, privateKeyFile)
	pubPath := filepath.Join(dir, publicKeyFile)

	// 拒绝覆盖：误毁发布密钥会导致**所有已发布客户端再也无法通过验签**，
	// 只能等新版客户端换公钥，代价极高。
	if !*force {
		for _, p := range []string{privPath, pubPath} {
			if _, err := os.Stat(p); err == nil {
				fmt.Fprintf(stderr, "错误: %s 已存在。如确需重新生成请加 -force（注意：旧公钥签出的更新将全部失效）\n", p)
				return fmt.Errorf("signtool: %s 已存在", p)
			}
		}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(stderr, "错误: 创建输出目录失败:", err)
		return err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(stderr, "错误: 生成密钥失败:", err)
		return err
	}

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		fmt.Fprintln(stderr, "错误: 序列化私钥失败:", err)
		return err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		fmt.Fprintln(stderr, "错误: 写入私钥失败:", err)
		return err
	}

	pubB64 := updatepkg.PublicKeyToBase64(pub)
	if err := os.WriteFile(pubPath, []byte(pubB64+"\n"), 0o644); err != nil {
		fmt.Fprintln(stderr, "错误: 写入公钥失败:", err)
		return err
	}

	fmt.Fprintf(stdout, "已生成发布密钥对:\n  私钥: %s  （⚠️ 绝不可入库/上传，请离线保存并做好备份）\n  公钥: %s\n\n", privPath, pubPath)
	fmt.Fprintf(stdout, "公钥指纹(SHA-256 前 16 位): %s\n\n", fingerprint(pub))
	fmt.Fprintf(stdout, "后续用法:\n  1) 构建客户端时注入公钥:\n")
	fmt.Fprintf(stdout, "     go build -ldflags \"-X vault/internal/agent.updatePublicKeyBase64=%s\" -o Vault-Agent.exe ./cmd/vault-agent\n", pubB64)
	fmt.Fprintf(stdout, "  2) 生成发布目录:\n")
	fmt.Fprintf(stdout, "     Vault-Server sign release -dir ./release -channel stable -version 0.2.0 -key %s -artifact agent=./dist/Vault-Agent.exe\n", privPath)
	fmt.Fprintf(stdout, "  3) 发布前自检:\n     Vault-Server sign verify -dir ./release -pub %s\n", pubPath)
	return nil
}

// fingerprint 返回公钥的 SHA-256 前 16 位十六进制（与客户端界面展示的指纹一致）。
func fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// ---- release ----

// artifactFlag 支持重复出现的 -artifact kind=path。
type artifactFlag map[updatepkg.ArtifactKind]string

// String 实现 flag.Value。
func (a artifactFlag) String() string {
	if len(a) == 0 {
		return ""
	}
	parts := make([]string, 0, len(a))
	for k, v := range a {
		parts = append(parts, string(k)+"="+v)
	}
	return strings.Join(parts, ",")
}

// Set 解析单个 -artifact 值。
func (a artifactFlag) Set(v string) error {
	kind, path, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("格式应为 kind=path，实际为 %q", v)
	}
	kind = strings.TrimSpace(kind)
	path = strings.TrimSpace(path)
	if !updatepkg.ValidArtifactKind(updatepkg.ArtifactKind(kind)) {
		return fmt.Errorf("kind 必须是 agent / client / client_web，实际为 %q", kind)
	}
	if path == "" {
		return fmt.Errorf("产物路径不能为空")
	}
	a[updatepkg.ArtifactKind(kind)] = path
	return nil
}

// RunRelease 生成发布目录（清单 + 签名 + 产物）。
func RunRelease(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "发布目录（必填）")
	channel := fs.String("channel", "stable", "分发通道（stable|beta）")
	version := fs.String("version", "", "目标版本号，如 0.2.0（必填）")
	notes := fs.String("notes", "", "更新说明")
	minSupported := fs.String("min-supported-version", "", "允许直接升级的最低当前版本（留空表示不限制）")
	keyPath := fs.String("key", "", "Ed25519 私钥 PEM 路径（必填）")
	artifacts := artifactFlag{}
	fs.Var(artifacts, "artifact", "产物，可重复：-artifact agent=./dist/Vault-Agent.exe")
	if err := fs.Parse(args); err != nil {
		return err
	}

	outDir := strings.TrimSpace(*dir)
	if outDir == "" {
		fmt.Fprintln(stderr, "错误: -dir 不能为空")
		return fmt.Errorf("signtool: -dir 不能为空")
	}
	ver := strings.TrimSpace(*version)
	if ver == "" {
		fmt.Fprintln(stderr, "错误: -version 不能为空")
		return fmt.Errorf("signtool: -version 不能为空")
	}
	if _, err := semver.Parse(ver); err != nil {
		fmt.Fprintln(stderr, "错误: -version 不是合法的语义化版本号:", err)
		return err
	}
	ch := strings.TrimSpace(*channel)
	if ch == "" {
		fmt.Fprintln(stderr, "错误: -channel 不能为空")
		return fmt.Errorf("signtool: -channel 不能为空")
	}
	if len(artifacts) == 0 {
		fmt.Fprintln(stderr, "错误: 至少需要一个 -artifact kind=path")
		return fmt.Errorf("signtool: 缺少 -artifact")
	}
	key := strings.TrimSpace(*keyPath)
	if key == "" {
		fmt.Fprintln(stderr, "错误: -key 不能为空（私钥只在发布方持有）")
		return fmt.Errorf("signtool: -key 不能为空")
	}
	if ms := strings.TrimSpace(*minSupported); ms != "" {
		if _, err := semver.Parse(ms); err != nil {
			fmt.Fprintln(stderr, "错误: -min-supported-version 不是合法的语义化版本号:", err)
			return err
		}
	}

	priv, err := loadPrivateKey(key)
	if err != nil {
		fmt.Fprintln(stderr, "错误: 读取私钥失败:", err)
		return err
	}

	// 先复制产物并计算摘要，再写清单：这样清单里不会出现"指向不存在的文件"的产物。
	targetDir := filepath.Join(outDir, artifactsDir)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		fmt.Fprintln(stderr, "错误: 创建产物目录失败:", err)
		return err
	}

	// 产物顺序固定（agent → client → client_web），保证同一批输入产出逐字节一致的清单。
	list := make([]updatepkg.Artifact, 0, len(artifacts))
	for _, kind := range updatepkg.ArtifactKindOrder() {
		src, ok := artifacts[kind]
		if !ok {
			continue
		}
		art, err := publishArtifact(kind, src, targetDir)
		if err != nil {
			fmt.Fprintf(stderr, "错误: 处理产物 %s 失败: %v\n", kind, err)
			return err
		}
		list = append(list, *art)
	}

	manifest := updatepkg.Manifest{
		Schema:              updatepkg.SchemaVersion,
		Channel:             ch,
		Version:             ver,
		ReleasedAt:          nowMillis(),
		Notes:               *notes,
		MinSupportedVersion: strings.TrimSpace(*minSupported),
		Artifacts:           list,
	}
	// 缩进 2 空格 + 末尾换行：既是给人看的，也是**被签名的字节**。
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "错误: 序列化清单失败:", err)
		return err
	}
	manifestBytes = append(manifestBytes, '\n')

	sigB64, err := updatepkg.Sign(priv, manifestBytes)
	if err != nil {
		fmt.Fprintln(stderr, "错误: 签名失败:", err)
		return err
	}

	if err := os.WriteFile(filepath.Join(outDir, manifestName), manifestBytes, 0o644); err != nil {
		fmt.Fprintln(stderr, "错误: 写入清单失败:", err)
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, signatureName), []byte(sigB64+"\n"), 0o644); err != nil {
		fmt.Fprintln(stderr, "错误: 写入签名失败:", err)
		return err
	}

	fmt.Fprintf(stdout, "发布目录已生成: %s（channel=%s, version=%s）\n", outDir, ch, ver)
	for _, art := range list {
		fmt.Fprintf(stdout, "  %-6s %-40s %10d 字节  sha256=%s\n", art.Kind, art.Filename, art.Size, art.SHA256)
	}
	fmt.Fprintf(stdout, "清单: %s（%d 字节，已签名）\n签名: %s\n", manifestName, len(manifestBytes), signatureName)
	fmt.Fprintf(stdout, "公钥指纹(SHA-256 前 16 位): %s\n", fingerprint(pubOf(priv)))
	fmt.Fprintln(stdout, "\n下一步: 把整个发布目录内容作为服务端 update.artifacts_dir 的内容分发（服务端不改写任何字节）")
	return nil
}

// publishArtifact 复制单个产物到发布目录并计算摘要。
func publishArtifact(kind updatepkg.ArtifactKind, src, targetDir string) (*updatepkg.Artifact, error) {
	info, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s 是目录", src)
	}
	name := filepath.Base(src)
	if !updatepkg.ValidArtifactName(name) {
		return nil, fmt.Errorf("文件名 %q 非法（只允许字母数字与 . _ -）", name)
	}

	sum, size, err := copyAndHash(src, filepath.Join(targetDir, name))
	if err != nil {
		return nil, err
	}
	art := &updatepkg.Artifact{
		Kind:     kind,
		Filename: name,
		Size:     size,
		SHA256:   sum,
		URL:      artifactsDir + "/" + name,
	}
	// client_web 平台无关：留空 os/arch（updatepkg 也据此免除 os/arch 必填校验）。
	if kind != updatepkg.KindClientWeb {
		art.OS = "windows"
		art.Arch = "amd64"
	}
	return art, nil
}

// copyAndHash 复制文件并返回其十六进制小写 SHA256 与字节数（单次读取）。
func copyAndHash(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(dst)
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// loadPrivateKey 读取 PKCS#8 Ed25519 私钥 PEM。
func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("不是合法的 PEM 文件")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("私钥类型不是 Ed25519")
	}
	return priv, nil
}

// pubOf 从私钥导出公钥。
func pubOf(priv ed25519.PrivateKey) ed25519.PublicKey {
	return priv.Public().(ed25519.PublicKey)
}

// ---- verify ----

// RunVerify 校验发布目录自身的清单签名与产物摘要（发布前自检）。
func RunVerify(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "发布目录（必填）")
	pubPath := fs.String("pub", "", "公钥文件（base64 文本，必填）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	outDir := strings.TrimSpace(*dir)
	if outDir == "" {
		fmt.Fprintln(stderr, "错误: -dir 不能为空")
		return fmt.Errorf("signtool: -dir 不能为空")
	}
	if strings.TrimSpace(*pubPath) == "" {
		fmt.Fprintln(stderr, "错误: -pub 不能为空")
		return fmt.Errorf("signtool: -pub 不能为空")
	}

	pubB64, err := os.ReadFile(*pubPath)
	if err != nil {
		fmt.Fprintln(stderr, "错误: 读取公钥失败:", err)
		return err
	}
	pub, err := updatepkg.PublicKeyFromBase64(string(pubB64))
	if err != nil {
		fmt.Fprintln(stderr, "错误: 解析公钥失败:", err)
		return err
	}

	manifestBytes, err := os.ReadFile(filepath.Join(outDir, manifestName))
	if err != nil {
		fmt.Fprintln(stderr, "错误: 读取清单失败:", err)
		return err
	}
	sigBytes, err := os.ReadFile(filepath.Join(outDir, signatureName))
	if err != nil {
		fmt.Fprintln(stderr, "错误: 读取签名失败:", err)
		return err
	}

	// 通道不校验（发布目录自身的自检），只验签 + 校验结构。
	manifest, err := updatepkg.VerifyAndParse(pub, manifestBytes, string(sigBytes), "")
	if err != nil {
		fmt.Fprintf(stderr, "✗ 清单签名/格式校验失败: %v\n", err)
		return err
	}
	fmt.Fprintf(stdout, "✓ 清单签名校验通过（channel=%s, version=%s, schema=%d）\n",
		manifest.Channel, manifest.Version, manifest.Schema)

	failed := false
	for _, art := range manifest.Artifacts {
		path := filepath.Join(outDir, filepath.FromSlash(art.URL))
		sum, size, err := hashFile(path)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "✗ %s: 读取失败: %v\n", art.Filename, err)
			failed = true
			continue
		case sum != art.SHA256:
			fmt.Fprintf(stderr, "✗ %s: sha256 不匹配（清单 %s，实际 %s）\n", art.Filename, art.SHA256, sum)
			failed = true
			continue
		case size != art.Size:
			fmt.Fprintf(stderr, "✗ %s: 大小不匹配（清单 %d，实际 %d）\n", art.Filename, art.Size, size)
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "✓ %-6s %-40s %10d 字节  sha256=%s\n", art.Kind, art.Filename, size, sum)
	}
	if failed {
		fmt.Fprintln(stderr, "\n自检失败：发布目录不可分发")
		return fmt.Errorf("signtool: 自检失败")
	}
	fmt.Fprintf(stdout, "\n自检通过（公钥指纹 SHA-256 前 16 位: %s）\n", fingerprint(pub))
	return nil
}

// hashFile 计算文件摘要与大小。
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// nowMillis 返回当前毫秒时间戳（清单的 released_at）。
func nowMillis() int64 { return time.Now().UnixMilli() }
