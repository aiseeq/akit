package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
)

// heavyThreshold — с какого объёма чтения вызов тяжёлый: 512 МиБ — около 5 CPU-с при ~10 CPU-с на ГиБ; меньшее
// чтение не стоит ожидания в очереди машины.
var heavyThreshold int64 = 512 << 20

// lightThreads — потоки лёгкого вызова: он идёт мимо очереди, и десяток таких сразу (тёплый кэш --wait у
// нескольких агентов) не должен занять все ядра; до порога это не дольше пары секунд.
const lightThreads = 2

// admission пускает тяжёлый вызов через очередь graft: перезапускает tl под `graft exec --heavy --local` и
// отдаёт код дочернего; done — вызов уже выполнен там. Под очередью (GRAFT_THREADS задан) и для лёгкого
// чтения — done false, читать здесь.
func admission(sources []source, args []string, stdout, stderr io.Writer, cache *waitCache) (int, bool, error) {
	threads, set, err := configuredThreads()
	if err != nil {
		return exitUsage, true, err
	}
	if set {
		// Под очередью потоки — столько, сколько дала очередь: иначе фоновые и простаивающие воркеры GC
		// займут все ядра сверх воркеров scan.
		runtime.GOMAXPROCS(threads)
	}
	if !set {
		var size int64
		for _, src := range sources {
			if cache != nil && cache.valid(src.path) {
				continue
			}
			// Файл, пропавший после выбора, не в счёт: его ошибку чтения scan отдаст в итог, как раньше.
			if info, err := os.Stat(src.path); err == nil {
				size += info.Size()
			}
		}
		if size < heavyThreshold {
			runtime.GOMAXPROCS(min(lightThreads, runtime.NumCPU()))
			return 0, false, nil
		}
		graft, err := exec.LookPath("graft")
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				fmt.Fprintln(stderr, "tl: graft not in PATH, reading without the machine queue")
				return 0, false, nil
			}
			return exitUsage, true, fmt.Errorf("find graft: %w", err)
		}
		executable, err := os.Executable()
		if err != nil {
			return exitUsage, true, fmt.Errorf("find tl executable: %w", err)
		}
		cmd := exec.Command(graft, append([]string{"exec", "--heavy", "--local", "--", executable}, args...)...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code := exit.ExitCode()
				if code == 125 {
					fmt.Fprintln(stderr, "tl: graft could not start the heavy command (exit 125)")
				}
				return code, true, nil
			}
			return exitUsage, true, fmt.Errorf("run graft: %w", err)
		}
		return 0, true, nil
	}
	return 0, false, nil
}

func configuredThreads() (int, bool, error) {
	raw, ok := os.LookupEnv("GRAFT_THREADS")
	if !ok {
		return 0, false, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, true, fmt.Errorf("invalid GRAFT_THREADS %q: want a positive integer", raw)
	}
	return min(n, runtime.NumCPU()), true, nil
}

func scanWorkerCount() int {
	threads, set, err := configuredThreads()
	if err == nil && set {
		return threads
	}
	return max(1, runtime.NumCPU()/2)
}
