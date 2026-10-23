package agent

import (
	"bufio"
	"bytes"
	"cmp"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
)

// UnattendedSchemaは無人実行モードのスキルが返す構造化出力のJSON Schema。
// 共有ガイドラインの docs/korylus/schemas/unattended-result.json と同じ内容で、
// 出力を読むUnattendedResultの型と版を揃えるためにバイナリへ埋め込む。
//
//go:embed unattended-result.json
var UnattendedSchema []byte

// UnattendedResultは無人実行モードのスキルが返す構造化出力。
type UnattendedResult struct {
	// Statusは "completed" か "escalated"。
	Status            string             `json:"status"`
	Summary           string             `json:"summary"`
	Escalations       []Escalation       `json:"escalations"`
	VerificationItems []VerificationItem `json:"verification_items"`
	ProtectedChanges  []ProtectedChange  `json:"protected_changes"`
	ProtectedChecks   []ProtectedCheck   `json:"protected_checks"`
}

// 構造化出力のstatusの値。
const (
	StatusCompleted = "completed"
	StatusEscalated = "escalated"
)

// Escalationは人間の判断を求めて止めた理由。
type Escalation struct {
	Reason      string   `json:"reason"`
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	Recommended string   `json:"recommended"`
}

// VerificationItemは作業報告書のチェックリストに載せる動作確認の項目。
type VerificationItem struct {
	// Kindは "screen" か "process"。
	Kind     string   `json:"kind"`
	Title    string   `json:"title"`
	Target   string   `json:"target"`
	Steps    []string `json:"steps"`
	Expected string   `json:"expected"`
}

// ProtectedChangeは、実装と修正のステップが触れた保護領域ごとに返す変更の説明。
// エージェントの自己申告であり、宣言との照合はゲート検査が行う。
type ProtectedChange struct {
	Area    string `json:"area"`
	Summary string `json:"summary"`
	Reason  string `json:"reason"`
}

// ProtectedCheckは、レビューが保護領域と観点ごとに返す照合の結果。
type ProtectedCheck struct {
	Area  string `json:"area"`
	Point string `json:"point"`
	// Resultは "ok" か "concern"。
	Result string `json:"result"`
	Detail string `json:"detail"`
}

// DecodeUnattendedは構造化出力を読み、statusの値を確かめる。
func DecodeUnattended(output []byte) (*UnattendedResult, error) {
	var r UnattendedResult
	dec := json.NewDecoder(bytes.NewReader(output))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("構造化出力を読み取れません: %w", err)
	}
	if !slices.Contains([]string{StatusCompleted, StatusEscalated}, r.Status) {
		return nil, fmt.Errorf("構造化出力のstatusが不正です: %q", r.Status)
	}
	return &r, nil
}

// reRateLimitは、ツールが報告した失敗の文面のうち使用量の上限によるものを拾う。
// 上限に当たったときの文面は実機で確かめられていないため、各ツールが使う
// 言い回しを広めに拾う。
var reRateLimit = regexp.MustCompile(`(?i)usage limit|rate limit|hit your limit|limit reached`)

// maxLineはstream-json / JSONLの1行の最大長。ツールの出力にファイルの内容が
// そのまま載ることがあるため、bufio.Scannerの既定値 (64KiB) より大きくする。
const maxLine = 64 << 20

// parsedは出力から取り出した内容。
type parsed struct {
	sessionID string
	// outputは構造化出力。得られなければnil。
	output []byte
	// failureはツールが報告した失敗の文面。失敗していなければ空。
	failure     string
	rateLimited bool
	// turnsとcostUSDはClaude Codeが最終結果に載せるターン数とコスト。Codexは載せない。
	turns   int
	costUSD float64
}

// claudeEventはClaude Codeのstream-jsonの1行のうち、読む項目。
type claudeEvent struct {
	Type          string `json:"type"`
	Subtype       string `json:"subtype"`
	SessionID     string `json:"session_id"`
	RateLimitInfo *struct {
		Status string `json:"status"`
	} `json:"rate_limit_info"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	NumTurns         int             `json:"num_turns"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
}

// parseClaudeはClaude Codeのstream-jsonを読む。
func parseClaude(r io.Reader) (parsed, error) {
	var p parsed
	err := eachLine(r, func(line []byte) {
		var ev claudeEvent
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		if ev.SessionID != "" {
			p.sessionID = ev.SessionID
		}
		switch ev.Type {
		case "rate_limit_event":
			if ev.RateLimitInfo != nil && ev.RateLimitInfo.Status == "rejected" {
				p.rateLimited = true
			}
		case "result":
			p.turns, p.costUSD = ev.NumTurns, ev.TotalCostUSD
			if !ev.IsError {
				// 追加の使用量などで続行したターンは、途中の通知より最終結果を優先する。
				p.rateLimited = false
			}
			if ev.IsError {
				p.failure = cmp.Or(ev.Result, ev.Subtype)
				if reRateLimit.MatchString(ev.Result) {
					p.rateLimited = true
				}
			}
			if len(ev.StructuredOutput) > 0 && !bytes.Equal(ev.StructuredOutput, []byte("null")) {
				p.output = ev.StructuredOutput
			}
		}
	})
	// 上限で止まったとき、resultの行が無いまま終わることがあっても止まった理由を残す。
	if p.rateLimited && p.failure == "" {
		p.failure = "rate_limit_eventのstatusがrejected"
	}
	return p, err
}

// codexEventはCodexのJSONLの1行のうち、読む項目。
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
}

// parseCodexはCodexのJSONLを読む。構造化出力は最後のエージェントの
// メッセージの本文に、JSONとして入る。
func parseCodex(r io.Reader) (parsed, error) {
	var p parsed
	var last string
	err := eachLine(r, func(line []byte) {
		var ev codexEvent
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		switch ev.Type {
		case "thread.started":
			p.sessionID = ev.ThreadID
		case "item.completed":
			// item.typeが "error" の項目はモデルの切り替えの注意などの警告で、
			// ターンは続くため失敗として扱わない。
			if ev.Item != nil && ev.Item.Type == "agent_message" {
				last = ev.Item.Text
			}
		case "turn.completed":
			// 接続の再試行中にもerrorが届くため、正常完了したターンの途中の失敗は残さない。
			p.failure = ""
		case "turn.failed":
			msg := ""
			if ev.Error != nil {
				msg = ev.Error.Message
			}
			// 本文が無くてもターンは失敗しているため、成功として扱わないよう文面を残す。
			p.failure = cmp.Or(msg, p.failure, "turn.failed")
		case "error":
			p.failure = ev.Message
		}
	})
	if p.failure != "" && reRateLimit.MatchString(p.failure) {
		p.rateLimited = true
	}
	if json.Valid([]byte(last)) {
		p.output = []byte(last)
	}
	return p, err
}

// eachLineはrの空でない行ごとにfnを呼ぶ。fnはJSONとして読めない行を
// 読み飛ばす。ツールが警告などをJSON以外で混ぜても、他の行から
// 取り出したセッションIDを失わないためである (元の出力はログに残る)。
func eachLine(r io.Reader, fn func(line []byte)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, maxLine)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			fn(line)
		}
	}
	return sc.Err()
}
