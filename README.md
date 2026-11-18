# gosdk-console-core

`gosdk-console-core` — тонкая обёртка над [cobra](https://github.com/spf13/cobra) для
консольных (CLI/ops) команд, подключаемых к приложению на `gosdk-core` как
обычный **kernel**.

Основная цель — дать единый способ собирать консольные команды проекта (миграции,
разовые скрипты, ops-утилиты) через тот же `RegisterAndInitKernels`, которым
собирается остальное приложение, без отдельного парсинга `os.Args` руками.

---

## 📦 Возможности

- 🧩 `ConsoleKernel` — реализует `KernelInterface` из `gosdk-core`
- 🌳 Регистрация групп команд через `AddCommand(...cobra.Command)`
- 🪄 Диспетчеризация `os.Args`, usage/help и валидация аргументов — всё из коробки от cobra
- 🚫 Не долгоживущий — команда выполняется и процесс завершается, без `WaitForShutdown()`
- 🛑 `SIGINT`/`SIGTERM` отменяют контекст команды (`cmd.Context()`) — команда завершается сама; второй сигнал
  завершает процесс
- 🚨 Ошибка кода команды (`RunE`, `PreRunE`/`PostRunE` и их `Persistent`-варианты) уходит в Sentry
  (`errorreporter`) с тегом команды; ошибка разбора аргументов (неизвестная команда или флаг, неверное число
  аргументов, не задан обязательный флаг) — не инцидент и в Sentry не уходит
- 💥 Паника в коде команды перехватывается: событие уровня `fatal` со стеком, `RunAll` возвращает ошибку
- 📤 `errorreporter.Flush` на любом пути выхода — последнее событие не теряется
- 🔗 Свой id корреляции на запуск (с v1.2.0): `sdkctx.CorrelationID(cmd.Context())` (gosdk-core) — новый, если в
  контексте приложения его нет; логи, исходящие запросы builder, публикации и события Sentry команды несут его

---

## 🚀 Установка

```bash
go get github.com/exgamer/gosdk-console-core
```

---

## 🧠 Концепция

В отличие от `HttpKernel`/`RabbitKernel`, `ConsoleKernel` **не поднимает
долгоживущий процесс**. `Start()` разбирает `os.Args` и выполняет ровно одну
сматченную cobra-команду, после чего возвращает управление. Поэтому
`ConsoleKernel` рассчитан на использование в отдельном бинарнике (своя
`cmd/console/main.go`), без HTTP/Rabbit kernels рядом — после `RunAll()`
вызывающий код проверяет ошибку и завершает процесс. `WaitForShutdown()` (ожидание
`SIGINT`/`SIGTERM`) для консольных команд не нужен и не вызывается.

Ядра запускаются по очереди в порядке регистрации, а `Start` у `ConsoleKernel` блокирует до конца
команды — регистрируйте его последним.

Команда, которая долго работает (импорт, обход очереди), проверяет `cmd.Context()`: по `SIGINT`/`SIGTERM`
он отменяется, и команда должна завершиться сама, вернув результат.

Это же касается и других kernel'ов, зарегистрированных вместе с `ConsoleKernel`
(например, `PostgresKernel`) — их `Stop()` вызывается через stop hook, который
срабатывает только внутри `WaitForShutdown()`. Если консольной команде нужно
закрыть соединение/ресурс, делайте это явно (`defer`), а не полагаясь на
kernel-lifecycle — см. пример ниже.

---

## 🔌 Пример использования

`cmd/console/main.go`:

```go
package main

import (
	"log"

	console "github.com/exgamer/gosdk-console-core/pkg/app"
	"github.com/exgamer/gosdk-core/pkg/app"
	"github.com/spf13/cobra"
)

func main() {
	consoleKernel := console.NewConsoleKernel("console").
		AddCommand(migrateCmd()).
		AddCommand(cacheCmd())

	a := app.NewApp()

	if err := a.RegisterAndInitKernels(consoleKernel); err != nil {
		log.Fatal(err)
	}

	if err := a.RunAll(); err != nil {
		log.Fatal(err)
	}
}

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "migrate", Short: "manage DB schema migrations"}

	cmd.AddCommand(&cobra.Command{
		Use:   "up",
		Short: "apply all pending migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			// ...
			return nil
		},
	})

	return cmd
}

func cacheCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cache-clear",
		Short: "clear application cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			// ...
			return nil
		},
	}
}
```

Запуск:

```bash
go run ./cmd/console migrate up
go run ./cmd/console cache-clear
go run ./cmd/console --help
```

Каждая новая консольная команда — это новая `*cobra.Command` и ещё один
`.AddCommand(...)` в `main.go`. Новый бинарник заводить не нужно — один
`cmd/console` обслуживает все команды проекта, разница только в аргументах
запуска.

Живой пример — `cmd/console` в
[`go-sdk-rest-template`](https://github.com/exgamer/go-sdk-rest-template)
(миграции через
[`gosdk-postgres-core.Migrator`](https://github.com/exgamer/gosdk-postgres-core/blob/main/MIGRATIONS.md)).

---

## 📌 Используется вместе с

- `gosdk-core`
- любой SDK-пакет, чьи операции (миграции, кэш и т.п.) нужно вызывать вручную
  из консоли — например `gosdk-postgres-core.Migrator`

---

## 📝 License

MIT
