package browser

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

// Browser 是带反爬伪装的浏览器封装。
//
// 与上游 headless_browser.Browser 相比, 本实现:
//  1. UA / 平台 / sec-ch-ua 三者保持一致（不再固定写 macOS 而进程跑在 Linux 上）。
//  2. 启动参数补齐 stealth 必备项（disable-blink-features=AutomationControlled 等）。
//  3. 通过 CDP Emulation.setUserAgentOverride 同步 UA Client Hints 元数据。
//  4. 通过 Page.addScriptToEvaluateOnNewDocument 覆盖 WebGL/平台/语言等指纹。
//  5. 默认窗口大小落在桌面常见分辨率, 且每个进程加随机抖动。
type Browser struct {
	browser  *rod.Browser
	launcher *launcher.Launcher
	cfg      *browserConfig
}

type browserConfig struct {
	binPath  string
	headless bool

	// runtime UA 配置 (在 New 时根据 GOOS/Chrome 版本计算)
	userAgent string
	platform  string // "Linux" / "macOS" / "Windows" - 用于 Sec-CH-UA-Platform
	chromeVer string // 主版本号字符串, 例如 "131"

	windowWidth  int
	windowHeight int
}

type Option func(*browserConfig)

func WithBinPath(binPath string) Option {
	return func(c *browserConfig) {
		c.binPath = binPath
	}
}

// detectChromeMajor 在指纹和实际 chromium 版本对齐前先做版本一致性, 不可直接探测时取一个相对新的稳定版。
func detectChromeMajor() string {
	if v := os.Getenv("XHS_CHROME_MAJOR"); v != "" {
		return v
	}
	// 默认锁一个相对新的 Chrome 主版本。Chrome 大约每 4 周一次大版本, 这里挑一个不容易立刻被风控盯上的稳定版本。
	return "131"
}

// hostPlatform 推断当前进程所在平台, 用于让 UA / sec-ch-ua / navigator.platform 保持一致。
//
// 可以通过 XHS_PLATFORM=Linux|macOS|Windows 强制覆盖（例如部署在 Linux 但希望伪装成 Windows）。
func hostPlatform() string {
	if v := os.Getenv("XHS_PLATFORM"); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

func buildUserAgent(platform, chromeMajor string) string {
	switch platform {
	case "macOS":
		return fmt.Sprintf("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", chromeMajor)
	case "Windows":
		return fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", chromeMajor)
	default: // Linux
		return fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", chromeMajor)
	}
}

// pickWindowSize 在常见桌面分辨率里挑一个并加少量抖动, 避免 1024x768 这种典型 headless 默认值。
func pickWindowSize() (int, int) {
	commons := [][2]int{
		{1920, 1080},
		{1680, 1050},
		{1600, 900},
		{1536, 864},
		{1440, 900},
		{1366, 768},
	}
	pick := commons[rand.Intn(len(commons))]
	w := pick[0] - rand.Intn(20)
	h := pick[1] - rand.Intn(20)
	return w, h
}

// maskProxyCredentials masks username and password in proxy URL for safe logging.
func maskProxyCredentials(proxyURL string) string {
	u, err := url.Parse(proxyURL)
	if err != nil || u.User == nil {
		return proxyURL
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		u.User = url.UserPassword("***", "***")
	} else {
		u.User = url.User("***")
	}
	return u.String()
}

// NewBrowser 创建一个带反爬伪装的浏览器实例。
//
// headless=false 时通常用于扫码登录（需要看到二维码界面）。
func NewBrowser(headless bool, options ...Option) *Browser {
	cfg := &browserConfig{
		headless: headless,
	}
	for _, opt := range options {
		opt(cfg)
	}

	cfg.platform = hostPlatform()
	cfg.chromeVer = detectChromeMajor()
	cfg.userAgent = buildUserAgent(cfg.platform, cfg.chromeVer)
	cfg.windowWidth, cfg.windowHeight = pickWindowSize()

	logrus.Debugf("browser fingerprint: platform=%s chrome=%s ua=%q size=%dx%d",
		cfg.platform, cfg.chromeVer, cfg.userAgent, cfg.windowWidth, cfg.windowHeight)

	l := launcher.New().
		Set("no-sandbox").
		// 关闭最常见的自动化标记
		Set("disable-blink-features", "AutomationControlled").
		// 移除 chrome 启动时携带的 enable-automation 开关
		Set("disable-features", "AutomationControlled,IsolateOrigins,site-per-process").
		// 关闭 enable-automation infobar / 通知栏的 "Chrome 正受到自动化软件控制"
		Set("excludeSwitches", "enable-automation").
		Set("disable-infobars").
		// 与本地化 / IP 区域对齐
		Set("lang", "zh-CN").
		Set("accept-lang", "zh-CN,zh;q=0.9,en;q=0.8").
		// 窗口尺寸明确指定, 避免 headless 默认 1024x768
		Set("window-size", strconv.Itoa(cfg.windowWidth)+","+strconv.Itoa(cfg.windowHeight)).
		// 避免 headless 时 GPU 探测失败影响 webgl 指纹
		Set("disable-gpu-shader-disk-cache").
		// 关闭某些自动化场景下会触发 "正在被自动化控制" 横幅的开关
		Set("disable-default-apps").
		Set("disable-popup-blocking").
		// 限制 WebRTC 内网 IP 泄漏（如果通过代理则更有用）
		Set("force-webrtc-ip-handling-policy", "disable_non_proxied_udp").
		Set("user-agent", cfg.userAgent)

	if cfg.headless {
		// 用新版 headless（headless=new）, 老版 headless 的特征更明显
		l = l.HeadlessNew(true)
	} else {
		l = l.Headless(false)
	}

	if cfg.binPath != "" {
		l = l.Bin(cfg.binPath)
	}

	if proxy := os.Getenv("XHS_PROXY"); proxy != "" {
		l = l.Proxy(proxy)
		logrus.Infof("Using proxy: %s", maskProxyCredentials(proxy))
	}

	url := l.MustLaunch()

	browser := rod.New().
		ControlURL(url).
		MustConnect()

	// 加载 cookies
	cookiePath := cookies.GetCookiesFilePath()
	cookieLoader := cookies.NewLoadCookie(cookiePath)
	if data, err := cookieLoader.LoadCookies(); err == nil {
		var cks []*proto.NetworkCookie
		if err := json.Unmarshal(data, &cks); err != nil {
			logrus.Warnf("failed to unmarshal cookies: %v", err)
		} else {
			browser.MustSetCookies(cks...)
			logrus.Debugf("loaded cookies from file successfully")
		}
	} else {
		logrus.Warnf("failed to load cookies: %v", err)
	}

	return &Browser{
		browser:  browser,
		launcher: l,
		cfg:      cfg,
	}
}

// Close 关闭浏览器并清理 launcher 资源。
func (b *Browser) Close() {
	b.browser.MustClose()
	b.launcher.Cleanup()
}

// NewPage 创建一个 stealth 页面, 同时通过 CDP 注入 UA 元数据 / 指纹覆盖脚本。
func (b *Browser) NewPage() *rod.Page {
	page := stealth.MustPage(b.browser)

	// 1) Emulation.setUserAgentOverride: 让 sec-ch-ua / sec-ch-ua-platform / navigator.userAgentData 全部跟 UA 对齐
	if err := applyUserAgentOverride(page, b.cfg); err != nil {
		logrus.Warnf("setUserAgentOverride failed: %v", err)
	}

	// 2) Emulation.setTimezoneOverride: 与 IP / 语言区域对齐
	//   即使容器 TZ 已经设过, 这里再走一次 CDP 也无副作用,
	//   并且能在不重新部署的情况下通过 XHS_TIMEZONE 强制覆盖。
	if err := applyTimezoneOverride(page); err != nil {
		logrus.Warnf("setTimezoneOverride failed: %v", err)
	}

	// 3) Emulation.setDeviceMetricsOverride: 让 viewport / screen 与 launcher 的 window-size 对齐
	//    headless 模式下默认 screen.width/height 可能与窗口不一致, 风控会用这个差值识别。
	if err := applyDeviceMetrics(page, b.cfg); err != nil {
		logrus.Warnf("setDeviceMetricsOverride failed: %v", err)
	}

	// 4) addScriptToEvaluateOnNewDocument: 在每个 navigation 之前注入指纹覆盖脚本
	//    覆盖 navigator.webdriver、navigator.platform、navigator.languages、WebGL renderer/vendor、屏幕参数等
	if _, err := page.EvalOnNewDocument(buildFingerprintScript(b.cfg)); err != nil {
		logrus.Warnf("addScriptToEvaluateOnNewDocument failed: %v", err)
	}

	return page
}

// applyTimezoneOverride 通过 CDP 让 Intl.DateTimeFormat() 与 Date 对象使用真实使用区时区。
//
// 默认 Asia/Shanghai (与代理 IP 通常一致, 大陆站点最不容易触发风险), 可用 XHS_TIMEZONE 覆盖。
func applyTimezoneOverride(page *rod.Page) error {
	tz := os.Getenv("XHS_TIMEZONE")
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	return proto.EmulationSetTimezoneOverride{TimezoneID: tz}.Call(page)
}

// applyDeviceMetrics 让 window.innerWidth/Height + screen.width/Height 与 launcher 启动窗口一致。
func applyDeviceMetrics(page *rod.Page, cfg *browserConfig) error {
	w := cfg.windowWidth
	h := cfg.windowHeight
	if w <= 0 || h <= 0 {
		return nil
	}
	return proto.EmulationSetDeviceMetricsOverride{
		Width:             w,
		Height:            h,
		DeviceScaleFactor: 1,
		Mobile:            false,
		ScreenWidth:       &w,
		ScreenHeight:      &h,
	}.Call(page)
}

// NewWarmedPage 创建页面后, 先访问 https://www.xiaohongshu.com 作为浏览历史, 再返回。
//
// 这样紧接着的深链 (例如 /explore/{id}, /user/profile/{uid}) 在请求中会带上
// xiaohongshu.com 的 Referer, 而不是空 Referer 或外站, 更接近真实用户的浏览路径。
//
// 仅在需要访问 www.xiaohongshu.com 子页面时使用; 发布流程 (creator.xiaohongshu.com) 不要用。
func (b *Browser) NewWarmedPage() *rod.Page {
	page := b.NewPage()
	if err := warmUp(page); err != nil {
		logrus.Warnf("warm-up navigation failed (continue anyway): %v", err)
	}
	return page
}

// warmUp 让页面先经过一次 xiaohongshu.com 主页的真实请求, 给后续深链留下 Referer。
func warmUp(page *rod.Page) error {
	// 跳到主页, 等 DOM 加载稳定后小停留再返回, 模拟真人 "看一眼首页再点笔记" 的路径。
	if err := rod.Try(func() {
		page.Timeout(20 * time.Second).MustNavigate("https://www.xiaohongshu.com").MustWaitDOMStable()
	}); err != nil {
		return err
	}
	// 200~600ms 的随机停顿, 让 referer 有"停留过"的感觉
	time.Sleep(time.Duration(200+rand.Intn(400)) * time.Millisecond)
	return nil
}

// Browser 暴露底层 rod.Browser, 给需要 GetCookies() 等的调用方。
func (b *Browser) Rod() *rod.Browser {
	return b.browser
}

// applyUserAgentOverride 通过 CDP 同步 UA、平台、Sec-CH-UA-* 头。
func applyUserAgentOverride(page *rod.Page, cfg *browserConfig) error {
	// 取一个合理的主版本以及伪造的完整版本号
	major := cfg.chromeVer
	full := major + ".0.6778.86" // 一个合理范围内的 build 号

	platformNav := "Linux x86_64"
	platform := cfg.platform
	switch platform {
	case "macOS":
		platformNav = "MacIntel"
	case "Windows":
		platformNav = "Win32"
	}

	brands := []*proto.EmulationUserAgentBrandVersion{
		{Brand: "Chromium", Version: major},
		{Brand: "Google Chrome", Version: major},
		{Brand: "Not_A Brand", Version: "24"},
	}
	fullList := []*proto.EmulationUserAgentBrandVersion{
		{Brand: "Chromium", Version: full},
		{Brand: "Google Chrome", Version: full},
		{Brand: "Not_A Brand", Version: "24.0.0.0"},
	}

	meta := &proto.EmulationUserAgentMetadata{
		Brands:          brands,
		FullVersionList: fullList,
		FullVersion:     full,
		Platform:        platform,
		PlatformVersion: platformVersion(platform),
		Architecture:    "x86",
		Bitness:         "64",
		Mobile:          false,
		Model:           "",
	}

	// platformNav 给 navigator.platform 用
	return proto.NetworkSetUserAgentOverride{
		UserAgent:         cfg.userAgent,
		AcceptLanguage:    "zh-CN,zh;q=0.9,en;q=0.8",
		Platform:          platformNav,
		UserAgentMetadata: meta,
	}.Call(page)
}

func platformVersion(p string) string {
	switch p {
	case "macOS":
		return "10.15.7"
	case "Windows":
		return "10.0.0"
	default:
		return "6.5.0"
	}
}

// buildFingerprintScript 返回一段在 page 创建后第一时间就执行的指纹覆盖 JS。
//
// 这里覆盖的字段都是 stealth 库不一定包能盖住的、且小红书风控可能会检测的:
//  1. WebGL UNMASKED_VENDOR_WEBGL / UNMASKED_RENDERER_WEBGL
//  2. navigator.languages
//  3. navigator.hardwareConcurrency / deviceMemory
//  4. screen.colorDepth / pixelDepth (headless 下经常异常)
//  5. window.chrome.runtime 的存在性兜底
func buildFingerprintScript(cfg *browserConfig) string {
	vendor, renderer := pickWebGLString(cfg.platform)

	// 注意: 不能直接拼用户输入到 JS 里, 这里的字符串都是受控常量。
	js := `
(() => {
  try {
    // ---- WebGL 伪装 ----
    const _getParameter = WebGLRenderingContext.prototype.getParameter;
    WebGLRenderingContext.prototype.getParameter = function(parameter) {
      if (parameter === 37445) return ` + jsString(vendor) + `;   // UNMASKED_VENDOR_WEBGL
      if (parameter === 37446) return ` + jsString(renderer) + `; // UNMASKED_RENDERER_WEBGL
      return _getParameter.call(this, parameter);
    };
    if (typeof WebGL2RenderingContext !== "undefined") {
      const _getParameter2 = WebGL2RenderingContext.prototype.getParameter;
      WebGL2RenderingContext.prototype.getParameter = function(parameter) {
        if (parameter === 37445) return ` + jsString(vendor) + `;
        if (parameter === 37446) return ` + jsString(renderer) + `;
        return _getParameter2.call(this, parameter);
      };
    }

    // ---- navigator 字段补齐 ----
    Object.defineProperty(navigator, 'languages', {
      get: () => ['zh-CN', 'zh', 'en'],
      configurable: true,
    });
    Object.defineProperty(navigator, 'hardwareConcurrency', {
      get: () => 8,
      configurable: true,
    });
    if (!('deviceMemory' in navigator) || navigator.deviceMemory < 4) {
      Object.defineProperty(navigator, 'deviceMemory', {
        get: () => 8,
        configurable: true,
      });
    }

    // ---- screen 字段兜底（headless 下 colorDepth 经常是 24 / availHeight 经常异常） ----
    try {
      Object.defineProperty(screen, 'colorDepth', { get: () => 24, configurable: true });
      Object.defineProperty(screen, 'pixelDepth', { get: () => 24, configurable: true });
    } catch (e) {}

    // ---- window.chrome 兜底（stealth 已处理大部分, 这里防止 navigator.userAgent 与 chrome 对象不一致） ----
    if (!window.chrome) {
      window.chrome = { runtime: {} };
    } else if (!window.chrome.runtime) {
      window.chrome.runtime = {};
    }

    // ---- navigator.webdriver 二次兜底（stealth 已经做过, 这里防止某些场景被还原） ----
    try {
      Object.defineProperty(navigator, 'webdriver', {
        get: () => undefined,
        configurable: true,
      });
    } catch (e) {}
  } catch (e) {
    // 静默吞错以免暴露注入痕迹
  }
})();
`
	return js
}

// jsString 把 Go 字符串安全编码成 JS 字面量。
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// pickWebGLString 根据平台返回真实硬件常见的 WebGL vendor/renderer。
func pickWebGLString(platform string) (vendor, renderer string) {
	switch platform {
	case "macOS":
		return "Apple Inc.", "ANGLE (Apple, ANGLE Metal Renderer: Apple M1, Unspecified Version)"
	case "Windows":
		return "Google Inc. (NVIDIA)", "ANGLE (NVIDIA, NVIDIA GeForce RTX 3060 Direct3D11 vs_5_0 ps_5_0, D3D11)"
	default:
		// Linux: 桌面常见的真实渲染器, 而不是 SwiftShader / Mesa OffScreen
		return "Google Inc. (Intel)", "ANGLE (Intel, Mesa Intel(R) UHD Graphics 630 (CFL GT2), OpenGL 4.6)"
	}
}

// ---------- 共享浏览器实例 ----------

var (
	sharedMu      sync.Mutex
	sharedBrowser *Browser
)

// Shared 返回进程内复用的浏览器实例。
//
// 设计动机: 之前每个业务调用都开关一次浏览器 = 每次都给小红书后端造成"同账号 + 全新设备指纹"的现象,
// 是非常强的风控信号。复用同一个 browser 后, 同账号在风控视角看到的是稳定设备。
//
// 通过 XHS_REUSE_BROWSER=0 可以禁用复用（回退到旧行为, 仅用于排障）。
func Shared(headless bool, options ...Option) *Browser {
	if !reuseEnabled() {
		return NewBrowser(headless, options...)
	}

	sharedMu.Lock()
	defer sharedMu.Unlock()

	if sharedBrowser != nil {
		return sharedBrowser
	}

	sharedBrowser = NewBrowser(headless, options...)
	return sharedBrowser
}

// ReleaseShared 关闭并清理共享 browser, 通常只在进程退出时调用。
func ReleaseShared() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedBrowser != nil {
		sharedBrowser.Close()
		sharedBrowser = nil
	}
}

// CloseIfNotShared 如果 b 不是当前共享 browser, 则关闭它（旧调用方式 defer b.Close() 的兼容层）。
//
// 这样 service 层即使把 NewBrowser 替换成 Shared(), 旧的 defer b.Close() 也不会真正关掉共享实例。
func CloseIfNotShared(b *Browser) {
	if b == nil {
		return
	}
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedBrowser != nil && sharedBrowser == b {
		return
	}
	b.Close()
}

func reuseEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("XHS_REUSE_BROWSER")))
	if v == "0" || v == "false" || v == "no" {
		return false
	}
	return true
}
