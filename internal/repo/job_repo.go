package repo

// job_repo.go —— 回补任务进度仓储（MySQL 版）
//
// 400 合约 × 6 周期 = 2400 个 (合约,周期) 任务行，
// 回补过程中每完成一批就刷新一次进度。批量写用一条 upsert 搞定。

import (
	"time"
)

// jobCols backfill_job 表列序
var jobCols = []string{
	"inst_id", "bar", "from_ts", "to_ts", "rows_cnt", "status", "msg", "updated_at",
}

var jobUpdateCols = []string{"from_ts", "to_ts", "rows_cnt", "status", "msg", "updated_at"}

// ---------------------------------------------------------------------------
// backfill 进度
// ---------------------------------------------------------------------------

// SaveJob 记录单个回补进度
func (d *DB) SaveJob(j BackfillJob) error {
	_, err := d.SaveJobs([]BackfillJob{j})
	return err
}

// SaveJobs 批量记录回补进度（回补收尾时一次性刷 2400 行）
func (d *DB) SaveJobs(list []BackfillJob) (int, error) {
	if len(list) == 0 {
		return 0, nil
	}
	now := time.Now().UnixMilli()
	args := make([][]any, 0, len(list))
	for _, j := range list {
		upd := j.UpdatedAt
		if upd == 0 {
			upd = now
		}
		args = append(args, []any{
			j.InstID, j.Bar, j.FromTs, j.ToTs, j.Rows, j.Status, j.Msg, upd,
		})
	}
	return d.bulkUpsert("backfill_job", jobCols, args, jobUpdateCols)
}

// ListJobs 回补进度列表
func (d *DB) ListJobs() ([]BackfillJob, error) {
	rows, err := d.sql.Query(`SELECT inst_id,bar,COALESCE(from_ts,0),COALESCE(to_ts,0),
		COALESCE(rows_cnt,0),COALESCE(status,''),COALESCE(msg,''),COALESCE(updated_at,0)
		FROM backfill_job ORDER BY updated_at DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BackfillJob, 0, 512)
	for rows.Next() {
		var j BackfillJob
		if err := rows.Scan(&j.InstID, &j.Bar, &j.FromTs, &j.ToTs, &j.Rows, &j.Status,
			&j.Msg, &j.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobSummary 回补进度汇总（done / running / pending 各多少）
func (d *DB) JobSummary() (map[string]int64, error) {
	rows, err := d.sql.Query(
		`SELECT COALESCE(status,''), COUNT(*) FROM backfill_job GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}
