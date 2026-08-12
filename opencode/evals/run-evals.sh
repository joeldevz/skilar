#!/usr/bin/env bash
set -euo pipefail

# ================================================================
# Agent Evaluation Runner
# Runs golden tests against our 3 agents and reports results.
#
# Usage:
#   ./evals/run-evals.sh                              # Run all golden tests
#   ./evals/run-evals.sh golden/01-planner-reads-conventions.yaml  # One test
#   ./evals/run-evals.sh --agent tech-planner   # Filter by agent
# ================================================================

EVALS_DIR="$(cd "$(dirname "$0")" && pwd)"
RESULTS_DIR="$EVALS_DIR/results"
GOLDEN_DIR="$EVALS_DIR/golden"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
RESULTS_FILE="$RESULTS_DIR/run-$TIMESTAMP.json"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

# Parse args
FILTER_AGENT=""
FILTER_FILE=""

while [[ $# -gt 0 ]]; do
  case $1 in
    --agent)
      FILTER_AGENT="$2"
      shift 2
      ;;
    *)
      FILTER_FILE="$1"
      shift
      ;;
  esac
done

mkdir -p "$RESULTS_DIR"

echo -e "${CYAN}═══════════════════════════════════════════${NC}"
echo -e "${CYAN}  Agent Evaluation Framework${NC}"
echo -e "${CYAN}  $(date)${NC}"
echo -e "${CYAN}═══════════════════════════════════════════${NC}"
echo ""

# Collect test files
if [[ -n "$FILTER_FILE" ]]; then
  TEST_FILES=("$EVALS_DIR/$FILTER_FILE")
else
  TEST_FILES=("$GOLDEN_DIR"/*.yaml)
fi

TOTAL=0
PASSED=0
FAILED=0
SKIPPED=0

results_json="[]"

for test_file in "${TEST_FILES[@]}"; do
  if [[ ! -f "$test_file" ]]; then
    echo -e "${YELLOW}SKIP${NC} $test_file (not found)"
    SKIPPED=$((SKIPPED + 1))
    continue
  fi

  # Extract metadata from YAML (basic parsing)
  test_id=$(grep '^id:' "$test_file" | head -1 | sed 's/id: *//')
  test_name=$(grep '^name:' "$test_file" | head -1 | sed 's/name: *//' | tr -d '"')
  test_agent=$(grep '^agent:' "$test_file" | head -1 | sed 's/agent: *//')

  # Filter by agent if specified
  if [[ -n "$FILTER_AGENT" && "$test_agent" != "$FILTER_AGENT" ]]; then
    SKIPPED=$((SKIPPED + 1))
    continue
  fi

  TOTAL=$((TOTAL + 1))

  echo -e "${CYAN}─────────────────────────────────────────${NC}"
  echo -e "TEST: ${YELLOW}$test_name${NC}"
  echo -e "  ID:    $test_id"
  echo -e "  Agent: $test_agent"
  echo -e "  File:  $(basename "$test_file")"
  echo ""

  # Extract checks for display
  echo -e "  ${CYAN}Expected behavior:${NC}"
  
  # Show must_read checks
  in_must_read=false
  while IFS= read -r line; do
    if echo "$line" | grep -q "^  must_read:"; then
      in_must_read=true
      continue
    fi
    if $in_must_read; then
      if echo "$line" | grep -q "^    -"; then
        item=$(echo "$line" | sed 's/^    - //')
        echo -e "    ✓ Must read: $item"
      else
        in_must_read=false
      fi
    fi
  done < "$test_file"

  # Show must_not checks
  in_must_not=false
  while IFS= read -r line; do
    if echo "$line" | grep -q "^  must_not:"; then
      in_must_not=true
      continue
    fi
    if $in_must_not; then
      if echo "$line" | grep -q "^    -"; then
        item=$(echo "$line" | sed 's/^    - //' | tr -d '"')
        echo -e "    ✗ Must NOT: $item"
      else
        in_must_not=false
      fi
    fi
  done < "$test_file"

  # Show expect_in_output
  in_expect=false
  while IFS= read -r line; do
    if echo "$line" | grep -q "^  expect_in_output:"; then
      in_expect=true
      continue
    fi
    if $in_expect; then
      if echo "$line" | grep -q "^    -"; then
        item=$(echo "$line" | sed 's/^    - //' | tr -d '"')
        echo -e "    ◎ Output contains: $item"
      else
        in_expect=false
      fi
    fi
  done < "$test_file"

  echo ""
  echo -e "  ${YELLOW}▶ Status: MANUAL REVIEW REQUIRED${NC}"
  echo -e "  Run this test manually with the agent and verify checks above."
  echo ""

  # For now, all tests are "pending manual review"
  # When we have the automated runner, this will execute and evaluate
  result_entry=$(cat <<EOF
{
  "id": "$test_id",
  "name": "$test_name",
  "agent": "$test_agent",
  "file": "$(basename "$test_file")",
  "status": "pending_review",
  "timestamp": "$TIMESTAMP"
}
EOF
)
  # Track as pending (not passed/failed yet)
done

echo -e "${CYAN}═══════════════════════════════════════════${NC}"
echo -e "  SUMMARY"
echo -e "${CYAN}═══════════════════════════════════════════${NC}"
echo -e "  Total:   $TOTAL"
echo -e "  Skipped: $SKIPPED"
echo ""
echo -e "  ${YELLOW}All $TOTAL tests require manual review.${NC}"
echo -e "  Run each agent with the test prompt and verify against checks."
echo ""
echo -e "  Next steps:"
echo -e "  - Automated runner: parse YAML → opencode CLI → evaluate tool calls"
echo -e "  - Track results in $RESULTS_DIR/"
echo ""
