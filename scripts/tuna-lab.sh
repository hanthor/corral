#!/usr/bin/env bash
# tuna-lab — pick an image, get a VM you can click around in.
#
#   tuna-lab                               # TUI: pick a mode + image, then boot it
#   tuna-lab iso   <image-ref> [vm-name]   # tacklebox builds an ISO, boot it to test INSTALLING
#   tuna-lab image <image-ref> [vm-name]   # boot the bootc image directly, to test the IMAGE
#
# Ends by starting the VM and bringing up `corral web`, whose dashboard has
# in-browser VNC and serial consoles — so one command gets you from an image
# name to something you can click around in.
#
# Both land on a corral VM. Nothing here is new capability: corral already
# boots an ISO (`create --iso` → `-cdrom … -boot once=d,menu=on`) and already
# boots a bootc image (`corral-bootc create --image`). This is the glue that
# was missing — one command from an image ref to something running.
#
# Backend: corral's own default, which falls back to qemu when no kubevirt or
# incus context is configured (pkg/config.DefaultBackend). We deliberately do
# not pass --qemu; overriding here would defeat the fallback on hosts that DO
# have a cluster. Use CORRAL_DEFAULT_BACKEND to force one.
set -euo pipefail

MODE="${1:-}"
IMAGE="${2:-}"
VM_NAME="${3:-}"
DISK="${TUNA_LAB_DISK:-40G}"
# corral appends "G" when --mem carries no unit, so a bare 4096 becomes
# -m 4096G and qemu dies with "cannot set up guest memory". Always unit-ful.
MEM="${TUNA_LAB_MEM:-4G}"
CPUS="${TUNA_LAB_CPUS:-4}"
WORK="${TUNA_LAB_WORK:-${XDG_CACHE_HOME:-$HOME/.cache}/tuna-lab}"

# ISO authoring runs in a container. tacklebox's pure-Go ISO writer refuses
# files >= 4 GiB (single-extent ISO9660 — tuna-os/tacklebox#158) and the
# unpacked rootfs of a desktop edition is routinely over that: marlin's
# CachyOS rootfs is 4.9 GB. xorriso emits multi-extent files and handles it,
# but immutable bootc hosts (himachal) have no xorriso and can't get one
# without layering. A container gives every host the same tool.
ISO_BUILDER_IMAGE="${TUNA_LAB_BUILDER_IMAGE:-docker.io/library/alpine:latest}"

die() { echo "tuna-lab: $*" >&2; exit 1; }

# Prefer the system podman over a Homebrew one.
#
# TunaOS hosts ship Homebrew, and brew's podman is configured for root storage
# (/run/containers/storage, /var/lib/containers/storage). Run as a normal user
# it dies before doing anything:
#
#   Error: configure storage: open /var/lib/containers/storage/storage.lock:
#   permission denied
#
# The system podman in /usr/bin is the rootless-configured one. Anyone who
# puts brew ahead of /usr/bin in PATH to pick up qemu — which is exactly what
# you have to do on an immutable host where qemu only exists via brew — gets
# brew's podman as a side effect and a confusing permission error.
pick_podman() {
	if [[ -x /usr/bin/podman ]]; then
		echo /usr/bin/podman
	else
		command -v podman 2>/dev/null || true
	fi
}

# The editions the ISO Builder publishes: every variant x desktop that has a
# published image. Kept as one list so the picker and the docs cannot drift
# apart the way a hand-maintained menu would.
TUNA_EDITIONS="yellowfin:gnome yellowfin:kde yellowfin:cosmic yellowfin:niri
bonito:gnome bonito:kde bonito:cosmic bonito:niri bonito:xfce
sailfin:gnome sailfin:kde sailfin:niri sailfin:xfce
flounder:gnome flounder:kde flounder:cosmic flounder:niri flounder:xfce
grouper:gnome grouper:kde grouper:niri grouper:xfce
marlin:gnome marlin:kde marlin:cosmic marlin:niri marlin:xfce
skipjack:gnome skipjack:kde skipjack:cosmic skipjack:niri
albacore:gnome albacore:kde albacore:cosmic albacore:niri
guppy:gnome guppy:kde
sailfin:base yellowfin:base bonito:base flounder:base marlin:base"

# fzf when it is there (fuzzy search over ~40 refs beats scrolling), a plain
# numbered menu when it is not. No hard dependency either way.
pick_from() {
	local prompt="$1" list="$2"
	if command -v fzf >/dev/null; then
		printf '%s\n' $list | fzf --prompt="$prompt " --height=20 --reverse || true
		return
	fi
	local -a opts; read -r -d '' -a opts < <(printf '%s\0' $list) || true
	printf '%s\n' "$prompt" >&2
	local i=1
	for o in "${opts[@]}"; do printf '  %2d) %s\n' "$i" "$o" >&2; i=$((i+1)); done
	printf '  select [1-%d]: ' "${#opts[@]}" >&2
	local n; read -r n
	[[ "$n" =~ ^[0-9]+$ ]] && ((n>=1 && n<=${#opts[@]})) || die "invalid selection"
	printf '%s\n' "${opts[$((n-1))]}"
}

run_tui() {
	MODE="$(pick_from 'What do you want to test?' 'iso image')"
	[[ -n "$MODE" ]] || die "nothing picked"
	local hint="the installer (builds an ISO)"
	[[ "$MODE" == image ]] && hint="the image itself (boots it directly)"
	echo "==> testing $hint" >&2
	local ref
	ref="$(pick_from 'Which image?' "$TUNA_EDITIONS")"
	[[ -n "$ref" ]] || die "nothing picked"
	# Bare variant:desktop refs are tuna-os/; anything with a slash is literal.
	[[ "$ref" == */* ]] || ref="tuna-os/$ref"
	IMAGE="$ref"
}

# corral web is the console. Bind the tailnet IP when we can, so the URL works
# from a laptop rather than only on the host we happen to be ssh'd into.
web_addr() {
	local ip=""
	command -v tailscale >/dev/null && ip="$(tailscale ip -4 2>/dev/null | head -1)"
	[[ -n "$ip" ]] && echo "$ip:8006" || echo "127.0.0.1:8006"
}

start_web() {
	local addr; addr="$(web_addr)"
	if curl -fsS --max-time 2 "http://$addr/" >/dev/null 2>&1; then
		echo "$addr"; return
	fi
	nohup corral web --addr "$addr" >/tmp/corral-web.log 2>&1 &
	for _ in $(seq 1 20); do
		curl -fsS --max-time 1 "http://$addr/" >/dev/null 2>&1 && break
		sleep 0.5
	done
	echo "$addr"
}

usage() {
	sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-1}"
}

if [[ -z "$MODE" && -z "$IMAGE" ]]; then
	run_tui
elif [[ -z "$MODE" || -z "$IMAGE" ]]; then
	usage 1
fi
command -v corral >/dev/null || die "corral not on PATH"

# tuna-os/sailfin:base → sailfin-base; used for the VM name and ISO filename.
slug() { echo "${1##*/}" | tr ':/' '--' | tr -cd '[:alnum:]-'; }
[[ -n "$VM_NAME" ]] || VM_NAME="lab-$(slug "$IMAGE")"

corral_backend() {
	corral config get-default-backend 2>/dev/null || echo "${CORRAL_DEFAULT_BACKEND:-qemu}"
}

case "$MODE" in
image)
	# Testing the image itself: no ISO, no installer. corral-bootc builds the
	# container into a bootable disk and creates the VM.
	command -v corral-bootc >/dev/null || die "corral-bootc plugin not on PATH"
	echo "==> backend: $(corral_backend)"
	echo "==> building $IMAGE into a VM (corral-bootc)"
	corral-bootc create "$VM_NAME" --image "$IMAGE" --disk "$DISK" --mem "$MEM" --cpu "$CPUS"
	;;

iso)
	# Testing the installer: build a live ISO with tacklebox, boot it with a
	# blank disk attached so there is somewhere to install TO.
	PODMAN="$(pick_podman)"
	[[ -n "$PODMAN" ]] || die "podman is required to author the ISO (see ISO_BUILDER_IMAGE)"
	PUREBUILD="${TUNA_LAB_PUREBUILD:-$(command -v purebuild || true)}"
	[[ -n "$PUREBUILD" ]] || die "purebuild not found; build it with: go build -o purebuild ./cmd/purebuild (in tacklebox) and set TUNA_LAB_PUREBUILD"

	mkdir -p "$WORK"
	ISO="$WORK/$(slug "$IMAGE").iso"
	LABEL="$(slug "$IMAGE" | tr 'a-z-' 'A-Z_' | cut -c1-32)"

	if [[ -s "$ISO" && -z "${TUNA_LAB_REBUILD:-}" ]]; then
		echo "==> reusing $ISO ($(du -h "$ISO" | cut -f1)) — set TUNA_LAB_REBUILD=1 to force"
	else
		echo "==> authoring ISO for $IMAGE (xorriso in $ISO_BUILDER_IMAGE)"
		# purebuild is static (CGO_ENABLED=0), so it runs in any container; we
		# only need the container for xorriso itself.
		"$PODMAN" run --rm \
			-v "$WORK:/work:z" \
			-v "$PUREBUILD:/usr/local/bin/purebuild:ro,z" \
			-w /work \
			"$ISO_BUILDER_IMAGE" \
			sh -euc "
				apk add -q xorriso 2>/dev/null || (apt-get update -qq && apt-get install -y -qq xorriso)
				purebuild -image '$IMAGE' -label '$LABEL' -xorriso \
					-out '/work/$(basename "$ISO")' -workdir /work/.build
			"
		[[ -s "$ISO" ]] || die "ISO build produced nothing"
		echo "==> built $ISO ($(du -h "$ISO" | cut -f1))"
	fi

	echo "==> backend: $(corral_backend)"
	echo "==> creating $VM_NAME with a blank ${DISK} disk to install onto"
	corral create "$VM_NAME" --iso "$ISO" --disk "$DISK" --mem "$MEM" --cpu "$CPUS"
	;;

*) usage 1 ;;
esac

echo "==> starting $VM_NAME"
corral start "$VM_NAME" >/dev/null 2>&1 || true

# A VM that reports "started" and is Stopped a second later has died in the
# unit — worth surfacing here rather than letting someone open a console onto
# nothing. (This caught -m 4096G: corral appends G to a unit-less --mem, so
# 4096 asked qemu for 4 TB and it exited immediately.)
sleep 3
if ! corral list 2>/dev/null | grep -q "^${VM_NAME}[[:space:]].*Running"; then
	echo "!!! $VM_NAME is not running — the qemu unit exited. Recent journal:" >&2
	journalctl --user -u "corral-${VM_NAME}" -n 8 --no-pager 2>&1 | tail -8 >&2
	die "VM failed to start"
fi

ADDR="$(start_web)"
cat <<EOF

==> $VM_NAME is running.

    Console:  http://$ADDR/          <- VNC + serial in the browser, pick "$VM_NAME"
    Native:   corral viewer $VM_NAME
    Shell:    corral ssh $VM_NAME
    Clean up: corral delete $VM_NAME

Deliberately no automated pass/fail: the point is that you look at it.
tunaOS scripts/iso-e2e.sh is the automated gate, and its screenshot fallback
has passed an ISO sitting in a dracut emergency shell before
(tuna-os/tunaOS#901), so eyes beat exit codes for interactive testing.
EOF
