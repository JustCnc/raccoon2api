package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// The gateway's authorize page can hand the one-time code back to a loopback
// URL instead of the office-raccoon:// deep link, but only on a strict
// contract (reverse engineered from the page bundle):
//
//	login_source=desktop
//	desktop_callback=loopback
//	redirect=http://127.0.0.1:<1024-99999>/auth/callback   (exact path)
//	state=<64 lowercase hex chars>
//
// When that holds, the browser is sent to redirect?code=<code>&state=<state>
// after login, so the plugin can finish the login without asking the user to
// copy anything. The deep link stays as the fallback: it needs the desktop
// client installed, and it silently swallows the code otherwise.
const loginCallbackPath = "/auth/callback"

func (a *app) loginCallbackPort() int {
	port := a.currentConfig().LoginCallbackPort
	if port < 1024 || port > 99999 {
		return 0
	}
	return port
}

// loginCallbackEnabled reports whether the loopback receiver is up, which is
// what decides between the automatic and the copy-the-url login flow.
func (a *app) loginCallbackEnabled() bool {
	a.loginCbMu.Lock()
	defer a.loginCbMu.Unlock()
	return a.loginCb != nil
}

// startLoginCallback binds the loopback receiver. A port conflict is not fatal:
// the plugin logs it and every login falls back to the deep link.
func (a *app) startLoginCallback() {
	port := a.loginCallbackPort()
	if port == 0 {
		return
	}
	a.loginCbMu.Lock()
	defer a.loginCbMu.Unlock()
	if a.loginCb != nil {
		return
	}
	// Listening on every interface is what makes this work from the Docker
	// host's browser; the handler itself only answers a state it issued.
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		a.log.warn("login", fmt.Sprintf("回调端口 %d 无法监听（%v），登录将退回「复制回调地址」方式", port, err))
		return
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(a.serveLoginCallback),
		ReadHeaderTimeout: 10 * time.Second,
	}
	a.loginCb = server
	go func() {
		if errServe := server.Serve(listener); errServe != nil && errServe != http.ErrServerClosed {
			a.log.warn("login", "回调服务退出："+errServe.Error())
		}
	}()
	a.log.info("login", fmt.Sprintf("授权回调已监听 :%d%s", port, loginCallbackPath))
}

func (a *app) stopLoginCallback() {
	a.loginCbMu.Lock()
	server := a.loginCb
	a.loginCb = nil
	a.loginCbMu.Unlock()
	if server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

// serveLoginCallback exchanges the code the browser brings back.
func (a *app) serveLoginCallback(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSuffix(r.URL.Path, "/") != loginCallbackPath {
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query()
	state := strings.TrimSpace(query.Get("state"))
	code := strings.TrimSpace(query.Get("code"))
	if state == "" || a.loginSession(state) == nil {
		writeCallbackPage(w, http.StatusBadRequest, false, "登录会话已失效", "这个回调链接对应的授权已过期或被使用过。请回到控制台重新发起登录。")
		return
	}
	if code == "" {
		writeCallbackPage(w, http.StatusBadRequest, false, "没有拿到授权码", "网关没有在回调里带回 code，请回到控制台重新发起登录。")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	account, err := a.completeLogin(ctx, state, code, "")
	if err != nil {
		writeCallbackPage(w, http.StatusBadRequest, false, "授权失败", err.Error())
		return
	}
	alias := account.Alias
	if alias == "" {
		alias = account.ID
	}
	a.log.info("login", "回调已完成授权："+alias)
	writeCallbackPage(w, http.StatusOK, true, "登录成功", "账号「"+alias+"」已加入账号池，可以关闭这个页面了。")
}

// writeCallbackPage renders the page the user lands on. It is a self-contained
// document because the browser has no access to the console's assets.
func writeCallbackPage(w http.ResponseWriter, status int, ok bool, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	accent := "#d64545"
	icon := "M12 8v5m0 3.5h.01" // exclamation
	if ok {
		accent = "#1a7f4b"
		icon = "M4 12.5l5 5L20 6.5"
	}
	fmt.Fprintf(w, `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%[1]s</title>
<style>
:root{color-scheme:light}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
  background:#f5f6f8;color:#1f2329;
  font:400 14px/1.6 system-ui,-apple-system,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
.card{width:min(420px,calc(100vw - 32px));padding:32px;background:#fff;border:1px solid #e5e6eb;
  border-radius:12px;box-shadow:0 8px 24px rgba(15,23,42,.06);text-align:center}
.badge{width:48px;height:48px;margin:0 auto 16px;border-radius:50%%;display:flex;
  align-items:center;justify-content:center;background:%[2]s1a}
.badge svg{width:24px;height:24px;stroke:%[2]s;fill:none;stroke-width:2.2;
  stroke-linecap:round;stroke-linejoin:round}
h1{margin:0 0 8px;font-size:17px;font-weight:600}
p{margin:0;color:#5c6370;font-size:13px;word-break:break-word}
</style></head><body>
<div class="card">
  <div class="badge"><svg viewBox="0 0 24 24"><path d="%[3]s"/></svg></div>
  <h1>%[1]s</h1>
  <p>%[4]s</p>
</div></body></html>`, htmlEscape(title), accent, icon, htmlEscape(detail))
}

// htmlEscape keeps the page safe when a message carries upstream text.
func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return replacer.Replace(value)
}
