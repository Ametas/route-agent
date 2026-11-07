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
`-ldflags "-X github.com/Ametas/route-agent/internal/version.Version=$(git describe --tags --match 'v*')"`,
и бинарник релиза обязан печатать её по `route-agent --version` в форме `vX.Y.Z[-N-gHASH]`.
Собирает их `scripts/build-release.sh` (версия — `git describe --tags --match 'v*'`, чтобы
тег `nightly` в неё не попадал). Публикуются они в двух местах: задание `go-assets`
в `.github/workflows/release.yml` прикладывает их к каждому релизу semantic-release на `main`, а
задание `nightly` в `go.yml` на каждом push в `alpha` заменяет вложения пре-релиза `nightly`.
Подробнее — [`cutover.md`](cutover.md).

Тег `nightly` ставится один раз и не двигается, заменяются только вложения (коммит сборки — в
заметке пре-релиза). Сдвинутый тег сломал бы TS-узлы: их SelfUpdate начинается с
`git fetch --all --tags` без `--force`, и git отвергает новое место тега, уже скачанного раньше
(«would clobber existing tag»), с кодом 1 — цепочка обновления останавливается на каждом
следующем обновлении.

## Каналы

Решение владельца (2026-11-07): у агента две ветки — `main` (stable) и `alpha` (nightly).

| Канал | Откуда | Ветка TS-агента |
|---|---|---|
| `stable` | `https://github.com/Ametas/route-agent/releases/latest/download/<вложение>` (`update.DefaultReleaseBaseURL`): последний релиз, не пре-релиз | `main` |
| `nightly` | `https://github.com/Ametas/route-agent/releases/download/nightly/<вложение>` (`update.NightlyReleaseBaseURL`): пре-релиз из `alpha` | `alpha` |

GitHub отдаёт вложение по имени, без API и его лимитов.

Канал выбирает оркестратор (кнопка «Поставить Nightly»). Прото не меняется, хеш договора
прежний, поэтому канал едет в metadata SelfUpdate: заголовок `x-agent-channel` со значением
`stable` или `nightly` (строчными, точно).
- **Заголовка нет** — как раньше: Go-агент берёт канал из файла `bin/channel`, TS — upstream
  текущей ветки. Ответы байт в байт прежние.
- **Другое значение** (в том числе пустое) — `{success:false, message:"Unknown update channel '<значение>'."}`
  (значение обрезается до 32 символов), ничего не запускается.
- **Заголовок принят** — в `message` ответа стоит `channel=<значение>`. По этому токену оркестратор
  узнаёт, что агент заголовок понял: старый агент его молча пропускает и отвечает старым текстом.
  Go: `Self-update sequence initiated (channel=<значение>). Agent will pull code and restart in 1 second.`;
  TS: `Self-update sequence initiated (channel=<значение>). Agent will switch to branch <ветка> and restart in 1 second.`

**Канал Go-агента помнится** в файле `channel` рядом с бинарником (`/opt/route-agent/bin/channel`):
одна строка `stable` или `nightly`, 0644, запись атомарная. Нет файла или в нём мусор — `stable`.
Пишут его SelfUpdate (после успешной подмены, до перезапуска), `bridge-to-go.sh --channel` и
`install-go.sh --channel`. Не записался после подмены — ошибка в журнал, перезапуск всё равно
идёт: новый бинарник уже на месте, а канал поправит следующее обновление с заголовком.

Вернуть Go-узел с nightly на stable до перехода (cutover.md §B) нельзя: в `releases/latest` ещё
нет Go-вложений, скачивание падает, и узел остаётся на nightly.

## Последовательность

1. Авторизация — только metadata `x-orchestrator-secret` (как в TS). Отказ —
   `{success:false, message:"Invalid orchestrator secret token."}`.
2. Заголовок `x-agent-channel` с неизвестным значением — отказ (см. «Каналы»). Канал — из
   заголовка, без него — из `bin/channel`.
3. Если обновление уже идёт — `{success:true, message:"Self-update sequence already in progress."}`
   (с заголовком — `Self-update sequence already in progress (channel=<канал идущего>).`),
   второе не запускается (в TS шли два конвейера параллельно, A R-5).
4. Иначе сразу ответ `{success:true, message:"Self-update sequence initiated. Agent will pull code and restart in 1 second."}`
   (без заголовка текст прежний: оркестратор его показывает), работа начинается через 1 с в фоне;
   весь проход ограничен 5 минутами.
5. `checksums.txt` канала (таймаут 30 с, не больше 1 МиБ) → строка для своего вложения.
6. Бинарник качается во временный файл в каталоге установленного (таймаут 3 мин, не больше
   256 МиБ), SHA-256 считается на лету. Не совпало — файл удаляется, установленный не тронут.
7. `chmod 0755`, fsync, запуск `<временный> --version` (таймаут 15 с). Вывод должен иметь форму
   `vX.Y.Z[-N-gHASH]`, иначе — отказ без подмены (иначе оркестратор счёл бы узел отставшим и
   обновлял его каждые 6 часов).
8. Текущий бинарник копируется атомарно в `<путь>.prev` (`/opt/route-agent/bin/route-agent.prev`)
   — для ручного отката: `mv route-agent.prev route-agent && systemctl restart route-agent`.
9. `rename` временного файла на место установленного, fsync каталога; канал пишется в
   `bin/channel`.
10. `systemctl restart route-agent` (таймаут 60 с). Процесс при этом погибает; задание уже в
   systemd. Оркестратор ждёт переподключения потока телеметрии ≤180 с и сверяет `GetAgentInfo`.

Любой сбой до шага 9 оставляет работающий агент, установленный бинарник и `bin/channel` как были; ошибка — в
журнал (`Failed to execute self-update sequence`), оркестратору не сообщается (как в TS).

Подменяется тот файл, из которого запущен процесс (`os.Executable()`); юнит должен запускать его
напрямую (`ExecStart=/opt/route-agent/bin/route-agent`, architecture §3).
