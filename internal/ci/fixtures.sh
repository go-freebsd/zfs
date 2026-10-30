#!/bin/sh
# Creates the datasets the FreeBSD integration tests compare against zfs(8).
# Usage: fixtures.sh [pool]   (run as root, pool must exist)
set -e
pool=${1:-testpool}

# A mounted tree with real data, so space accounting is not all zeros.
# Random data: it does not compress, so sizes are the same with and without lz4.
zfs create -o mountpoint=/fixture_a "$pool/a"
zfs create "$pool/a/b"
zfs create "$pool/a/b/c"
zfs create -V 8m "$pool/vol"
zfs set test:note="hello world" "$pool/a"
zfs set compression=lz4 "$pool/a/b"
zfs set quota=64m "$pool/a"

dd if=/dev/random of=/fixture_a/file0 bs=1k count=128 2>/dev/null
zfs snapshot "$pool/a@s1"
dd if=/dev/random of=/fixture_a/file1 bs=1k count=256 2>/dev/null
zfs snapshot "$pool/a@s2"

zfs snapshot -r "$pool/a/b@r1"
dd if=/dev/random of=/fixture_a/b/data bs=1k count=256 2>/dev/null
zfs snapshot "$pool/a/b@r2"
zfs snapshot "$pool/vol@v1"

zfs hold keep "$pool/a@s1"
zfs hold other "$pool/a@s1"
zfs bookmark "$pool/a@s1" "$pool/a#bm1"
zfs clone "$pool/a@s1" "$pool/clone"

# More than 128 KiB of properties: one Get must grow the reply buffer.
zfs create "$pool/big"
val=$(printf '%01000d' 0)
args=""
i=1
while [ $i -le 150 ]; do
	args="$args user:p$i=$val"
	i=$((i + 1))
done
# shellcheck disable=SC2086
zfs set $args "$pool/big"

# Many children: the list cursor must survive many calls.
zfs create "$pool/many"
i=1
while [ $i -le 120 ]; do
	zfs create "$pool/many/d$i"
	i=$((i + 1))
done

zpool sync "$pool"
echo "fixtures ready: $(zfs list -H -r -t all -o name "$pool" | wc -l | tr -d ' ') entries"
