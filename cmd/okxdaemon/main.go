package main

// okxdaemon —— 定时策略守护进程（原来的 run.go）
//
// 三层架构下的位置：这是「接口层」的可执行入口，只干两件事：
//   1. 把配置装载起来（internal/conf）
//   2. 起 cron 调度（internal/handler/cron.go）
// 具体策略逻辑全在 internal/service。

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"finally-main/internal/conf"
	"finally-main/internal/handler"
	"finally-main/internal/logx"
)

func main() {
	var (
		listOnly = flag.Bool("list", false, "只打印调度表，不真的跑")
		cfgPath  = flag.String("config", "", "策略配置文件路径（默认自动查找 configs/okx_strategy.json）")
	)
	flag.Parse()

	if *cfgPath != "" {
		os.Setenv("OKX_STRATEGY_CONFIG", *cfgPath)
	}

	cfg := conf.LoadConfig() // 内部会把配置注入 logx，并打第一条日志
	p := conf.ConfigPath()

	fmt.Println("========================================================")
	fmt.Println(" OKX 定时策略守护进程")
	fmt.Println("--------------------------------------------------------")
	fmt.Printf(" 项目根目录 : %s\n", projectRoot())
	fmt.Printf(" 配置文件   : %s\n", p)
	fmt.Printf(" 模拟盘     : %v\n", cfg.OKX.Simulated)
	fmt.Printf(" dry_run    : %v（true = 只算信号不下单）\n", cfg.DryRun)
	fmt.Printf(" 启用周期   : %v\n", cfg.BarsEnabled)
	fmt.Println("========================================================")

	specs := handler.DefaultSpecs()
	fmt.Println("调度表：")
	for _, s := range specs {
		fmt.Printf("  %-14s %s\n", s.Name, s.Spec)
	}
	if *listOnly {
		fmt.Println("（-list：只看不跑，退出）")
		return
	}

	logx.Logf("INFO", "守护进程启动，共 %d 个定时任务", len(specs))
	handler.RunForever()
}

// projectRoot 从当前目录往上找 go.mod
func projectRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	start := dir
	for i := 0; i < 6; i++ {
		if _, e := os.Stat(filepath.Join(dir, "go.mod")); e == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return start
}
