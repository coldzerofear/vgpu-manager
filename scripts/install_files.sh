#!/bin/bash
# Copyright 2025-2026 coldzerofear
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
set -eo pipefail

SRC_DIR="/installed"
DEST_DIR="${HOST_MANAGER_DIR:-/etc/vgpu-manager}"

if [[ ! -d "$SRC_DIR" ]]; then
    echo "error: source dir $SRC_DIR non-existent"
    exit 1
fi

if [[ ! -d "$DEST_DIR" ]]; then
    echo "error: target dir $DEST_DIR non-existent"
    exit 1
fi

find "$SRC_DIR" \( -type f -o -type l \) | while read -r src_file; do

    rel_path="${src_file#$SRC_DIR/}"
    dest_file="$DEST_DIR/$rel_path"

    dest_dir=$(dirname "$dest_file")
    if [[ ! -d "$dest_dir" ]]; then
        mkdir -p "$dest_dir"
    fi

    do_copy=false

    if [[ -L "$src_file" ]]; then
        if [[ ! -L "$dest_file" ]] || [[ "$(readlink "$src_file")" != "$(readlink "$dest_file")" ]]; then
            echo "copy symlink: $rel_path ($(readlink "$src_file"))"
            do_copy=true
        else
            echo "skipped symlink: $rel_path (already points to $(readlink "$dest_file"))"
        fi
    else
        if [[ ! -f "$dest_file" ]]; then
            echo "copy file: $rel_path ($src_file -> $dest_file)"
            do_copy=true
        else
            src_md5=$(md5sum "$src_file" | cut -d' ' -f1)
            dest_md5=$(md5sum "$dest_file" | cut -d' ' -f1)

            if [[ "$src_md5" != "$dest_md5" ]]; then
                echo "replace file: $rel_path (MD5: $dest_md5 -> $src_md5)"
                do_copy=true
            else
                echo "skipped file: $rel_path (MD5: $dest_md5)"
            fi
        fi
    fi

    if [[ "$do_copy" == true ]]; then
        if [[ -L "$src_file" ]]; then
            cp -a "$src_file" "$dest_file"
        else
            if cp --help 2>&1 | grep -q -- '--preserve'; then
                cp -f --preserve=all "$src_file" "$dest_file"
            else
                cp -fp "$src_file" "$dest_file"
            fi
        fi
    fi
done

echo "Installation successful"
