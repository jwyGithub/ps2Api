// login.go —— 面板登录：环境变量账号密码（ADMIN_USERNAME/ADMIN_PASSWORD）+ HMAC
// 签名的会话 Cookie。ADMIN_PASSWORD 未设置时登录整体关闭，面板维持旧的无密码
// 引导态（兼容既有部署）；设置后未登录访问 / 会跳转 /login。
package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// sessionCookieName 是登录会话 Cookie 的名字；值形如 "<过期Unix秒>.<HMAC-SHA256十六进制>"。
	sessionCookieName = "ps2api_admin"
	sessionTTL        = 7 * 24 * time.Hour
)

// sessionSecret 用于会话 Cookie 签名。优先读 GATEWAY_SESSION_SECRET（多副本/重启后
// 会话仍有效）；未设置则进程启动时随机生成——重启后所有会话失效、需重新登录。
var sessionSecret = func() []byte {
	if v := strings.TrimSpace(os.Getenv("GATEWAY_SESSION_SECRET")); v != "" {
		return []byte(v)
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}()

// adminCreds 返回面板登录凭据：ADMIN_USERNAME（默认 admin）与 ADMIN_PASSWORD。
func adminCreds() (string, string) {
	u := strings.TrimSpace(os.Getenv("ADMIN_USERNAME"))
	if u == "" {
		u = "admin"
	}
	return u, os.Getenv("ADMIN_PASSWORD")
}

// loginEnabled 报告面板登录是否开启（设了 ADMIN_PASSWORD 即开启）。
func loginEnabled() bool {
	_, p := adminCreds()
	return p != ""
}

func signSession(expires time.Time) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, sessionSecret)
	mac.Write([]byte(sessionCookieName + ":" + exp))
	return exp + "." + hex.EncodeToString(mac.Sum(nil))
}

// validSession 校验会话 Cookie：存在、未过期、签名吻合（常数时间比较）。
func validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	i := strings.LastIndexByte(c.Value, '.')
	if i <= 0 {
		return false
	}
	exp, err := strconv.ParseInt(c.Value[:i], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, sessionSecret)
	mac.Write([]byte(sessionCookieName + ":" + c.Value[:i]))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(c.Value[i+1:])) == 1
}

// login 校验账号密码并签发会话 Cookie。凭据错误统一返回 401「账号或密码错误」，
// 不区分是哪个错了。
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		jsonError(w, 400, "请求体需为 {\"username\":\"...\",\"password\":\"...\"}", "invalid_request_error")
		return
	}
	user, pass := adminCreds()
	uOK := subtle.ConstantTimeCompare([]byte(body.Username), []byte(user)) == 1
	pOK := subtle.ConstantTimeCompare([]byte(body.Password), []byte(pass)) == 1
	if !uOK || !pOK {
		jsonError(w, 401, "账号或密码错误", "authentication_error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    signSession(time.Now().Add(sessionTTL)),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	jsonWrite(w, 200, map[string]bool{"success": true})
}

// logout 过期会话 Cookie（值置空 + MaxAge=-1），面板前端登出按钮调用。
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	jsonWrite(w, 200, map[string]bool{"success": true})
}
