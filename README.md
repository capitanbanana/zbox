# agents

Обвязка на Go для Claude Code и Codex CLI внутри Linux VM на Mac. На хосте
остаются терминал, VPN и host application. VM получает только явно подключённые папки;
выбранные домены доступны через SOCKS5 на Mac.

## Установка

Требуются macOS с поддержкой Lima VZ, Lima 2.1+ и Go 1.25+ для сборки.

```sh
brew install lima go
make test
make install
```

Устанавливаются два бинарника: `agents` для Mac и `agents-linux-arm64`
(либо `amd64`) для VM. Гостевой бинарник должен лежать рядом с основным;
альтернатива — `AGENTS_GUEST_BINARY` to the guest helper executable.
Внешних Go-зависимостей нет.

## Быстрый старт

```sh
agents create                      # 4 CPU, 8 GiB RAM, 60 GiB disk; ещё не запускает
agents mounts add HOST_DIRECTORY    # RW -> GUEST_DIRECTORY
agents network add '*.example.com' example.com
agents start                       # установка CLI и инструментов при первом старте

cd "$PROJECT_DIR"
agents claude .
agents codex . --model MODEL
agents codex . login --device-auth

agents status
agents stop
agents delete                      # удаляет VM, её auth/history и конфиг; НЕ папки хоста
```

Авторизация выполняется **внутри VM** отдельно для каждого harness. Хостовые
`MANAGED_DIRECTORY`, `MANAGED_DIRECTORY`, `MANAGED_DIRECTORY`, ключи и переменные окружения не копируются.
Для браузерной авторизации используйте URL из CLI / device auth. Автоматический
проброс OAuth callback-портов отключён вместе с остальным автопробросом портов.

При первом `start` Ubuntu устанавливает git, ripgrep, tmux, jq, Python/venv,
build-essential, официальный Node 22/npm (проверяя SHA-256 архива) и оба harness
через npm. Нужен интернет. Версии
harness берутся из npm на момент первоначальной установки; они не обновляются
при каждом запуске. Обновить вручную:

```sh
agents shell npm install -g @openai/codex @anthropic-ai/claude-code
```

Состояние по умолчанию находится в `MANAGED_DIRECTORY`; Lima использует отдельный
`MANAGED_DIRECTORY`, не затрагивая другие Lima VM.

```sh
agents --vm work create --cpus 6 --memory 12 --disk 100
agents --vm work codex .
agents --state-dir "$STATE_DIR" status
```

Глобальные флаги идут **до команды**. `AGENTS_HOME` задаёт альтернативный каталог
состояния. Каждый VM-профиль имеет отдельные папки, network/MCP, credentials и
гостевой диск. Claude и Codex в одной VM имеют общий доступ к этой VM.

## Сессии и аргументы

```sh
agents claude . --resume
agents codex "$PROJECT_DIR" exec 'run the tests'
agents codex . -- exec 'argument with spaces; $(this is literal)'
agents ssh
agents ssh git --version
agents --vm work ssh
```

`agents ssh` запускает VM при необходимости и открывает интерактивный Bash
через SSH. Аргументы после команды выполняются внутри VM, например
`agents ssh git --version`; для shell-выражений используйте
`agents ssh bash -lc 'pwd && ls'`. Proxy env и управляемый `CODEX_HOME`
подключаются автоматически. `agents shell` остаётся алиасом этой команды.

Первый аргумент после `claude` / `codex` — папка на Mac. Она монтируется, если
ещё не экспортирована, после чего VM запускается и harness работает в этой
директории. Оставшиеся аргументы передаются без переинтерпретации локальным
shell; один разделитель `--` сразу после папки можно опустить. Код завершения
harness передаётся вызывающему процессу. В интерактивном терминале SSH выделяет
TTY, поэтому обычные интерфейсы Claude/Codex сохраняются.

Повторный запуск в уже работающей VM не перезапускает прокси или другие сессии.
`agents stop` останавливает все процессы VM. Сессии напрямую подключены через
SSH; автоматического tmux/reconnect нет. Для долгих сессий можно использовать
`agents shell tmux new -A -s work` (harness тогда запускается вручную внутри VM).

## Папки

```sh
agents mounts list
agents mounts add "$HOST_DIR"                 # Default guest destination, RW
agents mounts add "$HOST_DIR" "$GUEST_DIR" --ro
agents mounts remove "$HOST_DIR"
```

Если destination отсутствует, используется `MANAGED_DIRECTORY`,
то есть `MANAGED_DIRECTORY`. Относительный destination считается от
`MANAGED_DIRECTORY`. `~` в destination нужно заключать в кавычки, чтобы shell
на Mac не подставил **хостовый** home.

Для защиты инструментов VM разрешены назначения ниже `MANAGED_DIRECTORY` и `/mnt/`.
Перекрывающиеся mounts и одинаковые destinations запрещены. Запуск harness
в подпапке уже подключённого каталога использует существующий mount.
Для смены RO/RW удалите mount и добавьте заново.

**Изменение virtiofs mounts требует перезапуска VM.** По умолчанию команда
откажется прерывать работающую VM. Можно сначала остановить её или явно:

```sh
agents --restart claude "$PROJECT_DIR"
agents mounts add "$HOST_DIR" --restart
agents mounts remove "$HOST_DIR" --restart
```

Restart завершает **все** текущие сессии данной VM. Подключения сохраняются
между запусками, пока не сделан `mounts remove`. Запись в RW-mount изменяет
оригиналы на хосте. RO ограничивает запись, но разрешает чтение всех файлов
внутри экспортированного дерева, включая скрытые файлы.

## Сеть и VPN

```sh
agents network
# network> + *.example.com
# network> + git.corp.example
# network> - git.corp.example
# network> list
# network> quit

agents network add '*.example.com' example.com git.corp.example
agents network remove git.corp.example
agents network list
```

Это **split routing**, а не сетевой firewall:

* Совпавший hostname → гостевой прокси → SSH-туннель → SOCKS5 на Mac → VPN/сеть Mac.
* Остальные hostname → обычное TCP-соединение из VM.
* DNS для совпавших hostname разрешается на Mac; имя передаётся через SOCKS5.
* `*.example.com` включает поддомены любой глубины, но **не** `example.com`.
* Правила принимают hostname / IP; URL, порт и произвольные glob не принимаются.
* Разрешённое имя открывает **все TCP-порты** этого имени через хостовый прокси.
* Изменения применяются к **новым** соединениям сразу. Открытые соединения не
  обрываются при удалении правила.
* Нет UDP ASSOCIATE, перехвата DNS, TLS MITM или прозрачного TUN.

Harness и дочерние команды получают `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`
(и lowercase-аналоги). HTTP CONNECT нужен в том числе для Claude, у которого
нет прямой поддержки SOCKS. `localhost`, `127.0.0.1`, `::1` исключены через
`NO_PROXY`. Программы, игнорирующие proxy env, идут напрямую. Для SSH/Git SSH
может потребоваться собственный ProxyCommand; HTTPS Git использует HTTP proxy.

В госте HTTP proxy слушает `127.0.0.1:18080`, SOCKS5 — `127.0.0.1:18082`.
Хостовые endpoints доступны внутри VM через SSH reverse forwarding на
`127.0.0.1:18083` (SOCKS) и `127.0.0.1:18084` (read-only policy).
На Mac серверы слушают случайные loopback-порты, требуют токен и повторно
проверяют allowlist. Токен даёт право использовать список, но не изменять его.
При недоступной политике гостевой прокси возвращает ошибку и не делает
fallback напрямую. Прямые соединения других программ этим не ограничиваются.

Обычная сеть Lima сама может наследовать маршруты VPN и доступ к сервисам Mac.
В Lima 2.1 default user networking также предоставляет доступ к host loopback.
Эта версия обвязки **не блокирует** такие пути и не обещает, что перечисленные
имена — единственные доступные из VM адреса. SOCKS-прокси блокирует назначения
loopback/link-local/multicast; для хостового MCP используется отдельный туннель.

Лог направленных через Mac соединений: `MANAGED_DIRECTORY`.
Содержимое запросов и токены в лог не пишутся; hostname/port пишутся.

## MCP

```sh
# stdio: процесс и все его инструменты работают В VM
agents mcp add SERVER_NAME -- SERVER_COMMAND ARGUMENTS

# удалённый Streamable HTTP MCP
agents mcp add SERVER_NAME --url "$REMOTE_MCP_URL"

# уже работающий сервер на Mac, например host application; свой фактический порт/URL
agents mcp add SERVER_NAME --url "$HOST_MCP_URL" --host

agents mcp list
agents mcp disable SERVER_NAME
agents mcp enable SERVER_NAME
agents mcp remove SERVER_NAME
```

`--host` открывает SSH-туннель на указанный loopback TCP-порт Mac. Это доступ
**ко всему порту**, а не только к URL path или выбранным MCP tools. Сервер на
хосте обвязка не устанавливает и не запускает. HTTP(S) endpoints и stdio
поддерживаются; legacy SSE, произвольные headers и импорт host credentials — нет.
Программы для stdio должны быть установлены внутри VM.

Хостовый daemon замечает изменения туннелей примерно за секунду и переподключает
общий SSH-туннель; существующие proxy/MCP-соединения могут оборваться. Изменения
MCP-конфигурации harness применяются в новой сессии.

Claude запускается с `--strict-mcp-config` и сгенерированным JSON.
Codex использует отдельный `MANAGED_DIRECTORY`:
credentials/history сохраняются, а `config.toml` генерируется обвязкой. Настройки
Codex передавайте аргументами `-c key=value`. Это исключает неочевидное сохранение
старых MCP из обычного user configuration при слиянии таблиц. Проектные
настройки/плагины harness и действия самого агента всё ещё могут добавлять
возможности внутри VM; этот реестр не является policy enforcement для harness.

**host application:** доступ к MCP с выполнением C# либо RW-доступ к проекту, открытому
в редакторе на Mac, позволяет исполнять код с правами host application. VM вокруг CLI
не ограничивает этот код. `agents status` напоминает об этой границе.

## Что изолировано

Нет автоматического home mount, SSH-agent forwarding, импорта SSH public keys,
копирования host auth, проброса произвольных guest ports или Docker socket.
Конфигурация обвязки и её родительские директории не могут быть экспортированы
через `mounts`. Хостовый управляющий сокет не передаётся в VM. Мутации состояния
сериализуются файловой блокировкой, config заменяется атомарно с правами `0600`.

Гостевая среда считается доверенной только в пределах VM: у Lima-пользователя
есть sudo; агент может менять настройки гостя. Защитная граница файлов — VM и
список экспортов, а не промпт/AGENTS.md или гостевой firewall. Подключённые RW
папки, исполняемые хостом файлы, сеть Mac и явно предоставленный MCP — реальные
исключения из этой границы.

## Разработка и проверка

```sh
make test     # race detector + unit/integration tests на loopback + go vet
make build
./bin/agents help
```

Тесты покрывают границы wildcard, валидацию mounts, quoting аргументов, блокировки,
интерактивное обновление политики, SOCKS auth/CONNECT/отказ UDP, HTTP proxy и
буферизованные CONNECT-данные, смену маршрута без рестарта и отсутствие direct
fallback при недоступной политике.

Скрипт `scripts/smoke.sh` создаёт отдельную временную настоящую Lima VM,
проверяет CLI, RW/RO mounts, прокси и lifecycle, затем удаляет её. Он скачивает
Ubuntu и пакеты, поэтому требует интернет и занимает несколько минут.

Документация: [Lima mounts](https://lima-vm.io/docs/config/mount/),
[Lima networking](https://lima-vm.io/docs/config/network/user/),
[Claude proxy](https://code.claude.com/docs/en/network-config),
[Claude MCP](https://code.claude.com/docs/en/mcp).
