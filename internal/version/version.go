// Package version 保存构建期注入的版本信息与协议契约。
package version

// 构建期通过 -ldflags 注入。
var (
	Version   = "0.0.0-dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

// APIVersion 是 HTTP 协议版本。
//
// 注意：该值随代码版本走，**不可配置、不可关闭**。
// 客户端必须始终校验它——协议不匹配会导致请求/响应结构错位，
// 产生难以定位的解析错误。参见 docs/implementation.md 3.4.2。
const APIVersion = 1

// MinSupportedClientVersion 是服务端内置的兜底最低客户端版本。
//
// 当配置未提供 client_compat.min 时，用它作为默认下限，
// 避免服务端下发畸形区间导致客户端判定异常。
//
// 当前为 0.0.0，等价于"暂不设下限"：项目处于 pre-1.0 阶段，不承诺跨版本兼容语义。
// **当项目发布 1.0 后，应把它提升为实际支持的最低客户端版本**，
// 使"过旧客户端"能被服务端明确拒绝而不是带着未知差异继续工作。
const MinSupportedClientVersion = "0.0.0"
