#!/bin/sh
# Nightly backup of every store (consistent online copies; safe while serving).
# cron:  15 2 * * *  /opt/pricetag/deploy/backup.sh
# Then copy $OUT somewhere else (another machine, object storage, rclone ...): a backup on the same disk is not a backup.
set -eu
DATA=${PRICETAG_DATA:-/var/lib/pricetag}
OUT=${BACKUP_DIR:-/var/backups/pricetag}
KEEP_DAYS=${KEEP_DAYS:-14}
mkdir -p "$OUT"
for slug in $(pricetag store list -data "$DATA"); do
  pricetag store backup -data "$DATA" -slug "$slug" -out "$OUT/$slug-$(date +%Y%m%d-%H%M%S).db"
done
pricetag store backup-accounts -data "$DATA" -out "$OUT/accounts-$(date +%Y%m%d-%H%M%S).db"   # shopper accounts (shared by all shops)
find "$OUT" -name '*.db' -mtime +"$KEEP_DAYS" -delete
