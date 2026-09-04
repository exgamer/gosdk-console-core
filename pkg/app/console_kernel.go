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
	"os"
	"time"

	"github.com/exgamer/gosdk-core/pkg/app"
	"github.com/exgamer/gosdk-core/pkg/errorreporter"
	"github.com/spf13/cobra"
)

const ConsoleKernelName = "console"

type ConsoleKernel struct {
	root *cobra.Command
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
// Ошибка выполнения команды репортится в Sentry (тегом - какая именно
// команда упала) и возвращается отсюда же - RunAll в gosdk-core пробрасывает
// её вызывающей стороне как ошибку RunAll/RunKernel. Если команда уже сама
// вызвала errorreporter.Capture/CaptureError внутри своего RunE, повторной
// отправки не будет - дедуп встроен в errorreporter.Capture (WasReported).
//
// После капчура - errorreporter.Flush: репортеры вроде sentry-go отправляют
// события асинхронно в фоновой горутине, а ConsoleKernel.Stop() (где обычно
// был бы шанс дождаться) для одноразовых команд никогда не вызывается - нет
// graceful shutdown, вызывающий код (cmd/<name>/main.go) обычно сразу делает
// log.Fatal(err). Без явного Flush здесь последнее событие рискует не уйти.
func (k *ConsoleKernel) Start(a *app.App) error {
	k.root.SetArgs(os.Args[1:])

	cmd, err := k.root.ExecuteContextC(a.GetContext())
	if err != nil {
		commandName := "unknown"
		if cmd != nil {
			commandName = cmd.Name()
		}

		reported := errorreporter.CaptureError(a.GetContext(), err, map[string]string{
			"component": "console",
			"command":   commandName,
		})

		errorreporter.Flush(2 * time.Second)

		return reported
	}

	return nil
}

func (k *ConsoleKernel) Stop(ctx context.Context) error {
	return nil
}
