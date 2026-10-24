#!/bin/sh
# Pengganti useradd/groupadd/userdel/groupdel/gpasswd/smbpasswd untuk test:
# mengubah salinan passwd/group di $FAKE_DIR. Nama program dari $0.
set -e
prog=$(basename "$0")
dir=$(dirname "$0")
printf '%s %s\n' "$prog" "$*" >> "$dir/calls.log"
name=""; gid=""; members=""; min=""; max=""
while [ $# -gt 0 ]; do
  case "$1" in
    --key) case "$2" in *_MIN=*) min=${2#*=};; *_MAX=*) max=${2#*=};; esac; shift 2;;
    --gid) gid="$2"; shift 2;;
    --members) members="$2"; shift 2;;
    --home-dir|--shell) shift 2;;
    --) name="$2"; shift; break;;
    *) shift;;
  esac
done
next_id() { awk -F: -v min="$min" -v max="$max" 'BEGIN{n=min} $3>=min && $3<=max && $3>=n {n=$3+1} END{print n}' "$1"; }
case "$prog" in
  groupadd) echo "$name:x:$(next_id "$dir/group"):" >> "$dir/group" ;;
  useradd)
    g=$(awk -F: -v n="$gid" '$1==n{print $3}' "$dir/group")
    echo "$name:x:$(next_id "$dir/passwd"):$g::/nonexistent:/usr/sbin/nologin" >> "$dir/passwd" ;;
  userdel) awk -F: -v n="$name" '$1!=n' "$dir/passwd" > "$dir/p.tmp" && mv "$dir/p.tmp" "$dir/passwd" ;;
  groupdel) awk -F: -v n="$name" '$1!=n' "$dir/group" > "$dir/g.tmp" && mv "$dir/g.tmp" "$dir/group" ;;
  gpasswd) awk -F: -v OFS=: -v n="$name" -v m="$members" '$1==n{$4=m} {print}' "$dir/group" > "$dir/g.tmp" && mv "$dir/g.tmp" "$dir/group" ;;
  smbpasswd) cat > "$dir/smbpasswd.stdin" ;;
esac
