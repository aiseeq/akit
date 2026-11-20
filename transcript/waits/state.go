package waits

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

const snapshotVersion = 2

// snapshot stores each pending wait once and represents shared pointers by index.
type snapshot struct {
	Version    int                `json:"version"`
	LastTime   time.Time          `json:"last_time"`
	Pending    []*pendingSnapshot `json:"pending"`
	Cur        *int               `json:"cur"`
	ByCall     map[string]int     `json:"by_call"`
	BG         map[string]int     `json:"bg"`
	Sends      []int              `json:"sends"`
	Wakeup     *int               `json:"wakeup"`
	TokensTo   *int               `json:"tokens_to"`
	Tasks      map[string]string  `json:"tasks"`
	Polls      map[string]int     `json:"polls"`
	ShellText  map[string]string  `json:"shell_text"`
	SessionCmd map[string]string  `json:"session_cmd"`
	PrevCmd    string             `json:"prev_cmd"`
}

type pendingSnapshot struct {
	Wait    Wait            `json:"wait"`
	Limit   int64           `json:"limit"`
	CallAt  time.Time       `json:"call_at"`
	Replies map[string]bool `json:"replies"`
	To      string          `json:"to"`
	Session string          `json:"session"`
	Codex   bool            `json:"codex"`
	OutSeen bool            `json:"out_seen"`
}

func (r *Recognizer) MarshalJSON() ([]byte, error) {
	ps := r.all()
	// tokensTo may briefly point at a wait whose output has already completed.
	if r.tokensTo != nil && !slices.Contains(ps, r.tokensTo) {
		ps = append(ps, r.tokensTo)
	}
	slices.SortFunc(ps, func(a, b *pending) int {
		ac, bc := "", ""
		if len(a.w.CallIDs) > 0 {
			ac = a.w.CallIDs[0]
		}
		if len(b.w.CallIDs) > 0 {
			bc = b.w.CallIDs[0]
		}
		return cmp.Or(cmp.Compare(a.w.Line, b.w.Line), strings.Compare(ac, bc))
	})
	ids := make(map[*pending]int, len(ps))
	st := snapshot{Version: snapshotVersion, Pending: make([]*pendingSnapshot, len(ps)),
		ByCall: make(map[string]int, len(r.byCall)), BG: make(map[string]int, len(r.bg)), Sends: make([]int, 0, len(r.sends)),
		LastTime: r.lastTime, Tasks: maps.Clone(r.tasks), Polls: make(map[string]int, len(r.polls)),
		ShellText: maps.Clone(r.shellText), SessionCmd: maps.Clone(r.sessionCmd), PrevCmd: r.prevCmd}
	for i, p := range ps {
		ids[p] = i
		st.Pending[i] = &pendingSnapshot{Wait: p.w, Limit: int64(p.limit), CallAt: p.callAt, Replies: p.replies,
			To: p.to, Session: p.session, Codex: p.codex, OutSeen: p.outSeen}
	}
	cur, err := pendingIndex(ids, r.cur)
	if err != nil {
		return nil, err
	}
	wakeup, err := pendingIndex(ids, r.wakeup)
	if err != nil {
		return nil, err
	}
	tokensTo, err := pendingIndex(ids, r.tokensTo)
	if err != nil {
		return nil, err
	}
	st.Cur, st.Wakeup, st.TokensTo = &cur, &wakeup, &tokensTo
	for k, p := range r.byCall {
		id, err := pendingIndex(ids, p)
		if err != nil {
			return nil, err
		}
		st.ByCall[k] = id
	}
	for k, p := range r.bg {
		id, err := pendingIndex(ids, p)
		if err != nil {
			return nil, err
		}
		st.BG[k] = id
	}
	for _, p := range r.sends {
		id, err := pendingIndex(ids, p)
		if err != nil {
			return nil, err
		}
		st.Sends = append(st.Sends, id)
	}
	for k, p := range r.polls {
		id, err := pendingIndex(ids, p)
		if err != nil {
			return nil, err
		}
		st.Polls[k] = id
	}
	return json.Marshal(st)
}

func pendingIndex(ids map[*pending]int, p *pending) (int, error) {
	if p == nil {
		return -1, nil
	}
	id, ok := ids[p]
	if !ok {
		return 0, fmt.Errorf("wait pointer is not in recognizer state")
	}
	return id, nil
}

func (r *Recognizer) UnmarshalJSON(data []byte) error {
	st, err := decodeSnapshot(data)
	if err != nil {
		return err
	}
	restored, err := restoreSnapshot(st)
	if err != nil {
		return err
	}
	*r = restored
	return nil
}

func decodeSnapshot(data []byte) (snapshot, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var st snapshot
	if err := dec.Decode(&st); err != nil {
		return snapshot{}, fmt.Errorf("decode waits snapshot: %w", err)
	}
	if st.Version != snapshotVersion {
		return snapshot{}, fmt.Errorf("unsupported waits snapshot version %d", st.Version)
	}
	return st, nil
}

func restoreSnapshot(st snapshot) (Recognizer, error) {
	ps := make([]*pending, len(st.Pending))
	for i, p := range st.Pending {
		if p == nil {
			return Recognizer{}, fmt.Errorf("invalid waits snapshot pending[%d]: null", i)
		}
		if len(p.Wait.CallIDs) == 0 {
			return Recognizer{}, fmt.Errorf("invalid waits snapshot pending[%d]: missing call IDs", i)
		}
		if slices.Contains(p.Wait.CallIDs, "") {
			return Recognizer{}, fmt.Errorf("invalid waits snapshot pending[%d]: empty call ID", i)
		}
		replies := p.Replies
		if replies == nil {
			replies = map[string]bool{}
		}
		ps[i] = &pending{w: p.Wait, limit: time.Duration(p.Limit), callAt: p.CallAt, replies: replies,
			to: p.To, session: p.Session, codex: p.Codex, outSeen: p.OutSeen}
	}
	if st.Cur == nil || st.Wakeup == nil || st.TokensTo == nil {
		return Recognizer{}, fmt.Errorf("invalid waits snapshot: cur, wakeup and tokens_to are required")
	}
	cur, ok := pendingRef(ps, *st.Cur)
	if !ok {
		return Recognizer{}, fmt.Errorf("invalid waits snapshot reference cur=%d", *st.Cur)
	}
	wakeup, ok := pendingRef(ps, *st.Wakeup)
	if !ok {
		return Recognizer{}, fmt.Errorf("invalid waits snapshot reference wakeup=%d", *st.Wakeup)
	}
	tokensTo, ok := pendingRef(ps, *st.TokensTo)
	if !ok {
		return Recognizer{}, fmt.Errorf("invalid waits snapshot reference tokens_to=%d", *st.TokensTo)
	}
	byCall, err := pendingMap(ps, st.ByCall, "by_call")
	if err != nil {
		return Recognizer{}, err
	}
	bg, err := pendingMap(ps, st.BG, "bg")
	if err != nil {
		return Recognizer{}, err
	}
	sends, err := pendingSlice(ps, st.Sends, "sends")
	if err != nil {
		return Recognizer{}, err
	}
	polls, err := pendingMap(ps, st.Polls, "polls")
	if err != nil {
		return Recognizer{}, err
	}

	r := Recognizer{cur: cur, lastTime: st.LastTime, byCall: byCall, bg: bg, sends: sends, wakeup: wakeup, tokensTo: tokensTo,
		tasks: maps.Clone(st.Tasks), polls: polls, shellText: maps.Clone(st.ShellText),
		sessionCmd: maps.Clone(st.SessionCmd), prevCmd: st.PrevCmd}
	if r.tasks == nil {
		r.tasks = map[string]string{}
	}
	if r.shellText == nil {
		r.shellText = map[string]string{}
	}
	if r.sessionCmd == nil {
		r.sessionCmd = map[string]string{}
	}
	return r, nil
}

func pendingRef(ps []*pending, index int) (*pending, bool) {
	if index == -1 {
		return nil, true
	}
	if index < 0 || index >= len(ps) {
		return nil, false
	}
	return ps[index], true
}

func pendingMap(ps []*pending, refs map[string]int, field string) (map[string]*pending, error) {
	out := make(map[string]*pending, len(refs))
	for key, index := range refs {
		p, ok := pendingRef(ps, index)
		if !ok || p == nil {
			return nil, fmt.Errorf("invalid waits snapshot reference %s[%s]=%d", field, key, index)
		}
		out[key] = p
	}
	return out, nil
}

func pendingSlice(ps []*pending, refs []int, field string) ([]*pending, error) {
	out := make([]*pending, len(refs))
	for i, index := range refs {
		p, ok := pendingRef(ps, index)
		if !ok || p == nil {
			return nil, fmt.Errorf("invalid waits snapshot reference %s[%d]=%d", field, i, index)
		}
		out[i] = p
	}
	return out, nil
}

// OpenState describes one active wait.
type OpenState struct {
	Line  int
	Start time.Time
}

// OpenStates returns all active waits in transcript order.
func (r *Recognizer) OpenStates() []OpenState {
	ps := r.all()
	out := make([]OpenState, 0, len(ps))
	for _, p := range ps {
		out = append(out, OpenState{Line: p.w.Line, Start: p.w.Start})
	}
	slices.SortFunc(out, func(a, b OpenState) int { return cmp.Compare(a.Line, b.Line) })
	return out
}
