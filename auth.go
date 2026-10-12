package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	cookieName = "ifeira_session"
	sessionAge = 180 * 24 * time.Hour
)

// Auth is a single family password with a signed, stateless session cookie
// (no session table needed, which suits serverless hosting).
type Auth struct {
	passwordHash [32]byte
	enabled      bool
	key          []byte
}

func NewAuth(password, secret string) *Auth {
	a := &Auth{enabled: password != "", passwordHash: sha256.Sum256([]byte(password))}
	// Without SESSION_SECRET the key comes from the password, so changing the
	// password signs everyone out.
	seed := secret
	if seed == "" {
		seed = "ifeira-session:" + password
	}
	k := sha256.Sum256([]byte(seed))
	a.key = k[:]
	return a
}

func (a *Auth) sign(exp int64) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(m.Sum(nil))
}

// Valid reports whether the request carries a good, unexpired session cookie.
func (a *Auth) Valid(r *http.Request) bool {
	if !a.enabled {
		return false
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	expStr, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(a.sign(exp)))
}

func (a *Auth) CheckPassword(p string) bool {
	if !a.enabled {
		return false
	}
	h := sha256.Sum256([]byte(p))
	return subtle.ConstantTimeCompare(h[:], a.passwordHash[:]) == 1
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !a.auth.enabled {
		writeError(w, http.StatusServiceUnavailable, "setup")
		return
	}
	if !a.auth.CheckPassword(body.Password) {
		time.Sleep(700 * time.Millisecond) // slows down guessing
		writeError(w, http.StatusUnauthorized, "wrong_password")
		return
	}
	exp := time.Now().Add(sessionAge).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: a.auth.sign(exp), Path: "/",
		MaxAge: int(sessionAge.Seconds()), HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) requireLogin(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.auth.Valid(r) {
			writeError(w, http.StatusUnauthorized, "login")
			return
		}
		if a.db == nil {
			writeError(w, http.StatusServiceUnavailable, "setup")
			return
		}
		h(w, r)
	})
}
