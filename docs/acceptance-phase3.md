# Приёмка фазы 3

Аудит Q-01 выполнен 4 ноября 2026 года на исходниках
`7e088d0969994d261d5b39258fc8fa1735520fc9` (PR #31). Рабочие изменения
документации не меняют проверенную реализацию RPC.

**Итог: аудит завершён, фаза 3 пока не принята полностью.**
Обязательные возможности реализованы, существующие автоматические проверки
прошли, но найден дефект закрытия клиента во время подключения. Кроме того,
для RPC-5 и RPC-9 отсутствуют отдельные проверки, прямо указанные в исходных
критериях. Исправление обозначено Q-10, дополнение приёмки — Q-09;
оставшиеся работы перечислены в конце этого документа.
Удалённый статус GitHub CI в этом аудите подтвердить не удалось.

## Область и трактовка статусов

Проверены RPC-1…RPC-14, RPC-16, относящиеся к ним тесты, генерация, границы
модулей и ограничения `grpcbridge`. Желательное RPC-15 рассматривается отдельно.
Идентификаторы сохранены из исходных требований; этот документ содержит их
смысл и не требует локального `repo-requirements.md`.

- **Подтверждено:** реализация найдена, относящиеся к ней существующие проверки
  выполнены успешно. Это ограниченное свидетельство, а не доказательство
  отсутствия всех ошибок при любых сетевых условиях.
- **Частично:** поведение реализовано и проверено в указанных условиях,
  но исходный критерий приёмки целиком не проверяется.
- **Дефект:** описанный сценарий воспроизведён и противоречит контракту API.

Аудит не заменяет основные экспериментальные серии, полную интеграцию в Wisply
или проверку производительности на целевом окружении. runtime-код и тесты
библиотеки в Q-01 не изменялись.

## Матрица обязательных требований

Общие RPC-тесты через `forEachTransport` используют `mem` и настоящий QUIC;
детерминированные сценарии отдельно выполняются в `testing/synctest`.

| ID | Требование и реализация | Проверка | Статус и границы |
| --- | --- | --- | --- |
| RPC-1 | Один RPC — один клиентский bidi-поток; соединение переиспользуется. [ClientConn](../rpc/clientconn.go), [Server](../rpc/server.go) | `TestConformance/ConcurrentStreams`: 100 потоков одного соединения; [TestConcurrentCalls](../bench/common/common_test.go): 100 RPC; `TestConnect`: число handshake; [TestStreamSlotsReleased](../rpc/limits_test.go): последовательные вызовы при лимите 2 | Подтверждено составной проверкой. Отдельного RPC-теста, одновременно считающего N вызовов, N потоков и ровно один Dial, нет |
| RPC-2 | Unary, server-stream, client-stream, bidi; типизированные интерфейсы в [stream.go](../rpc/stream.go) | [TestUnary](../rpc/unary_test.go), [TestServerStreaming / TestClientStreaming](../rpc/streaming_test.go), [TestBidiEcho](../rpc/bidi_test.go), дифференциальные сценарии всех четырёх видов | Подтверждено на `mem` и QUIC |
| RPC-3 | Кадры, метаданные, завершающий статус; строгие varint и protobuf. [frame](../rpc/internal/frame/frame.go), [протокол](protocol.md) | `TestRoundTrip`, `TestNonMinimalVarint`, `TestTooLarge`, тесты декодеров, три fuzz-цели кадров | Подтверждено; размеры проверяются до выделения входного payload |
| RPC-4 | Статусы 0…16 и один конечный статус. [clientstream.go](../rpc/clientstream.go), [serverstream.go](../rpc/serverstream.go) | [TestClientRecvMatchesModel / TestServerRecvMatchesModel / TestReadRequestHeadersMatchesModel](../rpc/internal/call/grammar_test.go), `TestCardinalityViolations`, `TestDifferential` со всеми кодами | Подтверждено PBT грамматики и RPC-сценариями. PBT не является моделью всех конкурентных событий жизненного цикла |
| RPC-5 | Передача остатка дедлайна в микросекундах после ожидания соединения и слота; сервер создаёт свой `context`. [clientconn.go](../rpc/clientconn.go), [server.go](../rpc/server.go) | [TestDeadlinePropagation / TestDeadlineExceeded / TestDeadlineAndCancelRealTime](../rpc/deadline_test.go), `TestEarlyConcurrentCallsAndDeadline` | Частично: остаток и отмена проверены; отдельного теста намеренно расходящихся часов клиента и сервера нет |
| RPC-6 | Отмена контекста сбрасывает направления и останавливает обработчик | [TestCancellationStopsHandler / TestCancellationUnblocksServerRecv / TestCancellationUnblocksServerSend / TestManyCallsLeaveNothingBehind](../rpc/deadline_test.go), реальные сетевые сценарии | Подтверждено. `synctest` проверяет завершение горутин внутри bubble, а не произвольные утечки вне неё. Отдельный дефект `ClientConn.Close` описан ниже |
| RPC-7 | FIN закрывает только отправку, сервер продолжает отвечать | [TestBidiHalfClose](../rpc/bidi_test.go), [TestClientStreaming](../rpc/streaming_test.go), общий `TestConformance/HalfCloseEcho` | Подтверждено на обоих транспортах |
| RPC-8 | Начальные метаданные и trailers; бинарные и повторные значения | [TestMetadata / TestHeaderCanBeSentOnce](../rpc/metadata_test.go), `TestBidiHeaderWhileReceiving`, дифференциальные сценарии | Подтверждено; статусы с details проверены отдельно |
| RPC-9 | Блокировка отправителя окнами, целостность данных и ограниченная буферизация. [QUIC](../rpc/transport/quic/quic.go), [mem](../rpc/transport/mem/mem.go) | [TestConformance/Backpressure](../rpc/internal/transporttest/transporttest.go): запись 10 МиБ без чтения блокируется, после чтения байты совпадают; `TestCancellationUnblocksServerSend` | Частично: блокировка и данные проверены; нет отдельного длительного RPC-теста медленного читателя с измерением/бюджетом памяти |
| RPC-10 | Клиентские и серверные unary/stream interceptors, цепочки и обёртки | [TestUnaryInterceptors / TestStreamInterceptors / TestInterceptorsAbort / TestStreamInterceptorsWrap / TestInterceptorsSeeCancellation](../rpc/interceptor_test.go), `TestSmokeInterceptors` | Подтверждено для нативного API. У `grpcbridge` stream interceptors отсутствуют |
| RPC-11 | Общий контракт транспорта; независимые `mem` и QUIC | [transport.go](../rpc/transport/transport.go), общий [TestConformance](../rpc/internal/transporttest/transporttest.go), RPC-фабрики обоих транспортов | Подтверждено; у `mem` нет общего окна соединения, поэтому он не заменяет сетевые тесты |
| RPC-12 | Keepalive, обнаружение потери, новое соединение; безопасный повтор открытия до отправки | [TestRedialAfterConnectionLoss / TestRetryOnClosedConnection / TestRetryOnce / TestServerRestart / TestDeadServer](../rpc/clientconn_test.go), [TestKeepAlive](../rpc/transport/quic/quic_test.go), `TestStatelessReset` | Подтверждено указанными тестами. Уже отправленные вызовы произвольно не повторяются; рестарт без reset key обнаруживается по idle timeout |
| RPC-13 | Один `.proto` собирается для grpc-go и RPC; все streaming-индексы корректны | `make proto-check`, [TestGolden / TestPackageSuffixOption / TestStreamIndices](../rpc/cmd/protoc-gen-rpcquic/generate_test.go), smoke и differential tests | Подтверждено; старый marker 1 отдельно проверен на компиляцию |
| RPC-14 | TLS-билеты, opt-in 0-RTT, отметка идемпотентности на обеих сторонах, безопасный отказ | [TestEarlyIdempotencyGates](../rpc/early_test.go), [TestEarlyCalls / TestEarlyRejection / TestEarlyAuthenticationFailure / TestEarlyDisabled](../rpc/transport/quic/early_test.go), [TestGeneratedIdempotency](../internal/gen/difftest/v1/difftestv1quic/early_test.go), `BenchmarkEarlyReconnect` | Подтверждено тестами и отдельным замером ниже; `grpcbridge` не включает ранние RPC |
| RPC-16 | GOAWAY, REFUSED, завершение текущих RPC и уход новых на другое соединение | [TestGracefulStop / TestGracefulStopTimeout / TestGracefulStopWaitsForHandlers / TestGracefulStopRefuses / TestGracefulStopRealTime](../rpc/gracefulstop_test.go), [TestGracefulStop](../grpcbridge/gracefulstop_test.go) | Подтверждено; нативный тайм-аут задаётся контекстом, мост имеет свой ограниченный тайм-аут |

Для RPC-1 перечислены отдельные свидетельства, а не заявлен отсутствующий тест
с единым счётчиком. Для RPC-5 относительная длительность по протоколу исключает
передачу абсолютного времени, но это не подменяет отсутствующую проверку clock skew.

## Тестирование и дополнительные ограничения

| ID | Результат аудита |
| --- | --- |
| TST-1 | Пакет `rpc` покрыт на 92,3% statements при `go test -count=1 -coverprofile=… ./...` из `rpc/`, выше целевых 80%. Это покрытие конкретного пакета, не среднее всех трёх модулей. В том же запуске генератор — 97,5%, `transport/mem` — 95,6%, `transport/quic` — 93,8%; внутренние пакеты имеют другие значения |
| TST-2 | Все пять fuzz-целей прошли с бюджетами CI: три декодера по 1 000 000 исполнений, клиент и сервер по 100 000 |
| TST-3 | PBT проверяет грамматику запроса/ответа, конечный статус и некорректные последовательности; `TestNoMessageAfterCancel` отдельно проверяет уже буферизованный ответ в `synctest`. Единой случайной модели кадры + отмена + закрытие нет |
| TST-4 | Локальные тесты всех трёх модулей с `-race` прошли. Это не свидетельство удалённого статуса GitHub CI |
| TST-5 | `TestDifferential` прошёл на семи стеках: grpc-h2, rpc-quic/mem, bridge-quic/mem, connect-h2, grpc-h3. Для connect-go известные различия некорректных вызовов проверяются явно, а часть библиотечных сообщений сравнивается только по коду; полное побитовое совпадение всех ошибок не заявляется |
| TST-6 | Собственные дедлайны/отмена RPC и большая часть lifecycle-сценариев `mem` используют `synctest`; общая транспортная conformance suite для `mem` всё ещё использует реальные `time.After(blockedFor)`. Полного соответствия буквальному критерию отсутствия реальных пауз нет |
| TST-7 | Есть `TestRunOutage`, закрытие соединений, аварийный рестарт и проверка статусов; тесты стенда выполнены с запретом пропуска. Обёртка `PacketConn` добавляет задержку и удерживает пакеты, но не внедряет потери; отдельной автоматической RPC-проверки случайных потерь через неё нет. Проверка пробников потерь в `testbed/selfcheck` не заменяет такой RPC-тест |
| TST-8 | Проверки синтетического генератора, включая open-loop/closed-loop, прошли в составе корневого модуля |

### Желательное RPC-15

Входящий MESSAGE ограничен 4 МиБ, HEADERS/TRAILERS — 64 КиБ. Тип
`frame.Limits` настраивается внутри `internal/frame`, но RPC-клиент и сервер
создают reader через `frame.DefaultLimits()`; публичной настройки этих размеров
пока нет. Это часть Q-02, а не выполненная функция.

Лимит потоков и окна QUIC можно задать через `Dialer.QUICConfig` и конфигурацию
listener; транспорт клонирует их и принудительно задаёт разрешённые направления.
У `mem` доступны `StreamWindow` и `MaxIncomingStreams`. Есть нативные interceptors,
но отдельного публичного API RPC-хуков метрик с примером счётчиков нет.
RPC-15 выполнено частично и само по себе не является воротами фазы 3.

## Найденный дефект: закрытие во время Dial

**Q-10, воспроизведено.** Контракт `ClientConn.Close` обещает `Canceled` активным
вызовам. Пока `getConn` ждёт `Dial`, соединение ещё не входит в `cc.conns`.
`Close` отмечает клиент закрытым и закрывает имеющиеся соединения, но не отменяет
контекст Dial и не будит ожидающих его завершения. Ошибка вызывающего контекста
затем возвращается раньше проверки `closed`.

При Dialer, корректно ожидающем отмены переданного контекста:

```text
Close returned: <nil>
RPC still blocked 50ms after Close
RPC finished at caller deadline: DeadlineExceeded
```

Это поведенческий дефект, не просто отсутствующий тест. Контекст с дедлайном
250 мс ограничивает воспроизведение; без него ожидание зависит от Dialer.
`TestClientConnClose` проверяет уже установленное соединение и этот случай
не охватывает. Ожидание другого вызова на `cc.dialing` также нужно проверить
при исправлении, но отдельный результат для него здесь не заявляется.

Воспроизведение из корня репозитория: сохранить код ниже в `/tmp/qrpc-close.go`
и выполнить `go run /tmp/qrpc-close.go`. Нужны зависимости текущего корневого
модуля с локальным `replace` на `rpc/`; работающий сервер не требуется.

```go
package main

import (
    "context"
    "fmt"
    "time"

    "github.com/VladimirKhmelev/quic-rpc-go/rpc"
    "github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
    "github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
    "google.golang.org/protobuf/types/known/wrapperspb"
)

type blockedDialer struct{ started chan struct{} }

func (d blockedDialer) Dial(ctx context.Context) (transport.Conn, error) {
    close(d.started)
    <-ctx.Done()
    return nil, ctx.Err()
}

func main() {
    d := blockedDialer{started: make(chan struct{})}
    c := rpc.NewClient(d)
    ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
    defer cancel()
    done := make(chan error, 1)
    go func() {
        done <- c.Invoke(ctx, "/test.v1.Test/Call",
            wrapperspb.String("x"), new(wrapperspb.StringValue))
    }()
    <-d.started
    fmt.Printf("Close returned: %v\n", c.Close())
    select {
    case err := <-done:
        fmt.Printf("RPC finished after Close: %s\n", status.Code(err))
    case <-time.After(50 * time.Millisecond):
        fmt.Println("RPC still blocked 50ms after Close")
        err := <-done
        fmt.Printf("RPC finished at caller deadline: %s\n", status.Code(err))
    }
}
```

## Генератор и независимость модулей

- Корень требует Go 1.26.8; `rpc/` и `grpcbridge/` — Go 1.26.3. Все проверки
  этого аудита выполнены Go 1.26.8, отдельно минимальная версия 1.26.3 не запускалась.
- `rpc/` собирается и тестируется из своего модуля. `go list -deps .` из `rpc/`
  не содержит корневой модуль, `grpcbridge`, grpc-go или connect-go.
- Генератор в текущих исходниках печатает v0.2.0 и marker
  `SupportPackageIsVersion2`. Marker 1 сохранён. Это версии совместимости
  сгенерированного API, они не равны ALPN `qrpc/2`.
- Отдельно собран `bench.rpcquic.go` из локального тега `rpc/v0.2.0` с marker 1
  против текущего RPC через временный Go-модуль. Компиляция прошла; это не сетевой
  тест старого клиента против нового сервера.
- Новый сгенерированный код с marker 2 требует соответствующий runtime.
  В локальном теге `rpc/v0.2.0` marker 2 отсутствует, а версия генератора там v0.1.0.
  Версия модуля, строка версии генератора и marker — разные величины.
  Перед следующим релизом нужно согласовать их и обновить инструкции установки;
  аудит не создаёт тегов и не проверяет опубликованное разрешение `@latest`.
- `make proto-check` проверяет оба root `.proto`, служебную схему RPC, fixture
  и результаты генераторов; существующие golden и compile/smoke tests прошли.
  Сгенерированные файлы вручную не менялись.

## Границы grpcbridge

Подтверждены `grpc.ClientConnInterface` и `grpc.ServiceRegistrar`, четыре вида
методов через generated code, метаданные, статусы с неизвестными `Any`,
unary interceptors, ограниченные stats handlers и graceful shutdown.
Основания: `TestMetadata`, `TestStatuses`, `TestInterceptors`, `TestCallOptions`,
`TestHealth`, `TestStatsHandlers`, `TestStreamStats`, `TestOtelgrpc`,
`TestGracefulStop` и дифференциальные сценарии bridge-quic/mem.

- Поддерживаются `grpc.Header`, `grpc.Trailer`, `grpc.StaticMethod` и пустая
  `grpc.EmptyCallOption`. Другие call options, включая `WaitForReady` и
  `MaxCallRecvMsgSize`, возвращают `Internal`.
- Первый unary interceptor — внешний; клиентский получает `nil *grpc.ClientConn`.
- Stats handlers передают `Begin`, клиентский `OutHeader` / серверный `InHeader`
  и единственный `End`; trace context клиента попадает в исходящие метаданные.
- Peer API, payload/connection stats, события полученных headers/trailers,
  stream interceptors и compression не реализованы. Типы health поддерживаются,
  но полная совместимость всей инфраструктуры grpc-go из этого не следует.
- Методы моста не помечены идемпотентными, call option включения 0-RTT отсутствует.
  Даже ранний транспорт сам по себе не разрешает раннее выполнение этих RPC.

Дополнительные потребности Wisply выясняются в M-01; ради абстрактной полной
совместимости мост в рамках аудита не расширяется.

## Выполненные команды

| Проверка | Результат 4 ноября 2026 года |
| --- | --- |
| `make check` | Успех: fmt, tidy-diff, генерация и fixture, vet, lint и тесты с race detector всех трёх модулей |
| `QRPC_TESTBED_REQUIRED=1 make test TESTFLAGS=-cpu=1` | Успех во всех трёх модулях; недоступность стенда не могла превратиться в тихий skip |
| `make plots-test` | 50 тестов прошли; закреплённые зависимости установлены в `.venv` |
| `make fuzz` | Все пять целей прошли с исходными бюджетами Makefile/CI; паник и зависаний не обнаружено |
| `cd rpc && go test -count=1 -coverprofile=/tmp/quic-rpc-go-rpc.cover ./...` | Успех; пакет `rpc` — 92,3% statements |
| `cd rpc && go list -deps .` | Границы runtime-зависимостей подтверждены |
| Старый generated client + текущий runtime | `go test -mod=mod ./...` во временном модуле: компиляция marker 1 успешна; тестовых сценариев в этом модуле нет |
| `go run /tmp/qrpc-close.go` | Воспроизведён дефект Q-10; это наблюдение ошибки, не пройденный regression test |
| `gh pr checks 31`, публичный API checks и веб-страница checks | Удалённый CI не подтверждён: gh без авторизации, API вернул 403, веб-страница недоступна. Локальные проверки перечислены отдельно |

Среда: Linux amd64, Intel Core i5-12450H, Go 1.26.8, quic-go v0.63.0,
grpc-go v1.84.0, connect-go v2.0.2, protobuf v1.36.12, protoc 3.21.12,
golangci-lint 2.12.2, Python 3.12.3. Полные локальные журналы находятся в
`out/acceptance-phase3-20261104/`; этот каталог игнорируется Git и не является
опубликованным архивом результатов. Основные наблюдения сохранены в этом документе.

Первый запуск Python-тестов в песочнице не смог скачать зависимости; повтор
с сетевым доступом завершился успешно. Ошибки доступа к GitHub не означают
красный CI, но и не позволяют утверждать, что CI зелёный.

### Приёмочный замер RPC-14

```sh
cd rpc
go test ./transport/quic -run '^$' -bench '^BenchmarkEarlyReconnect$' -benchtime=20x -count=3
```

В обоих режимах TLS возобновляется по билету. `PacketConn` независимо добавляет
серверным UDP-пакетам 50 мс, то есть примерно один RTT; потерь нет. Интервал
`first_reply_ms` включает подключение и первый ответ, а не закрытие соединения.
Каждый повтор проверяет `TLS.DidResume` и ожидаемое `Used0RTT`.

| Режим | Три средних `first_reply_ms`, мс | Три `RTT/reply` |
| --- | --- | --- |
| Без 0-RTT | 105,2 / 105,6 / 105,4 | 2,105 / 2,111 / 2,107 |
| С 0-RTT | 54,31 / 54,18 / 54,59 | 1,086 / 1,084 / 1,092 |

Каждое значение — среднее 20 подключений внутри одной серии benchmark.
Разница медиан трёх средних — около 51,1 мс при добавленных 50 мс RTT:
приёмочный эффект RPC-14 подтверждён. Это не доверительный интервал,
не результат основной серии и не сравнение производительности с gRPC.

## Что не проверялось и что делать дальше

- Исправить Q-10 с regression tests отмены самого Dial и ожидающих его вызовов.
- В Q-09 закрыть точные критерии RPC-5/RPC-9, отметить оставшиеся ограничения
  тестов и повторить относящиеся к изменению проверки.
- Для объявления фазы 3 завершённой проверить зелёный CI соответствующего
  исправленного коммита и обновить этот документ, README и бэклог.
- Полная самопроверка всех профилей `make testbed-check`, основные серии,
  замеры на двух хостах, минимальный Go 1.26.3, релиз и полный охват Wisply
  в этом аудите не выполнялись. Они не заменяются короткими тестами и benchmark.
