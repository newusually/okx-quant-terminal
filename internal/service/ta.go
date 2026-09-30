package service

// 本文件由原 mvc.go（1023 行大杂烩）按「分类」拆出，见 docs/架构说明.md

import (
	"github.com/markcheno/go-talib"
)

func CalculatePSY(closes []float64, n int) float64 {
	var N1 float64
	start := len(closes) - n
	if start < 1 {
		start = 1
	}
	for i := start; i < len(closes); i++ {
		if closes[i] > closes[i-1] {
			N1++
		}
	}
	PSY := (N1 / float64(n)) * 100
	return PSY
}

func CalculateVR(closes []float64, vols []float64, n int) float64 {
	var AVS, BVS, CV float64
	start := len(closes) - n
	if start < 1 {
		start = 1
	}
	for i := start; i < len(closes); i++ {
		if closes[i] > closes[i-1] {
			AVS += vols[i]
		} else if closes[i] < closes[i-1] {
			BVS += vols[i]
		} else {
			CV += vols[i]
		}
	}
	VR := (AVS + 0.5*CV) / (BVS + 0.5*CV) * 100
	return VR
}

func CalculateAR(highs, lows, opens []float64, n int) float64 {
	var sumHighOpen, sumOpenLow float64
	for i := 0; i < n; i++ {
		sumHighOpen += highs[len(highs)-i-1] - opens[len(opens)-i-1]
		sumOpenLow += opens[len(opens)-i-1] - lows[len(lows)-i-1]
	}
	if sumOpenLow == 0 {
		return 0
	}
	return (sumHighOpen / float64(n)) / (sumOpenLow / float64(n)) * 100
}

func CalculateBR(highs, lows, closes []float64, n int) float64 {
	var sumHighPrevClose, sumPrevCloseLow float64
	for i := 1; i <= n; i++ {
		sumHighPrevClose += highs[len(highs)-i-1] - closes[len(closes)-i-1]
		sumPrevCloseLow += closes[len(closes)-i-1] - lows[len(lows)-i-1]
	}
	if sumPrevCloseLow == 0 {
		return 0
	}
	return (sumHighPrevClose / float64(n)) / (sumPrevCloseLow / float64(n)) * 100
}

func CalculateBIAS(closes []float64, n int) []float64 {

	ma := talib.Sma(closes, n)
	bias := make([]float64, len(closes))

	for i := 0; i < len(closes); i++ {
		bias[i] = (closes[i] - ma[i]) / ma[i] * 100

	}
	return bias
}
