#!/bin/sh
# shellcheck disable=SC2015 # "a && b || die" is meant: die when either fails.
# ITMO.Widgets agent installer for Linux, macOS and FreeBSD, for the current user (root only for the FreeBSD service):
#   curl -fsSL https://raw.githubusercontent.com/alllexey-dev/itmo-widgets-agent/main/install.sh | sh
#   fetch -qo - https://raw.githubusercontent.com/alllexey-dev/itmo-widgets-agent/main/install.sh | sh   # FreeBSD
# On a terminal with no flags it shows the state and a menu. Without a terminal it installs and starts the service.
# Flags (curl ... | sh -s -- FLAGS): --install, --pair, --service on|off|restart, --status, --unpair, --uninstall,
# --no-service, --version vX.Y.Z (or IW_VERSION), --yes. A re-run of --install upgrades in place.
# Every action checks its own errors: under `( action ) || ...` the shell ignores set -e.
set -u

REPO=alllexey-dev/itmo-widgets-agent
NAME=itmo-widgets-agent
LABEL=dev.alllexey.itmo-widgets-agent
BIN_DIR="$HOME/.local/bin"
BIN="$BIN_DIR/$NAME"
UNIT="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/$NAME.service"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
MAC_LOG="$HOME/Library/Logs/$NAME.log"
RC_NAME=itmo_widgets_agent
RC="/usr/local/etc/rc.d/$RC_NAME"
BSD_LOG="/var/log/$RC_NAME.log"
# Test-only: a local server with VERSION/<release assets> and VERSION/deploy/ instead of GitHub.
RELEASE_BASE="${IW_RELEASE_BASE:-https://github.com/$REPO/releases/download}"
DEPLOY_BASE="${IW_RELEASE_BASE:-https://raw.githubusercontent.com/$REPO}"
# Test-only: a file with the answers instead of the terminal.
TTY="${IW_TTY:-/dev/tty}"

usage="Флаги: --install, --pair, --service on|off|restart, --status, --unpair, --uninstall, --no-service, --version X, --yes"
version="${IW_VERSION:-}"
action=""
service=1
yes=0
mods=0
while [ $# -gt 0 ]; do
	case "$1" in
	--install | --pair | --status | --unpair | --uninstall) action="${1#--}" ;;
	--service)
		case "${2:-}" in on | off | restart) action="service-$2"; shift ;; *) echo "--service: on, off или restart" >&2; exit 2 ;; esac ;;
	--version) [ $# -ge 2 ] || { echo "--version: нужна версия, например v0.1.0" >&2; exit 2; }; version="$2"; mods=1; shift ;;
	--no-service) service=0; mods=1 ;;
	--yes | -y) yes=1; mods=1 ;;
	-h | --help) echo "$usage"; exit 0 ;;
	*) printf 'неизвестный флаг: %s\n%s\n' "$1" "$usage" >&2; exit 2 ;;
	esac
	shift
done

tty=0
(: <"$TTY") 2>/dev/null && tty=1
if [ "$tty" = 1 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-dumb}" != dumb ] && [ -t 1 ]; then
	B=$(printf '\033[1m') G=$(printf '\033[32m') R=$(printf '\033[31m') Y=$(printf '\033[33m') N=$(printf '\033[0m')
else
	B="" G="" R="" Y="" N=""
fi
say() { printf '%s\n' "$*"; }
warn() { printf '%s%s%s\n' "$Y" "$*" "$N"; }
die() { printf '%sОшибка: %s%s\n' "$R" "$*" "$N" >&2; exit 1; }

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
FreeBSD) os=freebsd ;;
*) die "поддерживаются Linux, macOS и FreeBSD; для Windows есть install.ps1" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "архитектура $(uname -m) не поддерживается: есть amd64 и arm64" ;;
esac
command -v curl >/dev/null || command -v fetch >/dev/null || die "нужен curl"

# get URL FILE downloads with curl, or with fetch(1) from the FreeBSD base system.
get() { if command -v curl >/dev/null; then curl -fsSL -o "$2" "$1"; else fetch -qo "$2" "$1"; fi; }

tmp=$(mktemp -d) || die "не удалось создать временную папку"
trap 'rm -rf "$tmp"' EXIT

has_systemd() { command -v systemctl >/dev/null && systemctl --user show-environment >/dev/null 2>&1; }
installed() { [ -x "$BIN" ] && "$BIN" version 2>/dev/null; }
paired() { [ -x "$BIN" ] && "$BIN" status 2>/dev/null | grep -q '^Токен агента: есть'; }

# service_state prints none, running or stopped.
service_state() {
	case "$os" in
	darwin)
		[ -f "$PLIST" ] || { echo none; return; }
		launchctl print "gui/$(id -u)/$LABEL" 2>/dev/null | grep -q 'state = running' && echo running || echo stopped ;;
	freebsd)
		[ -f "$RC" ] || { echo none; return; }
		pgrep -f "$BIN run" >/dev/null && echo running || echo stopped ;;
	*)
		[ -f "$UNIT" ] || { echo none; return; }
		has_systemd && systemctl --user is-active --quiet "$NAME" && echo running || echo stopped ;;
	esac
}

# latest prints the pinned version or the newest release, prereleases included (/releases/latest skips them).
latest() {
	[ -n "$version" ] && { echo "$version"; return; }
	api="https://api.github.com/repos/$REPO/releases?per_page=1"
	if command -v curl >/dev/null; then
		code=$(curl -sSL -o "$tmp/releases.json" -w '%{http_code}' -H 'Accept: application/vnd.github+json' "$api") || code=000
	else
		fetch -qo "$tmp/releases.json" "$api" 2>/dev/null && code=200 || code="ошибка fetch"
	fi
	if grep -q 'rate limit' "$tmp/releases.json" 2>/dev/null; then
		die "GitHub API: лимит запросов без токена исчерпан (60 в час на IP). Повторите через час или укажите версию: --version v0.1.0"
	fi
	[ "$code" = 200 ] || die "GitHub API не ответил ($code): нет сети или исчерпан лимит 60 запросов в час. Укажите версию: --version v0.1.0"
	v=$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/releases.json" | head -n 1)
	[ -n "$v" ] || die "на GitHub нет ни одного релиза"
	echo "$v"
}

confirm() {
	[ "$yes" = 1 ] && return 0
	[ "$tty" = 1 ] || die "$1 Без терминала добавьте --yes"
	printf '%s [y/N] ' "$1"
	read -r answer <"$TTY" || return 1
	case "$answer" in y | Y | д | Д | yes | да) return 0 ;; *) return 1 ;; esac
}

# ask_yes is a question that defaults to yes; it needs a terminal.
ask_yes() {
	printf '%s [Y/n] ' "$1"
	read -r answer <"$TTY" || return 1
	case "$answer" in n | N | н | Н | no | нет) return 1 ;; *) return 0 ;; esac
}

# as_root runs the commands (one per line) as root: directly, or through sudo or doas once the user agrees.
# Otherwise it prints them for the user and fails.
as_root() {
	[ "$(id -u)" = 0 ] && { sh -ec "$1"; return; }
	su=$(command -v sudo || command -v doas) || su=""
	if [ -n "$su" ] && { [ "$yes" = 1 ] || [ "$tty" = 1 ]; } && say "Нужны права root, команды:" && say "$1" &&
		confirm "Выполнить их через ${su##*/}?"; then
		"$su" sh -ec "$1"
		return
	fi
	warn "Нужны права root. Выполните сами (через sudo, doas или su -m root -c):"
	say "$1"
	return 1
}

# get_deploy FILE downloads deploy/FILE of the installed release (main for releases older than the file) to $tmp/service.
get_deploy() {
	v=$(installed) || die "агент не установлен"
	case "$v" in v*) ;; *) v="v$v" ;; esac
	deploy_url="$DEPLOY_BASE/$v/deploy/$1"
	get "$deploy_url" "$tmp/service" 2>/dev/null && return
	deploy_url="$DEPLOY_BASE/main/deploy/$1"
	get "$deploy_url" "$tmp/service" || die "не удалось скачать deploy/$1"
}

install_binary() {
	v=$(latest) || exit 1
	asset="$NAME-$os-$arch"
	say "Скачиваем $asset $v"
	get "$RELEASE_BASE/$v/$asset" "$tmp/$asset" || die "не удалось скачать $RELEASE_BASE/$v/$asset"
	get "$RELEASE_BASE/$v/SHA256SUMS" "$tmp/SHA256SUMS" || die "не удалось скачать SHA256SUMS"
	want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
	if command -v sha256sum >/dev/null; then
		got=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
	elif command -v shasum >/dev/null; then
		got=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
	else
		got=$(sha256 -q "$tmp/$asset")
	fi
	[ -n "$want" ] && [ "$want" = "$got" ] || die "контрольная сумма $asset не совпала с SHA256SUMS, файл не установлен"
	chmod 755 "$tmp/$asset" && mkdir -p "$BIN_DIR" || die "не удалось создать $BIN_DIR"
	[ "$os" = darwin ] && xattr -d com.apple.quarantine "$tmp/$asset" 2>/dev/null
	# Copy next to the target, then rename: atomic, and no foreign owner from the temp directory.
	cp "$tmp/$asset" "$BIN.new" && mv -f "$BIN.new" "$BIN" || die "не удалось записать $BIN"
	say "${G}Установлен${N} $BIN ($("$BIN" version))"
	case ":$PATH:" in
	*":$BIN_DIR:"*) ;;
	*) warn "$BIN_DIR нет в PATH. Добавьте в ~/.profile (или ~/.zshrc): export PATH=\"\$HOME/.local/bin:\$PATH\"" ;;
	esac
}

do_pair() {
	[ -x "$BIN" ] || die "агент не установлен"
	[ "$tty" = 1 ] || { warn "Нет терминала для подключения. Выполните потом: $NAME pair"; return 0; }
	# Under curl | sh stdin is the script, so the pairing screen reads the terminal directly.
	"$BIN" pair <"$TTY" || { warn "Подключение не закончено. Повторите: $NAME pair"; return 0; }
}

service_off() {
	if [ "$os" = darwin ]; then
		launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null
		rm -f "$PLIST"
	elif [ "$os" = freebsd ]; then
		as_root "service $RC_NAME onestop || true
sysrc -x ${RC_NAME}_enable ${RC_NAME}_user ${RC_NAME}_bin 2>/dev/null || true
rm -rf $RC $BSD_LOG /var/run/$RC_NAME" || return 0
	else
		[ -f "$UNIT" ] && has_systemd && systemctl --user disable --now "$NAME" 2>/dev/null
		rm -f "$UNIT"
		has_systemd && systemctl --user daemon-reload
	fi
	say "Служба выключена, автозапуска нет."
}

# service_on writes the unit, plist or rc.d script from deploy/ of the installed release with this machine's paths and
# (re)starts it, so it also serves as restart.
service_on() {
	if [ "$os" = linux ] && ! has_systemd; then
		warn "systemd --user недоступен, служба не установлена. Запускайте вручную ($NAME run) или через Docker (см. README)."
		return 0
	fi
	file="$NAME.service"
	[ "$os" = darwin ] && file="$LABEL.plist"
	[ "$os" = freebsd ] && file="$RC_NAME.rc"
	get_deploy "$file" || exit 1
	if [ "$os" = freebsd ]; then
		# rc.d needs root, so root fetches the script itself: the same commands work when the user runs them by hand.
		as_root "fetch -qo $RC $deploy_url
chmod 555 $RC
sysrc ${RC_NAME}_enable=YES ${RC_NAME}_user=$(id -un) ${RC_NAME}_bin=$BIN
service $RC_NAME stop 2>/dev/null || true
service $RC_NAME start" || return 0
		say "${G}Служба rc.d $RC_NAME запущена${N} и стартует при загрузке системы."
		say "Логи: tail -f $BSD_LOG"
		return 0
	fi
	if [ "$os" = darwin ]; then
		# The comment holds the manual steps; drop it with the placeholders.
		sed -e '/<!--/,/-->/d' -e "s|/usr/local/bin/$NAME|$BIN|" -e "s|/Users/YOUR_USER|$HOME|" "$tmp/service" >"$tmp/plist"
		! grep -q YOUR_USER "$tmp/plist" || die "в deploy/$file осталась заглушка YOUR_USER"
		mkdir -p "$(dirname "$PLIST")" "$(dirname "$MAC_LOG")" && mv -f "$tmp/plist" "$PLIST" || die "не удалось записать $PLIST"
		# bootout finishes asynchronously; an immediate bootstrap can fail with error 5.
		launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null && sleep 1
		launchctl bootstrap "gui/$(id -u)" "$PLIST" || die "launchctl не запустил службу: launchctl print gui/$(id -u)/$LABEL"
		say "${G}Служба launchd запущена${N} и стартует при входе в систему."
		say "Логи: tail -f ~/Library/Logs/$NAME.log"
		return 0
	fi
	mkdir -p "$(dirname "$UNIT")" && sed "s|%h/.local/bin/$NAME|$BIN|" "$tmp/service" >"$UNIT" || die "не удалось записать $UNIT"
	systemctl --user daemon-reload && systemctl --user enable --quiet "$NAME" && systemctl --user restart "$NAME" ||
		die "systemd не запустил службу: systemctl --user status $NAME"
	say "${G}Служба systemd --user запущена.${N}"
	if [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null)" != yes ] &&
		! loginctl enable-linger "$(id -un)" 2>/dev/null; then
		warn "Без linger агент остановится, когда вы выйдете из системы. Включите с правами администратора: sudo loginctl enable-linger $(id -un)"
	fi
	say "Логи: journalctl --user -u $NAME -f"
}

do_install() {
	fresh=1
	[ -x "$BIN" ] && fresh=0
	was=$(service_state)
	install_binary || exit 1
	if paired; then say "Телефон уже подключён, pair пропущен."; else do_pair; fi
	# An existing service is restarted without asking. Otherwise a terminal asks; without one a fresh install gets the
	# service and an upgrade keeps the user's choice.
	if [ "$service" = 0 ]; then
		say "Служба не установлена (--no-service). Запуск вручную: $NAME run"
	elif [ "$was" != none ]; then
		service_on
	elif [ "$tty" = 1 ] && [ "$yes" = 0 ]; then
		if ask_yes "Запускать агента в фоне как службу (автозапуск)?"; then
			service_on
		else
			say "Служба не создана. Включить потом: этот скрипт, пункт «Включить службу», или $NAME run вручную."
		fi
	elif [ "$fresh" = 1 ] || [ "$yes" = 1 ]; then
		service_on
	fi
}

show_state() {
	v=$(installed) || v=""
	l=$(latest 2>/dev/null) || l="неизвестна"
	if [ -z "$v" ]; then
		say "Агент:   ${Y}не установлен${N} (последняя версия $l)"
		return 0
	fi
	note="последняя"
	[ "v${v#v}" = "$l" ] || note="${Y}есть $l${N}"
	say "Агент:   $v ($note), $BIN"
	if paired; then
		say "Телефон: ${G}подключён${N}, имя ${IW_AGENT_NAME:-$(uname -n)}"
	else
		say "Телефон: ${Y}не подключён${N}"
	fi
	case "$(service_state)" in
	running) say "Служба:  ${G}работает${N}" ;;
	stopped) say "Служба:  ${R}остановлена${N}" ;;
	*) say "Служба:  не установлена" ;;
	esac
}

do_status() {
	show_state
	[ -x "$BIN" ] || return 0
	[ "$(service_state)" = running ] && say "Экран службы вживую: $NAME attach (q - закрыть, служба продолжит работу)"
	say ""
	"$BIN" status
	[ "$(service_state)" = none ] && return 0
	say ""
	say "Последние строки лога:"
	if [ "$os" = darwin ]; then
		tail -n 30 "$MAC_LOG" 2>/dev/null || say "(лога ещё нет)"
	elif [ "$os" = freebsd ]; then
		tail -n 30 "$BSD_LOG" 2>/dev/null || say "(лога ещё нет)"
	else
		journalctl --user -u "$NAME" -n 30 --no-pager 2>/dev/null || say "(журнал недоступен: journalctl --user -u $NAME)"
	fi
}

do_unpair() {
	[ -x "$BIN" ] || die "агент не установлен"
	confirm "Отключить агента от аккаунта? Токен и сессия ИТМО будут удалены с этой машины." || return 0
	"$BIN" unpair || die "unpair не удался: удалите агент в приложении, Профиль -> «Устройства»"
}

do_uninstall() {
	confirm "Удалить агента полностью: службу, бинарник, токен и сессию ИТМО?" || return 0
	[ "$(service_state)" != none ] && service_off
	dir=""
	if [ -x "$BIN" ]; then
		dir=$("$BIN" status 2>/dev/null | sed -n 's/^Папка: //p')
		"$BIN" unpair || warn "unpair не удался: удалите агент в приложении, Профиль -> «Устройства»"
	fi
	rm -f "$BIN" "$MAC_LOG"
	say "${G}Агент удалён:${N} бинарник, служба, токен и сессия ИТМО."
	[ -n "$dir" ] && [ -d "$dir" ] && say "В $dir остались файлы без секретов (state.json, agent.env); удалите папку, если не нужна."
	return 0
}

menu() {
	# One GitHub API call per run: the unauthenticated limit is 60 an hour.
	[ -n "$version" ] || version=$(latest 2>/dev/null) || version=""
	while :; do
		say ""
		say "${B}ITMO.Widgets agent${N}"
		show_state
		say ""
		ok="1 0"
		say "1. Установить или обновить"
		if [ -x "$BIN" ]; then
			s=$(service_state)
			paired || { say "2. Подключить к телефону (pair)"; ok="$ok 2"; }
			[ "$s" = none ] && { say "3. Включить службу (автозапуск)"; ok="$ok 3"; }
			[ "$s" != none ] && { say "4. Выключить службу"; say "5. Перезапустить службу"; ok="$ok 4 5"; }
			say "6. Статус и логи"
			ok="$ok 6"
			# attach draws a screen: it needs a real terminal for input and output, not an answers file.
			[ "$s" = running ] && [ -c "$TTY" ] && [ -t 1 ] && { say "7. Открыть экран службы (attach)"; ok="$ok 7"; }
			paired && { say "8. Отключить агента (unpair)"; ok="$ok 8"; }
		fi
		[ -x "$BIN" ] || [ "$(service_state)" != none ] && { say "9. Удалить полностью"; ok="$ok 9"; }
		say "0. Выход"
		printf '> '
		read -r choice <"$TTY" || return 0
		case " $ok " in *" $choice "*) ;; *) say "Нет такого пункта."; continue ;; esac
		say ""
		case "$choice" in
		0) return 0 ;;
		1) (do_install) ;;
		2) (do_pair) ;;
		3) (service_on) ;;
		4) (service_off) ;;
		5) (service_on) ;;
		6) (do_status) ;;
		7) "$BIN" attach <"$TTY" || warn "Экран не открылся: $NAME attach" ;;
		8) (do_unpair) ;;
		9) (do_uninstall) ;;
		esac
	done
}

if [ -z "$action" ]; then
	# Bare run on a terminal: the menu. Modifiers alone or no terminal (Docker, CI): install.
	if [ "$tty" = 1 ] && [ "$mods" = 0 ]; then
		menu
		exit 0
	fi
	action=install
fi
case "$action" in
install) do_install ;;
pair) do_pair ;;
service-on) service_on ;;
service-off) service_off ;;
service-restart) service_on ;;
status) do_status ;;
unpair) do_unpair ;;
uninstall) do_uninstall ;;
esac
