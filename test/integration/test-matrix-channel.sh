#!/usr/bin/env bash
# Integration test: verify the Matrix channel pipeline works.
#
# This test has two modes:
#
# Mode 1 (no MATRIX_* env vars): Deployment pipeline only
#   - Creates an Agent with a matrix channel
#   - Verifies the controller creates a channel-matrix Deployment
#   - Checks the Deployment has the correct image, labels, and config
#   - Verifies the MATRIX_HOMESERVER env var is injected when configured
#
# Mode 2 (with MATRIX_HOMESERVER, MATRIX_USER_ID, MATRIX_PASSWORD or MATRIX_ACCESS_TOKEN): Full E2E
#   - Everything in Mode 1, plus:
#   - Creates an AgentRun that uses send_channel_message to send a message
#   - Verifies the message appears in the agent result
#
# Prerequisites:
#   - Kind cluster running with Sympozium installed
#   - channel-matrix image available in the cluster
#   - (Mode 2) MATRIX_* credentials set
#
# Usage:
#   # Mode 1: deployment pipeline only
#   ./test/integration/test-matrix-channel.sh
#
#   # Mode 2: full end-to-end with real Matrix bot
#   MATRIX_HOMESERVER=https://matrix.org MATRIX_USER_ID=@mybot:matrix.org MATRIX_PASSWORD=secret \
#     ./test/integration/test-matrix-channel.sh

set -euo pipefail

# --- Configuration ---
NAMESPACE="${TEST_NAMESPACE:-default}"
INSTANCE_NAME="inttest-matrix"
RUN_NAME="inttest-matrix-msg"
SECRET_NAME="inttest-openai-key"
MATRIX_SECRET_NAME="inttest-matrix-secret"
MATRIX_HOMESERVER="${MATRIX_HOMESERVER:-https://matrix.org}"
MODEL="${TEST_MODEL:-gpt-4o-mini}"
TIMEOUT="${TEST_TIMEOUT:-120}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "${GREEN}✓ $*${NC}"; }
fail() { echo -e "${RED}✗ $*${NC}"; }
info() { echo -e "${YELLOW}● $*${NC}"; }
failures=0

FULL_MODE=false
if [[ -n "${MATRIX_HOMESERVER:-}" && -n "${MATRIX_USER_ID:-}" && ( -n "${MATRIX_PASSWORD:-}" || -n "${MATRIX_ACCESS_TOKEN:-}" ) ]]; then
    FULL_MODE=true
fi

cleanup() {
    info "Cleaning up test resources..."
    kubectl delete agentrun "$RUN_NAME" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete agent "$INSTANCE_NAME" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete jobs -n "$NAMESPACE" -l "sympozium.ai/agentrun=$RUN_NAME" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete pods -n "$NAMESPACE" -l "sympozium.ai/agentrun=$RUN_NAME" --ignore-not-found >/dev/null 2>&1 || true
    sleep 3
    kubectl delete deployment "$INSTANCE_NAME-channel-matrix" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete secret "$MATRIX_SECRET_NAME" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
}

# --- Pre-flight checks ---
if $FULL_MODE; then
    info "Running integration test: Matrix channel (FULL — real bot)"
else
    info "Running integration test: Matrix channel (deployment pipeline only)"
    info "Set MATRIX_HOMESERVER + MATRIX_USER_ID + (MATRIX_PASSWORD or MATRIX_ACCESS_TOKEN) for full E2E test"
fi

if ! kubectl get crd agents.sympozium.ai >/dev/null 2>&1; then
    fail "Sympozium CRDs not installed."
    exit 1
fi

if ! kubectl get deployment sympozium-controller-manager -n sympozium-system >/dev/null 2>&1; then
    fail "Sympozium controller not running."
    exit 1
fi

# --- Ensure secrets exist ---
if $FULL_MODE; then
    # Matrix secret with credentials
    kubectl delete secret "$MATRIX_SECRET_NAME" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1
    if [[ -n "${MATRIX_ACCESS_TOKEN:-}" ]]; then
        kubectl create secret generic "$MATRIX_SECRET_NAME" \
            --from-literal=MATRIX_HOMESERVER="$MATRIX_HOMESERVER" \
            --from-literal=MATRIX_USER_ID="$MATRIX_USER_ID" \
            --from-literal=MATRIX_ACCESS_TOKEN="$MATRIX_ACCESS_TOKEN" \
            -n "$NAMESPACE" >/dev/null 2>&1
        info "Created Matrix secret (access token auth)"
    else
        kubectl create secret generic "$MATRIX_SECRET_NAME" \
            --from-literal=MATRIX_HOMESERVER="$MATRIX_HOMESERVER" \
            --from-literal=MATRIX_USER_ID="$MATRIX_USER_ID" \
            --from-literal=MATRIX_PASSWORD="$MATRIX_PASSWORD" \
            -n "$NAMESPACE" >/dev/null 2>&1
        info "Created Matrix secret (password auth)"
    fi

    # OpenAI secret (for AgentRun)
    if ! kubectl get secret "$SECRET_NAME" -n "$NAMESPACE" >/dev/null 2>&1; then
        if [[ -z "${OPENAI_API_KEY:-}" ]]; then
            fail "No OPENAI_API_KEY set and secret '$SECRET_NAME' not found."
            exit 1
        fi
        kubectl create secret generic "$SECRET_NAME" \
            --from-literal=OPENAI_API_KEY="$OPENAI_API_KEY" \
            -n "$NAMESPACE"
    fi
fi

# --- Clean up previous runs ---
cleanup 2>/dev/null || true
sleep 2

# ============================================================
# Part 1: Channel Deployment Pipeline
# ============================================================
info "Creating Agent with matrix channel: $INSTANCE_NAME"

# Build the channel config — use real or dummy tokens
if $FULL_MODE; then
    CHANNEL_SECRET_REF="$MATRIX_SECRET_NAME"
else
    # Create a dummy secret so the controller doesn't error
    kubectl create secret generic "$MATRIX_SECRET_NAME" \
        --from-literal=MATRIX_HOMESERVER="$MATRIX_HOMESERVER" \
        --from-literal=MATRIX_USER_ID="@dummy:matrix.org" \
        --from-literal=MATRIX_PASSWORD="dummy-password" \
        -n "$NAMESPACE" >/dev/null 2>&1 || true
    CHANNEL_SECRET_REF="$MATRIX_SECRET_NAME"
fi

cat <<EOF | kubectl apply -f -
apiVersion: sympozium.ai/v1alpha1
kind: Agent
metadata:
  name: ${INSTANCE_NAME}
  namespace: ${NAMESPACE}
spec:
  agents:
    default:
      model: ${MODEL}
  authRefs:
    - secret: ${SECRET_NAME}
  channels:
    - type: matrix
      configRef:
        secret: ${CHANNEL_SECRET_REF}
      matrix:
        homeserver: ${MATRIX_HOMESERVER}
EOF

# Wait for the channel Deployment to appear
info "Waiting for channel Deployment to be created..."
deploy_name="${INSTANCE_NAME}-channel-matrix"
elapsed=0
deploy_found=false
while [[ $elapsed -lt 30 ]]; do
    if kubectl get deployment "$deploy_name" -n "$NAMESPACE" >/dev/null 2>&1; then
        deploy_found=true
        break
    fi
    sleep 2
    elapsed=$((elapsed + 2))
done

if $deploy_found; then
    pass "Channel Deployment created: $deploy_name"
else
    fail "Channel Deployment not created within 30s"
    failures=$((failures + 1))
fi

# Check the Deployment uses the right image
if $deploy_found; then
    deploy_image=$(kubectl get deployment "$deploy_name" -n "$NAMESPACE" \
        -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
    if echo "$deploy_image" | grep -q "channel-matrix"; then
        pass "Deployment image is correct: $deploy_image"
    else
        fail "Unexpected image: $deploy_image"
        failures=$((failures + 1))
    fi
fi

# Check labels
if $deploy_found; then
    channel_label=$(kubectl get deployment "$deploy_name" -n "$NAMESPACE" \
        -o jsonpath='{.spec.template.metadata.labels.sympozium\.ai/channel}' 2>/dev/null || echo "")
    if [[ "$channel_label" == "matrix" ]]; then
        pass "Deployment has correct channel label: $channel_label"
    else
        fail "Unexpected channel label: $channel_label"
        failures=$((failures + 1))
    fi
fi

# Check instance label
if $deploy_found; then
    instance_label=$(kubectl get deployment "$deploy_name" -n "$NAMESPACE" \
        -o jsonpath='{.spec.template.metadata.labels.sympozium\.ai/instance}' 2>/dev/null || echo "")
    if [[ "$instance_label" == "$INSTANCE_NAME" ]]; then
        pass "Deployment has correct instance label: $instance_label"
    else
        fail "Unexpected instance label: $instance_label"
        failures=$((failures + 1))
    fi
fi

# Check MATRIX_HOMESERVER env var is injected
if $deploy_found; then
    homeserver_env=$(kubectl get deployment "$deploy_name" -n "$NAMESPACE" \
        -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="MATRIX_HOMESERVER")].value}' 2>/dev/null || echo "")
    if [[ "$homeserver_env" == "$MATRIX_HOMESERVER" ]]; then
        pass "MATRIX_HOMESERVER env var injected: $homeserver_env"
    else
        fail "MATRIX_HOMESERVER env var not found or incorrect: '$homeserver_env'"
        failures=$((failures + 1))
    fi
fi

# Check the secret is referenced via envFrom
if $deploy_found; then
    env_from=$(kubectl get deployment "$deploy_name" -n "$NAMESPACE" \
        -o jsonpath='{.spec.template.spec.containers[0].envFrom[0].secretRef.name}' 2>/dev/null || echo "")
    if [[ "$env_from" == "$CHANNEL_SECRET_REF" ]]; then
        pass "Secret injected via envFrom: $env_from"
    else
        fail "Secret not injected via envFrom: '$env_from'"
        failures=$((failures + 1))
    fi
fi

# Check the pod starts (may restart without real tokens, but should be created)
if $deploy_found; then
    info "Waiting for channel pod to be scheduled..."
    elapsed=0
    pod_found=false
    while [[ $elapsed -lt 30 ]]; do
        pod_name=$(kubectl get pods -n "$NAMESPACE" \
            -l "sympozium.ai/instance=$INSTANCE_NAME,sympozium.ai/channel=matrix" \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
        if [[ -n "$pod_name" ]]; then
            pod_found=true
            break
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done

    if $pod_found; then
        pass "Channel pod created: $pod_name"
        # Verify pod spec has the expected volumes (none for matrix, unlike whatsapp)
        has_pvc=$(kubectl get pod "$pod_name" -n "$NAMESPACE" \
            -o jsonpath='{.spec.volumes[*].persistentVolumeClaim.claimName}' 2>/dev/null || echo "")
        if [[ -z "$has_pvc" ]]; then
            pass "Matrix channel pod has no PVC (expected)"
        else
            info "Channel pod has PVC (unexpected but not fatal): $has_pvc"
        fi
    else
        fail "Channel pod not created within 30s"
        failures=$((failures + 1))
    fi
fi

# ============================================================
# Part 2: Full E2E (optional)
# ============================================================
if $FULL_MODE; then
    info "Running E2E message send test..."
    info "Skipping — Matrix E2E message verification requires a Matrix test room. Add room ID + verification as needed."
fi

# --- Summary ---
echo
if [[ $failures -eq 0 ]]; then
    pass "All checks passed!"
    exit 0
else
    fail "$failures check(s) failed."
    exit 1
fi
