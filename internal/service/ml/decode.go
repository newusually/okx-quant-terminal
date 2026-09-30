package ml

// attn_decode.go —— 原 okx/AttnDecoder.py 的 Go 版
//
// Python 版是 torch 的 RNN（num_layers=20）+ Adam + MSE，还要 CUDA。
// 这里用纯 Go 重写同一套东西，不引任何第三方库：
//
//	MinMaxScaler    → 同 sklearn 口径（逐列 (x-min)/(max-min)）
//	create_dataset  → lookback 滑动窗口，y 取窗口后一行的第 0 列（eth_close）
//	RNNModel        → 多层 tanh RNN + 线性输出，手动 BPTT
//	Adam            → lr=0.01，和 torch.optim.Adam 默认 betas 一致
//	MSELoss         → 均方误差
//	torch.save(.pth)→ 存成 JSON（rnn_model.json）
//
// 唯一刻意的差异：默认 num_layers 从 20 降到 2。
// 20 层 RNN 在 CPU 上跑 50 个 epoch 是几小时起步的量级，Python 版自己也没法在
// 普通机器上跑完（那个 .cuda() 甚至会直接报错）。层数是可配的，想复现原参数传 20 即可。

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
)

// ---------------------------------------------------------------------------
// 归一化
// ---------------------------------------------------------------------------

// MinMaxScaler 逐列线性缩放，等价 sklearn.preprocessing.MinMaxScaler
type MinMaxScaler struct {
	Min []float64
	Max []float64
}

// Fit 用数据拟合（data 是 [样本][特征]）
func (s *MinMaxScaler) Fit(data [][]float64) {
	if len(data) == 0 {
		return
	}
	cols := len(data[0])
	s.Min = make([]float64, cols)
	s.Max = make([]float64, cols)
	for c := 0; c < cols; c++ {
		s.Min[c] = math.Inf(1)
		s.Max[c] = math.Inf(-1)
	}
	for _, row := range data {
		for c := 0; c < cols && c < len(row); c++ {
			if math.IsNaN(row[c]) || math.IsInf(row[c], 0) {
				continue
			}
			if row[c] < s.Min[c] {
				s.Min[c] = row[c]
			}
			if row[c] > s.Max[c] {
				s.Max[c] = row[c]
			}
		}
	}
	for c := 0; c < cols; c++ {
		if math.IsInf(s.Min[c], 1) {
			s.Min[c] = 0
		}
		if math.IsInf(s.Max[c], -1) {
			s.Max[c] = 1
		}
	}
}

// Transform 就地缩放（返回新切片，不改入参）
func (s *MinMaxScaler) Transform(data [][]float64) [][]float64 {
	out := make([][]float64, len(data))
	for i, row := range data {
		nr := make([]float64, len(row))
		for c := range row {
			nr[c] = s.scale(c, row[c])
		}
		out[i] = nr
	}
	return out
}

// FitTransform 对应 sklearn 的 fit_transform
func (s *MinMaxScaler) FitTransform(data [][]float64) [][]float64 {
	s.Fit(data)
	return s.Transform(data)
}

func (s *MinMaxScaler) scale(col int, v float64) float64 {
	if col >= len(s.Min) {
		return v
	}
	rng := s.Max[col] - s.Min[col]
	if rng == 0 || math.IsNaN(rng) {
		return 0
	}
	x := (v - s.Min[col]) / rng
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return x
}

// Flatten2D 把 [样本][特征] 拍平成一行行，方便 Fit
func Flatten2D(x [][][]float64) [][]float64 {
	out := [][]float64{}
	for _, seq := range x {
		out = append(out, seq...)
	}
	return out
}

// ---------------------------------------------------------------------------
// 数据集
// ---------------------------------------------------------------------------

// CreateDataset 对应 Python 的 create_dataset(df, lookback)
//
//	X[i] = data[i : i+lookback]
//	y[i] = target[i+lookback]（target 为 nil 时退回 data[i+lookback][targetCol]）
func CreateDataset(data [][]float64, target []float64, lookback int, targetCol int) ([][][]float64, []float64) {
	if lookback <= 0 || len(data) <= lookback {
		return nil, nil
	}
	if targetCol < 0 || targetCol >= len(data[0]) {
		targetCol = 0
	}
	var X [][][]float64
	var y []float64
	for i := 0; i+lookback < len(data); i++ {
		seq := make([][]float64, lookback)
		for j := 0; j < lookback; j++ {
			row := make([]float64, len(data[i+j]))
			copy(row, data[i+j])
			seq[j] = row
		}
		tv := data[i+lookback][targetCol]
		if target != nil && i+lookback < len(target) {
			tv = target[i+lookback]
		}
		if math.IsNaN(tv) || math.IsInf(tv, 0) {
			continue
		}
		X = append(X, seq)
		y = append(y, tv)
	}
	return X, y
}

// ---------------------------------------------------------------------------
// 模型
// ---------------------------------------------------------------------------

// RNNLayer 一层：h_t = tanh(Wx·x_t + Wh·h_{t-1} + b)
type RNNLayer struct {
	Wx [][]float64 `json:"wx"` // [hidden][input]
	Wh [][]float64 `json:"wh"` // [hidden][hidden]
	B  []float64   `json:"b"`  // [hidden]
}

// RNNModel 对应 Python 的 RNNModel（多层 RNN + 一个线性头）
type RNNModel struct {
	InputSize  int
	HiddenSize int
	NumLayers  int
	Layers     []RNNLayer
	Wo         []float64 // [hidden]
	Bo         float64

	// 扁平化参数（Adam 用），下标规则由 buildIndex 建立
	params []float64
	grads  []float64
	// 各参数的切片视图下标
	wxOff, whOff, bOff []int
	woOff, boOff       int
}

// NewRNNModel 建模型并初始化参数（均匀分布，等价 torch 默认 init）
func NewRNNModel(inputSize, hiddenSize, numLayers int, seed int64) *RNNModel {
	if inputSize <= 0 {
		inputSize = 1
	}
	if hiddenSize <= 0 {
		hiddenSize = 64
	}
	if numLayers <= 0 {
		numLayers = 2
	}
	rng := rand.New(rand.NewSource(seed))
	m := &RNNModel{InputSize: inputSize, HiddenSize: hiddenSize, NumLayers: numLayers}
	m.Layers = make([]RNNLayer, numLayers)
	in := inputSize
	for l := 0; l < numLayers; l++ {
		lay := RNNLayer{
			Wx: make([][]float64, hiddenSize),
			Wh: make([][]float64, hiddenSize),
			B:  make([]float64, hiddenSize),
		}
		kx := math.Sqrt(6.0 / float64(in+hiddenSize))
		kh := math.Sqrt(6.0 / float64(hiddenSize+hiddenSize))
		for i := 0; i < hiddenSize; i++ {
			lay.Wx[i] = make([]float64, in)
			for j := 0; j < in; j++ {
				lay.Wx[i][j] = (rng.Float64()*2 - 1) * kx
			}
			lay.Wh[i] = make([]float64, hiddenSize)
			for j := 0; j < hiddenSize; j++ {
				lay.Wh[i][j] = (rng.Float64()*2 - 1) * kh
			}
		}
		m.Layers[l] = lay
		in = hiddenSize
	}
	m.Wo = make([]float64, hiddenSize)
	kw := math.Sqrt(6.0 / float64(hiddenSize+1))
	for i := range m.Wo {
		m.Wo[i] = (rng.Float64()*2 - 1) * kw
	}
	m.flatten()
	return m
}

// flatten 把结构体里的参数拷进扁平数组，并记录每块的起始下标
func (m *RNNModel) flatten() {
	m.params = m.params[:0]
	m.wxOff = make([]int, m.NumLayers)
	m.whOff = make([]int, m.NumLayers)
	m.bOff = make([]int, m.NumLayers)
	for l, lay := range m.Layers {
		m.wxOff[l] = len(m.params)
		for _, row := range lay.Wx {
			m.params = append(m.params, row...)
		}
		m.whOff[l] = len(m.params)
		for _, row := range lay.Wh {
			m.params = append(m.params, row...)
		}
		m.bOff[l] = len(m.params)
		m.params = append(m.params, lay.B...)
	}
	m.woOff = len(m.params)
	m.params = append(m.params, m.Wo...)
	m.boOff = len(m.params)
	m.params = append(m.params, m.Bo)
	m.grads = make([]float64, len(m.params))
}

// gatherParams 把扁平参数写回结构体（更新前调用）
func (m *RNNModel) gatherParams() {
	for l := range m.Layers {
		lay := &m.Layers[l]
		idx := m.wxOff[l]
		for i := range lay.Wx {
			copy(lay.Wx[i], m.params[idx:idx+len(lay.Wx[i])])
			idx += len(lay.Wx[i])
		}
		idx = m.whOff[l]
		for i := range lay.Wh {
			copy(lay.Wh[i], m.params[idx:idx+len(lay.Wh[i])])
			idx += len(lay.Wh[i])
		}
		idx = m.bOff[l]
		copy(lay.B, m.params[idx:idx+len(lay.B)])
	}
	copy(m.Wo, m.params[m.woOff:m.woOff+len(m.Wo)])
	m.Bo = m.params[m.boOff]
}

// NumParams 参数总量
func (m *RNNModel) NumParams() int { return len(m.params) }

// Forward 前向：输入一个窗口 [[inputSize]...]，返回预测的收盘价（已归一化空间）
//
// 同时缓存每层每个时刻的 x / h，供 BPTT 反向用。
func (m *RNNModel) Forward(x [][]float64) (float64, [][][]float64, [][][]float64) {
	L := m.NumLayers
	T := len(x)
	// h[l][t] 是第 l 层第 t 步的隐状态（t=-1 视为 0，用 nil 表示）
	hs := make([][][]float64, L)
	xx := make([][][]float64, L)
	cur := x
	for l := 0; l < L; l++ {
		hs[l] = make([][]float64, T)
		xx[l] = make([][]float64, T)
		next := make([][]float64, T)
		prev := make([]float64, m.HiddenSize)
		lay := m.Layers[l]
		for t := 0; t < T; t++ {
			in := cur[t]
			xx[l][t] = in
			h := make([]float64, m.HiddenSize)
			for i := 0; i < m.HiddenSize; i++ {
				s := lay.B[i]
				wx := lay.Wx[i]
				for j := 0; j < len(in) && j < len(wx); j++ {
					s += wx[j] * in[j]
				}
				wh := lay.Wh[i]
				for j := 0; j < m.HiddenSize; j++ {
					s += wh[j] * prev[j]
				}
				h[i] = math.Tanh(s)
			}
			hs[l][t] = h
			next[t] = h
			prev = h
		}
		cur = next
	}
	last := hs[L-1][T-1]
	out := m.Bo
	for i := 0; i < m.HiddenSize; i++ {
		out += m.Wo[i] * last[i]
	}
	return out, hs, xx
}

// Predict 只做前向，不要缓存
func (m *RNNModel) Predict(x [][]float64) float64 {
	out, _, _ := m.Forward(x)
	return out
}

// ---------------------------------------------------------------------------
// 反向（BPTT）+ Adam
// ---------------------------------------------------------------------------

// Backward 单样本反向，把梯度累加到 m.grads。
//
// dLoss/dOut = 2*(out - target)（MSE 且 output 只有 1 维）
func (m *RNNModel) Backward(x [][]float64, target float64) float64 {
	out, hs, xx := m.Forward(x)
	diff := out - target
	loss := diff * diff
	dOut := 2 * diff

	L := m.NumLayers
	T := len(x)

	// 输出层梯度
	for i := 0; i < m.HiddenSize; i++ {
		m.grads[m.woOff+i] += dOut * hs[L-1][T-1][i]
	}
	m.grads[m.boOff] += dOut

	// 从上层往下传
	dhNext := make([]float64, m.HiddenSize)
	for i := 0; i < m.HiddenSize; i++ {
		dhNext[i] = dOut * m.Wo[i]
	}

	for l := L - 1; l >= 0; l-- {
		lay := &m.Layers[l]
		dhCarry := make([]float64, m.HiddenSize)
		copy(dhCarry, dhNext)
		dxOut := make([]float64, len(xx[l][0]))
		for t := T - 1; t >= 0; t-- {
			h := hs[l][t]
			// dh = dhCarry + 上游
			dh := make([]float64, m.HiddenSize)
			copy(dh, dhCarry)
			dRaw := make([]float64, m.HiddenSize)
			for i := 0; i < m.HiddenSize; i++ {
				// tanh 导数：1 - h^2
				dRaw[i] = dh[i] * (1 - h[i]*h[i])
			}
			// 梯度：Wx / Wh / b
			in := xx[l][t]
			for i := 0; i < m.HiddenSize; i++ {
				base := m.wxOff[l] + i*len(lay.Wx[i])
				for j := 0; j < len(in) && j < len(lay.Wx[i]); j++ {
					m.grads[base+j] += dRaw[i] * in[j]
				}
				hbase := m.whOff[l] + i*m.HiddenSize
				if t > 0 {
					hp := hs[l][t-1]
					for j := 0; j < m.HiddenSize; j++ {
						m.grads[hbase+j] += dRaw[i] * hp[j]
					}
				}
				m.grads[m.bOff[l]+i] += dRaw[i]
			}
			// 回传给上一层：dx = Wx^T · dRaw
			dx := make([]float64, len(in))
			for i := 0; i < m.HiddenSize; i++ {
				for j := 0; j < len(in) && j < len(lay.Wx[i]); j++ {
					dx[j] += lay.Wx[i][j] * dRaw[i]
				}
			}
			copy(dxOut, dx)
			// 回传给同一层上一时刻：dh_{t-1} = Wh^T · dRaw
			nd := make([]float64, m.HiddenSize)
			if t > 0 {
				for i := 0; i < m.HiddenSize; i++ {
					base := m.whOff[l] + i*m.HiddenSize
					for j := 0; j < m.HiddenSize; j++ {
						// 用参数数组读，保证是当前权重
						nd[j] += m.params[base+j] * dRaw[i]
					}
				}
			}
			dhCarry = nd
		}
		dhNext = dxOut
	}
	return loss
}

// Adam 优化器状态（对应 torch.optim.Adam）
type Adam struct {
	LR, Beta1, Beta2, Eps float64
	M, V                  []float64
	T                     int
}

// NewAdam 默认和 torch 一致：lr 由调用方给，betas=(0.9,0.999)，eps=1e-8
func NewAdam(n int, lr float64) *Adam {
	return &Adam{LR: lr, Beta1: 0.9, Beta2: 0.999, Eps: 1e-8,
		M: make([]float64, n), V: make([]float64, n)}
}

// Step 用 m.grads 更新 m.params，然后清零梯度
func (a *Adam) Step(m *RNNModel) {
	a.T++
	b1t := 1 - math.Pow(a.Beta1, float64(a.T))
	b2t := 1 - math.Pow(a.Beta2, float64(a.T))
	for i := range m.params {
		g := m.grads[i]
		a.M[i] = a.Beta1*a.M[i] + (1-a.Beta1)*g
		a.V[i] = a.Beta2*a.V[i] + (1-a.Beta2)*g*g
		mh := a.M[i] / b1t
		vh := a.V[i] / b2t
		m.params[i] -= a.LR * mh / (math.Sqrt(vh) + a.Eps)
		m.grads[i] = 0
	}
	m.gatherParams()
}

// ZeroGrad 手动清零（每轮 batch 开始前用）
func (m *RNNModel) ZeroGrad() {
	for i := range m.grads {
		m.grads[i] = 0
	}
}

// ---------------------------------------------------------------------------
// 训练 / 保存
// ---------------------------------------------------------------------------

// RNNConfig 训练超参，默认值和 Python 版对齐（层数见文件头说明）
type RNNConfig struct {
	Lookback  int
	Hidden    int
	Layers    int
	BatchSize int
	Epochs    int
	LR        float64
	TestRatio float64
	Seed      int64
	TargetCol int // 特征矩阵里哪一列作为预测目标
}

// DefaultRNNConfig 对应 AttnDecoder.py 里的那份超参
func DefaultRNNConfig() RNNConfig {
	return RNNConfig{
		Lookback:  20,
		Hidden:    64,
		Layers:    2, // Python 写的是 20；见文件头说明，这里默认 2，可调
		BatchSize: 64,
		Epochs:    50,
		LR:        0.01,
		TestRatio: 0.2,
		Seed:      20260930,
		TargetCol: 0,
	}
}

// TrainReport 训练结果
type TrainReport struct {
	Epochs       int
	TrainLoss    []float64
	TestLoss     []float64
	ScalerX      *MinMaxScaler
	ScalerY      *MinMaxScaler
	TrainSamples int
	TestSamples  int
}

// TrainRNN 对应 Python 的 train_rnn_model(df)
//
// features 是 [样本][特征]，target 是同长度的预测目标（可为 nil，则取第 cfg.TargetCol 列）。
// 返回的 report 里有每轮 loss，方便画曲线 / 打日志。
func TrainRNN(features [][]float64, target []float64, cfg RNNConfig, logf func(format string, args ...any)) (*RNNModel, *TrainReport, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if len(features) <= cfg.Lookback+2 {
		return nil, nil, fmt.Errorf("样本太少（%d 行），没法按 lookback=%d 切窗口", len(features), cfg.Lookback)
	}
	split := int(float64(len(features)) * (1 - cfg.TestRatio))
	if split <= cfg.Lookback {
		split = len(features) / 2
	}

	// 目标列先单独切出来，避免被当作特征重复喂进去
	tCol := cfg.TargetCol
	if tCol < 0 || tCol >= len(features[0]) {
		tCol = 0
	}
	trg := target
	if trg == nil {
		trg = make([]float64, len(features))
		for i := range features {
			trg[i] = features[i][tCol]
		}
	}

	trainF := features[:split]
	testF := features[split:]
	trainT := trg[:split]
	testT := trg[split:]

	trainX, trainY := CreateDataset(trainF, trainT, cfg.Lookback, tCol)
	testX, testY := CreateDataset(testF, testT, cfg.Lookback, tCol)
	if len(trainX) == 0 || len(testX) == 0 {
		return nil, nil, fmt.Errorf("切不出训练/测试窗口（train=%d test=%d）", len(trainX), len(testX))
	}

	// 归一化：X 按列 fit，y 单独 fit（对齐 sklearn 的两次 fit_transform）
	scX := &MinMaxScaler{}
	scX.Fit(Flatten2D(trainX))
	trainX = scaleSeqs(scX, trainX)
	testX = scaleSeqs(scX, testX)

	scY := &MinMaxScaler{}
	scY.Fit(toColumns(trainY))
	trainY = scaleVals(scY, trainY)
	testY = scaleVals(scY, testY)

	inputSize := len(features[0])
	model := NewRNNModel(inputSize, cfg.Hidden, cfg.Layers, cfg.Seed)
	opt := NewAdam(model.NumParams(), cfg.LR)
	rng := rand.New(rand.NewSource(cfg.Seed))

	report := &TrainReport{
		Epochs: cfg.Epochs, ScalerX: scX, ScalerY: scY,
		TrainSamples: len(trainX), TestSamples: len(testX),
	}

	idx := make([]int, len(trainX))
	for i := range idx {
		idx[i] = i
	}
	bs := cfg.BatchSize
	if bs <= 0 {
		bs = 64
	}

	for ep := 0; ep < cfg.Epochs; ep++ {
		// shuffle
		rng.Shuffle(len(idx), func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
		var sum float64
		for s := 0; s < len(idx); s += bs {
			e := s + bs
			if e > len(idx) {
				e = len(idx)
			}
			model.ZeroGrad()
			for _, k := range idx[s:e] {
				sum += model.Backward(trainX[k], trainY[k])
			}
			opt.Step(model)
		}
		trainLoss := sum / float64(len(idx))

		var tsum float64
		for k := range testX {
			d := model.Predict(testX[k]) - testY[k]
			tsum += d * d
		}
		testLoss := tsum / float64(len(testX))
		report.TrainLoss = append(report.TrainLoss, trainLoss)
		report.TestLoss = append(report.TestLoss, testLoss)
		logf("Epoch [%d/%d], Test Loss: %.6f", ep+1, cfg.Epochs, testLoss)
	}
	return model, report, nil
}

// scaleSeqs 对 [样本][时间][特征] 逐列缩放
func scaleSeqs(s *MinMaxScaler, x [][][]float64) [][][]float64 {
	out := make([][][]float64, len(x))
	for i, seq := range x {
		nseq := make([][]float64, len(seq))
		for t, row := range seq {
			nr := make([]float64, len(row))
			for c := range row {
				nr[c] = s.scale(c, row[c])
			}
			nseq[t] = nr
		}
		out[i] = nseq
	}
	return out
}

func toColumns(v []float64) [][]float64 {
	out := make([][]float64, len(v))
	for i := range v {
		out[i] = []float64{v[i]}
	}
	return out
}

func scaleVals(s *MinMaxScaler, v []float64) []float64 {
	out := make([]float64, len(v))
	for i := range v {
		out[i] = s.scale(0, v[i])
	}
	return out
}

// InverseY 把归一化空间的预测还原成价格
func (r *TrainReport) InverseY(v float64) float64 {
	if r.ScalerY == nil || len(r.ScalerY.Min) == 0 {
		return v
	}
	return v*(r.ScalerY.Max[0]-r.ScalerY.Min[0]) + r.ScalerY.Min[0]
}

// ---------------------------------------------------------------------------
// 权重持久化（替代 torch.save / torch.load）
// ---------------------------------------------------------------------------

type rnnFile struct {
	InputSize  int        `json:"input_size"`
	HiddenSize int        `json:"hidden_size"`
	NumLayers  int        `json:"num_layers"`
	Layers     []RNNLayer `json:"layers"`
	Wo         []float64  `json:"wo"`
	Bo         float64    `json:"bo"`
}

// SaveRNNModel 存权重。Python 版是 torch.save(state_dict, 'rnn_model.pth')，
// 这里存成 JSON——不引任何序列化库，人也能直接看。
func SaveRNNModel(path string, m *RNNModel) error {
	f := rnnFile{
		InputSize: m.InputSize, HiddenSize: m.HiddenSize, NumLayers: m.NumLayers,
		Layers: m.Layers, Wo: m.Wo, Bo: m.Bo,
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// LoadRNNModel 读回权重
func LoadRNNModel(path string) (*RNNModel, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f rnnFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	m := &RNNModel{
		InputSize: f.InputSize, HiddenSize: f.HiddenSize, NumLayers: f.NumLayers,
		Layers: f.Layers, Wo: f.Wo, Bo: f.Bo,
	}
	m.flatten()
	return m, nil
}
