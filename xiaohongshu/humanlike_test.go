package xiaohongshu

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEaseInOutCubic(t *testing.T) {
	// 边界点
	assert.InDelta(t, 0.0, easeInOutCubic(0), 1e-9)
	assert.InDelta(t, 1.0, easeInOutCubic(1), 1e-9)
	// 中点应当严格等于 0.5
	assert.InDelta(t, 0.5, easeInOutCubic(0.5), 1e-9)
	// 单调递增
	prev := -math.MaxFloat64
	for i := 0; i <= 20; i++ {
		v := easeInOutCubic(float64(i) / 20)
		assert.GreaterOrEqual(t, v, prev, "easeInOutCubic 应当单调递增, t=%d/20", i)
		prev = v
	}
}

func TestCubicBezierEndpoints(t *testing.T) {
	x, y := cubicBezier(0, 1, 2, 10, 20, 30, 40, 50, 60)
	assert.InDelta(t, 1.0, x, 1e-9)
	assert.InDelta(t, 2.0, y, 1e-9)

	x, y = cubicBezier(1, 1, 2, 10, 20, 30, 40, 50, 60)
	assert.InDelta(t, 50.0, x, 1e-9)
	assert.InDelta(t, 60.0, y, 1e-9)
}

func TestCubicBezierMidpointWithSymmetricControls(t *testing.T) {
	// 起点 (0,0), 终点 (100, 0), 控制点对称在 (33, 50) (66, -50) ⇒ 中点 y 应该接近 0
	x, y := cubicBezier(0.5, 0, 0, 33, 50, 66, -50, 100, 0)
	// x 应在 0~100 之间, y 在控制点附近不可能正好为 0, 但应远小于 |50|
	assert.Greater(t, x, 0.0)
	assert.Less(t, x, 100.0)
	assert.Less(t, math.Abs(y), 30.0)
}

func TestReadingPauseRange(t *testing.T) {
	// 对数正态分布会偶尔贴近边界, 但 100 次抽样必定落在 [min, max] 闭区间
	min, max := 100, 800
	for i := 0; i < 50; i++ {
		start := time.Now()
		readingPause(min, max)
		dur := time.Since(start)

		assert.GreaterOrEqual(t, dur, time.Duration(min-5)*time.Millisecond,
			"reading pause %v 短于下限 %dms", dur, min)
		// 给 250ms 的调度抖动, 防止 CI 慢机器误报
		assert.LessOrEqual(t, dur, time.Duration(max+250)*time.Millisecond,
			"reading pause %v 超过上限 %dms+宽限", dur, max)
	}
}

func TestReadingPauseDegenerate(t *testing.T) {
	// 如果 max <= min, 退化为固定睡眠
	start := time.Now()
	readingPause(50, 50)
	dur := time.Since(start)
	assert.GreaterOrEqual(t, dur, 45*time.Millisecond)
	assert.LessOrEqual(t, dur, 250*time.Millisecond)
}
