# quic-rpc-go

RPC для Go поверх нативных потоков QUIC ([quic-go](https://github.com/quic-go/quic-go)): один вызов — один двунаправленный поток, без HTTP. После реализации библиотеки — сравнение с gRPC в плохой сети по воспроизводимой методике.

> Проект в разработке: фазы 1 и 2 завершены, [итоги пилота](docs/pilot.md). API может меняться.

## Что планируется

- **Библиотека `rpc/`.** Сервисы описываются в `.proto`, клиенты и серверы генерирует плагин protoc. Четыре шаблона вызова (unary, server-stream, client-stream, bidi), статусы, дедлайны, отмена, метаданные, interceptors, keepalive, 0-RTT. Транспорты `quic` и `mem` (в памяти процесса, для тестов и замера накладных расходов).
- **Стенд.** Эмуляция плохой сети на Linux через network namespace и `tc netem`, без root: потери, задержка, джиттер, переупорядочивание, ограничение полосы.
- **Сравнение.** gRPC по HTTP/2 (одно соединение и пул), gRPC по HTTP/3, connect-go по HTTP/2. Open-loop нагрузка, p50/p95/p99 с доверительными интервалами, CPU, время установки соединения.
- **Отчёт.** Гипотезы и вердикты, графики, ограничения. Все графики строятся из сырых данных одной командой.

## Фазы

1. Основа: unary и bidi, транспорты `quic` и `mem`, стенд, генератор нагрузки, эталон gRPC/HTTP/2. **Готово.**
2. Пилот на малой матрице. **Готово:** [итоги](docs/pilot.md), [гипотезы](docs/hypotheses.md).
3. Полная библиотека и эталоны.
4. Основные эксперименты.
5. Анализ и отчёт.

## Интеграция в мессенджер

Параллельно с фазами библиотека встраивается в [messenger-on-go](https://github.com/VladimirKhmelev/messenger-on-go) — мессенджер Wisply из пяти сервисов, которые сейчас общаются по gRPC. План:

- прослойка [`grpcbridge`](grpcbridge), которая подставляет quic-rpc-go под код, сгенерированный для grpc-go (`grpc.ClientConnInterface` и `grpc.ServiceRegistrar`), чтобы сервисы работали по gRPC и по QUIC одновременно и транспорт выбирался настройкой;
- трассировка OpenTelemetry с теми же спанами, что у gRPC;
- сравнение транспортов на реальных серверах с метриками в Grafana.

Это отдельная работа: в методику сравнения из фаз 1–5 она не входит.

## Установка

Нужен Go 1.26.3 или новее. Модули версионируются отдельно, версии v0.x: до конца фазы 3 API может меняться.

```sh
go get github.com/VladimirKhmelev/quic-rpc-go/rpc@v0.2.0          # библиотека
go get github.com/VladimirKhmelev/quic-rpc-go/grpcbridge@v0.2.0   # код grpc-go поверх quic-rpc-go
```

Версии v0.2 говорят по протоколу версии 2 (`qrpc/2`, [описание](docs/protocol.md)) и не соединяются с клиентами и серверами v0.1: обновлять нужно обе стороны сразу.

`grpcbridge` подставляет quic-rpc-go под клиенты и серверы, сгенерированные protoc-gen-go-grpc; пример — в документации пакета.

## Генерация кода

Клиенты и серверы генерирует плагин `protoc-gen-rpcquic`. Код ложится в подпакет `<пакет>quic` (например, `chatv1quic`) с теми же именами, что у protoc-gen-go-grpc, поэтому оба генератора можно запускать на одном `.proto`.

```sh
go install github.com/VladimirKhmelev/quic-rpc-go/rpc/cmd/protoc-gen-rpcquic@latest
protoc --go_out=. --go_opt=paths=source_relative \
       --rpcquic_out=. --rpcquic_opt=paths=source_relative \
       chat/v1/chat.proto
```

Пока поддерживаются unary- и bidi-методы.

## Документация

- [Протокол](docs/protocol.md): кадры, порядок кадров в вызове, коды ошибок, соответствие gRPC.
- [Стенд](docs/testbed.md): топология, профили сети, как устроены воздействия, самопроверка.
- [Методика](docs/methodology.md): выравнивание TCP и QUIC, нагрузка, окружение серий, прогон, серия, анализ.
- [Гипотезы](docs/hypotheses.md): гипотезы, пороги и правила вердиктов, зафиксированные до основной серии.
- [Пилот](docs/pilot.md): проверка методики на малой матрице и первые результаты.
- [Схема данных](data/schema.md): формат сырых данных прогонов и серий, таблиц анализа и пилотных данных.

## Разработка

Нужны Go 1.26 и golangci-lint 2.x, для анализа — Python 3.12. Репозиторий состоит из трёх модулей: библиотека `rpc/`, прослойка `grpcbridge/` для кода grpc-go и корневой модуль со стендом и эталонами.

```sh
make check   # gofmt, go mod tidy, сгенерированный код, go vet, линтер, тесты с -race
make fuzz    # fuzz-тесты: по 1 млн запусков на цель, сквозные цели вызовов — по 100 тыс.
make proto   # перегенерировать код из .proto (нужен protoc 3.21.12)
make testbed-shell PROFILE=interregion   # шелл внутри стенда с профилем сети
make testbed-check   # самопроверка стенда на всех профилях (нужен iperf3)
sudo make host-setup  # настроить машину для серии измерений; sudo make host-restore — вернуть
make bench-run TRANSPORT=quic PROFILE=interregion LOAD=small   # один прогон в стенде, результат в out/
make bench-pilot     # пилотная серия на настроенной машине, около 4 часов, результат в out/
make bench-series MATRIX=my.yaml SERIESFLAGS=-dry-run   # план серии по своей матрице
make plots SERIES=out/…   # таблицы, рисунки и сводка серии в out/…/analysis
make plots ANALYSIS=data/pilot   # пересобрать данные пилота (calls.parquet — из релиза)
make plots-test           # тесты анализа
```

Тесты стенда создают network namespace без root. Ubuntu 24.04 по умолчанию это запрещает, как разрешить — в [описании стенда](docs/testbed.md#запуск). Без этого тесты стенда пропускаются.

## Лицензия

MIT, см. [LICENSE](LICENSE).
