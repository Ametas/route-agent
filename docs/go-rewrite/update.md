# Обновление Go-агента (SelfUpdate)

Решение владельца (architecture §3): Go-агент обновляется бинарником из релиза GitHub, без git,
npm и сборки на узле. Код — `internal/update`. Переход TS → Go (первое обновление стоящего узла
старым агентом) описан отдельно в architecture §4 и этим механизмом не покрывается.

## Что лежит в релизе

| Вложение | Что |
|---|---|
| `route-agent-linux-amd64` | статический бинарник (`CGO_ENABLED=0`) |
| `route-agent-linux-arm64` | то же для arm64 |
| `checksums.txt` | формат `sha256sum`: `<64 hex>  <имя вложения>` (допускается и `*<имя>`) |

Имя вложения — `route-agent-linux-<GOARCH>` (`update.AssetName`); агент берёт то, что совпадает с
его собственной `runtime.GOARCH`. Версия вшивается при сборке:
`-ldflags "-X github.com/Ametas/route-agent/internal/version.Version=$(git describe --tags --always)"`,
и бинарник релиза обязан печатать её по `route-agent --version` в форме `vX.Y.Z[-N-gHASH]`.
Сборка этих вложений — `.github/workflows/go.yml` (пока как артефакт CI ветки; публикация
вложений к релизу добавляется в release-задание последним коммитом ветки).

Откуда берётся: `https://github.com/Ametas/route-agent/releases/latest/download/<вложение>`
(`update.DefaultReleaseBaseURL`) — GitHub отдаёт вложение последнего релиза (не пре-релиза) по
имени, без API и его лимитов.

## Последовательность

1. Авторизация — только metadata `x-orchestrator-secret` (как в TS). Отказ —
   `{success:false, message:"Invalid orchestrator secret token."}`.
2. Если обновление уже идёт — `{success:true, message:"Self-update sequence already in progress."}`,
   второе не запускается (в TS шли два конвейера параллельно, A R-5).
3. Иначе сразу ответ `{success:true, message:"Self-update sequence initiated. Agent will pull code and restart in 1 second."}`
   (текст прежний: оркестратор его показывает), работа начинается через 1 с в фоне; весь проход
   ограничен 5 минутами.
4. `checksums.txt` (таймаут 30 с, не больше 1 МиБ) → строка для своего вложения.
5. Бинарник качается во временный файл в каталоге установленного (таймаут 3 мин, не больше
   256 МиБ), SHA-256 считается на лету. Не совпало — файл удаляется, установленный не тронут.
6. `chmod 0755`, fsync, запуск `<временный> --version` (таймаут 15 с). Вывод должен иметь форму
   `vX.Y.Z[-N-gHASH]`, иначе — отказ без подмены (иначе оркестратор счёл бы узел отставшим и
   обновлял его каждые 6 часов).
7. Текущий бинарник копируется атомарно в `<путь>.prev` (`/opt/route-agent/bin/route-agent.prev`)
   — для ручного отката: `mv route-agent.prev route-agent && systemctl restart route-agent`.
8. `rename` временного файла на место установленного, fsync каталога.
9. `systemctl restart route-agent` (таймаут 60 с). Процесс при этом погибает; задание уже в
   systemd. Оркестратор ждёт переподключения потока телеметрии ≤180 с и сверяет `GetAgentInfo`.

Любой сбой до шага 8 оставляет работающий агент и установленный бинарник как были; ошибка — в
журнал (`Failed to execute self-update sequence`), оркестратору не сообщается (как в TS).

Подменяется тот файл, из которого запущен процесс (`os.Executable()`); юнит должен запускать его
напрямую (`ExecStart=/opt/route-agent/bin/route-agent`, architecture §3).
