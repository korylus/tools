package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/gate"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/pilot/usage"
)

const (
	// maxAgentFailuresは、使用量の上限によらない失敗が続いたときに止めるまでの回数。
	maxAgentFailures = 2
	// rateLimitWaitは、使用量の上限に当たったのに、取り直した使用率からは待つ
	// 時刻を決められないときに待つ時間。
	rateLimitWait = 30 * time.Minute
	// resetMarginは、枠のリセット時刻を過ぎてから使用率を取り直すまでの余裕。
	// 取得元によってリセット時刻の秒の丸めが異なるため幅を持たせる。
	resetMargin = time.Minute
)

// agentStepはエージェントを起動するステップ1つ。
type agentStep struct {
	step state.Step
	role string
	kind agent.Kind
	// promptは最初に渡す指示。セッションを再開するときも同じものを渡す。
	prompt string
	// sessionは、再開するセッションIDを記録するステップ。
	session state.Step
	// schemaは、無人実行モードの構造化出力を求めるかどうか。
	schema bool
	// allowedは、このステップでエージェントに書かせるリポジトリの外のパス
	// (プロジェクトからの相対パスのglob)。
	allowed []string
}

// runAgentは、役割のランナーでエージェントを起動し、構造化出力を返す。
// 起動の前に使用量を確かめ、上限を超えそうならリセットまで待つ。上限に当たって
// 失敗したら、リセットを待ってから同じセッションで続けさせる。ステップの前後で
// リポジトリの外の監視するパスを比べ、変わっていれば止める。
// schemaを求めないステップでは、構造化出力にnilを返す。
func (r *run) runAgent(ctx context.Context, s agentStep) (*agent.UnattendedResult, bool, error) {
	role := r.cfg.Roles[s.role]
	tool := agent.Tool(role.Tool)
	req := agent.NewRequest(role, s.kind)
	req.Prompt = s.prompt
	req.SessionID = r.st.Sessions[s.session]
	if s.schema {
		req.Schema = agent.UnattendedSchema
	}
	ag := agent.Runner{Exec: r.roleRunner(s.role), ClaudeCommand: r.p.ClaudeCommand, CodexCommand: r.p.CodexCommand}

	r.logf("タスク %s: %s (%s) を始めます", r.st.Task, s.step.Label(), s.role)
	for failures := 0; ; {
		if r.st.OutsideBefore != nil {
			// 中断前の基準が残っていれば、使用率の取得やエージェントの起動より先に検査する。
			before, err := r.outsideBefore(ctx)
			if err != nil {
				return nil, true, r.stopErr(s.role, "ステップの前にリポジトリの外の監視するパスを読めません", err)
			}
			if stopped, err := r.checkOutside(ctx, s, before); err != nil || stopped {
				return nil, stopped, err
			}
		}
		before, err := r.outsideBefore(ctx)
		if err != nil {
			return nil, true, r.stopErr(s.role, "ステップの前にリポジトリの外の監視するパスを読めません", err)
		}
		if err := r.waitUsage(ctx, tool, s.step); err != nil {
			return nil, false, err
		}
		usageBefore := r.currentUsage(ctx, tool)
		if req.LogPath, err = r.logPath(string(s.step)); err != nil {
			return nil, false, err
		}

		ev := state.Event{Kind: state.EventAgent, Task: r.st.Task, Step: s.step, Role: s.role, Tool: string(tool), Started: r.now()}
		res, runErr := ag.Run(ctx, req)
		ev.Ended = r.now()
		ev.Outcome = state.OutcomeOK
		switch {
		case errors.Is(runErr, agent.ErrRateLimited):
			ev.Outcome = state.OutcomeRateLimited
		case runErr != nil:
			ev.Outcome = state.OutcomeFailed
		}
		var out *agent.UnattendedResult
		var decodeErr error
		saved := -1
		if res != nil {
			ev.Turns, ev.CostUSD = res.Turns, res.CostUSD
			if res.SessionID != "" {
				req.SessionID = res.SessionID
				if r.st.Sessions == nil {
					r.st.Sessions = map[state.Step]string{}
				}
				r.st.Sessions[s.session] = res.SessionID
			}
			if s.schema {
				out, decodeErr = agent.DecodeUnattended(res.Output)
				if decodeErr == nil {
					// 出力の保存はステップの成功を意味しない。失敗した試行や監視違反で止まる
					// 場合にも残し、試行の結果を添えて作業報告書で選べるようにする。
					saved = len(r.st.Outputs)
					r.st.Outputs = append(r.st.Outputs, state.Output{Task: r.st.Task, Step: s.step, Role: s.role, At: r.now(), Outcome: ev.Outcome, Result: *out})
				}
			}
		}
		r.event(ev)
		if err := r.save(); err != nil {
			return nil, false, err
		}
		// 中断の要求が届いていても、終了したエージェントの変更は検査する。
		// 監視自身のタイムアウトはOutsideWatcherが設定する。
		if stopped, err := r.checkOutside(context.WithoutCancel(ctx), s, before); err != nil || stopped {
			if stopped && saved >= 0 {
				r.st.Outputs[saved].Stopped = true
				if saveErr := r.save(); err == nil {
					err = saveErr
				}
			}
			return nil, stopped, err
		}
		if runErr == nil {
			r.st.OutsideBefore = nil
			if err := r.save(); err != nil {
				return nil, false, err
			}
		}
		r.recordUsage(ctx, tool, s.step, usageBefore, req.LogPath)

		if runErr == nil {
			if decodeErr != nil {
				return nil, true, r.stopErr(s.role, fmt.Sprintf("%sのエージェントの構造化出力を読み取れません (ログ: %s)", s.step.Label(), req.LogPath), decodeErr)
			}
			return out, false, nil
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		switch {
		case errors.Is(runErr, agent.ErrRateLimited):
			r.logf("%s が使用量の上限に当たりました: %v", tool, runErr)
			if err := r.waitReset(ctx, tool, s.step); err != nil {
				return nil, false, err
			}
		case errors.Is(runErr, agent.ErrTimeout):
			// 監視の検査を通ったあとの停止なので、人間が解消した状態を次の基準にする。
			r.st.OutsideBefore = nil
			return nil, true, r.stopErr(s.role, fmt.Sprintf("%sのエージェントがタイムアウトしました", s.step.Label()), runErr)
		default:
			failures++
			if failures >= maxAgentFailures {
				r.st.OutsideBefore = nil
				return nil, true, r.stopErr(s.role, fmt.Sprintf("%sのエージェントの実行が続けて失敗しました", s.step.Label()), runErr)
			}
			r.logf("%sのエージェントの実行が失敗したため、もう一度試します: %v", s.step.Label(), runErr)
		}
		if req.SessionID != "" {
			req.Prompt = continuePrompt(s.schema)
		}
	}
}

// stopErrはerrの内容を詳細に添えて止める。状態を書けなかったときはそのエラーを返す。
func (r *run) stopErr(role, reason string, err error) error {
	_, saveErr := r.stop(role, reason, err.Error())
	return saveErr
}

// outsideBeforeはステップの前の、リポジトリの外の監視するパスの状態を返す。
// ステップの途中で落ちたときは、状態に残した前の状態を使う。
func (r *run) outsideBefore(ctx context.Context) ([]gate.OutsideSnapshot, error) {
	if len(r.watcher.Watches) == 0 {
		return nil, nil
	}
	if r.st.OutsideBefore != nil {
		var before []gate.OutsideSnapshot
		if err := json.Unmarshal(r.st.OutsideBefore, &before); err != nil {
			return nil, fmt.Errorf("状態に残したステップの前の状態を読み取れません: %w", err)
		}
		return before, nil
	}
	before, err := r.watcher.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(before)
	if err != nil {
		return nil, err
	}
	r.st.OutsideBefore = b
	return before, r.save()
}

// checkOutsideは現在の状態をbeforeと比べ、許可外の変更があれば止める。
// 検査を通った状態は次の試行や中断からの復帰で比較するために保存する。
func (r *run) checkOutside(ctx context.Context, s agentStep, before []gate.OutsideSnapshot) (bool, error) {
	if len(r.watcher.Watches) == 0 {
		return false, nil
	}
	after, err := r.watcher.Snapshot(ctx)
	if err != nil {
		return true, r.stopErr(s.role, "ステップのあとにリポジトリの外の監視するパスを読めません", err)
	}
	findings, err := r.watcher.Check(before, after, s.allowed)
	if err != nil {
		return false, err
	}
	if len(findings) > 0 {
		// 変更を確認して停止した後は、人間が解消した状態を新しい基準にする。
		r.st.OutsideBefore = nil
		return r.stop(s.role, fmt.Sprintf("%sのステップで、リポジトリの外の許可外の変更を検知しました", s.step.Label()), findingMessages(findings)...)
	}
	b, err := json.Marshal(after)
	if err != nil {
		return false, err
	}
	r.st.OutsideBefore = b
	return false, r.save()
}

// waitUsageは、toolのすべての枠で「現在の使用率 + 見積もり × 安全係数 ≤ 上限」を
// 満たすまで、上限を超えそうな枠のリセットを待つ。使用率を取得できないときは、
// 確かめずに進める (上限に当たったら、その失敗から待って再開する)。
func (r *run) waitUsage(ctx context.Context, tool agent.Tool, step state.Step) error {
	for {
		latest, err := r.tracker.Current(ctx, tool)
		if err != nil {
			r.logf("警告: %s の使用率を取得できないため、確かめずに進めます: %v", tool, err)
			return nil
		}
		_, steps, err := r.tracker.Log.Read()
		if err != nil {
			r.logf("警告: 使用率の記録を読めないため、確かめずに進めます: %v", err)
			return nil
		}
		d := usage.Check(r.cfg.Usage, latest, usage.Estimates(r.cfg.Usage, steps, tool, step), r.now())
		if !d.Wait {
			return nil
		}
		r.logf("%s の%sが上限を超えそうなため (使用率 %g%% + 見積もり %g%% > 上限 %g%%)、%s まで待ちます",
			tool, d.Window.Label(), d.Percent, d.Needed, d.Cap, d.Until.Local().Format(time.DateTime))
		if err := r.sleepUntil(ctx, tool, step, d.Window, d.Until); err != nil {
			return err
		}
	}
}

// waitResetは、使用量の上限に当たったtoolの使用率を取り直し、上限を超えている
// 枠のリセットまで待つ。待つ時刻を決められなければrateLimitWaitだけ待つ。
func (r *run) waitReset(ctx context.Context, tool agent.Tool, step state.Step) error {
	until, window := r.now().Add(rateLimitWait), usage.Window("")
	if latest, err := r.tracker.Refresh(ctx, tool); err != nil {
		r.logf("警告: %s の使用率を取り直せません: %v", tool, err)
	} else if _, steps, err := r.tracker.Log.Read(); err == nil {
		if d := usage.Check(r.cfg.Usage, latest, usage.Estimates(r.cfg.Usage, steps, tool, step), r.now()); d.Wait {
			until, window = d.Until, d.Window
		}
	}
	r.logf("%s まで待ってから、同じセッションで続けます", until.Local().Format(time.DateTime))
	return r.sleepUntil(ctx, tool, step, window, until)
}

// sleepUntilはuntilのresetMargin後まで待ち、使用量の待機として記録する。
func (r *run) sleepUntil(ctx context.Context, tool agent.Tool, step state.Step, window usage.Window, until time.Time) error {
	started := r.now()
	err := r.sleep(ctx, until.Add(resetMargin).Sub(started))
	r.event(state.Event{Kind: state.EventUsageWait, Task: r.st.Task, Step: step, Tool: string(tool), Started: started, Ended: r.now(), Window: string(window)})
	return err
}

// currentUsageはステップの前のtoolの使用率を返す。取得できなければnilを返す
// (そのステップの消費は見積もりに使わない)。
func (r *run) currentUsage(ctx context.Context, tool agent.Tool) map[usage.Window]usage.Sample {
	latest, err := r.tracker.Current(ctx, tool)
	if err != nil {
		return nil
	}
	return latest
}

// recordUsageは、ステップのあとの使用率を記録し、前後の差をステップの消費として残す。
// Claude Codeはステップの出力 (stream-json) の値を使い、無ければ取り直す。
// 記録できなくても実行は止めない。
func (r *run) recordUsage(ctx context.Context, tool agent.Tool, step state.Step, before map[usage.Window]usage.Sample, logPath string) {
	var after map[usage.Window]usage.Sample
	if tool == agent.Claude {
		if f, err := os.Open(logPath); err == nil { //#nosec G304 -- logPathが状態ディレクトリの下に決めたパス
			after, err = r.tracker.RecordClaudeStream(f)
			_ = f.Close()
			if err != nil {
				r.logf("警告: ステップの出力から使用率を記録できません: %v", err)
			}
		}
	}
	if len(after) == 0 {
		var err error
		if after, err = r.tracker.Refresh(ctx, tool); err != nil {
			r.logf("警告: ステップのあとの %s の使用率を取得できません: %v", tool, err)
			return
		}
	}
	if before == nil {
		return
	}
	if err := r.tracker.Log.AppendSteps(usage.StepUsages(tool, step, before, after, r.now())); err != nil {
		r.logf("警告: ステップの消費を記録できません: %v", err)
	}
}

// logPathは、実行中のタスクのname (ステップ名など) のログのパスを、実行した順の番号を
// 付けて返す。
func (r *run) logPath(name string) (string, error) {
	dir := filepath.Join(r.dir.LogsPath(), r.st.Task)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	n := 0
	for _, e := range entries {
		// エージェントの標準エラー出力のログは、対になる標準出力のログと同じ番号にする。
		if !strings.HasSuffix(e.Name(), ".stderr") {
			n++
		}
	}
	ext := ".jsonl"
	if name == "gate" {
		// ゲートのコマンドのログは、コマンドごとのファイルを置くディレクトリにする。
		ext = ""
	}
	return filepath.Join(dir, fmt.Sprintf("%03d-%s%s", n+1, name, ext)), nil
}
