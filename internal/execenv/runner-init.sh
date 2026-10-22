#!/bin/sh
# kran-init: entrypoint of kran's job containers (SPEC §6.8). It runs as
# root for a moment to make the container look like the job's machine (a
# passwd entry for the job's uid, the origin's toolchains and time zone,
# its locale), then drops to the job's uid and gid for good. The inputs
# come from kran and are checked anyway: nothing here evaluates them.
# Its own variables are prefixed kr_ so they never shadow the job's.
set -u
set -f # toolchain lists are split on spaces, never globbed

kr_warn() { printf 'kran: %s\n' "$*" >&2; }

kr_lib=/usr/local/lib/kran
kr_uid=${KRAN_INIT_UID:-}
kr_gid=${KRAN_INIT_GID:-}
case $kr_uid in '' | *[!0-9]* | 0) kr_warn "the job has no valid uid ($kr_uid)"; exit 125 ;; esac
case $kr_gid in '' | *[!0-9]*) kr_warn "the job has no valid gid ($kr_gid)"; exit 125 ;; esac
if ! command -v setpriv >/dev/null 2>&1; then
	kr_warn "the runner image has no setpriv; remove it so kran builds it again"
	exit 125
fi

# A user or group name kran can write into /etc/passwd and /etc/group.
kr_valid_name() {
	case $1 in '' | -* | root | *[!A-Za-z0-9._-]*) return 1 ;; esac
	[ ${#1} -le 64 ]
}

# --- user and groups: os/user, id, whoami and ssh need an entry for the uid.
kr_user=${KRAN_INIT_USER:-}
kr_valid_name "$kr_user" || kr_user=kran
kr_group=${KRAN_INIT_GROUP:-}
kr_valid_name "$kr_group" || kr_group=$kr_user
kr_home=${HOME:-/}
case $kr_home in /*) ;; *) kr_home=/ ;; esac
case $kr_home in *:*) kr_home=/ ;; esac
kr_shell=/bin/bash
[ -x "$kr_shell" ] || kr_shell=/bin/sh
kr_extra=
for kr_g in $(printf '%s' "${KRAN_INIT_GROUPS:-}" | tr ',' ' '); do
	case $kr_g in '' | *[!0-9]*) ;; *) [ "$kr_g" = "$kr_gid" ] || kr_extra="$kr_extra,$kr_g" ;; esac
done

# Entries with the job's uid or name are replaced (root's never is).
awk -F: -v u="$kr_uid" -v n="$kr_user" '$1 == "root" || ($3 != u && $1 != n)' /etc/passwd >/etc/passwd.kran &&
	printf '%s:x:%s:%s:kran job:%s:%s\n' "$kr_user" "$kr_uid" "$kr_gid" "$kr_home" "$kr_shell" >>/etc/passwd.kran &&
	chmod 644 /etc/passwd.kran && mv /etc/passwd.kran /etc/passwd ||
	kr_warn "could not add the job's user to /etc/passwd"

# The primary group gets the target's name for it; extra groups (the
# Docker socket's) list the user as a member.
awk -F: -v OFS=: -v gid="$kr_gid" -v name="$kr_group" -v user="$kr_user" -v extra="$kr_extra," '
	$1 == "root" || $3 == 0 { print; names[$1] = 1; next }
	$3 == gid || $1 == name { next }
	index(extra, "," $3 ",") { $4 = ($4 == "" ? user : $4 "," user); seen[$3] = 1 }
	{ print; names[$1] = 1 }
	END {
		if (gid != 0) print name, "x", gid, ""
		n = split(extra, a, ",")
		for (i = 1; i <= n; i++) {
			if (a[i] == "" || (a[i] in seen)) continue
			nm = ("docker" in names) ? "docker" a[i] : "docker"
			names[nm] = 1
			print nm, "x", a[i], user
		}
	}' /etc/group >/etc/group.kran &&
	chmod 644 /etc/group.kran && mv /etc/group.kran /etc/group ||
	kr_warn "could not add the job's group to /etc/group"

# --- toolchains: mise resolves the origin's versions (a repo's own mise
# config wins over this global one); a shim per tool binary installs that
# tool on first use and nothing else.
mkdir -p "$kr_lib/shims"
{
	echo '[tools]'
	for kr_t in ${KRAN_INIT_TOOLS:-}; do
		kr_k=${kr_t%%=*}
		kr_v=${kr_t#*=}
		case $kr_k in '' | *[!a-z0-9_-]*) continue ;; esac
		case $kr_v in '' | *[!A-Za-z0-9._+-]*) continue ;; esac
		printf '%s = "%s"\n' "$kr_k" "$kr_v"
	done
} >"$kr_lib/mise.toml"
kr_mise=$(command -v mise || echo /usr/local/bin/mise)
for kr_b in ${KRAN_INIT_SHIMS:-}; do
	case $kr_b in '' | .* | *[!A-Za-z0-9._+-]*) continue ;; esac
	ln -sf "$kr_mise" "$kr_lib/shims/$kr_b"
done

# --- time zone: the origin's, like a local run ($TZ itself stays as is).
kr_tz=${KRAN_INIT_TZ:-}
case $kr_tz in
'' | /* | *..* | *[!A-Za-z0-9._+/-]*) ;;
*)
	if [ -f "/usr/share/zoneinfo/$kr_tz" ]; then
		ln -sf "/usr/share/zoneinfo/$kr_tz" /etc/localtime
		printf '%s\n' "$kr_tz" >/etc/timezone
	fi
	;;
esac

# --- locale: build a UTF-8 locale the image lacks (about a second); one
# that cannot exist here (macOS's bare LC_CTYPE=UTF-8) becomes C.UTF-8.
kr_have_locale() {
	locale -a 2>/dev/null | grep -qix -e "$1" -e "${1%.*}.utf8"
}
kr_ensure_locale() {
	case $1 in C | POSIX | C.* | POSIX.*) return 0 ;; esac
	case $1 in *.[Uu][Tt][Ff]8 | *.[Uu][Tt][Ff]-8) ;; *) kr_have_locale "$1"; return ;; esac
	kr_have_locale "$1" && return 0
	kr_base=${1%%.*}
	case $kr_base in '' | *[!A-Za-z_]*) return 1 ;; esac
	[ -f "/usr/share/i18n/locales/$kr_base" ] || return 1
	localedef -i "$kr_base" -f UTF-8 "$kr_base.UTF-8" 2>/dev/null
	kr_have_locale "$1"
}
if command -v localedef >/dev/null 2>&1; then
	for kr_var in LANG LC_ALL LC_CTYPE LC_COLLATE LC_MESSAGES LC_NUMERIC LC_TIME LC_MONETARY; do
		eval "kr_val=\${$kr_var:-}"
		[ -n "$kr_val" ] || continue
		kr_ensure_locale "$kr_val" && continue
		case $kr_val in *[Uu][Tt][Ff]-8 | *[Uu][Tt][Ff]8) export "$kr_var=C.UTF-8" ;; esac
	done
fi

# --- paths from the origin that do not exist here would break TLS or
# tool lookups instead of falling back to the defaults.
for kr_var in SSL_CERT_FILE SSL_CERT_DIR NODE_EXTRA_CA_CERTS REQUESTS_CA_BUNDLE CURL_CA_BUNDLE GIT_SSL_CAINFO JAVA_HOME VIRTUAL_ENV CONDA_PREFIX; do
	eval "kr_val=\${$kr_var:-}"
	if [ -n "$kr_val" ] && [ ! -e "$kr_val" ]; then unset "$kr_var"; fi
done

kr_groups=$kr_gid$kr_extra
kr_drop() { setpriv --reuid="$kr_uid" --regid="$kr_gid" --groups="$kr_groups" -- "$@"; }

# --- Docker: the job's /var/run/docker.sock is kran's proxy in front of
# Docker's socket, so bind mounts of the workspace (`docker run -v
# "$PWD":/src`, compose) reach the job's own files on the Docker host. It
# runs as the job's user, outside the job's process tree (its parent is the
# container's init), and ends with the container. Without it, the job gets
# Docker's socket as is.
kr_dhost=/run/kran/docker-host.sock
if [ -S "$kr_dhost" ]; then
	kr_dsock=/run/kran/docker/docker.sock
	if [ -x "$kr_lib/kran" ] && [ -n "${KRAN_INIT_DOCKER_MAP:-}" ] &&
		mkdir -p /run/kran/docker && chmod 1777 /run/kran/docker; then
		(kr_drop "$kr_lib/kran" internal docker-proxy --listen "$kr_dsock" --upstream "$kr_dhost" </dev/null >/dev/null 2>&1 9<&- &)
		kr_i=0
		while [ ! -S "$kr_dsock" ] && [ $kr_i -lt 200 ]; do
			sleep 0.01
			kr_i=$((kr_i + 1))
		done
		[ -S "$kr_dsock" ] || kr_warn "the Docker proxy did not start; bind mounts of the workspace will be empty"
	fi
	[ -S "$kr_dsock" ] || kr_dsock=$kr_dhost
	ln -sfn "$kr_dsock" /run/docker.sock 2>/dev/null || kr_warn "could not link the Docker socket"
fi

# --- shared caches: the job holds kran's cache lock (shared) for the life
# of its container, so the cache trim never deletes what it uses. A
# process outside the job's tree holds it: the job may close any fd.
kr_lock=/kran/cache/.kran-lock
if [ -f "$kr_lock" ] && [ -r "$kr_lock" ] && command -v flock >/dev/null 2>&1 && exec 9<"$kr_lock"; then
	if flock -s -w 900 9; then
		(kr_drop sleep infinity </dev/null >/dev/null 2>&1 &)
	else
		kr_warn "the shared caches stayed locked for 15 minutes; running anyway"
	fi
fi

unset KRAN_INIT_UID KRAN_INIT_GID KRAN_INIT_GROUPS KRAN_INIT_USER KRAN_INIT_GROUP KRAN_INIT_TOOLS KRAN_INIT_SHIMS KRAN_INIT_TZ KRAN_INIT_DOCKER_MAP
exec setpriv --reuid="$kr_uid" --regid="$kr_gid" --groups="$kr_groups" -- "$@" 9<&-
