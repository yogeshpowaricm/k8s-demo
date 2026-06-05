#!/usr/bin/env bash
# Generate a self-signed TLS cert and create the api-tls Secret in namespace demo.
# For local minikube demos only — not for production.
set -euo pipefail

HOST="${INGRESS_HOST:-api.demo.local}"
NAMESPACE="${NAMESPACE:-demo}"
SECRET_NAME="${SECRET_NAME:-api-tls}"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -keyout "$TMPDIR/tls.key" \
  -out "$TMPDIR/tls.crt" \
  -subj "/CN=${HOST}/O=k8s-autoscale-demo" \
  -addext "subjectAltName=DNS:${HOST}"

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "$NAMESPACE" create secret tls "$SECRET_NAME" \
  --cert="$TMPDIR/tls.crt" \
  --key="$TMPDIR/tls.key" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "Created secret ${SECRET_NAME} in namespace ${NAMESPACE} for host ${HOST}"
echo "Add to /etc/hosts:  $(minikube ip 2>/dev/null || echo '<minikube-ip>')  ${HOST}"
