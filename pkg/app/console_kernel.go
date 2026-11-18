// Package app — ConsoleKernel: тонкая обёртка над cobra для консольных
// команд, подключаемая как обычный gosdk-core KernelInterface.
//
// В отличие от HttpKernel/RabbitKernel, ConsoleKernel не поднимает
// долгоживущий процесс — Start выполняет ровно одну команду, сматченную по
// os.Args, и возвращает её результат. Поэтому ConsoleKernel рассчитан на
// использование в отдельном бинарнике (своя cmd/<name>/main.go), без
// HTTP/Rabbit kernels рядом: после RunAll() вызывающий код проверяет
// возвращённую ошибку и завершает процесс — WaitForShutdown() (ожидание
// SIGINT/SIGTERM) для консольных команд не нужен, команда сама по себе
// одноразовая.
package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/exgamer/gosdk-core/pkg/app"
	sdkctx "github.com/exgamer/gosdk-core/pkg/context"
	"github.com/exgamer/gosdk-core/pkg/errorreporter"
	"github.com/spf13/cobra"
)

const ConsoleKernelName = "console"

// flushTimeout Предел ожидания отправки событий в конце команды.
const flushTimeout = 2 * time.Second

type ConsoleKernel struct {
	root *cobra.Command

	// tracked — команды обёрнуты; commandFailed — ошибку вернул код команды, а не разбор аргументов
	tracked       bool
	commandFailed bool
}

// NewConsoleKernel создаёт kernel с корневой cobra-командой. use — имя
// бинарника, показывается в usage/--help (например "migrate" для
// go run ./cmd/migrate).
func NewConsoleKernel(use string) *ConsoleKernel {
	return &ConsoleKernel{
		root: &cobra.Command{
			Use:           use,
			SilenceUsage:  true,
			SilenceErrors: true,
		},
	}
}

// AddCommand регистрирует одну или несколько cobra-команд в корне.
func (k *ConsoleKernel) AddCommand(cmds ...*cobra.Command) *ConsoleKernel {
	k.root.AddCommand(cmds...)

	return k
}

func (k *ConsoleKernel) Name() string {
	return ConsoleKernelName
}

// Init — no-op. Конфиг/подключения поднимают сами команды (в своих RunE)
// или другие kernels, зарегистрированные тем же RegisterAndInitKernels; к
// моменту Start (после Init всех kernels) окружение уже загружено через
// app.ensureInit, так что RunE может полагаться на переменные окружения без
// отдельного config.LoadEnv().
func (k *ConsoleKernel) Init(a *app.App) error {
	return nil
}

// Start разбирает os.Args[1:] и выполняет сматченную cobra-команду.
//
//   - Контекст команды (cmd.Context()) отменяется по SIGINT/SIGTERM — команда завершает работу сама;
//     после первого сигнала обработка сигналов возвращается к умолчанию, второй завершает процесс.
//   - Ошибка кода команды (RunE и хуки PreRunE/PostRunE и их Persistent-варианты) репортится в Sentry с
//     тегом команды и возвращается отсюда же — RunAll в gosdk-core пробрасывает её вызывающей стороне.
//     Если команда уже сама вызвала errorreporter.Capture/CaptureError, повторной отправки не будет —
//     дедуп встроен в errorreporter.Capture (WasReported).
//   - Ошибка разбора аргументов (неизвестная команда или флаг, неверное число аргументов, не задан
//     обязательный флаг) — не инцидент: возвращается без отправки в Sentry.
//   - Паника в коде команды перехватывается: событие уровня fatal со стеком, ошибка возвращается.
//   - Контекст команды (cmd.Context()) несёт id корреляции запуска (sdkctx.CorrelationID, gosdk-core): новый, если
//     в контексте приложения его нет.
//   - На любом пути выхода — errorreporter.Flush: репортеры вроде sentry-go отправляют события асинхронно,
//     а одноразовая команда после RunAll обычно сразу завершает процесс.
func (k *ConsoleKernel) Start(a *app.App) error {
	ctx, stop := signal.NotifyContext(a.GetContext(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// id корреляции запуска: cmd.Context() команды, логи и события трекера несут один id
	ctx, _ = sdkctx.EnsureCorrelationID(ctx)

	// после первого сигнала (или конца команды) — сигналы по умолчанию: второй Ctrl+C завершает процесс
	go func() {
		<-ctx.Done()
		stop()
	}()

	defer errorreporter.Flush(flushTimeout)

	args := os.Args[1:]
	k.root.SetArgs(args)
	k.trackCommandErrors()

	cmd, err := k.execute(ctx, args)
	if err == nil {
		return nil
	}

	if !k.commandFailed {
		return err
	}

	return errorreporter.CaptureError(ctx, err, map[string]string{
		"component": "console",
		"command":   commandName(cmd),
	})
}

// execute Выполняет команду; паника кода команды — событие fatal и ошибка.
func (k *ConsoleKernel) execute(ctx context.Context, args []string) (cmd *cobra.Command, err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}

		if found, _, findErr := k.root.Find(args); findErr == nil {
			cmd = found
		}

		k.commandFailed = true
		err = errorreporter.Capture(ctx, fmt.Errorf("console command %s panic: %v", commandName(cmd), r), errorreporter.Options{
			Level: errorreporter.LevelFatal,
			Tags: map[string]string{
				"component": "console",
				"command":   commandName(cmd),
			},
			Extra: map[string]any{
				"panic": map[string]any{"value": fmt.Sprint(r), "stack": string(debug.Stack())},
			},
		})
	}()

	k.commandFailed = false

	return k.root.ExecuteContextC(ctx)
}

// trackCommandErrors Оборачивает RunE и хуки команд один раз: ошибка, которую вернул код команды,
// отличается от ошибки разбора аргументов cobra.
func (k *ConsoleKernel) trackCommandErrors() {
	if k.tracked {
		return
	}

	k.tracked = true
	k.wrapCommand(k.root)
}

func (k *ConsoleKernel) wrapCommand(c *cobra.Command) {
	c.PersistentPreRunE = k.wrapRunE(c.PersistentPreRunE)
	c.PreRunE = k.wrapRunE(c.PreRunE)
	c.RunE = k.wrapRunE(c.RunE)
	c.PostRunE = k.wrapRunE(c.PostRunE)
	c.PersistentPostRunE = k.wrapRunE(c.PersistentPostRunE)

	for _, sub := range c.Commands() {
		k.wrapCommand(sub)
	}
}

func (k *ConsoleKernel) wrapRunE(f func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	if f == nil {
		return nil
	}

	return func(cmd *cobra.Command, args []string) error {
		err := f(cmd, args)
		if err != nil {
			k.commandFailed = true
		}

		return err
	}
}

func commandName(cmd *cobra.Command) string {
	if cmd == nil {
		return "unknown"
	}

	return cmd.Name()
}

func (k *ConsoleKernel) Stop(ctx context.Context) error {
	return nil
}
