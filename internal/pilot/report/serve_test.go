package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestServerは、firstInputから作った作業報告書の版1からlatestまでをdirに書き、
// 最新の版をlatestとして配信するServerを返す。
func newTestServer(t *testing.T, latest int) (*Server, string) {
	t.Helper()

	dir := t.TempDir()
	for v := 1; v <= latest; v++ {
		in := firstInput()
		in.Version = v
		r, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := Write(dir, r, nil); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{Dir: dir, Latest: func() (int, error) { return latest, nil }, Now: func() time.Time { return t0 }}, dir
}

// postは、同一オリジンからのフォームの送信を模した要求をhへ送る。
func post(h http.Handler, form url.Values, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://dev.example.com"+BasePath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range header {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func validForm() url.Values {
	return url.Values{
		"version":                 {"1"},
		"status:task/1-1/1":       {"ok"},
		"status:task/1-1/2":       {"ng"},
		"comment:task/1-1/2":      {"戻らない\r\n2行目"},
		"status:protected/schema": {""},
		"overall":                 {"全体のコメント"},
	}
}

func assertSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	h := rec.Header()
	for key, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"Cache-Control":          "no-store",
	} {
		if got := h.Get(key); got != want {
			t.Errorf("%s = %q、期待値 = %q", key, got, want)
		}
	}
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "style-src 'sha256-", "form-action 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy = %q、含まれることを期待した文字列 = %q", csp, want)
		}
	}
}

func TestServer_Show(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, 1)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, BasePath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("ステータス = %d、期待値 = %d", rec.Code, http.StatusOK)
	}
	assertSecurityHeaders(t, rec)
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<input type="hidden" name="version" value="1">`) {
		t.Error("最新の版のフォームが配信されない")
	}

	// CSPのstyle-srcのハッシュが、配信したHTMLのstyle要素と一致しないとスタイルが効かない。
	start := strings.Index(body, "<style>") + len("<style>")
	end := strings.Index(body, "</style>")
	sum := sha256.Sum256([]byte(body[start:end]))
	want := "style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, want) {
		t.Errorf("Content-Security-Policy = %q、style要素のハッシュ = %q", csp, want)
	}
}

func TestServer_ShowNoReport(t *testing.T) {
	t.Parallel()

	s := &Server{Dir: t.TempDir(), Latest: func() (int, error) { return 0, nil }}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, BasePath, nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("ステータス = %d、期待値 = %d", rec.Code, http.StatusNotFound)
	}
	assertSecurityHeaders(t, rec)
	if body := rec.Body.String(); !strings.Contains(body, `<html lang="ja">`) || !strings.Contains(body, "作業報告書はまだありません") {
		t.Errorf("本文 = %q", body)
	}
}

func TestServer_ShowFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setupは読み書きに失敗するServerを返し、応答に出してはならないエラーの詳細を返す。
		setup func(t *testing.T) (*Server, string)
	}{
		{
			name: "状態を読めなければ500を返す",
			setup: func(t *testing.T) (*Server, string) {
				t.Helper()
				s, _ := newTestServer(t, 1)
				s.Latest = func() (int, error) { return 0, errors.New("/state/state.json: permission denied") }
				return s, "/state/state.json"
			},
		},
		{
			name: "保存済みの動作確認の結果が壊れていれば500を返す",
			setup: func(t *testing.T) (*Server, string) {
				t.Helper()
				s, dir := newTestServer(t, 1)
				path := VerificationPath(dir, 1)
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
				return s, path
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, detail := tt.setup(t)
			var log bytes.Buffer
			s.Log = &log
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, BasePath, nil))

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("ステータス = %d、期待値 = %d", rec.Code, http.StatusInternalServerError)
			}
			assertSecurityHeaders(t, rec)
			assertErrorPage(t, rec)
			if body := rec.Body.String(); !strings.Contains(body, "作業報告書を読み書きできません") || strings.Contains(body, detail) {
				t.Errorf("本文 = %q", body)
			}
			if !strings.Contains(log.String(), detail) {
				t.Errorf("端末へのログにエラーの詳細 %q がない: %q", detail, log.String())
			}
		})
	}
}

func TestServer_Save(t *testing.T) {
	t.Parallel()

	s, dir := newTestServer(t, 1)
	h := s.Handler()
	rec := post(h, validForm(), nil)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("ステータス = %d、期待値 = %d (本文: %s)", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != BasePath {
		t.Errorf("Location = %q、期待値 = %q", got, BasePath)
	}
	assertSecurityHeaders(t, rec)

	v, err := LoadVerification(dir, 1)
	if err != nil {
		t.Fatalf("動作確認の結果を読めない: %v", err)
	}
	if !v.SavedAt.Equal(t0) || v.Version != 1 || v.Comment != "全体のコメント" {
		t.Errorf("Verification = %+v", v)
	}
	want := map[string]ItemResult{
		"task/1-1/1":       {Status: StatusOK},
		"task/1-1/2":       {Status: StatusNG, Comment: "戻らない\n2行目"},
		"protected/schema": {},
		// フォームに無い項目は未確認として残す。
		"protected/dependency": {},
	}
	if len(v.Items) != len(want) {
		t.Errorf("Items = %+v、期待値 = %+v", v.Items, want)
	}
	for id, w := range want {
		if got := v.Items[id]; got != w {
			t.Errorf("Items[%q] = %+v、期待値 = %+v", id, got, w)
		}
	}

	// 保存したHTMLと、次に配信するページの両方に結果が埋め込まれる。
	saved, err := os.ReadFile(HTMLPath(dir, 1))
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, BasePath, nil))
	for name, html := range map[string]string{"report-1.html": string(saved), "配信したページ": rec.Body.String()} {
		for _, want := range []string{
			`<input type="radio" name="status:task/1-1/2" value="ng" checked>`,
			`name="comment:task/1-1/2" rows="2">戻らない` + "\n2行目</textarea>",
			`<textarea id="overall" name="overall" rows="4">全体のコメント</textarea>`,
		} {
			if !strings.Contains(html, want) {
				t.Errorf("%s に %q が含まれない", name, want)
			}
		}
	}
}

func TestServer_SaveRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		latest   int
		form     func(url.Values)
		header   map[string]string
		wantCode int
		wantBody []string
	}{
		{
			name:     "古い版の画面からの送信は409で拒否し、書いたコメントを載せる",
			latest:   2,
			wantCode: http.StatusConflict,
			wantBody: []string{"最新の第2版を開き直して", "第1版 #2 保存後に戻る", "戻らない", "全体のコメント", `<a href="/_koryluspilot/">`},
		},
		{
			name:     "版の無い送信は400で拒否する",
			latest:   1,
			form:     func(f url.Values) { f.Del("version") },
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "確認結果に許さない値があれば400で拒否する",
			latest:   1,
			form:     func(f url.Values) { f.Set("status:task/1-1/1", "maybe") },
			wantCode: http.StatusBadRequest,
			wantBody: []string{`#1 の確認結果に &#34;maybe&#34; は指定できません`},
		},
		{
			name:     "別サイトからの送信は403で拒否する",
			latest:   1,
			header:   map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantCode: http.StatusForbidden,
			wantBody: []string{"別のサイトからの送信は受け付けられません"},
		},
		{
			name:     "Sec-Fetch-Siteが無くOriginが別のホストなら403で拒否する",
			latest:   1,
			header:   map[string]string{"Sec-Fetch-Site": "", "Origin": "https://evil.example.org"},
			wantCode: http.StatusForbidden,
			wantBody: []string{"別のサイトからの送信は受け付けられません"},
		},
		{
			name:     "大きすぎる送信は413で拒否する",
			latest:   1,
			form:     func(f url.Values) { f.Set("overall", strings.Repeat("あ", maxFormBytes)) },
			wantCode: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, dir := newTestServer(t, tt.latest)
			form := validForm()
			if tt.form != nil {
				tt.form(form)
			}
			rec := post(s.Handler(), form, tt.header)

			if rec.Code != tt.wantCode {
				t.Errorf("ステータス = %d、期待値 = %d (本文: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			assertSecurityHeaders(t, rec)
			assertErrorPage(t, rec)
			for _, want := range tt.wantBody {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("本文に %q が含まれない: %s", want, rec.Body.String())
				}
			}
			for v := 1; v <= tt.latest; v++ {
				if _, err := LoadVerification(dir, v); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("拒否した送信で版%dの動作確認の結果が書かれた (err = %v)", v, err)
				}
			}
		})
	}
}

func TestServer_Routes(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, 1)
	tests := []struct {
		method, path string
		wantCode     int
	}{
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, BasePath + "report-1.json", http.StatusNotFound},
		{http.MethodPut, BasePath, http.StatusMethodNotAllowed},
		{http.MethodOptions, BasePath, http.StatusMethodNotAllowed},
		{http.MethodPost, BasePath + "missing", http.StatusNotFound},
		{http.MethodHead, BasePath, http.StatusOK},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
		if rec.Code != tt.wantCode {
			t.Errorf("%s %s のステータス = %d、期待値 = %d", tt.method, tt.path, rec.Code, tt.wantCode)
		}
		assertSecurityHeaders(t, rec)
		if tt.wantCode >= 400 {
			assertErrorPage(t, rec)
		}
		if tt.wantCode == http.StatusMethodNotAllowed {
			if got := rec.Header().Get("Allow"); got != "GET, HEAD, POST" {
				t.Errorf("Allow = %q、期待値 = %q", got, "GET, HEAD, POST")
			}
		}
		if tt.wantCode == http.StatusNotFound && !strings.Contains(rec.Body.String(), "ページが見つかりません") {
			t.Errorf("日本語の404メッセージがない: %s", rec.Body.String())
		}
		if tt.wantCode == http.StatusMethodNotAllowed && !strings.Contains(rec.Body.String(), "この送信方法は使えません") {
			t.Errorf("日本語の405メッセージがない: %s", rec.Body.String())
		}
	}
}

// assertErrorPageは、拒否された要求にも日本語のHTMLと復帰導線があることを確かめる。
func assertErrorPage(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	for _, want := range []string{
		`<html lang="ja">`,
		`<a href="/_koryluspilot/">最新の作業報告書を開く</a>`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("本文に %q が含まれない: %s", want, rec.Body.String())
		}
	}
}

func TestServer_SaveMalformedForm(t *testing.T) {
	t.Parallel()

	s, dir := newTestServer(t, 1)
	var log bytes.Buffer
	s.Log = &log
	req := httptest.NewRequest(http.MethodPost, BasePath, strings.NewReader("version=1&overall=%ZZ"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ステータス = %d、期待値 = %d", rec.Code, http.StatusBadRequest)
	}
	assertSecurityHeaders(t, rec)
	assertErrorPage(t, rec)
	if !strings.Contains(rec.Body.String(), "送信した内容を読み取れません") || strings.Contains(rec.Body.String(), "invalid URL escape") {
		t.Errorf("本文 = %q", rec.Body.String())
	}
	if !strings.Contains(log.String(), "invalid URL escape") {
		t.Errorf("端末へのログにパースエラーの詳細がない: %q", log.String())
	}
	if _, err := LoadVerification(dir, 1); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("拒否した送信で動作確認の結果が書かれた (err = %v)", err)
	}
}
