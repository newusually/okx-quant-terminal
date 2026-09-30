//go:build windows

package main

// service_windows.go —— 把 okxweb 变成真正的 Windows 服务
//
// 为什么非要做这一步：
//   老办法是 start_all.bat 里 `start /min okxweb.exe`，会有三个后遗症：
//     ① 桌面上永远挂着一个黑窗口（cmd 控制台），关掉就把进程带走；
//     ② 开机不会自己起来，每次重启机器都要人去双击一遍 bat；
//     ③ 进程崩了没人管，网页就一直 503，得人肉发现。
//   注册成服务之后这三个问题一次性解决：
//     ① 服务进程没有交互式窗口，任务栏干干净净；
//     ② start=auto 开机自启，不用再碰 bat；
//     ③ 配了「失败自动重启」，崩了 SCM 5 秒后自己拉起来。
//
// 用法（管理员 cmd）：
//   bin\okxweb.exe -install     注册服务（幂等，重复跑只是更新配置）
//   bin\okxweb.exe -uninstall   删除服务
//   net start OKXWeb / net stop OKXWeb
//
// 没带任何参数时：如果当前进程是被 SCM 拉起来的就走服务协议，否则按普通
// 控制台程序跑（开发调试用）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	svcName    = "OKXWeb"
	svcDisplay = "OKX 全合约量化终端（网页 + 数据 + 策略）"
	svcDesc    = "OKX 全合约量化终端：K 线回补 / 实时行情 / 自动买入 / 浮盈止盈，监听 127.0.0.1:8090，由 Apache(80) 反代对外。"
)

// runningAsService 当前进程是不是被 Windows 服务控制器拉起的
func runningAsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// svcHandler 把「一个会阻塞的 run(ctx)」适配成 SCM 要的 Execute 协议
type svcHandler struct {
	run func(ctx context.Context) error
}

func (h *svcHandler) Execute(args []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()

	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case err := <-done:
			// run 自己退出了：正常退出报 Stopped，异常退出让 SCM 按恢复策略重启
			status <- svc.Status{State: svc.Stopped}
			if err != nil {
				return false, 1
			}
			return false, 0

		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				// 给收尾留点时间（关 http、停回补），最多 20 秒
				select {
				case <-done:
				case <-time.After(20 * time.Second):
				}
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		}
	}
}

// runAsService 交给 SCM，阻塞到服务停止
func runAsService(run func(ctx context.Context) error) error {
	return svc.Run(svcName, &svcHandler{run: run})
}

// ---------------------------------------------------------------------------
// 安装 / 卸载
// ---------------------------------------------------------------------------

// installService 注册服务：自动启动 + 崩溃自动重启。幂等。
func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("取可执行文件路径失败：%w", err)
	}
	if abs, aerr := filepath.Abs(exe); aerr == nil {
		exe = abs
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连不上服务管理器（请用「管理员身份」运行）：%w", err)
	}
	defer m.Disconnect()

	cfg := mgr.Config{
		DisplayName: svcDisplay,
		Description: svcDesc,
		StartType:   mgr.StartAutomatic,
	}

	s, err := m.OpenService(svcName)
	created := false
	if err == nil {
		// 已存在 → 更新可执行路径与配置（换壳 exe 后重跑本命令即可）
		if uerr := s.UpdateConfig(cfg); uerr != nil {
			s.Close()
			return fmt.Errorf("更新服务配置失败：%w", uerr)
		}
	} else {
		s, err = m.CreateService(svcName, exe, cfg, "-service")
		if err != nil {
			return fmt.Errorf("创建服务失败：%w", err)
		}
		created = true
	}
	defer s.Close()

	// 崩溃自动重启：前两次隔 5 秒，之后隔 30 秒；稳定跑满 1 天计数器归零
	ra := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}
	if rerr := s.SetRecoveryActions(ra, 86400); rerr != nil {
		fmt.Printf("⚠ 设置「崩溃自动重启」失败（不影响使用）：%v\n", rerr)
	}
	// 让「服务自己报错退出」也算失败，同样触发重启
	_ = s.SetRecoveryActionsOnNonCrashFailures(true)

	if created {
		fmt.Printf("✔ 服务 %s 已注册\n", svcName)
	} else {
		fmt.Printf("✔ 服务 %s 已存在，配置已更新\n", svcName)
	}
	fmt.Printf("  可执行文件 : %s\n", exe)
	fmt.Printf("  启动方式   : 自动（开机自启）\n")
	fmt.Printf("  失败恢复   : 5s / 5s / 30s 自动重启\n")

	// 没在跑就顺手拉起来
	st, qerr := s.Query()
	if qerr == nil && st.State != svc.Running {
		if serr := s.Start(); serr != nil {
			fmt.Printf("⚠ 服务启动失败（可能是 8090 已被占用）：%v\n", serr)
			fmt.Printf("  手工重试：net start %s\n", svcName)
			return nil
		}
		fmt.Printf("✔ 服务已启动，浏览器打开 http://localhost/\n")
	} else if qerr == nil {
		fmt.Printf("✔ 服务已在运行\n")
	}
	return nil
}

// uninstallService 停止并删除服务
func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连不上服务管理器（请用「管理员身份」运行）：%w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(svcName)
	if err != nil {
		return fmt.Errorf("服务 %s 不存在：%w", svcName, err)
	}
	defer s.Close()

	if st, qerr := s.Query(); qerr == nil && st.State != svc.Stopped {
		_, _ = s.Control(svc.Stop)
		// 等它真的停下来，最多 20 秒
		for i := 0; i < 40; i++ {
			time.Sleep(500 * time.Millisecond)
			if st, e := s.Query(); e == nil && st.State == svc.Stopped {
				break
			}
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("删除服务失败：%w", err)
	}
	fmt.Printf("✔ 服务 %s 已删除\n", svcName)
	return nil
}
