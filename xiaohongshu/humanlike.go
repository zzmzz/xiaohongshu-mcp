package xiaohongshu

import (
	"math"
	"math/rand"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// 这个文件汇总"行为伪装"用的工具函数, 与反爬 P1 (#6 / #7 / #8 / #9) 对应:
//   - humanMoveTo:  贝塞尔曲线鼠标轨迹 (替代 Mouse.MustMoveTo 直跳)
//   - wheelScroll:  通过 CDP Input.dispatchMouseEvent 触发真实 wheel 事件
//                    (替代 page.Eval("window.scrollBy(...)") 这种合成滚动)
//   - readingPause: 模拟人类阅读时长, 让连续点击之间不再是固定 3000ms

// humanMoveTo 用三阶贝塞尔曲线把鼠标从当前位置移动到目标点。
//
// 与 rod.Mouse.MoveTo 的区别:
//   - 普通 MoveTo 一步到位（瞬移）, 风控做轨迹模型时是非常强的特征。
//   - 这里把整段路径切分成 N 个步骤, 每一步派发一次 mouseMoved 事件,
//     其中 control point 加随机扰动, 形成不规则曲线 + 速度变化。
//
// 调用频率: 每次悬停目标元素之前调用一次（不要在 hot loop 里循环调用）。
func humanMoveTo(page *rod.Page, x, y float64) error {
	from := page.Mouse.Position()
	dx := x - from.X
	dy := y - from.Y
	dist := math.Hypot(dx, dy)

	// 步数与距离正相关, 短距离少几步, 长距离多一些, 但不超过 ~40
	steps := int(dist/12) + 6
	if steps > 40 {
		steps = 40
	}
	if steps < 6 {
		steps = 6
	}

	// 两个 control point: 落在路径附近, 加 ±20% 距离的横向扰动, 形成自然曲线
	jitter := func(span float64) float64 {
		return (rand.Float64()*2 - 1) * span
	}
	span := dist * 0.2
	if span < 30 {
		span = 30
	}
	cp1 := proto.Point{
		X: from.X + dx*0.33 + jitter(span),
		Y: from.Y + dy*0.33 + jitter(span),
	}
	cp2 := proto.Point{
		X: from.X + dx*0.66 + jitter(span),
		Y: from.Y + dy*0.66 + jitter(span),
	}

	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		// 缓动: easeInOutCubic, 让速度先快再慢, 避免恒速直线
		te := easeInOutCubic(t)
		px, py := cubicBezier(te, from.X, from.Y, cp1.X, cp1.Y, cp2.X, cp2.Y, x, y)

		// 落点附近(<=2px) 加微小颤抖, 模拟手部抖动
		if i < steps {
			px += jitter(1.5)
			py += jitter(1.5)
		}
		if err := page.Mouse.MoveTo(proto.Point{X: px, Y: py}); err != nil {
			return err
		}
		// 步与步之间停顿 6~14ms, 累积 100~500ms 的整段移动时长
		time.Sleep(time.Duration(6+rand.Intn(8)) * time.Millisecond)
	}
	return nil
}

func cubicBezier(t, x0, y0, x1, y1, x2, y2, x3, y3 float64) (float64, float64) {
	mt := 1 - t
	mt2 := mt * mt
	mt3 := mt2 * mt
	t2 := t * t
	t3 := t2 * t

	x := mt3*x0 + 3*mt2*t*x1 + 3*mt*t2*x2 + t3*x3
	y := mt3*y0 + 3*mt2*t*y1 + 3*mt*t2*y2 + t3*y3
	return x, y
}

func easeInOutCubic(t float64) float64 {
	if t < 0.5 {
		return 4 * t * t * t
	}
	t = 2*t - 2
	return 0.5*t*t*t + 1
}

// wheelScroll 派发真实的 wheel 事件实现滚动。
//
// 与 page.Eval("window.scrollBy(0, dy)") 的区别:
//   - JS scrollBy 只产生 scroll 事件, 没有 wheel / pointermove, 风控前端埋点能识别。
//   - 这里通过 rod.Mouse.Scroll → Input.dispatchMouseEvent(MouseWheel),
//     浏览器内部把它当作真实滚轮输入处理, 会触发 wheel + scroll, 与人类一致。
//
// 把整段距离拆成多个 step, 每个 step 之间留 4~12ms, 形成"滚一下停一下"的节奏。
func wheelScroll(page *rod.Page, deltaY float64) error {
	abs := math.Abs(deltaY)
	if abs < 1 {
		return nil
	}
	// 80~140px 一个 step, 与真实滚轮单次刻度量级接近
	stepPx := 80.0 + rand.Float64()*60
	steps := int(math.Ceil(abs / stepPx))
	if steps < 1 {
		steps = 1
	}
	if steps > 30 {
		steps = 30
	}
	if err := page.Mouse.Scroll(0, deltaY, steps); err != nil {
		return err
	}
	return nil
}

// readingPause 用对数正态分布给出一个"阅读时长", 范围在 [minMs, maxMs] 内。
//
// 比 sleepRandom 更符合真实人类行为分布: 大多数集中在中段, 偶尔会有显著长尾。
func readingPause(minMs, maxMs int) {
	if maxMs <= minMs {
		time.Sleep(time.Duration(minMs) * time.Millisecond)
		return
	}
	// 中位数大约取 (min+max)/2, 偏度约 1.5
	mid := float64(minMs+maxMs) / 2
	sigma := 0.35
	mu := math.Log(mid)
	v := math.Exp(mu + sigma*rand.NormFloat64())
	if v < float64(minMs) {
		v = float64(minMs)
	}
	if v > float64(maxMs) {
		v = float64(maxMs)
	}
	time.Sleep(time.Duration(v) * time.Millisecond)
}
