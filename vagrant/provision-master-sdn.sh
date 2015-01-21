#!/bin/bash
set -ex
source $(dirname $0)/provision-config.sh

pushd $HOME
# build openshift-sdn
git clone https://github.com/openshift/openshift-sdn
cd openshift-sdn
make clean
make
make install
popd

# Create systemd service
cat <<EOF > /usr/lib/systemd/system/openshift-master-sdn.service
[Unit]
Description=openshift SDN master
After=openshift-master.service

[Service]
ExecStart=/usr/bin/openshift-sdn -etcd-endpoints=http://${MASTER_IP}:4001 

[Install]
WantedBy=multi-user.target
EOF

# Start the service
systemctl daemon-reload
systemctl enable openshift-master-sdn.service
systemctl start openshift-master-sdn.service
