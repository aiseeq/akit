// Package codexlimit reads the weekly account limits from the Codex app server.
package codexlimit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/aiseeq/akit/proc"
)

// WeekMins is the length of a weekly rate limit window in minutes.
const WeekMins = 10080

// Resets describes credits that can reset the current rate limit window early.
type Resets struct {
	Available int               `json:"availableCount"`
	Credits   []json.RawMessage `json:"credits"`
	// Known is false when the answer had no rateLimitResetCredits at all, so a
	// missing report is not read as zero resets.
	Known bool `json:"-"`
}

// Limits contains the weekly account limits reported by the app server.
type Limits struct {
	UsedPercent          float64
	WindowMins           int
	ResetsAt             time.Time
	PlanType             string
	AccountID            string
	OrdinaryUsageAllowed bool
	Resets               Resets
}

// answer is the JSON-RPC envelope; each request decodes its own result, so a field
// of one method's result never affects the parsing of another.
type answer struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type rateLimitsResult struct {
	OrdinaryUsageAllowed bool `json:"ordinaryUsageAllowed"`
	RateLimits           struct {
		Primary *struct {
			UsedPercent        float64 `json:"usedPercent"`
			WindowDurationMins int     `json:"windowDurationMins"`
			ResetsAt           int64   `json:"resetsAt"`
		} `json:"primary"`
		PlanType string `json:"planType"`
	} `json:"rateLimits"`
	AccountID string  `json:"accountId"`
	Resets    *Resets `json:"rateLimitResetCredits"`
}

const initializeRequests = "{\"id\":1,\"method\":\"initialize\",\"params\":{\"clientInfo\":{\"name\":\"akit\",\"title\":\"akit\",\"version\":\"0\"}}}\n" +
	"{\"method\":\"initialized\"}\n"

// StopWaitDelay is how long stopping waits for the server's output copies after the
// kill; Read and Consume set it as cmd.WaitDelay when the caller left that zero.
const StopWaitDelay = time.Second

// Read starts cmd, reads the weekly account limits, and stops the app server.
func Read(ctx context.Context, cmd *exec.Cmd) (Limits, error) {
	raw, err := exchange(ctx, cmd, `{"id":2,"method":"account/rateLimits/read"}`)
	if err != nil {
		return Limits{}, err
	}
	var result rateLimitsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return Limits{}, fmt.Errorf("codexlimit: decode rate limits: %w", err)
	}
	return parseAnswer(result)
}

// exchange sends an app-server request and returns the raw result of its answer,
// stopping cmd on every started path.
//
// An answer read before the context ended is returned even if stopping the server
// outlives the context: for a consume request that answer means the credit is spent.
func exchange(ctx context.Context, cmd *exec.Cmd, request string) (response json.RawMessage, retErr error) {
	// A context that has already ended never starts the server, so no request is sent.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("codexlimit: context ended before start: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codexlimit: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if closeErr := stdin.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close stdin pipe: %w", closeErr))
		}
		return nil, fmt.Errorf("codexlimit: stdout pipe: %w", err)
	}
	// Killing the server does not close a stdout or stderr copy held by a process it
	// started; without a bound Wait would block on that copy past the context.
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = StopWaitDelay
	}
	if err := cmd.Start(); err != nil {
		if closeErr := stdin.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close stdin pipe: %w", closeErr))
		}
		if closeErr := stdout.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close stdout pipe: %w", closeErr))
		}
		return nil, fmt.Errorf("codexlimit: start app-server: %w", err)
	}
	defer func() {
		if err := stop(cmd, stdin); err != nil {
			response = nil
			if retErr == nil {
				retErr = err
			} else {
				retErr = errors.Join(retErr, err)
			}
		}
	}()

	// The write runs aside: a server that does not read stdin would otherwise block it
	// past the context, and stop would never run. Closing stdin in stop ends a blocked write.
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(stdin, initializeRequests+request+"\n")
		written <- err
	}()
	a, err := readAnswer(ctx, stdout, written)
	if err != nil {
		return nil, err
	}
	if a.Error != nil {
		return nil, fmt.Errorf("codexlimit: %s", strings.TrimSpace(a.Error.Message))
	}
	if len(a.Result) == 0 || string(a.Result) == "null" {
		return nil, errors.New("codexlimit: answer has no result")
	}
	return a.Result, nil
}

func readAnswer(ctx context.Context, stdout io.Reader, written <-chan error) (answer, error) {
	// The scanner sends exactly one result — the answer with id 2, a read error, or
	// the end of output — into a buffered channel, so the send never blocks and an
	// answer once scanned is never dropped; the goroutine ends when stop closes stdout.
	type scanResult struct {
		a   answer
		err error
	}
	result := make(chan scanResult, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var a answer
			if json.Unmarshal(scanner.Bytes(), &a) != nil || a.ID != 2 {
				continue
			}
			result <- scanResult{a: a}
			return
		}
		if err := scanner.Err(); err != nil {
			result <- scanResult{err: fmt.Errorf("codexlimit: read app-server output: %w", err)}
			return
		}
		result <- scanResult{err: errors.New("codexlimit: app-server closed stdout without an answer")}
	}()

	for {
		select {
		case <-ctx.Done():
			// select picks at random among ready cases: an answer already scanned wins.
			select {
			case got := <-result:
				return got.a, got.err
			default:
			}
			return answer{}, fmt.Errorf("codexlimit: no answer before context ended: %w", ctx.Err())
		case err := <-written:
			if err != nil {
				return answer{}, fmt.Errorf("codexlimit: write requests: %w", err)
			}
			written = nil // a nil channel never fires again
		case got := <-result:
			return got.a, got.err
		}
	}
}

func stop(cmd *exec.Cmd, stdin io.Closer) error {
	var errs []error
	if err := stdin.Close(); err != nil {
		errs = append(errs, fmt.Errorf("codexlimit: close stdin: %w", err))
	}
	if _, err := proc.StopChild(cmd.Process.Pid, 0, nil); err != nil {
		errs = append(errs, fmt.Errorf("codexlimit: stop app-server: %w", err))
	}
	var exit *exec.ExitError
	// ErrWaitDelay only says an output copy held by another process was cut; the server is gone.
	if err := cmd.Wait(); err != nil && !errors.As(err, &exit) && !errors.Is(err, exec.ErrWaitDelay) {
		errs = append(errs, fmt.Errorf("codexlimit: wait for app-server: %w", err))
	}
	return errors.Join(errs...)
}

func parseAnswer(r rateLimitsResult) (Limits, error) {
	if r.RateLimits.Primary == nil {
		return Limits{}, errors.New("codexlimit: answer has no primary rate limit")
	}
	p := r.RateLimits.Primary
	if p.WindowDurationMins != WeekMins {
		return Limits{}, fmt.Errorf("codexlimit: window is %d minutes, want %d", p.WindowDurationMins, WeekMins)
	}
	var resets Resets
	if r.Resets != nil {
		resets = *r.Resets
		resets.Known = true
	}
	return Limits{
		UsedPercent:          p.UsedPercent,
		WindowMins:           p.WindowDurationMins,
		ResetsAt:             time.Unix(p.ResetsAt, 0),
		PlanType:             r.RateLimits.PlanType,
		AccountID:            r.AccountID,
		OrdinaryUsageAllowed: r.OrdinaryUsageAllowed,
		Resets:               resets,
	}, nil
}
