#!/usr/bin/env bash
set -euo pipefail

usage() {
    printf 'Usage: %s DEB-DIRECTORY OUTPUT-DIRECTORY [SUITE]\n' "$0" >&2
}

[[ $# -ge 2 && $# -le 3 ]] || { usage; exit 2; }

input_dir="$1"
output_dir="$2"
suite="${3:-stable}"

[[ -d "$input_dir" ]] || { echo "APT repository input directory does not exist: $input_dir" >&2; exit 1; }
[[ "$suite" =~ ^[a-z0-9][a-z0-9.-]*$ ]] || { echo "invalid APT suite: $suite" >&2; exit 1; }

for command in dpkg-deb dpkg-scanpackages apt-ftparchive gzip; do
    command -v "$command" >/dev/null 2>&1 || { echo "required command is missing: $command" >&2; exit 1; }
done

if [[ -e "$output_dir" ]]; then
    [[ -d "$output_dir" ]] || { echo "APT repository output is not a directory: $output_dir" >&2; exit 1; }
    [[ -z "$(find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]] || {
        echo "APT repository output directory must be empty: $output_dir" >&2
        exit 1
    }
else
    mkdir -p "$output_dir"
fi

input_dir="$(cd "$input_dir" && pwd -P)"
output_dir="$(cd "$output_dir" && pwd -P)"
pool_relative="pool/main/h/homeautomationmonitorgw"
pool_dir="$output_dir/$pool_relative"
mkdir -p "$pool_dir"

shopt -s nullglob
packages=("$input_dir"/*.deb)
(( ${#packages[@]} > 0 )) || { echo "no Debian packages found in $input_dir" >&2; exit 1; }

declare -A seen=()
declare -A architectures=()
for package_path in "${packages[@]}"; do
    [[ -f "$package_path" && ! -L "$package_path" ]] || { echo "package is not a regular non-symlink file: $package_path" >&2; exit 1; }
    package_name="$(dpkg-deb -f "$package_path" Package)"
    version="$(dpkg-deb -f "$package_path" Version)"
    architecture="$(dpkg-deb -f "$package_path" Architecture)"
    case "$package_name" in
        automation-gateway|automation-gateway-led|automation-gateway-zabbix) ;;
        *) echo "unexpected package name: $package_name" >&2; exit 1 ;;
    esac
    case "$architecture" in
        amd64|arm64|armhf|i386) ;;
        *) echo "unsupported package architecture: $architecture" >&2; exit 1 ;;
    esac
    [[ -n "$version" ]] || { echo "package version is empty: $package_path" >&2; exit 1; }
    key="$package_name:$version:$architecture"
    [[ -z "${seen[$key]:-}" ]] || { echo "duplicate package tuple: $key" >&2; exit 1; }
    seen[$key]=1
    architectures[$architecture]=1
    install -m 0644 "$package_path" "$pool_dir/$(basename "$package_path")"
done

architecture_list=()
for architecture in amd64 arm64 armhf i386; do
    [[ -n "${architectures[$architecture]:-}" ]] || continue
    architecture_list+=("$architecture")
    binary_dir="$output_dir/dists/$suite/main/binary-$architecture"
    mkdir -p "$binary_dir"
    (
        cd "$output_dir"
        dpkg-scanpackages --arch "$architecture" "$pool_relative" /dev/null > "dists/$suite/main/binary-$architecture/Packages"
        gzip -n -9 -c "dists/$suite/main/binary-$architecture/Packages" > "dists/$suite/main/binary-$architecture/Packages.gz"
    )
done

architectures_text="${architecture_list[*]}"
release_dir="$output_dir/dists/$suite"
(
    cd "$output_dir"
    apt-ftparchive \
        -o "APT::FTPArchive::Release::Origin=HomeAutomationMonitorGW" \
        -o "APT::FTPArchive::Release::Label=HomeAutomationMonitorGW" \
        -o "APT::FTPArchive::Release::Suite=$suite" \
        -o "APT::FTPArchive::Release::Codename=$suite" \
        -o "APT::FTPArchive::Release::Architectures=$architectures_text" \
        -o "APT::FTPArchive::Release::Components=main" \
        -o "APT::FTPArchive::Release::Description=Signed HomeAutomationMonitorGW packages" \
        release "dists/$suite" > "dists/$suite/Release"
)

touch "$output_dir/.nojekyll"
printf 'APT repository created at %s for suite %s and architectures: %s\n' "$output_dir" "$suite" "$architectures_text"
