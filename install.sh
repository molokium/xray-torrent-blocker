#!/usr/bin/env bash
#
# Install a locally built tblocker binary. This script does not install or
# modify a firewall.
#
set -euo pipefail

if [ "${EUID}" -ne 0 ]; then
	echo "Please run this script with root privileges (sudo)." >&2
	exit 1
fi

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
cd "${SCRIPT_DIR}"

BINARY_PATH="${1:-build/tblocker}"
if [ ! -x "${BINARY_PATH}" ]; then
	echo "Binary not found or not executable: ${BINARY_PATH}" >&2
	echo "Build it first with: make build" >&2
	exit 1
fi

INSTALL_DIR=/opt/tblocker
CONFIG="${INSTALL_DIR}/config.yaml"
SERVICE_USER=tblocker
SERVICE_GROUP=tblocker
LOG_FILE=""

# Dedicated unprivileged account. It has no login shell and no capabilities;
# the service only needs to read the node access log and write its journal.
if ! getent group "${SERVICE_GROUP}" >/dev/null 2>&1; then
	groupadd --system "${SERVICE_GROUP}"
fi
if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
	useradd --system --gid "${SERVICE_GROUP}" \
		--home-dir "${INSTALL_DIR}" --no-create-home \
		--shell /usr/sbin/nologin "${SERVICE_USER}"
fi

install -d -m 0750 -o "${SERVICE_USER}" -g "${SERVICE_GROUP}" "${INSTALL_DIR}"
install -m 0755 -o root -g root "${BINARY_PATH}" "${INSTALL_DIR}/tblocker"
install -m 0644 -o root -g root tblocker.service /etc/systemd/system/tblocker.service

# A previous root-run install may have left the journal owned by root.
if [ -f "${INSTALL_DIR}/blocks.json" ]; then
	chown "${SERVICE_USER}:${SERVICE_GROUP}" "${INSTALL_DIR}/blocks.json"
	chmod 0600 "${INSTALL_DIR}/blocks.json"
fi

if [ ! -f "${CONFIG}" ]; then
	install -m 0600 -o "${SERVICE_USER}" -g "${SERVICE_GROUP}" config.yaml.example "${CONFIG}"
	echo
	echo "New configuration created at ${CONFIG}"
	echo "Answer the prompts or press Enter to keep the example value."
	echo

	read -r -p "Xray access log [/var/lib/pg-node/access.log]: " LOG_FILE
	LOG_FILE="${LOG_FILE:-/var/lib/pg-node/access.log}"

	read -r -p "PasarGuard panel URL (no /api): " PANEL_URL
	if [ -z "${PANEL_URL}" ]; then
		echo "Panel URL is required." >&2
		exit 1
	fi

	read -r -s -p "PasarGuard API key (pg_key_...): " API_KEY
	echo
	if [ -z "${API_KEY}" ]; then
		echo "API key is required." >&2
		exit 1
	fi

	read -r -s -p "Telegram admin bot token (blank to disable admin messages): " ADMIN_TOKEN
	echo
	read -r -p "Telegram admin chat ID: " ADMIN_CHAT

	escape_sed() {
		printf '%s' "$1" | sed -e 's/[\\/&|]/\\&/g'
	}

	# Keep secrets out of the sed process command line (world-readable in
	# /proc/<pid>/cmdline). The 0600 script file is only readable by root.
	SED_SCRIPT=$(mktemp)
	trap 'rm -f "${SED_SCRIPT}"' EXIT
	chmod 0600 "${SED_SCRIPT}"

	{
		printf 's|/var/lib/pg-node/access.log|%s|\n' "$(escape_sed "${LOG_FILE}")"
		printf 's|https://panel.example.com|%s|\n' "$(escape_sed "${PANEL_URL}")"
		printf 's|pg_key_00000000-0000-4000-8000-000000000000|%s|\n' "$(escape_sed "${API_KEY}")"
	} > "${SED_SCRIPT}"
	sed -i -f "${SED_SCRIPT}" "${CONFIG}"

	if [ -n "${ADMIN_TOKEN}" ] && [ -n "${ADMIN_CHAT}" ]; then
		{
			printf 's|123456:ADMIN_BOT_TOKEN|%s|\n' "$(escape_sed "${ADMIN_TOKEN}")"
			printf 's|"123456789"|"%s"|\n' "$(escape_sed "${ADMIN_CHAT}")"
		} > "${SED_SCRIPT}"
		sed -i -f "${SED_SCRIPT}" "${CONFIG}"
	else
		printf '%s\n' \
			's|^SendAdminMessage: true|SendAdminMessage: false|' \
			's|^AdminBotToken: ".*"|AdminBotToken: ""|' \
			's|^AdminChatID: ".*"|AdminChatID: ""|' > "${SED_SCRIPT}"
		sed -i -f "${SED_SCRIPT}" "${CONFIG}"
		echo "Admin Telegram messages disabled."
	fi
	rm -f "${SED_SCRIPT}"
	trap - EXIT
else
	echo "Configuration already exists at ${CONFIG}; leaving it unchanged."
fi

# sed -i and install may leave the config root-owned; the service must own it.
chown "${SERVICE_USER}:${SERVICE_GROUP}" "${CONFIG}"
chmod 0600 "${CONFIG}"

if [ -z "${LOG_FILE}" ]; then
	LOG_FILE=$(awk -F'"' '/^LogFile:/ {print $2; exit}' "${CONFIG}")
fi
if [ -z "${LOG_FILE}" ]; then
	LOG_FILE=$(awk '/^LogFile:/ {print $2; exit}' "${CONFIG}")
fi
LOG_FILE=${LOG_FILE%\"}
LOG_FILE=${LOG_FILE#\"}
LOG_FILE=${LOG_FILE%\'}
LOG_FILE=${LOG_FILE#\'}

# Best-effort ACL so the unprivileged service can read the node log without
# making the whole log group or file world-readable. ACLs survive truncation,
# but not log rotation that creates a new inode; reapply after rotation.
grant_log_access() {
	local log_path="$1"

	if [ -z "${log_path}" ] || [ ! -e "${log_path}" ]; then
		echo "WARNING: ${log_path:-configured log} does not exist yet."
		echo "  Once it exists, grant ${SERVICE_USER} read access:"
		echo "  sudo setfacl -m u:${SERVICE_USER}:x $(dirname "${log_path:-/path/to/log}")"
		echo "  sudo setfacl -m u:${SERVICE_USER}:r ${log_path:-/path/to/log}"
		return
	fi

	if ! command -v runuser >/dev/null 2>&1; then
		echo "WARNING: runuser not found; skipping log-access check."
		return
	fi

	if runuser -u "${SERVICE_USER}" -- /usr/bin/test -r "${log_path}" 2>/dev/null; then
		return
	fi

	echo "Granting ${SERVICE_USER} read access to ${log_path}..."
	if command -v setfacl >/dev/null 2>&1; then
		setfacl -m "u:${SERVICE_USER}:x" "$(dirname "${log_path}")" 2>/dev/null || true
		setfacl -m "u:${SERVICE_USER}:r" "${log_path}" 2>/dev/null || true
	fi

	if runuser -u "${SERVICE_USER}" -- /usr/bin/test -r "${log_path}" 2>/dev/null; then
		echo "Log is readable by ${SERVICE_USER}."
	else
		echo "WARNING: ${SERVICE_USER} cannot read ${log_path}."
		echo "  sudo setfacl -m u:${SERVICE_USER}:x $(dirname "${log_path}")"
		echo "  sudo setfacl -m u:${SERVICE_USER}:r ${log_path}"
		echo "  Or place ${SERVICE_USER} in the log file's group."
	fi
}

grant_log_access "${LOG_FILE}"

systemctl daemon-reload
systemctl enable tblocker
systemctl restart tblocker
systemctl status tblocker --no-pager || true

echo
echo "Installed. Service user: ${SERVICE_USER}:${SERVICE_GROUP} (nologin)."
echo "Follow the log with:"
echo "  journalctl -u tblocker -f"
echo
echo "Review ${CONFIG} before relying on it in production."
