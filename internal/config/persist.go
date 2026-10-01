package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Save 把当前配置**写回配置文件**（管理页在线修改配置的落盘动作）。
//
// ⚠️ 已知副作用（与首次启动补写 instance_id/master_key 时相同）：整份 YAML 会被重新
// 序列化，**注释与字段顺序会丢失**。因此这里先备份一份 <config>.bak，并把这一条明确
// 告知调用方（接口响应里带 warning），让运维知道可以去 config.example.yaml 对照注释。
//
// 不做"局部改写"的原因：yaml.v3 的 Node 级改写要维护两套结构同步，风险与收益不成正比；
// 备份 + 明确告知远比"悄悄写坏注释"可控。
func Save(path string, c *Config) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("配置文件路径为空")
	}
	if c == nil {
		return errors.New("配置为空")
	}
	// 写盘前先校验：绝不能把一份跑不起来的配置写进文件（下次启动就起不来）。
	if err := c.validate(); err != nil {
		return err
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	// 备份（尽力而为：失败不阻断保存，但会让回滚能力缺失，故记录到 error 里返回路径信息）。
	if old, readErr := os.ReadFile(path); readErr == nil {
		if writeErr := os.WriteFile(path+".bak", old, 0o600); writeErr != nil {
			return fmt.Errorf("写备份 %s.bak 失败: %w", path, writeErr)
		}
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("写配置文件失败: %w", err)
	}
	return nil
}
