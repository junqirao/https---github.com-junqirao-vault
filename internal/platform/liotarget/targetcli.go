//go:build linux

package liotarget

import "context"

// 本文件是 Manager 与可移植驱动层（runner.go）之间的**粘合层**。
//
// 驱动机制——"该不该下发、发什么、发完算不算成功"、插件路径别名的探测与缓存、
// CHAP 密钥只经 stdin 下发——全部在 runner.go，那里刻意不加构建标签，因而可被
// 单元测试真正执行（runner_test.go 在本机即可运行）。
//
// 这里只做一件事：把 Manager 的方法逐一接到 cliDriver 上，不夹带任何逻辑。
// 这样做的目的是让"逻辑"与"configfs 路径知识"分离，前者可验证、后者才是 Linux 专有。

// apply 下发一批与 backstore 无关的命令，并用 verify 复验后置条件（详见 cliDriver.apply）。
func (m *Manager) apply(ctx context.Context, stage string, verify func() bool, commands ...string) error {
	return m.tcl.apply(ctx, stage, verify, commands...)
}

// applyStorage 下发需要引用 backstore 对象路径的命令（详见 cliDriver.applyStorage）。
func (m *Manager) applyStorage(
	ctx context.Context,
	stage string,
	verify func() bool,
	build func(alias string) []string,
) error {
	return m.tcl.applyStorage(ctx, stage, verify, build)
}

// targetcliProbe 校验 targetcli 可执行文件存在且能正常运行（详见 cliDriver.probe）。
func (m *Manager) targetcliProbe(ctx context.Context) error {
	return m.tcl.probe(ctx)
}

// backstoreObjectPath 返回 targetcli 中该 backstore 的对象路径，供 LUN 映射命令引用。
//
// 注意与 configfs 侧路径（backstorePath）区分：前者是 CLI 语法（别名可能是 block），
// 后者是内核固定布局（恒为 core/iblock_0）。
func (m *Manager) backstoreObjectPath(alias, name string) string {
	return "/backstores/" + alias + "/" + name
}
