#!/usr/bin/env bash
# Tunes this machine for a series of measurements (NET-5), and undoes it.
#
#   sudo testbed/env/host-setup.sh             tune
#   sudo testbed/env/host-setup.sh --restore   put the saved values back
#   testbed/env/host-setup.sh --dry-run        show what would change
#
# It sets the energy preference and the governor of every CPU to
# performance, turns turbo boost off, disables the idle states that take
# longer than 10 µs to leave, and raises the socket buffer limits to 8 MiB.
# Before changing a value it appends the old one to the state file;
# --restore goes from the newest entry to the oldest, so it puts back what
# was there before the first run, whatever changed in between. It only
# warns about running on battery and about services that disturb
# measurements, such as docker.
#
# The thresholds match testbed/env (MaxIdleLatency, MinSocketBuffer).
# HOST_SETUP_ROOT puts /sys and /proc under another directory, and
# HOST_SETUP_STATE moves the state file; both are for tests.
set -euo pipefail
shopt -s nullglob

root=${HOST_SETUP_ROOT:-}
state=${HOST_SETUP_STATE:-/var/tmp/qrpc-host-setup.state}
max_idle_latency_us=10
socket_buffer=8388608

mode=tune
case ${1:-} in
'') ;;
--restore) mode=restore ;;
--dry-run) mode=dry-run ;;
*)
	echo "usage: $0 [--restore | --dry-run]" >&2
	exit 2
	;;
esac
if [[ $mode != dry-run && -z $root && $EUID -ne 0 ]]; then
	echo "run as root: sudo $0 $*" >&2
	exit 1
fi

if [[ $mode == restore ]]; then
	if [[ ! -f $state ]]; then
		echo "nothing to restore: no $state"
		exit 0
	fi
	# Newest first: the oldest value of a file is written last, and the
	# energy preference can change only once the governor is no longer
	# performance.
	tac "$state" | while IFS=$'\t' read -r path old; do
		if printf '%s\n' "$old" >"$root$path" 2>/dev/null; then
			echo "restored $path: $old"
		else
			echo "warning: cannot restore $path to $old" >&2
		fi
	done
	rm -f "$state"
	exit 0
fi

# set_value PATH VALUE writes VALUE to PATH, a /sys or /proc file, after
# saving its old value; a missing file is skipped.
set_value() {
	local path=$1 want=$2 old
	[[ -e $root$path ]] || return 0
	old=$(<"$root$path")
	[[ $old == "$want" ]] && return 0
	if [[ $mode == dry-run ]]; then
		echo "would set $path: $old -> $want"
		return 0
	fi
	printf '%s\t%s\n' "$path" "$old" >>"$state"
	if printf '%s\n' "$want" >"$root$path" 2>/dev/null; then
		echo "set $path: $old -> $want"
	else
		echo "warning: cannot set $path to $want" >&2
	fi
}

cpus=("$root"/sys/devices/system/cpu/cpu[0-9]*)
# The energy preference first: intel_pstate refuses to change it under the
# performance governor.
for dir in "${cpus[@]}"; do
	set_value "${dir#"$root"}/cpufreq/energy_performance_preference" performance
done
for dir in "${cpus[@]}"; do
	set_value "${dir#"$root"}/cpufreq/scaling_governor" performance
done
if [[ -e $root/sys/devices/system/cpu/intel_pstate/no_turbo ]]; then
	set_value /sys/devices/system/cpu/intel_pstate/no_turbo 1
else
	set_value /sys/devices/system/cpu/cpufreq/boost 0
fi
for dir in "${cpus[@]}"; do
	for st in "$dir"/cpuidle/state[0-9]*; do
		if (($(<"$st/latency") > max_idle_latency_us)); then
			set_value "${st#"$root"}/disable" 1
		fi
	done
done
for name in rmem_max wmem_max; do
	path=/proc/sys/net/core/$name
	if [[ -e $root$path ]] && (($(<"$root$path") < socket_buffer)); then
		set_value "$path" "$socket_buffer"
	fi
done

for supply in "$root"/sys/class/power_supply/*; do
	if [[ $(<"$supply/type") == Mains && $(<"$supply/online") != 1 ]]; then
		echo "warning: running on battery; plug the machine in for a series" >&2
	fi
done
for comm in "$root"/proc/[0-9]*/comm; do
	# The process may be gone by now.
	read -r name 2>/dev/null <"$comm" || continue
	case $name in
	dockerd | containerd) echo "warning: $name is running; stop it for a series (sudo systemctl stop docker containerd)" ;;
	esac
done | sort -u >&2
