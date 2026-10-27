# Сводка серии

Прогонов: 372, из них прогонов дрейфа: 12. Вариантов: 3, сетей: 6, нагрузок: 2. Прогонов на ненастроенной машине: 0.

## p99 вызовов small, мс

Медиана p99 по прогонам ячейки и 95%-интервал bootstrap; в скобках — доля вызовов с ошибкой, если они были, и сколько прогонов перегружено, если ячейка перегружена (раздел «Перегрузка»). Вызов с ошибкой входит в перцентили со своей задержкой, не больше дедлайна 10 с.

| network | load | grpc-h2 | grpc-h2-tcp-default | quic |
|---|---|---|---|---|
| base | small | 51.3 [51.3, 51.3] | 51.3 [51.3, 51.3] | 51.3 [51.3, 51.3] |
| base | small-heavy | 129 [127, 130] | 133 [132, 135] | 70.5 [69.2, 71.4] |
| interregion | small | 1.39e+03 [800, 3.65e+03] | 2.33e+03 [1.27e+03, 4.76e+03] (ошибок 0.11%) | 718 [566, 947] |
| interregion | small-heavy | 1e+04 [1e+04, 1e+04] (ошибок 93%; перегрузка 10 из 10) | 1e+04 [1e+04, 1e+04] (ошибок 78%; перегрузка 10 из 10) | 3.39e+03 [2.69e+03, 4.01e+03] (перегрузка 10 из 10) |
| loss-1 | small | 178 [156, 208] | 158 [149, 221] | 108 [108, 108] |
| loss-1 | small-heavy | 1e+04 [1e+04, 1e+04] (ошибок 62%; перегрузка 10 из 10) | 1e+04 [2.76e+03, 1e+04] (ошибок 48%; перегрузка 7 из 10) | 227 [203, 241] |
| loss-2 | small | 713 [482, 997] | 415 [371, 557] | 121 [116, 139] |
| loss-2 | small-heavy | 1e+04 [9.8e+03, 1e+04] (ошибок 51%; перегрузка 8 из 10) | 1e+04 [1e+04, 1e+04] (ошибок 70%; перегрузка 10 из 10) | 339 [324, 367] |
| loss-5 | small | 1e+04 [1e+04, 1e+04] (ошибок 43%; перегрузка 10 из 10) | 1e+04 [8.95e+03, 1e+04] (ошибок 6.4%; перегрузка 10 из 10) | 303 [263, 382] |
| loss-5 | small-heavy | 1e+04 [1e+04, 1e+04] (ошибок 83%; перегрузка 10 из 10) | 1e+04 [1e+04, 1e+04] (ошибок 80%; перегрузка 10 из 10) | 859 [780, 1.1e+03] |
| rtt-150 | small | 151 [151, 151] | 151 [151, 151] | 151 [151, 151] |
| rtt-150 | small-heavy | 229 [227, 229] | 418 [393, 479] | 152 [152, 152] |

## Перегрузка

Прогон перегружен по виду вызова, если с ошибкой завершилось больше 1% вызовов или задержка выросла за измерение больше чем в 1,5 раза: рост — медиана задержки вызовов последней трети измерения против первой. Ячейка перегружена, если перегружено больше половины её прогонов. Перегруженный прогон не успевает за нагрузкой: задержки растут с длиной прогона до дедлайна, и его перцентили описывают прогон, а не транспорт.

Ниже — сети и нагрузки, где перегружен хотя бы один прогон. Рост — медиана по прогонам; goodput — полезные данные отвеченных вызовов всех видов в обе стороны, медиана по прогонам.

| network | load | variant | kind | перегружено | ошибок, % | рост | goodput, Мбит/с |
|---|---|---|---|---|---|---|---|
| interregion | small | grpc-h2 | small | 5 из 10 | 0 | 1.73 | 1.75 |
| interregion | small | grpc-h2-tcp-default | small | 5 из 10 | 0.109 | 3.17 | 1.75 |
| interregion | small | quic | small | 1 из 10 | 0 | 1.11 | 1.75 |
| interregion | small-heavy | grpc-h2 | heavy | 10 из 10 | 100 | 1 | 0.0518 |
| interregion | small-heavy | grpc-h2 | small | 10 из 10 | 93.1 | 1 | 0.0518 |
| interregion | small-heavy | grpc-h2-tcp-default | heavy | 10 из 10 | 100 | 1 | 0.239 |
| interregion | small-heavy | grpc-h2-tcp-default | small | 10 из 10 | 77.6 | 1.03 | 0.239 |
| interregion | small-heavy | quic | heavy | 10 из 10 | 100 | 1 | 1.75 |
| interregion | small-heavy | quic | small | 10 из 10 | 0 | 3.6 | 1.75 |
| loss-1 | small-heavy | grpc-h2 | heavy | 10 из 10 | 100 | 1 | 0.706 |
| loss-1 | small-heavy | grpc-h2 | small | 10 из 10 | 62.5 | 1.1 | 0.706 |
| loss-1 | small-heavy | grpc-h2-tcp-default | heavy | 10 из 10 | 100 | 1 | 0.591 |
| loss-1 | small-heavy | grpc-h2-tcp-default | small | 7 из 10 | 47.7 | 1 | 0.591 |
| loss-1 | small-heavy | quic | heavy | 10 из 10 | 96.7 | 1 | 2.03 |
| loss-1 | small-heavy | quic | small | 0 из 10 | 0 | 1.08 | 2.03 |
| loss-2 | small | grpc-h2 | small | 2 из 10 | 0 | 0.898 | 1.75 |
| loss-2 | small | grpc-h2-tcp-default | small | 2 из 10 | 0 | 1.2 | 1.75 |
| loss-2 | small | quic | small | 0 из 10 | 0 | 1 | 1.75 |
| loss-2 | small-heavy | grpc-h2 | heavy | 10 из 10 | 100 | 1 | 0.658 |
| loss-2 | small-heavy | grpc-h2 | small | 8 из 10 | 50.6 | 1.08 | 0.658 |
| loss-2 | small-heavy | grpc-h2-tcp-default | heavy | 10 из 10 | 100 | 1 | 0.374 |
| loss-2 | small-heavy | grpc-h2-tcp-default | small | 10 из 10 | 69.6 | 1.01 | 0.374 |
| loss-2 | small-heavy | quic | heavy | 10 из 10 | 100 | 1 | 1.75 |
| loss-2 | small-heavy | quic | small | 0 из 10 | 0 | 1.09 | 1.75 |
| loss-5 | small | grpc-h2 | small | 10 из 10 | 43 | 1.95 | 0.949 |
| loss-5 | small | grpc-h2-tcp-default | small | 10 из 10 | 6.37 | 2.73 | 1.71 |
| loss-5 | small | quic | small | 0 из 10 | 0 | 1.14 | 1.75 |
| loss-5 | small-heavy | grpc-h2 | heavy | 10 из 10 | 100 | 1 | 0.303 |
| loss-5 | small-heavy | grpc-h2 | small | 10 из 10 | 82.9 | 1.03 | 0.303 |
| loss-5 | small-heavy | grpc-h2-tcp-default | heavy | 10 из 10 | 100 | 1 | 0.338 |
| loss-5 | small-heavy | grpc-h2-tcp-default | small | 10 из 10 | 80.3 | 1.05 | 0.338 |
| loss-5 | small-heavy | quic | heavy | 10 из 10 | 100 | 1 | 1.75 |
| loss-5 | small-heavy | quic | small | 2 из 10 | 0 | 1.09 | 1.75 |

## Сравнение p99 вызовов small

Эффект — медиана p99 QUIC против медианы другого варианта; отрицательный — QUIC быстрее. p — двусторонний критерий Манна–Уитни по прогонам с поправкой Холма в пределах нагрузки и пары вариантов. Режим — какая из двух ячеек перегружена; если перегружена хоть одна, эффект говорит о перегрузке, а не о задержке.

| network | load | quic | other | режим | эффект, % [интервал] | p (Холм) |
|---|---|---|---|---|---|---|
| base | small | quic | grpc-h2 | без перегрузки | -0.0 [-0.0, -0.0] | 0.0044 |
| base | small | quic | grpc-h2-tcp-default | без перегрузки | -0.0 [-0.0, +0.0] | 0.281 |
| base | small-heavy | quic | grpc-h2 | без перегрузки | -45.1 [-46.3, -44.2] | 0.0011 |
| base | small-heavy | quic | grpc-h2-tcp-default | без перегрузки | -46.9 [-48.2, -46.2] | 0.0011 |
| interregion | small | quic | grpc-h2 | без перегрузки | -48.5 [-78.0, -4.5] | 0.0757 |
| interregion | small | quic | grpc-h2-tcp-default | без перегрузки | -69.2 [-86.1, -44.2] | 0.0219 |
| interregion | small-heavy | quic | grpc-h2 | перегружены оба | -66.1 [-73.1, -59.8] | 0.0011 |
| interregion | small-heavy | quic | grpc-h2-tcp-default | перегружены оба | -66.1 [-73.1, -59.8] | 0.0011 |
| loss-1 | small | quic | grpc-h2 | без перегрузки | -39.6 [-48.1, -30.9] | 0.0011 |
| loss-1 | small | quic | grpc-h2-tcp-default | без перегрузки | -31.7 [-51.2, -27.4] | 0.0011 |
| loss-1 | small-heavy | quic | grpc-h2 | перегружен другой | -97.7 [-98.0, -97.6] | 0.0011 |
| loss-1 | small-heavy | quic | grpc-h2-tcp-default | перегружен другой | -97.7 [-98.0, -91.7] | 0.0011 |
| loss-2 | small | quic | grpc-h2 | без перегрузки | -83.1 [-88.0, -73.9] | 0.0011 |
| loss-2 | small | quic | grpc-h2-tcp-default | без перегрузки | -70.9 [-78.1, -64.8] | 0.0011 |
| loss-2 | small-heavy | quic | grpc-h2 | перегружен другой | -96.6 [-96.8, -96.3] | 0.0011 |
| loss-2 | small-heavy | quic | grpc-h2-tcp-default | перегружен другой | -96.6 [-96.8, -96.3] | 0.0011 |
| loss-5 | small | quic | grpc-h2 | перегружен другой | -97.0 [-97.4, -96.2] | 0.0011 |
| loss-5 | small | quic | grpc-h2-tcp-default | перегружен другой | -97.0 [-97.4, -96.1] | 0.0011 |
| loss-5 | small-heavy | quic | grpc-h2 | перегружен другой | -91.4 [-92.2, -89.0] | 0.0011 |
| loss-5 | small-heavy | quic | grpc-h2-tcp-default | перегружен другой | -91.4 [-92.2, -89.0] | 0.0011 |
| rtt-150 | small | quic | grpc-h2 | без перегрузки | -0.0 [-0.0, -0.0] | 0.0011 |
| rtt-150 | small | quic | grpc-h2-tcp-default | без перегрузки | +0.0 [-0.0, +0.0] | 0.678 |
| rtt-150 | small-heavy | quic | grpc-h2 | без перегрузки | -33.4 [-33.6, -33.0] | 0.0011 |
| rtt-150 | small-heavy | quic | grpc-h2-tcp-default | без перегрузки | -63.6 [-68.2, -61.2] | 0.0011 |

## Разброс p99 между прогонами

cv — коэффициент вариации p99 по прогонам, s_log — стандартное отклонение его логарифма. runs_for_N — сколько прогонов на вариант нужно, чтобы различить разницу p99 в N% (критерий Манна–Уитни, α = 0,05, мощность 0,8, нормальное приближение для логарифма p99). В перегруженной ячейке p99 упирается в дедлайн или растёт с длиной прогона, и его разброс о транспорте не говорит.

| network | load | variant | runs | перегрузка | cv | s_log | runs_for_5pct | runs_for_10pct | runs_for_20pct |
|---|---|---|---|---|---|---|---|---|---|
| base | small | grpc-h2 | 10 |  | 0.00021 | 0.00021 | 2 | 2 | 2 |
| base | small | grpc-h2-tcp-default | 10 |  | 0.000253 | 0.000252 | 2 | 2 | 2 |
| base | small | quic | 10 |  | 0.000203 | 0.000203 | 2 | 2 | 2 |
| base | small-heavy | grpc-h2 | 10 |  | 0.0155 | 0.0154 | 2 | 2 | 2 |
| base | small-heavy | grpc-h2-tcp-default | 10 |  | 0.0157 | 0.0157 | 2 | 2 | 2 |
| base | small-heavy | quic | 10 |  | 0.0214 | 0.0215 | 4 | 2 | 2 |
| interregion | small | grpc-h2 | 10 |  | 0.871 | 0.856 | 5058 | 1326 | 363 |
| interregion | small | grpc-h2-tcp-default | 10 |  | 0.887 | 0.972 | 6527 | 1711 | 468 |
| interregion | small | quic | 10 |  | 0.44 | 0.382 | 1009 | 265 | 73 |
| interregion | small-heavy | grpc-h2 | 10 | да | 2.85e-07 | 2.85e-07 | 2 | 2 | 2 |
| interregion | small-heavy | grpc-h2-tcp-default | 10 | да | 0.0518 | 0.0555 | 22 | 6 | 2 |
| interregion | small-heavy | quic | 10 | да | 0.233 | 0.243 | 409 | 107 | 30 |
| loss-1 | small | grpc-h2 | 10 |  | 0.374 | 0.295 | 602 | 158 | 44 |
| loss-1 | small | grpc-h2-tcp-default | 10 |  | 0.257 | 0.231 | 370 | 97 | 27 |
| loss-1 | small | quic | 10 |  | 0.00501 | 0.00499 | 2 | 2 | 2 |
| loss-1 | small-heavy | grpc-h2 | 10 | да | 1.42e-06 | 1.42e-06 | 2 | 2 | 2 |
| loss-1 | small-heavy | grpc-h2-tcp-default | 10 | да | 0.489 | 0.753 | 3917 | 1027 | 281 |
| loss-1 | small-heavy | quic | 10 |  | 0.0932 | 0.094 | 62 | 17 | 5 |
| loss-2 | small | grpc-h2 | 10 |  | 0.372 | 0.405 | 1131 | 297 | 81 |
| loss-2 | small | grpc-h2-tcp-default | 10 |  | 0.403 | 0.384 | 1018 | 267 | 73 |
| loss-2 | small | quic | 10 |  | 0.122 | 0.116 | 94 | 25 | 7 |
| loss-2 | small-heavy | grpc-h2 | 10 | да | 0.186 | 0.255 | 450 | 118 | 33 |
| loss-2 | small-heavy | grpc-h2-tcp-default | 10 | да | 0.00377 | 0.00378 | 2 | 2 | 2 |
| loss-2 | small-heavy | quic | 10 |  | 0.0868 | 0.0875 | 53 | 14 | 4 |
| loss-5 | small | grpc-h2 | 10 | да | 1.3e-05 | 1.3e-05 | 2 | 2 | 2 |
| loss-5 | small | grpc-h2-tcp-default | 10 | да | 0.107 | 0.119 | 98 | 26 | 7 |
| loss-5 | small | quic | 10 |  | 0.241 | 0.233 | 377 | 99 | 27 |
| loss-5 | small-heavy | grpc-h2 | 10 | да | 5.43e-07 | 5.43e-07 | 2 | 2 | 2 |
| loss-5 | small-heavy | grpc-h2-tcp-default | 10 | да | 6.04e-07 | 6.04e-07 | 2 | 2 | 2 |
| loss-5 | small-heavy | quic | 10 |  | 0.266 | 0.243 | 408 | 107 | 30 |
| rtt-150 | small | grpc-h2 | 10 |  | 4.38e-05 | 4.38e-05 | 2 | 2 | 2 |
| rtt-150 | small | grpc-h2-tcp-default | 10 |  | 1.98e-05 | 1.98e-05 | 2 | 2 | 2 |
| rtt-150 | small | quic | 10 |  | 4.54e-05 | 4.54e-05 | 2 | 2 | 2 |
| rtt-150 | small-heavy | grpc-h2 | 10 |  | 0.0111 | 0.011 | 2 | 2 | 2 |
| rtt-150 | small-heavy | grpc-h2-tcp-default | 10 |  | 0.134 | 0.129 | 115 | 31 | 9 |
| rtt-150 | small-heavy | quic | 10 |  | 0.000165 | 0.000165 | 2 | 2 | 2 |

## CPU на вызов, мкс

Медиана по прогонам: время CPU процесса (user + system) или занятость его ядер (всё, кроме простоя), делённые на число вызовов измерения всех видов. Занятость ядер учитывает и работу ядра ОС для сети, которая процессу не засчитывается.

| network | load | variant | runs | сервер, процесс | сервер, ядра | клиент, процесс | клиент, ядра |
|---|---|---|---|---|---|---|---|
| base | small | grpc-h2 | 10 | 118 | 127 | 183 | 175 |
| base | small | grpc-h2-tcp-default | 10 | 119 | 131 | 181 | 176 |
| base | small | quic | 10 | 185 | 190 | 228 | 214 |
| base | small-heavy | grpc-h2 | 10 | 122 | 161 | 250 | 237 |
| base | small-heavy | grpc-h2-tcp-default | 10 | 124 | 145 | 244 | 240 |
| base | small-heavy | quic | 10 | 294 | 308 | 384 | 355 |
| interregion | small | grpc-h2 | 10 | 63.8 | 88.4 | 146 | 139 |
| interregion | small | grpc-h2-tcp-default | 10 | 60.4 | 89.4 | 145 | 144 |
| interregion | small | quic | 10 | 88.8 | 116 | 159 | 148 |
| interregion | small-heavy | grpc-h2 | 10 | 58.3 | 73.9 | 184 | 187 |
| interregion | small-heavy | grpc-h2-tcp-default | 10 | 58.4 | 85.6 | 177 | 174 |
| interregion | small-heavy | quic | 10 | 93.7 | 109 | 158 | 148 |
| loss-1 | small | grpc-h2 | 10 | 93.7 | 113 | 162 | 151 |
| loss-1 | small | grpc-h2-tcp-default | 10 | 89.5 | 99.9 | 157 | 136 |
| loss-1 | small | quic | 10 | 181 | 193 | 225 | 214 |
| loss-1 | small-heavy | grpc-h2 | 10 | 75.5 | 86.5 | 180 | 178 |
| loss-1 | small-heavy | grpc-h2-tcp-default | 10 | 74.1 | 87.5 | 174 | 157 |
| loss-1 | small-heavy | quic | 10 | 187 | 189 | 205 | 195 |
| loss-2 | small | grpc-h2 | 10 | 73.8 | 87.7 | 150 | 145 |
| loss-2 | small | grpc-h2-tcp-default | 10 | 74.2 | 85.5 | 148 | 134 |
| loss-2 | small | quic | 10 | 176 | 184 | 219 | 210 |
| loss-2 | small-heavy | grpc-h2 | 10 | 71.4 | 84.8 | 171 | 161 |
| loss-2 | small-heavy | grpc-h2-tcp-default | 10 | 56.5 | 78.3 | 175 | 157 |
| loss-2 | small-heavy | quic | 10 | 166 | 172 | 186 | 177 |
| loss-5 | small | grpc-h2 | 10 | 56 | 80.2 | 156 | 148 |
| loss-5 | small | grpc-h2-tcp-default | 10 | 50.6 | 67.8 | 147 | 135 |
| loss-5 | small | quic | 10 | 141 | 159 | 189 | 178 |
| loss-5 | small-heavy | grpc-h2 | 10 | 59.2 | 75.9 | 175 | 165 |
| loss-5 | small-heavy | grpc-h2-tcp-default | 10 | 51.8 | 78.4 | 174 | 172 |
| loss-5 | small-heavy | quic | 10 | 111 | 128 | 159 | 151 |
| rtt-150 | small | grpc-h2 | 10 | 115 | 125 | 180 | 174 |
| rtt-150 | small | grpc-h2-tcp-default | 10 | 114 | 130 | 179 | 164 |
| rtt-150 | small | quic | 10 | 186 | 197 | 230 | 216 |
| rtt-150 | small-heavy | grpc-h2 | 10 | 120 | 172 | 242 | 231 |
| rtt-150 | small-heavy | grpc-h2-tcp-default | 10 | 120 | 140 | 238 | 240 |
| rtt-150 | small-heavy | quic | 10 | 271 | 291 | 281 | 279 |

## Прогрев

Медиана по прогонам отношения медианной задержки первых 5 с измерения к остальным. Заметно больше 1 — прогрева не хватает; заметно меньше 1 — задержка растёт за прогон, это перегрузка.

| network | load | variant | head_ratio |
|---|---|---|---|
| base | small | grpc-h2 | 1 |
| base | small | grpc-h2-tcp-default | 1 |
| base | small | quic | 1 |
| base | small-heavy | grpc-h2 | 1 |
| base | small-heavy | grpc-h2-tcp-default | 1 |
| base | small-heavy | quic | 1 |
| interregion | small | grpc-h2 | 0.547 |
| interregion | small | grpc-h2-tcp-default | 0.481 |
| interregion | small | quic | 0.969 |
| interregion | small-heavy | grpc-h2 | 1 |
| interregion | small-heavy | grpc-h2-tcp-default | 0.945 |
| interregion | small-heavy | quic | 0.271 |
| loss-1 | small | grpc-h2 | 1.05 |
| loss-1 | small | grpc-h2-tcp-default | 0.969 |
| loss-1 | small | quic | 1 |
| loss-1 | small-heavy | grpc-h2 | 0.709 |
| loss-1 | small-heavy | grpc-h2-tcp-default | 0.98 |
| loss-1 | small-heavy | quic | 0.863 |
| loss-2 | small | grpc-h2 | 1.43 |
| loss-2 | small | grpc-h2-tcp-default | 0.912 |
| loss-2 | small | quic | 1 |
| loss-2 | small-heavy | grpc-h2 | 0.558 |
| loss-2 | small-heavy | grpc-h2-tcp-default | 0.594 |
| loss-2 | small-heavy | quic | 0.838 |
| loss-5 | small | grpc-h2 | 0.359 |
| loss-5 | small | grpc-h2-tcp-default | 0.333 |
| loss-5 | small | quic | 0.948 |
| loss-5 | small-heavy | grpc-h2 | 0.836 |
| loss-5 | small-heavy | grpc-h2-tcp-default | 0.748 |
| loss-5 | small-heavy | quic | 0.882 |
| rtt-150 | small | grpc-h2 | 1 |
| rtt-150 | small | grpc-h2-tcp-default | 1 |
| rtt-150 | small | quic | 1 |
| rtt-150 | small-heavy | grpc-h2 | 1 |
| rtt-150 | small-heavy | grpc-h2-tcp-default | 1 |
| rtt-150 | small-heavy | quic | 1 |

## Дрейф

Изменение перцентилей прогонов дрейфа от начала серии к концу, %.

| variant | load | p50_change_pct | p99_change_pct |
|---|---|---|---|
| quic | small | -0.5 | -1.1 |
| grpc-h2-tcp-default | small | -0.4 | -0.9 |
| grpc-h2 | small | -0.2 | -0.5 |
| grpc-h2-tcp-default | small-heavy | -0.3 | -0.9 |
| quic | small-heavy | -0.1 | -1.5 |
| grpc-h2 | small-heavy | -0.1 | +0.1 |

## Рисунки

![fig01-loss-small](fig01-loss-small.png)
![fig02-loss-small-heavy](fig02-loss-small-heavy.png)
![fig04-ecdf-loss-2](fig04-ecdf-loss-2.png)
![fig13-profiles](fig13-profiles.png)
