#!/usr/bin/env python3
"""Activate real application ingress only after stack health succeeds."""
import argparse
from ops import MACHINES, put, ssh

parser = argparse.ArgumentParser()
parser.add_argument('host', choices=list(MACHINES))
args = parser.parse_args()
machine = MACHINES[args.host]
common = '''    ssl_certificate /etc/arknet/tls/server.crt;
    ssl_certificate_key /etc/arknet/tls/server.key;
    ssl_protocols TLSv1.2 TLSv1.3;
    # Grant-bearing URLs (/announce/<grant>, web-seed/torrent media paths) must
    # never reach a log: no access sink, and nginx cannot redact request lines
    # in its error log at any level, so it has no request-time sink either
    # (same rule as deploy/core/ingress/nginx.conf, Prompt 49B).
    access_log off;
    error_log /dev/null;
    client_max_body_size 16m;
    location = /infrastructure/health {
        default_type text/plain;
        return 200 "NodeNet infrastructure TLS ready\\n";
    }
'''
proxy_headers = '''        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_read_timeout 300s;
'''
if machine['role'] == 'core':
    config = 'server {\n    listen 100.64.0.21:443 ssl;\n    server_name core.nodenet.arknet.media nodenet.arknet.media;\n' + common
    config += '''    location /api/admin/ {
        allow 100.64.0.10;
        allow 127.0.0.1;
        deny all;
        proxy_pass http://127.0.0.1:8083;
''' + proxy_headers + '''    }
    location / { proxy_pass http://127.0.0.1:8083;
''' + proxy_headers + '''    }
}
server {
    listen 100.64.0.21:443 ssl;
    server_name tracker.nodenet.arknet.media;
''' + common + '''    location / { proxy_pass http://127.0.0.1:6969;
''' + proxy_headers + '''    }
}
'''
else:
    label = args.host[-1]
    ip = machine['tailscale_ip']
    config = 'server {\n    listen ' + ip + ':443 ssl;\n    server_name node-' + label + '.nodenet.arknet.media;\n' + common
    config += '''    # Defense in depth: management has operator auth and remains VPN-restricted.
    # Do not trust forwarded headers as the operator identity.
    allow 100.64.0.10;
    allow 127.0.0.1;
    deny all;
    location / { proxy_pass http://127.0.0.1:5174;
''' + proxy_headers + '''    }
}
server {
    # Member login/session API is separate from the management listener.
    listen ''' + ip + ''':443 ssl;
    server_name member-''' + label + '''.nodenet.arknet.media;
''' + common + '''    # Operator sign-in belongs to the VPN-restricted node-x UI, not here.
    location /api/v1/operator/ { return 404; }
    # Member sign-in pages (Core returns members to /login/callback) come from the UI.
    location ~ ^/(member|login/callback|assets/.*)$ { proxy_pass http://127.0.0.1:5174;
''' + proxy_headers + '''    }
    location / { proxy_pass http://127.0.0.1:8086;
''' + proxy_headers + '''    }
}
server {
    # Only Jellyfin has a LAN/wildcard listener; Node management is not registered here.
    listen 443 ssl default_server;
    listen ''' + ip + ''':443 ssl;
    server_name jellyfin-''' + label + '''.nodenet.arknet.media;
''' + common + '''    location / {
        proxy_pass http://127.0.0.1:8096;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_buffering off;
''' + proxy_headers + '''    }
}
'''
put(machine, '/etc/nginx/sites-available/arknet-applications', config)
# All Docker-published management ports are loopback-bound in compose.json.
command = ('sudo rm -f /etc/nginx/sites-enabled/arknet-infrastructure && '
           'sudo ln -sfn /etc/nginx/sites-available/arknet-applications /etc/nginx/sites-enabled/arknet-applications && '
           'sudo nginx -t && sudo systemctl restart nginx')
print(ssh(machine, command).decode())
if machine['role'] != 'core':
    print(ssh(machine, 'sudo ufw allow from 192.168.0.0/24 to any port 443 proto tcp comment "LAN Jellyfin only; management vhost VPN-bound"').decode())
print('APPLICATION_INGRESS_ACTIVE', args.host)
