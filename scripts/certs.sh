#!/usr/bin/env bash
# Local PKI for meshshop (idempotent). Writes to .certs/ (git-ignored):
#
#   ca.crt / ca.key          a local CA; trust ca.crt in curl/newman/Postman
#   gateway.crt / .key       the ingress gateway's HTTPS certificate (meshshop.localhost)
#   logstash.crt / .key      Logstash's beats input (server, TLS)
#   filebeat.crt / .key      Filebeat's client certificate (mutual TLS to Logstash)
#
# In a real cluster use cert-manager with a public issuer for the gateway and an
# internal issuer for the rest; the secrets have the same shape.
set -euo pipefail
DIR="$(cd "$(dirname "$0")/.." && pwd)/.certs"
DAYS=397
mkdir -p "$DIR" && chmod 700 "$DIR"
cd "$DIR"

key() { openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$1" 2>/dev/null && chmod 600 "$1"; }

if [[ ! -s ca.key ]]; then
  key ca.key
  openssl req -x509 -new -key ca.key -sha256 -days 1825 -subj "/O=meshshop/CN=meshshop local CA" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" -addext "keyUsage=critical,keyCertSign,cRLSign" -out ca.crt
  echo "created CA .certs/ca.crt"
fi

leaf() { # name cn san extendedKeyUsage
  [[ -s "$1.crt" ]] && openssl x509 -checkend 2592000 -noout -in "$1.crt" >/dev/null && return 0
  key "$1.key"
  openssl req -new -key "$1.key" -subj "/O=meshshop/CN=$2" -out "$1.csr"
  openssl x509 -req -in "$1.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -sha256 -days "$DAYS" -out "$1.crt" \
    -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=%s\nkeyUsage=critical,digitalSignature\nbasicConstraints=critical,CA:FALSE\n' "$3" "$4") 2>/dev/null
  rm -f "$1.csr"
  echo "issued .certs/$1.crt ($2)"
}

leaf gateway  meshshop.localhost "DNS:meshshop.localhost,DNS:localhost,IP:127.0.0.1,IP:::1" serverAuth
leaf logstash meshshop-ls-beats "DNS:meshshop-ls-beats.observability.svc,DNS:meshshop-ls-beats.observability.svc.cluster.local" serverAuth
leaf filebeat filebeat "DNS:filebeat.observability-agents" clientAuth
