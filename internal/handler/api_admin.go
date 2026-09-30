package handler

// api_admin.go —— 运维类接口：回补进度 / 数据库自检 / 健康检查

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"finally-main/internal/model"
	"finally-main/internal/service"
)

// ---------------------------------------------------------------------------
// /api/backfill
// ---------------------------------------------------------------------------

func (s *Server) handleBackfill(w http.ResponseWriter, r *http.Request) (any, error) {
	if r.Method == http.MethodPost {
		var body struct {
			Inst string `json:"inst"`
			Bar  string `json:"bar"`
			All  bool   `json:"all"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		n := 0
		if body.All {
			// 给所有入库过的合约 × 全部周期排队（量大，慎用）
			insts, _ := s.db.ListInstruments()
			for _, it := range insts {
				for _, bar := range service.SupportedBars {
					if s.bf.Enqueue(it.InstID, bar) {
						n++
					}
				}
			}
		} else {
			if body.Inst == "" || !service.IsSupportedBar(body.Bar) {
				return nil, fmt.Errorf("参数不合法：inst=%q bar=%q", body.Inst, body.Bar)
			}
			if s.bf.Enqueue(body.Inst, body.Bar) {
				n = 1
			}
		}
		return map[string]any{"ok": true, "enqueued": n, "queueLen": s.bf.QueueLen()}, nil
	}

	jobs, err := s.db.ListJobs()
	if err != nil {
		return nil, err
	}
	covs, err := s.db.CoverageAll()
	if err != nil {
		return nil, err
	}
	insts, _ := s.db.ListInstruments()
	nameOf := make(map[string]string, len(insts))
	for _, it := range insts {
		nameOf[it.InstID] = it.BaseCcy + "/USDT"
	}
	type covItem struct {
		model.KlineCoverage
		Name string `json:"name"`
	}
	cl := make([]covItem, 0, len(covs))
	for _, c := range covs {
		cl = append(cl, covItem{KlineCoverage: c, Name: nameOf[c.InstID]})
	}
	return map[string]any{
		"ok": true, "days": s.bf.Config().Days,
		"queueLen": s.bf.QueueLen(),
		"jobs":     jobs, "coverage": cl,
	}, nil
}

// ---------------------------------------------------------------------------
// /api/tables & /api/health
// ---------------------------------------------------------------------------

func (s *Server) handleTables(w http.ResponseWriter, r *http.Request) (any, error) {
	counts, err := s.db.TableCounts()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(counts))
	for k := range counts {
		names = append(names, k)
	}
	sort.Strings(names)
	type row struct {
		Name string `json:"name"`
		Rows int64  `json:"rows"`
	}
	out := make([]row, 0, len(names))
	for _, n := range names {
		out = append(out, row{Name: n, Rows: counts[n]})
	}
	return map[string]any{"ok": true, "db": s.db.Path(), "tables": out}, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) (any, error) {
	ok := true
	dbErr := ""
	if _, err := s.db.Tables(); err != nil {
		ok = false
		dbErr = err.Error()
	}
	okxTs, okxErr := s.feed.Ping()
	return map[string]any{
		"ok": ok, "dbError": dbErr,
		"db":    s.db.Path(),
		"okxTs": okxTs, "okxTime": time.UnixMilli(okxTs).Format("2006-01-02 15:04:05"),
		"okxError": fmt.Sprint(okxErr),
		"uptime":   int(time.Since(s.startAt).Seconds()),
	}, nil
}
