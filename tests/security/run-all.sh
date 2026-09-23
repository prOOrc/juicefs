#!/usr/bin/env bash
# Stage 9 security suite (task 9.5): runs the five FR-TEST-26..30 scenarios and
# writes tests/security/report.md. Scenarios 27/28/30 are pure crypto (no services
# needed); 26 and 29 run Go tests against Redis (REDIS_ADDR, default 127.0.0.1:6379).
set -uo pipefail
cd "$(dirname "$0")/../.."

REPORT=tests/security/report.md

go_test() { # $1 = exact test name; prints PASS or FAIL
  if go test ./pkg/meta/ -run "^$1$" -count=1 >/dev/null 2>&1; then
    echo PASS
  else
    echo FAIL
  fi
}

echo "running FR-TEST-26 (TestInsiderCrossFileFEK_Denied)..."
S26=$(go_test TestInsiderCrossFileFEK_Denied)
echo "running FR-TEST-29 (TestRenderCrossCompany)..."
S29=$(go_test TestRenderCrossCompany)
echo "running FR-TEST-27/28/30 (crypto harness)..."
CRYPTO_STATUS=PASS
CRYPTO=$(go run ./tests/security 2>&1) || CRYPTO_STATUS=FAIL
S27=$(printf '%s\n' "$CRYPTO" | awk '/^STATUS FR-TEST-27 /{print $3}')
S28=$(printf '%s\n' "$CRYPTO" | awk '/^STATUS FR-TEST-28 /{print $3}')
S30=$(printf '%s\n' "$CRYPTO" | awk '/^STATUS FR-TEST-30 /{print $3}')
DETAILS=$(printf '%s\n' "$CRYPTO" | sed -n '/^## Details/,$p')

OVERALL=PASS
for s in "$S26" "${S27:-FAIL}" "${S28:-FAIL}" "$S29" "${S30:-FAIL}"; do
  if [ "$s" != "PASS" ]; then
    OVERALL=FAIL
  fi
done

{
  echo "# Security test report (stage 9, task 9.5)"
  echo
  echo "- Date: $(date '+%Y-%m-%d %H:%M %Z')"
  echo "- Scenarios 26/29: Go tests against Redis at \`${REDIS_ADDR:-127.0.0.1:6379}\`"
  echo "- Scenarios 27/28/30: pure crypto harness (no services required)"
  echo
  echo "| FR-TEST | Scenario | Test / harness | Status |"
  echo "|---|---|---|---|"
  echo "| FR-TEST-26 | Инсайдер с Read на X пытается получить FEK Y → deny | \`TestInsiderCrossFileFEK_Denied\` (pkg/meta) | ${S26} |"
  echo "| FR-TEST-27 | Утечка S3: чанки нечитаемы без CEK | crypto harness (tests/security) | ${S27:-FAIL} |"
  echo "| FR-TEST-28 | Утечка Redis: wrapped_fek нечитаем без Company KEK | crypto harness (tests/security) | ${S28:-FAIL} |"
  echo "| FR-TEST-29 | Компрометация render-ноды: доступ только к своей компании | \`TestRenderCrossCompany\` (pkg/meta) | ${S29} |"
  echo "| FR-TEST-30 | Утечка Redis + S3 одновременно: данные нечитаемы без Company KEK | crypto harness (tests/security) | ${S30:-FAIL} |"
  echo
  echo "Overall: **${OVERALL}**"
  echo
  printf '%s\n' "$DETAILS"
} > "$REPORT"

echo "report written to $REPORT (overall: $OVERALL)"
[ "$OVERALL" = "PASS" ]
