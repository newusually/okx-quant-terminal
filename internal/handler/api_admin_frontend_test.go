package handler

// api_admin_frontend_test.go —— 管理台「前端按键 / 后端认键」的跨层对齐测试
//
// ★ 为什么值得单独写一条 ★
//
// 管理台是三层拼起来的，任何一层对不上，症状都是**不报错的**：
//
//	前端 collect() 发出 p.foo = 1   → 后端 ParsePatch 不认识 "foo" → 静默丢弃
//	                                → 接口返回 ok:true → 页面提示"已保存"
//	                                → 用户回来一看：还是旧值。
//
// 这就是本项目反复踩的「改了没用」在管理台上的形状，而且**比一般情况更糟**：
// 它伪装成"保存成功"，用户会以为是自己没点对。
//
// 第二层更隐蔽：表单初值来自 GET /api/admin/config 的分区结构，
// 如果 JS 读的是 `g.cooldown_bars` 而后端下发的键叫 `gate.cooldown_bars2`，
// 拿到的是 undefined → 输入框显示成默认值 → **用户一保存就用默认值覆盖了真值**。
//
// 所以这里用两个"从源码里抽符号"的检查把它们钉死：
//
//	A. JS 里 write 的每个键（`p.X = ...`）都必须是 service.Patch 的 json tag；
//	B. JS 里 read 的每个键（`b.X` / `g.X` / `a.X` ...）都必须在 GET 响应
//	   对应分区里真实存在。分区映射直接从 JS 的
//	   `var g = cfg.gate || {}` 这行解析，不手抄。
//
// 三层（Patch tag / GET 分区 / JS 表单）中任意两层发生漂移，这里就会红。

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"finally-main/internal/service"
)

const adminJSRelPath = "../../web/assets/admin.js"

// jsSource 读管理台前端源码；读不到就让调用方 t.Skip（比如只带后端二进制跑测试）。
func jsSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(adminJSRelPath)
	if err != nil {
		t.Skipf("读不到 %s（%v），跳过跨层对齐检查", adminJSRelPath, err)
	}
	return string(raw)
}

// patchTags 用反射取出 service.Patch 全部 json tag —— 后端真正认识的键，
// 一个不落。手抄一份的话，这份清单本身就会过期。
func patchTags(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	rt := reflect.TypeOf(service.Patch{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		out[name] = f.Name
	}
	if len(out) < 30 {
		t.Fatalf("只解析出 %d 个 Patch tag，明显不对（是不是改了结构体？）", len(out))
	}
	return out
}

// jsWriteKeys 抽出 collect() 里写请求体的键：`p.<key> = ...`
var jsWriteKeyRE = regexp.MustCompile(`\bp\.([a-z0-9_]+)\s*=`)

// jsSectionVars 抽 `var g = cfg.gate || {}` 这种映射，得到 变量名 → 分区名
var jsSectionVarRE = regexp.MustCompile(`var\s+([A-Za-z_$][\w$]*)\s*=\s*cfg\.([a-z0-9_]+)\s*\|\|\s*\{\}`)

// TestAdminFrontend_PatchKeysAreAllAccepted
//
// A 层：JS 写出去的每个键，后端 ParsePatch 都必须认得。
func TestAdminFrontend_PatchKeysAreAllAccepted(t *testing.T) {
	js := jsSource(t)
	tags := patchTags(t)

	seen := map[string]bool{}
	for _, m := range jsWriteKeyRE.FindAllStringSubmatch(js, -1) {
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		t.Fatalf("没能从 %s 里抽出任何 `p.X = ...` 赋值 —— "+
			"要么前端被大改，要么正则过期了；两种情况都必须人工确认", adminJSRelPath)
	}

	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var unknown []string
	for _, k := range keys {
		if _, ok := tags[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		t.Fatalf("前端在发这些键，但 service.Patch 里没有对应的 json tag：%v\n"+
			"这些字段会被 ParsePatch **静默丢弃**，页面却提示保存成功 —— "+
			"正是「改了没用」最隐蔽的一种。请改名字或补 tag。", unknown)
	}
	t.Logf("✓ 前端写出的 %d 个键全部被 Patch 接受：%s", len(keys), strings.Join(keys, ", "))
}

// TestAdminFrontend_ReadKeysExistInGetConfig
//
// B 层：JS 从 GET /api/admin/config 读的每个键，都得在对应分区里真实存在。
func TestAdminFrontend_ReadKeysExistInGetConfig(t *testing.T) {
	js := jsSource(t)

	// 1) 变量名 → 分区名（从 JS 自己解析，不手抄）
	varToSect := map[string]string{}
	for _, m := range jsSectionVarRE.FindAllStringSubmatch(js, -1) {
		varToSect[m[1]] = m[2]
	}
	if len(varToSect) < 6 {
		t.Fatalf("只解析出 %d 个 `var X = cfg.Y || {}` 映射，前端结构可能变了：%v",
			len(varToSect), varToSect)
	}

	// 2) 从 handler 源码里抽每个分区的键
	hsrc, err := os.ReadFile("api_admin_config.go")
	if err != nil {
		t.Skipf("读不到 api_admin_config.go（%v），跳过", err)
	}
	gotSect := map[string]map[string]bool{}
	for _, sect := range varToSect {
		if _, done := gotSect[sect]; done {
			continue
		}
		gotSect[sect] = extractSectionKeys(string(hsrc), sect)
	}

	// 3) 逐个变量核对
	total := 0
	for v, sect := range varToSect {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(v) + `\.([a-z0-9_]+)`)
		keys := map[string]bool{}
		for _, m := range re.FindAllStringSubmatch(js, -1) {
			keys[m[1]] = true
		}
		if len(keys) == 0 {
			continue
		}
		var missing []string
		for k := range keys {
			if !gotSect[sect][k] {
				missing = append(missing, k)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			have := make([]string, 0, len(gotSect[sect]))
			for k := range gotSect[sect] {
				have = append(have, k)
			}
			sort.Strings(have)
			t.Fatalf("分区 %q：JS 在读这些键，但 GET /api/admin/config 没下发：%v\n"+
				"下发的键是：%v\n"+
				"→ 前端会拿到 undefined、输入框显示成默认值，"+
				"用户一保存就用默认值覆盖真值。", sect, missing, have)
		}
		total += len(keys)
	}
	t.Logf("✓ 前端从 %d 个分区读的 %d 个键，后端全部有下发", len(varToSect), total)
}

// extractSectionKeys 从 handleAdminGetConfig 的源码里，
// 抽出 `"<sect>": map[string]any{ ... }` 这块里的所有字符串键。
//
// 实现：找到起始位置后按花括号配平扫到块尾，块内用正则取 `"key":`。
// 注释（// 之后）先剜掉 —— Go 的注释里会写中文引号句子，
// 不剜的话容易被误当成键（这个文件里就有）。
func extractSectionKeys(src, sect string) map[string]bool {
	anchor := `"` + sect + `": map[string]any{`
	i := strings.Index(src, anchor)
	if i < 0 {
		// 退化形态：`"sect":  map[string]any{`（多空格）
		re := regexp.MustCompile(`"` + regexp.QuoteMeta(sect) + `"\s*:\s*map\[string\]any\{`)
		loc := re.FindStringIndex(src)
		if loc == nil {
			return map[string]bool{}
		}
		i = loc[0]
	}
	open := strings.Index(src[i:], "{")
	if open < 0 {
		return map[string]bool{}
	}
	open += i

	depth := 0
	end := -1
	for p := open; p < len(src); p++ {
		switch src[p] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = p
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return map[string]bool{}
	}

	body := stripLineComments(src[open : end+1])
	keyRE := regexp.MustCompile(`"([a-z0-9_]+)"\s*:`)
	out := map[string]bool{}
	for _, m := range keyRE.FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	return out
}

func stripLineComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if j := strings.Index(ln, "//"); j >= 0 {
			lines[i] = ln[:j]
		}
	}
	return strings.Join(lines, "\n")
}

// TestAdminFrontend_MaxConcurrentPositionsUsesComboRow
//
// 十期专项：用户原话「持仓数量 可以选择也可以输入数量」。
//
// 这条不看正则常量、直接盯住那个控件用对了没有 ——
// 因为「可选择也可输入」和「只是个数字框」在页面上长得几乎一样，
// 只有进了代码才分得清；而走错路线的代价是**用户以为能选，其实只能手输**。
func TestAdminFrontend_MaxConcurrentPositionsUsesComboRow(t *testing.T) {
	js := jsSource(t)

	// 1. 必须用 comboRow 造，而不是 numRow
	m := regexp.MustCompile(`var\s+rowMaxPos\s*=\s*(comboRow|numRow|condRow|selRow)\s*\(`)
	got := m.FindStringSubmatch(js)
	if got == nil {
		t.Fatalf("找不到 rowMaxPos 的构造语句")
	}
	if got[1] != "comboRow" {
		t.Fatalf("rowMaxPos 用的是 %s，但十期要求「可选择也可输入」= comboRow", got[1])
	}

	// 2. 预设里要有 0（不限）和 30（十期口径）
	block := js[strings.Index(js, got[0]):]
	if e := strings.Index(block, "});"); e > 0 {
		block = block[:e]
	}
	for _, need := range []string{`{ v: '0', t: '不限' }`, `{ v: '30', t: '30' }`} {
		if !strings.Contains(block, need) {
			t.Fatalf("预设选项里少了 %s", need)
		}
	}

	// 3. ★ 旧口径的残留物必须清干净 ★
	//    rowCooldown 控件本身、以及 collect() 里发 cooldown_bars 的那行，
	//    都不能再出现在**代码**里（注释里提名字是允许的，所以只查代码形态）。
	if regexp.MustCompile(`rowCooldown\s*\.`).MatchString(js) {
		t.Fatalf("rowCooldown 控件还在被使用 —— 十期要求冷却条件全部删除")
	}
	if regexp.MustCompile(`p\.cooldown_bars\s*=`).MatchString(js) {
		t.Fatalf("collect() 还在发 cooldown_bars —— 十期要求页面不再提这个字段")
	}

	t.Logf("✓ rowMaxPos 用 comboRow（预设含 0/30，可直接输入）；冷却控件与入参已彻底移除")
}
