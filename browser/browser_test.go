package browser

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildUserAgent(t *testing.T) {
	cases := []struct {
		platform string
		want     []string // 必须包含的子串
	}{
		{"macOS", []string{"Macintosh", "Mac OS X", "Chrome/131.", "Safari/537.36"}},
		{"Windows", []string{"Windows NT 10.0", "Win64; x64", "Chrome/131.", "Safari/537.36"}},
		{"Linux", []string{"X11", "Linux x86_64", "Chrome/131.", "Safari/537.36"}},
	}
	for _, c := range cases {
		ua := buildUserAgent(c.platform, "131")
		for _, sub := range c.want {
			assert.Contains(t, ua, sub, "platform=%s ua=%q missing %q", c.platform, ua, sub)
		}
	}
}

func TestHostPlatformOverride(t *testing.T) {
	t.Setenv("XHS_PLATFORM", "Windows")
	assert.Equal(t, "Windows", hostPlatform())

	t.Setenv("XHS_PLATFORM", "")
	// 默认值: Linux 容器跑应该返回 Linux, 但 darwin 上跑 go test 会返回 macOS
	got := hostPlatform()
	assert.Contains(t, []string{"Linux", "macOS", "Windows"}, got)
}

func TestDetectChromeMajor(t *testing.T) {
	t.Setenv("XHS_CHROME_MAJOR", "")
	def := detectChromeMajor()
	assert.NotEmpty(t, def, "默认 Chrome 主版本不能为空")

	t.Setenv("XHS_CHROME_MAJOR", "133")
	assert.Equal(t, "133", detectChromeMajor())
}

func TestPickWindowSize(t *testing.T) {
	w, h := pickWindowSize()
	// 桌面常见分辨率: 至少 1366x748 起步, 不应返回 1024x768
	assert.GreaterOrEqual(t, w, 1346)
	assert.GreaterOrEqual(t, h, 748)
	assert.LessOrEqual(t, w, 1920)
	assert.LessOrEqual(t, h, 1080)
}

func TestMaskProxyCredentials(t *testing.T) {
	// url.URL 在编码用户名/密码时会把 * 转义成 %2A, 这里只校验
	// 1) 真实用户名/密码不再出现; 2) host 端口被保留。
	cases := []struct {
		in       string
		excludes []string
		includes []string
	}{
		{
			in:       "http://user:pass@host:8080",
			excludes: []string{"user", "pass"},
			includes: []string{"host:8080"},
		},
		{
			in:       "http://user@host:8080",
			excludes: []string{"user"},
			includes: []string{"host:8080"},
		},
		{
			in:       "http://host:8080",
			includes: []string{"http://host:8080"},
		},
		{
			in:       "socks5://u:p@127.0.0.1:1080",
			excludes: []string{"u:p"},
			includes: []string{"127.0.0.1:1080"},
		},
		{
			in:       "",
			includes: []string{""},
		},
	}
	for _, c := range cases {
		got := maskProxyCredentials(c.in)
		for _, sub := range c.excludes {
			assert.NotContains(t, got, sub, "input=%q 不应泄漏 %q", c.in, sub)
		}
		for _, sub := range c.includes {
			assert.Contains(t, got, sub, "input=%q 应保留 %q", c.in, sub)
		}
	}
}

func TestJSString(t *testing.T) {
	cases := map[string]string{
		"hello":            `"hello"`,
		`with "quotes"`:    `"with \"quotes\""`,
		"line\nbreak":      `"line\nbreak"`,
		"unicode 🚀":        `"unicode 🚀"`,
	}
	for in, want := range cases {
		got := jsString(in)
		assert.Equal(t, want, got, "input=%q", in)
	}
}

func TestPickWebGLString(t *testing.T) {
	// 不应该出现明显 headless 信号: SwiftShader / Mesa OffScreen / llvmpipe
	bad := []string{"SwiftShader", "Mesa OffScreen", "llvmpipe"}
	for _, p := range []string{"macOS", "Windows", "Linux"} {
		v, r := pickWebGLString(p)
		assert.NotEmpty(t, v)
		assert.NotEmpty(t, r)
		for _, b := range bad {
			assert.NotContains(t, r, b, "platform=%s renderer 不应包含 %q (典型 headless 指纹)", p, b)
		}
	}
}

func TestReuseEnabled(t *testing.T) {
	t.Setenv("XHS_REUSE_BROWSER", "")
	assert.True(t, reuseEnabled(), "默认应启用复用")

	for _, off := range []string{"0", "false", "FALSE", "no", "  no  "} {
		t.Setenv("XHS_REUSE_BROWSER", off)
		assert.False(t, reuseEnabled(), "value=%q 应禁用", off)
	}

	t.Setenv("XHS_REUSE_BROWSER", "1")
	assert.True(t, reuseEnabled())
}

func TestPlatformVersion(t *testing.T) {
	assert.NotEmpty(t, platformVersion("macOS"))
	assert.NotEmpty(t, platformVersion("Windows"))
	assert.NotEmpty(t, platformVersion("Linux"))
	assert.NotEmpty(t, platformVersion("Other"))
}

func TestBuildFingerprintScriptShape(t *testing.T) {
	cfg := &browserConfig{platform: "Linux"}
	js := buildFingerprintScript(cfg)
	// 关键字段都要被覆盖
	wants := []string{
		"WebGLRenderingContext.prototype.getParameter",
		"navigator", "languages", "hardwareConcurrency",
		"webdriver",
		"chrome",
	}
	for _, w := range wants {
		require.Contains(t, js, w, "脚本应覆盖 %s", w)
	}
	// 注入脚本必须自包裹 IIFE, 避免污染全局
	assert.True(t, strings.HasPrefix(strings.TrimSpace(js), "(()"), "脚本应以 IIFE 开头")
}

func TestUserAgentChromeVersionMatchesEnv(t *testing.T) {
	t.Setenv("XHS_CHROME_MAJOR", "150")
	ua := buildUserAgent("Linux", detectChromeMajor())
	assert.Contains(t, ua, "Chrome/150.")

	// 清理避免影响其他测试
	require.NoError(t, os.Unsetenv("XHS_CHROME_MAJOR"))
}
