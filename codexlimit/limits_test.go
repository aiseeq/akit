package codexlimit

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fullAnswer = `{"id":2,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"primary":{"usedPercent":95,"windowDurationMins":10080,"resetsAt":1794148763},"planType":"synthetic-plan"},"accountId":"synthetic-account","rateLimitResetCredits":{"availableCount":1,"credits":[{"status":"available","title":"synthetic credit"},{"n":2}]}}}`

func helperCommand(t *testing.T, scenario string, response string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAppServerHelper$")
	cmd.Stderr = os.Stderr
	env := append(os.Environ(), "CODEXLIMIT_HELPER="+scenario, "CODEXLIMIT_RESPONSE="+response)
	cmd.Env = env
	return cmd
}

func TestAppServerHelper(t *testing.T) {
	scenario := os.Getenv("CODEXLIMIT_HELPER")
	if scenario == "" {
		return
	}
	want := []string{
		`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"akit","title":"akit","version":"0"}}}`,
		`{"method":"initialized"}`,
		`{"id":2,"method":"account/rateLimits/read"}`,
	}
	r := bufio.NewReader(os.Stdin)
	for _, expected := range want {
		got, err := r.ReadString('\n')
		if err != nil || strings.TrimSuffix(got, "\n") != expected {
			fmt.Fprintf(os.Stderr, "unexpected request %q (error %v), want %q\n", got, err, expected)
			os.Exit(3)
		}
	}
	if scenario == "closed" {
		os.Exit(0)
	}
	if scenario == "deadline" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if _, err := io.WriteString(os.Stdout, os.Getenv("CODEXLIMIT_RESPONSE")); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}

func TestReadParsesFullAnswer(t *testing.T) {
	response := `{"id":1,"result":{}}` + "\n" + fullAnswer + "\n"
	got, err := Read(context.Background(), helperCommand(t, "answer", response))
	if err != nil {
		t.Fatal(err)
	}
	if got.UsedPercent != 95 || got.WindowMins != WeekMins || got.ResetsAt.Unix() != 1794148763 || got.PlanType != "synthetic-plan" || got.AccountID != "synthetic-account" || !got.OrdinaryUsageAllowed {
		t.Fatalf("limits: %+v", got)
	}
	if got.Resets.Available != 1 || len(got.Resets.Credits) != 2 || string(got.Resets.Credits[0]) != `{"status":"available","title":"synthetic credit"}` || string(got.Resets.Credits[1]) != `{"n":2}` {
		t.Fatalf("resets: %+v", got.Resets)
	}
}

func TestReadAnswerError(t *testing.T) {
	got, err := Read(context.Background(), helperCommand(t, "answer", `{"id":2,"error":{"message":"synthetic auth failure"}}`))
	if err == nil || !strings.Contains(err.Error(), "codexlimit: synthetic auth failure") || !reflect.DeepEqual(got, Limits{}) {
		t.Fatalf("Read = %+v, %v", got, err)
	}
}

func TestReadNotWeeklyWindow(t *testing.T) {
	response := `{"id":2,"result":{"rateLimits":{"primary":{"windowDurationMins":300}}}}`
	got, err := Read(context.Background(), helperCommand(t, "answer", response))
	if err == nil || !strings.Contains(err.Error(), "300") || !reflect.DeepEqual(got, Limits{}) {
		t.Fatalf("Read = %+v, %v", got, err)
	}
}

func TestReadNoPrimary(t *testing.T) {
	got, err := Read(context.Background(), helperCommand(t, "answer", `{"id":2,"result":{"rateLimits":{}}}`))
	if err == nil || !strings.Contains(err.Error(), "primary") || !reflect.DeepEqual(got, Limits{}) {
		t.Fatalf("Read = %+v, %v", got, err)
	}
}

func TestReadNoResult(t *testing.T) {
	got, err := Read(context.Background(), helperCommand(t, "answer", `{"id":2}`))
	if err == nil || !strings.Contains(err.Error(), "result") || !reflect.DeepEqual(got, Limits{}) {
		t.Fatalf("Read = %+v, %v", got, err)
	}
}

func TestReadClosedWithoutAnswer(t *testing.T) {
	got, err := Read(context.Background(), helperCommand(t, "closed", ""))
	if err == nil || !strings.Contains(err.Error(), "closed stdout") || !reflect.DeepEqual(got, Limits{}) {
		t.Fatalf("Read = %+v, %v", got, err)
	}
}

func TestReadStartError(t *testing.T) {
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing-codex"))
	got, err := Read(context.Background(), cmd)
	if err == nil || !strings.Contains(err.Error(), "codexlimit: start") || !reflect.DeepEqual(got, Limits{}) {
		t.Fatalf("Read = %+v, %v", got, err)
	}
}

func TestReadContextDeadlineKillsServer(t *testing.T) {
	cmd := helperCommand(t, "deadline", "")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	got, err := Read(ctx, cmd)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Read took %v", elapsed)
	}
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "context deadline exceeded") || !reflect.DeepEqual(got, Limits{}) {
		t.Fatalf("Read = %+v, %v", got, err)
	}
	// The server is the direct child, so its pid is known even if the deadline
	// came before it read the requests; after Read it must be killed and reaped.
	if cmd.ProcessState == nil {
		t.Fatal("server process was not waited for")
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("server process %d is still present (kill probe: %v)", pid, err)
	}
}

func TestReadSkipsNotificationsAndForeignIDs(t *testing.T) {
	response := "this is not JSON\n" +
		`{"method":"account/rateLimits/updated","params":{}}` + "\n" +
		`{"id":1,"result":{}}` + "\n" + fullAnswer + "\n"
	got, err := Read(context.Background(), helperCommand(t, "answer", response))
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "synthetic-account" {
		t.Fatalf("limits: %+v", got)
	}
}
