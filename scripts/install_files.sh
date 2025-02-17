#!/bin/bash
set -eo pipefail

SRC_DIR="/installed"
DEST_DIR="/etc/vgpu-manager"

if [[ ! -d "$SRC_DIR" ]]; then
    echo "error: source dir $SRC_DIR non-existent"
    exit 1
fi

if [[ ! -d "$DEST_DIR" ]]; then
    echo "error: target dir $DEST_DIR non-existent"
    exit 1
fi

find "$SRC_DIR" -type f | while read -r src_file; do

    rel_path="${src_file#$SRC_DIR/}"
    dest_file="$DEST_DIR/$rel_path"

    do_copy=false
    if [[ ! -f "$dest_file" ]]; then
        echo "copy file: $rel_path ($src_file -> $dest_file)"
        do_copy=true
    else
        src_md5=$(md5sum "$src_file" | cut -d' ' -f1)
        dest_md5=$(md5sum "$dest_file" | cut -d' ' -f1)

        if [[ "$src_md5" != "$dest_md5" ]]; then
            echo "replace file: $rel_path (MD5: $dest_md5 -> $src_md5)"
            do_copy=true
        fi
    fi

    if [[ "$do_copy" == true ]]; then
        cp -f --preserve=all "$src_file" "$dest_file"
    fi
done

echo "Installation successful"