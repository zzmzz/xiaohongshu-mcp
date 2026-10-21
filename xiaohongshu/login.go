package xiaohongshu

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/go-rod/rod"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type LoginAction struct {
	page *rod.Page
}

func NewLogin(page *rod.Page) *LoginAction {
	return &LoginAction{page: page}
}

func (a *LoginAction) CheckLoginStatus(ctx context.Context) (bool, error) {
	// 加超时保护：只是查登录态的快速检查，不应无限挂（登录扫码的等待在 Login/WaitForLogin 里）
	pp := a.page.Context(ctx).Timeout(30 * time.Second)
	applySiteLocale(pp)
	pp.MustNavigate(Site().Home).MustWaitLoad()

	time.Sleep(1 * time.Second)

	exists, _, err := pp.Has(Site().LoggedInSel)
	if err != nil {
		return false, errors.Wrap(err, "check login status failed")
	}

	return exists, nil
}

// Login 打开站点首页,等待用户在窗口中完成登录(扫码/手机验证码均可)。
// 轮询已登录标志,最长等待 loginWaitTimeout;比旧版的单次 MustElement 更鲁棒。
const loginWaitTimeout = 10 * time.Minute

// CurrentUser 当前登录用户的基础信息。
type CurrentUser struct {
	Nickname string `json:"nickname"`
	UserID   string `json:"userId"`
}

// CurrentUser 从当前页面的 __INITIAL_STATE__ 读取登录用户信息。
// 需在 CheckLoginStatus 之后调用：复用已加载的 explore 页，不做额外导航。
func (a *LoginAction) CurrentUser(ctx context.Context) (*CurrentUser, error) {
	pp := a.page.Context(ctx).Timeout(10 * time.Second)

	res, err := pp.Eval(`() => {
		const u = window.__INITIAL_STATE__ && window.__INITIAL_STATE__.user;
		const info = u && u.userInfo && u.userInfo.value !== undefined ? u.userInfo.value : (u && u.userInfo);
		if (!info || info.guest) return "";
		return JSON.stringify({nickname: info.nickname, userId: info.userId || info.user_id});
	}`)
	if err != nil {
		return nil, errors.Wrap(err, "read current user state failed")
	}

	raw := res.Value.String()
	if raw == "" {
		return nil, errors.New("current user not found in page state")
	}

	var user CurrentUser
	if err := json.Unmarshal([]byte(raw), &user); err != nil {
		return nil, errors.Wrap(err, "unmarshal current user failed")
	}

	return &user, nil
}

func (a *LoginAction) Login(ctx context.Context) error {
	pp := a.page.Context(ctx)
	applySiteLocale(pp)
	pp.MustNavigate(Site().Home).MustWaitLoad()

	time.Sleep(2 * time.Second)

	if exists, _, _ := pp.Has(Site().LoggedInSel); exists {
		return nil
	}

	logrus.Infof("请在浏览器窗口中完成 %s 登录(扫码或手机验证码),最长等待 %v...", Site().Name, loginWaitTimeout)

	deadline := time.NewTimer(loginWaitTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.Errorf("等待登录超时(%v)", loginWaitTimeout)
		case <-ticker.C:
			if exists, _, _ := pp.Has(Site().LoggedInSel); exists {
				logrus.Info("检测到登录成功")
				// 稍等让会话 cookie 全部落定
				time.Sleep(2 * time.Second)
				return nil
			}
		}
	}
}

func (a *LoginAction) FetchQrcodeImage(ctx context.Context) (string, bool, error) {
	pp := a.page.Context(ctx)
	applySiteLocale(pp)

	pp.MustNavigate(Site().Home).MustWaitLoad()

	time.Sleep(2 * time.Second)

	if exists, _, _ := pp.Has(Site().LoggedInSel); exists {
		return "", true, nil
	}

	// 首页已不再自动弹登录框,找不到二维码就去独立的 /login 页
	const qrcodeSel = ".login-container .qrcode-img"
	el, err := pp.Timeout(5 * time.Second).Element(qrcodeSel)
	if err != nil {
		if err := pp.Navigate(Site().Base + "/login"); err != nil {
			return "", false, errors.Wrap(err, "open login page failed")
		}
		el, err = pp.Timeout(30 * time.Second).Element(qrcodeSel)
		if err != nil {
			dumpLoginPage(pp)
			return "", false, errors.Wrap(err, "qrcode not found on login page")
		}
	}

	src, err := el.Attribute("src")
	if err != nil {
		return "", false, errors.Wrap(err, "get qrcode src failed")
	}
	if src == nil || len(*src) == 0 {
		return "", false, errors.New("qrcode src is empty")
	}

	return *src, false, nil
}

func (a *LoginAction) WaitForLogin(ctx context.Context) bool {
	pp := a.page.Context(ctx)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			// 二维码现在在独立的 /login 页上,页面没有侧栏,只能靠登录后才下发的 id_token 判断
			if hasLoginCookie(pp) {
				// 稍等让会话 cookie 全部落定
				time.Sleep(2 * time.Second)
				return true
			}
			if has, _, err := pp.Has(Site().LoggedInSel); err == nil && has {
				return true
			}
		}
	}
}

// hasLoginCookie 游客也有 web_session,id_token 只在扫码登录后才下发
func hasLoginCookie(page *rod.Page) bool {
	cks, err := page.Browser().GetCookies()
	if err != nil {
		return false
	}
	for _, c := range cks {
		if c.Name == "id_token" && c.Value != "" {
			return true
		}
	}
	return false
}

// dumpLoginPage 取不到二维码时留一张截图和当前 URL,方便判断是改版还是风控页
func dumpLoginPage(page *rod.Page) {
	pp := page.Timeout(10 * time.Second)
	if info, err := pp.Info(); err == nil {
		logrus.Warnf("登录页未找到二维码,当前 URL: %s, 标题: %s", info.URL, info.Title)
	}
	img, err := pp.Screenshot(false, nil)
	if err != nil {
		logrus.Warnf("登录页截图失败: %v", err)
		return
	}
	path := "/app/images/login-debug.png"
	if err := os.WriteFile(path, img, 0o644); err != nil {
		logrus.Warnf("登录页截图保存失败: %v", err)
		return
	}
	logrus.Warnf("登录页截图已保存: %s", path)
}
