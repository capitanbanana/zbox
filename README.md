# agents

Обвязка на Go для Claude Code и Codex CLI внутри Linux VM на Mac. На хосте
остаются терминал и host application. VM получает только явно подключённые папки;
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
Для режима VLESS дополнительно нужен `sing-box` на Mac (проверено с 1.13.11).
Go-зависимости шлюза зафиксированы в `go.mod` / `go.sum`.

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

### Браузер на Mac через сеть VM

```sh
agents socks                       # 127.0.0.1:1080, работает до Ctrl+C
agents --vm work socks --port 1081 # другая VM / другой локальный порт
curl --noproxy '' --proxy socks5h://127.0.0.1:1080 https://example.com
```

Команда при необходимости запускает VM и открывает SSH-туннель к **тому же
гостевому SOCKS5**, который используется агентами. Совпадают split-routing
`network`, DNS и обычный выход VM (VLESS / домашний bypass). Имена передаются
в VM; корпоративные исключения разрешаются через Mac, как у агентов.
Прямого fallback на Mac при отказе туннеля нет. Уже занятый локальный порт
приводит к ошибке. Ctrl+C закрывает этот туннель; VM и сессии агентов остаются.
При остановке/перезапуске VM запустите `agents socks` заново.

В браузере задайте SOCKS5 `127.0.0.1:1080` и DNS через SOCKS. Например, в
[настройках соединения Firefox](https://support.mozilla.org/en-US/kb/connection-settings-firefox)
выберите ручную настройку и SOCKS v5; [DNS для SOCKS v5 должен оставаться
через прокси](https://firefox-admin-docs.mozilla.org/reference/policies/proxy/).
Системные настройки прокси Mac команда не меняет. Cookie, логины и профиль
браузера остаются браузерными: одинаковый сетевой доступ не копирует сессию агента.

Чтобы открывать сервисы на `localhost` **внутри VM**, браузер должен отправлять
и loopback-адреса в прокси. Chrome по умолчанию их обходит; пример отдельного
профиля для проверки локального веб-приложения:

```sh
open -na 'Google Chrome' --args \
  --user-data-dir="$BROWSER_PROFILE" \
  --proxy-server='socks5://127.0.0.1:1080' \
  --proxy-bypass-list='<-loopback>' \
  --host-resolver-rules='MAP * ~NOTFOUND, EXCLUDE 127.0.0.1'
```

Флаги описаны в документации Chromium: [SOCKS и DNS](https://www.chromium.org/developers/design-documents/network-stack/socks-proxy/),
[loopback bypass](https://chromium.googlesource.com/chromium/src/+/HEAD/net/docs/proxy.md).
Через такой профиль `http://127.0.0.1:3000` относится к VM. SOCKS поддерживает
TCP CONNECT; это доступ к сайтам, а не изоляция всего браузера: UDP/WebRTC
и обращения вне его настроек прокси не охватываются. Локальный listener без
пароля доступен процессам на Mac, но не другим машинам в LAN; закройте его,
когда он не нужен.

### Терминал и harness

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

## Выборочные маршруты через Mac

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

Список `network` управляет **split routing** через прикладной прокси:

* Совпавший hostname → гостевой прокси → SSH-туннель → SOCKS5 на Mac → VPN/сеть Mac.
* Остальные hostname → сеть VM: через VLESS, если он включён, иначе обычная сеть Lima.
* DNS для совпавших hostname разрешается на Mac; имя передаётся через SOCKS5.
* `*.example.com` включает поддомены любой глубины, но **не** `example.com`.
* Правила принимают hostname / IP; URL, порт и произвольные glob не принимаются.
* Разрешённое имя открывает **все TCP-порты** этого имени через хостовый прокси.
* Изменения применяются к **новым** соединениям сразу. Открытые соединения не
  обрываются при удалении правила.
* Этот прикладной прокси поддерживает только TCP; UDP через Mac не предоставляется.
  Режим VLESS ниже отдельно охватывает TCP, UDP и DNS всей виртуальной карты.

Harness и дочерние команды получают `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`
(и lowercase-аналоги). HTTP CONNECT нужен в том числе для Claude, у которого
нет прямой поддержки SOCKS. `localhost`, `127.0.0.1`, `::1` исключены через
`NO_PROXY`. Программы, игнорирующие proxy env, используют сеть VM: при включённом VLESS
они тоже идут через VPN, но не применяют исключения `network`. Для SSH/Git SSH
может потребоваться собственный ProxyCommand; HTTPS Git использует HTTP proxy.

В госте HTTP proxy слушает `127.0.0.1:18080`, SOCKS5 — `127.0.0.1:18082`.
Хостовые endpoints доступны внутри VM через SSH reverse forwarding на
`127.0.0.1:18083` (SOCKS) и `127.0.0.1:18084` (read-only policy).
На Mac серверы слушают случайные loopback-порты, требуют токен и повторно
проверяют allowlist. Токен даёт право использовать список, но не изменять его.
При недоступной политике гостевой прокси возвращает ошибку и не делает
fallback напрямую. Прямые соединения других программ ограничиваются только шлюзом VLESS, если он включён.

**Без `agents vpn`** обычная сеть Lima может наследовать маршруты VPN и доступ к сервисам Mac.
В Lima 2.1 default user networking также предоставляет доступ к host loopback.
Без VLESS обвязка **не блокирует** такие пути и не обещает, что перечисленные
имена — единственные доступные из VM адреса. SOCKS-прокси блокирует назначения
loopback/link-local/multicast; для хостового MCP используется отдельный туннель.

Лог направленных через Mac соединений: `MANAGED_DIRECTORY`.
Содержимое запросов и токены в лог не пишутся; hostname/port пишутся.

## VLESS для всей VM и kill switch

```sh
brew install sing-box
agents create
agents vpn add --stdin < "$VLESS_FILE"
agents start
agents vpn status
agents status

agents stop
agents vpn update --stdin < "$VLESS_FILE"
agents start

# Либо явно перезапустить работающую VM, завершив все её сессии:
agents --restart vpn update --stdin < "$VLESS_FILE"

agents stop
agents vpn delete       # осознанно вернуть обычную сеть Lima
```

Можно передать URI аргументом: `agents vpn add 'vless://…'`. `--stdin` позволяет
не сохранять секрет в истории shell и аргументах процесса. Конфиг хранится
в приватном каталоге состояния с правами `0600`; UUID/ключи не выводятся в status.
`AGENTS_SING_BOX` выбирает отдельный бинарник.
Существующие VPN-клиенты Mac и настройки роутера не меняются.

Поддержаны VLESS TCP/raw, WebSocket, gRPC, HTTP и HTTPUpgrade; TLS и REALITY,
включая `sni`, `fp`, `pbk`, `sid`, `alpn`, `serviceName` и Vision для TCP.
Неподдерживаемые параметры (например, XHTTP) отклоняются явно. Проверка TLS
не отключается. При включении проверяется конфигурация установленным `sing-box`.

У VPN-VM **единственная** сетевая карта: выделенный шлюз на Mac. Он стартует
**до загрузки VM**, а стандартная неограниченная карта Lima заменяется.
По умолчанию TCP/UDP пересылаются только через локальный SOCKS клиента VLESS,
без direct outbound и без fallback на сеть Mac. Это охватывает provisioning, процессы
с `sudo` и программы, игнорирующие proxy env. DNS гостя идёт через DoH
Cloudflare, затем Google внутри VLESS; системного DNS fallback нет.
Произвольный UDP, в том числе DNS на заданный IP, также проходит через VLESS.
IPv6 и внешний ICMP блокируются (поэтому `ping` не проверяет доступность VPN).

В режиме VLESS при смерти клиента, обрыве туннеля или отказе шлюза обычный внешний трафик
блокируется. `vpn status` различает состояние процессов и **не** выдаёт живой
процесс за проверенную доступность сервера. После смерти процесса восстановление:
`agents stop`, затем `agents start`. Пока шлюз жив, SSH остаётся доступным.
При падении самого шлюза SSH по виртуальной карте тоже может пропасть.

`agents network` сохраняется как **явное исключение**: совпавшие домены через
гостевой proxy идут по сети Mac и продолжают работать при недоступном VLESS.
Так же сохраняются включённые `mcp --host` туннели. `network` применяется к
программам, использующим proxy env; raw-соединение к тому же IP остаётся в VLESS.
Доступ к host application с выполнением кода всё ещё означает доступ с правами host application на Mac.

Хосту разрешено установить транспортное соединение с самим VLESS-сервером и
разрешить **его** hostname через DNS Mac — это bootstrap туннеля, не обход
для гостевых запросов. Если VPN уже включён на роутере, VLESS проходит внутри
него. Обвязка не меняет эту маршрутизацию; при проблемах с вложенным туннелем
нужен сравнительный тест с исключением адреса VLESS-сервера на роутере.

Интеграция защищённого шлюза рассчитана на **Lima 2.1.x** и отклоняет другие
версии до проверки совместимости. Она также блокирует автозапуск стандартного
usernet после падения шлюза: `leases.json` выделенной сети ссылается на
`/dev/null`, поэтому Lima не может прочитать leases и включить неограниченный
fallback. Этот guard сохраняется при остановке и перезагрузке Mac. Не меняйте
его вручную. Управляйте VM через `agents`; запуск через `limactl` без нашего
шлюза ожидаемо завершается ошибкой `failed to start usernet … EOF`.

Для защиты с **первой** загрузки добавляйте VPN после `create`, до первого
`start`. Включение в уже созданной VM требует остановки или `--restart`.
Удаление VPN намеренно снимает сетевую изоляцию. Ошибка при изменении
конфигурации сохраняет намерение использовать VPN, чтобы следующий запуск
не открыл прямую сеть. Лог клиента/шлюза: `MANAGED_DIRECTORY`.

### Прямой доступ в домашней сети

Находясь дома, сохраните текущую сеть для выбранной VM:

```sh
agents vpn bypass current          # показать сеть Mac; VM создавать необязательно
agents vpn bypass add home         # запомнить текущую сеть
agents vpn bypass list
agents vpn status                  # фактический режим: DIRECT или VLESS
agents vpn bypass delete home      # вернуть VLESS и дома
```

Правила хранятся в `vpn_bypass` конфигурации VM и переживают `vpn update`.
При отключённом VPN правила не действуют. Добавление/удаление применяется
к работающему шлюзу без перезагрузки VM. При запуске шлюз проверяет сеть
**до подключения VM**. Домашняя сеть совпадает по IPv4-подсети, IP шлюза
и MAC шлюза; одного совпадения `192.168.x.0/24` недостаточно. Ethernet и Wi-Fi
с одной такой идентичностью могут использовать одно правило. Если шлюзы у них
разные, сохраните отдельные правила. После замены роутера удалите старое
правило и заново выполните `add` дома.

При совпадении весь обычный TCP/UDP и гостевой DNS выходят через сеть Mac
без дополнительного VLESS обвязки. VPN на домашнем роутере продолжает работать
как прежде. Исключения `network` и MCP сохраняются; IPv6/внешний ICMP по-прежнему
блокируются. Защищённая карта VM остаётся единственной. VLESS-клиент остаётся
запущенным для выхода из дома; его недоступность не мешает прямому режиму дома.
В другой сети или при ошибке определения используется только VLESS, с kill switch.

Сеть проверяется перед каждым новым соединением и примерно раз в секунду;
при смене сети/режима старые соединения закрываются, незавершённые подключения
отменяются. Прямые сокеты привязаны к наблюдаемому IPv4 и интерфейсу Mac.
Приложениям может понадобиться повторить запрос. Проверка сети ограничена двумя
секундами; переключение **не атомарно со сменой маршрута ОС**: существующее
соединение может оставаться открытым до обнаружения смены, в том числе при
роуминге на том же интерфейсе. Подсеть/IP/MAC — распознавание доверенной LAN,
а не криптографическая аутентификация: их можно подделать. Если требуется
безусловный запрет прямых пакетов при любом роуминге, не добавляйте bypass.
В прямом режиме DNS гостя также использует DoH; имена самих DoH-провайдеров
разрешаются системным DNS Mac.

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
fallback при недоступной политике. VPN-тесты проверяют разбор/редакцию VLESS,
TCP/UDP через единственные forwarders, отказ DNS без туннеля, сохранение
VPN-намерения при ошибках, блокировку лишних NIC и fallback guard Lima.

Скрипт `scripts/smoke.sh` создаёт отдельную временную настоящую Lima VM,
проверяет CLI, RW/RO mounts, прокси и lifecycle, затем удаляет её. Он скачивает
Ubuntu и пакеты, поэтому требует интернет и занимает несколько минут.

`python3 scripts/smoke-socks.py` проверяет браузерный SOCKS на отдельной VM:
гостевой localhost, запросы к доменным именам через SOCKS, HTTPS, конфликт порта,
kill switch, живые `network`-исключения и закрытие туннеля по Ctrl+C.
Используется фиктивный недоступный VLESS и исключение текущей LAN только
для тестовой VM; нужны Lima 2.1.x и sing-box. VM удаляется после проверки.

Документация: [Lima mounts](https://lima-vm.io/docs/config/mount/),
[Lima networking](https://lima-vm.io/docs/config/network/user/),
[Claude proxy](https://code.claude.com/docs/en/network-config),
[Claude MCP](https://code.claude.com/docs/en/mcp).

Проверка VPN на настоящей Lima 2.1.1 (macOS arm64): через локальный тестовый
VLESS-сервер выполнены первоначальная установка пакетов, загрузка 5 МБ,
UDP DNS и DNS шлюза. После SIGKILL клиента проверены отказ raw TCP от root,
отказ UDP DNS и SERVFAIL DNS шлюза; `network` и хостовый MCP остались доступны.
После SIGKILL шлюза и остановки VM проверен отказ запуска стандартного usernet
через `limactl` (EOF guard). Это проверка механизма изоляции; работоспособность
конкретного удалённого VLESS зависит также от сервера и сети до него.

Проверка `vpn bypass` на отдельной Lima VM: с заведомо недоступным VLESS
домашнее исключение позволило первоначальную установку пакетов, HTTPS,
raw UDP DNS и DNS шлюза. Удаление правила блокировало raw TCP; повторное
добавление возвращало доступ без рестарта. После SIGKILL VLESS-клиента дома
доступ сохранялся, без домашнего правила трафик блокировался; SSH работал.
Автотесты проверяют также периодическое обнаружение смены сети, закрытие
TCP/UDP, отмену незавершённых подключений и отказ при ошибке определения сети.
Физический роуминг Mac между сетями в этой проверке не выполнялся.
