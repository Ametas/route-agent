# Переход узлов на Go-агента

Как Go-агент попадает на узлы: до слияния — на тестовые узлы с канала nightly (пре-релиз из
ветки `alpha`), после слияния — на весь флот через мост в старом SelfUpdate. Почему переход устроен именно так, объясняет
architecture §4 и spec A §8.3. Здесь только порядок действий.

## Что для этого есть

| Файл | Что делает |
|---|---|
| `scripts/build-release.sh <версия> [каталог]` | Собирает `route-agent-linux-amd64`, `-arm64` и `checksums.txt`. Версию другой формы, чем `vX.Y.Z[-N-gHASH]`, отвергает, а у собранного бинарника сверяет `--version`. Им собирают все три потребителя ниже. |
| `.github/workflows/go.yml`, задание `nightly` | После зелёных проверок на каждом push в `alpha` (на PR не запускается) заменяет вложения пре-релиза `nightly`. Тег `nightly` ставится один раз и не двигается (update.md, «Что лежит в релизе»); коммит сборки — в заметке. Версия — `git describe --tags --match 'v*'`, так что `nightly` в неё не попадает. |
| `.github/workflows/release.yml`, задание `go-assets` | Запускается, если semantic-release на `main` выпустил релиз: собирает бинарники с тегом релиза в роли версии и прикладывает их к релизу. Без релиза пропускается. |
| `install-go.sh` | Установщик Go-агента. CLI тот же, что у `install.sh`, плюс `--channel stable\|nightly`, `--convert` и `--rollback`. Пишет канал в `bin/channel`. |
| `scripts/bridge-to-go.sh --channel stable\|nightly` | Мост. Его запускает `npm run build`: в `alpha` — с `--channel nightly` (§A.0), в коммите слияния — с `--channel stable` (§B). Пишет канал в `bin/channel`. |
| `scripts/test-delivery.sh` | Сухие прогоны обоих скриптов против поддельного релиза, который раздаётся локально. Запускается в CI ветки. |

Каналы (подробно — [`update.md`](update.md), «Каналы»):
- **stable**: `https://github.com/Ametas/route-agent/releases/latest/download/<вложение>`. Это
  последний настоящий релиз. Отсюда берут Go-агент на канале stable, мост с `--channel stable` и
  `install-go.sh --channel stable`.
- **nightly**: `https://github.com/Ametas/route-agent/releases/download/nightly/<вложение>`.
  Это пре-релиз, а пре-релиз никогда не становится `releases/latest`, поэтому узлы на stable его
  не видят.

Все три скрипта на узле делают одни и те же проверки, и ни одна из них не трогает рабочий агент до
подмены:
- SHA-256 сверяется с `checksums.txt`;
- `--version` бинарника должен иметь форму `vX.Y.Z[-N-gHASH]`;
- бинарник ставится в `/opt/route-agent/bin/route-agent` переименованием, а прежний остаётся рядом
  как `route-agent.prev`.

## A. Проверка на узлах до слияния (nightly)

### A.0 Основной путь: кнопка «Поставить Nightly» в оркестраторе

Оркестратор на узлы по SSH не ходит, поэтому узел переводит сам агент своим SelfUpdate:
оркестратор шлёт его с заголовком `x-agent-channel: nightly` (update.md, «Каналы»).
1. TS-агент с поддержкой заголовка (`main` с 2026-11-07) отвечает
   `Self-update sequence initiated (channel=nightly). Agent will switch to branch alpha and restart in 1 second.`
   и через 1 с выполняет:
   `cd /opt/route-agent && git fetch --tags origin +refs/heads/alpha:refs/remotes/origin/alpha && git checkout -q -f -B alpha HEAD && git branch -q --set-upstream-to=origin/alpha alpha && git reset --hard @{u} && git clean -fd && npm ci && npm run build && systemctl restart route-agent`.
   `checkout -B alpha HEAD`, а потом `reset --hard @{u}` — нарочно: reset оставляет прежний коммит
   в `ORIG_HEAD`, и мост записывает его для отката.
2. `npm run build` в `alpha` — `bash scripts/bridge-to-go.sh --channel nightly`: бинарник из
   пре-релиза `nightly`, `bin/channel` = `nightly`, юнит Go, сторож (§B, «Что делает мост на узле»).
   Перезапуск из цепочки поднимает Go-агента.
3. **Старый TS-агент заголовок не знает:** обновляется как обычно (`main`) и отвечает старым
   текстом, без `channel=`. Оркестратор сам делает такой проход первым, а затем повторяет
   SelfUpdate с заголовком — агент уже с `main`, где заголовок понимается.
4. **Go-агент** с заголовком `nightly` берёт пре-релиз и пишет `bin/channel`; дальше обычное
   «Обновить агента» (без заголовка) обновляет его тоже с nightly.
5. **Сторож откатил** — чекаут возвращается на прежний коммит **на ветку `main`** (upstream
   `origin/main`), иначе следующее обычное обновление TS-агента снова потянуло бы `alpha`.
6. **Мост упал до сторожа** (не скачалось, сумма, `daemon-reload`) — цепочка остановилась до
   перезапуска, TS-агент работает, но чекаут остаётся на `alpha` с upstream `origin/alpha`: каждое
   следующее обычное обновление повторит мост nightly. Вернуть на `main` — SelfUpdate с
   `x-agent-channel: stable`.
7. **Клон `--single-branch`** (не `install.sh`: тот клонирует целиком) не считает `origin/alpha`
   удалённой веткой: `git branch --set-upstream-to` падает, цепочка останавливается на локальной
   `alpha` без upstream, и обычное обновление после этого падает на `@{u}`. Чинится тем же
   SelfUpdate с `x-agent-channel: stable` (ветка `main` у такого клона есть).

Вернуть Go-узел с nightly на stable до слияния нельзя: в `releases/latest` нет Go-вложений,
скачивание падает, узел остаётся на nightly (update.md).

### A.1 Перед началом

1. CI `go` на последнем коммите `alpha` зелёный, задание `nightly` прошло.
2. Проверить пре-релиз: `gh release view nightly -R Ametas/route-agent`. Там должны быть три
   вложения, а в заметке — нужный коммит.
3. Пока ветка не слита, `install-go.sh` в `alpha` по умолчанию берёт канал `nightly`
   (`DEFAULT_CHANNEL`). Это нужно для обёртки установки из оркестратора: передать `--channel` она
   не может.

### A.2 Вариант 1: перевести стоящий TS-узел (по SSH)

```bash
curl -fsSL "https://raw.githubusercontent.com/Ametas/route-agent/alpha/install-go.sh?v=$(date +%s)" -o /tmp/install-go.sh
sudo bash /tmp/install-go.sh --convert --channel nightly
```

То же одной строкой: `curl -fsSL "…/install-go.sh?v=$(date +%s)" | sudo bash -s -- --convert --channel nightly`.
Файл удобнее: он понадобится для отката.

Что делает `--convert`:
1. Скачивает и проверяет бинарник, ставит его в `bin/`, канал — в `bin/channel`. TS-агент при
   этом работает и `bin/` не использует. Ошибка на этом шаге: выход с ненулевым кодом, на узле
   ничего не изменилось.
2. Сохраняет юнит TS как `/etc/systemd/system/route-agent.service.ts-backup`.
3. Снимает `/etc/sudoers.d/route-agent-ufw` (architecture §6 п. 2): Go-агент работает от root и
   зовёт `ufw` без `sudo`.
4. `systemctl stop route-agent` → юнит Go → `daemon-reload` → `enable` → `start`.
5. Ждёт до 30 с, пока юнит не станет `active` и порт из `.env` не начнёт слушаться пять секунд
   подряд. Не дождался: показывает журнал Go-агента, возвращает юнит TS, запускает TS-агента
   (его `dist/` не тронут), пишет «TypeScript agent restored…» и выходит с ненулевым кодом.

`.env` и сертификаты при этом не трогаются: Go-агент читает тот же файл (spec A §2.2).

### A.2 Вариант 2: новый узел через оркестратор

1. В боте: «Смена URL-адреса инсталлятора нод» (настройка `node_installer_url`) →
   `https://raw.githubusercontent.com/Ametas/route-agent/alpha/install-go.sh`.
2. Обычная установка узла из оркестратора. Обёртка запустит
   `install-go.sh --secret <бандл> --port <порт>`. Скрипт:
   - ставит пакеты как `install.sh`, но без git, Node и npm, зато с `jq` (разбор бандла);
   - пишет `.env` (0600) и сертификаты (ключ 0600);
   - ставит бинарник и юнит, запускает агента и проверяет его так же, как в варианте 1.
3. **Вернуть URL инсталлятора на `main/install.sh`.** Иначе все следующие узлы встанут на nightly.

Отличия от `install.sh`, все намеренные:
- нет sudoers-правила;
- нет swap-файла: он нужен был только для `npm ci` и сборки;
- существующий `/etc/sing-box/config.json` не перезаписывается (риск R-7).

### A.3 Проверка на узле

```bash
systemctl status route-agent              # ExecStart=/opt/route-agent/bin/route-agent, active
/opt/route-agent/bin/route-agent --version   # v1.4.0-N-gHASH
journalctl -u route-agent -n 50 --no-pager   # JSON-строки slog, строка о прослушивании с хешем договора
ss -ltnp | grep 8081                       # слушает route-agent
```

### A.4 Проверка из оркестратора

- В окне узла (сайт) или карточке узла (бот):
  - версия агента — `v1.4.0-N-gHASH`;
  - договор — `match`: хеш тот же, `21108c0a…ac9f`;
  - плитка — `ok`, то есть телеметрия идёт и нагрузка видна.
- Прогнать действия журнала узла по списку architecture §4 п. 1: быстрые маршруты, Mesh, тест
  сети, Caddy и сертификаты, Sync sing-box, Sync AWG, бинарники, AWG узла, тыл и WARP, пути
  транспорта. Итог каждого действия приходит в системный канал.
- **«Обновить агента» на этом узле обновляет его с nightly:** канал берётся из `bin/channel`.
  Узел, переведённый до 2026-11-07 (без `bin/channel`), считается stable: обновление с
  `releases/latest` тихо не случится (`Failed to execute self-update sequence` в журнале, через
  180 с оркестратор напишет, что агент не вернулся), узел при этом продолжит работать. Такому
  узлу — «Поставить Nightly» или повтор `--convert --channel nightly`.
- Если на `main` вышел новый TS-релиз и кнопка массового обновления подняла цель флота, версия
  nightly (`vX.Y.Z-N-gHASH` от прошлого тега) окажется ниже цели, и догоняющее обновление
  (`agentCatchUp`, раз в 6 ч) будет ставить узлу тот же nightly и перезапускать его. Лечится так:
  влить `main` в `alpha` и дождаться нового nightly.

### A.5 Откат тестового узла

```bash
sudo bash /tmp/install-go.sh --rollback
```

Скрипт возвращает `route-agent.service.ts-backup`, делает `daemon-reload`, запускает TS-агента и
проверяет, что он поднялся. Вручную то же самое:
`cp /etc/systemd/system/route-agent.service.ts-backup /etc/systemd/system/route-agent.service && systemctl daemon-reload && systemctl restart route-agent`.

Бинарник в `/opt/route-agent/bin/` можно оставить, он ничему не мешает, или удалить
(`rm -rf /opt/route-agent/bin`). Снятое sudoers-правило не возвращается: оба агента работают от
root, а root запускает `sudo ufw` (TS) и без него.

## B. Коммит слияния (`alpha` → `main`)

После слияния этот коммит и есть `@{u}`, который подтягивает старый SelfUpdate на `main`:
`git fetch --all --tags && git reset --hard @{u} && git clean -fd && npm ci && npm run build && systemctl restart route-agent`.
Поэтому у него, кроме удаления TypeScript, есть обязательные свойства.

1. **`package.json` остаётся, `dependencies` и `devDependencies` в нём без изменений.** Меняется
   только `scripts`:
   ```json
   "scripts": {
     "build": "bash scripts/bridge-to-go.sh --channel stable",
     "start": "node dist/index.js"
   }
   ```
   Зачем держать зависимости: `npm ci` сначала удаляет `node_modules`. Старый `dist/index.js`
   вынес зависимости наружу (tsup externalizes `dependencies`), то есть без `@grpc/grpc-js`,
   `pino` и прочих он не запустится. Пустой `package.json` оставил бы TS-агента без
   `node_modules`: тогда при сбое моста (цепочка остановилась, старый процесс ещё жив) следующий
   перезапуск или откат сторожем уйдёт в цикл перезапусков.
2. **`package-lock.json` байт в байт прежний.** `npm ci` сверяет зависимости с lock, а скрипты
   не сверяет.
3. **`.gitignore`:** `dist/` и `node_modules/` остаются (иначе `git clean -fd` сотрёт старого
   агента, и откатываться станет некуда). Добавляются `/bin/` и `/active_ports.json`: на откаченном
   узле следующий TS-SelfUpdate не будет их стирать (R-4).
4. **Удаляются:** `src/`, `tests/`, `tsconfig.json`, `tsup.config.ts`,
   `scripts/check-proto-sync.js`; `.env.example`
   переписывается под Go (те же переменные, spec A §2.2). **Остаётся `proto/agent.proto`**: из
   него сгенерирован Go-код, а откаченный TS-агент читает его во время работы.
5. **`install.sh` ← `install-go.sh`** (`git mv`, старый `install.sh` уходит),
   `DEFAULT_CHANNEL="stable"`. Ссылки обновить: шапка `scripts/bridge-to-go.sh`, список
   `SCRIPTS` и вызовы в `scripts/test-delivery.sh`, этот документ. Оркестратор менять не нужно:
   умолчание `node_installer_url` — `main/install.sh`.
6. **`uninstall.sh`** уже убирает `route-agent.service.ts-backup` и сторож моста. Каталог
   `/opt/route-agent` с `bin/` он и так удаляет целиком.
7. **`release.yml`:** задание `check` переключается на Go: gofmt, `go vet`, `go test -race`,
   сверка сгенерированного кода с proto и `bash scripts/test-delivery.sh`, то есть шаги `check` из
   `go.yml`. Задания `release` и `go-assets` остаются как есть. `go.yml` остаётся: он собирает
   nightly из `alpha`.
8. **README.md:** установка (`curl -fsSL …/main/install.sh | sudo bash -s -- --secret … --port …`),
   перевод TS-узла (`--convert`), обновление (SelfUpdate из релиза), откат (`route-agent.prev`,
   `--rollback`), сборка (`scripts/build-release.sh`).
9. **Тип коммита обязан дать релиз** — `feat!` → `v2.0.0` (согласовано владельцем 2026-11-07).
   Мост и Go-SelfUpdate на stable берут бинарник с `releases/latest/download`. Если релиза нет,
   у последнего релиза нет Go-вложений: мост падает на скачивании, и узлы остаются на TS. Это
   безопасно, но перехода не будет.
10. **`alpha` после слияния остаётся nightly.** В её `package.json` `build` — с
    `--channel nightly`, в `install-go.sh` — `DEFAULT_CHANNEL="nightly"`: при вливании `main` в
    `alpha` эти две строки остаются за `alpha`. С этого релиза Go-узел на nightly можно вернуть
    на stable (SelfUpdate с `x-agent-channel: stable`).

После слияния, до любого обновления узлов:

```bash
gh run list -R Ametas/route-agent -w ci -L 1     # release и go-assets зелёные
curl -fsSL https://github.com/Ametas/route-agent/releases/latest/download/checksums.txt
curl -fsSLO https://github.com/Ametas/route-agent/releases/latest/download/route-agent-linux-amd64
sha256sum -c --ignore-missing <(curl -fsSL https://github.com/Ametas/route-agent/releases/latest/download/checksums.txt)
chmod +x route-agent-linux-amd64 && ./route-agent-linux-amd64 --version   # = тег релиза
```

Если `go-assets` упало, нажмите «Re-run failed jobs»: выход задания `release` сохраняется. «Re-run
all jobs» релиза уже не сделает, и вложения не появятся.

**Пока весь флот не на Go, в `main` действуют инварианты:** `package.json` и lock с теми же
зависимостями, `"build": "bash scripts/bridge-to-go.sh --channel stable"`, `dist/` и
`node_modules/` в `.gitignore`, и у каждого релиза есть Go-вложения (в `alpha` — то же с
`--channel nightly`). Когда оркестратор покажет все узлы на версии не ниже релиза слияния,
отдельный коммит удалит `package.json`, lock, мост и строки про `ts-backup` в обеих ветках.
Пре-релиз `nightly` остаётся: это канал nightly.

### Что делает мост на узле

`npm run build` → `scripts/bridge-to-go.sh` (cwd `/opt/route-agent`, от root):
1. скачивает бинарник своей архитектуры и `checksums.txt` канала (`--channel stable` —
   `releases/latest/download`, `nightly` — пре-релиз `nightly`), сверяет SHA-256 и форму `--version`;
2. ставит бинарник в `bin/`, канал — в `bin/channel`;
3. сохраняет юнит TS как `route-agent.service.ts-backup`, а коммит до обновления (`ORIG_HEAD`) —
   в `bin/ts-rollback-commit`, и пишет юнит Go;
4. делает `systemctl daemon-reload`. Если он не прошёл, мост возвращает прежний юнит;
5. снимает sudoers-правило (если root обходится без него) и ставит сторожа
   `route-agent-bridge-guard` (`systemd-run`, через 120 с).

Любой сбой в шагах 1–4 даёт ненулевой выход с прежним юнитом. Цепочка останавливается до
`systemctl restart`, и старый агент продолжает работать. После успеха перезапуск из цепочки
поднимает Go-агента.

Сторож через 120 с смотрит `ActiveState` и `NRestarts`. Если агент не `active` или перезапускался
3 раза и больше, сторож:
- возвращает юнит TS;
- возвращает чекаут на `bin/ts-rollback-commit`, чтобы TS-агент показал прежнюю версию, а не тег
  слияния, и оркестратор увидел, что перехода не было. Чекаут не на `main` (узел шёл на nightly,
  ветка `alpha`) возвращается на `main` с upstream `origin/main`, если тот известен;
- перезапускает TS-агента.

Журнал сторожа: `journalctl -u route-agent-bridge-guard`.

## C. Раскатка по флоту

0. **До слияния** проверить, кого обновит догоняющее обновление. `agentCatchUp` на первом пакете
   телеметрии после переподключения сам вызывает SelfUpdate у узла, если у того расходится
   договор или версия ниже цели флота (`agent_fleet_target_version`). После слияния такой узел
   перейдёт на Go без кнопки. Лучше, чтобы к слиянию все узлы были на цели. Если нет, это надо
   осознанно принять.
1. **Слияние** (§B), затем проверка вложений релиза (команды выше). Сами узлы не обновятся:
   массовое обновление запускается только кнопкой, а цель флота записывает
   `recordFleetTarget` в момент нажатия.
2. **Один боевой узел**: окно узла → «Обновить агента», то есть SelfUpdate старого агента.
   Ожидаемо:
   - ответ «Self-update sequence initiated…»;
   - через 1 с `git` → `npm ci` → мост → `systemctl restart`;
   - Go-агент переподключается, оркестратор в пределах 180 с сверяет `GetAgentInfo`: версия = тег
     релиза, договор `match`.

   Мост тратит на скачивание столько, сколько займут ~15 МБ с GitHub, обычно секунды. Если
   оркестратор не дождался, смотрите на узле:
   - `journalctl -u route-agent -n 100` — строки `bridge-to-go:` и ошибки старого агента;
   - `systemctl cat route-agent` — какой юнит стоит;
   - `journalctl -u route-agent-bridge-guard` — что решил сторож.
3. **Сутки наблюдения** за этим узлом:
   - телеметрия и предупреждения узла;
   - действия журнала;
   - выдача абонентам (онлайн на узле не упал);
   - нет перезапусков: `systemctl show route-agent -p NRestarts`.
4. **Остальные узлы** — по одному из окна узла или кнопкой массового обновления. Кнопка записывает
   цель флота = релиз слияния, и лежавшие во время раскатки узлы догонят её сами, когда
   поднимутся (`agentCatchUp`).
5. **Откат узла после перехода:**
   - назад на прошлый Go-бинарник: `mv /opt/route-agent/bin/route-agent.prev /opt/route-agent/bin/route-agent && systemctl restart route-agent`;
   - назад на TS: шапка `scripts/bridge-to-go.sh`, то же, что делает сторож.

   После отката на TS узел снова перейдёт на Go при следующем «Обновить агента». Пока причина не
   найдена, такой узел не трогайте.
