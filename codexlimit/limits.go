// Package codexlimit reads the weekly account limits from the Codex app server.
package codexlimit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// WeekMins is the length of a weekly rate limit window in minutes.
const WeekMins = 10080

// Resets describes credits that can reset the current rate limit window early.
type Resets struct {
	Available int               `json:"availableCount"`
	Credits   []json.RawMessage `json:"credits"`
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

type answer struct {
	ID     int `json:"id"`
	Result *struct {
		OrdinaryUsageAllowed bool `json:"ordinaryUsageAllowed"`
		RateLimits           struct {
			Primary *struct {
				UsedPercent        float64 `json:"usedPercent"`
				WindowDurationMins int     `json:"windowDurationMins"`
				ResetsAt           int64   `json:"resetsAt"`
			} `json:"primary"`
			PlanType string `json:"planType"`
		} `json:"rateLimits"`
		AccountID string `json:"accountId"`
		Resets    Resets `json:"rateLimitResetCredits"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Read starts cmd, reads the weekly account limits, and stops the app server.
func Read(ctx context.Context, cmd *exec.Cmd) (limits Limits, retErr error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Limits{}, fmt.Errorf("codexlimit: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if closeErr := stdin.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close stdin pipe: %w", closeErr))
		}
		return Limits{}, fmt.Errorf("codexlimit: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		if closeErr := stdin.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close stdin pipe: %w", closeErr))
		}
		if closeErr := stdout.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close stdout pipe: %w", closeErr))
		}
		return Limits{}, fmt.Errorf("codexlimit: start app-server: %w", err)
	}
	scanCtx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()
	defer func() {
		if err := stop(cmd, stdin); err != nil {
			limits = Limits{}
			if retErr == nil {
				retErr = err
			} else {
				retErr = errors.Join(retErr, err)
			}
		}
	}()

	requests := "{\"id\":1,\"method\":\"initialize\",\"params\":{\"clientInfo\":{\"name\":\"akit\",\"title\":\"akit\",\"version\":\"0\"}}}\n" +
		"{\"method\":\"initialized\"}\n" +
		"{\"id\":2,\"method\":\"account/rateLimits/read\"}\n"
	if _, err := io.WriteString(stdin, requests); err != nil {
		return Limits{}, fmt.Errorf("codexlimit: write requests: %w", err)
	}

	return readLimits(ctx, stdout, scanCtx)
}

func readLimits(ctx context.Context, stdout io.Reader, scanCtx context.Context) (Limits, error) {
	type scanResult struct {
		line []byte
		err  error
	}
	lines := make(chan scanResult, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := append([]byte{}, scanner.Bytes()...)
			select {
			case lines <- scanResult{line: line}:
			case <-scanCtx.Done():
				return
			}
		}
		select {
		case lines <- scanResult{err: scanner.Err()}:
		case <-scanCtx.Done():
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return Limits{}, fmt.Errorf("codexlimit: no answer before context ended: %w", ctx.Err())
		case got := <-lines:
			if got.err != nil {
				return Limits{}, fmt.Errorf("codexlimit: read app-server output: %w", got.err)
			}
			if got.line == nil {
				return Limits{}, errors.New("codexlimit: app-server closed stdout without an answer")
			}
			var a answer
			if json.Unmarshal(got.line, &a) != nil || a.ID != 2 {
				continue
			}
			return parseAnswer(a)
		}
	}
}

func stop(cmd *exec.Cmd, stdin io.Closer) error {
	var errs []error
	if err := stdin.Close(); err != nil {
		errs = append(errs, fmt.Errorf("codexlimit: close stdin: %w", err))
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		errs = append(errs, fmt.Errorf("codexlimit: kill app-server: %w", err))
	}
	var exit *exec.ExitError
	if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
		errs = append(errs, fmt.Errorf("codexlimit: wait for app-server: %w", err))
	}
	return errors.Join(errs...)
}

func parseAnswer(a answer) (Limits, error) {
	if a.Error != nil {
		return Limits{}, fmt.Errorf("codexlimit: %s", strings.TrimSpace(a.Error.Message))
	}
	if a.Result == nil {
		return Limits{}, errors.New("codexlimit: answer has no result")
	}
	if a.Result.RateLimits.Primary == nil {
		return Limits{}, errors.New("codexlimit: answer has no primary rate limit")
	}
	p := a.Result.RateLimits.Primary
	if p.WindowDurationMins != WeekMins {
		return Limits{}, fmt.Errorf("codexlimit: window is %d minutes, want %d", p.WindowDurationMins, WeekMins)
	}
	return Limits{
		UsedPercent:          p.UsedPercent,
		WindowMins:           p.WindowDurationMins,
		ResetsAt:             time.Unix(p.ResetsAt, 0),
		PlanType:             a.Result.RateLimits.PlanType,
		AccountID:            a.Result.AccountID,
		OrdinaryUsageAllowed: a.Result.OrdinaryUsageAllowed,
		Resets:               a.Result.Resets,
	}, nil
}
