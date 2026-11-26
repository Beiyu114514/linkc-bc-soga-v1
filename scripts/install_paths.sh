#!/bin/sh
# Shared safety checks for privileged installer paths.

install_path_error() {
	printf 'unsafe install path: %s\n' "$*" >&2
	return 1
}

normalize_install_path() { # normalize_install_path ENV_NAME PATH DEFAULT
	env_name=$1
	path=$2
	default_path=$3

	if [ -z "$path" ]; then
		install_path_error "$env_name must not be empty"
		return 1
	fi
	case "$path" in
		/*) ;;
		*)
			install_path_error "$env_name must be absolute: $path"
			return 1
			;;
	esac
	case "$path" in
		*[[:cntrl:]]*)
		install_path_error "$env_name must not contain a newline"
		return 1
		;;
	esac
	case "/$path/" in
		*/../*|*/./*)
			install_path_error "$env_name must not contain . or .. components: $path"
			return 1
			;;
	esac
	if ! command -v realpath >/dev/null 2>&1; then
		install_path_error "realpath is required to validate $env_name"
		return 1
	fi
	normalized=$(realpath -m -- "$path") || {
		install_path_error "$env_name cannot be resolved: $path"
		return 1
	}

	if [ "$normalized" != "$default_path" ]; then
		case "$normalized" in
			/|/bin|/boot|/dev|/etc|/home|/lib|/lib32|/lib64|/media|/mnt|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var|/etc/systemd|/etc/systemd/system|/run/systemd|/usr/bin|/usr/sbin|/usr/local|/usr/local/bin|/var/cache|/var/lib|/var/log|/var/spool|/var/tmp)
				install_path_error "$env_name points at a system-wide directory: $normalized"
				return 1
				;;
		esac
		relative=${normalized#/}
		case "$relative" in
			*/*) ;;
			*)
				install_path_error "$env_name is too close to the filesystem root: $normalized"
				return 1
				;;
		esac
		case "$env_name" in
			LINKC_*)
				base=${normalized##*/}
				parent=${normalized%/*}
				parent_base=${parent##*/}
				dedicated=0
				case "$base" in
					linkc-node|linkc-node-*) dedicated=1 ;;
				esac
				case "$parent_base" in
					linkc-node|linkc-node-*) dedicated=1 ;;
				esac
				if [ "$dedicated" -ne 1 ]; then
					install_path_error "$env_name must use a dedicated linkc-node directory: $normalized"
					return 1
				fi
				;;
		esac
	fi

	printf '%s\n' "$normalized"
}

ensure_disjoint_install_paths() {
	while [ "$#" -gt 0 ]; do
		left=$1
		shift
		for right in "$@"; do
			if [ "$left" = "$right" ]; then
				install_path_error "install directories must be distinct: $left"
				return 1
			fi
			case "$left" in
				"$right"/*)
					install_path_error "install directories must not overlap: $left is inside $right"
					return 1
					;;
			esac
			case "$right" in
				"$left"/*)
					install_path_error "install directories must not overlap: $right is inside $left"
					return 1
					;;
			esac
		done
	done
}

ensure_managed_file_slot() { # ensure_managed_file_slot PATH LABEL
	path=$1
	label=$2
	if [ -L "$path" ]; then
		install_path_error "$label must not be a symbolic link: $path"
		return 1
	fi
	if [ -e "$path" ] && [ ! -f "$path" ]; then
		install_path_error "$label must be a regular file: $path"
		return 1
	fi
}

write_managed_file() { # write_managed_file PATH MODE OWNER; content is read from stdin
	path=$1
	mode=$2
	owner=$3
	ensure_managed_file_slot "$path" "managed file" || return 1
	dir=${path%/*}
	base=${path##*/}
	if [ -z "$dir" ]; then
		dir=/
	fi
	if [ ! -d "$dir" ] || [ -L "$dir" ]; then
		install_path_error "managed file parent must be a real directory: $dir"
		return 1
	fi
	tmp=$(mktemp "$dir/.$base.tmp.XXXXXX") || {
		install_path_error "cannot create a temporary file in $dir"
		return 1
	}
	if ! cat >"$tmp" || ! chown "$owner" "$tmp" || ! chmod "$mode" "$tmp"; then
		rm -f -- "$tmp"
		install_path_error "cannot prepare managed file: $path"
		return 1
	fi
	if ! mv -fT -- "$tmp" "$path"; then
		rm -f -- "$tmp"
		install_path_error "cannot replace managed file atomically: $path"
		return 1
	fi
}

enable_and_restart_service() { # enable_and_restart_service UNIT
	unit=$1
	systemctl daemon-reload
	systemctl enable "$unit"
	systemctl restart "$unit"
}
