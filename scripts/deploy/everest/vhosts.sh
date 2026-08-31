#!/usr/bin/env bash
set -uo pipefail
ND=/opt/homebrew/etc/nginx/servers
TLS_C=/Users/local/polar/tls/everest.crt
TLS_K=/Users/local/polar/tls/everest.key

# static UI plugins: name|port|index
STATIC=("wg|8090|wg-tokens.html" "lawyer|8099|lawyer.html")
# svc-served UI plugins: name|port
SVCUI=("buildings|8100" "release|8101" "stock|8103" "screen|8105" "film|8106" "dns|8107")

for row in "${STATIC[@]}"; do
  IFS='|' read -r NAME PORT INDEX <<< "$row"
  cat > "$ND/everest-${NAME}.conf" <<EOF
server {
  listen 443 ssl;
  server_name ${NAME}.everest.4950.store;
  ssl_certificate ${TLS_C};
  ssl_certificate_key ${TLS_K};
  root /Users/local/var/polar-ui/${NAME};
  index ${INDEX};
  client_max_body_size 500m;
  location = /styles.css { alias /Users/local/var/polar-ui-shared/styles.css; expires 1h; }
  location ^~ /assets/   { alias /Users/local/var/polar-ui-shared/assets/;   expires 1h; }
  location ^~ /api/${NAME} { proxy_pass http://127.0.0.1:${PORT}; proxy_set_header Host \$host; proxy_set_header X-Forwarded-Proto https; proxy_set_header X-Real-IP \$remote_addr; proxy_http_version 1.1; proxy_set_header Upgrade \$http_upgrade; proxy_set_header Connection \$everest_conn; proxy_read_timeout 600s; }
  location ~ ^/(api|ws|uploads|internal|oauth|release)(/|\$) { proxy_pass http://127.0.0.1:8080; proxy_set_header Host \$host; proxy_set_header X-Forwarded-Proto https; proxy_set_header X-Real-IP \$remote_addr; proxy_http_version 1.1; proxy_set_header Upgrade \$http_upgrade; proxy_set_header Connection \$everest_conn; proxy_read_timeout 3600s; }
  location / { try_files \$uri \$uri.html \$uri/ /${INDEX}; }
}
EOF
  echo "wrote everest-${NAME}.conf (static)"
done

for row in "${SVCUI[@]}"; do
  IFS='|' read -r NAME PORT <<< "$row"
  cat > "$ND/everest-${NAME}.conf" <<EOF
server {
  listen 443 ssl;
  server_name ${NAME}.everest.4950.store;
  ssl_certificate ${TLS_C};
  ssl_certificate_key ${TLS_K};
  client_max_body_size 500m;
  location ^~ /api/${NAME}/ { proxy_pass http://127.0.0.1:${PORT}; proxy_set_header Host \$host; proxy_set_header X-Forwarded-Proto https; proxy_set_header X-Real-IP \$remote_addr; proxy_http_version 1.1; proxy_set_header Upgrade \$http_upgrade; proxy_set_header Connection \$everest_conn; proxy_read_timeout 600s; }
  location ~ ^/(api|ws|uploads|internal|oauth|release)(/|\$) { proxy_pass http://127.0.0.1:8080; proxy_set_header Host \$host; proxy_set_header X-Forwarded-Proto https; proxy_set_header X-Real-IP \$remote_addr; proxy_http_version 1.1; proxy_set_header Upgrade \$http_upgrade; proxy_set_header Connection \$everest_conn; proxy_read_timeout 3600s; }
  location / { proxy_pass http://127.0.0.1:${PORT}; proxy_set_header Host \$host; proxy_set_header X-Forwarded-Proto https; proxy_set_header X-Real-IP \$remote_addr; proxy_http_version 1.1; proxy_set_header Upgrade \$http_upgrade; proxy_set_header Connection \$everest_conn; proxy_read_timeout 600s; proxy_request_buffering off; }
}
EOF
  echo "wrote everest-${NAME}.conf (svc-ui)"
done

echo "=== nginx -t ==="
/opt/homebrew/bin/nginx -t 2>&1 | tail -4
echo "=== reload ==="
/opt/homebrew/bin/nginx -s reload 2>&1 && echo "reloaded"
