package service

// credentials.go —— 原 Python 版 userinfo.py 的 Go 版
//
// Python 版靠 `from userinfo import User` 取 api_key / secret_key / passphrase / flag，
// 但仓库里根本没有 userinfo.py —— 这也是原来那一堆 .py 脚本（cash.py / sells.py /
// gorun.py / cashhistory.py / getuplRatio.py）压根跑不起来的原因。
//
// Go 版改成从三个地方按顺序找凭据，找不到就明确报错，不再静默崩：

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Credentials 账户凭据。Flag: "1"=模拟盘，"0"=实盘。
type Credentials struct {
	APIKey     string `json:"api_key"`
	SecretKey  string `json:"secret_key"`
	Passphrase string `json:"passphrase"`
	Flag       string `json:"flag"`
	Proxy      string `json:"proxy"`
}

// FlagDemo / FlagLive 语义和 okx SDK 一致
const (
	FlagDemo = "1"
	FlagLive = "0"
)

// candidates 依次尝试的凭据文件位置（对应原 Python 的 ../datas/api.json）
func credentialCandidates(root string) []string {
	return []string{
		filepath.Join(root, "datas", "api.json"),
		filepath.Join(root, "api.json"),
		filepath.Join("..", "datas", "api.json"),
		filepath.Join("datas", "api.json"),
		"api.json",
	}
}

// LoadCredentials 按顺序找凭据：先看环境变量，再看 api.json 文件。
//
// 环境变量：OKX_API_KEY / OKX_SECRET_KEY / OKX_PASSPHRASE / OKX_FLAG / OKX_PROXY
func LoadCredentials(root string) (*Credentials, string, error) {
	if k := strings.TrimSpace(os.Getenv("OKX_API_KEY")); k != "" {
		c := &Credentials{
			APIKey:     k,
			SecretKey:  strings.TrimSpace(os.Getenv("OKX_SECRET_KEY")),
			Passphrase: strings.TrimSpace(os.Getenv("OKX_PASSPHRASE")),
			Flag:       strings.TrimSpace(os.Getenv("OKX_FLAG")),
			Proxy:      strings.TrimSpace(os.Getenv("OKX_PROXY")),
		}
		if c.Flag == "" {
			c.Flag = FlagDemo
		}
		return c, "(环境变量)", nil
	}

	for _, p := range credentialCandidates(root) {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c Credentials
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, p, fmt.Errorf("读取 %s 失败（不是合法 JSON）：%w", p, err)
		}
		if strings.TrimSpace(c.APIKey) == "" {
			return nil, p, fmt.Errorf("%s 里 api_key 是空的", p)
		}
		if c.Flag == "" {
			c.Flag = FlagDemo
		}
		return &c, p, nil
	}
	return nil, "", errors.New("找不到凭据：请设置 OKX_API_KEY 等环境变量，或在 datas/api.json 里写 api_key/secret_key/passphrase")
}

// IsDemo 是否模拟盘
func (c *Credentials) IsDemo() bool { return c.Flag == FlagDemo || c.Flag == "" }

// Mask 打日志用，别把明文 Key 打出去
func (c *Credentials) Mask() string {
	if len(c.APIKey) <= 8 {
		return "****"
	}
	return c.APIKey[:4] + "****" + c.APIKey[len(c.APIKey)-4:]
}
