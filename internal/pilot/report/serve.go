package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BasePathは作業報告書を配信するパス。devサイトのリバースプロキシが、アプリと同じ
// オリジンのこのパスをkoryluspilotへ転送する (パスを取り除かずに渡す)。
const BasePath = "/_koryluspilot/"

// maxFormBytesは保存の要求の本文の上限。確認項目のコメントを書き込むだけの大きさにする。
const maxFormBytes = 1 << 20

// styleHashは作業報告書のstyle要素の中身のハッシュで、Content-Security-Policyの
// style-srcに許す。style要素の中身はテンプレートの固定の文字列のため、そのまま出力される。
var styleHash = func() string {
	const open, closing = "<style>", "</style>"
	start := strings.Index(templateText, open) + len(open)
	end := strings.Index(templateText, closing)
	sum := sha256.Sum256([]byte(templateText[start:end]))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}()

// contentSecurityPolicyはすべての応答に付けるポリシー。ページはJavaScriptを使わず、
// 埋め込んだstyle要素と同じオリジンへのフォームの送信だけで動く。
var contentSecurityPolicy = "default-src 'none'; style-src " + styleHash + "; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// Serverは計画書1本の作業報告書のうち最新の版を配信し、POSTで受けた動作確認の結果を保存する。
type Server struct {
	// Dirは作業報告書を置くディレクトリ (Dirの戻り値)。
	Dir string
	// Latestは最新の版を返す。まだ作業報告書を作っていなければ0を返す。
	// 実行中のrunが新しい版を作ることがあるため、要求のたびに呼ぶ。
	Latest func() (int, error)
	// Nowは保存した時刻に使う。nilならtime.Nowを使う。
	Now func() time.Time
	// Logは、読み書きやフォームのパースの失敗の詳細を出す先。応答には詳細を含めない。nilなら出さない。
	Log io.Writer

	// muは保存を1件ずつ行うためのロック。同時に保存すると、HTMLに埋め込む結果と
	// verification-<版>.json の内容が食い違う。
	mu sync.Mutex
}

// Handlerは作業報告書を配信するハンドラーを返す。別サイトからのフォームの送信 (CSRF) は、
// Sec-Fetch-Site / Originで同一オリジンかを確かめて403で拒否する。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+BasePath+"{$}", s.show)
	mux.HandleFunc("POST "+BasePath+"{$}", s.save)
	mux.HandleFunc(BasePath+"{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "GET, HEAD, POST")
		writeError(w, http.StatusMethodNotAllowed, "この送信方法は使えません", "作業報告書を開き直し、確認結果の保存ボタンから送信してください。")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "ページが見つかりません", "作業報告書のURLを確認してください。")
	})
	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "別のサイトからの送信は受け付けられません", "作業報告書を開き直し、同じサイトの画面から保存してください。")
	}))
	return withSecurityHeaders(protection.Handler(mux))
}

// withSecurityHeadersは、エラーを含むすべての応答にセキュリティのヘッダーを付ける。
// 動作確認の結果を含むため、ブラウザにもプロキシにも保存させない。
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// showは最新の版を、保存済みの動作確認の結果を埋め込んで描画する。
func (s *Server) show(w http.ResponseWriter, _ *http.Request) {
	rep, verification, ok := s.load(w)
	if !ok {
		return
	}
	var buf bytes.Buffer
	if err := Render(&buf, rep, verification); err != nil {
		s.fail(w, err)
		return
	}
	writeHTML(w, http.StatusOK, buf.Bytes())
}

// saveは送信された動作確認の結果を verification-<版>.json に書き、report-<版>.html に
// 埋め込み直して、303で同じページに戻す。送信した画面の版が最新でなければ409で拒否する。
func (s *Server) save(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "送信した内容が大きすぎます", "コメントを短くしてから、もう一度保存してください。")
			return
		}
		if s.Log != nil {
			fmt.Fprintf(s.Log, "エラー: フォームを読み取れません: %v\n", err)
		}
		writeError(w, http.StatusBadRequest, "送信した内容を読み取れません", "作業報告書を開き直してから、もう一度保存してください。")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rep, _, ok := s.load(w)
	if !ok {
		return
	}
	sent, err := strconv.Atoi(r.PostForm.Get("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "送信した内容が正しくありません", "送信した画面の版がありません。作業報告書を開き直してから保存してください。")
		return
	}
	if sent != rep.Version {
		// 送信した画面の版の項目から、コメントに項目の番号を添える。読めなくても拒否はする。
		old, _ := Load(s.Dir, sent)
		writeConflict(w, rep.Version, old, r.PostForm)
		return
	}
	verification, err := parseVerification(rep, r.PostForm)
	if err != nil {
		writeError(w, http.StatusBadRequest, "送信した内容が正しくありません", err.Error())
		return
	}
	verification.SavedAt = s.now()
	if err := SaveVerification(s.Dir, rep, verification); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, BasePath, http.StatusSeeOther)
}

// loadは最新の版と、保存済みの動作確認の結果 (まだ無ければnil) を読む。読めなければ
// エラーの応答を書いてfalseを返す。
func (s *Server) load(w http.ResponseWriter) (*Report, *Verification, bool) {
	v, err := s.Latest()
	if err != nil {
		s.fail(w, err)
		return nil, nil, false
	}
	if v == 0 {
		writeError(w, http.StatusNotFound, "作業報告書はまだありません", "koryluspilot run で計画書の最後まで進めると、作業報告書が作られます。")
		return nil, nil, false
	}
	rep, err := Load(s.Dir, v)
	if err != nil {
		s.fail(w, err)
		return nil, nil, false
	}
	verification, err := LoadVerification(s.Dir, v)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.fail(w, err)
		return nil, nil, false
	}
	return rep, verification, true
}

// failは作業報告書を読み書きできなかったことを500で返す。詳細は応答に含めず、
// koryluspilotを動かしている端末に出す。
func (s *Server) fail(w http.ResponseWriter, err error) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, "エラー: %v\n", err)
	}
	writeError(w, http.StatusInternalServerError, "作業報告書を読み書きできません", "koryluspilot report を実行している端末のエラーを確認してください。")
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// parseVerificationは、作業報告書repの確認項目ごとに、フォームの status:<ID> と
// comment:<ID> を読む。項目に無いIDの値は読まない。
func parseVerification(rep *Report, form map[string][]string) (*Verification, error) {
	get := func(key string) string {
		if vs := form[key]; len(vs) > 0 {
			// textareaの改行はCRLFで送られるため、ファイルに残す形をLFに揃える。
			return strings.ReplaceAll(vs[0], "\r\n", "\n")
		}
		return ""
	}
	res := &Verification{Version: rep.Version, Items: make(map[string]ItemResult, len(rep.Items)), Comment: get("overall")}
	for _, it := range rep.Items {
		status := get("status:" + it.ID)
		if status != StatusOK && status != StatusNG && status != "" {
			return nil, fmt.Errorf("#%d の確認結果に %q は指定できません", it.Number, status)
		}
		res.Items[it.ID] = ItemResult{Status: status, Comment: get("comment:" + it.ID)}
	}
	return res, nil
}

var errorTmpl = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
{{- with .Comments}}
<p>送信した内容は保存していません。書いたコメントは次のとおりです。必要なら最新の版に書き写してください。</p>
<dl>
{{- range .}}
<dt>{{.Label}}</dt>
<dd><pre>{{.Text}}</pre></dd>
{{- end}}
</dl>
{{- end}}
<p><a href="{{.Link}}">最新の作業報告書を開く</a></p>
</main>
</body>
</html>
`))

// errorViewはエラーのページに渡すデータ。
type errorView struct {
	Title, Message, Link string
	Comments             []sentComment
}

// sentCommentは、保存しなかった送信に含まれていたコメント1件。
type sentComment struct {
	Label, Text string
}

func writeError(w http.ResponseWriter, status int, title, message string) {
	writeErrorView(w, status, errorView{Title: title, Message: message, Link: BasePath})
}

// writeConflictは、古い版の画面からの送信を409で拒否する。書いたコメントを失わないよう、
// 送信に含まれていたコメントをページに載せる。oldは送信した画面の版で、読めなければnil。
func writeConflict(w http.ResponseWriter, latest int, old *Report, form map[string][]string) {
	view := errorView{
		Title:   "作業報告書が新しい版になっています",
		Message: fmt.Sprintf("この画面は最新の版のものではありません。最新の第%d版を開き直して、確認結果を付け直してください。", latest),
		Link:    BasePath,
	}
	add := func(label, key string) {
		if text := firstValue(form, key); strings.TrimSpace(text) != "" {
			view.Comments = append(view.Comments, sentComment{Label: label, Text: text})
		}
	}
	add("全体へのコメント", "overall")
	if old != nil {
		for _, it := range old.Items {
			add(fmt.Sprintf("第%d版 #%d %s", old.Version, it.Number, it.Title), "comment:"+it.ID)
		}
	} else {
		var keys []string
		for key := range form {
			if strings.HasPrefix(key, "comment:") {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		for _, key := range keys {
			add(strings.TrimPrefix(key, "comment:"), key)
		}
	}
	writeErrorView(w, http.StatusConflict, view)
}

func writeErrorView(w http.ResponseWriter, status int, view errorView) {
	var buf bytes.Buffer
	if err := errorTmpl.Execute(&buf, view); err != nil {
		http.Error(w, view.Title, status)
		return
	}
	writeHTML(w, status, buf.Bytes())
}

func writeHTML(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// 書き込みの失敗は接続が切れたときで、返す相手がいない。
	_, _ = w.Write(body)
}

func firstValue(form map[string][]string, key string) string {
	if vs := form[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}
