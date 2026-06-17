# GitHub 登录凭证刷新器

这是一个独立的 GitHub Actions 仓库：从 Manager 拉取需要刷新的账号，在 GitHub Runner 上启动 Chrome 刷新登录凭证，再把结果回传给 Manager。

目标站点的域名、Cookie 名、Header 名和 AppID 不写入仓库。部署时把它们放进 GitHub Secrets，运行时通过环境变量注入。

## 1. 推到 GitHub

在 GitHub 创建一个私有仓库，然后执行：

```bash
git init
git add .
git commit -m "init login refresher"
git branch -M main
git remote add origin https://github.com/<owner>/<repo>.git
git push -u origin main
```

后续更新：

```bash
git add .
git commit -m "update login refresher"
git push
```

## 2. 配置 GitHub Secrets

进入仓库：

`Settings -> Secrets and variables -> Actions -> New repository secret`

添加：

- `MANAGER_EXTERNAL_URL`：Manager 对 GitHub 暴露的外部地址。
- `MANAGER_EXTERNAL_TOKEN`：Manager 外部 API token，必须和 Manager 启动参数一致。
- `REFRESH_TARGET_CONFIG`：目标站点配置 JSON，包含所有不应提交到仓库的 URL、Cookie、Header 和 AppID。

`REFRESH_TARGET_CONFIG` 示例结构如下，值请替换为真实值：

```json
{
  "loginUrl": "https://login.example.com/login",
  "passwordLoginUrl": "https://login.example.com/api/login/with-password",
  "checkLoginUrl": "https://login.example.com/api/sso/check-login",
  "secretUrl": "https://www.example.com/api/secret/update",
  "webLoginByCodeUrl": "https://www.example.com/login/login-by-code",
  "mobileLoginByCodeUrl": "https://m.example.com/api/login/login-by-code",
  "userInfoUrl": "https://www.example.com/api/user/getUserInfo",
  "browserCookieUrls": [
    "https://login.example.com/",
    "https://www.example.com/",
    "https://member.example.com/"
  ],
  "loginUrlMarker": "https://login.example.com/login",
  "authCookieName": "AUTH_COOKIE_NAME",
  "primaryCookieName": "PRIMARY_COOKIE_NAME",
  "sessionCookieName": "SESSION_COOKIE_NAME",
  "customerCookieName": "CUSTOMER_COOKIE_NAME",
  "webAppId": "WEB_APP_ID",
  "mobileAppId": "MOBILE_APP_ID",
  "webOrigin": "https://www.example.com",
  "webReferer": "https://www.example.com/",
  "mobileOrigin": "https://m.example.com",
  "mobileReferer": "https://m.example.com/",
  "mobileAccessHeader": "X-Access-Token",
  "mobileClientHeader": "X-Client-Type",
  "mobileClientType": "WEB",
  "tempProfilePattern": "login-profile-*"
}
```

也可以不用 JSON，把这些字段拆成 `TARGET_*` 环境变量；代码会优先读取拆开的环境变量覆盖 JSON 值。

## 3. 手动触发测试

在 GitHub 仓库的 `Actions -> Refresh Credentials -> Run workflow` 里填：

- `job_id`：Manager 创建的刷新任务 ID。
- `group_count`：这个刷新任务拆出来的组数量。
- `max_parallel`：同时启动多少个 GitHub Runner。
- `browsers_per_runner`：单台 Runner 同时跑几个浏览器。

正常情况下 Manager 创建刷新任务后会自动触发 GitHub Actions；手动触发主要用于排查。

## 4. 并发逻辑

Manager 负责把账号按组拆开。GitHub Actions 会按组启动 matrix runner：

- `max_parallel` 控制最多同时跑多少台 GitHub Runner。
- `browsers_per_runner` 控制每台 Runner 内部最多同时打开多少个 Chrome。

## 5. 仓库内容

- `.github/workflows/refresh-credentials.yml`：GitHub Actions 工作流。
- `cmd/login-refresher`：拉取账号、刷新、回传结果的 CLI。
- `internal/login`：浏览器登录和凭证生成逻辑。
- `internal/refreshapi`：和 Manager 外部 API 对接的最小类型。

这个仓库不包含 Manager UI、库存、任务、Worker，也不包含本地账号数据。
