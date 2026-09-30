package service

// ai.go —— AI 解读（对应文案 §8）
//
// 只在「真的下单」时调用，一天最多 max_calls_per_day 次。
// 任何失败都只写日志，绝不影响交易主流程。

import (
	"bytes"
	"encoding/json"
	"finally-main/internal/conf"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type aiCounter struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

var aiMu sync.Mutex

func aiCounterPath(cfg *conf.Config) string {
	// 数据库换成 MySQL 后不再有 .db 文件，AI 调用计数改放到 <项目根>/data/ai_calls.json
	dir := cfg.Resolve(filepath.Join("data"))
	if dir == "" || dir == "." {
		dir = cfg.Resolve(filepath.Join(cfg.Dir(), "data"))
	}
	return filepath.Join(dir, "ai_calls.json")
}

func aiToday() string { return time.Now().Format("2006-01-02") }

func aiLoadCount(cfg *conf.Config) int {
	b, err := os.ReadFile(aiCounterPath(cfg))
	if err != nil {
		return 0
	}
	var c aiCounter
	if json.Unmarshal(b, &c) != nil || c.Date != aiToday() {
		return 0
	}
	return c.Count
}

func aiBump(cfg *conf.Config, n int) {
	p := aiCounterPath(cfg)
	os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.Marshal(aiCounter{Date: aiToday(), Count: n})
	os.WriteFile(p, b, 0o644)
}

// AIReview 调大模型点评一次信号。返回点评文本；err != nil 时文本为空，调用方忽略即可。
func AIReview(cfg *conf.Config, cli *OKXClient, sig *Signal, price float64) (string, error) {
	if cfg.AI == nil || !cfg.AI.Enabled {
		return "", fmt.Errorf("AI 未启用")
	}
	if strings.TrimSpace(cfg.AI.APIKey) == "" {
		return "", fmt.Errorf("AI 未配置 api_key")
	}
	if !strings.HasPrefix(strings.ToLower(cfg.AI.BaseURL), "http") {
		return "", fmt.Errorf("AI base_url 非法")
	}

	aiMu.Lock()
	defer aiMu.Unlock()

	used := aiLoadCount(cfg)
	if cfg.AI.MaxCallsPerDay > 0 && used >= cfg.AI.MaxCallsPerDay {
		return "", fmt.Errorf("AI 今日调用已达上限 %d", cfg.AI.MaxCallsPerDay)
	}

	prompt := buildAIPrompt(sig, price)
	payload := map[string]interface{}{
		"model": cfg.AI.Model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.3,
		"max_tokens":  200,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(cfg.AI.BaseURL, "/") + "/chat/completions"

	timeout := time.Duration(cfg.AI.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	httpCli := &http.Client{Timeout: timeout}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.AI.APIKey)

	resp, err := httpCli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("响应不是 JSON：%s", truncate(string(raw), 200))
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", fmt.Errorf("接口报错：%s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("接口未返回内容")
	}
	note := strings.TrimSpace(out.Choices[0].Message.Content)
	note = strings.ReplaceAll(note, "\n", " ")
	if len(note) > 200 {
		note = note[:200]
	}
	aiBump(cfg, used+1)
	return note, nil
}

// buildAIPrompt 文案 §8 给的模板，原样用
func buildAIPrompt(sig *Signal, price float64) string {
	return fmt.Sprintf(`你是加密货币合约交易助手。请用一句中文（不超过 40 字）点评这次抄底信号，不要客套话，直说风险。

合约：%s
周期：%s
开仓价：%.6f
8 因子共振：%d/8
命中项：%s
指标值：势能=%.2f 摩擦=%.2f 动能=%.2f RSI=%.1f TD=%d`,
		sig.InstID, sig.Bar, price, sig.Score, sig.HitList,
		sig.Pot, sig.Fri, sig.Kin, sig.Rsi, sig.Td)
}
