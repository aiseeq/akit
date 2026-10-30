package pressure

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrNoIOSource означает, что сокет источника состояния диска отсутствует.
var ErrNoIOSource = errors.New("no disk state source")

// ErrIOSourceFailed означает, что источник состояния диска ответил с ошибкой или нарушил протокол.
var ErrIOSourceFailed = errors.New("disk state source failed")

// DiskNow — текущий снимок источника состояния диска.
type DiskNow struct {
	At      time.Time
	One     DiskWindow
	Five    DiskWindow
	Thirty  DiskWindow
	NProc   int
	Open    bool
	OpenMs  int64
	Profile json.RawMessage
}

// DiskWindow — замеры окна источника состояния диска.
type DiskWindow struct {
	Steps       int64   `json:"steps"`
	IOFullMS    float64 `json:"io_full_ms"`
	StallMS     float64 `json:"stall_ms"`
	WriteBPS    float64 `json:"write_bps"`
	ReadBPS     float64 `json:"read_bps"`
	WAwaitMS    float64 `json:"w_await_ms"`
	InflightMax int64   `json:"inflight_max"`
	CPUSomeMS   float64 `json:"cpu_some_ms"`
	RunnableMax int64   `json:"runnable_max"`
}

type diskRequest struct {
	V    int    `json:"v"`
	Q    string `json:"q"`
	Kind string `json:"kind,omitempty"`
}

type diskResponse struct {
	V      int        `json:"v"`
	Error  string     `json:"error"`
	At     string     `json:"at"`
	One    DiskWindow `json:"1s"`
	Five   DiskWindow `json:"5s"`
	Thirty DiskWindow `json:"30s"`
	NProc  int        `json:"nproc"`
	Open   struct {
		Open bool  `json:"open"`
		MS   int64 `json:"ms"`
	} `json:"open"`
	Profile json.RawMessage `json:"profile"`
}

// diskPath задаёт сокет источника состояния диска для замерщика.
func diskPath() string {
	return filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "io-state.sock")
}

func readDisk(ctx context.Context, path, kind string, now time.Time) (*DiskNow, error) {
	if os.Getenv("XDG_RUNTIME_DIR") == "" && path == diskPath() {
		return nil, ErrNoIOSource
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	conn, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "unix", path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w: %w", ErrNoIOSource, err)
		}
		return nil, fmt.Errorf("%w: connect: %w", ErrIOSourceFailed, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("%w: deadline: %w", ErrIOSourceFailed, err)
	}
	if err := json.NewEncoder(conn).Encode(diskRequest{V: 1, Q: "now", Kind: kind}); err != nil {
		return nil, fmt.Errorf("%w: request: %w", ErrIOSourceFailed, err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("%w: response: %w", ErrIOSourceFailed, err)
	}
	var response diskResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON: %w", ErrIOSourceFailed, err)
	}
	if response.V != 1 {
		return nil, fmt.Errorf("%w: version %d", ErrIOSourceFailed, response.V)
	}
	if response.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrIOSourceFailed, response.Error)
	}
	at, err := time.Parse(time.RFC3339Nano, response.At)
	if err != nil {
		return nil, fmt.Errorf("%w: at: %w", ErrIOSourceFailed, err)
	}
	if now.Sub(at) > 2*time.Second || at.Sub(now) > 2*time.Second {
		return nil, fmt.Errorf("%w: snapshot at %s is stale", ErrIOSourceFailed, response.At)
	}
	return &DiskNow{At: at, One: response.One, Five: response.Five, Thirty: response.Thirty,
		NProc: response.NProc, Open: response.Open.Open, OpenMs: response.Open.MS,
		Profile: response.Profile}, nil
}
