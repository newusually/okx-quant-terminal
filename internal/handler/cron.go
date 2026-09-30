package handler

// cron.go —— 定时任务调度（接口层：把外部的时间触发翻译成业务层调用）
//
// 原来这是项目根目录的 run.go（package main）。按三层架构挪到这里：
// 本文件只负责「什么时候调」，具体「调什么」全部交给 internal/service。

import (
	"github.com/robfig/cron/v3"

	"finally-main/internal/logx"
	"finally-main/internal/service"
)

// CronSpec 一个定时任务：cron 表达式 + 要执行的动作
type CronSpec struct {
	Spec string
	Name string
	Fn   func()
}

// DefaultSpecs 默认调度表（与老 run.go 一一对应，注释里是被停用的档位）
//
// cron 表达式是「秒 分 时 日 月 周」六段式 —— 用 cron.WithSeconds() 才认。
func DefaultSpecs() []CronSpec {
	everyMinute := "55 0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30," +
		"31,32,33,34,35,36,37,38,39,40,41,42,43,44,45,46,47,48,49,50,51,52,53,54,55,56,57,58,59 * * * ?"
	return []CronSpec{
		// —— 5m 周期：主策略扫描 ——
		{Spec: "59 0,5,10,15,20,25,30,35,40,45,50,55 * * * ?", Name: "5m策略", Fn: service.Run5},
		// —— 15m 周期：主策略扫描 ——
		{Spec: "20 1,16,31,46 * * * ?", Name: "15m策略", Fn: service.Run15},
		// —— 15m 周期：逐仓浮盈巡检（浮亏自动补仓）——
		{Spec: "20 1,16,31,46 * * * ?", Name: "15m浮盈巡检", Fn: service.GetuplRatio},
		// —— 1m 周期：全平检查（浮盈 >35% 或 浮亏 < -30%）——
		{Spec: everyMinute, Name: "1m全平检查", Fn: service.SellAll},

		// 以下档位与老 run.go 保持一致：默认不启用，需要时把注释打开即可
		// {Spec: everyMinute, Name: "1m策略", Fn: service.Run1},
		// {Spec: "30 2,5,8,11,14,17,20,23,26,29,32,35,38,41,44,47,50,53,56,59 * * * ?", Name: "3m策略", Fn: service.Run3},
		// {Spec: "55 50 0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23 * * ?", Name: "1H策略", Fn: service.Run1H},
		// {Spec: "55 50 1,3,5,7,9,11,13,15,17,19,21,23 * * ?", Name: "2H策略", Fn: service.Run2H},
		// {Spec: "55 50 3,7,11,15,19,23 * * ?", Name: "4H策略", Fn: service.Run4H},
		// {Spec: "55 50 5,11,17,23 * * ?", Name: "6H策略", Fn: service.Run6H},
		// {Spec: "55 50 11,23 * * ?", Name: "12H策略", Fn: service.Run12H},
		// {Spec: "55 50 23 * * ?", Name: "1D策略", Fn: service.Run1D},
		// {Spec: "50 50 0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23 * * ?", Name: "1H资金", Fn: service.Getcashbal},
	}
}

// CronRunner 定时任务宿主
type CronRunner struct {
	c    *cron.Cron
	spec []CronSpec
}

// NewCronRunner 建调度器
func NewCronRunner(specs []CronSpec) *CronRunner {
	return &CronRunner{c: cron.New(cron.WithSeconds()), spec: specs}
}

// Start 注册并启动所有任务
func (r *CronRunner) Start() {
	for _, s := range r.spec {
		s := s
		if _, err := r.c.AddFunc(s.Spec, func() {
			defer func() {
				if rec := recover(); rec != nil {
					logx.Logf("ERROR", "定时任务[%s] panic: %v", s.Name, rec)
				}
			}()
			s.Fn()
		}); err != nil {
			logx.Logf("ERROR", "定时任务[%s] 注册失败（%s）：%v", s.Name, s.Spec, err)
			continue
		}
		logx.Logf("INFO", "定时任务已注册：%s（%s）", s.Name, s.Spec)
	}
	r.c.Start()
}

// Stop 停调度（已在执行的任务不会被打断）
func (r *CronRunner) Stop() { r.c.Stop() }

// RunForever 起调度并阻塞主线程（老的 run.go 语义）
func RunForever() {
	r := NewCronRunner(DefaultSpecs())
	r.Start()
	defer r.Stop()
	select {}
}
