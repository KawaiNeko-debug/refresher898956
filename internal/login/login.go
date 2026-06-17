package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

var (
	accountSelectors = []string{
		`input[placeholder*="手机号"]`,
		`input[placeholder*="账号"]`,
		`input[placeholder*="客户编号"]`,
		`input[name="username"]`,
		`input[type="text"]`,
	}
	passwordSelectors = []string{
		`input[placeholder*="密码"]`,
		`input[name="password"]`,
		`input[type="password"]`,
	}
	agreementSelectors = []string{
		`label .el-checkbox__inner`,
		`input[type="checkbox"]`,
	}
	loginButtonSelectors = []string{
		`#submitLogin`,
		`button[type="submit"]`,
		`button.el-button--primary`,
	}
)

type Config struct {
	Proxy        string
	ChromePath   string
	Timeout      time.Duration
	QueueTimeout time.Duration
	Headless     bool
	Workers      int
	Target       TargetConfig
	Logger       LogFunc
}

type LogEvent struct {
	Level        string
	Type         string
	Message      string
	CustomerCode string
	Fields       map[string]any
}

type LogFunc func(LogEvent)

type Runner struct {
	proxy        string
	chromePath   string
	timeout      time.Duration
	queueTimeout time.Duration
	headless     bool
	sem          chan struct{}
	target       TargetConfig
	logger       LogFunc
}

type Account struct {
	CustomerCode      string `json:"customerCode"`
	Password          string `json:"-"`
	TGC               string `json:"tgc"`
	PrimaryCredential string `json:"primaryCredential"`
	SessionCredential string `json:"sessionCredential"`
	MobileAccessToken string `json:"mobileAccessToken"`
	CanUseVoucher     int    `json:"canUseVoucher"`
}

type loginCookieCapture struct {
	mu         sync.Mutex
	target     TargetConfig
	requestURL map[network.RequestID]string
	values     map[string]string
}

func NewRunner(config Config) *Runner {
	workers := config.Workers
	if workers <= 0 {
		workers = 1
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	queueTimeout := config.QueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = 30 * time.Minute
	}
	return &Runner{
		proxy:        strings.TrimSpace(config.Proxy),
		chromePath:   strings.TrimSpace(config.ChromePath),
		timeout:      timeout,
		queueTimeout: queueTimeout,
		headless:     config.Headless,
		sem:          make(chan struct{}, workers),
		target:       config.Target,
		logger:       config.Logger,
	}
}

func (r *Runner) Login(parent context.Context, account, password string) (Account, error) {
	account = strings.ToUpper(strings.TrimSpace(account))
	password = strings.TrimSpace(password)
	if account == "" || password == "" {
		return Account{}, errors.New("账号和密码不能为空")
	}
	ctx, cancel := context.WithTimeout(parent, r.queueTimeout)
	defer cancel()

	r.log("info", account, "进入登录队列", nil)
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
		r.log("info", account, "已分配登录执行名额", nil)
	case <-ctx.Done():
		err := errors.New("登录队列等待超时或请求已取消")
		r.log("error", account, "登录队列退出", map[string]any{"error": err.Error()})
		return Account{}, err
	}

	var result Account
	var err error
	const maxAttempts = 2
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			err = fmt.Errorf("全局登录超时: %w", ctx.Err())
			break
		}
		r.log("info", account, "开始登录尝试", map[string]any{"attempt": attempt})
		result, err = r.runLogin(ctx, account, password)
		if err == nil {
			r.log("info", account, "登录成功", map[string]any{
				"attempt":       attempt,
				"canUseVoucher": result.CanUseVoucher,
			})
			return result, nil
		}
		r.log("error", account, "登录尝试失败", map[string]any{
			"attempt": attempt,
			"error":   err.Error(),
		})
		if attempt < maxAttempts {
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				err = fmt.Errorf("登录重试等待时超时: %w", ctx.Err())
				break
			}
		}
	}
	if err == nil {
		err = errors.New("登录失败")
	}
	r.log("error", account, "登录最终失败", map[string]any{"error": err.Error()})
	return Account{}, fmt.Errorf("尝试%d次后依然失败: %w", maxAttempts, err)
}

func (r *Runner) runLogin(parent context.Context, account, password string) (Account, error) {
	profilePath, cleanupProfile, err := prepareProfile("", r.target.TempProfilePattern)
	if err != nil {
		return Account{}, err
	}
	defer cleanupProfile()

	chromePath, err := resolveBrowserPath(r.chromePath)
	if err != nil {
		return Account{}, err
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", r.headless),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-infobars", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-features", "WebGPU,Vulkan,CanvasOopRasterization"),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("start-maximized", true),
		chromedp.Flag("window-size", "1365,900"),
		chromedp.Flag("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36"),
		chromedp.UserDataDir(profilePath),
		chromedp.ExecPath(chromePath),
	)
	if r.proxy != "" {
		opts = append(opts, chromedp.ProxyServer(r.proxy))
	}

	allocatorCtx, cancelAllocator := chromedp.NewExecAllocator(parent, opts...)
	defer cancelAllocator()
	browserCtx, cancelBrowser := chromedp.NewContext(allocatorCtx)
	defer cancelBrowser()

	ctx, cancelTimeout := context.WithTimeout(browserCtx, r.timeout)
	defer cancelTimeout()

	loginCookies := newLoginCookieCapture(r.target)
	chromedp.ListenTarget(ctx, loginCookies.handleEvent)

	if err := chromedp.Run(ctx,
		network.Enable(),
		chromedp.ActionFunc(func(ctx context.Context) error {
			script := `Object.defineProperty(navigator, 'webdriver', { get: () => undefined });`
			_, err := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
			return err
		}),
		chromedp.Navigate(r.target.LoginURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.ActionFunc(clickAccountLoginTab),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return fillFirstVisible(ctx, accountSelectors, account)
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return fillFirstVisible(ctx, passwordSelectors, password)
		}),
		chromedp.ActionFunc(clickAgreementIfNeeded),
		chromedp.Sleep(randomPause(350*time.Millisecond, 850*time.Millisecond)),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return clickFirstVisible(ctx, loginButtonSelectors)
		}),
	); err != nil {
		return Account{}, fmt.Errorf("填写并提交登录表单失败: %w", err)
	}

	sliderVisible, err := waitForSlider(ctx, 5*time.Second)
	if err != nil {
		return Account{}, fmt.Errorf("检测滑块失败: %w", err)
	}
	if sliderVisible {
		if err := ensureSolved(ctx, 5); err != nil {
			return Account{}, fmt.Errorf("滑块处理失败: %w", err)
		}
	}

	if err := waitForLogin(ctx, r.target); err != nil {
		return Account{}, err
	}

	var cookies []*network.Cookie
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var cookieErr error
		cookies, cookieErr = network.GetCookies().WithURLs(r.target.BrowserCookieURLs).Do(ctx)
		return cookieErr
	})); err != nil {
		return Account{}, fmt.Errorf("读取 Cookie 失败: %w", err)
	}

	responseCookies := loginCookies.snapshot()
	generated, err := buildGeneratedAccount(ctx, password, cookies, responseCookies, r.target)
	if err != nil {
		return Account{}, fmt.Errorf("生成账号配置失败: %w", err)
	}
	if generated.CustomerCode != account {
		return Account{}, fmt.Errorf("请求客编 %s 与登录后客编 %s 不一致", account, generated.CustomerCode)
	}
	return generated, nil
}

func (r *Runner) log(level, customerCode, message string, fields map[string]any) {
	if r.logger == nil {
		return
	}
	r.logger(LogEvent{
		Level:        strings.TrimSpace(level),
		Type:         "login",
		Message:      strings.TrimSpace(message),
		CustomerCode: strings.ToUpper(strings.TrimSpace(customerCode)),
		Fields:       copyFields(fields),
	})
}

const sliderHandleXPath = `//div[@id='aliyunCaptcha-sliding-slider']`

func waitForSlider(ctx context.Context, timeout time.Duration) (bool, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := chromedp.Run(waitCtx, chromedp.WaitVisible(sliderHandleXPath, chromedp.BySearch))
	if err == nil {
		return true, nil
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		return false, nil
	}
	if waitCtx.Err() != nil {
		return false, waitCtx.Err()
	}
	return false, err
}

func solveSlider(ctx context.Context) error {
	if err := chromedp.Run(ctx, chromedp.WaitVisible(sliderHandleXPath, chromedp.BySearch)); err != nil {
		return fmt.Errorf("滑块未出现: %w", err)
	}

	sliderScript := `
	(async function() {
		const slider = document.getElementById('aliyunCaptcha-sliding-slider');
		const wrapper = document.getElementById('aliyunCaptcha-sliding-wrapper');

		if (!slider || !wrapper) {
			console.error("未找到滑块元素");
			return;
		}

		function generateHumanPath(x1, y1, x2, y2) {
			const points = [];
			const cx1 = x1 + (x2 - x1) * 0.3 + (Math.random() - 0.5) * 20;
			const cy1 = y1 + (Math.random() - 0.5) * 50;
			const cx2 = x1 + (x2 - x1) * 0.7 + (Math.random() - 0.5) * 20;
			const cy2 = y1 + (Math.random() - 0.5) * 50;

			const totalDuration = 800 + Math.random() * 700;
			const steps = 60 + Math.floor(Math.random() * 40);

			for (let i = 0; i <= steps; i++) {
				const t = i / steps;
				const ease = t < 0.5 ? 2 * t * t : -1 + (4 - 2 * t) * t;

				const x = Math.pow(1 - ease, 3) * x1 +
						  3 * Math.pow(1 - ease, 2) * ease * cx1 +
						  3 * (1 - ease) * ease * ease * cx2 +
						  Math.pow(ease, 3) * x2;

				const y = Math.pow(1 - ease, 3) * y1 +
						  3 * Math.pow(1 - ease, 2) * ease * cy1 +
						  3 * (1 - ease) * ease * ease * cy2 +
						  Math.pow(ease, 3) * y2;

				const timeOffset = Math.floor(totalDuration * t);
				const noiseX = (Math.random() - 0.5) * 2;
				const noiseY = (Math.random() - 0.5) * 2;

				points.push({ x: x + noiseX, y: y + noiseY, t: timeOffset });
			}
			return points;
		}

		function triggerEvent(el, type, x, y) {
			const event = new MouseEvent(type, {
				bubbles: true, cancelable: true, view: window, detail: 1,
				screenX: x, screenY: y, clientX: x, clientY: y,
				ctrlKey: false, altKey: false, shiftKey: false, metaKey: false,
				button: 0, buttons: 1
			});
			el.dispatchEvent(event);
		}

		const sliderRect = slider.getBoundingClientRect();
		const wrapperRect = wrapper.getBoundingClientRect();

		const startX = sliderRect.left + sliderRect.width / 2;
		const startY = sliderRect.top + sliderRect.height / 2;
		const extraDistance = 15;
		const endX = wrapperRect.left + wrapperRect.width - (sliderRect.width / 2) + extraDistance;
		const endY = startY + (Math.random() - 0.5) * 5;

		const path = generateHumanPath(startX, startY, endX, endY);
		triggerEvent(slider, 'mousedown', startX, startY);

		let previousTime = 0;
		for (let i = 0; i < path.length; i++) {
			const point = path[i];
			const waitTime = point.t - previousTime;
			if (waitTime > 0) {
				await new Promise(resolve => setTimeout(resolve, waitTime));
			}
			triggerEvent(slider, 'mousemove', point.x, point.y);
			triggerEvent(document, 'mousemove', point.x, point.y);
			previousTime = point.t;
		}

		await new Promise(resolve => setTimeout(resolve, 200 + Math.random() * 100));
		const lastPoint = path[path.length - 1];
		triggerEvent(slider, 'mouseup', lastPoint.x, lastPoint.y);
		triggerEvent(document, 'mouseup', lastPoint.x, lastPoint.y);
	})();
	`

	if err := chromedp.Run(ctx, chromedp.Evaluate(sliderScript, nil)); err != nil {
		return fmt.Errorf("执行 JS 注入滑块验证失败: %w", err)
	}
	return nil
}

func ensureSolved(ctx context.Context, maxRetries int) error {
	for i := 0; i < maxRetries; i++ {
		if err := solveSlider(ctx); err != nil {
			return err
		}
		select {
		case <-time.After(4 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}

		var needRetry bool
		err := chromedp.Run(ctx, chromedp.Evaluate(`
			(function() {
				const slider = document.getElementById('aliyunCaptcha-sliding-slider');
				if (!slider) return false;

				const style = window.getComputedStyle(slider);
				const leftVal = parseFloat(style.left) || 0;
				if (leftVal < 10) {
					return true;
				}

				const text = document.body.innerText || "";
				if (text.includes('验证失败') || text.includes('出错了') || text.includes('请重试')) {
					return true;
				}

				return false;
			})()
		`, &needRetry))
		if err != nil {
			return nil
		}
		if !needRetry {
			return nil
		}
		select {
		case <-time.After(time.Duration(rand.Intn(1000)+1500) * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.New("滑块验证已达到最大重试次数，仍未能通过")
}

func prepareProfile(profile, tempPattern string) (string, func(), error) {
	if profile = strings.TrimSpace(profile); profile != "" {
		profilePath, err := filepath.Abs(profile)
		if err != nil {
			return "", nil, fmt.Errorf("解析 profile 路径失败: %w", err)
		}
		if err := os.MkdirAll(profilePath, 0o700); err != nil {
			return "", nil, fmt.Errorf("创建 profile 目录失败: %w", err)
		}
		return profilePath, func() {}, nil
	}

	if tempPattern = strings.TrimSpace(tempPattern); tempPattern == "" {
		tempPattern = "login-profile-*"
	}
	profilePath, err := os.MkdirTemp("", tempPattern)
	if err != nil {
		return "", nil, fmt.Errorf("创建临时 profile 失败: %w", err)
	}
	return profilePath, func() {
		_ = os.RemoveAll(profilePath)
	}, nil
}

func resolveBrowserPath(configured string) (string, error) {
	if configured = strings.TrimSpace(configured); configured != "" {
		if _, err := os.Stat(configured); err != nil {
			return "", fmt.Errorf("浏览器不存在 %q: %w", configured, err)
		}
		return configured, nil
	}

	var candidates []string
	if runtime.GOOS == "darwin" {
		candidates = []string{
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		}
	} else if runtime.GOOS == "windows" {
		candidates = []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		}
	} else {
		candidates = []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/usr/bin/microsoft-edge",
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("未找到 Edge 或 Chrome，请指定浏览器可执行文件")
}

func clickAccountLoginTab(ctx context.Context) error {
	const expression = `(function () {
		const elements = Array.from(document.querySelectorAll('button, [role="tab"], .el-tabs__item, div, span'));
		const tab = elements.find((element) =>
			element.offsetParent &&
			element.textContent.trim() === '账号登录'
		);
		if (!tab) return false;
		tab.click();
		return true;
	})()`

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var clicked bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &clicked)); err == nil && clicked {
			return chromedp.Run(ctx,
				chromedp.WaitVisible(`input[type="password"]`, chromedp.ByQuery),
				chromedp.Sleep(300*time.Millisecond),
			)
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.New(`未找到“账号登录”标签`)
}

func fillFirstVisible(ctx context.Context, selectors []string, value string) error {
	for _, selector := range selectors {
		var filled bool
		expression := `(function () {
			const element = Array.from(document.querySelectorAll(` + jsString(selector) + `))
				.find((candidate) => candidate.offsetParent);
			if (!element) return false;
			const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
			element.focus();
			setter.call(element, ` + jsString(value) + `);
			element.dispatchEvent(new Event('input', {bubbles: true}));
			element.dispatchEvent(new Event('change', {bubbles: true}));
			return true;
		})()`
		if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &filled)); err != nil || !filled {
			continue
		}
		return nil
	}
	return fmt.Errorf("未找到可见输入框，尝试过: %s", strings.Join(selectors, ", "))
}

func clickFirstVisible(ctx context.Context, selectors []string) error {
	for _, selector := range selectors {
		var visible bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(
			`Boolean(document.querySelector(`+jsString(selector)+`)?.offsetParent)`,
			&visible,
		)); err != nil || !visible {
			continue
		}
		return chromedp.Run(ctx, chromedp.Click(selector, chromedp.ByQuery))
	}
	return fmt.Errorf("未找到可见按钮，尝试过: %s", strings.Join(selectors, ", "))
}

func clickAgreementIfNeeded(ctx context.Context) error {
	for _, selector := range agreementSelectors {
		var state struct {
			Visible bool `json:"visible"`
			Checked bool `json:"checked"`
		}
		expression := `(function () {
			const element = document.querySelector(` + jsString(selector) + `);
			if (!element || !element.offsetParent) return {visible:false, checked:false};
			const input = element.matches('input') ? element : element.closest('label')?.querySelector('input');
			return {visible:true, checked:Boolean(input?.checked)};
		})()`
		if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &state)); err != nil || !state.Visible {
			continue
		}
		if state.Checked {
			return nil
		}
		return chromedp.Run(ctx, chromedp.Click(selector, chromedp.ByQuery))
	}
	return nil
}

func waitForLogin(ctx context.Context, target TargetConfig) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待登录完成失败: %w", ctx.Err())
		case <-ticker.C:
			var state struct {
				URL          string `json:"url"`
				HasPassword  bool   `json:"hasPassword"`
				HasLoginForm bool   `json:"hasLoginForm"`
			}
			expression := `({
				url: location.href,
				hasPassword: Boolean(document.querySelector('input[type="password"]')?.offsetParent),
				hasLoginForm: Boolean(document.querySelector('#submitLogin, button[type="submit"]')?.offsetParent)
			})`
			if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &state)); err != nil {
				continue
			}
			if !strings.Contains(state.URL, target.LoginURLMarker) &&
				(!state.HasPassword || !state.HasLoginForm) {
				return nil
			}
		}
	}
}

func newLoginCookieCapture(target TargetConfig) *loginCookieCapture {
	return &loginCookieCapture{
		target:     target,
		requestURL: make(map[network.RequestID]string),
		values:     make(map[string]string),
	}
}

func (capture *loginCookieCapture) handleEvent(event any) {
	switch event := event.(type) {
	case *network.EventRequestWillBeSent:
		if event.Request != nil {
			capture.mu.Lock()
			capture.requestURL[event.RequestID] = event.Request.URL
			capture.mu.Unlock()
		}
	case *network.EventResponseReceived:
		if event.Response == nil || !capture.isPasswordLoginURL(event.Response.URL) {
			return
		}
		capture.mu.Lock()
		capture.requestURL[event.RequestID] = event.Response.URL
		capture.captureHeadersLocked(event.Response.Headers)
		capture.mu.Unlock()
	case *network.EventResponseReceivedExtraInfo:
		capture.mu.Lock()
		if capture.isPasswordLoginURL(capture.requestURL[event.RequestID]) {
			capture.captureHeadersLocked(event.Headers)
		}
		capture.mu.Unlock()
	}
}

func (capture *loginCookieCapture) isPasswordLoginURL(rawURL string) bool {
	return strings.SplitN(rawURL, "?", 2)[0] == capture.target.PasswordLoginURL
}

func (capture *loginCookieCapture) captureHeadersLocked(headers network.Headers) {
	for name, rawValue := range headers {
		if !strings.EqualFold(name, "set-cookie") {
			continue
		}
		value := fmt.Sprint(rawValue)
		for _, line := range strings.Split(value, "\n") {
			response := &http.Response{Header: http.Header{"Set-Cookie": {strings.TrimSpace(line)}}}
			for _, cookie := range response.Cookies() {
				if (cookie.Name == capture.target.AuthCookieName || cookie.Name == capture.target.PrimaryCookieName) && cookie.Value != "" {
					capture.values[cookie.Name] = cookie.Value
				}
			}
		}
	}
}

func (capture *loginCookieCapture) snapshot() map[string]string {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	values := make(map[string]string, len(capture.values))
	for name, value := range capture.values {
		values[name] = value
	}
	return values
}

func buildGeneratedAccount(ctx context.Context, password string, cookies []*network.Cookie, responseCookies map[string]string, target TargetConfig) (Account, error) {
	values := make(map[string]string, len(cookies))
	for _, cookie := range cookies {
		if cookie.Value != "" {
			values[cookie.Name] = cookie.Value
		}
	}
	for name, value := range responseCookies {
		if value != "" {
			values[name] = value
		}
	}

	authToken := values[target.AuthCookieName]
	primaryToken := values[target.PrimaryCookieName]
	if authToken == "" || primaryToken == "" {
		return Account{}, errors.New("响应头及浏览器 Cookie 缺少必要登录凭证")
	}

	client := &http.Client{Timeout: 20 * time.Second}
	acCode, customerCode, err := requestAccountCode(ctx, client, authToken, primaryToken, target)
	if err != nil {
		return Account{}, err
	}
	sessionID, secretKey, err := requestGroupSession(ctx, client, primaryToken, target)
	if err != nil {
		return Account{}, err
	}

	wwwCookie := strings.Join([]string{
		cookiePair(target.SessionCookieName, sessionID),
		cookiePair(target.PrimaryCookieName, primaryToken),
		cookiePair("customerCode", customerCode),
		cookiePair(target.CustomerCookieName, customerCode),
	}, "; ")
	if err := bindAccountCode(ctx, client, wwwCookie, acCode, secretKey, target); err != nil {
		return Account{}, err
	}
	canUseVoucher, confirmedCode, err := requestUserInfo(ctx, client, wwwCookie, target)
	if err != nil {
		return Account{}, err
	}
	if confirmedCode == "" || confirmedCode != customerCode {
		return Account{}, fmt.Errorf("登录客编 %s 与用户信息客编 %s 不一致", customerCode, confirmedCode)
	}
	mobileAccessToken := ""
	if token, mobileCustomerCode, err := requestMobileAccessToken(ctx, client, authToken, primaryToken, target); err != nil {
		fmt.Fprintf(os.Stderr, "mobile access token refresh warning customer=%s error=%v\n", customerCode, err)
	} else if mobileCustomerCode != "" && mobileCustomerCode != customerCode {
		fmt.Fprintf(os.Stderr, "mobile access token refresh warning customer=%s mobile_customer=%s error=customer_mismatch\n", customerCode, mobileCustomerCode)
	} else {
		mobileAccessToken = token
	}

	return Account{
		CustomerCode:      customerCode,
		Password:          password,
		TGC:               authToken,
		PrimaryCredential: primaryToken,
		SessionCredential: sessionID,
		MobileAccessToken: mobileAccessToken,
		CanUseVoucher:     canUseVoucher,
	}, nil
}

func requestAccountCode(ctx context.Context, client *http.Client, authToken, primaryToken string, target TargetConfig) (string, string, error) {
	return requestAccountCodeForApp(ctx, client, authToken, primaryToken, target.WebAppID, target.WebOrigin, target.WebReferer, target)
}

func requestAccountCodeForApp(ctx context.Context, client *http.Client, authToken, primaryToken, appID, origin, referer string, target TargetConfig) (string, string, error) {
	body, err := json.Marshal(map[string]string{"appId": appID})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.CheckLoginURL, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	setBrowserHeaders(req, origin, referer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", strings.Join([]string{
		cookiePair(target.AuthCookieName, authToken),
		cookiePair(target.PrimaryCookieName, primaryToken),
	}, "; "))

	respBody, _, err := doHTTPRequest(client, req)
	if err != nil {
		return "", "", fmt.Errorf("获取登录 code 失败: %w", err)
	}
	var response struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Code         string `json:"code"`
			IsLogin      bool   `json:"isLogin"`
			CustomerCode string `json:"customerCode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &response); err != nil {
		return "", "", fmt.Errorf("解析登录 code 失败: %w；响应: %s", err, respBody)
	}
	if !response.Success || response.Code != 200 || !response.Data.IsLogin ||
		response.Data.Code == "" || response.Data.CustomerCode == "" {
		return "", "", fmt.Errorf("登录态无效 appId=%s code=%d message=%s", appID, response.Code, response.Message)
	}
	return response.Data.Code, response.Data.CustomerCode, nil
}

func requestMobileAccessToken(ctx context.Context, client *http.Client, authToken, primaryToken string, target TargetConfig) (string, string, error) {
	acCode, customerCode, err := requestAccountCodeForApp(ctx, client, authToken, primaryToken, target.MobileAppID, target.MobileOrigin, target.MobileReferer, target)
	if err != nil {
		return "", "", fmt.Errorf("获取移动端登录 code 失败: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("code", acCode); err != nil {
		return "", "", fmt.Errorf("写入移动端登录 code 失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", "", fmt.Errorf("关闭移动端登录表单失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.MobileLoginByCodeURL, &body)
	if err != nil {
		return "", "", err
	}
	setMobileLoginHeaders(req, customerCode, target)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set(target.MobileAccessHeader, "NONE")
	req.Header.Set("Cookie", buildMobileLoginCookie(authToken, primaryToken, target))

	respBody, _, err := doHTTPRequest(client, req)
	if err != nil {
		return "", "", fmt.Errorf("获取移动端 accessToken 失败: %w", err)
	}
	var response struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AccessToken  string `json:"accessToken"`
			CustomerCode string `json:"customerCode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &response); err != nil {
		return "", "", fmt.Errorf("解析移动端 login-by-code 失败: %w；响应: %s", err, respBody)
	}
	if response.Code != 200 || strings.TrimSpace(response.Data.AccessToken) == "" {
		return "", "", fmt.Errorf("移动端 login-by-code 异常 code=%d message=%s", response.Code, response.Message)
	}
	if strings.TrimSpace(response.Data.CustomerCode) != "" {
		customerCode = strings.ToUpper(strings.TrimSpace(response.Data.CustomerCode))
	}
	return strings.TrimSpace(response.Data.AccessToken), customerCode, nil
}

func requestGroupSession(ctx context.Context, client *http.Client, primaryToken string, target TargetConfig) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.SecretURL, nil)
	if err != nil {
		return "", "", err
	}
	setBrowserHeaders(req, target.WebOrigin, target.WebReferer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookiePair(target.PrimaryCookieName, primaryToken))

	respBody, cookies, err := doHTTPRequest(client, req)
	if err != nil {
		return "", "", fmt.Errorf("获取会话凭证失败: %w", err)
	}
	var response struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			KeyID string `json:"keyId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &response); err != nil {
		return "", "", fmt.Errorf("解析 secret/update 失败: %w", err)
	}
	if !response.Success || response.Code != 200 || response.Data.KeyID == "" {
		return "", "", fmt.Errorf("secret/update 异常 code=%d message=%s", response.Code, response.Message)
	}
	for _, cookie := range cookies {
		if cookie.Name == target.SessionCookieName && cookie.Value != "" {
			return cookie.Value, response.Data.KeyID, nil
		}
	}
	return "", "", errors.New("secret/update 未返回会话凭证")
}

func bindAccountCode(ctx context.Context, client *http.Client, cookie, acCode, secretKey string, target TargetConfig) error {
	form := "code=" + url.QueryEscape(acCode)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.WebLoginByCodeURL, strings.NewReader(form))
	if err != nil {
		return err
	}
	setBrowserHeaders(req, target.WebOrigin, target.WebReferer)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("secretkey", secretKey)

	respBody, _, err := doHTTPRequest(client, req)
	if err != nil {
		return fmt.Errorf("绑定登录 code 失败: %w", err)
	}
	var response struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &response); err != nil {
		return fmt.Errorf("解析 login-by-code 失败: %w", err)
	}
	if !response.Success || response.Code != 200 {
		return fmt.Errorf("login-by-code 异常 code=%d message=%s", response.Code, response.Message)
	}
	return nil
}

func requestUserInfo(ctx context.Context, client *http.Client, cookie string, target TargetConfig) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.UserInfoURL, nil)
	if err != nil {
		return 0, "", err
	}
	setBrowserHeaders(req, target.WebOrigin, target.WebReferer)
	req.Header.Set("Cookie", cookie)

	respBody, _, err := doHTTPRequest(client, req)
	if err != nil {
		return 0, "", fmt.Errorf("获取用户信息失败: %w", err)
	}
	var response struct {
		Code int `json:"code"`
		Body struct {
			CustomerCode  string `json:"customerCode"`
			CanUseVoucher int    `json:"canUseVoucher"`
		} `json:"body"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &response); err != nil {
		return 0, "", fmt.Errorf("解析用户信息失败: %w", err)
	}
	if response.Code != 0 && response.Code != 200 {
		return 0, "", fmt.Errorf("用户信息接口异常 code=%d message=%s%s", response.Code, response.Msg, response.Message)
	}
	return response.Body.CanUseVoucher, response.Body.CustomerCode, nil
}

func doHTTPRequest(client *http.Client, req *http.Request) ([]byte, []*http.Cookie, error) {
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, response.Cookies(), err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return body, response.Cookies(), fmt.Errorf("HTTP %d: %s", response.StatusCode, body)
	}
	return body, response.Cookies(), nil
}

func setBrowserHeaders(req *http.Request, origin, referer string) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", referer)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0 Safari/537.36")
}

func setMobileLoginHeaders(req *http.Request, customerCode string, target TargetConfig) {
	setBrowserHeaders(req, target.MobileOrigin, target.MobileReferer)
	req.Header.Set("User-Agent", mobileUserAgentFor(customerCode))
	req.Header.Set(target.MobileClientHeader, target.MobileClientType)
}

func buildMobileLoginCookie(authToken, primaryToken string, target TargetConfig) string {
	return strings.Join([]string{
		cookiePair(target.AuthCookieName, authToken),
		cookiePair(target.PrimaryCookieName, primaryToken),
	}, "; ")
}

func cookiePair(name, value string) string {
	return strings.TrimSpace(name) + "=" + strings.TrimSpace(value)
}

func mobileUserAgentFor(seed string) string {
	pool := []string{
		"Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Mobile Safari/537.36",
		"Mozilla/5.0 (Linux; Android 14; SM-S928B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Mobile Safari/537.36",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (Linux; Android 15; Pixel 9 Pro) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Mobile Safari/537.36",
	}
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return pool[0]
	}
	sum := 0
	for _, ch := range seed {
		sum += int(ch)
	}
	return pool[sum%len(pool)]
}

func jsString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func randomPause(minimum, maximum time.Duration) time.Duration {
	return minimum + (maximum-minimum)/2
}

func copyFields(fields map[string]any) map[string]any {
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]any, len(fields))
	for key, value := range fields {
		if strings.EqualFold(key, "password") {
			continue
		}
		out[key] = value
	}
	return out
}
