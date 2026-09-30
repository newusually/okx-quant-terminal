package repo

// db.go —— 模型别名（全项目实体统一收敛到 internal/model）
//
// 历史沿革：本文件原本是 SQLite 的建库 / 建表 / 句柄实现（用 modernc.org/sqlite）。
// 因为要支撑 400+ 合约并发写入，SQLite 的单写者模型成为瓶颈，
// 现已整体切换到 MySQL：句柄与 DDL 搬到 mysql.go，本文件只保留
// 「repo 层对外类型名 ↔ model 层实体」的别名，保证 service / handler 无需改动。

import "finally-main/internal/model"

type (
	// Kline K 线
	Kline = model.Kline
	// Instrument 合约信息
	Instrument = model.Instrument
	// Ticker 行情快照
	Ticker = model.Ticker
	// OpenPosition 在持仓（trade 表 status='open'）
	OpenPosition = model.OpenPosition
	// ClosedTrade 历史仓位
	ClosedTrade = model.ClosedTrade
	// SignalRow 信号
	SignalRow = model.SignalRow
	// BackfillJob 回补进度
	BackfillJob = model.BackfillJob
	// KlineQuery K 线查询条件
	KlineQuery = model.KlineQuery
	// KlineCoverage 某个 (合约,周期) 的覆盖情况
	KlineCoverage = model.KlineCoverage
	// Stats 给前端顶部条用的汇总
	Stats = model.Stats
)
