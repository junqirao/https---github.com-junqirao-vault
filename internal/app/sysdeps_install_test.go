package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
	"vault/internal/store"
)

// fakeSysDepsInstaller 模拟平台层"能否装 / 装了什么"。
//
// 同时记录 Check 与 Install 的入参：本文件最核心的一条断言就是
// "前置检查拒绝时根本没走到安装"，只看返回值是证明不了的。
type fakeSysDepsInstaller struct {
	checkErr   error
	installErr error
	cmd        string
	checked    []string
	installed  []string
}

func (f *fakeSysDepsInstaller) Check(key string) error {
	f.checked = append(f.checked, key)
	return f.checkErr
}

func (f *fakeSysDepsInstaller) Install(_ context.Context, key string) (string, error) {
	f.installed = append(f.installed, key)
	return f.cmd, f.installErr
}

// newSysDepsTestApp 构造只带 Store / Log / Jobs / 假安装器的 App。
func newSysDepsTestApp(t *testing.T, ins SysDepsInstaller) (*App, *store.Store) {
	t.Helper()
	st := openAppTestStore(t)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &App{Deps: Deps{
		Store:          st,
		Log:            discard,
		Jobs:           job.NewWorker(st, lock.NewKeyed(), job.Options{Logger: discard}),
		SysDepsInstall: ins,
	}}, st
}

// countJobs 数任务表里的行数。
func countJobs(t *testing.T, st *store.Store) int {
	t.Helper()
	lst, err := st.ListJobs(context.Background(), "", 100, 0)
	if err != nil {
		t.Fatalf("列任务失败：%v", err)
	}
	return len(lst)
}

// TestInstallSysDepsRejectsWithoutPlatformAbility 平台没有"装包"这项能力（Windows）时必须
// 直接拒绝，而不是造一个永远无人处理的任务 —— 后者会让前端一直 loading 到超时。
func TestInstallSysDepsRejectsWithoutPlatformAbility(t *testing.T) {
	a, st := newSysDepsTestApp(t, nil)

	_, err := a.InstallSysDeps(context.Background(), "lio_tools")
	if got := apperr.CodeOf(err); got != apperr.PlatformUnsupported().Code {
		t.Fatalf("错误码 = %q，期望 %q：%v", got, apperr.PlatformUnsupported().Code, err)
	}
	if n := countJobs(t, st); n != 0 {
		t.Fatalf("能力缺失时不得落任务，实际 %d 条", n)
	}
}

// TestInstallSysDepsRejectsBeforeEnqueue 前置条件不满足时的两条硬要求：
//
//  1. 错误码**原样透传**，不被包成 system.internal。前端是靠 code 翻译文案的，
//     丢掉码就只能显示一句"安装失败"，用户无从判断是权限、配置还是网络。
//  2. **不得留下任务**。前置检查的意义就在于"此刻就能确定装不了"，
//     若仍然落一条任务再让它失败，用户会在任务中心看到一条说不清原因的失败记录。
func TestInstallSysDepsRejectsBeforeEnqueue(t *testing.T) {
	cases := []struct {
		name string
		err  *apperr.Error
	}{
		{"装包被配置关闭", apperr.SysDepsInstallDisabled()},
		{"服务端非 root", apperr.SysDepsInstallNotRoot()},
		{"宿主机无包管理器", apperr.SysDepsNoPackageManager()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ins := &fakeSysDepsInstaller{checkErr: c.err}
			a, st := newSysDepsTestApp(t, ins)

			_, err := a.InstallSysDeps(context.Background(), "lio_tools")
			if got := apperr.CodeOf(err); got != c.err.Code {
				t.Fatalf("错误码 = %q，期望 %q（必须原样透传）：%v", got, c.err.Code, err)
			}
			if len(ins.installed) != 0 {
				t.Fatalf("前置检查已拒绝却仍执行了安装：%v", ins.installed)
			}
			if n := countJobs(t, st); n != 0 {
				t.Fatalf("前置检查拒绝后不得落任务，实际 %d 条", n)
			}
		})
	}
}

// TestInstallSysDepsEnqueuesEachAttempt 每次提交都必须独立成一个任务。
//
// job 表的 idem_key 上是**永久**唯一索引（空值不参与）：若这里图省事用固定键
// （如 "install_deps:lio_tools"），第一次装失败之后用户再点，只会一遍遍拿回那条
// 历史失败任务，永远也装不上 —— 而"重试装包"恰恰是本功能最常见的用法（源没配好、
// 网络抖动、包名要改手工装）。这条用例就是钉住"装包不能有永久幂等键"。
func TestInstallSysDepsEnqueuesEachAttempt(t *testing.T) {
	ins := &fakeSysDepsInstaller{cmd: "apt-get install -y targetcli-fb"}
	a, st := newSysDepsTestApp(t, ins)
	ctx := context.Background()

	// 带空格提交：Key 会被 trim 后再落库与比对，否则 ref_id 里会混进空白。
	j1, err := a.InstallSysDeps(ctx, " lio_tools ")
	if err != nil {
		t.Fatalf("首次提交失败：%v", err)
	}
	j2, err := a.InstallSysDeps(ctx, "lio_tools")
	if err != nil {
		t.Fatalf("二次提交失败：%v", err)
	}
	if j1.ID == j2.ID {
		t.Fatalf("两次提交返回了同一个任务（%s）：装包失败后用户将无法重试", j1.ID)
	}
	if j1.Type != domain.JobInstallDeps || j1.RefID != "lio_tools" {
		t.Fatalf("类型/引用 = %q/%q，期望 install_deps/lio_tools", j1.Type, j1.RefID)
	}
	// 并发装包由宿主机级锁串行化：两个任务必须盯同一个锁，否则会同时去撞 dpkg 的锁。
	if j1.LockKey != j2.LockKey || j1.LockKey == "" {
		t.Fatalf("装包任务必须共用宿主级锁，实际 %q / %q", j1.LockKey, j2.LockKey)
	}
	if n := countJobs(t, st); n != 2 {
		t.Fatalf("应有 2 条任务，实际 %d 条", n)
	}
	if len(ins.checked) != 2 {
		t.Fatalf("每次提交都应做前置检查，实际 %v", ins.checked)
	}
}

// TestInstallSysDepsRejectsEmptyKey 空 Key 是客户端 bug，必须在提交前拦掉：
// 放过去就会变成一个"不知道要装什么"的任务。
func TestInstallSysDepsRejectsEmptyKey(t *testing.T) {
	a, st := newSysDepsTestApp(t, &fakeSysDepsInstaller{})

	_, err := a.InstallSysDeps(context.Background(), "   ")
	if got := apperr.CodeOf(err); got != apperr.CodeInvalidParam {
		t.Fatalf("错误码 = %q，期望 %q：%v", got, apperr.CodeInvalidParam, err)
	}
	if n := countJobs(t, st); n != 0 {
		t.Fatalf("参数非法时不得落任务，实际 %d 条", n)
	}
}

// TestRunInstallSysDeps 处理器按 Key 安装，并把结果如实反映到任务上。
func TestRunInstallSysDeps(t *testing.T) {
	ctx := context.Background()

	t.Run("成功", func(t *testing.T) {
		ins := &fakeSysDepsInstaller{cmd: "apt-get install -y targetcli-fb"}
		a, _ := newSysDepsTestApp(t, ins)

		var last int
		err := a.runInstallSysDeps(ctx, &domain.Job{
			ID: "j1", Type: domain.JobInstallDeps, RefID: "lio_tools",
			Payload: `{"key":"lio_tools"}`,
		}, job.ReporterFunc(func(p int) { last = p }))
		if err != nil {
			t.Fatalf("安装应成功：%v", err)
		}
		if len(ins.installed) != 1 || ins.installed[0] != "lio_tools" {
			t.Fatalf("应按 Key 安装，实际 %v", ins.installed)
		}
		if last != 100 {
			t.Fatalf("结束时应上报 100，实际 %d（前端据此把进度条收尾）", last)
		}
	})

	// RefID 是 Key 的冗余副本（见 domain.JobInstallDeps）：payload 缺失时仍要能装上，
	// 否则历史任务或手工造的任务会变成"空转成功"——任务显示成功但什么都没装。
	t.Run("无 payload 时回退到 RefID", func(t *testing.T) {
		ins := &fakeSysDepsInstaller{}
		a, _ := newSysDepsTestApp(t, ins)

		if err := a.runInstallSysDeps(ctx, &domain.Job{
			ID: "j2", RefID: "lvm_tools",
		}, job.ReporterFunc(func(int) {})); err != nil {
			t.Fatalf("应回退到 RefID 并成功：%v", err)
		}
		if len(ins.installed) != 1 || ins.installed[0] != "lvm_tools" {
			t.Fatalf("应安装 RefID 指定的依赖，实际 %v", ins.installed)
		}
	})

	t.Run("安装失败必须上抛", func(t *testing.T) {
		ins := &fakeSysDepsInstaller{installErr: apperr.SysDepsInstallFailed()}
		a, _ := newSysDepsTestApp(t, ins)

		err := a.runInstallSysDeps(ctx, &domain.Job{ID: "j3", RefID: "lio_tools"},
			job.ReporterFunc(func(int) {}))
		if got := apperr.CodeOf(err); got != apperr.SysDepsInstallFailed().Code {
			t.Fatalf("错误码 = %q，期望 %q（任务必须落成失败）：%v",
				got, apperr.SysDepsInstallFailed().Code, err)
		}
	})

	t.Run("payload 损坏", func(t *testing.T) {
		a, _ := newSysDepsTestApp(t, &fakeSysDepsInstaller{})

		err := a.runInstallSysDeps(ctx, &domain.Job{
			ID: "j4", RefID: "lio_tools", Payload: "{",
		}, job.ReporterFunc(func(int) {}))
		if got := apperr.CodeOf(err); got != apperr.CodeInvalidParam {
			t.Fatalf("错误码 = %q，期望 %q：%v", got, apperr.CodeInvalidParam, err)
		}
	})
}
