#!/usr/bin/env bash
# Install scripts + units and start the soak. Run on the VM from the repo dir ops/soak, after setup.sh.
# usage: sudo-capable user: ops/soak/install.sh
set -eu
D=$(cd "$(dirname "$0")" && pwd); SOAK=/home/ubuntu/soak
mkdir -p $SOAK/{log,reports,state}
cp $D/{lib.sh,load.py,maint.sh,audit.sh,metrics.sh,report.sh,CRITERIA.md} $SOAK/
chmod +x $SOAK/*.sh $SOAK/load.py
[ -f $SOAK/START ] || date -u +%FT%TZ > $SOAK/START
sudo cp $D/toki-soak*.service $D/toki-soak*.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now toki-soak.service
for i in 1 2 3 4 5 6 7 8 9 10; do curl -sf http://127.0.0.1:8099/api/health >/dev/null && break; sleep 1; done
python3 $SOAK/load.py setup
sudo systemctl enable --now toki-soak-{load,metrics,maint,audit,report}.timer
systemctl list-timers 'toki-soak-*' --no-pager
echo "soak started $(cat $SOAK/START), ends $(date -u -d "$(cat $SOAK/START) + 7 days" +%FT%TZ)"
