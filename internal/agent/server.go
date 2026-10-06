package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"vault/internal/apperr"
)

// 服务端 HTTP 调用的参数。
const (
	// serverRequestTimeout 单次服务端调用的超时（普通接口）。
	serverRequestTimeout = 30 * time.Second
	// mountRequestTimeout 挂载申请（POST /v1/allocations/{id}/mount）的专用超时。
	//
	// 为什么要单独放宽：该接口在服务端是**同步重活** —— 发布 iSCSI 目标、配置 CHAP、
	// 登记 initiator 都要跑 PowerShell（单次约 10s，多步叠加常超 30s）。
	// 沿用 30s 会导致：代理先超时，并把错误报成 agent.server_unreachable
	// （界面显示"本地代理无法连接服务端"），同时把取消传到服务端，服务端日志则是
	// "PowerShell 执行超时或被取消 + status=500" —— 用户看到的两条错误其实是同一次超时，
	// 与地址、连通性无关。
	// 渲染层等待挂载的上限为 MOUNT_TIMEOUT_MS（3 分钟），这里取 2 分钟，
	// 给随后的 iSCSI 会话建立（最多 60s）留出余量。
	mountRequestTimeout = 2 * time.Minute
	// maxServerResponseBytes 服务端响应体的读取上限。
	maxServerResponseBytes = 4 << 20
	// leaseRevokedCode 服务端在租约被撤销时返回的错误码。
	leaseRevokedCode = "lease.revoked"
)

// ClientCompat 是服务端声明的客户端兼容区间。
type ClientCompat struct {
	Enabled bool   `json:"enabled"`
	Min     string `json:"min"`
	Max     string `json:"max"`
}

// SystemInfo 是匿名系统信息（GET /v1/system/info）。
type SystemInfo struct {
	ServerInstanceID string       `json:"server_instance_id"`
	ServerName       string       `json:"server_name"`
	APIVersion       int          `json:"api_version"`
	ServerVersion    string       `json:"server_version"`
	ClientCompat     ClientCompat `json:"client_compat"`
	// UpdateChannel 该服务端提供更新分发的通道（服务端只 mirror 一份发布目录）。
	UpdateChannel string `json:"update_channel"`
}

// ProbeCert 是探测到的服务端证书摘要（仅连通性探测返回，供人工与 pki/server.crt 比对）。
type ProbeCert struct {
	// SHA256 证书 DER 的 SHA-256 指纹（小写十六进制）。
	SHA256 string `json:"sha256"`
	// Subject 证书主题。
	Subject string `json:"subject"`
	// NotAfter 证书过期时间（Unix 秒）。
	NotAfter int64 `json:"not_after"`
}

// ProbeResult 是 /agent/server/test 的探测结果。
type ProbeResult struct {
	OK bool `json:"ok"`
	// ServerURL 实际生效的地址（可能已从 http 自动升级为 https）。
	ServerURL string `json:"server_url"`
	// NormalizedFrom 用户原始输入。
	NormalizedFrom string `json:"normalized_from"`
	// UpgradedToTLS 是否发生了 http → https 的自动升级。
	UpgradedToTLS    bool         `json:"upgraded_to_tls"`
	ServerName       string       `json:"server_name"`
	APIVersion       int          `json:"api_version"`
	ServerVersion    string       `json:"server_version"`
	ServerInstanceID string       `json:"server_instance_id"`
	ClientCompat     ClientCompat `json:"client_compat"`
	// Cert 服务端证书摘要；纯 http 服务端为 nil（json 中省略）。
	Cert *ProbeCert `json:"cert,omitempty"`
}

// errProbeHTTP400 表示服务端（TLS 端口）对明文 HTTP 请求返回了 400。
//
// 这是 Go 的 http.Server 在 TLS 监听端口收到明文请求时的标准行为
// （"Client sent an HTTP request to an HTTPS server"）。
// 由 probeServer 判定是否需要从 http 自动升级到 https。
var errProbeHTTP400 = errors.New("agent: 服务端要求 TLS（明文 HTTP 请求被拒绝，HTTP 400）")

// MountSpec 是服务端下发的挂载参数（对应 internal/app.MountSpec 的 JSON 形状）。
type MountSpec struct {
	ServerInstanceID string `json:"server_instance_id"`
	ServerName       string `json:"server_name"`
	// RepoName 存储库名称：用于卷标（盘符模式）与目录名（目录模式），见 mountEngine.mountAt。
	RepoName      string `json:"repo_name"`
	TargetIQN     string `json:"target_iqn"`
	PortalAddress string `json:"portal_address"`
	PortalPort    int    `json:"portal_port"`
	AuthMode      string `json:"auth_mode"`
	ChapUser      string `json:"chap_user"`
	// ChapSecret 仅本次下发；只在内存中保留到卸载。
	ChapSecret       string `json:"chap_secret"`
	DiskSizeBytes    int64  `json:"disk_size_bytes"`
	LeaseID          string `json:"lease_id"`
	LeaseTTLSeconds  int    `json:"lease_ttl_seconds"`
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
	MountMode        string `json:"mount_mode"`
	MountPath        string `json:"mount_path"`
	PostScript       string `json:"post_script"`
}

// HeartbeatResult 是心跳响应（POST /v1/leases/{id}/heartbeat）。
type HeartbeatResult struct {
	LeaseTTLSeconds int   `json:"lease_ttl_seconds"`
	ExpiresAt       int64 `json:"expires_at"`
}

// serverClient 是与服务端通信的 HTTP 客户端（Bearer 认证）。
type serverClient struct {
	base   string
	token  string
	logger *slog.Logger

	http *http.Client
	// long 是"长耗时调用"专用客户端（目前只有挂载申请），与 http 共用 Transport，
	// 仅超时不同；原因见 mountRequestTimeout 的说明。
	long   *http.Client
	stream *http.Client

	// onAuthExpired 在"会话失效"（服务端返回 401 auth.required）时被调用，
	// 用本地客户端证书重新登录并返回一个**新令牌**；返回错误表示续期失败。
	//
	// 存在意义：**服务端重启会清空内存会话表**，此前签发的令牌一律失效。
	// 证书登录的客户端据此自愈（重新用证书换一个会话），不需要用户重新输密码。
	// 为空（如证书登录用的 mTLS 客户端、匿名探测）表示不做重试。
	onAuthExpired func(ctx context.Context) (string, error)
}

// withAuthRetry 绑定"会话失效后重签令牌"的回调，返回自身便于链式调用。
func (c *serverClient) withAuthRetry(fn func(ctx context.Context) (string, error)) *serverClient {
	c.onAuthExpired = fn
	return c
}

// newServerClient 构造服务端客户端。
//
// baseURL 必须是合法的 http/https 绝对地址；token 可为空（匿名接口，如 GET /v1/system/info）。
//
// certSHA256 非空时启用 **TOFU 指纹固定**：服务端使用自建 CA，首次连接无法用公信 CA
// 验证（见 probeOnce 的说明），因此把首次探测得到的证书指纹固化下来，此后每个连接都要求
// 服务端证书 DER 的 SHA-256 与之完全一致；指纹不符即拒绝连接（防中间人）。
// 为空时退回 Go 默认的链路校验（适合服务端使用公信 CA 的场景）。
func newServerClient(baseURL, token, certSHA256 string, logger *slog.Logger) (*serverClient, error) {
	if logger == nil {
		logger = slog.Default()
	}
	raw := strings.TrimSpace(baseURL)
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, apperr.InvalidParam("server_url").WithCause(err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, apperr.InvalidParam("server_url").WithArg("scheme", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, apperr.InvalidParam("server_url")
	}

	pinned := strings.ToLower(strings.TrimSpace(certSHA256))
	if _, err := hex.DecodeString(pinned); pinned != "" && (err != nil || len(pinned) != sha256.Size*2) {
		return nil, apperr.InvalidParam("cert_sha256")
	}

	// 事件流是长连接：必须去掉整体超时，因此与普通请求分开两个 Client，
	// 但**共用同一个 Transport**，确保 TLS 策略（含指纹固定）对两者一致。
	transport := &http.Transport{}
	if pinned != "" {
		// 指纹固定场景下必须关闭内建链校验：自签证书本来就不在系统信任库里，
		// 开启会先于指纹校验失败。真正的校验由 VerifyPeerCertificate 完成。
		// 注意：这里不再做主机名/SAN 校验——身份由指纹单独完成绑定，
		// 换 IP/换域名不会导致连接被拒（指纹不变即视为同一服务端）。
		transport.TLSClientConfig = &tls.Config{
			//nolint:gosec // 链校验被下方指纹固定替代，见方法注释
			InsecureSkipVerify:    true,
			VerifyPeerCertificate: verifyPeerFingerprint(pinned),
		}
	}

	return &serverClient{
		base:   strings.TrimRight(raw, "/"),
		token:  token,
		logger: logger,
		http:   &http.Client{Timeout: serverRequestTimeout, Transport: transport},
		long:   &http.Client{Timeout: mountRequestTimeout, Transport: transport},
		stream: &http.Client{Transport: transport},
	}, nil
}

// verifyPeerFingerprint 返回一个 TLS 对端校验回调：要求服务端证书 DER 的 SHA-256
// 与 pinned 完全一致（TOFU 指纹固定，防中间人）。
func verifyPeerFingerprint(pinned string) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("agent: 服务端未提供证书")
		}
		sum := sha256.Sum256(rawCerts[0])
		if !strings.EqualFold(hex.EncodeToString(sum[:]), pinned) {
			return fmt.Errorf("agent: 服务端证书指纹与首次记录不一致（期望 %s）", pinned)
		}
		return nil
	}
}

// newMTLSClient 构造携带客户端证书、并以指纹固定服务端证书的客户端（用于证书免密登录）。
//
// certPEM/keyPEM 是本地客户端身份（私钥绝不外传）；certSHA256 是服务端证书指纹（必填）。
// 服务端证书自动维护自建 CA，无法用公信 CA 校验，因此沿用与 newServerClient 相同的
// 指纹固定策略：跳过内建链校验，改由 VerifyPeerCertificate 固定指纹。
func newMTLSClient(baseURL, certPEM, keyPEM, certSHA256 string, logger *slog.Logger) (*serverClient, error) {
	if logger == nil {
		logger = slog.Default()
	}
	raw := strings.TrimSpace(baseURL)
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, apperr.InvalidParam("server_url").WithCause(err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, apperr.InvalidParam("server_url").WithArg("scheme", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, apperr.InvalidParam("server_url")
	}

	pinned := strings.ToLower(strings.TrimSpace(certSHA256))
	if pinned == "" {
		return nil, errServerCertUnknown()
	}
	if sum, err := hex.DecodeString(pinned); err != nil || len(sum) != sha256.Size {
		return nil, apperr.InvalidParam("cert_sha256")
	}

	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, apperr.New(CodeIdentityInvalid, http.StatusConflict).
			WithArg("reason", "keypair").WithCause(err)
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
		//nolint:gosec // 链校验被下方指纹固定替代，见方法注释
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifyPeerFingerprint(pinned),
	}}
	return &serverClient{
		base:   strings.TrimRight(raw, "/"),
		logger: logger,
		http:   &http.Client{Timeout: serverRequestTimeout, Transport: transport},
		long:   &http.Client{Timeout: mountRequestTimeout, Transport: transport},
		stream: &http.Client{Transport: transport},
	}, nil
}

// SystemInfo 调用匿名接口 GET /v1/system/info。
func (c *serverClient) SystemInfo(ctx context.Context) (*SystemInfo, error) {
	var out SystemInfo
	if err := c.do(ctx, http.MethodGet, "/v1/system/info", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RequestMount 调用 POST /v1/allocations/{id}/mount 获取挂载参数。
//
// 用 doLong（2 分钟）：服务端会在该请求内同步发布 iSCSI 目标（PowerShell 多步），
// 30s 的普通超时必然先把请求掐断（详见 mountRequestTimeout）。
func (c *serverClient) RequestMount(ctx context.Context, allocationID, clientID string) (*MountSpec, error) {
	if strings.TrimSpace(allocationID) == "" {
		return nil, apperr.InvalidParam("allocation_id")
	}
	body := map[string]any{"client_id": clientID}
	var out MountSpec
	path := "/v1/allocations/" + url.PathEscape(allocationID) + "/mount"
	if err := c.doLong(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Heartbeat 调用 POST /v1/leases/{id}/heartbeat 续期租约。
func (c *serverClient) Heartbeat(ctx context.Context, leaseID, clientID string) (*HeartbeatResult, error) {
	if strings.TrimSpace(leaseID) == "" {
		return nil, apperr.InvalidParam("lease_id")
	}
	body := map[string]any{"client_id": clientID}
	var out HeartbeatResult
	path := "/v1/leases/" + url.PathEscape(leaseID) + "/heartbeat"
	if err := c.do(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReportMounted 调用 POST /v1/leases/{id}/mounted 回写挂载点。
func (c *serverClient) ReportMounted(ctx context.Context, leaseID, clientID, mountPoint string) error {
	if strings.TrimSpace(leaseID) == "" {
		return apperr.InvalidParam("lease_id")
	}
	body := map[string]any{"client_id": clientID, "mount_point": mountPoint}
	path := "/v1/leases/" + url.PathEscape(leaseID) + "/mounted"
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// Release 调用 POST /v1/leases/{id}/release 通知服务端已卸载。
func (c *serverClient) Release(ctx context.Context, leaseID, clientID string) error {
	if strings.TrimSpace(leaseID) == "" {
		return apperr.InvalidParam("lease_id")
	}
	body := map[string]any{"client_id": clientID}
	path := "/v1/leases/" + url.PathEscape(leaseID) + "/release"
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// ---- 分块上传建库（见 docs/agent-api.md「本地目录扫描与上传建库」）----

// uploadManifestEntry 是清单中的单条文件记录（与服务端 DTO 对齐）。
type uploadManifestEntry struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime,omitempty"`
}

// uploadManifest 是上传清单。
type uploadManifest struct {
	Root  string                `json:"root"`
	Files []uploadManifestEntry `json:"files"`
}

// createUploadRequest 是 POST /v1/uploads 请求体。
type createUploadRequest struct {
	RepoName  string         `json:"repo_name"`
	Mode      string         `json:"mode"`
	Manifest  uploadManifest `json:"manifest"`
	RepoMode  string         `json:"repo_mode,omitempty"`
	StorageID string         `json:"storage_id,omitempty"`
	// QuotaBytes 0=不限。
	QuotaBytes int64 `json:"quota_bytes,omitempty"`
}

// uploadSession 是创建上传会话的响应。
type uploadSession struct {
	UploadID      string `json:"upload_id"`
	ChunkSize     int64  `json:"chunk_size"`
	MissingChunks []int  `json:"missing_chunks"`
	ReceivedBytes int64  `json:"received_bytes"`
	TotalBytes    int64  `json:"total_bytes"`
}

// uploadStatus 是查询上传会话状态的响应（断点续传用）。
type uploadStatus struct {
	UploadID      string `json:"upload_id"`
	State         string `json:"state"`
	TotalFiles    int    `json:"total_files"`
	TotalBytes    int64  `json:"total_bytes"`
	ReceivedBytes int64  `json:"received_bytes"`
	MissingChunks []int  `json:"missing_chunks"`
}

// serverJob 是 GET /v1/jobs/{id} 的最小响应形状。
type serverJob struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	RefID    string `json:"ref_id"`
	State    string `json:"state"`
	Progress int    `json:"progress"`
	Failed   bool   `json:"failed"`
}

// repoListItem 是 GET /v1/repos 的最小响应形状。
type repoListItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CreateUpload 调用 POST /v1/uploads 创建分块上传会话。
func (c *serverClient) CreateUpload(ctx context.Context, in createUploadRequest) (*uploadSession, error) {
	var out uploadSession
	if err := c.do(ctx, http.MethodPost, "/v1/uploads", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadStatus 调用 GET /v1/uploads/{id} 查询会话状态与缺失分块。
func (c *serverClient) UploadStatus(ctx context.Context, uploadID string) (*uploadStatus, error) {
	var out uploadStatus
	path := "/v1/uploads/" + url.PathEscape(uploadID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PutUploadChunk 调用 PUT /v1/uploads/{id}/chunks/{index} 上传单个分块（原始字节）。
//
// 使用无整体超时的 stream 客户端上传（8MiB 分块在慢链路上可能超过普通请求的固定超时），
// 超时控制交由 ctx。X-Chunk-SHA256 为小写十六进制。
func (c *serverClient) PutUploadChunk(ctx context.Context, uploadID string, index int, sha256Hex string, body []byte) error {
	path := "/v1/uploads/" + url.PathEscape(uploadID) + "/chunks/" + strconv.Itoa(index)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+path, bytes.NewReader(body))
	if err != nil {
		return errServerUnreachable(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Chunk-SHA256", sha256Hex)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return errServerUnreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxServerResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseServerError(resp.StatusCode, data)
	}
	return nil
}

// CompleteUpload 调用 POST /v1/uploads/{id}/complete 触发建库任务，返回 job_id。
func (c *serverClient) CompleteUpload(ctx context.Context, uploadID string) (string, error) {
	var out struct {
		JobID string `json:"job_id"`
	}
	path := "/v1/uploads/" + url.PathEscape(uploadID) + "/complete"
	if err := c.do(ctx, http.MethodPost, path, nil, &out); err != nil {
		return "", err
	}
	return out.JobID, nil
}

// AbortUpload 调用 DELETE /v1/uploads/{id} 放弃会话（清理服务端暂存）。
func (c *serverClient) AbortUpload(ctx context.Context, uploadID string) error {
	path := "/v1/uploads/" + url.PathEscape(uploadID)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// Job 调用 GET /v1/jobs/{id} 查询任务状态。
func (c *serverClient) Job(ctx context.Context, jobID string) (*serverJob, error) {
	var out serverJob
	path := "/v1/jobs/" + url.PathEscape(jobID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FindRepoID 通过 GET /v1/repos 按名称查找存储库 ID；未找到返回空串（不报错）。
func (c *serverClient) FindRepoID(ctx context.Context, name string) (string, error) {
	var out struct {
		Items []repoListItem `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/repos?limit=200&offset=0", nil, &out); err != nil {
		return "", err
	}
	for _, it := range out.Items {
		if it.Name == name {
			return it.ID, nil
		}
	}
	return "", nil
}

// allocationStateReleased 是服务端分配状态里"已释放"的取值（见 internal/domain AllocationState）。
const allocationStateReleased = "released"

// allocationItem 是 GET /v1/repos/{id}/allocations 的最小响应形状（只取筛选用得到的字段）。
type allocationItem struct {
	ID     string `json:"id"`
	RepoID string `json:"repo_id"`
	UserID string `json:"user_id"`
	State  string `json:"state"`
}

// ListRepos 调用 GET /v1/repos 返回当前会话可见的存储库（id + 名称）。
func (c *serverClient) ListRepos(ctx context.Context) ([]repoListItem, error) {
	var out struct {
		Items []repoListItem `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/repos?limit=200&offset=0", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ListAllocations 调用 GET /v1/repos/{id}/allocations 列出该库的**全部**分配。
//
// 注意：服务端不按用户/状态过滤，已释放的历史记录也在列表里，调用方自己筛
// （见 ensureMyAllocation）。
func (c *serverClient) ListAllocations(ctx context.Context, repoID string) ([]allocationItem, error) {
	if strings.TrimSpace(repoID) == "" {
		return nil, apperr.InvalidParam("repo_id")
	}
	var out struct {
		Items []allocationItem `json:"items"`
	}
	path := "/v1/repos/" + url.PathEscape(repoID) + "/allocations"
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Allocate 调用 POST /v1/repos/{id}/allocations 为指定用户在该库建立分配。
//
// 用 doLong：共享库要派生差异盘、独享库要建独立盘，服务端在请求内同步完成。
func (c *serverClient) Allocate(ctx context.Context, repoID, userID string) (allocationItem, error) {
	var out allocationItem
	if strings.TrimSpace(repoID) == "" {
		return out, apperr.InvalidParam("repo_id")
	}
	path := "/v1/repos/" + url.PathEscape(repoID) + "/allocations"
	body := map[string]any{"user_id": userID}
	if err := c.doLong(ctx, http.MethodPost, path, body, &out); err != nil {
		return out, err
	}
	return out, nil
}

// GetRawBytes 发起 GET 并返回响应体的**原始字节**（不做任何解析）。
//
// 更新清单与签名必须逐字节取回：客户端要对这些字节验签，
// 因此不能走 do() 的 JSON 反序列化路径（那会先把字节变成结构体，验签就失去意义）。
func (c *serverClient) GetRawBytes(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, errServerUnreachable(err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errServerUnreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxServerResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseServerError(resp.StatusCode, data)
	}
	if readErr != nil {
		return nil, errServerUnreachable(readErr)
	}
	return data, nil
}

// OpenRange 发起带 Range 的 GET 并把响应体交给调用方流式处理（调用方负责关闭 Body）。
//
// offset > 0 时请求 `bytes=<offset>-` 以支持断点续传。这里使用**无整体超时**的客户端：
// 产物可能有几十 MB，普通请求的固定超时会让下载永远无法完成；超时控制交由 ctx。
func (c *serverClient) OpenRange(ctx context.Context, path string, offset int64) (*http.Response, error) {
	if offset > 0 {
		return c.OpenRangeBytes(ctx, path, offset, -1, "")
	}
	return c.OpenRangeBytes(ctx, path, -1, -1, "")
}

// OpenRangeBytes 发起一次可控区间的 GET（调用方负责关闭 Body）。
//
//   - start < 0：不发送 Range（整文件请求）；
//   - start >= 0 且 end < 0：发送 `Range: bytes=<start>-`（到文件末尾）；
//   - start >= 0 且 end >= 0：发送 `Range: bytes=<start>-<end>`（闭区间，用于分段并发下载）；
//   - ifRange 非空：附带 `If-Range`（陈旧前缀保护：远端已变时服务端会回 200 而非 206）。
func (c *serverClient) OpenRangeBytes(ctx context.Context, path string, start, end int64, ifRange string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, errServerUnreachable(err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	if start >= 0 {
		value := "bytes=" + strconv.FormatInt(start, 10) + "-"
		if end >= 0 {
			value += strconv.FormatInt(end, 10)
		}
		req.Header.Set("Range", value)
	}
	if v := strings.TrimSpace(ifRange); v != "" {
		req.Header.Set("If-Range", v)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, errServerUnreachable(err)
	}
	return resp, nil
}

// do 发起一次 JSON 调用；会话失效时**用本地证书换发新令牌并重试一次**。
//
// 失败语义：网络层错误 → agent.server_unreachable（**超时** → agent.server_timeout）；
// 服务端结构化错误 → 原样保留其错误码与 HTTP 状态。
//
// 重试条件（缺一不可）：本次带过令牌、错误是 auth.required（401）、且已绑定 onAuthExpired。
// 只重试一次：重试后仍失败就把**原始错误**返回给调用方，绝不无限循环。
func (c *serverClient) do(ctx context.Context, method, path string, body any, out any) error {
	return c.doWith(ctx, c.http, method, path, body, out)
}

// doLong 与 do 同语义，但使用**长耗时调用**专用超时（见 mountRequestTimeout）。
//
// 目前只有挂载申请走这条路：该接口在服务端是同步重活，30s 必然超时。
func (c *serverClient) doLong(ctx context.Context, method, path string, body any, out any) error {
	return c.doWith(ctx, c.long, method, path, body, out)
}

// doWith 用指定客户端发起调用，并处理"会话失效换令牌重试一次"。
func (c *serverClient) doWith(ctx context.Context, cl *http.Client, method, path string, body any, out any) error {
	err := c.doOnceWith(ctx, cl, method, path, body, out)
	if !isSessionExpiredError(err) || c.token == "" || c.onAuthExpired == nil {
		return err
	}
	// 续期走独立超时：请求自身的 ctx 可能已接近截止。
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serverRequestTimeout)
	defer cancel()
	newToken, renewErr := c.onAuthExpired(callCtx)
	if renewErr != nil || strings.TrimSpace(newToken) == "" {
		return err
	}
	c.token = newToken
	if retryErr := c.doOnceWith(ctx, cl, method, path, body, out); retryErr != nil {
		// 重试仍失败时返回**重试的错误**（更贴近当前状态），但续期本身已生效。
		return retryErr
	}
	return nil
}

// doOnce 发起一次 JSON 调用（不含重试），使用普通超时。
func (c *serverClient) doOnce(ctx context.Context, method, path string, body any, out any) error {
	return c.doOnceWith(ctx, c.http, method, path, body, out)
}

// doOnceWith 用指定客户端发起一次 JSON 调用（不含重试）。
func (c *serverClient) doOnceWith(ctx context.Context, cl *http.Client, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return apperr.InvalidParam("body").WithCause(err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return errServerUnreachable(err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := cl.Do(req)
	if err != nil {
		return serverCallError(err, cl.Timeout)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxServerResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseServerError(resp.StatusCode, data)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return errServerUnreachable(fmt.Errorf("解析服务端响应失败: %w", err))
		}
	}
	return nil
}

// serverCallError 把传输层错误映射为稳定错误码：**超时与不可达必须区分**。
//
// 为什么要区分：挂载接口在服务端是同步重活（见 mountRequestTimeout），
// 把"服务端还在干活"笼统报成"无法连接服务端"，会把排查直接引向错误的地址/网络方向
// （真实工单：客户端显示 server_unreachable，实际是 30s 超时）。
func serverCallError(err error, timeout time.Duration) *apperr.Error {
	if isTimeoutError(err) {
		return errServerTimeout(err, timeout)
	}
	return errServerUnreachable(err)
}

// isTimeoutError 判断错误是否为超时（客户端整体超时，或调用方 ctx 截止）。
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// parseServerError 解析服务端统一错误体 {"error":{"code":...,"args":{}}}。
func parseServerError(status int, data []byte) error {
	var body struct {
		Error struct {
			Code string         `json:"code"`
			Args map[string]any `json:"args"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err == nil && body.Error.Code != "" {
		e := apperr.New(body.Error.Code, status)
		for k, v := range body.Error.Args {
			e = e.WithArg(k, v)
		}
		return e
	}
	return apperr.New(apperr.CodeInternal, status).WithArg("http_status", status)
}

// sessionExpiredCode 服务端在"令牌无效/已过期"时返回的错误码（401）。
const sessionExpiredCode = "auth.required"

// isSessionExpiredError 判断错误是否为"会话失效"（令牌无效/过期/服务端重启后丢失）。
//
// 只认 401 的 auth.required：**不**含 auth.forbidden（那是"身份有效但没权限"，
// 用证书重登录也拿不到更多权限，重试只会打爆服务端）。
func isSessionExpiredError(err error) bool {
	return err != nil && apperr.CodeOf(err) == sessionExpiredCode
}

// isLeaseRevoked 判断错误是否表示租约已被服务端撤销。
func isLeaseRevoked(err error) bool {
	return err != nil && apperr.CodeOf(err) == leaseRevokedCode
}

// ---- 服务端连通性探测（POST /agent/server/test）----

// normalizeProbeURL 归一化用户输入的服务端地址。
//
// 规则：无协议时按服务端默认 TLS 补 https://（而不是 http://）；保留端口与路径；去掉末尾多余的斜杠。
func normalizeProbeURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", apperr.InvalidParam("url")
	}
	withScheme := trimmed
	if !strings.Contains(trimmed, "://") {
		withScheme = "https://" + trimmed
	}
	parsed, err := url.Parse(withScheme)
	if err != nil {
		return "", apperr.InvalidParam("url").WithCause(err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", apperr.InvalidParam("url").WithArg("scheme", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", apperr.InvalidParam("url")
	}
	return strings.TrimRight(withScheme, "/"), nil
}

// certFingerprint 计算证书 DER 的 SHA-256 指纹（小写十六进制）。
func certFingerprint(cert *x509.Certificate) *ProbeCert {
	sum := sha256.Sum256(cert.Raw)
	return &ProbeCert{
		SHA256:   hex.EncodeToString(sum[:]),
		Subject:  cert.Subject.String(),
		NotAfter: cert.NotAfter.Unix(),
	}
}

// probeOnce 对给定地址执行一次匿名 GET /v1/system/info 探测。
//
// 安全约束：**只有这条连通性探测路径**跳过 TLS 证书校验（服务端使用自建 CA，
// 首次连接无法验证证书链），目的是测通并回传证书指纹供人工核对；
// 登录/心跳/挂载等正常 API 调用一律走 newServerClient 的默认严格校验，
// 绝不继承此处的 InsecureSkipVerify。
func (a *Agent) probeOnce(ctx context.Context, baseURL string) (*SystemInfo, *ProbeCert, error) {
	var cert *ProbeCert
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			//nolint:gosec // 仅连通性探测路径；见方法注释，正常 API 调用不使用该配置。
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) > 0 {
					cert = certFingerprint(cs.PeerCertificates[0])
				}
				return nil
			},
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/system/info", nil)
	if err != nil {
		return nil, nil, errServerUnreachable(err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, errServerUnreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxServerResponseBytes))
	switch {
	case resp.StatusCode == http.StatusBadRequest:
		// TLS 端口对明文 HTTP 请求的典型响应，交由 probeServer 决定是否升级协议。
		return nil, cert, errProbeHTTP400
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, cert, errBadResponse(nil).WithArg("http_status", resp.StatusCode)
	}

	var info SystemInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, cert, errBadResponse(err)
	}
	// 连上了但字段全空，说明不是 Vault 服务端（例如别的 Web 服务返回了合法 JSON）。
	if info.ServerInstanceID == "" && info.ServerName == "" && info.APIVersion == 0 {
		return nil, cert, errBadResponse(nil)
	}
	return &info, cert, nil
}

// probeServer 探测服务端连通性，自动识别并纠正协议：
//   - 无协议输入默认按 https 处理（服务端默认启用 TLS）；
//   - 明文 http 得到 HTTP 400 时，自动改用 https 重试一次，成功则以 https 结果为准；
//   - 证书指纹随结果一并返回，便于与服务器 pki/server.crt 比对。
func (a *Agent) probeServer(ctx context.Context, raw string) (*ProbeResult, error) {
	normalized, err := normalizeProbeURL(raw)
	if err != nil {
		return nil, err
	}
	parsed, _ := url.Parse(normalized)

	info, cert, probeErr := a.probeOnce(ctx, normalized)
	effective := normalized
	upgraded := false

	if probeErr != nil {
		if !errors.Is(probeErr, errProbeHTTP400) {
			return nil, probeErr
		}
		if parsed.Scheme != "http" {
			// 已是 https 却仍被拒：按坏响应处理，避免误报为"需要 TLS"。
			return nil, errBadResponse(probeErr)
		}
		httpsURL := "https://" + strings.TrimPrefix(normalized, "http://")
		httpsInfo, httpsCert, httpsErr := a.probeOnce(ctx, httpsURL)
		if httpsErr != nil {
			// 明文被拒且 https 也失败：明确提示改用 https://。
			return nil, errTLSRequired(probeErr)
		}
		info, cert, effective, upgraded = httpsInfo, httpsCert, httpsURL, true
	}

	return &ProbeResult{
		OK:               true,
		ServerURL:        effective,
		NormalizedFrom:   strings.TrimSpace(raw),
		UpgradedToTLS:    upgraded,
		ServerName:       info.ServerName,
		APIVersion:       info.APIVersion,
		ServerVersion:    info.ServerVersion,
		ServerInstanceID: info.ServerInstanceID,
		ClientCompat:     info.ClientCompat,
		Cert:             cert,
	}, nil
}
