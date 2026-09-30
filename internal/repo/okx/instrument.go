package okx

// instrument.go —— 合约元数据（供 mvc / 下单精度计算使用）

// Instrument 单个合约的关键要素。只放交易真正要用的字段，
// 完整字段请直接用 GetInstruments 返回的 map。
type Instrument struct {
	InstID string  `json:"instId"`
	CtVal  float64 `json:"ctVal"`  // 一张合约的面值（币）
	CtMult float64 `json:"ctMult"` // 合约乘数
	LotSz  float64 `json:"lotSz"`  // 下单张数步长
	MinSz  float64 `json:"minSz"`  // 最小下单张数
	TickSz float64 `json:"tickSz"` // 价格最小变动
}

// GetInstrument 查单个合约的元数据（免密钥公共接口）。
func (p *PublicAPI) GetInstrument(instID string) (Instrument, error) {
	resp, err := p.GetInstruments(InstTypeSwap, "", instID)
	if err != nil {
		return Instrument{}, err
	}
	rows := Data(resp)
	if len(rows) == 0 {
		return Instrument{}, ErrNoInstrument(instID)
	}
	r := rows[0]
	return Instrument{
		InstID: Str(r, "instId"),
		CtVal:  F(r, "ctVal"),
		CtMult: F(r, "ctMult"),
		LotSz:  F(r, "lotSz"),
		MinSz:  F(r, "minSz"),
		TickSz: F(r, "tickSz"),
	}, nil
}

// ErrNoInstrument 合约不存在
type ErrNoInstrument string

func (e ErrNoInstrument) Error() string { return "查不到合约 " + string(e) }
