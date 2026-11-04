# Приёмка фазы 3

Проверены исходники этого изменения поверх `8bff066` (PR #32), 4 ноября
2026 года. Первичный аудит Q-01 выявил пробелы; Q-09/Q-10 устраняют их кодом
и regression-тестами.

**Реализация и локальная приёмка фазы 3 готовы.** Исправлено закрытие клиента
во время подключения; добавлены прямые проверки потоков, расходящихся часов,
backpressure с бюджетом памяти и восстановления под потерями. Локальный набор
проверок CI прошёл. Удалённый CI нового изменения должен быть подтверждён
после создания коммита и push пользователем; до этого формальные ворота CI
не считаются пройденными.

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

Приёмка не заменяет основные экспериментальные серии, полную интеграцию в Wisply
или проверку производительности на целевом окружении.

## Матрица обязательных требований

Общие RPC-тесты через `forEachTransport` используют `mem` и настоящий QUIC;
детерминированные сценарии отдельно выполняются в `testing/synctest`.

| ID | Требование и реализация | Проверка | Статус и границы |
| --- | --- | --- | --- |
| RPC-1 | Один RPC — один клиентский bidi-поток; соединение переиспользуется. [ClientConn](../rpc/clientconn.go), [Server](../rpc/server.go) | [TestConcurrentRPCsUseOneConnection](../rpc/acceptance_test.go) удерживает 32 обработчика одновременно, проверяет один Dial и 32 потока, затем успешный 33-й RPC без переподключения; `TestStreamSlotsReleased` | Подтверждено непосредственно на `mem` и QUIC |
| RPC-2 | Unary, server-stream, client-stream, bidi; типизированные интерфейсы в [stream.go](../rpc/stream.go) | [TestUnary](../rpc/unary_test.go), [TestServerStreaming / TestClientStreaming](../rpc/streaming_test.go), [TestBidiEcho](../rpc/bidi_test.go), дифференциальные сценарии всех четырёх видов | Подтверждено на `mem` и QUIC |
| RPC-3 | Кадры, метаданные, завершающий статус; строгие varint и protobuf. [frame](../rpc/internal/frame/frame.go), [протокол](protocol.md) | `TestRoundTrip`, `TestNonMinimalVarint`, `TestTooLarge`, тесты декодеров, три fuzz-цели кадров | Подтверждено; размеры проверяются до выделения входного payload |
| RPC-4 | Статусы 0…16 и один конечный статус. [clientstream.go](../rpc/clientstream.go), [serverstream.go](../rpc/serverstream.go) | [TestClientRecvMatchesModel / TestServerRecvMatchesModel / TestReadRequestHeadersMatchesModel](../rpc/internal/call/grammar_test.go), `TestCardinalityViolations`, `TestDifferential` со всеми кодами | Подтверждено PBT грамматики и RPC-сценариями. PBT не является моделью всех конкурентных событий жизненного цикла |
| RPC-5 | Остаток дедлайна передаётся в микросекундах; сервер создаёт свой контекст. [clientconn.go](../rpc/clientconn.go), [server.go](../rpc/server.go) | [TestDeadlineWithClockSkew](../rpc/acceptance_test.go): записанный настоящим клиентом запрос исполняется настоящим сервером в отдельной bubble с часами на ±24 часа; остаток 5 с и точное истечение сохраняются. Также `TestDeadlinePropagation`, `TestDeadlineExceeded`, сетевой deadline test | Подтверждено. Сдвиг виртуальных часов, без изменения системного времени или clock hooks в runtime |
| RPC-6 | Отмена контекста сбрасывает направления и останавливает обработчик | [TestCancellationStopsHandler / TestCancellationUnblocksServerRecv / TestCancellationUnblocksServerSend / TestManyCallsLeaveNothingBehind](../rpc/deadline_test.go), реальные сетевые сценарии | Подтверждено. `synctest` проверяет завершение горутин внутри bubble, а не произвольные утечки вне неё. Закрытие во время подключения дополнительно проверено ниже |
| RPC-7 | FIN закрывает только отправку, сервер продолжает отвечать | [TestBidiHalfClose](../rpc/bidi_test.go), [TestClientStreaming](../rpc/streaming_test.go), общий `TestConformance/HalfCloseEcho` | Подтверждено на обоих транспортах |
| RPC-8 | Начальные метаданные и trailers; бинарные и повторные значения | [TestMetadata / TestHeaderCanBeSentOnce](../rpc/metadata_test.go), `TestBidiHeaderWhileReceiving`, дифференциальные сценарии | Подтверждено; статусы с details проверены отдельно |
| RPC-9 | Backpressure блокирует отправителя, сохраняет данные и ограничивает удерживаемую память. [QUIC](../rpc/transport/quic/quic.go), [mem](../rpc/transport/mem/mem.go) | [TestSlowReaderBoundsMemory](../rpc/backpressure_test.go): 64 МиБ, 1024 сообщения по 64 КиБ, читатель останавливается и затем читает с паузами; проверяются все payload, конечный EOF и бюджет 32 МиБ прироста live Go heap | Подтверждено на `mem` и QUIC. Память измеряется после GC в контрольных точках, это не предел RSS и не измерение каждого мгновения |
| RPC-10 | Клиентские и серверные unary/stream interceptors, цепочки и обёртки | [TestUnaryInterceptors / TestStreamInterceptors / TestInterceptorsAbort / TestStreamInterceptorsWrap / TestInterceptorsSeeCancellation](../rpc/interceptor_test.go), `TestSmokeInterceptors` | Подтверждено для нативного API. У `grpcbridge` stream interceptors отсутствуют |
| RPC-11 | Общий контракт транспорта; независимые `mem` и QUIC | [transport.go](../rpc/transport/transport.go), общий [TestConformance](../rpc/internal/transporttest/transporttest.go), RPC-фабрики обоих транспортов | Подтверждено; у `mem` нет общего окна соединения, поэтому он не заменяет сетевые тесты |
| RPC-12 | Keepalive, обнаружение потери, новое соединение; безопасный повтор открытия до отправки | [TestRedialAfterConnectionLoss / TestRetryOnClosedConnection / TestRetryOnce / TestServerRestart / TestDeadServer](../rpc/clientconn_test.go), [TestKeepAlive](../rpc/transport/quic/quic_test.go), `TestStatelessReset` | Подтверждено указанными тестами. Уже отправленные вызовы произвольно не повторяются; рестарт без reset key обнаруживается по idle timeout |
| RPC-13 | Один `.proto` собирается для grpc-go и RPC; все streaming-индексы корректны | `make proto-check`, [TestGolden / TestPackageSuffixOption / TestStreamIndices](../rpc/cmd/protoc-gen-rpcquic/generate_test.go), smoke и differential tests | Подтверждено; старый marker 1 отдельно проверен на компиляцию |
| RPC-14 | TLS-билеты, opt-in 0-RTT, отметка идемпотентности на обеих сторонах, безопасный отказ | [TestEarlyIdempotencyGates](../rpc/early_test.go), [TestEarlyCalls / TestEarlyRejection / TestEarlyAuthenticationFailure / TestEarlyDisabled](../rpc/transport/quic/early_test.go), [TestGeneratedIdempotency](../internal/gen/difftest/v1/difftestv1quic/early_test.go), `BenchmarkEarlyReconnect` | Подтверждено тестами и отдельным замером ниже; `grpcbridge` не включает ранние RPC |
| RPC-16 | GOAWAY, REFUSED, завершение текущих RPC и уход новых на другое соединение | [TestGracefulStop / TestGracefulStopTimeout / TestGracefulStopWaitsForHandlers / TestGracefulStopRefuses / TestGracefulStopRealTime](../rpc/gracefulstop_test.go), [TestGracefulStop](../grpcbridge/gracefulstop_test.go) | Подтверждено; нативный тайм-аут задаётся контекстом, мост имеет свой ограниченный тайм-аут |

## Тестирование и дополнительные ограничения

| ID | Результат аудита |
| --- | --- |
| TST-1 | Пакет `rpc` покрыт на 94,5% statements, выше целевых 80%. Генератор — 97,5%, `transport/mem` — 95,6%, `transport/quic` — 93,8%. Это значения отдельных пакетов, не среднее всех модулей |
| TST-2 | Все пять fuzz-целей прошли с бюджетами CI: три декодера по 1 000 000 исполнений, клиент и сервер по 100 000 |
| TST-3 | PBT проверяет грамматику запроса/ответа, конечный статус и некорректные последовательности; `TestNoMessageAfterCancel` отдельно проверяет уже буферизованный ответ в `synctest`. Единой случайной модели кадры + отмена + закрытие нет |
| TST-4 | Локальные тесты всех трёх модулей с `-race` прошли. Это не свидетельство удалённого статуса GitHub CI |
| TST-5 | `TestDifferential` прошёл на семи стеках: grpc-h2, rpc-quic/mem, bridge-quic/mem, connect-h2, grpc-h3. Для connect-go известные различия некорректных вызовов проверяются явно, а часть библиотечных сообщений сравнивается только по коду; полное побитовое совпадение всех ошибок не заявляется |
| TST-6 | `RunSynctest` выполняет каждый conformance case `mem` в своей bubble. Дедлайны, отмена, закрытие и тест медленного читателя на `mem` используют виртуальное время; polling в REFUSED-тесте заменён каналом. Реальные сетевые таймеры проверяются на QUIC, а не дублируются реальными паузами на `mem` |
| TST-7 | [TestRPCPacketLoss](../rpc/transport/quic/loss_test.go) проверяет все четыре вида RPC, отмену, deadline и восстановление при потере каждого 20-го серверного UDP-пакета; unit test подтверждает, что пакеты действительно не доставлены. [TestRunPacketLoss](../bench/cmd/bench/run_test.go) проверяет QUIC через настоящий netem с потерями 5% в каждом направлении. Пропуск стенда запрещён |
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

## Исправлено закрытие во время Dial

Ранее `Close` возвращался, но вызов, ожидающий Dial, дожидался своего дедлайна
и получал `DeadlineExceeded`. Теперь клиент хранит отмену текущего Dial и
отдельный сигнал закрытия для ожидающих вызовов. Они получают `Canceled`.
Позднее соединение после закрытия не устанавливается и освобождается.

[Регрессионные тесты](../rpc/clientconn_lifecycle_test.go) внутри `synctest`
проверяют отмену самого Dial и восьми ожидающих RPC без дедлайна, раннее
завершение ожидающего стрима при задержке возврата Dial, закрытие позднего
соединения и независимость контекстов разных вызовов. Bubble проверяет,
что все созданные горутины завершились.

[TestCloseDuringHandshake](../rpc/transport/quic/clientclose_test.go) повторяет
сценарий на настоящем QUIC: серверные пакеты удерживаются, handshake не закончен,
`Close` даёт `Canceled` при ещё живом вызывающем контексте, обработчик не запускается.
Контекст Dial управляет установкой, а не временем жизни возвращённого соединения;
пользовательский Dialer обязан учитывать его отмену.

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

| Проверка | Результат локальной приёмки 4 ноября 2026 года |
| --- | --- |
| `QRPC_TESTBED_REQUIRED=1 make check` | Успех: fmt, tidy-diff, генерация и fixture, vet, lint и тесты с race detector всех трёх модулей |
| `QRPC_TESTBED_REQUIRED=1 make test TESTFLAGS=-cpu=1` | Успех во всех трёх модулях; недоступность стенда не могла превратиться в тихий skip |
| `make plots-test` | 50 тестов прошли; закреплённые зависимости установлены в `.venv` |
| `make fuzz` | Все пять целей прошли с исходными бюджетами Makefile/CI; паник и зависаний не обнаружено |
| `cd rpc && go test -count=1 -coverprofile=/tmp/quic-rpc-go-phase3-rpc.cover ./...` | Успех; пакет `rpc` — 94,5% statements |
| `cd rpc && go list -deps .` | Границы runtime-зависимостей подтверждены |
| Старый generated client + текущий runtime | `go test -mod=mod ./...` во временном модуле: компиляция marker 1 успешна; тестовых сценариев в этом модуле нет |
| Новые пограничные тесты с `-count=5 -cpu=1` | Пять повторов закрытия, разных часов, памяти и потерь прошли; обычные и race-проверки также выполнены |
| Удалённый CI нового изменения | Ожидается после коммита и push пользователем; локальные проверки перечислены отдельно |

Среда: Linux amd64, Intel Core i5-12450H, Go 1.26.8, quic-go v0.63.0,
grpc-go v1.84.0, connect-go v2.0.2, protobuf v1.36.12, protoc 3.21.12,
golangci-lint 2.12.2, Python 3.12.3. Полные локальные журналы находятся в
`out/phase3-completion-20261104/`; этот каталог игнорируется Git и не является
опубликованным архивом результатов. Основные наблюдения сохранены в этом документе.

Эти исходники ещё не отправлены в GitHub: зелёный удалённый CI этого изменения
не заявляется. Локальные проверки подтверждаются независимо от доступности GitHub.

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
| Без 0-RTT | 105,7 / 106,2 / 105,8 | 2,114 / 2,123 / 2,116 |
| С 0-RTT | 54,24 / 54,27 / 54,77 | 1,085 / 1,085 / 1,095 |

Каждое значение — среднее 20 подключений внутри одной серии benchmark.
Разница медиан трёх средних — около 51,5 мс при добавленных 50 мс RTT:
приёмочный эффект RPC-14 подтверждён. Это не доверительный интервал,
не результат основной серии и не сравнение производительности с gRPC.

## Границы завершения

Код исправления и недостающие локальные проверки готовы. Для формального
закрытия фазы 3 остаётся зелёный CI соответствующего PR после коммита и push.
Предыдущие результаты Q-01 не заменяют CI нового кода.

Полная самопроверка всех профилей `make testbed-check`, основные научные серии,
двуххостовые измерения, минимальный Go 1.26.3, выпуск модулей и полный охват Wisply
не выполнялись. Это дальнейшие этапы; желательное RPC-15 не добавлено к воротам
фазы 3. Проверенные бюджеты памяти и модели потерь перечислены выше, выводы
не распространяются на произвольные нагрузки и все возможные модели сети.
