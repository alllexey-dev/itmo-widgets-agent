# Установка: подробности

Короткая установка одной командой описана в [README](../README.md#установка). Здесь: что делает скрипт,
куда он кладёт файлы в каждой системе, его флаги, установка бинарника вручную и Docker.

## Скрипт установки

Скрипт показывает, что уже есть (версия агента и последняя на GitHub, подключён ли телефон, работает ли
служба), и меню: установить или обновить, подключить к телефону, включить, выключить или перезапустить
службу, статус и логи, открыть экран работающей службы (`attach`, только в терминале), отключить агента
(`unpair`), удалить полностью. Пункты, которые сейчас не имеют смысла, не показываются; отключение и удаление
спрашивают подтверждение.

Установка берёт последний релиз (включая предварительные), сверяет SHA-256 с `SHA256SUMS` и спрашивает,
запускать ли агента в фоне как службу (автозапуск). После установки он сразу переходит к подключению и показывает
QR-код для телефона; без терминала этот шаг пропускается, подключите потом: `itmo-widgets-agent pair`. Если служба уже есть, обновление её просто перезапускает.
Права администратора нужны только для службы на FreeBSD:

- Linux: `~/.local/bin/itmo-widgets-agent`, служба `systemd --user` и `loginctl enable-linger`, чтобы
  агент работал и после выхода из системы. Логи: `journalctl --user -u itmo-widgets-agent -f`.
- macOS: `~/.local/bin/itmo-widgets-agent`, агент launchd `dev.alllexey.itmo-widgets-agent`. Логи:
  `~/Library/Logs/itmo-widgets-agent.log`.
- Windows: `%LOCALAPPDATA%\itmo-widgets-agent`, задача планировщика «ITMO.Widgets agent» при входе в
  Windows, без окна. Логи: `agent.log` в той же папке.
- FreeBSD: `~/.local/bin/itmo-widgets-agent`, скрипт rc.d `/usr/local/etc/rc.d/itmo_widgets_agent`
  (`daemon(8)` от вашего пользователя, включается через `sysrc`). Для этого нужен root:
  скрипт покажет команды и выполнит их через `sudo` или `doas`, только если вы согласитесь, иначе просто
  напечатает их. Логи: `/var/log/itmo_widgets_agent.log`.

Без терминала (Docker, CI, скрипты) меню и вопросов нет: скрипт ставит агент и службу. Действие можно задать
флагом, например `curl -fsSL .../install.sh | sh -s -- --uninstall --yes`. Флаги: `--install`, `--pair`,
`--service on|off|restart`, `--status`, `--unpair`, `--uninstall`, `--no-service`, `--version v0.1.0`
(или `IW_VERSION`), `--yes`. В PowerShell те же параметры с одним дефисом:

```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/alllexey-dev/itmo-widgets-agent/main/install.ps1))) -Uninstall -Yes
```

Повторная установка обновляет агент на месте и перезапускает службу. Удаление останавливает службу,
выполняет `unpair` и удаляет программу и файл службы; в папке настроек остаются только `state.json` и
`agent.env`, без токенов.

## Бинарник вручную

Скачайте файл для своей системы со [страницы релизов](https://github.com/alllexey-dev/itmo-widgets-agent/releases):
`itmo-widgets-agent-<os>-<arch>` для `linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`,
`freebsd-amd64`, `freebsd-arm64` или `windows-amd64.exe`, и рядом `SHA256SUMS`. Переименуйте в `itmo-widgets-agent`, положите в `PATH` и
подключите. Например, macOS на Apple Silicon:

```sh
shasum -a 256 --ignore-missing -c SHA256SUMS    # на Linux: sha256sum, на FreeBSD: sha256 -q <файл>
install -m 755 itmo-widgets-agent-darwin-arm64 /usr/local/bin/itmo-widgets-agent
xattr -d com.apple.quarantine /usr/local/bin/itmo-widgets-agent   # если скачали браузером: бинарник не подписан
itmo-widgets-agent pair
itmo-widgets-agent run
```

Файлы хранятся в `/data`, если такая папка есть, иначе в папке настроек пользователя
(`~/.config/itmo-widgets-agent` на Linux и FreeBSD, `~/Library/Application Support/itmo-widgets-agent` на
macOS). Другую папку задаёт `IW_STATE_DIR`. Примеры службы для автозапуска лежат в `deploy/`:
`launchd` для macOS, `systemd --user` для Linux и rc.d для FreeBSD.

## Docker

```sh
curl -O https://raw.githubusercontent.com/alllexey-dev/itmo-widgets-agent/main/compose.yaml
docker compose run --rm agent pair
docker compose up -d
```

Без токена `run` не завершается, а ждёт, пока `pair` его сохранит: так `restart: unless-stopped` не
перезапускает контейнер по кругу. Обновить вход в ИТМО можно при запущенном контейнере:
`docker compose run --rm agent renew`.
