package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	coreApp "github.com/exgamer/gosdk-core/pkg/app"
	sdkctx "github.com/exgamer/gosdk-core/pkg/context"
	"github.com/exgamer/gosdk-core/pkg/errorreporter"
	"github.com/spf13/cobra"
)

// recReporter Записывает события и вызовы Flush.
type recReporter struct {
	mu      sync.Mutex
	events  []recEvent
	flushes int
}

type recEvent struct {
	err  error
	opts errorreporter.Options
}

func (r *recReporter) Capture(_ context.Context, err error, opts errorreporter.Options) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, recEvent{err: err, opts: opts})
}

func (r *recReporter) Flush(time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.flushes++

	return true
}

func (r *recReporter) snapshot() ([]recEvent, int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]recEvent(nil), r.events...), r.flushes
}

var errJob = errors.New("import failed")

func testCommands() []*cobra.Command {
	job := &cobra.Command{
		Use:  "job",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	job.Flags().String("mode", "", "")

	required := &cobra.Command{Use: "required", RunE: func(*cobra.Command, []string) error { return nil }}
	required.Flags().String("id", "", "")
	_ = required.MarkFlagRequired("id")

	return []*cobra.Command{
		job,
		required,
		{Use: "exact", Args: cobra.ExactArgs(1), RunE: func(*cobra.Command, []string) error { return nil }},
		{Use: "fail", RunE: func(*cobra.Command, []string) error { return errJob }},
		{Use: "prefail", PreRunE: func(*cobra.Command, []string) error { return errJob }, RunE: func(*cobra.Command, []string) error { return nil }},
		{Use: "boom", RunE: func(*cobra.Command, []string) error { panic("nil map") }},
		{Use: "boomrun", Run: func(*cobra.Command, []string) { panic("plain run") }},
		{Use: "selfreport", RunE: func(cmd *cobra.Command, _ []string) error {
			return errorreporter.CaptureError(cmd.Context(), errJob, map[string]string{"own": "1"})
		}},
	}
}

func TestConsoleKernel_Start(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantErr     string
		wantErrIs   error
		wantEvents  int
		wantLevel   errorreporter.Level
		wantCommand string
	}{
		{name: "success", args: []string{"job"}},
		{name: "command error is reported", args: []string{"fail"}, wantErrIs: errJob, wantEvents: 1, wantLevel: errorreporter.LevelError, wantCommand: "fail"},
		{name: "pre-run error is reported", args: []string{"prefail"}, wantErrIs: errJob, wantEvents: 1, wantLevel: errorreporter.LevelError, wantCommand: "prefail"},
		{name: "already reported by command", args: []string{"selfreport"}, wantErrIs: errJob, wantEvents: 1, wantLevel: errorreporter.LevelError},
		{name: "unknown command", args: []string{"jbo"}, wantErr: "unknown command"},
		{name: "unknown flag", args: []string{"job", "--mdoe=x"}, wantErr: "unknown flag"},
		{name: "extra args", args: []string{"job", "x"}, wantErr: "unknown command"},
		{name: "wrong arg count", args: []string{"exact"}, wantErr: "accepts 1 arg"},
		{name: "missing required flag", args: []string{"required"}, wantErr: "required flag"},
		{name: "panic in RunE", args: []string{"boom"}, wantErr: "console command boom panic: nil map", wantEvents: 1, wantLevel: errorreporter.LevelFatal, wantCommand: "boom"},
		{name: "panic in Run", args: []string{"boomrun"}, wantErr: "panic: plain run", wantEvents: 1, wantLevel: errorreporter.LevelFatal, wantCommand: "boomrun"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := &recReporter{}
			errorreporter.SetReporter(rep)

			err := runConsole(t, tc.args, testCommands()...)

			switch {
			case tc.wantErrIs != nil:
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want %v", err, tc.wantErrIs)
				}
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			}

			events, flushes := rep.snapshot()
			if len(events) != tc.wantEvents {
				t.Fatalf("events = %d (%v), want %d", len(events), events, tc.wantEvents)
			}

			if flushes == 0 {
				t.Error("errorreporter.Flush not called")
			}

			if tc.wantEvents == 0 {
				return
			}

			ev := events[0]
			if ev.opts.Level != tc.wantLevel {
				t.Errorf("level = %q, want %q", ev.opts.Level, tc.wantLevel)
			}

			if tc.wantCommand != "" && ev.opts.Tags["command"] != tc.wantCommand {
				t.Errorf("command tag = %q, want %q", ev.opts.Tags["command"], tc.wantCommand)
			}

			if tc.wantLevel == errorreporter.LevelFatal {
				panicInfo, _ := ev.opts.Extra["panic"].(map[string]any)
				if stack, _ := panicInfo["stack"].(string); !strings.Contains(stack, "console_kernel_test.go") {
					t.Errorf("panic stack does not point to the command: %q", stack)
				}
			}

			if err != nil && !errorreporter.WasReported(err) {
				t.Error("returned error is not marked as reported")
			}
		})
	}
}

// TestConsoleKernel_SignalCancelsCommandContext SIGTERM отменяет контекст команды, команда завершается сама.
func TestConsoleKernel_SignalCancelsCommandContext(t *testing.T) {
	cases := []struct {
		name string
		sig  syscall.Signal
	}{
		{name: "SIGTERM", sig: syscall.SIGTERM},
		{name: "SIGINT", sig: syscall.SIGINT},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errorreporter.SetReporter(&recReporter{})

			started := make(chan struct{})
			canceled := false

			wait := &cobra.Command{Use: "wait", RunE: func(cmd *cobra.Command, _ []string) error {
				close(started)

				select {
				case <-cmd.Context().Done():
					canceled = true
				case <-time.After(5 * time.Second):
				}

				return nil
			}}

			go func() {
				<-started
				_ = syscall.Kill(os.Getpid(), tc.sig)
			}()

			if err := runConsole(t, []string{"wait"}, wait); err != nil {
				t.Fatalf("err = %v", err)
			}

			if !canceled {
				t.Fatal("command context was not canceled by the signal")
			}
		})
	}
}

// runConsole Запускает ConsoleKernel с аргументами args через App.
func runConsole(t *testing.T, args []string, cmds ...*cobra.Command) error {
	t.Helper()

	prevArgs := os.Args
	os.Args = append([]string{"console"}, args...)
	t.Cleanup(func() { os.Args = prevArgs })

	a := coreApp.NewApp().SetEnvFiles(t.TempDir() + "/absent.env")
	if err := a.RegisterAndInitKernels(NewConsoleKernel("console").AddCommand(cmds...)); err != nil {
		t.Fatalf("init: %v", err)
	}

	return a.RunAll()
}

// TestConsoleKernel_CorrelationIDPerRun Каждый запуск команды получает свой id корреляции в cmd.Context().
func TestConsoleKernel_CorrelationIDPerRun(t *testing.T) {
	errorreporter.SetReporter(&recReporter{})

	var ids []string

	cmd := func() *cobra.Command {
		return &cobra.Command{Use: "corr", RunE: func(cmd *cobra.Command, _ []string) error {
			ids = append(ids, sdkctx.CorrelationID(cmd.Context()))

			return nil
		}}
	}

	for range 2 {
		if err := runConsole(t, []string{"corr"}, cmd()); err != nil {
			t.Fatalf("err = %v", err)
		}
	}

	if len(ids) != 2 || ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Fatalf("correlation ids = %q, want two distinct non-empty ids", ids)
	}
}
